/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package webhook

import (
	"fmt"
	"regexp"
	"strings"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

// steps.<stepName> or steps.<stepName>.field..., and stepStatus.<stepName>...
var stepsRefRe = regexp.MustCompile(`(?:steps|stepStatus)\.([a-z][a-zA-Z0-9]*)`)

// variables.<name>
var variablesRefRe = regexp.MustCompile(`variables\.([a-zA-Z][a-zA-Z0-9]*)`)

// inputs.<name>
var inputsRefRe = regexp.MustCompile(`inputs\.([a-zA-Z_][a-zA-Z0-9_]*)`)

// celStringLiteralRe matches CEL double-quoted string literals (including escape sequences).
// Used to strip string literal content before scanning for identifier references so that
// occurrences of inputs.* inside quoted strings don't produce false-positive UNDEFINED_INPUT errors.
var celStringLiteralRe = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)

// ValidateStepDependencies checks that any step referencing another step's output
// (via steps.* or variables.* from a step output) has the corresponding dependency in dependsOn.
func ValidateStepDependencies(spec *ottoflowv1alpha1.WorkflowSpec) error {
	if spec == nil || len(spec.Steps) == 0 {
		return nil
	}

	stepNames := make(map[string]struct{})
	for _, s := range spec.Steps {
		stepNames[s.Name] = struct{}{}
	}

	// Workflow-level variable names (no step dependency needed)
	workflowVars := make(map[string]struct{})
	for _, v := range spec.Variables {
		workflowVars[v.Name] = struct{}{}
	}

	// variable name -> step names that produce it (step.Outputs[].Name)
	variableToSteps := make(map[string][]string)
	for i := range spec.Steps {
		s := &spec.Steps[i]
		for _, o := range s.Outputs {
			if o.Name != "" {
				variableToSteps[o.Name] = append(variableToSteps[o.Name], s.Name)
			}
		}
		// ForEach inline step outputs are written by the forEach step itself (parent step name)
		if s.ForEach != nil && s.ForEach.Step != nil {
			for _, o := range s.ForEach.Step.Outputs {
				if o.Name != "" {
					variableToSteps[o.Name] = append(variableToSteps[o.Name], s.Name)
				}
			}
		}
	}

	dependsOnSet := func(deps []string) map[string]struct{} {
		m := make(map[string]struct{}, len(deps))
		for _, d := range deps {
			m[d] = struct{}{}
		}
		return m
	}

	for i := range spec.Steps {
		step := &spec.Steps[i]
		exprs := collectCELStringsFromStep(step)
		stepRefs := extractStepRefs(exprs)
		varRefs := extractVariableRefs(exprs)

		deps := dependsOnSet(step.DependsOn)

		for ref := range stepRefs {
			if _, exists := stepNames[ref]; !exists {
				continue // reference to non-existent step; DAG validation will catch invalid dependsOn
			}
			if ref == step.Name {
				continue // self-reference is a no-op for dependency
			}
			if _, ok := deps[ref]; !ok {
				return fmt.Errorf("step %q references steps.%s but does not list %q in dependsOn (dependencies must be explicit)", step.Name, ref, ref)
			}
		}

		for varName := range varRefs {
			if _, ok := workflowVars[varName]; ok {
				continue // workflow-level variable, no step dependency
			}
			producerSteps := variableToSteps[varName]
			if len(producerSteps) == 0 {
				continue // variable not produced by any step (e.g. inputs, or typo; runtime will fail)
			}
			if !hasProducerDependency(step.Name, producerSteps, deps) {
				return fmt.Errorf("step %q references variables.%s but lists none of its producing steps (%s) in dependsOn (dependencies must be explicit)",
					step.Name, varName, strings.Join(producerSteps, ", "))
			}
		}
	}

	// ForEach steps: validate inline step expressions if present (they run in parent workflow context)
	for i := range spec.Steps {
		step := &spec.Steps[i]
		if step.ForEach == nil || step.ForEach.Step == nil {
			continue
		}
		inner := step.ForEach.Step
		exprs := collectCELStringsFromForEachStep(inner)
		stepRefs := extractStepRefs(exprs)
		varRefs := extractVariableRefs(exprs)
		deps := dependsOnSet(step.DependsOn)

		for ref := range stepRefs {
			if _, exists := stepNames[ref]; !exists {
				continue
			}
			if ref == step.Name {
				continue
			}
			if _, ok := deps[ref]; !ok {
				return fmt.Errorf("step %q forEach references steps.%s but does not list %q in dependsOn", step.Name, ref, ref)
			}
		}
		for varName := range varRefs {
			if _, ok := workflowVars[varName]; ok {
				continue
			}
			producerSteps := variableToSteps[varName]
			if len(producerSteps) == 0 {
				continue
			}
			if !hasProducerDependency(step.Name, producerSteps, deps) {
				return fmt.Errorf("step %q forEach references variables.%s but lists none of its producing steps (%s) in dependsOn",
					step.Name, varName, strings.Join(producerSteps, ", "))
			}
		}
	}

	return nil
}

