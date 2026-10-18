/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
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
})
