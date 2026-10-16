/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/agent"
)

const (
	agentExecutorCAPath = "/etc/ottoflow/agent-executor-ca/ca.crt"
	// defaultServiceAccountTokenPath is the in-cluster path for the ServiceAccount token.
	defaultServiceAccountTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	// defaultExecHTTPTimeout limits any single HTTP operation for exec endpoint calls.
	defaultExecHTTPTimeout = 30 * time.Minute

	// execHTTPMaxRetries is the number of attempts for transient HTTP failures.
	execHTTPMaxRetries = 3
)

// execHTTPRetryBaseWait is the base backoff duration between retry attempts (doubles each retry).
// Declared as a var so tests can override it to avoid real sleeps.
var execHTTPRetryBaseWait = time.Second

// XLLMEnvHeader is the HTTP header for the LLM env map (base64-encoded JSON). Used by runner and agent-executor.
const XLLMEnvHeader = "X-LLM-Env"

// MetadataKeyLLMEnv is the key for the LLM env map in request metadata.
const MetadataKeyLLMEnv = "llmEnv"

// testExecURLBase overrides the exec endpoint base URL for tests (empty in production).
var testExecURLBase string

// LLMEnvAllowlist is the list of env var names the runner forwards to the agent-executor
// via the X-LLM-Env header (credentials and LLM configuration). Only these keys are sent.
// Also used by the controller to filter keys from the well-known LLM credentials Secret.
var LLMEnvAllowlist = []string{
	"NIRMATA_LLM_TOKEN", "NIRMATA_LLM_SERVICEACCOUNT_TOKEN", "NIRMATA_LLM_APIKEY", "NIRMATA_URL",
	"NIRMATA_LLM_MODEL",
}

// OttoFlowAgentExecutor implements the server-side agent executor for the exec HTTP endpoint.
// It wraps an agent.AgentExecutor and provides HTTP handler support for the agent-executor service.
type OttoFlowAgentExecutor struct {
	k8sClient     client.Client
	agentExecutor agent.AgentExecutor
}

// NewOttoFlowAgentExecutor creates a new OttoFlowAgentExecutor.
// If mcpProvider is non-nil, agents with Spec.MCPTools will get those tools registered for LLM execution.
func NewOttoFlowAgentExecutor(k8sClient client.Client, mcpProvider agent.MCPClientProvider) *OttoFlowAgentExecutor {
	agentExec := agent.NewRoutingAgentExecutor(mcpProvider)
	return &OttoFlowAgentExecutor{
		k8sClient:     k8sClient,
		agentExecutor: agentExec,
	}
}

// NewOttoFlowAgentExecutorWithAgentExecutor creates an executor that uses the given AgentExecutor (for tests).
func NewOttoFlowAgentExecutorWithAgentExecutor(k8sClient client.Client, agentExecutor agent.AgentExecutor) *OttoFlowAgentExecutor {
	return &OttoFlowAgentExecutor{k8sClient: k8sClient, agentExecutor: agentExecutor}
}

