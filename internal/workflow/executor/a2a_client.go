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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/secretmount"
)

// testA2AHTTPClient overrides the HTTP client built by newA2AClient (tests only; nil in production).
var testA2AHTTPClient *http.Client

const (
	a2aDefaultTimeout         = 5 * time.Minute
	a2aDefaultPollInterval    = 3 * time.Second
	a2aAgentCardPath          = "/.well-known/agent-card.json"
	a2aMessageSendMethod      = "message/send"
	a2aTasksGetMethod         = "tasks/get"
	a2aKindMessage            = "message"
	a2aTaskStateCompleted     = "completed"
	a2aTaskStateFailed        = "failed"
	a2aTaskStateCanceled      = "canceled"
	a2aTaskStateRejected      = "rejected"
	a2aTaskStateInputRequired = "input-required"
	a2aTaskStateAuthRequired  = "auth-required"
	a2aWireVersionHeader      = "A2A-Version"
	a2aWireVersion            = "0.3"
)

// a2aJSONRPCRequest is a JSON-RPC 2.0 request envelope.
type a2aJSONRPCRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
	ID      int         `json:"id"`
}

// a2aJSONRPCResponse is a JSON-RPC 2.0 response envelope.
// Result is kept as raw JSON so we avoid deep-copying the full task graph.
type a2aJSONRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *a2aRPCError    `json:"error,omitempty"`
	ID      int             `json:"id"`
}

// a2aRPCError is the JSON-RPC error object.
type a2aRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// a2aSendResult unmarshals the discriminating fields of a message/send (or tasks/get) result:
// the kind ("task" or "message"), the server-assigned task id, and the task status.state. The
// full payload is preserved separately as raw JSON for CEL access.
type a2aSendResult struct {
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	Status struct {
		State string `json:"state"`
	} `json:"status"`
}

// a2aTaskResult is a shallow wrapper around the raw JSON task result.
// Deserialization into map[string]interface{} is deferred until a CEL expression needs it.
type a2aTaskResult struct {
	raw json.RawMessage
}

// ToMap converts the raw task result to map[string]interface{} for CEL access.
func (r *a2aTaskResult) ToMap() (map[string]interface{}, error) {
	var m map[string]interface{}
	if err := json.Unmarshal(r.raw, &m); err != nil {
		return nil, fmt.Errorf("decoding a2a task result: %w", err)
	}
	return m, nil
}

// a2aClient is a single-use HTTP client for one external A2A agent call.
type a2aClient struct {
	httpClient      *http.Client  // carries bearer token when auth is configured
	discoveryClient *http.Client  // bare transport, no auth — used for /.well-known/agent-card.json
	baseURL         string        // agent card base URL (no trailing slash)
	pollInterval    time.Duration // wait between tasks/get polls; 0 uses a2aDefaultPollInterval
}

