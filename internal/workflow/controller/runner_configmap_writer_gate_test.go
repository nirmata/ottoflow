/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

func writerBindingExists(t *testing.T, c client.Client, sa string) bool {
	t.Helper()
	err := c.Get(context.Background(), client.ObjectKey{Namespace: cmWriterNS, Name: runnerConfigMapWriterBindingName(sa)}, &rbacv1.RoleBinding{})
	return err == nil
}

func expectWriterRefusalRecorded(t *testing.T, c client.Client, wr *ottoflowv1alpha1.WorkflowRun, rec *events.FakeRecorder) {
	t.Helper()
	got := &ottoflowv1alpha1.WorkflowRun{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(wr), got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase == ottoflowv1alpha1.WorkflowRunPhaseFailed {
		t.Error("run failed")
	}
	select {
	case evt := <-rec.Events:
		if !strings.Contains(evt, "RunnerServiceAccountNotManaged") || !strings.Contains(evt, cmWriterSA) {
			t.Errorf("event = %q, want RunnerServiceAccountNotManaged naming %s", evt, cmWriterSA)
		}
	default:
		t.Error("expected a RunnerServiceAccountNotManaged Warning event")
	}
}

func TestReconcile_ExistingJob_UnlabelledSA_NoWriterBindingEventRecorded(t *testing.T) {
	wr, objs := existingJobFixture(batchv1.JobStatus{Active: 1})
	objs[len(objs)-1].SetLabels(nil)
	saGets := 0
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(wr).WithObjects(objs...).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.ServiceAccount); ok {
				saGets++
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	}).Build()
	r := cmWriterReconciler(c, cmWriterRole)
	rec := events.NewFakeRecorder(10)
	r.EventRecorder = rec
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(wr)}); err != nil {
		t.Fatal(err)
	}
	if saGets != 1 {
		t.Errorf("ServiceAccount Gets = %d, want 1", saGets)
	}
	if writerBindingExists(t, c, cmWriterSA) {
		t.Error("writer RoleBinding created for an unlabelled ServiceAccount")
	}
	expectWriterRefusalRecorded(t, c, wr, rec)
}

func TestReconcile_ExistingJob_UnlabelledSA_RoleDrift_KeepsWriterBinding(t *testing.T) {
	wr, objs := existingJobFixture(batchv1.JobStatus{Active: 1})
	objs[len(objs)-1].SetLabels(nil)
	objs = append(objs, managedWriterBinding("ClusterRole", "old-role", cmWriterUID))
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(wr).WithObjects(objs...).Build()
	r := cmWriterReconciler(c, cmWriterRole)
	rec := events.NewFakeRecorder(10)
	r.EventRecorder = rec
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(wr)}); err != nil {
		t.Fatal(err)
	}
	rb, err := getWriterBinding(t, c)
	if err != nil {
		t.Fatalf("writer RoleBinding gone after role drift on an unlabelled ServiceAccount: %v", err)
	}
	if rb.RoleRef.Name != "old-role" {
		t.Errorf("RoleRef.Name = %q, want old-role kept", rb.RoleRef.Name)
	}
	expectWriterRefusalRecorded(t, c, wr, rec)
}

func TestReconcile_ExistingJob_LabelledSA_RoleDrift_ReplacesWriterBinding(t *testing.T) {
	wr, objs := existingJobFixture(batchv1.JobStatus{Active: 1})
	objs = append(objs, managedWriterBinding("ClusterRole", "old-role", cmWriterUID))
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(wr).WithObjects(objs...).Build()
	r := cmWriterReconciler(c, cmWriterRole)
	r.EventRecorder = events.NewFakeRecorder(10)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(wr)}); err != nil {
		t.Fatal(err)
	}
	rb, err := getWriterBinding(t, c)
	if err != nil {
		t.Fatal(err)
	}
	if rb.RoleRef.Name != cmWriterRole {
		t.Errorf("RoleRef.Name = %q, want %s", rb.RoleRef.Name, cmWriterRole)
	}
}

