/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"context"
	"fmt"
	"sync"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

// ForEachItemResult represents the result of processing a single item
type ForEachItemResult struct {
	Item    interface{}            `json:"item"`
	Outputs map[string]interface{} `json:"outputs"`
	Status  string                 `json:"status"` // "succeeded" or "failed"
}

// ForEachItemFailure represents a failed item processing
type ForEachItemFailure struct {
	Item  interface{} `json:"item"`
	Error string      `json:"error"`
}

// ForEachResults contains all results from forEach execution
type ForEachResults struct {
	Results []ForEachItemResult  `json:"results"`
	Failed  []ForEachItemFailure `json:"failed"`
}

// executeForEach executes a forEach step by processing items in parallel
func (e *WorkflowExecutor) executeForEach(ctx context.Context, workflowRun *ottoflowv1alpha1.WorkflowRun, step ottoflowv1alpha1.Step) error {
	forEach := step.ForEach
	if forEach == nil {
		return fmt.Errorf("forEach is nil")
	}

	// Read current context
	contextData, err := e.contextManager.ReadContext(ctx)
	if err != nil {
		return fmt.Errorf("failed to read context: %w", err)
	}

	// Build variable map for CEL evaluation
	vars := e.celEvaluator.BuildVariableMap(contextData)

	// Evaluate items expression
	itemsVal, err := e.celEvaluator.EvaluateExpression(ctx, forEach.Items, vars)
	if err != nil {
		return fmt.Errorf("failed to evaluate items expression '%s': %w", forEach.Items, err)
	}

	// Convert to list - handle both []interface{} and []string
	var itemsList []interface{}
	switch v := itemsVal.(type) {
	case []interface{}:
		itemsList = v
	case []string:
		itemsList = make([]interface{}, len(v))
		for i, s := range v {
			itemsList[i] = s
		}
	case nil:
		// Handle nil case
		itemsList = []interface{}{}
	default:
		// The CEL evaluator should convert CEL list types to []interface{}
		// If we get here, try to extract from CEL types or return error
		return fmt.Errorf("items expression must evaluate to a list, got %T (value: %v). Make sure variables are evaluated correctly", itemsVal, itemsVal)
	}

	if len(itemsList) == 0 {
		// No items to process - write empty results
		results := &ForEachResults{
			Results: []ForEachItemResult{},
			Failed:  []ForEachItemFailure{},
		}
		return e.writeForEachResults(ctx, step.Name, results)
	}

	// Determine item variable name
	itemVariable := forEach.ItemVariable
	if itemVariable == "" {
		itemVariable = "item"
	}

	// Determine max concurrency
	maxConcurrency := forEach.MaxConcurrency
	if maxConcurrency <= 0 {
		maxConcurrency = e.maxWorkers
		if maxConcurrency <= 0 {
			maxConcurrency = 5 // Default
		}
	}

	// Determine failure policy
	itemFailurePolicy := forEach.ItemFailurePolicy
	if itemFailurePolicy == "" {
		itemFailurePolicy = ottoflowv1alpha1.FailurePolicyContinue // Default
	}

	// Report initial progress for CLI (0/N items) so the user sees forEach iteration count immediately
	if st, ok := workflowRun.Status.StepStatuses[step.Name]; ok && st.Phase == ottoflowv1alpha1.StepPhaseRunning {
		st.Message = fmt.Sprintf("0/%d items", len(itemsList))
		workflowRun.Status.StepStatuses[step.Name] = st
		e.invokeForEachProgressCallback(workflowRun)
	}

	// Process items concurrently with worker pool
	results := e.processItemsConcurrently(ctx, workflowRun, step, itemsList, itemVariable, maxConcurrency, itemFailurePolicy)

	// Write results to context first (so outputs can reference them)
	if err := e.writeForEachResults(ctx, step.Name, results); err != nil {
		return err
	}

	// Honour itemFailurePolicy: Fail. Results are already in context above, so a failing
	// run still leaves steps.<name>.failed available for debugging. Note that Fail also
	// signals early termination, so items after the first failure may never have run.
	// Failed is appended under resultsMutex as items complete, so [0] is the first failure
	// *by completion*, not the first by item index -- with maxConcurrency > 1 those differ.
	if len(results.Failed) > 0 && itemFailurePolicy == ottoflowv1alpha1.FailurePolicyFail {
		return fmt.Errorf("forEach: %d item(s) failed with itemFailurePolicy=Fail; first failure to complete: %s",
			len(results.Failed), results.Failed[0].Error)
	}

	// Evaluate step-level outputs (if any) that can reference results. This must happen
	// before the succeeded==0 guard below returns an error: a forEach step can carry its
	// own step-level failurePolicy: Continue, which lets the workflow proceed past a Failed
	// forEach step -- but only if the step's declared outputs actually got written to
	// context. Otherwise a downstream steps.<name>.<out> / variables.<out> read would hit
	// "no such key" even though the workflow "succeeded".
	if len(step.Outputs) > 0 {
		// Read updated context (now includes steps map with results)
		contextData, err := e.contextManager.ReadContext(ctx)
		if err != nil {
			return fmt.Errorf("failed to read context: %w", err)
		}

		// Build variable map for output evaluation
		vars := e.celEvaluator.BuildVariableMap(contextData)

		// Evaluate step outputs
		outputs, err := e.celEvaluator.EvaluateStepOutputs(ctx, step, vars)
		if err != nil {
			return fmt.Errorf("failed to evaluate step outputs: %w", err)
		}

		// Write outputs to context (directly to variables map)
		err = e.contextManager.WriteStepOutputs(ctx, step.Name, outputs)
		if err != nil {
			return fmt.Errorf("failed to write step outputs: %w", err)
		}
	}

	// A forEach where every item failed (or none completed at all) must never report success,
	// regardless of itemFailurePolicy -- Continue only tolerates partial failure, not total failure.
	// itemsList is non-empty here: the len(itemsList) == 0 case already returned above.
	succeeded := len(results.Results) - len(results.Failed)
	if succeeded == 0 {
		if len(results.Failed) > 0 {
			return fmt.Errorf("forEach: all %d item(s) failed with itemFailurePolicy=%s; first failure to complete: %s",
				len(itemsList), itemFailurePolicy, results.Failed[0].Error)
		}
		return fmt.Errorf("forEach: no items completed successfully (loop cancelled or incomplete); %d item(s) requested", len(itemsList))
	}

	// itemFailurePolicy=Continue succeeds by design, but a partially or fully failed loop
	// must not look identical to a clean one. Record the tally on the step message. Reuses
	// the same succeeded count as the guard above so the two never disagree.
	if len(results.Failed) > 0 {
		if st, ok := workflowRun.Status.StepStatuses[step.Name]; ok {
			st.Message = fmt.Sprintf("%d/%d items succeeded, %d failed",
				succeeded, len(itemsList), len(results.Failed))
			workflowRun.Status.StepStatuses[step.Name] = st
			e.invokeForEachProgressCallback(workflowRun)
		}
		klog.V(2).InfoS("forEach completed with failures",
			"step", step.Name, "total", len(itemsList), "failed", len(results.Failed),
			"itemFailurePolicy", itemFailurePolicy)
	}

	return nil
}

