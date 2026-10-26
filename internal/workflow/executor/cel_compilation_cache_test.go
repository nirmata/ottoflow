/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2/textlogger"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

var _ = Describe("CELCompilationCache", func() {
	var (
		k8sClient client.Client
		cache     *CELCompilationCache
	)

	BeforeEach(func() {
		scheme := runtime.NewScheme()
		utilruntime.Must(ottoflowv1alpha1.AddToScheme(scheme))
		utilruntime.Must(clientgoscheme.AddToScheme(scheme))
		k8sClient = fake.NewClientBuilder().WithScheme(scheme).Build()

		var err error
		cache, err = NewCELCompilationCache(k8sClient, nil, nil, nil, textlogger.NewLogger(textlogger.NewConfig()))
		Expect(err).NotTo(HaveOccurred())
		Expect(cache).NotTo(BeNil())
	})

	It("should compile valid CEL expressions", func() {
		workflow := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "test-wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Variables: []ottoflowv1alpha1.Variable{
					{Name: "threshold", Expression: "42"},
				},
				Steps: []ottoflowv1alpha1.Step{
					{
						Name: "compute",
						Expressions: []ottoflowv1alpha1.Expression{
							{Name: "sum", Expression: "1 + 2"},
							{Name: "msg", Expression: `"hello " + "world"`},
						},
						Outputs: []ottoflowv1alpha1.Output{
							{Name: "result", Expression: "expressions.sum * 10"},
						},
					},
				},
				Outputs: []ottoflowv1alpha1.Output{
					{Name: "final", Expression: "variables.threshold"},
				},
			},
		}

		errs := cache.CompileWorkflow(workflow)
		Expect(errs).To(BeEmpty())

		programs := exprTexts(workflow)
		Expect(programs).NotTo(BeNil())
		Expect(programs).To(HaveLen(5))
		Expect(programs).To(HaveKey("42"))
		Expect(programs).To(HaveKey("1 + 2"))
		Expect(programs).To(HaveKey(`"hello " + "world"`))
		Expect(programs).To(HaveKey("expressions.sum * 10"))
		Expect(programs).To(HaveKey("variables.threshold"))
	})

	It("should compile workflow with ForEach step expressions", func() {
		workflow := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "foreach-wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{
						Name: "fe",
						ForEach: &ottoflowv1alpha1.StepForEach{
							Items: `["a", "b"]`,
							Step: &ottoflowv1alpha1.StepForEachStep{
								Expressions: []ottoflowv1alpha1.Expression{{Name: "e", Expression: `variables.item + "-suffix"`}},
								Outputs:     []ottoflowv1alpha1.Output{{Name: "out", Expression: `expressions.e`}},
							},
						},
					},
				},
			},
		}
		errs := cache.CompileWorkflow(workflow)
		Expect(errs).To(BeEmpty())
		programs := exprTexts(workflow)
		Expect(programs).NotTo(BeNil())
		Expect(programs).To(HaveKey(`["a", "b"]`))
		Expect(programs).To(HaveKey(`variables.item + "-suffix"`))
		Expect(programs).To(HaveKey("expressions.e"))
	})

	It("should report compilation errors for invalid expressions", func() {
		workflow := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "bad-wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{
						Name: "broken",
						Expressions: []ottoflowv1alpha1.Expression{
							{Name: "ok", Expression: "1 + 1"},
							{Name: "bad", Expression: "???invalid!!!"},
						},
					},
				},
			},
		}

		errs := cache.CompileWorkflow(workflow)
		Expect(errs).To(HaveLen(1))
		Expect(errs[0].Error()).To(ContainSubstring("bad"))
	})

	It("should compile step with WorkflowRef, MCPToolCall, ResourceQuery, PrometheusQuery, Mutate, Output Metric (extractStepExpressions branches)", func() {
		workflow := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "full-step-wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{
						Name: "full",
						WorkflowRef: &ottoflowv1alpha1.StepWorkflowRef{
							Name:   "sub",
							Inputs: map[string]string{"p": "variables.x"},
						},
						MCPToolCall: &ottoflowv1alpha1.StepMCPToolCall{
							Server:    "srv",
							Tool:      "t",
							Arguments: map[string]string{"arg": "1"},
						},
						ResourceQuery: &ottoflowv1alpha1.StepResourceQuery{
							APIVersion: "v1",
							Resource:   "pods",
							Outputs:    map[string]string{"name": "string(\"x\")"},
						},
						PrometheusQuery: &ottoflowv1alpha1.StepPrometheusQuery{
							Query:     "up",
							Variables: map[string]string{"v": "\"5m\""},
							Outputs:   map[string]string{"val": "1"},
						},
						Mutate: &ottoflowv1alpha1.StepMutate{
							Target:             ottoflowv1alpha1.StepMutateTarget{APIVersion: "v1", Resource: "Pod", Name: "x"},
							PatchType:          "ApplyConfiguration",
							ApplyConfiguration: &ottoflowv1alpha1.MutateApplyConfiguration{Expression: "object"},
							JSONPatch:          &ottoflowv1alpha1.MutateJSONPatch{Expression: "[]"},
							Outputs:            map[string]string{"o": "object.metadata.name"},
						},
						Outputs: []ottoflowv1alpha1.Output{
							{Name: "out", Expression: "1", Metric: &ottoflowv1alpha1.OutputMetric{Name: "m", Type: "gauge", Labels: []ottoflowv1alpha1.MetricLabel{{Name: "l", Value: "\"v\""}}}},
						},
					},
				},
			},
		}
		errs := cache.CompileWorkflow(workflow)
		Expect(errs).To(BeEmpty())
		programs := exprTexts(workflow)
		Expect(programs).NotTo(BeNil())
		Expect(programs).To(HaveKey("variables.x"))
		Expect(programs).To(HaveKey("1"))
		Expect(programs).To(HaveKey("string(\"x\")"))
		Expect(programs).To(HaveKey("\"5m\""))
		Expect(programs).To(HaveKey("1"))
		Expect(programs).To(HaveKey("object"))
		Expect(programs).To(HaveKey("[]"))
		Expect(programs).To(HaveKey("object.metadata.name"))
		Expect(programs).To(HaveKey("\"v\""))
	})

	It("should compile forEach step with WorkflowRef, MCP, ResourceQuery, PrometheusQuery, Mutate (extractForEachStepExpressions)", func() {
		workflow := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "foreach-full-wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{
						Name: "fe",
						ForEach: &ottoflowv1alpha1.StepForEach{
							Items: `[1]`,
							Step: &ottoflowv1alpha1.StepForEachStep{
								WorkflowRef:     &ottoflowv1alpha1.StepWorkflowRef{Name: "w", Inputs: map[string]string{"i": "item"}},
								MCPToolCall:     &ottoflowv1alpha1.StepMCPToolCall{Server: "s", Tool: "t", Arguments: map[string]string{"a": "item"}},
								ResourceQuery:   &ottoflowv1alpha1.StepResourceQuery{APIVersion: "v1", Resource: "pods", Outputs: map[string]string{"n": "string(\"n\")"}},
								PrometheusQuery: &ottoflowv1alpha1.StepPrometheusQuery{Query: "up", Variables: map[string]string{"t": "\"5m\""}, Outputs: map[string]string{"v": "1"}},
								Mutate: &ottoflowv1alpha1.StepMutate{
									Target:             ottoflowv1alpha1.StepMutateTarget{APIVersion: "v1", Resource: "Pod", Name: "p"},
									PatchType:          "ApplyConfiguration",
									ApplyConfiguration: &ottoflowv1alpha1.MutateApplyConfiguration{Expression: "object"},
									Outputs:            map[string]string{"x": "object"},
								},
								MatchConditions: []ottoflowv1alpha1.MatchCondition{{Name: "m", Expression: "true"}},
							},
						},
					},
				},
			},
		}
		errs := cache.CompileWorkflow(workflow)
		Expect(errs).To(BeEmpty())
		programs := exprTexts(workflow)
		Expect(programs).NotTo(BeNil())
		Expect(programs).To(HaveKey("item"))
		Expect(programs).To(HaveKey("object"))
		Expect(programs).To(HaveKey("true"))
	})

	It("should compile matchConditions and forEach expressions", func() {
		workflow := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "complex-wf", Namespace: "ns"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{
						Name: "cond",
						MatchConditions: []ottoflowv1alpha1.MatchCondition{
							{Name: "check", Expression: "true"},
						},
						Expressions: []ottoflowv1alpha1.Expression{
							{Name: "val", Expression: `"ok"`},
						},
					},
					{
						Name: "loop",
						ForEach: &ottoflowv1alpha1.StepForEach{
							Items: `[1, 2, 3]`,
							Step: &ottoflowv1alpha1.StepForEachStep{
								Expressions: []ottoflowv1alpha1.Expression{
									{Name: "doubled", Expression: "item * 2"},
								},
								Outputs: []ottoflowv1alpha1.Output{
									{Name: "res", Expression: "expressions.doubled"},
								},
							},
						},
					},
				},
			},
		}

		errs := cache.CompileWorkflow(workflow)
		Expect(errs).To(BeEmpty())

		programs := exprTexts(workflow)
		Expect(programs).NotTo(BeNil())
		Expect(programs).To(HaveKey("true"))
		Expect(programs).To(HaveKey(`"ok"`))
		Expect(programs).To(HaveKey("[1, 2, 3]"))
		Expect(programs).To(HaveKey("item * 2"))
		Expect(programs).To(HaveKey("expressions.doubled"))
	})

})