func TestReconcile_ExplicitSAThenCleared_NoWriterBinding(t *testing.T) {
	wr, objs := existingJobFixture(batchv1.JobStatus{Active: 1})
	const explicit = "other-sa"
	wr.Spec.Execution = &ottoflowv1alpha1.WorkflowRunExecutionSpec{Job: &ottoflowv1alpha1.WorkflowRunJobSpec{ServiceAccountName: explicit}}
	wr.Status.Execution = nil
	objs = []client.Object{objs[0], wr, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: explicit, Namespace: cmWriterNS}}}
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(wr).WithObjects(objs...).Build()
	r := cmWriterReconciler(c, cmWriterRole)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(wr)}
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if writerBindingExists(t, c, explicit) {
		t.Fatal("binding created on the explicit-SA reconcile")
	}
	cur := &ottoflowv1alpha1.WorkflowRun{}
	if err := c.Get(ctx, req.NamespacedName, cur); err != nil {
		t.Fatal(err)
	}
	cur.Spec.Execution = nil
	if err := c.Update(ctx, cur); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if writerBindingExists(t, c, explicit) {
		t.Error("writer RoleBinding created for an unlabelled SA once the spec stopped being explicit")
	}
}

func TestEnsureRunnerAccess_LabelRemoved_WriterBindingKept(t *testing.T) {
	sa := cmWriterSAObj()
	sa.Labels = nil
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(sa, managedWriterBinding("ClusterRole", cmWriterRole, cmWriterUID)).Build()
	wr := &ottoflowv1alpha1.WorkflowRun{ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: cmWriterNS}}
	if err := cmWriterReconciler(c, cmWriterRole).ensureRunnerAccess(context.Background(), wr, nil, cmWriterSA, false); !errors.Is(err, errRunnerServiceAccountNotManaged) {
		t.Fatalf("err = %v, want errRunnerServiceAccountNotManaged", err)
	}
	if !writerBindingExists(t, c, cmWriterSA) {
		t.Error("writer RoleBinding was deleted by the gate; only the sweep removes it")
	}
}

func TestDeleteRunnerBindingsFor_UnlabelledSameNameWriterBindingPreserved(t *testing.T) {
	rb := managedWriterBinding("ClusterRole", cmWriterRole, cmWriterUID)
	rb.Labels = nil
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(rb).Build()
	cmWriterReconciler(c, cmWriterRole).deleteRunnerBindingsFor(context.Background(), cmWriterNS, cmWriterSA)
	if !writerBindingExists(t, c, cmWriterSA) {
		t.Error("unlabelled RoleBinding was deleted")
	}
}

func TestDeleteRunnerBindingsFor_WriterBindingNotSingleMatchingSubject_Preserved(t *testing.T) {
	mine := rbacv1.Subject{Kind: "ServiceAccount", Name: cmWriterSA, Namespace: cmWriterNS}
	other := rbacv1.Subject{Kind: "ServiceAccount", Name: "someone-else", Namespace: cmWriterNS}
	cases := map[string][]rbacv1.Subject{
		"two subjects":    {mine, other},
		"other subject":   {other},
		"other namespace": {{Kind: "ServiceAccount", Name: cmWriterSA, Namespace: "elsewhere"}},
		"no subjects":     nil,
		"non-SA kind":     {{Kind: "User", Name: cmWriterSA, Namespace: cmWriterNS}},
	}
	for name, subjects := range cases {
		t.Run(name, func(t *testing.T) {
			rb := managedWriterBinding("ClusterRole", cmWriterRole, cmWriterUID)
			rb.Subjects = subjects
			c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(rb).Build()
			cmWriterReconciler(c, cmWriterRole).deleteRunnerBindingsFor(context.Background(), cmWriterNS, cmWriterSA)
			if !writerBindingExists(t, c, cmWriterSA) {
				t.Error("writer RoleBinding with a non-matching subject was deleted")
			}
		})
	}
}

