/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package agent

import (
	"context"
	"fmt"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

// RoutingAgentExecutor dispatches ExecuteAgent to the Nirmata delegate for
// ModelProvider "nirmata" (and the empty/default provider) and to
// DefaultAgentExecutor for every other provider. Routing happens per call,
// not at construction time, because ModelProvider is a field on the Agent CRD
// passed into ExecuteAgent, while a single RoutingAgentExecutor instance is
// constructed once and reused across many Agents with potentially different
// providers.
//
// In this source-available build the Nirmata delegate is nirmataUnavailableExecutor,
// which returns a clear enterprise-required error; the enterprise plugin swaps
// in a real Nirmata-backed executor via NewRoutingAgentExecutorFromExecutors.
type RoutingAgentExecutor struct {
	nirmata AgentExecutor
	def     AgentExecutor
}

// NewRoutingAgentExecutor creates a RoutingAgentExecutor whose Nirmata delegate
// reports that the enterprise plugin is required, and whose default delegate is
// a real DefaultAgentExecutor bound to mcpProvider.
func NewRoutingAgentExecutor(mcpProvider MCPClientProvider) *RoutingAgentExecutor {
	return NewRoutingAgentExecutorFromExecutors(
		newNirmataUnavailableExecutor(),
		NewDefaultAgentExecutor(mcpProvider),
	)
}

// NewRoutingAgentExecutorFromExecutors injects both delegate executors directly.
// The enterprise plugin uses this to supply a real Nirmata-backed executor; tests
// use it to inject mocks.
func NewRoutingAgentExecutorFromExecutors(nirmataExec, defaultExec AgentExecutor) *RoutingAgentExecutor {
	return &RoutingAgentExecutor{nirmata: nirmataExec, def: defaultExec}
}

// ExecuteAgent routes to the Nirmata delegate for ModelProvider "nirmata" (and
// empty — retained only for backward compatibility with Agent objects stored
// before modelProvider became a required field; CRD required is enforced at
// admission, not retroactively), and to the default delegate for every other
// provider.
func (e *RoutingAgentExecutor) ExecuteAgent(ctx context.Context, agentCRD *ottoflowv1alpha1.Agent, prompt string, workflowContext map[string]interface{}, namespace string) (string, AgentTokenUsage, error) {
	provider := agentCRD.Spec.ModelProvider
	// Only the anthropic client sends spec.thinking; refuse a set block here, where every
	// Agent run passes, so no delegate can drop it silently.
	if agentCRD.Spec.Thinking.IsSet() && provider != providerAnthropic {
		return "", AgentTokenUsage{}, fmt.Errorf("agent config rejected: spec.thinking is supported only for "+
			"modelProvider %s, not %q; remove it from this Agent", providerAnthropic, provider)
	}
	if provider == "" || provider == providerNirmata {
		return e.nirmata.ExecuteAgent(ctx, agentCRD, prompt, workflowContext, namespace)
	}
	return e.def.ExecuteAgent(ctx, agentCRD, prompt, workflowContext, namespace)
}