var _ = Describe("extractCELExpressions", func() {
	It("should extract all CEL expression types from a workflow", func() {
		workflow := &ottoflowv1alpha1.Workflow{
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Variables: []ottoflowv1alpha1.Variable{
					{Name: "v1", Expression: "expr_v1"},
				},
				Steps: []ottoflowv1alpha1.Step{
					{
						Name: "step1",
						Expressions: []ottoflowv1alpha1.Expression{
							{Name: "e1", Expression: "expr_e1"},
						},
						Outputs: []ottoflowv1alpha1.Output{
							{Name: "o1", Expression: "expr_o1"},
						},
						MatchConditions: []ottoflowv1alpha1.MatchCondition{
							{Name: "mc1", Expression: "expr_mc1"},
						},
						MCPToolCall: &ottoflowv1alpha1.StepMCPToolCall{
							Server:    "srv",
							Tool:      "tool",
							Arguments: map[string]string{"arg1": "expr_arg1"},
						},
						ResourceQuery: &ottoflowv1alpha1.StepResourceQuery{
							APIVersion: "v1",
							Resource:   "Pod",
							Outputs:    map[string]string{"rqo": "expr_rqo"},
						},
					},
				},
				Outputs: []ottoflowv1alpha1.Output{
					{Name: "wo1", Expression: "expr_wo1"},
				},
			},
		}

		exprs := extractCELExpressions(workflow)

		byName := make(map[string]string, len(exprs))
		for _, e := range exprs {
			byName[e.Name] = e.Text
		}

		Expect(byName).To(HaveKeyWithValue("var.v1", "expr_v1"))
		Expect(byName).To(HaveKeyWithValue("step.step1.expr.e1", "expr_e1"))
		Expect(byName).To(HaveKeyWithValue("step.step1.out.o1", "expr_o1"))
		Expect(byName).To(HaveKeyWithValue("step.step1.match.mc1", "expr_mc1"))
		Expect(byName).To(HaveKeyWithValue("step.step1.mcp.arg.arg1", "expr_arg1"))
		Expect(byName).To(HaveKeyWithValue("step.step1.rq.out.rqo", "expr_rqo"))
		Expect(byName).To(HaveKeyWithValue("out.wo1", "expr_wo1"))
	})

	It("should extract forEach step expressions", func() {
		workflow := &ottoflowv1alpha1.Workflow{
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{
						Name: "loop",
						ForEach: &ottoflowv1alpha1.StepForEach{
							Items: "items_expr",
							Step: &ottoflowv1alpha1.StepForEachStep{
								Expressions: []ottoflowv1alpha1.Expression{
									{Name: "fe1", Expression: "fe_expr1"},
								},
								Outputs: []ottoflowv1alpha1.Output{
									{Name: "feo", Expression: "fe_out_expr"},
								},
								MatchConditions: []ottoflowv1alpha1.MatchCondition{
									{Name: "femc", Expression: "fe_match_expr"},
								},
							},
						},
					},
				},
			},
		}

		exprs := extractCELExpressions(workflow)

		byName := make(map[string]string, len(exprs))
		for _, e := range exprs {
			byName[e.Name] = e.Text
		}

		Expect(byName).To(HaveKeyWithValue("step.loop.forEach.items", "items_expr"))
		Expect(byName).To(HaveKeyWithValue("step.loop.forEach.expr.fe1", "fe_expr1"))
		Expect(byName).To(HaveKeyWithValue("step.loop.forEach.out.feo", "fe_out_expr"))
		Expect(byName).To(HaveKeyWithValue("step.loop.forEach.match.femc", "fe_match_expr"))
	})

	It("should handle empty workflow", func() {
		workflow := &ottoflowv1alpha1.Workflow{
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{Name: "noop"},
				},
			},
		}
		exprs := extractCELExpressions(workflow)
		Expect(exprs).To(BeEmpty())
	})
})

// exprTexts returns the set of expression texts the walker extracts from a workflow.
func exprTexts(wf *ottoflowv1alpha1.Workflow) map[string]struct{} {
	out := map[string]struct{}{}
	for _, e := range extractCELExpressions(wf) {
		out[e.Text] = struct{}{}
	}
	return out
}
