/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"fmt"

	"github.com/go-logr/logr"
	celapi "github.com/google/cel-go/cel"
	metricsclientset "k8s.io/metrics/pkg/client/clientset/versioned"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

// rawExpression pairs an identifier with the CEL expression text.
type rawExpression struct {
	Name string // e.g. "var.threshold", "step.ping.expr.ts"
	Text string // the actual CEL source
}

// CELCompilationCache validates a Workflow's CEL expressions at reconcile time by
// compiling each one against a shared CEL environment. Compiled programs are
// discarded: the workflow-runner Job compiles its own in a separate process.
type CELCompilationCache struct {
	env      *celapi.Env
	progOpts []celapi.ProgramOption
	logger   logr.Logger
}

// NewCELCompilationCache creates a validator backed by a single CEL environment.
// Create one instance at controller startup and pass it to the Workflow reconciler.
func NewCELCompilationCache(
	k8sClient client.Client,
	metricsClient metricsclientset.Interface,
	customMetricsClient CustomMetricsClient,
	prometheusClient PrometheusClient,
	logger logr.Logger,
) (*CELCompilationCache, error) {
	// Programs compiled here are never evaluated, so the holder's context is unused.
	cacheHolder := &macroContextHolder{}
	env, progOpts, err := createCELEnvironment(k8sClient, nil, metricsClient, customMetricsClient, prometheusClient, "", cacheHolder)
	if err != nil {
		return nil, fmt.Errorf("failed to create CEL environment for compilation: %w", err)
	}

	return &CELCompilationCache{
		env:      env,
		progOpts: progOpts,
		logger:   logger.WithName("cel-cache"),
	}, nil
}

// WorkflowKey returns the canonical cache key for a workflow.
func WorkflowKey(namespace, name string) string {
	return namespace + "/" + name
}

// CompileWorkflow extracts every CEL expression from a Workflow and compiles it,
// returning one error per expression that fails. Programs are not retained.
// Uses the workflow's CELCostLimit (or default) for program options.
func (c *CELCompilationCache) CompileWorkflow(workflow *ottoflowv1alpha1.Workflow) []error {
	workflowKey := WorkflowKey(workflow.Namespace, workflow.Name)
	costLimit := ResolveCELCostLimit(&workflow.Spec)
	exprs := extractCELExpressions(workflow)

	var errs []error
	for _, raw := range exprs {
		if _, err := c.compileProgram(raw.Text, costLimit); err != nil {
			errs = append(errs, fmt.Errorf("expression %q (%s): %w", raw.Name, raw.Text, err))
		}
	}

	c.logger.Info("Compiled workflow CEL expressions",
		"workflow", workflowKey,
		"total", len(exprs),
		"compiled", len(exprs)-len(errs),
		"errors", len(errs))
	return errs
}

// compileProgram compiles a single CEL expression into an executable program.
func (c *CELCompilationCache) compileProgram(expr string, costLimit uint64) (celapi.Program, error) {
	ast, issues := c.env.Compile(expr)
	if issues != nil && issues.Err() != nil {
		return nil, fmt.Errorf("compilation failed: %w", issues.Err())
	}
	if costLimit == 0 {
		costLimit = DefaultCELCostLimit
	}
	programOpts := []celapi.ProgramOption{
		celapi.EvalOptions(celapi.OptOptimize),
		celapi.CostLimit(costLimit),
	}
	if len(c.progOpts) > 0 {
		programOpts = append(programOpts, c.progOpts...)
	}

	prg, err := c.env.Program(ast, programOpts...)
	if err != nil {
		return nil, fmt.Errorf("program creation failed: %w", err)
	}
	return prg, nil
}

// extractCELExpressions walks a Workflow and returns every CEL expression
// paired with a human-readable identifier.
func extractCELExpressions(workflow *ottoflowv1alpha1.Workflow) []rawExpression {
	var exprs []rawExpression

	// Workflow-level variables
	for _, v := range workflow.Spec.Variables {
		exprs = append(exprs, rawExpression{Name: "var." + v.Name, Text: v.Expression})
	}

	// Workflow-level outputs
	for _, o := range workflow.Spec.Outputs {
		if o.Expression != "" {
			exprs = append(exprs, rawExpression{Name: "out." + o.Name, Text: o.Expression})
		}
		if o.Metric != nil {
			for _, l := range o.Metric.Labels {
				exprs = append(exprs, rawExpression{Name: "out." + o.Name + ".metric.label." + l.Name, Text: l.Value})
			}
		}
	}

	// Steps
	for _, step := range workflow.Spec.Steps {
		prefix := "step." + step.Name
		extractStepExpressions(&exprs, prefix, &step)
	}

	return exprs
}