// processItemsConcurrently processes items using a worker pool pattern
func (e *WorkflowExecutor) processItemsConcurrently(
	ctx context.Context,
	workflowRun *ottoflowv1alpha1.WorkflowRun,
	step ottoflowv1alpha1.Step,
	items []interface{},
	itemVariable string,
	maxConcurrency int,
	itemFailurePolicy string,
) *ForEachResults {
	// Create semaphore for concurrency control
	semaphore := make(chan struct{}, maxConcurrency)

	// Results collection
	var resultsMutex sync.Mutex
	results := &ForEachResults{
		Results: make([]ForEachItemResult, 0, len(items)),
		Failed:  make([]ForEachItemFailure, 0),
	}

	// WaitGroup to wait for all workers
	var wg sync.WaitGroup

	// Channel to signal early termination on failure
	stopChan := make(chan struct{})
	var stopOnce sync.Once

	// Process each item
itemLoop:
	for i, item := range items {
		// Check if we should stop early
		select {
		case <-stopChan:
			// Early termination requested
			break itemLoop
		default:
		}

		wg.Add(1)
		go func(_ int, currentItem interface{}) {
			defer wg.Done()

			// Acquire semaphore, but stop waiting if cancellation or early termination is signaled.
			select {
			case <-stopChan:
				return
			case <-ctx.Done():
				return
			case semaphore <- struct{}{}:
			}
			defer func() { <-semaphore }()

			// Check if we should stop early
			select {
			case <-stopChan:
				return
			default:
			}

			// Process item
			result, err := e.processForEachItem(ctx, workflowRun, step, currentItem, itemVariable)

			// Check ctx cancellation before accumulating results
			select {
			case <-ctx.Done():
				return
			default:
			}

			resultsMutex.Lock()
			if err != nil {
				// Log error for debugging
				klog.V(4).InfoS("forEach item failed", "error", err)
				failure := ForEachItemFailure{
					Item:  currentItem,
					Error: err.Error(),
				}
				results.Failed = append(results.Failed, failure)
				results.Results = append(results.Results, ForEachItemResult{
					Item:    currentItem,
					Outputs: nil,
					Status:  "failed",
				})

				// If failure policy is Fail, signal early termination
				if itemFailurePolicy == "Fail" {
					stopOnce.Do(func() {
						close(stopChan)
					})
				}
			} else {
				itemResult := ForEachItemResult{
					Item:    currentItem,
					Outputs: result,
					Status:  "succeeded",
				}
				results.Results = append(results.Results, itemResult)
			}
			// Report progress for CLI: update step message with item count and invoke callback
			total := len(items)
			completed := len(results.Results)
			if st, ok := workflowRun.Status.StepStatuses[step.Name]; ok && st.Phase == ottoflowv1alpha1.StepPhaseRunning {
				st.Message = fmt.Sprintf("%d/%d items", completed, total)
				workflowRun.Status.StepStatuses[step.Name] = st
				e.invokeForEachProgressCallback(workflowRun)
			}
			resultsMutex.Unlock()
		}(i, item)
	}

	// Wait for all workers to complete
	wg.Wait()

	return results
}