// newA2AClient builds an a2aClient for the given step, resolving auth and CA secrets as needed.
//
// localExecutionMode distinguishes the CLI (true, has full API access) from the in-cluster
// workflow-runner Job (false): when it is false the CA bundle and bearer token Secrets are
// read from the files the controller mounted into the pod (internal/secretmount) and the
// Secret API is never called.
func newA2AClient(
	ctx context.Context,
	step *ottoflowv1alpha1.StepExternalAgentRef,
	k8sClient client.Client,
	namespace string,
	localExecutionMode bool,
) (*a2aClient, error) {
	parsedURL, err := ValidateExternalAgentTransport(step)
	if err != nil {
		return nil, err
	}

	// Test-mode shortcut: skip TLS/auth setup and use the injected client directly.
	if testA2AHTTPClient != nil {
		return &a2aClient{
			httpClient:      testA2AHTTPClient,
			discoveryClient: testA2AHTTPClient,
			baseURL:         strings.TrimRight(step.URL, "/"),
		}, nil
	}

	tlsCfg := &tls.Config{}

	// Load custom CA bundle if caSecretRef is provided
	if step.CASecretRef != nil {
		caNamespace := step.CASecretRef.Namespace
		if caNamespace == "" {
			caNamespace = namespace
		}
		caBundle, err := caBundleFromSecretRef(ctx, k8sClient, caNamespace, step.CASecretRef.Name, localExecutionMode)
		if err != nil {
			return nil, err
		}
		pool, _ := x509.SystemCertPool()
		if pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(caBundle) {
			return nil, fmt.Errorf("CA secret %s/%s: no valid PEM certificates found in ca.crt", caNamespace, step.CASecretRef.Name)
		}
		tlsCfg.RootCAs = pool
	}

	transport := &http.Transport{TLSClientConfig: tlsCfg}

	// discoveryClient uses only the TLS config — no bearer token.
	// The /.well-known/agent-card.json endpoint is a public discovery endpoint; sending
	// auth credentials there unnecessarily widens the blast radius of a token leak.
	discoveryClient := &http.Client{
		Transport: transport,
		// Refuse redirects on the discovery GET as well: an allowed http://*.svc endpoint
		// must not be able to redirect the agent-card probe to an arbitrary external or
		// metadata URL. Matches the JSON-RPC client's redirect policy below.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	var roundTripper http.RoundTripper = transport

	// Attach bearer token only over https. ValidateExternalAgentTransport already rejects
	// http+auth.secretRef, so this scheme gate is defense-in-depth: a bearer token must
	// never ride a cleartext transport.
	if parsedURL.Scheme == "https" && step.Auth != nil && step.Auth.SecretRef != nil {
		token, err := getSecretValue(ctx, step.Auth.SecretRef, namespace, k8sClient, localExecutionMode)
		if err != nil {
			return nil, fmt.Errorf("loading bearer token from secret: %w", err)
		}
		roundTripper = &bearerAuthTransport{
			transport: transport,
			token:     token,
		}
	}

	return &a2aClient{
		httpClient: &http.Client{
			Transport: roundTripper,
			// Refuse all redirects: bearerAuthTransport adds Authorization on every RoundTrip
			// call, including redirect-following calls. Go's default CheckRedirect strips the
			// header before calling the transport, but the transport re-adds it — defeating the
			// cross-host protection. JSON-RPC endpoints don't legitimately redirect.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
			// Timeout is 0 (unlimited): every request already carries a context deadline via
			// timeoutCtx in the caller. A non-zero http.Client.Timeout is per-request and
			// overrides context cancellation, which would cut off long-running steps.
		},
		discoveryClient: discoveryClient,
		baseURL:         strings.TrimRight(step.URL, "/"),
	}, nil
}

// fetchAgentCard probes /.well-known/agent-card.json as a discovery/liveness check. It uses
// discoveryClient (no bearer token) since agent card endpoints are public, and returns an error
// if the agent is unreachable or responds with a non-2xx status. The body is not decoded — this
// is a status-only probe that surfaces a clear "agent unreachable at URL" error before the
// message/send POST.
func (c *a2aClient) fetchAgentCard(ctx context.Context) error {
	cardURL := c.baseURL + a2aAgentCardPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cardURL, nil)
	if err != nil {
		return fmt.Errorf("building agent card request: %w", err)
	}
	resp, err := c.discoveryClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetching agent card from %s: %w", cardURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("agent card fetch returned %d from %s", resp.StatusCode, cardURL)
	}
	return nil
}

