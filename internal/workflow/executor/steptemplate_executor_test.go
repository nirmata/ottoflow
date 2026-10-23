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

var _ = Describe("StepTemplate Step Execution", func() {
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
	})

	It("should instantiate StepTemplate with arguments and execute resulting step", func() {
		tpl := &ottoflowv1alpha1.StepTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "echo-tpl", Namespace: "default"},
			Spec: ottoflowv1alpha1.StepTemplateSpec{
				Parameters: []ottoflowv1alpha1.StepTemplateParameter{
					{Name: "value", Required: true},
				},
				Step: ottoflowv1alpha1.StepTemplateStep{
					// Produce a CEL string literal so expression evaluates: "\"{{.value}}\"" -> "\"hello\"" in CEL
					Outputs: []ottoflowv1alpha1.Output{
						{Name: "result", Expression: `"{{.value}}"`},
					},
				},
			},
		}
		k8sClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(tpl).Build()

		var err error
		workflowExecutor, err = NewWorkflowExecutorWithAgentExecutor(
			k8sClient, nil, nil, nil, workflowRun, agent.NewMockAgentExecutor(), false, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())

		workflow := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "test-wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{
						Name: "useTpl",
						StepTemplateRef: &ottoflowv1alpha1.StepTemplateRef{
							Name: "echo-tpl",
							Arguments: map[string]string{
								"value": `"hello"`,
							},
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, workflow)).To(Succeed())

		err = workflowExecutor.ExecuteWorkflow(ctx, workflow, workflowRun)
		Expect(err).NotTo(HaveOccurred())
		Expect(workflowRun.Status.Phase).To(Equal(ottoflowv1alpha1.WorkflowRunPhaseSucceeded))
		Expect(workflowRun.Status.StepStatuses["useTpl"].Phase).To(Equal(ottoflowv1alpha1.StepPhaseSucceeded))

		ctxData, err := workflowExecutor.GetContextManager().ReadContext(ctx)
		Expect(err).NotTo(HaveOccurred())
		variables := ctxData["variables"].(map[string]interface{})
		Expect(variables).To(HaveKey("result"))
		Expect(variables["result"]).To(Equal("hello"))
	})

	It("should fail when StepTemplate is not found", func() {
		k8sClient = fake.NewClientBuilder().WithScheme(scheme).Build()
		var err error
		workflowExecutor, err = NewWorkflowExecutorWithAgentExecutor(
			k8sClient, nil, nil, nil, workflowRun, agent.NewMockAgentExecutor(), false, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())

		workflow := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "test-wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{
						Name: "useTpl",
						StepTemplateRef: &ottoflowv1alpha1.StepTemplateRef{
							Name: "missing-tpl",
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, workflow)).To(Succeed())

		err = workflowExecutor.ExecuteWorkflow(ctx, workflow, workflowRun)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("failed to get StepTemplate"))
	})

	It("should use default parameter value when argument not provided", func() {
		tpl := &ottoflowv1alpha1.StepTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "default-tpl", Namespace: "default"},
			Spec: ottoflowv1alpha1.StepTemplateSpec{
				Parameters: []ottoflowv1alpha1.StepTemplateParameter{
					{Name: "value", Required: false, Default: `"default-value"`},
				},
				Step: ottoflowv1alpha1.StepTemplateStep{
					Outputs: []ottoflowv1alpha1.Output{
						{Name: "result", Expression: `"{{.value}}"`},
					},
				},
			},
		}
		k8sClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(tpl).Build()
		var err error
		workflowExecutor, err = NewWorkflowExecutorWithAgentExecutor(
			k8sClient, nil, nil, nil, workflowRun, agent.NewMockAgentExecutor(), false, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())

		workflow := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "test-wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{
						Name: "useDefault",
						StepTemplateRef: &ottoflowv1alpha1.StepTemplateRef{
							Name:      "default-tpl",
							Arguments: map[string]string{},
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
		Expect(variables["result"]).To(Equal("default-value"))
	})

	It("should substitute Message and Expressions in template", func() {
		tpl := &ottoflowv1alpha1.StepTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "msg-tpl", Namespace: "default"},
			Spec: ottoflowv1alpha1.StepTemplateSpec{
				Parameters: []ottoflowv1alpha1.StepTemplateParameter{
					{Name: "name", Required: true},
					{Name: "n", Required: true},
				},
				Step: ottoflowv1alpha1.StepTemplateStep{
					Message: "Hello {{.name}}",
					Expressions: []ottoflowv1alpha1.Expression{
						{Name: "double", Expression: `{{.n}} * 2`},
					},
					Outputs: []ottoflowv1alpha1.Output{
						{Name: "greeting", Expression: `"{{.name}}"`},
						{Name: "num", Expression: `expressions.double`},
					},
				},
			},
		}
		k8sClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(tpl).Build()
		var err error
		workflowExecutor, err = NewWorkflowExecutorWithAgentExecutor(
			k8sClient, nil, nil, nil, workflowRun, agent.NewMockAgentExecutor(), false, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())

		workflow := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "test-wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{
						Name: "subst",
						StepTemplateRef: &ottoflowv1alpha1.StepTemplateRef{
							Name: "msg-tpl",
							Arguments: map[string]string{
								"name": `"alice"`,
								"n":    "3",
							},
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
		Expect(variables["greeting"]).To(Equal("alice"))
		// expressions.double is "3 * 2" evaluated as CEL = 6
		Expect(variables["num"]).To(Equal(int64(6)))
	})

	It("should substitute MatchConditions in template", func() {
		tpl := &ottoflowv1alpha1.StepTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "match-tpl", Namespace: "default"},
			Spec: ottoflowv1alpha1.StepTemplateSpec{
				Parameters: []ottoflowv1alpha1.StepTemplateParameter{
					{Name: "cond", Required: true},
				},
				Step: ottoflowv1alpha1.StepTemplateStep{
					MatchConditions: []ottoflowv1alpha1.MatchCondition{
						{Name: "allow", Expression: "{{.cond}}"},
					},
					Outputs: []ottoflowv1alpha1.Output{
						{Name: "ok", Expression: `"done"`},
					},
				},
			},
		}
		k8sClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(tpl).Build()
		var err error
		workflowExecutor, err = NewWorkflowExecutorWithAgentExecutor(
			k8sClient, nil, nil, nil, workflowRun, agent.NewMockAgentExecutor(), false, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())
		workflow := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "test-wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{
						Name: "useMatch",
						StepTemplateRef: &ottoflowv1alpha1.StepTemplateRef{
							Name:      "match-tpl",
							Arguments: map[string]string{"cond": "true"},
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
		Expect(variables["ok"]).To(Equal("done"))
	})

	It("should fail when required parameter is not provided", func() {
		tpl := &ottoflowv1alpha1.StepTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "required-tpl", Namespace: "default"},
			Spec: ottoflowv1alpha1.StepTemplateSpec{
				Parameters: []ottoflowv1alpha1.StepTemplateParameter{
					{Name: "required", Required: true},
				},
				Step: ottoflowv1alpha1.StepTemplateStep{
					Outputs: []ottoflowv1alpha1.Output{
						{Name: "out", Expression: `"ok"`},
					},
				},
			},
		}
		k8sClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(tpl).Build()
		var err error
		workflowExecutor, err = NewWorkflowExecutorWithAgentExecutor(
			k8sClient, nil, nil, nil, workflowRun, agent.NewMockAgentExecutor(), false, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())

		workflow := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "test-wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{
						Name: "missingRequired",
						StepTemplateRef: &ottoflowv1alpha1.StepTemplateRef{
							Name:      "required-tpl",
							Arguments: map[string]string{},
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, workflow)).To(Succeed())

		err = workflowExecutor.ExecuteWorkflow(ctx, workflow, workflowRun)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("required parameter"))
	})

	Describe("instantiateStepTemplate", func() {
		var workflowExecutor *WorkflowExecutor

		BeforeEach(func() {
			k8sClient = fake.NewClientBuilder().WithScheme(scheme).Build()
			var err error
			workflowExecutor, err = NewWorkflowExecutorWithAgentExecutor(
				k8sClient, nil, nil, nil, workflowRun, agent.NewMockAgentExecutor(), false, 0, 5, nil)
			Expect(err).NotTo(HaveOccurred())
		})

		It("substitutes Message and Expressions", func() {
			templateStep := ottoflowv1alpha1.StepTemplateStep{
				Message: "Hello {{.name}}",
				Expressions: []ottoflowv1alpha1.Expression{
					{Name: "x", Expression: "{{.expr}}"},
				},
			}
			args := map[string]interface{}{"name": "World", "expr": "1+1"}
			step, err := workflowExecutor.instantiateStepTemplate(templateStep, "s1", args)
			Expect(err).NotTo(HaveOccurred())
			Expect(step.Name).To(Equal("s1"))
			Expect(step.Message).To(Equal("Hello World"))
			Expect(step.Expressions).To(HaveLen(1))
			Expect(step.Expressions[0].Expression).To(Equal("1+1"))
		})

		It("copies every ExternalAgentRef field and substitutes Prompt", func() {
			ref := &ottoflowv1alpha1.StepExternalAgentRef{
				URL:               "http://kagent-controller.kagent.svc:8083",
				Protocol:          "a2a",
				Prompt:            "{{.p}}",
				Timeout:           "30s",
				AllowInsecureHTTP: true,
				Auth:              &ottoflowv1alpha1.ExternalAgentAuth{SecretRef: &ottoflowv1alpha1.SecretReference{Name: "tok", Key: "token"}},
				CASecretRef:       &ottoflowv1alpha1.NamespacedSecretRef{Name: "ca", Namespace: "kagent"},
			}
			templateStep := ottoflowv1alpha1.StepTemplateStep{ExternalAgentRef: ref}
			step, err := workflowExecutor.instantiateStepTemplate(templateStep, "s1", map[string]interface{}{"p": "hi"})
			Expect(err).NotTo(HaveOccurred())

			want := ref.DeepCopy()
			want.Prompt = "hi"
			Expect(step.ExternalAgentRef).To(Equal(want))
			Expect(step.ExternalAgentRef.Auth).NotTo(BeIdenticalTo(ref.Auth))
			Expect(step.ExternalAgentRef.Auth.SecretRef).NotTo(BeIdenticalTo(ref.Auth.SecretRef))
			Expect(step.ExternalAgentRef.CASecretRef).NotTo(BeIdenticalTo(ref.CASecretRef))
		})

		It("substitutes Outputs with Value.Raw", func() {
			templateStep := ottoflowv1alpha1.StepTemplateStep{
				Outputs: []ottoflowv1alpha1.Output{
					{Name: "tag", Value: &apiextensionsv1.JSON{Raw: []byte(`"{{.version}}"`)}},
				},
			}
			args := map[string]interface{}{"version": "v2"}
			step, err := workflowExecutor.instantiateStepTemplate(templateStep, "s1", args)
			Expect(err).NotTo(HaveOccurred())
			Expect(step.Outputs).To(HaveLen(1))
			Expect(string(step.Outputs[0].Value.Raw)).To(Equal(`"v2"`))
		})

		It("substitutes MatchConditions", func() {
			templateStep := ottoflowv1alpha1.StepTemplateStep{
				MatchConditions: []ottoflowv1alpha1.MatchCondition{
					{Name: "skip", Expression: "{{.cond}}"},
				},
			}
			args := map[string]interface{}{"cond": "variables.foo == 'bar'"}
			step, err := workflowExecutor.instantiateStepTemplate(templateStep, "s1", args)
			Expect(err).NotTo(HaveOccurred())
			Expect(step.MatchConditions).To(HaveLen(1))
			Expect(step.MatchConditions[0].Expression).To(Equal("variables.foo == 'bar'"))
		})

		It("substitutes ResourceQuery Namespace, Name, FieldSelector, LabelSelector and Outputs", func() {
			templateStep := ottoflowv1alpha1.StepTemplateStep{
				ResourceQuery: &ottoflowv1alpha1.StepResourceQuery{
					APIVersion:    "v1",
					Resource:      "configmaps",
					Namespace:     "{{.ns}}",
					Name:          "{{.resName}}",
					FieldSelector: "metadata.name={{.name}}",
					LabelSelector: map[string]string{"app": "{{.app}}"},
					Outputs:       map[string]string{"label": "{{.outExpr}}"},
				},
			}
			args := map[string]interface{}{"ns": "kube-system", "resName": "my-cm", "name": "x", "app": "foo", "outExpr": "metadata.labels.app"}
			step, err := workflowExecutor.instantiateStepTemplate(templateStep, "s1", args)
			Expect(err).NotTo(HaveOccurred())
			Expect(step.ResourceQuery).NotTo(BeNil())
			Expect(step.ResourceQuery.Namespace).To(Equal("kube-system"))
			Expect(step.ResourceQuery.Name).To(Equal("my-cm"))
			Expect(step.ResourceQuery.FieldSelector).To(Equal("metadata.name=x"))
			Expect(step.ResourceQuery.LabelSelector).To(HaveKeyWithValue("app", "foo"))
			Expect(step.ResourceQuery.Outputs).To(HaveKeyWithValue("label", "metadata.labels.app"))
		})

		It("returns error for invalid template syntax in message", func() {
			templateStep := ottoflowv1alpha1.StepTemplateStep{
				Message: "{{.unclosed",
			}
			_, err := workflowExecutor.instantiateStepTemplate(templateStep, "s1", map[string]interface{}{"x": "y"})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to substitute message"))
		})

		It("substitutes PrometheusQuery and ResourceQuery labelSelector", func() {
			templateStep := ottoflowv1alpha1.StepTemplateStep{
				PrometheusQuery: &ottoflowv1alpha1.StepPrometheusQuery{
					Query:     "up{job=\"{{.job}}\"}",
					TimeRange: "{{.range}}",
					Outputs:   map[string]string{"up": "result"},
				},
			}
			args := map[string]interface{}{"job": "api", "range": "5m"}
			step, err := workflowExecutor.instantiateStepTemplate(templateStep, "s1", args)
			Expect(err).NotTo(HaveOccurred())
			Expect(step.PrometheusQuery).NotTo(BeNil())
			Expect(step.PrometheusQuery.Query).To(Equal("up{job=\"api\"}"))
			Expect(step.PrometheusQuery.TimeRange).To(Equal("5m"))
		})

		It("substitutes AgentRef AdditionalPrompts", func() {
			templateStep := ottoflowv1alpha1.StepTemplateStep{
				AgentRef: &ottoflowv1alpha1.StepAgentRef{
					Name:              "my-agent",
					AdditionalPrompts: []string{"Use context: {{.ctx}}"},
				},
			}
			args := map[string]interface{}{"ctx": "production"}
			step, err := workflowExecutor.instantiateStepTemplate(templateStep, "s1", args)
			Expect(err).NotTo(HaveOccurred())
			Expect(step.AgentRef).NotTo(BeNil())
			Expect(step.AgentRef.AdditionalPrompts).To(Equal([]string{"Use context: production"}))
		})

		It("substitutes MCPToolCall Arguments and WorkflowRef Inputs", func() {
			templateStep := ottoflowv1alpha1.StepTemplateStep{
				MCPToolCall: &ottoflowv1alpha1.StepMCPToolCall{
					Server: "svr",
					Tool:   "tool",
					Arguments: map[string]string{
						"key": "{{.arg}}",
					},
				},
			}
			args := map[string]interface{}{"arg": "value"}
			step, err := workflowExecutor.instantiateStepTemplate(templateStep, "s1", args)
			Expect(err).NotTo(HaveOccurred())
			Expect(step.MCPToolCall).NotTo(BeNil())
			Expect(step.MCPToolCall.Arguments["key"]).To(Equal("value"))

			templateStep2 := ottoflowv1alpha1.StepTemplateStep{
				WorkflowRef: &ottoflowv1alpha1.StepWorkflowRef{
					Name:   "child",
					Inputs: map[string]string{"p": "{{.input}}"},
				},
			}
			args2 := map[string]interface{}{"input": "data"}
			step2, err := workflowExecutor.instantiateStepTemplate(templateStep2, "s2", args2)
			Expect(err).NotTo(HaveOccurred())
			Expect(step2.WorkflowRef).NotTo(BeNil())
			Expect(step2.WorkflowRef.Inputs["p"]).To(Equal("data"))
		})
	})
})