// processForEachItem processes a single item in isolation
func (e *WorkflowExecutor) processForEachItem(
	ctx context.Context,
	workflowRun *ottoflowv1alpha1.WorkflowRun,
	parentStep ottoflowv1alpha1.Step,
	item interface{},
	itemVariable string,
) (map[string]interface{}, error) {
	// Read parent context
	parentContext, err := e.contextManager.ReadContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to read parent context: %w", err)
	}

	// Create isolated context snapshot with deep copy of nested maps
	isolatedContext := make(map[string]interface{})
	for k, v := range parentContext {
		// Deep copy nested maps to avoid concurrent write issues
		if mapVal, ok := v.(map[string]interface{}); ok {
			copiedMap := make(map[string]interface{})
			for mk, mv := range mapVal {
				copiedMap[mk] = mv
			}
			isolatedContext[k] = copiedMap
		} else {
			isolatedContext[k] = v
		}
	}

	// Ensure variables map exists and is isolated
	if isolatedContext["variables"] == nil {
		isolatedContext["variables"] = make(map[string]interface{})
	} else {
		// Deep copy variables map
		parentVars := parentContext["variables"].(map[string]interface{})
		isolatedVars := make(map[string]interface{})
		for k, v := range parentVars {
			isolatedVars[k] = v
		}
		isolatedContext["variables"] = isolatedVars
	}
	variables := isolatedContext["variables"].(map[string]interface{})
	// Add item to variables map so it's accessible as variables.<itemVariable> in expressions
	variables[itemVariable] = item
	// Also expose it at the top level as `item`. The CEL env declares `item` unconditionally
	// (see cel.go) so expressions using it compile; without this binding they compiled and
	// then failed at evaluation. Keyed on "item" rather than itemVariable so bare `item`
	// always means "the current forEach item" regardless of the configured alias.
	isolatedContext["item"] = item

	// Ensure expressions map exists and is isolated
	if isolatedContext["expressions"] == nil {
		isolatedContext["expressions"] = make(map[string]interface{})
	} else {
		// Deep copy expressions map
		parentExprs := parentContext["expressions"].(map[string]interface{})
		isolatedExprs := make(map[string]interface{})
		for k, v := range parentExprs {
			isolatedExprs[k] = v
		}
		isolatedContext["expressions"] = isolatedExprs
	}

	// Determine which step definition to use
	var childStep ottoflowv1alpha1.Step
	if parentStep.ForEach.Step != nil {
		// Inline step definition
		childStep = convertForEachStepToStep(parentStep.ForEach.Step)
	} else if parentStep.ForEach.StepTemplateRef != nil {
		// StepTemplate reference - instantiate template with isolated context
		var err error
		childStep, err = e.instantiateForEachStepTemplate(ctx, workflowRun, parentStep, isolatedContext, itemVariable)
		if err != nil {
			return nil, fmt.Errorf("failed to instantiate step template: %w", err)
		}
	} else {
		return nil, fmt.Errorf("forEach step must specify either 'step' or 'stepTemplateRef'")
	}

	// Synthetic name for the child step (used by WriteStepOutputs; step status is not updated for forEach items)
	if childStep.Name == "" {
		childStep.Name = "_item_"
	}

	// Run the child step with scoped context so all step types (resourceQuery, mutate, expressions, etc.) work
	ctxWithScope := context.WithValue(ctx, scopedContextKey, isolatedContext)
	outputs, err := e.executeStep(ctxWithScope, workflowRun, childStep)
	if err != nil {
		return nil, err
	}

	// Return the outputs produced by the step (for aggregation in forEach results)
	if outputs == nil {
		outputs = make(map[string]interface{})
	}
	return outputs, nil
}