// sendTask sends a task to the external agent via message/send and waits for it to complete.
// An immediate message response or a terminal task returns directly; a non-terminal task is
// polled via tasks/get (using the server-assigned id) until it reaches a terminal state.
// ctx must already carry the step deadline (set by the caller via context.WithTimeout).
func (c *a2aClient) sendTask(
	ctx context.Context,
	prompt string,
	timeout time.Duration, // kept for callers that haven't set a deadline; ignored when ctx already has one
) (*a2aTaskResult, error) {
	// Only apply timeout when ctx has no deadline (e.g. tests calling sendTask directly).
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		if timeout <= 0 {
			timeout = a2aDefaultTimeout
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	messageID := uuid.New().String()

	// Always route to the configured base URL — never follow card.URL to a different host.
	// Trusting a discovered URL would allow a malicious agent card to redirect bearer tokens
	// to an attacker-controlled origin.
	agentEndpoint := c.baseURL

	rpcReq := a2aJSONRPCRequest{
		JSONRPC: "2.0",
		Method:  a2aMessageSendMethod,
		Params: map[string]interface{}{
			"message": map[string]interface{}{
				"kind": a2aKindMessage,
				"role": "user",
				"parts": []map[string]interface{}{
					{"kind": "text", "text": prompt},
				},
				"messageId": messageID,
			},
		},
		ID: 1,
	}

	rpcResp, err := c.doJSONRPC(ctx, agentEndpoint, rpcReq)
	if err != nil {
		return nil, fmt.Errorf("message/send: %w", err)
	}
	if len(rpcResp.Result) == 0 || string(rpcResp.Result) == "null" {
		return nil, fmt.Errorf("message/send: server returned no result")
	}

	var res a2aSendResult
	if err := json.Unmarshal(rpcResp.Result, &res); err != nil {
		return nil, fmt.Errorf("parsing message/send response: %w", err)
	}

	// A message result is terminal by definition — the agent answered inline without a task.
	// A missing kind ("") is treated as a Task for backward compatibility with pre-0.3 agents.
	if res.Kind == a2aKindMessage {
		// Mirror the message's parts into an artifact so parts-based CEL
		// (a2aResult.artifacts[0].parts[0].text) resolves for both Task and Message
		// replies. Only parts are mirrored — a message has no artifactId or name.
		var msg map[string]interface{}
		if err := json.Unmarshal(rpcResp.Result, &msg); err != nil {
			return nil, fmt.Errorf("parsing message/send message response: %w", err)
		}
		if parts, ok := msg["parts"]; ok {
			msg["artifacts"] = []map[string]interface{}{{"parts": parts}}
		}
		normalized, err := json.Marshal(msg)
		if err != nil {
			return nil, fmt.Errorf("normalizing message/send response: %w", err)
		}
		return &a2aTaskResult{raw: normalized}, nil
	}

	result, terminal, err := resolveTaskState(res.ID, res.Status.State, rpcResp.Result)
	if err != nil {
		return nil, err
	}
	if terminal {
		return result, nil
	}
	if res.ID == "" {
		return nil, fmt.Errorf("message/send: non-terminal task has no server-assigned id")
	}

	// Task is still running — poll until terminal state or timeout, using the server id.
	return c.pollTask(ctx, agentEndpoint, res.ID)
}

// pollTask polls tasks/get until the task reaches a terminal state. taskID is the
// server-assigned id returned by message/send.
func (c *a2aClient) pollTask(ctx context.Context, agentEndpoint, taskID string) (*a2aTaskResult, error) {
	interval := c.pollInterval
	if interval <= 0 {
		interval = a2aDefaultPollInterval
	}
	for {
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("timed out waiting for external agent task %s: %w", taskID, ctx.Err())
		case <-timer.C:
		}

		rpcReq := a2aJSONRPCRequest{
			JSONRPC: "2.0",
			Method:  a2aTasksGetMethod,
			Params:  map[string]interface{}{"id": taskID},
			ID:      1,
		}
		rpcResp, err := c.doJSONRPC(ctx, agentEndpoint, rpcReq)
		if err != nil {
			return nil, fmt.Errorf("tasks/get: %w", err)
		}
		if len(rpcResp.Result) == 0 || string(rpcResp.Result) == "null" {
			return nil, fmt.Errorf("tasks/get: server returned no result for task %s", taskID)
		}

		var res a2aSendResult
		if err := json.Unmarshal(rpcResp.Result, &res); err != nil {
			return nil, fmt.Errorf("parsing tasks/get response: %w", err)
		}

		result, terminal, err := resolveTaskState(taskID, res.Status.State, rpcResp.Result)
		if err != nil {
			return nil, err
		}
		if terminal {
			return result, nil
		}
		// not terminal → continue the poll loop
	}
}

