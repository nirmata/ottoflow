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

	"github.com/GoogleCloudPlatform/kubectl-ai/gollm"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

// These tests use modelProvider "azure-openai" exclusively: with OSS's unforked gollm pin,
// azopenai.go is the ONLY client that reads ClientOptions.URL (azopenai.go:62-64) -- the
// anthropic and openai clients never consult it at all. So azure-openai is the one provider
// where a wire-level assertion on "did spec.config.endpoint actually change the destination"
// is meaningful here; the allowlist/refusal logic itself is exercised before any provider
// dispatch and applies identically to every provider (see default_executor.go).

// capturedRequest records what a fake LLM endpoint actually received, so these tests assert
// on the wire, not on a mock a few layers above it.
type capturedRequest struct {
	mu      sync.Mutex
	hits    int
	apiKey  string // azure-openai: api-key
	authHdr string // set only if userinfo leaked into an Authorization header
	body    string
}

func (c *capturedRequest) snapshot() (hits int, apiKey, authHdr, body string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, c.apiKey, c.authHdr, c.body
}

// newAzureCaptureServer fakes an Azure OpenAI endpoint. azopenai.go forces the scheme to
// https (azopenai.go:62-64), so this must be a TLS server; the test trusts it via the
// operator-side LLM_SKIP_VERIFY_SSL fallback, never via Agent.spec.config.skipVerifySSL,
// which stays rejected for every provider.
func newAzureCaptureServer(t *testing.T, c *capturedRequest) *httptest.Server {
	t.Helper()
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.hits++
		c.apiKey = r.Header.Get("api-key")
		c.authHdr = r.Header.Get("Authorization")
		c.body = string(b)
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
}

func mkEndpointAgent(endpoint string) *ottoflowv1alpha1.Agent {
	config := map[string]string{}
	if endpoint != "" {
		config["endpoint"] = endpoint
	}
	return &ottoflowv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "tenant-agent", Namespace: "tenant-a"},
		Spec:       ottoflowv1alpha1.AgentSpec{ModelProvider: providerAzureOpenAI, Config: config},
	}
}

// TestLLMEndpointCredentialTravel_AllowlistUnset_ZeroHits is the negative case of the
// credential-travel proof: with no allowlist, createLLMClient must refuse before any gollm
// client exists, so the capture server sees nothing at all -- not "sees a request that then
// fails," but never dialed.
func TestLLMEndpointCredentialTravel_AllowlistUnset_ZeroHits(t *testing.T) {
	c := &capturedRequest{}
	srv := newAzureCaptureServer(t, c)
	defer srv.Close()

	t.Setenv("AZURE_OPENAI_API_KEY", "azure-OPERATOR-SHARED-SECRET")
	t.Setenv("LLM_SKIP_VERIFY_SSL", "true") // trust the self-signed TLS test server
	// AGENT_LLM_ENDPOINT_ALLOWLIST intentionally left unset.

	e := &DefaultAgentExecutor{}
	client, err := e.createLLMClient(context.Background(), mkEndpointAgent(srv.URL))
	if err == nil {
		t.Fatalf("createLLMClient: expected refusal with no allowlist, got client=%v err=nil", client)
	}
	if hits, _, _, _ := c.snapshot(); hits != 0 {
		t.Fatalf("capture server recorded %d hits; want 0 -- the operator's credential must never be "+
			"dialed toward an unapproved origin", hits)
	}
}

