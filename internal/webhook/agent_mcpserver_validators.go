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
// DefaultAgentExecutor recognises (see internal/agent/default_executor.go). "endpoint" is
// listed so it takes the dedicated warning below, not the generic unknown-key warning — the
// executor honors the key only if the operator has pre-approved its origin, and this webhook
// runs in the controller process, which does not have that operator allowlist available, so
// it can only warn that the run may fail rather than say whether it will.
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

// validateAgentConfig warns about spec.config keys no executor in this build reads, and
// about malformed values for keys that are read.
func validateAgentConfig(a *ottoflowv1alpha1.Agent) admission.Warnings {
	if a == nil || len(a.Spec.Config) == 0 {
		return nil
	}

	var unknown []string
	var warnings admission.Warnings
	for k, val := range a.Spec.Config {
		if _, ok := agentConfigKeysReadByDefaultExecutor[k]; !ok {
			unknown = append(unknown, k)
			continue
		}
		if k == "skipVerifySSL" {
			switch val {
			case "true":
				warnings = append(warnings, fmt.Sprintf(
					"spec.config.skipVerifySSL (%q) is no longer honored: it is rejected at execution time. "+
						"Preferred: mount the endpoint's CA bundle and set SSL_CERT_FILE or SSL_CERT_DIR on the "+
						"agent-executor, which keeps TLS verification on. Fallback: set LLM_SKIP_VERIFY_SSL=true "+
						"on the agent-executor process, which disables verification for all LLM egress from that "+
						"pod, not just this Agent.", val))
			case "false":
				// no-op: "false" is the safe value and remains legal.
			default:
				warnings = append(warnings, fmt.Sprintf(
					"spec.config.skipVerifySSL must be \"true\" or \"false\"; got %q, which is treated as false", val))
			}
		}
		if k == "endpoint" && val != "" {
			warnings = append(warnings, "spec.config.endpoint is honored only if the agent-executor "+
				"operator has allowlisted its origin (AGENT_LLM_ENDPOINT_ALLOWLIST / Helm "+
				"agentExecutor.llmEndpointAllowlist). Otherwise the agent run will FAIL.")
		}
	}

	if len(unknown) > 0 {
		sort.Strings(unknown)
		warnings = append(warnings, fmt.Sprintf(
			"spec.config keys %v are not read by the built-in agent executor (it recognises only endpoint, skipVerifySSL); "+
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
