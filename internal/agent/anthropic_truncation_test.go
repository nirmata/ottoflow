/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

// These tests drive DefaultAgentExecutor.ExecuteAgent against a fake Anthropic Messages
// endpoint (SSE) whose reply stops at max_tokens, and assert what ExecuteAgent returns.

// sseEvent renders one server-sent event.
func sseEvent(typ, data string) string {
	return fmt.Sprintf("event: %s\ndata: %s\n\n", typ, data)
}

// newTruncatedAnthropicServer answers every POST with a streamed reply made of blocks
// (content_block_start/delta/stop events) that ends with stop_reason max_tokens.
func newTruncatedAnthropicServer(t *testing.T, blocks string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseEvent("message_start", `{"type":"message_start","message":{"id":"m1","type":"message","role":"assistant","content":[],"model":"claude-sonnet-5-5","usage":{"input_tokens":1,"output_tokens":1}}}`)+
			blocks+
			sseEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"input_tokens":1,"output_tokens":16384}}`)+
			sseEvent("message_stop", `{"type":"message_stop"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func runAnthropicAgent(t *testing.T, endpoint string) (string, error) {
	t.Helper()
	for _, k := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_MAX_TOKENS"} {
		t.Setenv(k, "")
	}
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	t.Setenv(AgentLLMEndpointAllowlistEnv, endpoint)
	agentCRD := &ottoflowv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "default"},
		Spec: ottoflowv1alpha1.AgentSpec{
			ModelProvider: "anthropic", ModelName: "claude-sonnet-5-5",
			Config: map[string]string{"endpoint": endpoint},
		},
	}
	resp, _, err := NewDefaultAgentExecutor(nil).ExecuteAgent(context.Background(), agentCRD, "prompt", nil, "default")
	return resp, err
}

// A step whose reply is cut off inside a tool call must fail, not run a half-written call.
func TestExecuteAgent_AnthropicMaxTokensTruncatedToolCall_Fails(t *testing.T) {
	srv := newTruncatedAnthropicServer(t,
		sseEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)+
			sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Checking pods."}}`)+
			sseEvent("content_block_stop", `{"type":"content_block_stop","index":0}`)+
			sseEvent("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_pods","input":{}}}`)+
			sseEvent("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"namespace\":\"def"}}`))

	resp, err := runAnthropicAgent(t, srv.URL)

	if err == nil {
		t.Fatalf("ExecuteAgent returned %q and no error; want a failure for a truncated tool call", resp)
	}
	for _, want := range []string{"agent execution failed", "stop_reason=max_tokens", `last block "tool_use"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v; want it to contain %q", err, want)
		}
	}
}

// A reply truncated inside a text block is kept: the partial text is the step's result.
func TestExecuteAgent_AnthropicMaxTokensTruncatedText_ReturnsPartial(t *testing.T) {
	srv := newTruncatedAnthropicServer(t,
		sseEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)+
			sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial answer"}}`))

	resp, err := runAnthropicAgent(t, srv.URL)

	if err != nil {
		t.Fatalf("ExecuteAgent error = %v; want the partial text kept", err)
	}
	if resp != "partial answer" {
		t.Errorf("response = %q; want %q", resp, "partial answer")
	}
}
