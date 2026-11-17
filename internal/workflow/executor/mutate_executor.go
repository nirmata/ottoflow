/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"context"
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/logging"
)

// executeMutate runs a mutate step: GET target resource, evaluate patch (CEL with "object"), apply and UPDATE
func (e *WorkflowExecutor) executeMutate(ctx context.Context, workflowRun *ottoflowv1alpha1.WorkflowRun, step ottoflowv1alpha1.Step) (map[string]interface{}, error) {
	if e.client == nil {
		return nil, fmt.Errorf("kubernetes client not available (no kubeconfig); this workflow requires a cluster")
	}

	mutate := step.Mutate
	if mutate == nil {
		return nil, fmt.Errorf("mutate step config is nil")
	}

	contextData, err := e.contextManager.ReadContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to read context: %w", err)
	}
	vars := e.celEvaluator.BuildVariableMap(contextData)

	// Resolve target
	gv, err := schema.ParseGroupVersion(mutate.Target.APIVersion)
	if err != nil {
		return nil, fmt.Errorf("invalid target apiVersion %q: %w", mutate.Target.APIVersion, err)
	}
	gvk := schema.GroupVersionKind{
		Group:   gv.Group,
		Version: gv.Version,
		Kind:    mutate.Target.Resource,
	}
	if gvk.Kind == "" {
		return nil, fmt.Errorf("target resource kind cannot be empty")
	}

	namespace := ""
	if mutate.Target.Namespace != "" {
		result, err := e.celEvaluator.EvaluateExpression(ctx, mutate.Target.Namespace, vars)
		if err != nil {
			return nil, fmt.Errorf("failed to evaluate target namespace: %w", err)
		}
		if ns, ok := result.(string); ok {
			namespace = ns
		} else {
			namespace = workflowRun.Namespace
		}
	} else {
		namespace = workflowRun.Namespace
	}

	nameResult, err := e.celEvaluator.EvaluateExpression(ctx, mutate.Target.Name, vars)
	if err != nil {
		return nil, fmt.Errorf("failed to evaluate target name: %w", err)
	}
	name, ok := nameResult.(string)
	if !ok || name == "" {
		return nil, fmt.Errorf("target name must evaluate to a non-empty string, got %T", nameResult)
	}

	// GET resource
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	key := types.NamespacedName{Name: name, Namespace: namespace}
	if err := e.client.Get(ctx, key, obj); err != nil {
		return nil, fmt.Errorf("failed to get target resource %s/%s %s/%s: %w",
			gvk.GroupVersion().String(), gvk.Kind, namespace, name, err)
	}

	// Build vars with "object" for CEL patch evaluation
	patchVars := make(map[string]interface{})
	for k, v := range vars {
		patchVars[k] = v
	}
	patchVars["object"] = obj.Object

	var patchData []byte
	switch mutate.PatchType {
	case "ApplyConfiguration":
		if mutate.ApplyConfiguration == nil || mutate.ApplyConfiguration.Expression == "" {
			return nil, fmt.Errorf("mutate patchType ApplyConfiguration requires applyConfiguration.expression")
		}
		patchData, err = e.applyConfigurationPatch(ctx, obj, mutate.ApplyConfiguration.Expression, patchVars)
		if err != nil {
			return nil, err
		}
	case "JSONPatch":
		patchData, err = e.jsonPatchData(ctx, mutate.JSONPatch, patchVars)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unsupported mutate patchType %q", mutate.PatchType)
	}

	if len(patchData) == 0 {
		klog.V(4).InfoS("Mutate step produced empty patch, skipping update", logging.KeyStep, step.Name, "resource", key.String())
	} else {
		if mutate.PatchType == "ApplyConfiguration" {
			var merged map[string]interface{}
			if err := json.Unmarshal(patchData, &merged); err != nil {
				return nil, fmt.Errorf("failed to unmarshal merged object: %w", err)
			}
			patched := obj.DeepCopy()
			patched.Object = merged
			if err := e.client.Patch(ctx, patched, client.MergeFrom(obj)); err != nil {
				return nil, fmt.Errorf("failed to patch resource %s: %w", key.String(), err)
			}
			obj = patched
		} else {
			if err := e.client.Patch(ctx, obj, client.RawPatch(types.JSONPatchType, patchData)); err != nil {
				return nil, fmt.Errorf("failed to apply JSON patch to %s: %w", key.String(), err)
			}
		}
		klog.V(3).InfoS("Mutate step applied patch", logging.KeyStep, step.Name, "resource", key.String())
	}

	// Optional outputs: mutate.outputs, then step-level outputs
	var outputs map[string]interface{}
	if len(mutate.Outputs) > 0 || len(step.Outputs) > 0 {
		outputContext := make(map[string]interface{})
		for k, v := range contextData {
			outputContext[k] = v
		}
		outputContext["object"] = obj.Object
		outputVars := e.celEvaluator.BuildVariableMap(outputContext)
		outputVars["object"] = obj.Object // expose patched resource for output CEL expressions
		outputs = make(map[string]interface{})
		for outputName, celExpr := range mutate.Outputs {
			result, err := e.celEvaluator.EvaluateExpression(ctx, celExpr, outputVars)
			if err != nil {
				return nil, fmt.Errorf("failed to evaluate mutate output %q: %w", outputName, err)
			}
			outputs[outputName] = result
		}
		if err := e.evaluateStepLevelOutputs(ctx, step, outputVars, outputs); err != nil {
			return nil, err
		}
		if err := e.contextManager.WriteStepOutputs(ctx, step.Name, outputs); err != nil {
			return nil, fmt.Errorf("failed to write mutate step outputs: %w", err)
		}
	}

	return outputs, nil
}