// TestLLMEndpointCredentialTravel_Allowlisted_OneHitWithSentinel is the positive case: once
// the operator allowlists the capture server's origin, the run proceeds, the operator's
// AZURE_OPENAI_API_KEY sentinel arrives at that origin, and the tenant's prompt travels with
// it (proving this is not a vacuous "the client was constructed" assertion).
func TestLLMEndpointCredentialTravel_Allowlisted_OneHitWithSentinel(t *testing.T) {
	c := &capturedRequest{}
	srv := newAzureCaptureServer(t, c)
	defer srv.Close()

	const sentinel = "azure-OPERATOR-SHARED-SECRET"
	t.Setenv("AZURE_OPENAI_API_KEY", sentinel)
	t.Setenv("LLM_SKIP_VERIFY_SSL", "true")
	t.Setenv(AgentLLMEndpointAllowlistEnv, srv.URL)

	e := &DefaultAgentExecutor{}
	client, err := e.createLLMClient(context.Background(), mkEndpointAgent(srv.URL))
	if err != nil {
		t.Fatalf("createLLMClient: unexpected refusal of an allowlisted origin: %v", err)
	}
	const tenantPrompt = "TENANT-VISIBLE-PROMPT-CONTENT"
	if _, err := client.GenerateCompletion(context.Background(), &gollm.CompletionRequest{
		Model: "gpt-4o", Prompt: tenantPrompt,
	}); err != nil {
		t.Fatalf("GenerateCompletion: %v", err)
	}

	hits, apiKey, authHdr, body := c.snapshot()
	if hits != 1 {
		t.Fatalf("capture server recorded %d hits; want exactly 1", hits)
	}
	if apiKey != sentinel {
		t.Fatalf("api-key = %q; want operator sentinel %q", apiKey, sentinel)
	}
	if authHdr != "" {
		t.Errorf("Authorization header = %q; want empty -- no userinfo was set on the tenant endpoint, "+
			"so none should have been converted into a Basic-auth header", authHdr)
	}
	if !strings.Contains(body, tenantPrompt) {
		t.Fatalf("request body does not contain the tenant prompt; got: %s", body)
	}
}

// TestLLMEndpointCredentialTravel_NotAllowlisted_RefusedBeforeClientExists proves that even
// with a safe operator-pinned AZURE_OPENAI_ENDPOINT already set, an Agent naming a
// non-allowlisted endpoint is refused before any client -- pointed at either server -- is
// ever constructed.
func TestLLMEndpointCredentialTravel_NotAllowlisted_RefusedBeforeClientExists(t *testing.T) {
	operatorCapture := &capturedRequest{}
	operatorSrv := newAzureCaptureServer(t, operatorCapture)
	defer operatorSrv.Close()

	attackerCapture := &capturedRequest{}
	attackerSrv := newAzureCaptureServer(t, attackerCapture)
	defer attackerSrv.Close()

	t.Setenv("AZURE_OPENAI_API_KEY", "azure-OPERATOR-SHARED-SECRET")
	t.Setenv("AZURE_OPENAI_ENDPOINT", operatorSrv.URL)
	t.Setenv("LLM_SKIP_VERIFY_SSL", "true")
	// AGENT_LLM_ENDPOINT_ALLOWLIST intentionally left unset: the attacker server is not listed.

	e := &DefaultAgentExecutor{}
	client, err := e.createLLMClient(context.Background(), mkEndpointAgent(attackerSrv.URL))
	if err == nil {
		t.Fatalf("createLLMClient: expected refusal of a non-allowlisted endpoint, got client=%v err=nil", client)
	}
	if hits, _, _, _ := attackerCapture.snapshot(); hits != 0 {
		t.Errorf("attacker server hits = %d; want 0", hits)
	}
	if hits, _, _, _ := operatorCapture.snapshot(); hits != 0 {
		t.Errorf("operator server hits = %d; want 0 (no client was ever constructed)", hits)
	}
}

// TestLLMEndpointCredentialTravel_Userinfo_Refused_ZeroHits is the regression for userinfo
// becoming an Authorization: Basic header on the request carrying the operator's credential.
func TestLLMEndpointCredentialTravel_Userinfo_Refused_ZeroHits(t *testing.T) {
	c := &capturedRequest{}
	srv := newAzureCaptureServer(t, c)
	defer srv.Close()

	t.Setenv("AZURE_OPENAI_API_KEY", "azure-OPERATOR-SHARED-SECRET")
	t.Setenv("LLM_SKIP_VERIFY_SSL", "true")
	t.Setenv(AgentLLMEndpointAllowlistEnv, srv.URL) // operator allowlists the bare origin

	withUserinfo := strings.Replace(srv.URL, "https://", "https://alice:s3cret@", 1)
	e := &DefaultAgentExecutor{}
	client, err := e.createLLMClient(context.Background(), mkEndpointAgent(withUserinfo))
	if err == nil {
		t.Fatalf("createLLMClient: expected refusal of userinfo, got client=%v err=nil", client)
	}
	if !strings.Contains(err.Error(), "userinfo") {
		t.Errorf("error = %v; want it to mention userinfo", err)
	}
	if hits, _, _, _ := c.snapshot(); hits != 0 {
		t.Fatalf("capture server recorded %d hits; want 0", hits)
	}
}