func TestDeleteRunnerBindingsFor_WriterRoleUnset_SkipsWriterBinding(t *testing.T) {
	for _, tc := range []struct {
		name        string
		role        string
		wantRBGets  int
		wantRemoved bool
	}{
		{"writer role empty: no RoleBinding read", "", 0, false},
		{"writer role set: binding read and removed", cmWriterRole, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rbGets := 0
			c := fake.NewClientBuilder().WithScheme(unitTestScheme).
				WithObjects(managedWriterBinding("ClusterRole", cmWriterRole, cmWriterUID)).
				WithInterceptorFuncs(interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, ok := obj.(*rbacv1.RoleBinding); ok {
						rbGets++
					}
					return c.Get(ctx, key, obj, opts...)
				}}).Build()
			cmWriterReconciler(c, tc.role).deleteRunnerBindingsFor(context.Background(), cmWriterNS, cmWriterSA)
			if rbGets != tc.wantRBGets {
				t.Errorf("RoleBinding Gets = %d, want %d", rbGets, tc.wantRBGets)
			}
			_, err := getWriterBinding(t, c)
			if removed := err != nil; removed != tc.wantRemoved {
				t.Errorf("writer RoleBinding removed = %v, want %v (err=%v)", removed, tc.wantRemoved, err)
			}
		})
	}
}

func TestReconcile_ExistingJob_UnlabelledSA_SubjectDrift_KeepsWriterBinding(t *testing.T) {
	for name, subj := range map[string][]rbacv1.Subject{
		"empty":         nil,
		"other subject": {{Kind: "ServiceAccount", Name: "other", Namespace: cmWriterNS}},
	} {
		t.Run(name, func(t *testing.T) {
			wr, objs := existingJobFixture(batchv1.JobStatus{Active: 1})
			objs[len(objs)-1].SetLabels(nil)
			drifted := managedWriterBinding("ClusterRole", cmWriterRole, cmWriterUID)
			drifted.Subjects = subj
			objs = append(objs, drifted)
			c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(wr).WithObjects(objs...).Build()
			r := cmWriterReconciler(c, cmWriterRole)
			rec := events.NewFakeRecorder(10)
			r.EventRecorder = rec
			if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(wr)}); err != nil {
				t.Fatal(err)
			}
			rb, err := getWriterBinding(t, c)
			if err != nil {
				t.Fatalf("writer RoleBinding gone: %v", err)
			}
			if !slices.Equal(rb.Subjects, subj) {
				t.Errorf("subjects = %+v, want unchanged %+v", rb.Subjects, subj)
			}
			expectWriterRefusalRecorded(t, c, wr, rec)
		})
	}
}

func TestReconcile_ExistingJob_LabelledSA_SubjectDrift_RepairsWriterBinding(t *testing.T) {
	wr, objs := existingJobFixture(batchv1.JobStatus{Active: 1})
	drifted := managedWriterBinding("ClusterRole", cmWriterRole, cmWriterUID)
	drifted.Subjects = nil
	objs = append(objs, drifted)
	reads := map[string]int{}
	funcs := accountReadCounter(reads)
	funcs.Delete = func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
		if _, ok := obj.(*rbacv1.RoleBinding); ok {
			t.Error("writer RoleBinding deleted; want subjects repaired in place")
		}
		return cl.Delete(ctx, obj, opts...)
	}
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(wr).WithObjects(objs...).WithInterceptorFuncs(funcs).Build()
	if _, err := cmWriterReconciler(c, cmWriterRole).Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(wr)}); err != nil {
		t.Fatal(err)
	}
	rb, err := getWriterBinding(t, c)
	if err != nil || len(rb.Subjects) != 1 || rb.Subjects[0].Name != cmWriterSA {
		t.Errorf("rb=%+v err=%v, want subjects repaired", rb, err)
	}
	if reads[cmWriterSA] != 1 {
		t.Errorf("ServiceAccount Gets = %d, want 1", reads[cmWriterSA])
	}
}