// hasProducerDependency reports whether stepName may read a variable written by producers,
// i.e. whether it declares a dependency on at least one of them (or writes it itself).
//
// One declared producer is enough. Requiring a dependency on *every* producer is wrong
// twice over: it forces edges the workflow does not need, and where two steps write the
// same variable name it manufactures a cycle out of a valid workflow -- the referencing
// step is told to depend on a downstream producer that already depends on it.
//
// Note that two steps writing one variable name is itself ambiguous at runtime (last write
// wins, and under concurrency the order is not fixed). Validation deliberately does not
// reject it, but it is worth avoiding in workflow design.
func hasProducerDependency(stepName string, producers []string, deps map[string]struct{}) bool {
	for _, prod := range producers {
		if prod == stepName {
			return true // the step writes this variable itself
		}
		if _, ok := deps[prod]; ok {
			return true
		}
	}
	return false
}

func extractStepRefs(exprs []string) map[string]struct{} {
	out := make(map[string]struct{})
	for _, s := range exprs {
		for _, m := range stepsRefRe.FindAllStringSubmatch(s, -1) {
			if len(m) >= 2 {
				out[m[1]] = struct{}{}
			}
		}
	}
	return out
}

func extractVariableRefs(exprs []string) map[string]struct{} {
	out := make(map[string]struct{})
	for _, s := range exprs {
		for _, m := range variablesRefRe.FindAllStringSubmatch(s, -1) {
			if len(m) >= 2 {
				out[m[1]] = struct{}{}
			}
		}
	}
	return out
}

func extractInputRefs(exprs []string) map[string]struct{} {
	out := make(map[string]struct{})
	for _, s := range exprs {
		// Strip string literals so that inputs.* inside quoted strings (e.g. in prompt text
		// or label values) don't produce false-positive UNDEFINED_INPUT errors.
		stripped := celStringLiteralRe.ReplaceAllString(s, `""`)
		for _, m := range inputsRefRe.FindAllStringSubmatch(stripped, -1) {
			if len(m) >= 2 {
				out[m[1]] = struct{}{}
			}
		}
	}
	return out
}

