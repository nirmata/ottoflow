/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package agent

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

// These tests assert on what a fake LLM endpoint actually received on the wire, through the
// production entry point (DefaultAgentExecutor.ExecuteAgent), not on a mock a few layers above.

const operatorAzureKey = "azure-OPERATOR-SHARED-SECRET"

// capturedRequest records what a fake LLM endpoint received.
type capturedRequest struct {
	mu     sync.Mutex
	hits   int
	apiKey string
}

func (c *capturedRequest) snapshot() (hits int, apiKey string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, c.apiKey
}

// newAzureCaptureServer starts a fake Azure OpenAI endpoint. gollm's azure client forces the
// endpoint scheme to https, so it must be a TLS server; tests trust it through the operator-side
// LLM_SKIP_VERIFY_SSL process variable, never through Agent.spec.config.skipVerifySSL.
func newAzureCaptureServer(t *testing.T) (*httptest.Server, *capturedRequest) {
	t.Helper()
	c := &capturedRequest{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		c.mu.Lock()
		c.hits++
		c.apiKey = r.Header.Get("api-key")
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"index":0,"finish_reason":"stop",` +
			`"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv, c
}

// setOperatorAzureEnv sets the agent-executor process environment an operator would: the shared
// Azure key, and trust for the self-signed test server. Every other provider variable a shell
// might export is cleared so it cannot steer the request.
func setOperatorAzureEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"AZURE_OPENAI_ENDPOINT", "AGENT_LLM_ENDPOINT_ALLOWLIST",
		"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "OPENAI_ENDPOINT", "OPENAI_API_BASE"} {
		t.Setenv(k, "")
	}
	t.Setenv("AZURE_OPENAI_API_KEY", operatorAzureKey)
	t.Setenv("LLM_SKIP_VERIFY_SSL", "true")
}

func runAzureAgent(t *testing.T, config map[string]string) error {
	t.Helper()
	agentCRD := &ottoflowv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "tenant-agent", Namespace: "tenant-a"},
		Spec: ottoflowv1alpha1.AgentSpec{
			ModelProvider: "azure-openai", ModelName: "gpt-4o", Config: config,
		},
	}
	_, _, err := NewDefaultAgentExecutor(nil).ExecuteAgent(context.Background(), agentCRD, "prompt", nil, "tenant-a")
	return err
}

// An endpoint the operator has not allowlisted never receives a request, so the operator's
// shared credential never leaves the process toward it.
func TestLLMEndpointCredential_NotAllowlisted_NeverDialed(t *testing.T) {
	srv, c := newAzureCaptureServer(t)
	setOperatorAzureEnv(t)

	err := runAzureAgent(t, map[string]string{"endpoint": srv.URL})

	if hits, apiKey := c.snapshot(); hits != 0 {
		t.Fatalf("endpoint not on the allowlist received %d request(s) (api-key operator credential: %v); want 0",
			hits, apiKey == operatorAzureKey)
	}
	if err == nil {
		t.Fatal("ExecuteAgent: want a refusal for an endpoint that is not allowlisted, got nil")
	}
}

// With AZURE_OPENAI_ENDPOINT pinned by the operator, a tenant endpoint that is not allowlisted
// is still refused before any client exists: neither server sees a request.
func TestLLMEndpointCredential_NotAllowlisted_OperatorEndpointPinned_NeverDialed(t *testing.T) {
	operatorSrv, operator := newAzureCaptureServer(t)
	tenantSrv, tenant := newAzureCaptureServer(t)
	setOperatorAzureEnv(t)
	t.Setenv("AZURE_OPENAI_ENDPOINT", operatorSrv.URL)

	err := runAzureAgent(t, map[string]string{"endpoint": tenantSrv.URL})

	if hits, _ := tenant.snapshot(); hits != 0 {
		t.Errorf("tenant endpoint received %d request(s); want 0", hits)
	}
	if hits, _ := operator.snapshot(); hits != 0 {
		t.Errorf("operator endpoint received %d request(s); want 0 (no client may be built)", hits)
	}
	if err == nil {
		t.Fatal("ExecuteAgent: want a refusal for an endpoint that is not allowlisted, got nil")
	}
}

