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
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/agent"
)

var _ = Describe("Expression-based step execution", func() {
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
			ObjectMeta: metav1.ObjectMeta{Name: "test-run", Namespace: "default"},
			Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "test-wf"}},
		}
		k8sClient = fake.NewClientBuilder().WithScheme(scheme).Build()
	})

	It("should evaluate expressions and outputs and write to variables", func() {
		var err error
		workflowExecutor, err = NewWorkflowExecutorWithAgentExecutor(
			k8sClient, nil, nil, nil, workflowRun, agent.NewMockAgentExecutor(), false, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())

		workflow := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "test-wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{
						Name: "exprStep",
						Expressions: []ottoflowv1alpha1.Expression{
							{Name: "sum", Expression: "1 + 2"},
						},
						Outputs: []ottoflowv1alpha1.Output{
							{Name: "total", Expression: "expressions.sum + 10"},
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, workflow)).To(Succeed())

		err = workflowExecutor.ExecuteWorkflow(ctx, workflow, workflowRun)
		Expect(err).NotTo(HaveOccurred())
		Expect(workflowRun.Status.Phase).To(Equal(ottoflowv1alpha1.WorkflowRunPhaseSucceeded))
		Expect(workflowRun.Status.StepStatuses["exprStep"].Phase).To(Equal(ottoflowv1alpha1.StepPhaseSucceeded))

		ctxData, err := workflowExecutor.GetContextManager().ReadContext(ctx)
		Expect(err).NotTo(HaveOccurred())
		variables := ctxData["variables"].(map[string]interface{})
		Expect(variables["total"]).To(Equal(int64(13)))
	})

	It("should evaluate outputs only when no expressions", func() {
		var err error
		workflowExecutor, err = NewWorkflowExecutorWithAgentExecutor(
			k8sClient, nil, nil, nil, workflowRun, agent.NewMockAgentExecutor(), false, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())

		workflow := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "test-wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{
						Name: "outStep",
						Outputs: []ottoflowv1alpha1.Output{
							{Name: "value", Expression: `"hello"`},
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, workflow)).To(Succeed())

		err = workflowExecutor.ExecuteWorkflow(ctx, workflow, workflowRun)
		Expect(err).NotTo(HaveOccurred())
		ctxData, _ := workflowExecutor.GetContextManager().ReadContext(ctx)
		variables := ctxData["variables"].(map[string]interface{})
		Expect(variables["value"]).To(Equal("hello"))
	})

	It("should execute step when match condition is true", func() {
		var err error
		workflowExecutor, err = NewWorkflowExecutorWithAgentExecutor(
			k8sClient, nil, nil, nil, workflowRun, agent.NewMockAgentExecutor(), false, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())

		workflow := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "test-wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{
						Name: "condStep",
						MatchConditions: []ottoflowv1alpha1.MatchCondition{
							{Name: "allow", Expression: "true"},
						},
						Outputs: []ottoflowv1alpha1.Output{{Name: "x", Expression: `"executed"`}},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, workflow)).To(Succeed())
		err = workflowExecutor.ExecuteWorkflow(ctx, workflow, workflowRun)
		Expect(err).NotTo(HaveOccurred())
		ctxData, _ := workflowExecutor.GetContextManager().ReadContext(ctx)
		variables := ctxData["variables"].(map[string]interface{})
		Expect(variables["x"]).To(Equal("executed"))
	})

	It("should continue workflow when step fails with FailurePolicyContinue", func() {
		var err error
		workflowExecutor, err = NewWorkflowExecutorWithAgentExecutor(
			k8sClient, nil, nil, nil, workflowRun, agent.NewMockAgentExecutor(), false, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())
		workflow := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "test-wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{
						Name:          "failingStep",
						FailurePolicy: ottoflowv1alpha1.FailurePolicyContinue,
						Outputs:       []ottoflowv1alpha1.Output{{Name: "x", Expression: "undefined_var"}}, // fails
					},
					{
						Name:    "okStep",
						Outputs: []ottoflowv1alpha1.Output{{Name: "y", Expression: `"ok"`}},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, workflow)).To(Succeed())
		err = workflowExecutor.ExecuteWorkflow(ctx, workflow, workflowRun)
		Expect(err).NotTo(HaveOccurred())
		Expect(workflowRun.Status.Phase).To(Equal(ottoflowv1alpha1.WorkflowRunPhaseSucceeded))
		Expect(workflowRun.Status.StepStatuses["failingStep"].Phase).To(Equal(ottoflowv1alpha1.StepPhaseFailed))
		Expect(workflowRun.Status.StepStatuses["okStep"].Phase).To(Equal(ottoflowv1alpha1.StepPhaseSucceeded))
		ctxData, _ := workflowExecutor.GetContextManager().ReadContext(ctx)
		variables := ctxData["variables"].(map[string]interface{})
		Expect(variables["y"]).To(Equal("ok"))
	})

	It("should unblock dependent steps when step fails with FailurePolicyContinue", func() {
		var err error
		workflowExecutor, err = NewWorkflowExecutorWithAgentExecutor(
			k8sClient, nil, nil, nil, workflowRun, agent.NewMockAgentExecutor(), false, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())
		workflow := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "test-wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{
						Name:          "optionalStep",
						FailurePolicy: ottoflowv1alpha1.FailurePolicyContinue,
						Outputs:       []ottoflowv1alpha1.Output{{Name: "x", Expression: "undefined_var"}}, // fails
					},
					{
						Name:      "dependentStep",
						DependsOn: []string{"optionalStep"},
						Outputs:   []ottoflowv1alpha1.Output{{Name: "y", Expression: `"executed"`}},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, workflow)).To(Succeed())
		err = workflowExecutor.ExecuteWorkflow(ctx, workflow, workflowRun)
		Expect(err).NotTo(HaveOccurred())
		Expect(workflowRun.Status.Phase).To(Equal(ottoflowv1alpha1.WorkflowRunPhaseSucceeded))
		Expect(workflowRun.Status.StepStatuses["optionalStep"].Phase).To(Equal(ottoflowv1alpha1.StepPhaseFailed))
		Expect(workflowRun.Status.StepStatuses["dependentStep"].Phase).To(Equal(ottoflowv1alpha1.StepPhaseSucceeded))
		ctxData, _ := workflowExecutor.GetContextManager().ReadContext(ctx)
		variables := ctxData["variables"].(map[string]interface{})
		Expect(variables["y"]).To(Equal("executed"))
	})

	It("should skip step when match condition is false", func() {
		var err error
		workflowExecutor, err = NewWorkflowExecutorWithAgentExecutor(
			k8sClient, nil, nil, nil, workflowRun, agent.NewMockAgentExecutor(), false, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())

		workflow := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "test-wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{
						Name: "skipStep",
						MatchConditions: []ottoflowv1alpha1.MatchCondition{
							{Name: "deny", Expression: "false"},
						},
						Outputs: []ottoflowv1alpha1.Output{{Name: "x", Expression: `"should not run"`}},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, workflow)).To(Succeed())
		err = workflowExecutor.ExecuteWorkflow(ctx, workflow, workflowRun)
		Expect(err).NotTo(HaveOccurred())
		ctxData, _ := workflowExecutor.GetContextManager().ReadContext(ctx)
		variables := ctxData["variables"].(map[string]interface{})
		Expect(variables).NotTo(HaveKey("x"))
	})

	It("should evaluate workflow-level outputs (Spec.Outputs) and write to status", func() {
		var err error
		workflowExecutor, err = NewWorkflowExecutorWithAgentExecutor(
			k8sClient, nil, nil, nil, workflowRun, agent.NewMockAgentExecutor(), false, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())

		workflow := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "test-wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{Name: "s1", Outputs: []ottoflowv1alpha1.Output{{Name: "x", Expression: `"step-value"`}}},
				},
				Outputs: []ottoflowv1alpha1.Output{
					{Name: "summary", Expression: `variables.x`},
				},
			},
		}
		Expect(k8sClient.Create(ctx, workflow)).To(Succeed())
		err = workflowExecutor.ExecuteWorkflow(ctx, workflow, workflowRun)
		Expect(err).NotTo(HaveOccurred())
		Expect(workflowRun.Status.Outputs).NotTo(BeNil())
		Expect(workflowRun.Status.Outputs["summary"].Raw).To(MatchJSON(`"step-value"`))
	})

	It("should redact sensitive workflow-level output in status", func() {
		var err error
		workflowExecutor, err = NewWorkflowExecutorWithAgentExecutor(
			k8sClient, nil, nil, nil, workflowRun, agent.NewMockAgentExecutor(), false, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())

		workflow := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "test-wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{Name: "s1", Outputs: []ottoflowv1alpha1.Output{{Name: "secret", Expression: `"my-secret"`}}},
				},
				Outputs: []ottoflowv1alpha1.Output{
					{Name: "secretOut", Expression: `variables.secret`, Sensitive: true},
				},
			},
		}
		Expect(k8sClient.Create(ctx, workflow)).To(Succeed())
		err = workflowExecutor.ExecuteWorkflow(ctx, workflow, workflowRun)
		Expect(err).NotTo(HaveOccurred())
		Expect(workflowRun.Status.Outputs["secretOut"].Raw).To(ContainSubstring("_ottoflow_redacted"))
	})

	It("should evaluate workflow-level output with Value (structured JSON)", func() {
		var err error
		workflowExecutor, err = NewWorkflowExecutorWithAgentExecutor(
			k8sClient, nil, nil, nil, workflowRun, agent.NewMockAgentExecutor(), false, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())
		workflow := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "test-wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{Name: "s1", Outputs: []ottoflowv1alpha1.Output{{Name: "x", Expression: `"step-x"`}}},
				},
				Outputs: []ottoflowv1alpha1.Output{
					{
						Name:  "nested",
						Value: &apiextensionsv1.JSON{Raw: []byte(`{"label": "variables.x", "num": 42}`)},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, workflow)).To(Succeed())
		err = workflowExecutor.ExecuteWorkflow(ctx, workflow, workflowRun)
		Expect(err).NotTo(HaveOccurred())
		Expect(workflowRun.Status.Outputs["nested"].Raw).To(MatchJSON(`{"label": "step-x", "num": 42}`))
	})

	It("should respect outbound rate limit when ExecutionLimits.OutboundRequestsPerMinute is set", func() {
		rpm := int32(60)
		workflowExecutor, err := NewWorkflowExecutorWithAgentExecutor(
			k8sClient, nil, nil, nil, workflowRun, agent.NewMockAgentExecutor(), false, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())
		workflow := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "test-wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				ExecutionLimits: &ottoflowv1alpha1.ExecutionLimits{
					OutboundRequestsPerMinute: &rpm, // triggers waitOutboundRateLimit path
				},
				Steps: []ottoflowv1alpha1.Step{
					{Name: "s1", Outputs: []ottoflowv1alpha1.Output{{Name: "x", Expression: `"ok"`}}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, workflow)).To(Succeed())
		err = workflowExecutor.ExecuteWorkflow(ctx, workflow, workflowRun)
		Expect(err).NotTo(HaveOccurred())
	})

	It("should proceed normally when workflow run has RestartRequired set but phase is not Running", func() {
		// Even if RestartRequired is present on the workflow run status, execution should proceed
		// normally when the phase is not Running.
		workflowRun.Status.RestartRequired = true
		workflowExecutor, err := NewWorkflowExecutorWithAgentExecutor(
			k8sClient, nil, nil, nil, workflowRun, agent.NewMockAgentExecutor(), false, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())
		workflow := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "w", Namespace: "default"},
			Spec:       ottoflowv1alpha1.WorkflowSpec{Steps: []ottoflowv1alpha1.Step{{Name: "s", Outputs: []ottoflowv1alpha1.Output{{Name: "x", Expression: `"v"`}}}}},
		}
		Expect(k8sClient.Create(ctx, workflow)).To(Succeed())
		err = workflowExecutor.ExecuteWorkflow(ctx, workflow, workflowRun)
		Expect(err).NotTo(HaveOccurred())
	})

	It("SetImageDataFetcher is used when step evaluates image expression", func() {
		workflowExecutor, err := NewWorkflowExecutorWithAgentExecutor(
			k8sClient, nil, nil, nil, workflowRun, agent.NewMockAgentExecutor(), false, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())
		workflowExecutor.SetImageDataFetcher(&MockImageDataFetcher{
			Data: map[string]any{"digest": "sha256:test", "registry": "docker.io"},
		})

		workflow := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "test-wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{
						Name: "imgStep",
						Outputs: []ottoflowv1alpha1.Output{
							{Name: "digest", Expression: `image.GetMetadata("nginx:latest").digest`},
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, workflow)).To(Succeed())
		err = workflowExecutor.ExecuteWorkflow(ctx, workflow, workflowRun)
		Expect(err).NotTo(HaveOccurred())
		ctxData, _ := workflowExecutor.GetContextManager().ReadContext(ctx)
		variables := ctxData["variables"].(map[string]interface{})
		Expect(variables["digest"]).To(Equal("sha256:test"))
	})
})