// ValidateInputRefs checks that all inputs.<key> references in CEL expressions correspond
// to declared workflow inputs (spec.inputs[].name).
func ValidateInputRefs(spec *ottoflowv1alpha1.WorkflowSpec) error {
	if spec == nil {
		return nil
	}
	declared := make(map[string]struct{}, len(spec.Inputs))
	for _, inp := range spec.Inputs {
		declared[inp.Name] = struct{}{}
	}

	for i := range spec.Steps {
		step := &spec.Steps[i]
		for ref := range extractInputRefs(collectCELStringsFromStep(step)) {
			if _, ok := declared[ref]; !ok {
				return fmt.Errorf("step %q references inputs.%s which is not declared in workflow inputs", step.Name, ref)
			}
		}
		if step.ForEach != nil && step.ForEach.Step != nil {
			for ref := range extractInputRefs(collectCELStringsFromForEachStep(step.ForEach.Step)) {
				if _, ok := declared[ref]; !ok {
					return fmt.Errorf("step %q forEach references inputs.%s which is not declared in workflow inputs", step.Name, ref)
				}
			}
		}
	}
	for _, v := range spec.Variables {
		for ref := range extractInputRefs([]string{v.Expression}) {
			if _, ok := declared[ref]; !ok {
				return fmt.Errorf("variable %q references inputs.%s which is not declared in workflow inputs", v.Name, ref)
			}
		}
	}
	for _, o := range spec.Outputs {
		if o.Expression == "" {
			continue
		}
		for ref := range extractInputRefs([]string{o.Expression}) {
			if _, ok := declared[ref]; !ok {
				return fmt.Errorf("workflow output %q references inputs.%s which is not declared in workflow inputs", o.Name, ref)
			}
		}
	}
	return nil
}

// collectCELStringsFromStep gathers all string fields that may contain CEL expressions for the given step.
func collectCELStringsFromStep(step *ottoflowv1alpha1.Step) []string {
	var out []string

	for _, e := range step.Expressions {
		if e.Expression != "" {
			out = append(out, e.Expression)
		}
	}
	for _, o := range step.Outputs {
		if o.Expression != "" {
			out = append(out, o.Expression)
		}
		if o.Value != nil && len(o.Value.Raw) > 0 {
			out = append(out, string(o.Value.Raw))
		}
	}
	for _, m := range step.MatchConditions {
		if m.Expression != "" {
			out = append(out, m.Expression)
		}
	}
	if step.WorkflowRef != nil {
		for _, v := range step.WorkflowRef.Inputs {
			if v != "" {
				out = append(out, v)
			}
		}
	}
	if step.AgentRef != nil {
		for _, p := range step.AgentRef.AdditionalPrompts {
			if p != "" {
				out = append(out, p)
			}
		}
	}
	if step.MCPToolCall != nil {
		for _, v := range step.MCPToolCall.Arguments {
			if v != "" {
				out = append(out, v)
			}
		}
	}
	if step.ResourceQuery != nil {
		if step.ResourceQuery.Namespace != "" {
			out = append(out, step.ResourceQuery.Namespace)
		}
		if step.ResourceQuery.Name != "" {
			out = append(out, step.ResourceQuery.Name)
		}
		if step.ResourceQuery.FieldSelector != "" {
			out = append(out, step.ResourceQuery.FieldSelector)
		}
		for _, v := range step.ResourceQuery.Outputs {
			if v != "" {
				out = append(out, v)
			}
		}
		for _, v := range step.ResourceQuery.LabelSelector {
			if v != "" {
				out = append(out, v)
			}
		}
	}
	if step.PrometheusQuery != nil {
		for _, v := range step.PrometheusQuery.Variables {
			if v != "" {
				out = append(out, v)
			}
		}
		for _, v := range step.PrometheusQuery.Outputs {
			if v != "" {
				out = append(out, v)
			}
		}
	}
	if step.Mutate != nil {
		if step.Mutate.Target.Namespace != "" {
			out = append(out, step.Mutate.Target.Namespace)
		}
		if step.Mutate.Target.Name != "" {
			out = append(out, step.Mutate.Target.Name)
		}
		if step.Mutate.ApplyConfiguration != nil && step.Mutate.ApplyConfiguration.Expression != "" {
			out = append(out, step.Mutate.ApplyConfiguration.Expression)
		}
		if step.Mutate.JSONPatch != nil && step.Mutate.JSONPatch.Expression != "" {
			out = append(out, step.Mutate.JSONPatch.Expression)
		}
		for _, v := range step.Mutate.Outputs {
			if v != "" {
				out = append(out, v)
			}
		}
	}
	if step.StepTemplateRef != nil {
		for _, v := range step.StepTemplateRef.Arguments {
			if v != "" {
				out = append(out, v)
			}
		}
	}
	if step.ForEach != nil {
		if step.ForEach.Items != "" {
			out = append(out, step.ForEach.Items)
		}
	}
	if step.ExternalAgentRef != nil && step.ExternalAgentRef.Prompt != "" {
		out = append(out, step.ExternalAgentRef.Prompt)
	}
	out = append(out, collectCELStringsFromOpenReport(step.OpenReport)...)
	return out
}