// TestLLMEndpointCredentialTravel_TenantURLComponent_Refused_ZeroHits is the regression for an
// allowlisted origin that still lets a tenant-supplied path, query, or fragment through: on a
// multiplexing gateway that would select a different backend while carrying the operator's
// credential.
func TestLLMEndpointCredentialTravel_TenantURLComponent_Refused_ZeroHits(t *testing.T) {
	for _, tc := range []struct {
		name   string
		suffix string
	}{
		{"Path", "/other-backend"},
		{"Query", "?route=backend-b"},
		{"Fragment", "#backend-b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &capturedRequest{}
			srv := newAzureCaptureServer(t, c)
			defer srv.Close()

			t.Setenv("AZURE_OPENAI_API_KEY", "azure-OPERATOR-SHARED-SECRET")
			t.Setenv("LLM_SKIP_VERIFY_SSL", "true")
			t.Setenv(AgentLLMEndpointAllowlistEnv, srv.URL) // operator allowlists the origin only

			e := &DefaultAgentExecutor{}
			client, err := e.createLLMClient(context.Background(), mkEndpointAgent(srv.URL+tc.suffix))
			if err == nil {
				t.Fatalf("createLLMClient: expected refusal of a tenant-supplied %s, got client=%v err=nil",
					strings.ToLower(tc.name), client)
			}
			if hits, _, _, _ := c.snapshot(); hits != 0 {
				t.Fatalf("capture server recorded %d hits; want 0", hits)
			}
		})
	}
}

// TestLLMEndpointCredentialTravel_TenantRootPath_Allowed_OneHit proves the path rejection is
// not overbroad: a tenant endpoint whose path is exactly "/" (what url.Parse leaves on a bare
// origin plus trailing slash) must still be accepted, same as the clean-origin case.
func TestLLMEndpointCredentialTravel_TenantRootPath_Allowed_OneHit(t *testing.T) {
	c := &capturedRequest{}
	srv := newAzureCaptureServer(t, c)
	defer srv.Close()

	const sentinel = "azure-OPERATOR-SHARED-SECRET"
	t.Setenv("AZURE_OPENAI_API_KEY", sentinel)
	t.Setenv("LLM_SKIP_VERIFY_SSL", "true")
	t.Setenv(AgentLLMEndpointAllowlistEnv, srv.URL)

	e := &DefaultAgentExecutor{}
	client, err := e.createLLMClient(context.Background(), mkEndpointAgent(srv.URL+"/"))
	if err != nil {
		t.Fatalf("createLLMClient: unexpected refusal of a root-path (\"/\") endpoint: %v", err)
	}
	if _, err := client.GenerateCompletion(context.Background(), &gollm.CompletionRequest{Model: "gpt-4o", Prompt: "p"}); err != nil {
		t.Fatalf("GenerateCompletion: %v", err)
	}

	hits, apiKey, _, _ := c.snapshot()
	if hits != 1 {
		t.Fatalf("capture server recorded %d hits; want exactly 1", hits)
	}
	if apiKey != sentinel {
		t.Fatalf("api-key = %q; want operator sentinel %q", apiKey, sentinel)
	}
}

// TestLLMEndpointCredentialTravel_OperatorEntryWithPathQueryFragmentUserinfo_FailsAtBoot is the
// fail-fast regression: an allowlist entry carrying a path, query, fragment, or userinfo reads
// like a credential boundary and is not one, so each must be rejected at process startup, naming
// the reason, not admitted and then silently treated as origin-only.
func TestLLMEndpointCredentialTravel_OperatorEntryWithPathQueryFragmentUserinfo_FailsAtBoot(t *testing.T) {
	for _, entry := range []string{
		"https://llm.corp.example/teams/platform",
		"https://llm.corp.example?x=1",
		"https://llm.corp.example#f",
		"https://u:p@llm.corp.example",
	} {
		t.Setenv(AgentLLMEndpointAllowlistEnv, entry)
		err := ValidateLLMEndpointAllowlist()
		if err == nil {
			t.Errorf("entry %q: expected a boot-time failure", entry)
			continue
		}
		if !strings.Contains(err.Error(), "origin only") {
			t.Errorf("entry %q: error = %v; want it to mention \"origin only\"", entry, err)
		}
	}
}