// convertForEachStepToStep converts a StepForEachStep to a Step
func convertForEachStepToStep(forEachStep *ottoflowv1alpha1.StepForEachStep) ottoflowv1alpha1.Step {
	step := ottoflowv1alpha1.Step{
		Expressions:      forEachStep.Expressions,
		Outputs:          forEachStep.Outputs,
		MatchConditions:  forEachStep.MatchConditions,
		Retry:            forEachStep.Retry,
		Timeout:          forEachStep.Timeout,
		FailurePolicy:    forEachStep.FailurePolicy,
		ResourceQuery:    forEachStep.ResourceQuery,
		PrometheusQuery:  forEachStep.PrometheusQuery,
		Mutate:           forEachStep.Mutate,
		AgentRef:         forEachStep.AgentRef,
		MCPToolCall:      forEachStep.MCPToolCall,
		WorkflowRef:      forEachStep.WorkflowRef,
		ExternalAgentRef: forEachStep.ExternalAgentRef,
		OpenReport:       forEachStep.OpenReport,
	}
	return step
}

// instantiateForEachStepTemplate instantiates a StepTemplate for forEach execution
func (e *WorkflowExecutor) instantiateForEachStepTemplate(
	ctx context.Context,
	workflowRun *ottoflowv1alpha1.WorkflowRun,
	parentStep ottoflowv1alpha1.Step,
	isolatedContext map[string]interface{},
	_ string, // itemVariable - reserved for future per-item template customization
) (ottoflowv1alpha1.Step, error) {
	stepTemplateRef := parentStep.ForEach.StepTemplateRef

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
		return ottoflowv1alpha1.Step{}, fmt.Errorf("failed to get StepTemplate %s/%s: %w", templateNamespace, stepTemplateRef.Name, err)
	}

	// Build variable map from isolated context (includes itemVariable)
	vars := e.celEvaluator.BuildVariableMap(isolatedContext)

	// Evaluate template arguments (CEL expressions) in isolated context
	resolvedArgs := make(map[string]interface{})
	for paramName, argExpr := range stepTemplateRef.Arguments {
		result, err := e.celEvaluator.EvaluateExpression(ctx, argExpr, vars)
		if err != nil {
			return ottoflowv1alpha1.Step{}, fmt.Errorf("failed to evaluate argument '%s': %w", paramName, err)
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
					return ottoflowv1alpha1.Step{}, fmt.Errorf("failed to evaluate default value for parameter '%s': %w", param.Name, err)
				}
				resolvedArgs[param.Name] = defaultResult
			} else if param.Required {
				return ottoflowv1alpha1.Step{}, fmt.Errorf("required parameter '%s' not provided", param.Name)
			}
		}
	}

	// Validate all required parameters are provided
	for _, param := range stepTemplateCRD.Spec.Parameters {
		if param.Required {
			if _, provided := resolvedArgs[param.Name]; !provided {
				return ottoflowv1alpha1.Step{}, fmt.Errorf("required parameter '%s' not provided", param.Name)
			}
		}
	}

	// Instantiate the template step by substituting parameters
	instantiatedStep, err := e.instantiateStepTemplate(stepTemplateCRD.Spec.Step, "", resolvedArgs)
	if err != nil {
		return ottoflowv1alpha1.Step{}, fmt.Errorf("failed to instantiate step template: %w", err)
	}

	return instantiatedStep, nil
}

// writeForEachResults writes forEach results to the steps map in context
func (e *WorkflowExecutor) writeForEachResults(_ context.Context, stepName string, results *ForEachResults) error {
	// Get the in-memory context
	inMemoryContext := e.contextManager.GetContext()
	if inMemoryContext == nil {
		return fmt.Errorf("context not initialized")
	}

	// Get or create steps map
	stepsMap, ok := inMemoryContext["steps"].(map[string]interface{})
	if !ok {
		stepsMap = make(map[string]interface{})
		inMemoryContext["steps"] = stepsMap
	}

	// Convert results to []interface{} for proper serialization
	resultsList := make([]interface{}, len(results.Results))
	var succeededList []interface{}
	for i, r := range results.Results {
		resultsList[i] = map[string]interface{}{
			"item":    r.Item,
			"outputs": r.Outputs,
			"status":  r.Status,
		}
		if r.Status == "succeeded" {
			succeededList = append(succeededList, resultsList[i])
		}
	}
	if succeededList == nil {
		succeededList = []interface{}{}
	}

	failedList := make([]interface{}, len(results.Failed))
	for i, f := range results.Failed {
		failedList[i] = map[string]interface{}{
			"item":  f.Item,
			"error": f.Error,
		}
	}

	// Write forEach results
	stepsMap[stepName] = map[string]interface{}{
		"results":   resultsList,
		"succeeded": succeededList,
		"failed":    failedList,
	}

	return nil
}
