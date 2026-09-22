/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package agent

import (
	"context"
	"fmt"

	"github.com/GoogleCloudPlatform/kubectl-ai/gollm"
	"github.com/GoogleCloudPlatform/kubectl-ai/pkg/tools"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"k8s.io/klog/v2"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/agent/toolloop"
	"github.com/nirmata/ottoflow/internal/tracing"
)

// DefaultAgentExecutor implements AgentExecutor using only public dependencies:
// kubectl-ai's gollm client and the ported tool-calling loop in internal/agent/toolloop.
// It has no private dependencies and supports every ModelProvider except
// "nirmata", which is served only by the enterprise plugin — see RoutingAgentExecutor
// for how the two are combined.
type DefaultAgentExecutor struct {
	mcpProvider MCPClientProvider
	llmFactory  LLMClientFactory // optional; when set, used instead of gollm.NewClient (for tests)
}

// NewDefaultAgentExecutor creates a new DefaultAgentExecutor.
// If mcpProvider is non-nil, agents with Spec.MCPTools will get those tools registered for LLM execution.
func NewDefaultAgentExecutor(mcpProvider MCPClientProvider) *DefaultAgentExecutor {
	return &DefaultAgentExecutor{mcpProvider: mcpProvider}
}

// NewDefaultAgentExecutorWithLLMFactory creates a DefaultAgentExecutor with an optional LLM client factory (for tests).
// When llmFactory is non-nil, it is used instead of gollm.NewClient so tests can inject a mock LLM.
func NewDefaultAgentExecutorWithLLMFactory(mcpProvider MCPClientProvider, llmFactory LLMClientFactory) *DefaultAgentExecutor {
	return &DefaultAgentExecutor{mcpProvider: mcpProvider, llmFactory: llmFactory}
}

// ExecuteAgent executes an agent step against a public gollm provider.
// namespace is used for MCP server/tool lookup when the agent has Spec.MCPTools configured.
func (e *DefaultAgentExecutor) ExecuteAgent(ctx context.Context, agentCRD *ottoflowv1alpha1.Agent, prompt string, workflowContext map[string]interface{}, namespace string) (string, AgentTokenUsage, error) {
	klog.V(2).InfoS("Executing agent step", "agent", agentCRD.Name, "provider", agentCRD.Spec.ModelProvider)

	provider := agentCRD.Spec.ModelProvider

	llmClient, err := e.createLLMClient(ctx, agentCRD)
	if err != nil {
		klog.ErrorS(err, "Failed to create LLM client", "agent", agentCRD.Name, "provider", provider)
		return "", AgentTokenUsage{}, fmt.Errorf("failed to create LLM client: %w", err)
	}

	model := agentCRD.Spec.ModelName
	if model == "" {
		model = e.getDefaultModel(provider)
	}
	klog.V(4).InfoS("Using model", "agent", agentCRD.Name, "model", model)

	toolset := map[string]tools.Tool{}
	if len(agentCRD.Spec.MCPTools) > 0 && e.mcpProvider != nil {
		sessionTools, err := buildSessionToolsFromMCP(ctx, agentCRD.Spec.MCPTools, namespace, e.mcpProvider)
		if err != nil {
			klog.ErrorS(err, "Failed to build MCP session tools", "agent", agentCRD.Name)
			return "", AgentTokenUsage{}, fmt.Errorf("building MCP session tools: %w", err)
		}
		if len(sessionTools) > 0 {
			toolset = sessionTools
			klog.V(3).InfoS("Registered MCP tools for agent", "agent", agentCRD.Name, "toolCount", len(sessionTools))
		}
	}

	// prompt doubles as both the system prompt (StartChat) and the first user
	// message (toolloop.Run), matching the Nirmata-backed executor's behavior.
	chat := llmClient.StartChat(prompt, model)
	functionDefinitions := make([]*gollm.FunctionDefinition, 0, len(toolset))
	for _, tool := range toolset {
		functionDefinitions = append(functionDefinitions, tool.FunctionDefinition())
	}
	if err := chat.SetFunctionDefinitions(functionDefinitions); err != nil {
		return "", AgentTokenUsage{}, fmt.Errorf("setting function definitions: %w", err)
	}

	// chat span: one per LLM call, child of invoke_agent (started in agent_executor.go).
	klog.V(3).InfoS("Streaming agent conversation", "agent", agentCRD.Name)
	chatCtx, chatSpan := otel.Tracer("ottoflow").Start(ctx, "chat",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("gen_ai.operation.name", "chat"),
			attribute.String("gen_ai.system", provider),
			attribute.String("gen_ai.request.model", model),
		))
	response, lastUsage, err := toolloop.Run(chatCtx, chat, []any{prompt}, toolset, toolloop.Options{})
	if err != nil {
		chatSpan.SetStatus(codes.Error, err.Error())
		chatSpan.End()
		klog.ErrorS(err, "Agent execution failed", "agent", agentCRD.Name)
		return "", AgentTokenUsage{}, fmt.Errorf("agent execution failed: %s: %w", condenseLLMError(err), err)
	}
	inTokens, outTokens := usageTokenCounts(lastUsage)
	if inTokens > 0 || outTokens > 0 {
		chatSpan.SetAttributes(
			tracing.GenAIUsageInputTokens.Int64(inTokens),
			tracing.GenAIUsageOutputTokens.Int64(outTokens),
		)
	}
	chatSpan.SetStatus(codes.Ok, "")
	chatSpan.End()

	klog.V(2).InfoS("Agent execution completed", "agent", agentCRD.Name, "responseLength", len(response))
	return response, AgentTokenUsage{InputTokens: inTokens, OutputTokens: outTokens}, nil
}

