/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/mark3labs/mcp-go/client/transport"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/agent"
)

// mcpToolErrorMessage recognises the envelope the kubectl-ai fork returns, with a nil error, for an isError tool result.
func mcpToolErrorMessage(result interface{}) (string, bool) {
	const prefix, suffix = `{"error": true, "message": `, `, "status": "failed"}`
	switch r := result.(type) {
	case map[string]interface{}:
		msg, _ := r["message"].(string)
		return msg, r["error"] == true && r["status"] == "failed"
	case string:
		if !strings.HasPrefix(r, prefix) || !strings.HasSuffix(r, suffix) {
			return "", false
		}
		msg, err := strconv.Unquote(r[len(prefix) : len(r)-len(suffix)])
		return msg, err == nil
	}
	return "", false
}

// executeMCPToolCall executes an MCP tool call step
func (e *WorkflowExecutor) executeMCPToolCall(ctx context.Context, workflowRun *ottoflowv1alpha1.WorkflowRun, step ottoflowv1alpha1.Step) (_ map[string]interface{}, err error) {
	mcpToolCall := step.MCPToolCall

	ctx, toolSpan := otel.Tracer("ottoflow").Start(ctx, "execute_tool",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("gen_ai.operation.name", "execute_tool"),
			attribute.String("gen_ai.tool.name", mcpToolCall.Tool),
			attribute.String("mcp.server.name", mcpToolCall.Server),
		))
	defer func() {
		if err != nil {
			toolSpan.SetStatus(codes.Error, err.Error())
		} else {
			toolSpan.SetStatus(codes.Ok, "")
		}
		toolSpan.End()
	}()

	// Best-effort: fetch MCPServer CRD to add transport type to the span.
	// Skipped silently if not found (e.g. in tests with a fake client).
	var mcpServerCRD ottoflowv1alpha1.MCPServer
	if getErr := e.controlClient.Get(ctx,
		client.ObjectKey{Name: mcpToolCall.Server, Namespace: workflowRun.Namespace},
		&mcpServerCRD); getErr == nil {
		toolSpan.SetAttributes(attribute.String("mcp.transport.type", mcpServerCRD.Spec.Transport.Type))
	}

	if err := e.waitOutboundRateLimit(ctx); err != nil {
		return nil, err
	}

	// Read current context for argument evaluation
	contextData, err := e.contextManager.ReadContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to read context: %w", err)
	}

	// Build variable map for CEL evaluation
	vars := e.celEvaluator.BuildVariableMap(contextData)

	// Resolve tool arguments using CEL expressions
	resolvedArgs := make(map[string]interface{})
	for argName, argExpr := range mcpToolCall.Arguments {
		result, err := e.celEvaluator.EvaluateExpression(ctx, argExpr, vars)
		if err != nil {
			return nil, fmt.Errorf("failed to evaluate argument '%s': %w", argName, err)
		}
		resolvedArgs[argName] = result
	}

	// Get MCP client for the server
	mcpClient, err := e.mcpManager.GetClient(ctx, mcpToolCall.Server, workflowRun.Namespace)
	if err != nil {
		return nil, fmt.Errorf("failed to get MCP client for server '%s': %w", mcpToolCall.Server, err)
	}

	// Call the MCP tool
	toolResult, err := mcpClient.CallTool(ctx, mcpToolCall.Tool, resolvedArgs)
	if err != nil {
		// The server dropped the session (e.g. it restarted): the cached client stays dead, so
		// evict it and let the next call build a new one.
		if errors.Is(err, transport.ErrSessionTerminated) {
			if ev, ok := e.mcpManager.(agent.MCPClientEvicter); ok {
				ev.Evict(workflowRun.Namespace, mcpToolCall.Server, mcpClient)
			}
		}
		return nil, fmt.Errorf("MCP tool call failed: %w", err)
	}
	if msg, isToolErr := mcpToolErrorMessage(toolResult); isToolErr {
		return nil, fmt.Errorf("MCP tool reported an error: %s", msg)
	}

	var outputs map[string]interface{}
	// If step has output definitions, evaluate them with tool result in context
	if len(step.Outputs) > 0 {
		// Add tool result to context for output evaluation
		outputContext := make(map[string]interface{})
		for k, v := range contextData {
			outputContext[k] = v
		}
		outputContext["toolResult"] = toolResult

		// Build variable map for output evaluation
		outputVars := e.celEvaluator.BuildVariableMap(outputContext)
		outputVars["toolResult"] = toolResult

		// Evaluate step outputs
		outputs, err = e.celEvaluator.EvaluateStepOutputs(ctx, step, outputVars)
		if err != nil {
			return nil, fmt.Errorf("failed to evaluate step outputs: %w", err)
		}

		// Write outputs to context
		if err := e.contextManager.WriteStepOutputs(ctx, step.Name, outputs); err != nil {
			return nil, fmt.Errorf("failed to write step outputs: %w", err)
		}
	} else {
		// No output definitions - store tool result directly
		outputs = map[string]interface{}{
			"result": toolResult,
		}
		if err := e.contextManager.WriteStepOutputs(ctx, step.Name, outputs); err != nil {
			return nil, fmt.Errorf("failed to write step outputs: %w", err)
		}
	}

	return outputs, nil
}
