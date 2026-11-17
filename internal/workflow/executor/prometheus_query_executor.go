/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"text/template"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

// executePrometheusQuery runs a Prometheus (PromQL) query step: resolves Variables into Query,
// executes the query, then evaluates Outputs over the result.
func (e *WorkflowExecutor) executePrometheusQuery(ctx context.Context, _ *ottoflowv1alpha1.WorkflowRun, step ottoflowv1alpha1.Step) (map[string]interface{}, error) {
	pq := step.PrometheusQuery
	if pq == nil {
		return nil, fmt.Errorf("prometheusQuery step has nil spec")
	}

	if e.prometheusClient == nil {
		return nil, fmt.Errorf("prometheus client not configured - cannot execute prometheusQuery step")
	}

	contextData, err := e.contextManager.ReadContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to read context: %w", err)
	}
	vars := e.celEvaluator.BuildVariableMap(contextData)

	// Resolve Variables (CEL) and substitute {{.varName}} in Query
	resolvedQuery := pq.Query
	if len(pq.Variables) > 0 {
		templateData := make(map[string]string)
		for name, celExpr := range pq.Variables {
			result, err := e.celEvaluator.EvaluateExpression(ctx, celExpr, vars)
			if err != nil {
				return nil, fmt.Errorf("failed to evaluate prometheusQuery variable %q: %w", name, err)
			}
			templateData[name] = fmt.Sprintf("%v", result)
		}
		tmpl, err := template.New("promql").Parse(pq.Query)
		if err != nil {
			return nil, fmt.Errorf("invalid prometheusQuery query template: %w", err)
		}
		var buf strings.Builder
		if err := tmpl.Execute(&buf, templateData); err != nil {
			return nil, fmt.Errorf("failed to substitute prometheusQuery variables: %w", err)
		}
		resolvedQuery = buf.String()
	}

	duration, err := parsePrometheusTimeRange(pq.TimeRange)
	if err != nil {
		return nil, fmt.Errorf("invalid prometheusQuery timeRange %q: %w", pq.TimeRange, err)
	}
	ts := time.Now().Add(-duration)

	result, err := e.prometheusClient.Query(ctx, resolvedQuery, ts)
	if err != nil {
		return nil, fmt.Errorf("prometheus query failed: %w", err)
	}

	resultMap := prometheusResultToMap(result)

	// Build context for output evaluation: workflow vars + result
	outputContext := make(map[string]interface{})
	for k, v := range contextData {
		outputContext[k] = v
	}
	outputContext["result"] = resultMap

	outputVars := e.celEvaluator.BuildVariableMap(outputContext)
	outputVars["result"] = resultMap

	outputs := make(map[string]interface{})
	if len(pq.Outputs) > 0 {
		for outputName, celExpr := range pq.Outputs {
			resultVal, err := e.celEvaluator.EvaluateExpression(ctx, celExpr, outputVars)
			if err != nil {
				return nil, fmt.Errorf("failed to evaluate prometheusQuery output %q: %w", outputName, err)
			}
			outputs[outputName] = resultVal
		}
	} else {
		// Default: expose full result as "result"
		outputs["result"] = resultMap
	}

	if err := e.evaluateStepLevelOutputs(ctx, step, outputVars, outputs); err != nil {
		return nil, err
	}

	if err := e.contextManager.WriteStepOutputs(ctx, step.Name, outputs); err != nil {
		return nil, fmt.Errorf("failed to write step outputs: %w", err)
	}
	return outputs, nil
}

// parsePrometheusTimeRange parses time range strings like "7d", "1h", "5m", "30s".
// Supports "d" (days) in addition to Go's time.ParseDuration (h, m, s, etc.).
func parsePrometheusTimeRange(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty timeRange")
	}
	if strings.HasSuffix(s, "d") {
		numStr := strings.TrimSuffix(s, "d")
		n, err := strconv.ParseFloat(strings.TrimSpace(numStr), 64)
		if err != nil {
			return 0, fmt.Errorf("invalid days value %q: %w", numStr, err)
		}
		return time.Duration(n * 24 * float64(time.Hour)), nil
	}
	return time.ParseDuration(s)
}