// executeAgentViaExecHTTP executes an agent via the lightweight exec HTTP endpoint.
// When the workflow executor's agentExecutor is a MockAgentExecutor (for tests), it calls
// the executor directly without HTTP. Otherwise it POSTs to the exec endpoint.
func (e *WorkflowExecutor) executeAgentViaExecHTTP(
	ctx context.Context,
	workflowRun *ottoflowv1alpha1.WorkflowRun,
	agentCRD *ottoflowv1alpha1.Agent,
	agentRef *ottoflowv1alpha1.StepAgentRef,
	prompt string,
	contextData map[string]interface{},
	serviceName string,
	serviceNamespace string,
) (string, agent.AgentTokenUsage, map[string]interface{}, error) {
	// Resolve namespace first so both the mock and real paths use the same value.
	agentNamespace := agentRef.Namespace
	if agentNamespace == "" {
		agentNamespace = workflowRun.Namespace
	}

	// When the agentExecutor is a MockAgentExecutor, call it directly without HTTP (for tests).
	if mock, ok := e.agentExecutor.(*agent.MockAgentExecutor); ok {
		response, tokenUsage, err := mock.ExecuteAgent(ctx, agentCRD, prompt, contextData, agentNamespace)
		if err != nil {
			return "", agent.AgentTokenUsage{}, nil, err
		}
		return response, tokenUsage, nil, nil
	}

	// Determine URL for the exec endpoint.
	url := buildExecURL(testExecURLBase, serviceName, serviceNamespace, agentNamespace, agentCRD.Name)

	reqBody := ExecRequest{
		Prompt: prompt,
		// Context is intentionally omitted: ExecuteAgent never reads workflowContext,
		// and for large clusters the full context (all pods/nodes/deployments) can
		// exceed the 32 MiB maxBodyBytes limit on the exec endpoint.
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", agent.AgentTokenUsage{}, nil, fmt.Errorf("marshaling exec request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", agent.AgentTokenUsage{}, nil, fmt.Errorf("creating exec request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	// Attach X-LLM-Env header with allowlisted env vars for credential forwarding.
	envMap := buildLLMEnvMap(LLMEnvAllowlist)
	if len(envMap) > 0 {
		data, err := json.Marshal(envMap)
		if err != nil {
			return "", agent.AgentTokenUsage{}, nil, fmt.Errorf("marshaling LLM env map: %w", err)
		}
		httpReq.Header.Set(XLLMEnvHeader, base64.StdEncoding.EncodeToString(data))
	}

	httpClient, err := e.getOrCreateExecHTTPClient()
	if err != nil {
		return "", agent.AgentTokenUsage{}, nil, fmt.Errorf("creating exec HTTP client: %w", err)
	}

	var resp *http.Response
	var lastErr error
	for attempt := 0; attempt < execHTTPMaxRetries; attempt++ {
		if attempt > 0 {
			// Exponential backoff: 1s then 2s for 3 attempts.
			wait := execHTTPRetryBaseWait * time.Duration(1<<uint(attempt-1))
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return "", agent.AgentTokenUsage{}, nil, fmt.Errorf("POST %s: %w", url, ctx.Err())
			}
			// GetBody is set automatically by http.NewRequestWithContext for *bytes.Reader
			// bodies. It returns a fresh reader at position 0 without copying the data.
			if httpReq.GetBody != nil {
				httpReq.Body, err = httpReq.GetBody()
				if err != nil {
					return "", agent.AgentTokenUsage{}, nil, fmt.Errorf("resetting exec request body for retry: %w", err)
				}
			}
		}

		resp, lastErr = httpClient.Do(httpReq)
		if lastErr != nil {
			// http.Client.Do can return a non-nil resp alongside a non-nil error
			// (e.g. redirect policy failure). Drain and close it to return the
			// connection to the pool before retrying or returning.
			if resp != nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
			if !isRetryableExecErr(lastErr) {
				break
			}
			klog.V(2).InfoS("Agent exec call failed, retrying",
				"attempt", attempt+1, "of", execHTTPMaxRetries, "err", lastErr)
			continue
		}

		if resp.StatusCode < 500 {
			break // 2xx success or 4xx client error — do not retry either
		}

		// Intermediate attempts drain and close the body before retrying so that
		// the underlying TCP connection is returned to the pool (Go's http.Transport
		// only reuses a connection when the previous response body is fully consumed).
		// On the last attempt we skip draining and break instead, letting the body
		// fall through to the status-check below which decodes the structured JSON
		// error message (e.g. {"error": "agent crashed"}) from the response body.
		if attempt == execHTTPMaxRetries-1 {
			// lastErr is nil here (Do succeeded); the 5xx error is surfaced by the
			// resp.StatusCode check after the loop, which also decodes the structured
			// JSON error body (e.g. {"error": "agent crashed"}).
			break
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		lastErr = fmt.Errorf("exec endpoint returned %d", resp.StatusCode)
		klog.V(2).InfoS("Agent exec call returned server error, retrying",
			"attempt", attempt+1, "of", execHTTPMaxRetries, "status", resp.StatusCode)
		resp = nil
	}

	if lastErr != nil {
		return "", agent.AgentTokenUsage{}, nil, fmt.Errorf("POST %s: %w", url, lastErr)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		var errResp ExecErrorResponse
		if decErr := json.NewDecoder(resp.Body).Decode(&errResp); decErr == nil && errResp.Error != "" {
			return "", agent.AgentTokenUsage{}, nil, fmt.Errorf("exec endpoint returned %d: %s", resp.StatusCode, errResp.Error)
		}
		return "", agent.AgentTokenUsage{}, nil, fmt.Errorf("exec endpoint returned %d", resp.StatusCode)
	}

	var execResp ExecResponse
	if err := json.NewDecoder(resp.Body).Decode(&execResp); err != nil {
		return "", agent.AgentTokenUsage{}, nil, fmt.Errorf("decoding exec response: %w", err)
	}

	klog.V(3).InfoS("Agent executed via exec HTTP", "agent", agentCRD.Name, "namespace", agentNamespace)
	return execResp.Content, agent.AgentTokenUsage{InputTokens: execResp.InputTokens, OutputTokens: execResp.OutputTokens}, execResp.ExtractedOutputs, nil
}

// buildExecURL returns the URL for the exec endpoint.
// urlBase overrides the exec endpoint URL prefix (used in tests); it may already include a path
// prefix such as /api/exec, and this function appends only /{agentNamespace}/{agentName}.
// When urlBase is empty, the production in-cluster exec endpoint URL is used.
// The production URL uses the short-form DNS name ({service}.{namespace}.svc:8443) which matches
// the TLS certificate SANs generated by kyverno/pkg/tls (which stops at .svc, not .cluster.local).
func buildExecURL(urlBase, serviceName, serviceNamespace, agentNamespace, agentName string) string {
	if urlBase != "" {
		return fmt.Sprintf("%s/%s/%s", urlBase, agentNamespace, agentName)
	}
	return fmt.Sprintf("https://%s.%s.svc:8443/api/exec/%s/%s",
		serviceName, serviceNamespace, agentNamespace, agentName)
}

// getOrCreateExecHTTPClient returns the cached exec HTTP client, creating it on first call.
// Uses sync.Once for safe concurrent initialization (forEach steps run in parallel goroutines).
func (e *WorkflowExecutor) getOrCreateExecHTTPClient() (*http.Client, error) {
	e.execHTTPClientOnce.Do(func() {
		e.execHTTPClient, e.execHTTPClientErr = createExecHTTPClient()
	})
	return e.execHTTPClient, e.execHTTPClientErr
}

// createExecHTTPClient returns an HTTP client for calling the exec endpoint.
// It loads the agent-executor CA cert (if mounted) and attaches the ServiceAccount token.
func createExecHTTPClient() (*http.Client, error) {
	tlsCfg := &tls.Config{}

	// Load agent-executor CA cert if available (self-signed cert generated by the controller)
	if caCert, err := os.ReadFile(agentExecutorCAPath); err == nil {
		pool, _ := x509.SystemCertPool()
		if pool == nil {
			pool = x509.NewCertPool()
		}
		pool.AppendCertsFromPEM(caCert)
		tlsCfg.RootCAs = pool
	}

	// Read ServiceAccount token (path overridable for tests)
	tokenPath := defaultServiceAccountTokenPath
	token, err := os.ReadFile(tokenPath)
	if err != nil {
		if os.IsNotExist(err) {
			klog.V(2).InfoS("Service account token not found, using unauthenticated client for local development")
			return &http.Client{
				Transport: &http.Transport{TLSClientConfig: tlsCfg},
				Timeout:   defaultExecHTTPTimeout,
			}, nil
		}
		return nil, fmt.Errorf("failed to read service account token: %w", err)
	}

	return &http.Client{
		Transport: &saAuthTransport{
			transport: &http.Transport{TLSClientConfig: tlsCfg},
			token:     strings.TrimSpace(string(token)),
		},
		Timeout: defaultExecHTTPTimeout,
	}, nil
}

// saAuthTransport adds Authorization: Bearer header to requests.
type saAuthTransport struct {
	transport http.RoundTripper
	token     string
}

func (t *saAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone before mutating to satisfy the net/http RoundTripper contract.
	r2 := req.Clone(req.Context())
	r2.Header.Set("Authorization", fmt.Sprintf("Bearer %s", t.token))
	return t.transport.RoundTrip(r2)
}

// buildLLMEnvMap returns a map of allowlisted env var names to values (only keys that are set).
func buildLLMEnvMap(allowlist []string) map[string]string {
	out := make(map[string]string)
	for _, key := range allowlist {
		if v := os.Getenv(key); v != "" {
			out[key] = v
		}
	}
	return out
}

// isRetryableExecErr reports whether err is a transient network failure safe to retry.
// Context cancellation and deadline exceeded are never retryable — they must propagate.
func isRetryableExecErr(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var netErr *net.OpError
	return errors.As(err, &netErr) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}