// applyConfigurationPatch evaluates the CEL expression (returning a merge object) and deep-merges it into a copy of obj; returns merged object as JSON for Patch.
func (e *WorkflowExecutor) applyConfigurationPatch(ctx context.Context, obj *unstructured.Unstructured, expression string, vars map[string]interface{}) ([]byte, error) {
	result, err := e.celEvaluator.EvaluateExpression(ctx, expression, vars)
	if err != nil {
		return nil, fmt.Errorf("failed to evaluate applyConfiguration expression: %w", err)
	}
	patchMap, ok := result.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("applyConfiguration expression must return a map/object, got %T", result)
	}
	merged := deepMergeMap(obj.Object, patchMap)
	return json.Marshal(merged)
}

// deepMergeMap merges patch into base (patch wins; nested maps merged recursively)
func deepMergeMap(base, patch map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{})
	for k, v := range base {
		out[k] = v
	}
	for k, patchVal := range patch {
		if patchVal == nil {
			out[k] = nil
			continue
		}
		baseVal, exists := out[k]
		baseMap, baseOk := baseVal.(map[string]interface{})
		patchMap, patchOk := patchVal.(map[string]interface{})
		if exists && baseOk && patchOk {
			out[k] = deepMergeMap(baseMap, patchMap)
		} else {
			out[k] = patchVal
		}
	}
	return out
}

// jsonPatchData returns the JSON-serialized RFC 6902 patch (either from CEL expression or static operations)
func (e *WorkflowExecutor) jsonPatchData(ctx context.Context, jsonPatch *ottoflowv1alpha1.MutateJSONPatch, vars map[string]interface{}) ([]byte, error) {
	var ops []map[string]interface{}
	if jsonPatch == nil {
		return nil, fmt.Errorf("mutate patchType JSONPatch requires jsonPatch with expression or operations")
	}
	if jsonPatch.Expression != "" {
		result, err := e.celEvaluator.EvaluateExpression(ctx, jsonPatch.Expression, vars)
		if err != nil {
			return nil, fmt.Errorf("failed to evaluate jsonPatch expression: %w", err)
		}
		// CEL list of maps
		list, ok := result.([]interface{})
		if !ok {
			return nil, fmt.Errorf("jsonPatch expression must return a list of operations, got %T", result)
		}
		for i, item := range list {
			m, ok := item.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("jsonPatch operation at index %d must be a map, got %T", i, item)
			}
			ops = append(ops, m)
		}
	} else if len(jsonPatch.Operations) > 0 {
		for _, op := range jsonPatch.Operations {
			m := map[string]interface{}{"op": op.Op, "path": op.Path}
			if op.Value != nil {
				var v interface{}
				if err := json.Unmarshal(op.Value.Raw, &v); err != nil {
					return nil, fmt.Errorf("invalid jsonPatch operation value: %w", err)
				}
				m["value"] = v
			}
			if op.From != "" {
				m["from"] = op.From
			}
			ops = append(ops, m)
		}
	} else {
		return nil, fmt.Errorf("jsonPatch must set either expression or operations")
	}
	if len(ops) == 0 {
		return nil, nil
	}
	return json.Marshal(ops)
}
