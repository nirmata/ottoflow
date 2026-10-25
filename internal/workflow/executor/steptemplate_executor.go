/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"context"
	"fmt"
	"strings"
	"text/template"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/types"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

// executeStepTemplate instantiates a StepTemplate and executes the resulting step
func (e *WorkflowExecutor) executeStepTemplate(ctx context.Context, workflowRun *ottoflowv1alpha1.WorkflowRun, step ottoflowv1alpha1.Step) (map[string]interface{}, error) {
	stepTemplateRef := step.StepTemplateRef

	// Determine namespace for StepTemplate CRD
	templateNamespace := stepTemplateRef.Namespace
	if templateNamespace == "" {
		templateNamespace = workflowRun.Namespace
	}

	// Get the StepTemplate CRD
	stepTemplateCRD := &ottoflowv1alpha1.StepTemplate{}
	templateKey := types.NamespacedName{
		Name:      stepTemplateRef.Name,
		Namespace: templateNamespace,
	}
	if err := e.controlClient.Get(ctx, templateKey, stepTemplateCRD); err != nil {
		return nil, fmt.Errorf("failed to get StepTemplate %s/%s: %w", templateNamespace, stepTemplateRef.Name, err)
	}

	// Read current context for argument evaluation
	contextData, err := e.contextManager.ReadContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to read context: %w", err)
	}

	// Build variable map for CEL evaluation
	vars := e.celEvaluator.BuildVariableMap(contextData)

	// Evaluate template arguments (CEL expressions)
	resolvedArgs := make(map[string]interface{})
	for paramName, argExpr := range stepTemplateRef.Arguments {
		result, err := e.celEvaluator.EvaluateExpression(ctx, argExpr, vars)
		if err != nil {
			return nil, fmt.Errorf("failed to evaluate argument '%s': %w", paramName, err)
		}
		resolvedArgs[paramName] = result
	}

	// Apply default values for parameters not provided
	for _, param := range stepTemplateCRD.Spec.Parameters {
		if _, provided := resolvedArgs[param.Name]; !provided {
			if param.Default != "" {
				// Evaluate default value as CEL expression
				defaultResult, err := e.celEvaluator.EvaluateExpression(ctx, param.Default, vars)
				if err != nil {
					return nil, fmt.Errorf("failed to evaluate default value for parameter '%s': %w", param.Name, err)
				}
				resolvedArgs[param.Name] = defaultResult
			} else if param.Required {
				return nil, fmt.Errorf("required parameter '%s' not provided", param.Name)
			}
		}
	}

	// Validate all required parameters are provided
	for _, param := range stepTemplateCRD.Spec.Parameters {
		if param.Required {
			if _, provided := resolvedArgs[param.Name]; !provided {
				return nil, fmt.Errorf("required parameter '%s' not provided", param.Name)
			}
		}
	}

	// Instantiate the template step by substituting parameters
	instantiatedStep, err := e.instantiateStepTemplate(stepTemplateCRD.Spec.Step, step.Name, resolvedArgs)
	if err != nil {
		return nil, fmt.Errorf("failed to instantiate step template: %w", err)
	}

	// Merge step-level fields (dependsOn, matchConditions, retry, timeout, failurePolicy)
	// These can be overridden at the step level
	if len(step.DependsOn) > 0 {
		instantiatedStep.DependsOn = step.DependsOn
	}
	if len(step.MatchConditions) > 0 {
		instantiatedStep.MatchConditions = step.MatchConditions
	}
	if step.Retry != nil {
		instantiatedStep.Retry = step.Retry
	}
	if step.Timeout != "" {
		instantiatedStep.Timeout = step.Timeout
	}
	if step.FailurePolicy != "" {
		instantiatedStep.FailurePolicy = step.FailurePolicy
	}

	// Execute the instantiated step
	return e.executeStep(ctx, workflowRun, instantiatedStep)
}

