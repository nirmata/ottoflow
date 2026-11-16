/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"context"
	"fmt"
	"maps"

	"k8s.io/klog/v2"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/logging"
)

// evaluateStepLevelOutputs evaluates step.Outputs over outputVars and merges them into outputs.
// The type's own outputs are visible as variables.<name>; on a name collision the step-level output wins.
func (e *WorkflowExecutor) evaluateStepLevelOutputs(ctx context.Context, step ottoflowv1alpha1.Step, outputVars, outputs map[string]interface{}) error {
	if len(step.Outputs) == 0 {
		return nil
	}
	// Copy the variables map rather than mutating the one held in the shared context.
	existing, _ := outputVars["variables"].(map[string]interface{})
	mergedVariables := make(map[string]interface{}, len(existing)+len(outputs))
	maps.Copy(mergedVariables, existing)
	maps.Copy(mergedVariables, outputs)
	stepVars := maps.Clone(outputVars)
	stepVars["variables"] = mergedVariables

	stepOutputs, err := e.celEvaluator.EvaluateStepOutputs(ctx, step, stepVars)
	if err != nil {
		return fmt.Errorf("failed to evaluate step outputs: %w", err)
	}
	for k, v := range stepOutputs {
		if _, collides := outputs[k]; collides {
			klog.V(1).InfoS("step-level output shadows a step output of the same name", logging.KeyStep, step.Name, "output", k)
		}
		outputs[k] = v
	}
	return nil
}
