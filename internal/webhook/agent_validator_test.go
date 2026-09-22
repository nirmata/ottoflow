/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package webhook

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

func agentWithConfig(config map[string]string) *ottoflowv1alpha1.Agent {
	return &ottoflowv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "default"},
		Spec:       ottoflowv1alpha1.AgentSpec{Config: config},
	}
}

func TestAgentValidatorConfigWarnings(t *testing.T) {
	tests := []struct {
		name         string
		config       map[string]string
		wantCount    int
		wantContains []string
	}{
		{
			name:      "no config produces no warnings",
			config:    nil,
			wantCount: 0,
		},
		{
			name:         "endpoint and skipVerifySSL=true each get their own dedicated warning",
			config:       map[string]string{"endpoint": "https://llm.example.com", "skipVerifySSL": "true"},
			wantCount:    2,
			wantContains: []string{"spec.config.endpoint is honored only if", "spec.config.skipVerifySSL", "no longer honored"},
		},
		{
			name:      "skipVerifySSL=false with no endpoint produces no warnings",
			config:    map[string]string{"skipVerifySSL": "false"},
			wantCount: 0,
		},
		{
			// The CRD doc-comment advertises apiKey/baseURL, but no executor in this build
			// reads them. Silently dropping them was the originally reported bug.
			name:         "keys no executor reads are reported",
			config:       map[string]string{"apiKey": "sk-x", "baseURL": "https://x"},
			wantCount:    1,
			wantContains: []string{"apiKey", "baseURL", "not read by the built-in agent executor"},
		},
		{
			name:         "malformed skipVerifySSL is reported",
			config:       map[string]string{"skipVerifySSL": "yes"},
			wantCount:    1,
			wantContains: []string{"skipVerifySSL", "treated as false"},
		},
		{
			name:         "malformed known key and unknown key are reported separately",
			config:       map[string]string{"skipVerifySSL": "yes", "projectId": "p"},
			wantCount:    2,
			wantContains: []string{"skipVerifySSL", "projectId"},
		},
	}

	v := &AgentValidator{}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			warnings, err := v.ValidateCreate(context.Background(), agentWithConfig(tc.config))
			if err != nil {
				t.Fatalf("ValidateCreate returned an error, want warnings only: %v", err)
			}
			if len(warnings) != tc.wantCount {
				t.Fatalf("got %d warnings %v, want %d", len(warnings), warnings, tc.wantCount)
			}
			joined := strings.Join(warnings, " | ")
			for _, want := range tc.wantContains {
				if !strings.Contains(joined, want) {
					t.Errorf("warnings %q do not mention %q", joined, want)
				}
			}
		})
	}
}

// Unknown config keys must never block admission: spec.config is free-form and an
// alternative AgentExecutor may read keys this build does not know about.
func TestAgentValidatorNeverRejects(t *testing.T) {
	v := &AgentValidator{}
	agent := agentWithConfig(map[string]string{"totallyUnknown": "v", "skipVerifySSL": "maybe"})

	if _, err := v.ValidateCreate(context.Background(), agent); err != nil {
		t.Errorf("ValidateCreate rejected an Agent with unknown config: %v", err)
	}
	if _, err := v.ValidateUpdate(context.Background(), agentWithConfig(nil), agent); err != nil {
		t.Errorf("ValidateUpdate rejected an Agent with unknown config: %v", err)
	}
	if _, err := v.ValidateDelete(context.Background(), agent); err != nil {
		t.Errorf("ValidateDelete returned an error: %v", err)
	}
}