// doJSONRPC sends a JSON-RPC request and returns the response.
func (c *a2aClient) doJSONRPC(ctx context.Context, endpoint string, req a2aJSONRPCRequest) (*a2aJSONRPCResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshaling JSON-RPC request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("building JSON-RPC request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set(a2aWireVersionHeader, a2aWireVersion)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("JSON-RPC POST %s returned %d: %s", endpoint, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var rpcResp a2aJSONRPCResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		return nil, fmt.Errorf("decoding JSON-RPC response from %s: %w", endpoint, err)
	}
	if rpcResp.Error != nil {
		return nil, fmt.Errorf("JSON-RPC error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}
	return &rpcResp, nil
}

// resolveTaskState maps a task's state to an outcome. terminal==false means the
// task is still in progress and the caller should keep polling.
func resolveTaskState(taskID, state string, raw json.RawMessage) (result *a2aTaskResult, terminal bool, err error) {
	switch {
	case isPauseState(state):
		return nil, true, fmt.Errorf("external agent task %s requires interaction (%q); OttoFlow external agents run non-interactively", taskID, state)
	case !isTerminalState(state):
		return nil, false, nil
	case isFailureState(state):
		return nil, true, fmt.Errorf("external agent task %s ended with state %q", taskID, state)
	default:
		return &a2aTaskResult{raw: raw}, true, nil
	}
}

// isTerminalState reports whether a task state string indicates the task has finished.
func isTerminalState(state string) bool {
	return state == a2aTaskStateCompleted || isFailureState(state)
}

// isFailureState reports whether a terminal task state indicates the task did not succeed.
func isFailureState(s string) bool {
	return s == a2aTaskStateFailed || s == a2aTaskStateCanceled || s == a2aTaskStateRejected
}

// isPauseState reports whether a task state indicates the agent is waiting for interaction.
// OttoFlow runs external agents non-interactively, so these states are treated as errors.
func isPauseState(s string) bool {
	return s == a2aTaskStateInputRequired || s == a2aTaskStateAuthRequired
}

// caBundleFromSecretRef returns the "ca.crt" bytes for a CASecretRef. When localExecutionMode
// is true (the CLI), it reads the live Secret via the API. When false (the in-cluster
// workflow-runner Job), it reads the controller-mounted file via internal/secretmount instead,
// and never touches the Secret API.
func caBundleFromSecretRef(ctx context.Context, k8sClient client.Client, caNamespace, caName string, localExecutionMode bool) ([]byte, error) {
	caBundle, err := secretmount.Resolve(ctx, k8sClient, localExecutionMode, caNamespace, caName, "ca.crt")
	if err != nil {
		return nil, fmt.Errorf("loading CA secret %s/%s: %w", caNamespace, caName, err)
	}
	return caBundle, nil
}

// getSecretValue retrieves a single key value for a bearer token. When localExecutionMode is
// true (the CLI), it reads the live Secret via the API. When false (the in-cluster
// workflow-runner Job), it reads the controller-mounted file via internal/secretmount instead,
// and never touches the Secret API.
func getSecretValue(ctx context.Context, ref *ottoflowv1alpha1.SecretReference, namespace string, k8sClient client.Client, localExecutionMode bool) (string, error) {
	secretNamespace := ref.Namespace
	if secretNamespace == "" {
		secretNamespace = namespace
	}

	value, err := secretmount.Resolve(ctx, k8sClient, localExecutionMode, secretNamespace, ref.Name, ref.Key)
	if err != nil {
		return "", fmt.Errorf("getting secret %s/%s: %w", secretNamespace, ref.Name, err)
	}
	return strings.TrimSpace(string(value)), nil
}

// bearerAuthTransport adds Authorization: Bearer <token> to every request.
type bearerAuthTransport struct {
	transport http.RoundTripper
	token     string
}

func (t *bearerAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r2 := req.Clone(req.Context())
	r2.Header.Set("Authorization", "Bearer "+t.token)
	return t.transport.RoundTrip(r2)
}
