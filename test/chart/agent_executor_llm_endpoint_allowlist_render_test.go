/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package chart

import (
	"os"
	"path/filepath"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"
)

// renderAllowlistEnv renders the chart with the given values (empty for defaults) and returns
// the AGENT_LLM_ENDPOINT_ALLOWLIST env entry of any rendered Deployment container.
func renderAllowlistEnv(t *testing.T, values string) (found bool, value string) {
	t.Helper()
	valuesFile := ""
	if values != "" {
		valuesFile = filepath.Join(t.TempDir(), "values.yaml")
		if err := os.WriteFile(valuesFile, []byte(values), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	output, err := helmTemplate(t, "ottoflow", valuesFile)
	if err != nil {
		t.Fatalf("helm template failed: %v", err)
	}
	for _, doc := range splitYAMLDocs(output) {
		var d appsv1.Deployment
		if yaml.Unmarshal([]byte(doc), &d) != nil || d.Kind != "Deployment" {
			continue
		}
		for _, c := range d.Spec.Template.Spec.Containers {
			for _, e := range c.Env {
				if e.Name == "AGENT_LLM_ENDPOINT_ALLOWLIST" {
					return true, e.Value
				}
			}
		}
	}
	return false, ""
}

// A default install renders no allowlist, so an Agent's spec.config.endpoint is refused.
func TestAgentLLMEndpointAllowlist_AbsentByDefault(t *testing.T) {
	if found, value := renderAllowlistEnv(t, ""); found {
		t.Errorf("AGENT_LLM_ENDPOINT_ALLOWLIST rendered by default with value %q, want absent", value)
	}
}

// agentExecutor.llmEndpointAllowlist renders onto the agent-executor container, comma-joined.
func TestAgentLLMEndpointAllowlist_RenderedCommaJoined(t *testing.T) {
	for _, tc := range []struct{ values, want string }{
		{"agentExecutor:\n  llmEndpointAllowlist: [\"https://llm.corp.example\"]\n", "https://llm.corp.example"},
		{"agentExecutor:\n  llmEndpointAllowlist: [\"https://llm.corp.example\", \"https://other.corp.example\"]\n",
			"https://llm.corp.example,https://other.corp.example"},
	} {
		found, value := renderAllowlistEnv(t, tc.values)
		if !found || value != tc.want {
			t.Errorf("AGENT_LLM_ENDPOINT_ALLOWLIST found=%v value=%q, want %q", found, value, tc.want)
		}
	}
}
