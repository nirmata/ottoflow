/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package webhook

import (
	"context"
	"fmt"
	"sort"

	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

// agentConfigKeysReadByDefaultExecutor lists the spec.config keys the built-in
// DefaultAgentExecutor recognises (see internal/agent/default_executor.go). endpoint and
// skipVerifySSL="true" get their own warnings below: the executor refuses them unless the
// operator's endpoint allowlist admits the endpoint, and that allowlist lives in the
// agent-executor process, not in this webhook, so the webhook can only warn.
//
// Unrecognised keys are reported as warnings, never errors: spec.config is a free-form
// map and an alternative AgentExecutor implementation may legitimately read keys this
// build knows nothing about. The warning exists to catch typos and keys the CRD
// doc-comment advertises but no executor in this build reads.
var agentConfigKeysReadByDefaultExecutor = map[string]struct{}{
	"endpoint":      {},
	"skipVerifySSL": {},
}

// AgentValidator validates Agent resources.
type AgentValidator struct{}

func (v *AgentValidator) ValidateCreate(ctx context.Context, a *ottoflowv1alpha1.Agent) (admission.Warnings, error) {
	return validateAgentConfig(a), nil
}
func (v *AgentValidator) ValidateUpdate(ctx context.Context, oldA, a *ottoflowv1alpha1.Agent) (admission.Warnings, error) {
	return validateAgentConfig(a), nil
}

// validateAgentConfig warns about an unsupported modelProvider, spec.config keys no executor in this
// build reads, and about malformed values for keys that are read.
func validateAgentConfig(a *ottoflowv1alpha1.Agent) admission.Warnings {
	if a == nil {
		return nil
	}

	var unknown []string
	var warnings admission.Warnings
	if a.Spec.ModelProvider == "azure-openai" {
		warnings = append(warnings, "spec.modelProvider azure-openai is not supported: "+
			"the Azure OpenAI client in this build does not keep the model's tool calls and replies in the conversation history (single-turn prompts may still work)")
	}

	for k, val := range a.Spec.Config {
		if _, ok := agentConfigKeysReadByDefaultExecutor[k]; !ok {
			unknown = append(unknown, k)
			continue
		}
		switch {
		case k == "skipVerifySSL" && val == "true":
			warnings = append(warnings, "spec.config.skipVerifySSL=\"true\" is no longer honored: the agent run "+
				"will FAIL. To reach an endpoint with a private CA, set SSL_CERT_FILE or SSL_CERT_DIR on the "+
				"agent-executor, which keeps TLS verification on.")
		case k == "skipVerifySSL" && val != "false":
			warnings = append(warnings, fmt.Sprintf(
				"spec.config.skipVerifySSL must be \"true\" or \"false\"; got %q, which is treated as false", val))
		case k == "endpoint" && val != "":
			warnings = append(warnings, "spec.config.endpoint is honored only for providers that read it (anthropic, azure-openai), "+
				"and only if it is origin-only (no path, query or fragment) and the agent-executor operator "+
				"has allowlisted its origin (AGENT_LLM_ENDPOINT_ALLOWLIST / Helm "+
				"agentExecutor.llmEndpointAllowlist). Otherwise the agent run will FAIL. "+
				"google, gemini and nirmata always FAIL when it is set; openai and local ignore it.")
		}
	}

	if len(unknown) > 0 {
		sort.Strings(unknown)
		warnings = append(warnings, fmt.Sprintf(
			"spec.config keys %v are not read by the built-in agent executor (it reads only endpoint, skipVerifySSL); "+
				"they will be ignored unless a custom executor consumes them", unknown))
	}
	return warnings
}
func (v *AgentValidator) ValidateDelete(ctx context.Context, a *ottoflowv1alpha1.Agent) (admission.Warnings, error) {
	return nil, nil
}

// MCPServerValidator validates MCPServer resources (placeholder for future rules).
type MCPServerValidator struct{}

func (v *MCPServerValidator) ValidateCreate(ctx context.Context, m *ottoflowv1alpha1.MCPServer) (admission.Warnings, error) {
	return nil, nil
}
func (v *MCPServerValidator) ValidateUpdate(ctx context.Context, oldM, m *ottoflowv1alpha1.MCPServer) (admission.Warnings, error) {
	return nil, nil
}
func (v *MCPServerValidator) ValidateDelete(ctx context.Context, m *ottoflowv1alpha1.MCPServer) (admission.Warnings, error) {
	return nil, nil
}
