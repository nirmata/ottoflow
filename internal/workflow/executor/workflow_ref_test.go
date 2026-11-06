/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/agent"
)

var _ = Describe("WorkflowRef step execution", func() {
	var (
		ctx              context.Context
		k8sClient        client.Client
		scheme           *runtime.Scheme
		workflowRun      *ottoflowv1alpha1.WorkflowRun
		workflowExecutor *WorkflowExecutor
	)

	BeforeEach(func() {
		ctx = context.Background()
		scheme = runtime.NewScheme()
		utilruntime.Must(ottoflowv1alpha1.AddToScheme(scheme))
		utilruntime.Must(clientgoscheme.AddToScheme(scheme))
		workflowRun = &ottoflowv1alpha1.WorkflowRun{
			ObjectMeta: metav1.ObjectMeta{Name: "parent-run", Namespace: "default"},
			Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "parent-wf"}},
		}
	})

	It("should run sub-workflow inline and copy outputs to parent step", func() {
		parentWF := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "parent-wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{
						Name: "callSub",
						WorkflowRef: &ottoflowv1alpha1.StepWorkflowRef{
							Name: "sub-wf",
							Inputs: map[string]string{
								"x": `"from-parent"`,
							},
						},
					},
				},
			},
		}
		subWF := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "sub-wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{
						Name: "subStep",
						Outputs: []ottoflowv1alpha1.Output{
							{Name: "out", Expression: `"sub-result"`},
						},
					},
				},
				Outputs: []ottoflowv1alpha1.Output{
					{Name: "out", Expression: `variables.out`},
				},
			},
		}
		k8sClient = fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(parentWF, subWF).
			Build()

		var err error
		workflowExecutor, err = NewWorkflowExecutorWithAgentExecutor(
			k8sClient, nil, nil, nil, workflowRun, agent.NewMockAgentExecutor(), false, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())

		err = workflowExecutor.ExecuteWorkflow(ctx, parentWF, workflowRun)
		Expect(err).NotTo(HaveOccurred())
		Expect(workflowRun.Status.Phase).To(Equal(ottoflowv1alpha1.WorkflowRunPhaseSucceeded))
		Expect(workflowRun.Status.StepStatuses["callSub"].Phase).To(Equal(ottoflowv1alpha1.StepPhaseSucceeded))

		ctxData, _ := workflowExecutor.GetContextManager().ReadContext(ctx)
		variables := ctxData["variables"].(map[string]interface{})
		Expect(variables["out"]).To(Equal("sub-result"))
	})

	It("should not write StepStatuses from concurrent forEach items running a workflowRef", func() {
		parentWF := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "parent-wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{
						Name: "fe",
						ForEach: &ottoflowv1alpha1.StepForEach{
							Items:          `[1, 2, 3, 4, 5, 6, 7, 8]`,
							MaxConcurrency: 8,
							Step: &ottoflowv1alpha1.StepForEachStep{
								WorkflowRef: &ottoflowv1alpha1.StepWorkflowRef{Name: "sub-wf"},
							},
						},
					},
				},
			},
		}
		subWF := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "sub-wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{Name: "subStep", Outputs: []ottoflowv1alpha1.Output{{Name: "out", Expression: `"ok"`}}},
				},
			},
		}
		k8sClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(parentWF, subWF).Build()

		var err error
		workflowExecutor, err = NewWorkflowExecutorWithAgentExecutor(
			k8sClient, nil, nil, nil, workflowRun, agent.NewMockAgentExecutor(), false, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())

		Expect(workflowExecutor.ExecuteWorkflow(ctx, parentWF, workflowRun)).To(Succeed())
		Expect(workflowRun.Status.Phase).To(Equal(ottoflowv1alpha1.WorkflowRunPhaseSucceeded))
		Expect(workflowRun.Status.StepStatuses).To(HaveLen(1))
		Expect(workflowRun.Status.StepStatuses).To(HaveKey("fe"))
	})

	It("should fail when WorkflowRef is nil", func() {
		step := ottoflowv1alpha1.Step{Name: "bad"}
		k8sClient = fake.NewClientBuilder().WithScheme(scheme).Build()
		workflowExecutor, err := NewWorkflowExecutorWithAgentExecutor(
			k8sClient, nil, nil, nil, workflowRun, agent.NewMockAgentExecutor(), false, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())
		_, err = workflowExecutor.executeWorkflowReference(ctx, workflowRun, step)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("WorkflowRef is nil"))
	})
})