func collectCELStringsFromForEachStep(inner *ottoflowv1alpha1.StepForEachStep) []string {
	var out []string
	for _, e := range inner.Expressions {
		if e.Expression != "" {
			out = append(out, e.Expression)
		}
	}
	for _, o := range inner.Outputs {
		if o.Expression != "" {
			out = append(out, o.Expression)
		}
		if o.Value != nil && len(o.Value.Raw) > 0 {
			out = append(out, string(o.Value.Raw))
		}
	}
	for _, m := range inner.MatchConditions {
		if m.Expression != "" {
			out = append(out, m.Expression)
		}
	}
	if inner.ResourceQuery != nil {
		if inner.ResourceQuery.Namespace != "" {
			out = append(out, inner.ResourceQuery.Namespace)
		}
		if inner.ResourceQuery.Name != "" {
			out = append(out, inner.ResourceQuery.Name)
		}
		if inner.ResourceQuery.FieldSelector != "" {
			out = append(out, inner.ResourceQuery.FieldSelector)
		}
		for _, v := range inner.ResourceQuery.Outputs {
			if v != "" {
				out = append(out, v)
			}
		}
		for _, v := range inner.ResourceQuery.LabelSelector {
			if v != "" {
				out = append(out, v)
			}
		}
	}
	if inner.PrometheusQuery != nil {
		for _, v := range inner.PrometheusQuery.Variables {
			if v != "" {
				out = append(out, v)
			}
		}
		for _, v := range inner.PrometheusQuery.Outputs {
			if v != "" {
				out = append(out, v)
			}
		}
	}
	if inner.Mutate != nil {
		if inner.Mutate.Target.Namespace != "" {
			out = append(out, inner.Mutate.Target.Namespace)
		}
		if inner.Mutate.Target.Name != "" {
			out = append(out, inner.Mutate.Target.Name)
		}
		if inner.Mutate.ApplyConfiguration != nil && inner.Mutate.ApplyConfiguration.Expression != "" {
			out = append(out, inner.Mutate.ApplyConfiguration.Expression)
		}
		if inner.Mutate.JSONPatch != nil && inner.Mutate.JSONPatch.Expression != "" {
			out = append(out, inner.Mutate.JSONPatch.Expression)
		}
		for _, v := range inner.Mutate.Outputs {
			if v != "" {
				out = append(out, v)
			}
		}
	}
	if inner.AgentRef != nil {
		for _, p := range inner.AgentRef.AdditionalPrompts {
			if p != "" {
				out = append(out, p)
			}
		}
	}
	if inner.MCPToolCall != nil {
		for _, v := range inner.MCPToolCall.Arguments {
			if v != "" {
				out = append(out, v)
			}
		}
	}
	if inner.WorkflowRef != nil {
		for _, v := range inner.WorkflowRef.Inputs {
			if v != "" {
				out = append(out, v)
			}
		}
	}
	if inner.ExternalAgentRef != nil && inner.ExternalAgentRef.Prompt != "" {
		out = append(out, inner.ExternalAgentRef.Prompt)
	}
	out = append(out, collectCELStringsFromOpenReport(inner.OpenReport)...)
	return out
}

func collectCELStringsFromOpenReport(ref *ottoflowv1alpha1.StepOpenReport) []string {
	if ref == nil {
		return nil
	}
	var out []string
	if ref.ResultsExpression != "" {
		out = append(out, ref.ResultsExpression)
	}
	if ref.ScopeExpression != "" {
		out = append(out, ref.ScopeExpression)
	}
	if ref.SummaryExpression != "" {
		out = append(out, ref.SummaryExpression)
	}
	return out
}