// instantiateStepTemplate substitutes parameters in a template step definition
func (e *WorkflowExecutor) instantiateStepTemplate(templateStep ottoflowv1alpha1.StepTemplateStep, stepName string, args map[string]interface{}) (ottoflowv1alpha1.Step, error) {
	// Create a template data structure for substitution
	templateData := make(map[string]interface{})
	for k, v := range args {
		templateData[k] = v
	}

	// Helper function to substitute placeholders in a string
	substituteString := func(s string) (string, error) {
		if s == "" {
			return s, nil
		}
		tmpl, err := template.New("substitute").Parse(s)
		if err != nil {
			return "", fmt.Errorf("failed to parse template string: %w", err)
		}
		var buf strings.Builder
		if err := tmpl.Execute(&buf, templateData); err != nil {
			return "", fmt.Errorf("failed to execute template: %w", err)
		}
		return buf.String(), nil
	}

	// Instantiate the step
	instantiated := ottoflowv1alpha1.Step{
		Name:            stepName,
		Message:         templateStep.Message,
		DependsOn:       templateStep.DependsOn,
		Retry:           templateStep.Retry,
		Timeout:         templateStep.Timeout,
		FailurePolicy:   templateStep.FailurePolicy,
		Expressions:     make([]ottoflowv1alpha1.Expression, len(templateStep.Expressions)),
		Outputs:         make([]ottoflowv1alpha1.Output, len(templateStep.Outputs)),
		MatchConditions: make([]ottoflowv1alpha1.MatchCondition, len(templateStep.MatchConditions)),
	}

	// Substitute message
	if templateStep.Message != "" {
		substituted, err := substituteString(templateStep.Message)
		if err != nil {
			return instantiated, fmt.Errorf("failed to substitute message: %w", err)
		}
		instantiated.Message = substituted
	}

	// Substitute expressions
	for i, expr := range templateStep.Expressions {
		instantiated.Expressions[i] = ottoflowv1alpha1.Expression{
			Name: expr.Name,
		}
		substituted, err := substituteString(expr.Expression)
		if err != nil {
			return instantiated, fmt.Errorf("failed to substitute expression '%s': %w", expr.Name, err)
		}
		instantiated.Expressions[i].Expression = substituted
	}

	// Substitute outputs
	for i, output := range templateStep.Outputs {
		instantiated.Outputs[i] = ottoflowv1alpha1.Output{
			Name: output.Name,
		}
		// Handle both expression and value fields
		if output.Expression != "" {
			substituted, err := substituteString(output.Expression)
			if err != nil {
				return instantiated, fmt.Errorf("failed to substitute output expression '%s': %w", output.Name, err)
			}
			instantiated.Outputs[i].Expression = substituted
		}
		if output.Value != nil {
			// For value field (apiextensionsv1.JSON), we need to substitute within the JSON
			// This is more complex - for now, we'll convert to string, substitute, and convert back
			valueStr := string(output.Value.Raw)
			substituted, err := substituteString(valueStr)
			if err != nil {
				return instantiated, fmt.Errorf("failed to substitute output value '%s': %w", output.Name, err)
			}
			instantiated.Outputs[i].Value = &apiextensionsv1.JSON{Raw: []byte(substituted)}
		}
	}

	// Substitute matchConditions
	for i, condition := range templateStep.MatchConditions {
		instantiated.MatchConditions[i] = ottoflowv1alpha1.MatchCondition{
			Name: condition.Name,
		}
		substituted, err := substituteString(condition.Expression)
		if err != nil {
			return instantiated, fmt.Errorf("failed to substitute matchCondition '%s': %w", condition.Name, err)
		}
		instantiated.MatchConditions[i].Expression = substituted
	}

	// Handle ResourceQuery
	if templateStep.ResourceQuery != nil {
		instantiated.ResourceQuery = &ottoflowv1alpha1.StepResourceQuery{
			APIVersion:    templateStep.ResourceQuery.APIVersion,
			Resource:      templateStep.ResourceQuery.Resource,
			Namespace:     templateStep.ResourceQuery.Namespace,
			Name:          templateStep.ResourceQuery.Name,
			LabelSelector: templateStep.ResourceQuery.LabelSelector,
			FieldSelector: templateStep.ResourceQuery.FieldSelector,
			Outputs:       make(map[string]string),
		}
		// Substitute ResourceQuery fields
		if templateStep.ResourceQuery.Namespace != "" {
			substituted, err := substituteString(templateStep.ResourceQuery.Namespace)
			if err != nil {
				return instantiated, fmt.Errorf("failed to substitute ResourceQuery namespace: %w", err)
			}
			instantiated.ResourceQuery.Namespace = substituted
		}
		if templateStep.ResourceQuery.Name != "" {
			substituted, err := substituteString(templateStep.ResourceQuery.Name)
			if err != nil {
				return instantiated, fmt.Errorf("failed to substitute ResourceQuery name: %w", err)
			}
			instantiated.ResourceQuery.Name = substituted
		}
		if templateStep.ResourceQuery.FieldSelector != "" {
			substituted, err := substituteString(templateStep.ResourceQuery.FieldSelector)
			if err != nil {
				return instantiated, fmt.Errorf("failed to substitute ResourceQuery fieldSelector: %w", err)
			}
			instantiated.ResourceQuery.FieldSelector = substituted
		}
		// Substitute labelSelector values (each value is a CEL expression that may contain template placeholders)
		if len(templateStep.ResourceQuery.LabelSelector) > 0 {
			instantiated.ResourceQuery.LabelSelector = make(map[string]string)
			for k, v := range templateStep.ResourceQuery.LabelSelector {
				substituted, err := substituteString(v)
				if err != nil {
					return instantiated, fmt.Errorf("failed to substitute ResourceQuery labelSelector '%s': %w", k, err)
				}
				instantiated.ResourceQuery.LabelSelector[k] = substituted
			}
		}
		// Substitute outputs
		for k, v := range templateStep.ResourceQuery.Outputs {
			substituted, err := substituteString(v)
			if err != nil {
				return instantiated, fmt.Errorf("failed to substitute ResourceQuery output '%s': %w", k, err)
			}
			instantiated.ResourceQuery.Outputs[k] = substituted
		}
	}

	// Handle PrometheusQuery
	if templateStep.PrometheusQuery != nil {
		instantiated.PrometheusQuery = &ottoflowv1alpha1.StepPrometheusQuery{
			Query:     templateStep.PrometheusQuery.Query,
			TimeRange: templateStep.PrometheusQuery.TimeRange,
			Step:      templateStep.PrometheusQuery.Step,
			Variables: make(map[string]string),
			Outputs:   make(map[string]string),
		}
		if substituted, err := substituteString(templateStep.PrometheusQuery.Query); err != nil {
			return instantiated, fmt.Errorf("failed to substitute PrometheusQuery query: %w", err)
		} else {
			instantiated.PrometheusQuery.Query = substituted
		}
		if substituted, err := substituteString(templateStep.PrometheusQuery.TimeRange); err != nil {
			return instantiated, fmt.Errorf("failed to substitute PrometheusQuery timeRange: %w", err)
		} else {
			instantiated.PrometheusQuery.TimeRange = substituted
		}
		if templateStep.PrometheusQuery.Step != "" {
			if substituted, err := substituteString(templateStep.PrometheusQuery.Step); err != nil {
				return instantiated, fmt.Errorf("failed to substitute PrometheusQuery step: %w", err)
			} else {
				instantiated.PrometheusQuery.Step = substituted
			}
		}
		for k, v := range templateStep.PrometheusQuery.Variables {
			substituted, err := substituteString(v)
			if err != nil {
				return instantiated, fmt.Errorf("failed to substitute PrometheusQuery variable '%s': %w", k, err)
			}
			instantiated.PrometheusQuery.Variables[k] = substituted
		}
		for k, v := range templateStep.PrometheusQuery.Outputs {
			substituted, err := substituteString(v)
			if err != nil {
				return instantiated, fmt.Errorf("failed to substitute PrometheusQuery output '%s': %w", k, err)
			}
			instantiated.PrometheusQuery.Outputs[k] = substituted
		}
	}

	// Handle AgentRef
	if templateStep.AgentRef != nil {
		instantiated.AgentRef = &ottoflowv1alpha1.StepAgentRef{
			Name:              templateStep.AgentRef.Name,
			Namespace:         templateStep.AgentRef.Namespace,
			AdditionalPrompts: make([]string, len(templateStep.AgentRef.AdditionalPrompts)),
		}
		// Substitute additionalPrompts
		for i, prompt := range templateStep.AgentRef.AdditionalPrompts {
			substituted, err := substituteString(prompt)
			if err != nil {
				return instantiated, fmt.Errorf("failed to substitute AgentRef prompt %d: %w", i, err)
			}
			instantiated.AgentRef.AdditionalPrompts[i] = substituted
		}
	}

	// Handle MCPToolCall
	if templateStep.MCPToolCall != nil {
		instantiated.MCPToolCall = &ottoflowv1alpha1.StepMCPToolCall{
			Server:    templateStep.MCPToolCall.Server,
			Tool:      templateStep.MCPToolCall.Tool,
			Arguments: make(map[string]string),
		}
		// Substitute arguments
		for k, v := range templateStep.MCPToolCall.Arguments {
			substituted, err := substituteString(v)
			if err != nil {
				return instantiated, fmt.Errorf("failed to substitute MCPToolCall argument '%s': %w", k, err)
			}
			instantiated.MCPToolCall.Arguments[k] = substituted
		}
	}

	// Handle WorkflowRef
	if templateStep.WorkflowRef != nil {
		instantiated.WorkflowRef = &ottoflowv1alpha1.StepWorkflowRef{
			Name:      templateStep.WorkflowRef.Name,
			Namespace: templateStep.WorkflowRef.Namespace,
			Inputs:    make(map[string]string),
		}
		// Substitute inputs
		for k, v := range templateStep.WorkflowRef.Inputs {
			substituted, err := substituteString(v)
			if err != nil {
				return instantiated, fmt.Errorf("failed to substitute WorkflowRef input '%s': %w", k, err)
			}
			instantiated.WorkflowRef.Inputs[k] = substituted
		}
	}

	// Handle ExternalAgentRef
	if templateStep.ExternalAgentRef != nil {
		substituted, err := substituteString(templateStep.ExternalAgentRef.Prompt)
		if err != nil {
			return instantiated, fmt.Errorf("failed to substitute ExternalAgentRef prompt: %w", err)
		}
		instantiated.ExternalAgentRef = deepCopyExternalAgentRef(templateStep.ExternalAgentRef, substituted)
	}

	return instantiated, nil
}

// deepCopyExternalAgentRef returns an isolated copy of src with Prompt replaced by the
// already-substituted value. Auth and CASecretRef pointer trees are fully deep-copied so
// concurrent forEach template instantiations cannot share the same underlying structs.
func deepCopyExternalAgentRef(src *ottoflowv1alpha1.StepExternalAgentRef, prompt string) *ottoflowv1alpha1.StepExternalAgentRef {
	dst := src.DeepCopy()
	dst.Prompt = prompt
	return dst
}
