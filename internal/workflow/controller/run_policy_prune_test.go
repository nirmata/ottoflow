/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

// pruneFixture builds a workflow and one completed run per entry of ages (time since completion).
// The run named "current" completed just now.
func pruneFixture(run *ottoflowv1alpha1.RunPolicy, ages []time.Duration) []client.Object {
	ns := defaultNamespace
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: ns},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{{Name: "s1", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `"ok"`}}}},
			Run:   run,
		},
	}
	mk := func(name string, age time.Duration) *ottoflowv1alpha1.WorkflowRun {
		t := metav1.NewTime(time.Now().Add(-age))
		return &ottoflowv1alpha1.WorkflowRun{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID(name)},
			Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: ns}},
			Status:     ottoflowv1alpha1.WorkflowRunStatus{Phase: ottoflowv1alpha1.WorkflowRunPhaseSucceeded, CompletionTime: &t},
		}
	}
	objs := make([]client.Object, 0, len(ages)+2)
	objs = append(objs, wf, mk("current", 0))
	for i, a := range ages {
		objs = append(objs, mk(fmt.Sprintf("run-%03d", i), a))
	}
	return objs
}

func countRuns(t *testing.T, c client.Client) int {
	t.Helper()
	var l ottoflowv1alpha1.WorkflowRunList
	if err := c.List(context.Background(), &l, client.InNamespace(defaultNamespace)); err != nil {
		t.Fatal(err)
	}
	return len(l.Items)
}

func reconcileCurrent(c client.Client) (ctrl.Result, error) {
	r := &WorkflowRunReconciler{Client: c, Scheme: unitTestScheme}
	return r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "current", Namespace: defaultNamespace}})
}

// A cached re-list can return runs that were already deleted; their Delete returns NotFound.
// That must not fail the reconcile or requeue it.
func TestApplyRunPolicy_DeleteNotFound_IsNotAnError(t *testing.T) {
	notFound := func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
		return apierrors.NewNotFound(schema.GroupResource{Group: "ottoflow.nirmata.io", Resource: "workflowruns"}, obj.GetName())
	}
	for name, run := range map[string]*ottoflowv1alpha1.RunPolicy{
		"retention":  {RetentionMinutes: ptr.To[int32](60)},
		"maxAllowed": {MaxAllowed: ptr.To[int32](1)},
	} {
		t.Run(name, func(t *testing.T) {
			objs := pruneFixture(run, []time.Duration{3 * time.Hour, 4 * time.Hour})
			base := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(&ottoflowv1alpha1.WorkflowRun{}).WithObjects(objs...).Build()
			res, err := reconcileCurrent(interceptor.NewClient(base, interceptor.Funcs{Delete: notFound}))
			if err != nil {
				t.Fatalf("NotFound on delete must be tolerated, got: %v", err)
			}
			if res.RequeueAfter != 0 {
				t.Errorf("must not requeue, got %v", res)
			}
		})
	}
}

// Defaults are 10080 minutes / 50 runs: 5 runs older than 7 days go by age, then the oldest of the
// remaining 56 (55 within 7 days plus current) go by count, leaving 50.
func TestApplyRunPolicy_Defaults_Prune(t *testing.T) {
	ages := []time.Duration{}
	for i := 0; i < 5; i++ {
		ages = append(ages, 8*24*time.Hour+time.Duration(i)*time.Hour)
	}
	for i := 0; i < 55; i++ {
		ages = append(ages, time.Duration(i+1)*time.Minute)
	}
	objs := pruneFixture(&ottoflowv1alpha1.RunPolicy{RetentionMinutes: ptr.To[int32](10080), MaxAllowed: ptr.To[int32](50)}, ages)
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(&ottoflowv1alpha1.WorkflowRun{}).WithObjects(objs...).Build()
	if _, err := reconcileCurrent(c); err != nil {
		t.Fatal(err)
	}
	if got := countRuns(t, c); got != 50 {
		t.Errorf("runs left = %d, want 50", got)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "current", Namespace: defaultNamespace}, &ottoflowv1alpha1.WorkflowRun{}); err != nil {
		t.Errorf("current run must be kept: %v", err)
	}
	// run-000 (8 days old) must be gone; run-005 (1 minute old) must remain.
	if err := c.Get(context.Background(), types.NamespacedName{Name: "run-000", Namespace: defaultNamespace}, &ottoflowv1alpha1.WorkflowRun{}); !apierrors.IsNotFound(err) {
		t.Errorf("run-000 should be deleted, err=%v", err)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "run-005", Namespace: defaultNamespace}, &ottoflowv1alpha1.WorkflowRun{}); err != nil {
		t.Errorf("run-005 should remain: %v", err)
	}
}

// A Workflow that sets only one field gets the default for the other (CRD defaulting is covered in
// run_policy_defaults_test.go); here the controller sees both values and each limit works independently.
func TestApplyRunPolicy_OneFieldZero_OtherLimitStillApplies(t *testing.T) {
	old := []time.Duration{}
	for i := 0; i < 5; i++ {
		old = append(old, 30*24*time.Hour+time.Duration(i)*time.Hour)
	}
	recent := []time.Duration{}
	for i := 0; i < 50; i++ {
		recent = append(recent, time.Duration(i+1)*time.Minute)
	}
	cases := map[string]struct {
		run  *ottoflowv1alpha1.RunPolicy
		want int
	}{
		// retentionMinutes: 0 -> no age limit; maxAllowed defaults to 50: keeps the 50 newest of 56.
		"retentionMinutes 0, maxAllowed default": {&ottoflowv1alpha1.RunPolicy{RetentionMinutes: ptr.To[int32](0), MaxAllowed: ptr.To[int32](50)}, 50},
		// maxAllowed: 0 -> no count limit; retention defaults to 7d: removes only the 5 old runs.
		"maxAllowed 0, retentionMinutes default": {&ottoflowv1alpha1.RunPolicy{RetentionMinutes: ptr.To[int32](10080), MaxAllowed: ptr.To[int32](0)}, 51},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			objs := pruneFixture(tc.run, append(append([]time.Duration{}, old...), recent...))
			c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(&ottoflowv1alpha1.WorkflowRun{}).WithObjects(objs...).Build()
			if _, err := reconcileCurrent(c); err != nil {
				t.Fatal(err)
			}
			if got := countRuns(t, c); got != tc.want {
				t.Errorf("runs left = %d, want %d", got, tc.want)
			}
		})
	}
}
