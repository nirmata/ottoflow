/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

// These specs run against the suite's envtest and the generated CRD:
// spec.run defaults apply, and an explicit 0 opt-out survives a typed Update.
var _ = Describe("Workflow spec.run defaults", func() {
	ctx := context.Background()
	steps := []ottoflowv1alpha1.Step{{Name: "s1", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `"ok"`}}}}
	runOf := func(name string) *ottoflowv1alpha1.RunPolicy {
		w := &ottoflowv1alpha1.Workflow{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, w)).To(Succeed())
		return w.Spec.Run
	}

	It("keeps an explicit 0 opt-out across a typed Update", func() {
		u := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "ottoflow.nirmata.io/v1alpha1", "kind": "Workflow",
			"metadata": map[string]interface{}{"name": "optout", "namespace": "default"},
			"spec": map[string]interface{}{
				"steps": []interface{}{map[string]interface{}{"name": "s1", "expressions": []interface{}{
					map[string]interface{}{"name": "x", "expression": `"ok"`}}}},
				"run": map[string]interface{}{"retentionMinutes": int64(0), "maxAllowed": int64(0)},
			}}}
		Expect(k8sClient.Create(ctx, u)).To(Succeed())

		w := &ottoflowv1alpha1.Workflow{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "optout", Namespace: "default"}, w)).To(Succeed())
		controllerutil.AddFinalizer(w, "ottoflow.nirmata.io/a2a-exposure")
		Expect(k8sClient.Update(ctx, w)).To(Succeed())

		r := runOf("optout")
		Expect(r.RetentionMinutes).To(HaveValue(BeEquivalentTo(0)))
		Expect(r.MaxAllowed).To(HaveValue(BeEquivalentTo(0)))
	})

	DescribeTable("rejects negative values",
		func(name string, run *ottoflowv1alpha1.RunPolicy) {
			err := k8sClient.Create(ctx, &ottoflowv1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
				Spec:       ottoflowv1alpha1.WorkflowSpec{Steps: steps, Run: run},
			})
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), err.Error())
		},
		Entry("negative retentionMinutes", "negretention", &ottoflowv1alpha1.RunPolicy{RetentionMinutes: ptr.To[int32](-1)}),
		Entry("negative maxAllowed", "negmax", &ottoflowv1alpha1.RunPolicy{MaxAllowed: ptr.To[int32](-1)}),
	)

	DescribeTable("defaults for typed creates",
		func(name string, run *ottoflowv1alpha1.RunPolicy, wantRetention, wantMax int32) {
			Expect(k8sClient.Create(ctx, &ottoflowv1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
				Spec:       ottoflowv1alpha1.WorkflowSpec{Steps: steps, Run: run},
			})).To(Succeed())
			r := runOf(name)
			Expect(r).NotTo(BeNil())
			Expect(r.RetentionMinutes).To(HaveValue(Equal(wantRetention)))
			Expect(r.MaxAllowed).To(HaveValue(Equal(wantMax)))
		},
		Entry("no spec.run", "norun", nil, int32(10080), int32(50)),
		Entry("partial spec.run", "partial", &ottoflowv1alpha1.RunPolicy{MaxConcurrentRuns: ptr.To[int32](3)}, int32(10080), int32(50)),
		Entry("only retentionMinutes 0", "onlyretention0", &ottoflowv1alpha1.RunPolicy{RetentionMinutes: ptr.To[int32](0)}, int32(0), int32(50)),
		Entry("only maxAllowed 0", "onlymax0", &ottoflowv1alpha1.RunPolicy{MaxAllowed: ptr.To[int32](0)}, int32(10080), int32(0)),
		Entry("typed explicit zero", "typedzero", &ottoflowv1alpha1.RunPolicy{RetentionMinutes: ptr.To[int32](0), MaxAllowed: ptr.To[int32](0)}, int32(0), int32(0)),
	)

	// The Workflow sets one field; the API server fills in the other, and applyRunPolicy prunes with it.
	// Fixture: 3 completed runs 30 days old and 52 completed within the last hour.
	DescribeTable("prunes with the API server default for the unset field",
		func(name string, run *ottoflowv1alpha1.RunPolicy, wantLeft int) {
			Expect(k8sClient.Create(ctx, &ottoflowv1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
				Spec:       ottoflowv1alpha1.WorkflowSpec{Steps: steps, Run: run},
			})).To(Succeed())
			wf := &ottoflowv1alpha1.Workflow{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, wf)).To(Succeed())

			ages := make([]time.Duration, 0, 55)
			for i := range 3 {
				ages = append(ages, 30*24*time.Hour+time.Duration(i)*time.Minute)
			}
			for i := range 52 {
				ages = append(ages, time.Duration(i)*time.Minute)
			}
			var newest *ottoflowv1alpha1.WorkflowRun
			for i, age := range ages {
				wr := &ottoflowv1alpha1.WorkflowRun{
					ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-%03d", name, i), Namespace: "default"},
					Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: name, Namespace: "default"}},
				}
				Expect(k8sClient.Create(ctx, wr)).To(Succeed())
				done := metav1.NewTime(time.Now().Add(-age))
				wr.Status = ottoflowv1alpha1.WorkflowRunStatus{Phase: ottoflowv1alpha1.WorkflowRunPhaseSucceeded, CompletionTime: &done}
				Expect(k8sClient.Status().Update(ctx, wr)).To(Succeed())
				if age == 0 {
					newest = wr
				}
			}

			r := &WorkflowRunReconciler{Client: k8sClient}
			deleted, err := r.applyRunPolicy(ctx, wf, newest)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeFalse())

			var runs ottoflowv1alpha1.WorkflowRunList
			Expect(k8sClient.List(ctx, &runs, client.InNamespace("default"))).To(Succeed())
			left := 0
			for _, wr := range runs.Items {
				if wr.Spec.WorkflowRef.Name == name {
					left++
				}
			}
			Expect(left).To(Equal(wantLeft))
		},
		// maxAllowed defaults to 50: no age limit, so the 50 newest of 55 stay.
		Entry("only retentionMinutes 0", "pruneret0", &ottoflowv1alpha1.RunPolicy{RetentionMinutes: ptr.To[int32](0)}, 50),
		// retentionMinutes defaults to 10080: the 3 old runs go, no count limit, so 52 stay.
		Entry("only maxAllowed 0", "prunemax0", &ottoflowv1alpha1.RunPolicy{MaxAllowed: ptr.To[int32](0)}, 52),
	)
})
