/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

// contextKey type for scoped context (e.g. forEach item isolation)
type contextKey struct{}

// scopedContextKey is set on ctx when executing a step with an isolated context (e.g. forEach child step).
// Value must be map[string]interface{} with "variables", "expressions", etc.
var scopedContextKey = &contextKey{}

// ContextManager manages workflow execution context
type ContextManager struct {
	workflowRun        *ottoflowv1alpha1.WorkflowRun
	inMemoryContext    map[string]interface{}
	contextInitialized bool
	completionOrder    []string // step names in execution-completion order, for ContextBudgetMode=lastN
}

// NewContextManager creates a new context manager
func NewContextManager(workflowRun *ottoflowv1alpha1.WorkflowRun) *ContextManager {
	return &ContextManager{
		workflowRun:        workflowRun,
		inMemoryContext:    nil,
		contextInitialized: false,
	}
}

// InitializeContext initializes the in-memory context with input values
// This is idempotent - can be called multiple times safely
func (cm *ContextManager) InitializeContext(ctx context.Context, workflow *ottoflowv1alpha1.Workflow, inputValues map[string]string) error {
	// If already initialized, skip
	if cm.contextInitialized && cm.inMemoryContext != nil {
		return nil
	}

	// Build params from input values
	params := make(map[string]interface{})
	for name, value := range inputValues {
		params[name] = value
	}

	// Set defaults for inputs that weren't provided so CEL always sees every declared input.
	for _, input := range workflow.Spec.Inputs {
		if _, exists := params[input.Name]; !exists {
			if input.Required && input.Default == "" {
				return fmt.Errorf("required input %s not provided", input.Name)
			}
			// Apply default (including empty string) so inputs.<name> is always defined in CEL.
			params[input.Name] = input.Default
		}
	}

	// Initialize in-memory context
	// Variables are flat (no namespacing) - step outputs write directly here
	cm.inMemoryContext = map[string]interface{}{
		"inputs":      params,
		"expressions": make(map[string]interface{}),
		"variables":   make(map[string]interface{}), // Flat variables - no step name prefix
		"steps":       make(map[string]interface{}), // Steps map for step-specific results (e.g., forEach, agent)
	}
	cm.contextInitialized = true

	return nil
}

// IsInitialized returns true if the context has been initialized (e.g. by a prior call to InitializeContext in this process).
// Used to distinguish "same-process continuation" (e.g. tests) from "reconciling after controller restart" (context lost).
func (cm *ContextManager) IsInitialized() bool {
	return cm.contextInitialized && cm.inMemoryContext != nil
}

// ReadContext reads the current context from in-memory storage
// Returns error if context not initialized (workflow needs restart)
// When ctx carries scopedContextKey (e.g. forEach item), returns that map directly so step modifications apply to it.
func (cm *ContextManager) ReadContext(ctx context.Context) (map[string]interface{}, error) {
	if scoped := ctx.Value(scopedContextKey); scoped != nil {
		return scoped.(map[string]interface{}), nil
	}
	if !cm.contextInitialized || cm.inMemoryContext == nil {
		return nil, fmt.Errorf("context not initialized - workflow needs restart")
	}
	// Return a copy to prevent external modifications
	contextCopy := make(map[string]interface{})
	for k, v := range cm.inMemoryContext {
		contextCopy[k] = v
	}
	return contextCopy, nil
}

// WriteStepOutputs writes step outputs directly to the variables map (no namespacing)
// Step names are already camelCase (validated by CRD)
// When ctx carries scopedContextKey, writes to that map's "variables" (e.g. forEach item).
func (cm *ContextManager) WriteStepOutputs(ctx context.Context, stepName string, outputs map[string]interface{}) error {
	if scoped := ctx.Value(scopedContextKey); scoped != nil {
		scopedMap := scoped.(map[string]interface{})
		variablesMap, ok := scopedMap["variables"].(map[string]interface{})
		if !ok {
			variablesMap = make(map[string]interface{})
			scopedMap["variables"] = variablesMap
		}
		for k, v := range outputs {
			variablesMap[k] = v
		}
		return nil
	}
	if !cm.contextInitialized || cm.inMemoryContext == nil {
		return fmt.Errorf("context not initialized - workflow needs restart")
	}

	// Get variables map
	variablesMap, ok := cm.inMemoryContext["variables"].(map[string]interface{})
	if !ok {
		variablesMap = make(map[string]interface{})
		cm.inMemoryContext["variables"] = variablesMap
	}

	// Write directly to variables map (no step name prefix)
	for k, v := range outputs {
		variablesMap[k] = v
	}

	return nil
}

