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
	"strconv"
	"strings"
	"time"

	kyvernohttp "github.com/kyverno/sdk/extensions/cel/libs/http"
)

// celHTTPTimeout limits CEL http.Post requests so a stalled endpoint cannot
// block workflow execution indefinitely.
const celHTTPTimeout = 30 * time.Second

// jsonNormalizingHTTPClient wraps http.Client and normalizes successful (2xx) responses
// whose body is not valid JSON into {"ok":true,"body":"<original>"}. Without this wrapper,
// the Kyverno SDK's http context silently decodes a non-JSON body as nil rather than
// erroring (see contextImpl.executeRequest), so a plain-text 2xx response like a
// Slack webhook's literal "ok" would surface to CEL as {"body":null,"statusCode":200} with
// the actual response text discarded. This wrapper preserves that text instead, so it
// surfaces as {"ok":true,"body":"ok","statusCode":200}. JSON responses pass through unchanged.
type jsonNormalizingHTTPClient struct {
	client *http.Client
}

func (c *jsonNormalizingHTTPClient) Do(req *http.Request) (*http.Response, error) {
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp, nil
	}
	bodyBytes, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}

	if json.Valid(bodyBytes) && len(bytes.TrimSpace(bodyBytes)) > 0 {
		resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		return resp, nil
	}

	// Non-JSON 2xx: wrap original body so CEL expressions can still inspect it.
	bodyStr := strings.TrimSpace(string(bodyBytes))
	wrapped := map[string]any{"ok": true, "body": bodyStr}
	syntheticBody, _ := json.Marshal(wrapped)
	resp.Body = io.NopCloser(bytes.NewReader(syntheticBody))
	if resp.Header == nil {
		resp.Header = make(http.Header)
	}
	resp.Header.Set("Content-Type", "application/json")
	resp.Header.Set("Content-Length", strconv.Itoa(len(syntheticBody)))
	resp.ContentLength = int64(len(syntheticBody))
	return resp, nil
}

// celHTTPContext implements kyvernohttp.ContextInterface directly rather than delegating to
// kyvernohttp.NewHTTP, for two reasons: NewHTTP() takes no arguments, so the SDK exposes no
// way to inject a client at all; and every context derived from this one — including those
// returned by Client() — must retain celHTTPTimeout and jsonNormalizingHTTPClient's body
// normalization. The client field is deliberately the concrete wrapper type rather than a
// Do(*http.Request) interface, so normalization cannot be dropped by assigning a bare
// *http.Client.
type celHTTPContext struct {
	client *jsonNormalizingHTTPClient
}

// NewCELHTTPContext returns a Kyverno HTTP context with a timeout and non-JSON
// body normalization. Used as the global CEL "http" variable so http.Post works
// with any endpoint (JSON or not). JSON responses pass through; non-JSON 2xx
// responses are wrapped as {"ok":true,"body":"<original>"}.
func NewCELHTTPContext() kyvernohttp.ContextInterface {
	return newCELHTTPContext(nil)
}

// newCELHTTPContext builds a context whose client always carries celHTTPTimeout and the
// body-normalizing wrapper. A nil transport uses http.DefaultTransport.
func newCELHTTPContext(transport http.RoundTripper) *celHTTPContext {
	return &celHTTPContext{
		client: &jsonNormalizingHTTPClient{
			client: &http.Client{Transport: transport, Timeout: celHTTPTimeout},
		},
	}
}

func (c *celHTTPContext) Get(url string, headers map[string]string) (any, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	for h, v := range headers {
		req.Header.Add(h, v)
	}
	return c.executeRequest(req)
}

func (c *celHTTPContext) Post(url string, data any, headers map[string]string) (any, error) {
	buf := new(bytes.Buffer)
	if err := json.NewEncoder(buf).Encode(data); err != nil {
		return nil, fmt.Errorf("failed to encode HTTP POST data (%T): %w", data, err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, buf)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	for h, v := range headers {
		req.Header.Add(h, v)
	}
	return c.executeRequest(req)
}

// executeRequest mirrors the Kyverno SDK's response handling (contextImpl.executeRequest
// in extensions/cel/libs/http): a non-JSON body decodes to nil rather than erroring, and
// a JSON object body gets "statusCode" injected directly rather than nested under "body".
func (c *celHTTPContext) executeRequest(req *http.Request) (any, error) {
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var body any
	if resp.Body != nil {
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			body = nil
		}
	}

	if bodyMap, ok := body.(map[string]any); ok {
		bodyMap["statusCode"] = resp.StatusCode
		return bodyMap, nil
	}

	return map[string]any{
		"body":       body,
		"statusCode": resp.StatusCode,
	}, nil
}

// Client returns a derived context that trusts the supplied PEM CA bundle. The TLS setup
// mirrors the Kyverno SDK (contextImpl.Client in extensions/cel/libs/http), but unlike
// the SDK — which hands back a bare *http.Client with no timeout and no body
// normalization — the derived client keeps celHTTPTimeout and the jsonNormalizingHTTPClient
// wrapper, so a CA-pinned endpoint behaves consistently with the default context.
// An empty caBundle reuses the receiver; a non-empty bundle that fails to
// parse returns an error rather than silently falling back to the system trust store.
// Results are wrapped in kyvernohttp.Context: CEL cannot convert a bare *celHTTPContext.
func (c *celHTTPContext) Client(caBundle string) (kyvernohttp.ContextInterface, error) {
	if caBundle == "" {
		return kyvernohttp.Context{ContextInterface: c}, nil
	}
	caCertPool := x509.NewCertPool()
	if ok := caCertPool.AppendCertsFromPEM([]byte(caBundle)); !ok {
		return nil, fmt.Errorf("failed to parse PEM CA bundle for APICall")
	}
	baseTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok || baseTransport == nil {
		baseTransport = &http.Transport{}
	}
	transport := baseTransport.Clone()
	if transport.TLSClientConfig != nil {
		transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	} else {
		transport.TLSClientConfig = &tls.Config{}
	}
	transport.TLSClientConfig.RootCAs = caCertPool
	if transport.TLSClientConfig.MinVersion < tls.VersionTLS12 {
		transport.TLSClientConfig.MinVersion = tls.VersionTLS12
	}
	return kyvernohttp.Context{ContextInterface: newCELHTTPContext(transport)}, nil
}