// extractStepExpressions appends all CEL expressions from a single Step.
func extractStepExpressions(exprs *[]rawExpression, prefix string, step *ottoflowv1alpha1.Step) {
	for _, e := range step.Expressions {
		*exprs = append(*exprs, rawExpression{Name: prefix + ".expr." + e.Name, Text: e.Expression})
	}

	for _, o := range step.Outputs {
		if o.Expression != "" {
			*exprs = append(*exprs, rawExpression{Name: prefix + ".out." + o.Name, Text: o.Expression})
		}
		if o.Metric != nil {
			for _, l := range o.Metric.Labels {
				*exprs = append(*exprs, rawExpression{Name: prefix + ".out." + o.Name + ".metric.label." + l.Name, Text: l.Value})
			}
		}
	}

	for _, mc := range step.MatchConditions {
		*exprs = append(*exprs, rawExpression{Name: prefix + ".match." + mc.Name, Text: mc.Expression})
	}

	if step.WorkflowRef != nil {
		for k, v := range step.WorkflowRef.Inputs {
			*exprs = append(*exprs, rawExpression{Name: prefix + ".wfRef.input." + k, Text: v})
		}
	}

	if step.MCPToolCall != nil {
		for k, v := range step.MCPToolCall.Arguments {
			*exprs = append(*exprs, rawExpression{Name: prefix + ".mcp.arg." + k, Text: v})
		}
	}

	if step.ResourceQuery != nil {
		for k, v := range step.ResourceQuery.Outputs {
			*exprs = append(*exprs, rawExpression{Name: prefix + ".rq.out." + k, Text: v})
		}
	}

	if step.PrometheusQuery != nil {
		for k, v := range step.PrometheusQuery.Variables {
			*exprs = append(*exprs, rawExpression{Name: prefix + ".pq.var." + k, Text: v})
		}
		for k, v := range step.PrometheusQuery.Outputs {
			*exprs = append(*exprs, rawExpression{Name: prefix + ".pq.out." + k, Text: v})
		}
	}

	if step.Mutate != nil {
		if step.Mutate.ApplyConfiguration != nil && step.Mutate.ApplyConfiguration.Expression != "" {
			*exprs = append(*exprs, rawExpression{Name: prefix + ".mutate.apply", Text: step.Mutate.ApplyConfiguration.Expression})
		}
		if step.Mutate.JSONPatch != nil && step.Mutate.JSONPatch.Expression != "" {
			*exprs = append(*exprs, rawExpression{Name: prefix + ".mutate.jsonPatch", Text: step.Mutate.JSONPatch.Expression})
		}
		for k, v := range step.Mutate.Outputs {
			*exprs = append(*exprs, rawExpression{Name: prefix + ".mutate.out." + k, Text: v})
		}
	}

	if step.StepTemplateRef != nil {
		for k, v := range step.StepTemplateRef.Arguments {
			*exprs = append(*exprs, rawExpression{Name: prefix + ".tpl.arg." + k, Text: v})
		}
	}

	if step.ForEach != nil {
		*exprs = append(*exprs, rawExpression{Name: prefix + ".forEach.items", Text: step.ForEach.Items})
		if step.ForEach.Step != nil {
			extractForEachStepExpressions(exprs, prefix+".forEach", step.ForEach.Step)
		}
		if step.ForEach.StepTemplateRef != nil {
			for k, v := range step.ForEach.StepTemplateRef.Arguments {
				*exprs = append(*exprs, rawExpression{Name: prefix + ".forEach.tpl.arg." + k, Text: v})
			}
		}
	}
}

// extractForEachStepExpressions appends CEL expressions from a forEach inline step.
func extractForEachStepExpressions(exprs *[]rawExpression, prefix string, step *ottoflowv1alpha1.StepForEachStep) {
	for _, e := range step.Expressions {
		*exprs = append(*exprs, rawExpression{Name: prefix + ".expr." + e.Name, Text: e.Expression})
	}

	for _, o := range step.Outputs {
		if o.Expression != "" {
			*exprs = append(*exprs, rawExpression{Name: prefix + ".out." + o.Name, Text: o.Expression})
		}
	}

	for _, mc := range step.MatchConditions {
		*exprs = append(*exprs, rawExpression{Name: prefix + ".match." + mc.Name, Text: mc.Expression})
	}

	if step.WorkflowRef != nil {
		for k, v := range step.WorkflowRef.Inputs {
			*exprs = append(*exprs, rawExpression{Name: prefix + ".wfRef.input." + k, Text: v})
		}
	}

	if step.MCPToolCall != nil {
		for k, v := range step.MCPToolCall.Arguments {
			*exprs = append(*exprs, rawExpression{Name: prefix + ".mcp.arg." + k, Text: v})
		}
	}

	if step.ResourceQuery != nil {
		for k, v := range step.ResourceQuery.Outputs {
			*exprs = append(*exprs, rawExpression{Name: prefix + ".rq.out." + k, Text: v})
		}
	}

	if step.PrometheusQuery != nil {
		for k, v := range step.PrometheusQuery.Variables {
			*exprs = append(*exprs, rawExpression{Name: prefix + ".pq.var." + k, Text: v})
		}
		for k, v := range step.PrometheusQuery.Outputs {
			*exprs = append(*exprs, rawExpression{Name: prefix + ".pq.out." + k, Text: v})
		}
	}

	if step.Mutate != nil {
		if step.Mutate.ApplyConfiguration != nil && step.Mutate.ApplyConfiguration.Expression != "" {
			*exprs = append(*exprs, rawExpression{Name: prefix + ".mutate.apply", Text: step.Mutate.ApplyConfiguration.Expression})
		}
		if step.Mutate.JSONPatch != nil && step.Mutate.JSONPatch.Expression != "" {
			*exprs = append(*exprs, rawExpression{Name: prefix + ".mutate.jsonPatch", Text: step.Mutate.JSONPatch.Expression})
		}
		for k, v := range step.Mutate.Outputs {
			*exprs = append(*exprs, rawExpression{Name: prefix + ".mutate.out." + k, Text: v})
		}
	}

}