// GetContext returns the in-memory context (for internal use).
// Use GetContextFrom(ctx) when inside a step that may run with scoped context (e.g. forEach child).
func (cm *ContextManager) GetContext() map[string]interface{} {
	return cm.inMemoryContext
}

// GetContextFrom returns the effective context map for the given ctx.
// When ctx carries scopedContextKey (e.g. forEach item), returns that map; otherwise returns in-memory context.
func (cm *ContextManager) GetContextFrom(ctx context.Context) map[string]interface{} {
	if scoped := ctx.Value(scopedContextKey); scoped != nil {
		return scoped.(map[string]interface{})
	}
	return cm.inMemoryContext
}

// RestoreContext replaces the in-memory context with a previously saved snapshot.
// After this call, IsInitialized() returns true and ReadContext() returns the restored data.
func (cm *ContextManager) RestoreContext(snapshot map[string]interface{}) {
	cm.inMemoryContext = snapshot
	cm.contextInitialized = true
}

// RecordStepCompletion appends stepName to the completion order.
// Called by the executor after each successful executeStep for use by ContextBudgetMode=lastN.
func (cm *ContextManager) RecordStepCompletion(stepName string) {
	cm.completionOrder = append(cm.completionOrder, stepName)
}

// CompletionOrder returns a copy of the recorded step completion sequence, so callers
// cannot mutate the manager's internal slice.
func (cm *ContextManager) CompletionOrder() []string {
	return slices.Clone(cm.completionOrder)
}

// SetCompletionOrder restores completionOrder directly from a persisted, ordered snapshot
// (checkpoint.CompletionOrder), giving exact execution order without CompletionTime
// reconstruction. Use RestoreCompletionOrder as a fallback for older checkpoints saved before
// completionOrder was persisted.
func (cm *ContextManager) SetCompletionOrder(order []string) {
	cm.completionOrder = slices.Clone(order)
}

// RestoreCompletionOrder rebuilds completionOrder from persisted StepStatuses after a checkpoint
// restore. Only succeeded steps are included (matching what RecordStepCompletion records during
// live execution). Steps are sorted by CompletionTime to approximate the original execution order.
//
// A succeeded step with no CompletionTime sorts oldest, so it is pruned first by lastN rather
// than dropped from the order entirely. metav1.Time serializes at second granularity, so equal
// timestamps are common after a checkpoint round-trip on fast workflows; the name tie-break
// exists only to make the result deterministic — it does not recover true execution order.
// A stable sort alone would not help here: the input is a map range, so pre-sort order is random.
func (cm *ContextManager) RestoreCompletionOrder(statuses map[string]ottoflowv1alpha1.StepStatus) {
	type entry struct {
		name string
		t    int64 // UnixNano; 0 when CompletionTime is nil, so the step sorts oldest
	}
	completed := make([]entry, 0, len(statuses))
	for name, s := range statuses {
		if s.Phase != ottoflowv1alpha1.StepPhaseSucceeded {
			continue
		}
		var t int64
		if s.CompletionTime != nil {
			t = s.CompletionTime.UnixNano()
		}
		completed = append(completed, entry{name, t})
	}
	slices.SortFunc(completed, func(a, b entry) int {
		return cmp.Or(cmp.Compare(a.t, b.t), cmp.Compare(a.name, b.name))
	})
	cm.completionOrder = make([]string, len(completed))
	for i, e := range completed {
		cm.completionOrder[i] = e.name
	}
}