// The positive control: once the operator allowlists the origin, the request reaches it with the
// operator's credential. Without this, the zero-hit assertions above could pass vacuously.
func TestLLMEndpointCredential_Allowlisted_ReceivesOperatorKey(t *testing.T) {
	for _, suffix := range []string{"", "/"} { // a bare trailing slash is still origin-only
		srv, c := newAzureCaptureServer(t)
		setOperatorAzureEnv(t)
		t.Setenv("AGENT_LLM_ENDPOINT_ALLOWLIST", srv.URL)

		_ = runAzureAgent(t, map[string]string{"endpoint": srv.URL + suffix}) // the capture is what matters

		hits, apiKey := c.snapshot()
		if hits != 1 || apiKey != operatorAzureKey {
			t.Fatalf("allowlisted endpoint %q: hits=%d, operator key received=%v; want 1 and true",
				srv.URL+suffix, hits, apiKey == operatorAzureKey)
		}
	}
}

// Each of these is refused before any client exists, even though the origin is allowlisted,
// and the refusal names the rule that fired.
func TestLLMEndpointCredential_AllowlistedOrigin_RefusedForms_NeverDialed(t *testing.T) {
	cases := []struct {
		name, want string
		endpoint   func(srvURL string) string
		config     map[string]string
	}{
		{name: "path", want: "path/query/fragment are not permitted",
			endpoint: func(u string) string { return u + "/other-backend" }},
		{name: "query", want: "path/query/fragment are not permitted",
			endpoint: func(u string) string { return u + "?route=b" }},
		{name: "fragment", want: "path/query/fragment are not permitted",
			endpoint: func(u string) string { return u + "#b" }},
		{name: "userinfo", want: "userinfo is not permitted",
			endpoint: func(u string) string { return strings.Replace(u, "https://", "https://user:pass@", 1) }},
		{name: "http on an allowlisted http origin", want: "rewrites the endpoint scheme to https",
			endpoint: func(u string) string { return strings.Replace(u, "https://", "http://", 1) }},
		{name: "skipVerifySSL", want: "skipVerifySSL is no longer honored",
			endpoint: func(u string) string { return u },
			config:   map[string]string{"skipVerifySSL": "true"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, c := newAzureCaptureServer(t)
			setOperatorAzureEnv(t)
			httpOrigin := strings.Replace(srv.URL, "https://", "http://", 1)
			t.Setenv("AGENT_LLM_ENDPOINT_ALLOWLIST", srv.URL+","+httpOrigin)

			config := map[string]string{"endpoint": tc.endpoint(srv.URL)}
			for k, v := range tc.config {
				config[k] = v
			}
			err := runAzureAgent(t, config)

			if hits, _ := c.snapshot(); hits != 0 {
				t.Fatalf("endpoint received %d request(s); want 0", hits)
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ExecuteAgent error = %v; want it to contain %q", err, tc.want)
			}
		})
	}
}

// A non-ASCII host could fold onto an allowlisted ASCII name (U+0130 lowercases to "i", the
// Kelvin sign U+212A to "k") while the transport dials a different name, so both the operator's
// entry and the tenant's endpoint must be ASCII.
func TestAllowedLLMEndpoint_RejectsNonASCIIHost(t *testing.T) {
	t.Setenv(AgentLLMEndpointAllowlistEnv, "https://api.ilk.example")
	if _, err := allowedLLMEndpoint("https://api.ilk.example"); err != nil {
		t.Fatalf("control: ASCII origin must be admitted: %v", err)
	}
	for _, host := range []string{"https://api.İlk.example", "https://api.ilK.example"} {
		if _, err := allowedLLMEndpoint(host); err == nil || !strings.Contains(err.Error(), "must be ASCII") {
			t.Errorf("allowedLLMEndpoint(%q) error = %v; want a must-be-ASCII refusal", host, err)
		}
		t.Setenv(AgentLLMEndpointAllowlistEnv, host)
		if err := ValidateLLMEndpointAllowlist(); err == nil || !strings.Contains(err.Error(), "must be ASCII") {
			t.Errorf("allowlist entry %q error = %v; want a must-be-ASCII startup error", host, err)
		}
		t.Setenv(AgentLLMEndpointAllowlistEnv, "https://api.ilk.example")
	}
}
