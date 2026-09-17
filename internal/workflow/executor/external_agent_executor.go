/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

// executeExternalAgentStep executes an externalAgentRef step via the A2A protocol.
func (e *WorkflowExecutor) executeExternalAgentStep(
	ctx context.Context,
	workflowRun *ottoflowv1alpha1.WorkflowRun,
	step ottoflowv1alpha1.Step,
) (_ map[string]interface{}, err error) {
	ref := step.ExternalAgentRef

	ctx, a2aSpan := otel.Tracer("ottoflow").Start(ctx, "invoke_agent",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("gen_ai.operation.name", "invoke_agent"),
			attribute.String("gen_ai.system", "a2a"),
			attribute.String("a2a.agent.url", ref.URL),
		))
	defer func() {
		if err != nil {
			a2aSpan.SetStatus(codes.Error, err.Error())
		} else {
			a2aSpan.SetStatus(codes.Ok, "")
		}
		a2aSpan.End()
	}()

	if ref.Protocol != "" && ref.Protocol != "a2a" {
		return nil, fmt.Errorf("externalAgentRef protocol %q is not supported (only \"a2a\" is supported)", ref.Protocol)
	}

	// Parse timeout first so the entire step — rate limiting, CEL evaluation, secret
	// resolution, card fetch, and task execution — runs under a single deadline.
	timeout := a2aDefaultTimeout
	if ref.Timeout != "" {
		d, err := time.ParseDuration(ref.Timeout)
		if err != nil {
			return nil, fmt.Errorf("invalid externalAgentRef timeout %q: %w", ref.Timeout, err)
		}
		timeout = d
	}
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if err := e.waitOutboundRateLimit(timeoutCtx); err != nil {
		return nil, err
	}

	// Read current context and build CEL variable map
	contextData, err := e.contextManager.ReadContext(timeoutCtx)
	if err != nil {
		return nil, fmt.Errorf("failed to read context: %w", err)
	}
	vars := e.celEvaluator.BuildVariableMap(contextData)

	// Evaluate the prompt CEL expression → string
	promptVal, err := e.celEvaluator.EvaluateExpression(timeoutCtx, ref.Prompt, vars)
	if err != nil {
		return nil, fmt.Errorf("failed to evaluate externalAgentRef prompt: %w", err)
	}
	promptStr, ok := promptVal.(string)
	if !ok {
		return nil, fmt.Errorf("externalAgentRef prompt must evaluate to a string, got %T", promptVal)
	}

	// Build A2A client (resolves TLS and auth secrets — uses timeoutCtx so secret reads are bounded).
	namespace := workflowRun.Namespace
	a2aHTTPClient, err := newA2AClient(timeoutCtx, ref, e.controlClient, namespace, e.localExecutionMode)
	if err != nil {
		return nil, fmt.Errorf("failed to build A2A client: %w", err)
	}

	// Probe the agent card endpoint to verify agent availability (part of the A2A discovery handshake).
	if err := a2aHTTPClient.fetchAgentCard(timeoutCtx); err != nil {
		return nil, fmt.Errorf("failed to fetch agent card from %s: %w", ref.URL, err)
	}

	// Send task and wait for completion.
	result, err := a2aHTTPClient.sendTask(timeoutCtx, promptStr, timeout)
	if err != nil {
		return nil, fmt.Errorf("external agent task failed: %w", err)
	}

	// Convert raw result to map for CEL access
	a2aResult, err := result.ToMap()
	if err != nil {
		return nil, fmt.Errorf("failed to decode external agent task result: %w", err)
	}

	var outputs map[string]interface{}
	if len(step.Outputs) > 0 {
		// Re-read context (may have been updated by prior steps in forEach)
		contextData, err = e.contextManager.ReadContext(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to read context for output evaluation: %w", err)
		}
		outputContext := make(map[string]interface{}, len(contextData))
		for k, v := range contextData {
			outputContext[k] = v
		}

		outputVars := e.celEvaluator.BuildVariableMap(outputContext)
		outputVars["a2aResult"] = a2aResult

		outputs, err = e.celEvaluator.EvaluateStepOutputs(ctx, step, outputVars)
		if err != nil {
			return nil, fmt.Errorf("failed to evaluate step outputs: %w", err)
		}
	} else {
		// No output definitions — expose a2aResult directly
		outputs = map[string]interface{}{
			"a2aResult": a2aResult,
		}
	}

	if err := e.contextManager.WriteStepOutputs(ctx, step.Name, outputs); err != nil {
		return nil, fmt.Errorf("failed to write step outputs: %w", err)
	}
	return outputs, nil
}