// createLLMClient creates an LLM client based on the provider.
func (e *DefaultAgentExecutor) createLLMClient(ctx context.Context, agentCRD *ottoflowv1alpha1.Agent) (gollm.Client, error) {
	provider := agentCRD.Spec.ModelProvider

	providerID := e.mapProviderToID(provider)
	if providerID == "" {
		klog.ErrorS(nil, "Unknown provider", "provider", provider)
		return nil, fmt.Errorf("unknown provider: %s", provider)
	}

	klog.V(4).InfoS("Creating LLM client", "provider", provider, "providerID", providerID)

	// Both checks below apply to every provider, before the client (and its credential) is
	// ever constructed -- not just azure-openai, which is the only provider that honors
	// endpoint today. A provider that ignores the key gets no exception: a stale endpoint
	// that is inert now would go live the instant an SDK started reading it, and a loud
	// failure beats that silent trap.
	var opts []gollm.Option
	if endpoint := agentCRD.Spec.Config["endpoint"]; endpoint != "" {
		parsed, aerr := allowedLLMEndpoint(endpoint)
		if aerr != nil {
			return nil, fmt.Errorf("agent endpoint rejected: spec.config.endpoint (%q) is not permitted "+
				"for provider %s: %v -- ask the operator to add its origin to %s (Helm: "+
				"agentExecutor.llmEndpointAllowlist). Listing an origin authorises it to receive the "+
				"agent-executor's shared LLM credential", endpoint, provider, aerr, AgentLLMEndpointAllowlistEnv)
		}
		klog.V(4).InfoS("Setting custom endpoint", "endpoint", endpoint)
		opts = append(opts, func(co *gollm.ClientOptions) { co.URL = parsed })
	}
	if agentCRD.Spec.Config["skipVerifySSL"] == "true" {
		return nil, fmt.Errorf("agent config rejected: spec.config.skipVerifySSL is no longer honored "+
			"(provider %q): the agent-executor's LLM credential is shared process-wide, so a "+
			"tenant-authored Agent may not turn off TLS verification for requests carrying it. "+
			"Preferred: mount the endpoint's CA bundle and set SSL_CERT_FILE or SSL_CERT_DIR on the "+
			"agent-executor, which keeps TLS verification ON. Fallback: set LLM_SKIP_VERIFY_SSL=true "+
			"on the agent-executor process, which disables verification for ALL LLM egress from that "+
			"pod, not just this Agent", provider)
	}

	var client gollm.Client
	var err error
	if e.llmFactory != nil {
		client, err = e.llmFactory.NewClient(ctx, providerID, opts...)
	} else {
		client, err = gollm.NewClient(ctx, providerID, opts...)
	}
	if err != nil {
		klog.ErrorS(err, "Failed to create LLM client", "provider", provider, "providerID", providerID)
		return nil, fmt.Errorf("failed to create LLM client for provider %s: %w", provider, err)
	}

	klog.V(4).InfoS("LLM client created successfully", "provider", provider)
	return client, nil
}

// mapProviderToID maps OttoFlow provider names to gollm provider IDs, using only
// IDs that are registered in gollm's provider registry (openai, anthropic,
// azopenai, gemini, vertexai, bedrock, grok, llamacpp).
func (e *DefaultAgentExecutor) mapProviderToID(provider string) string {
	switch provider {
	case providerOpenAI:
		return providerOpenAI
	case providerAnthropic:
		return providerAnthropic
	case providerAzureOpenAI:
		return "azopenai"
	case providerGoogle, providerGemini:
		return providerGemini
	case providerLocal:
		return "llamacpp"
	default:
		// Includes "nirmata" and "": DefaultAgentExecutor never handles those in
		// practice, since RoutingAgentExecutor routes them to the Nirmata delegate.
		return ""
	}
}

// getDefaultModel returns the default model for a provider. It has no Nirmata
// fallback; the "nirmata" provider is handled by the enterprise plugin.
func (e *DefaultAgentExecutor) getDefaultModel(provider string) string {
	switch provider {
	case providerOpenAI:
		return "gpt-4o"
	case providerAnthropic:
		return "claude-opus-5"
	case providerAzureOpenAI:
		return "gpt-4o"
	case providerGoogle, providerGemini:
		return "gemini-3.6-flash"
	default:
		return ""
	}
}
