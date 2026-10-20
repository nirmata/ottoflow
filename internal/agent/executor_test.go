/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package agent

// This file holds shared gollm test doubles (mockLLMClientFactory and the
// mockGollm* client/chat/response fakes) used across the agent package's
// executor tests, notably default_executor_test.go. It intentionally contains
// no Ginkgo specs of its own.

import (
	"context"

	"github.com/GoogleCloudPlatform/kubectl-ai/gollm"
	"github.com/GoogleCloudPlatform/kubectl-ai/pkg/api"
)

// mockLLMClientFactory implements LLMClientFactory for tests.
type mockLLMClientFactory struct {
	client     gollm.Client
	err        error
	called     bool
	providerID string
	opts       []gollm.Option // captured so a test can apply them and assert co.URL
}

func (m *mockLLMClientFactory) NewClient(ctx context.Context, providerID string, opts ...gollm.Option) (gollm.Client, error) {
	m.called = true
	m.providerID = providerID
	m.opts = opts
	if m.err != nil {
		return nil, m.err
	}
	return m.client, nil
}

// mockGollmClient implements gollm.Client for tests (minimal; used when we only need a non-nil client).
type mockGollmClient struct{}

func (m *mockGollmClient) Close() error { return nil }

func (m *mockGollmClient) StartChat(systemPrompt, model string) gollm.Chat {
	return &mockGollmChat{}
}

func (m *mockGollmClient) GenerateCompletion(ctx context.Context, req *gollm.CompletionRequest) (gollm.CompletionResponse, error) {
	return nil, nil
}

func (m *mockGollmClient) SetResponseSchema(schema *gollm.Schema) error { return nil }

func (m *mockGollmClient) ListModels(ctx context.Context) ([]string, error) { return nil, nil }

// mockGollmChat implements gollm.Chat for tests.
type mockGollmChat struct{}

func (m *mockGollmChat) Send(ctx context.Context, contents ...any) (gollm.ChatResponse, error) {
	return nil, nil
}

func (m *mockGollmChat) SendStreaming(ctx context.Context, contents ...any) (gollm.ChatResponseIterator, error) {
	return nil, nil
}

// mockChatResponse and related types implement gollm.ChatResponse for ExecuteAgent success path.
type mockChatResponse struct{ text string }

func (m *mockChatResponse) UsageMetadata() any { return nil }

func (m *mockChatResponse) Candidates() []gollm.Candidate {
	return []gollm.Candidate{&mockCandidate{text: m.text}}
}

type mockCandidate struct{ text string }

func (m *mockCandidate) String() string { return m.text }

func (m *mockCandidate) Parts() []gollm.Part {
	return []gollm.Part{&mockPart{text: m.text}}
}

type mockPart struct{ text string }

func (m *mockPart) AsText() (string, bool) { return m.text, true }

func (m *mockPart) AsFunctionCalls() ([]gollm.FunctionCall, bool) { return nil, false }

// mockGollmClientWithStreamResponse returns a chat whose SendStreaming yields one ChatResponse then ends.
type mockGollmClientWithStreamResponse struct {
	responseText string
}

func (m *mockGollmClientWithStreamResponse) Close() error { return nil }

func (m *mockGollmClientWithStreamResponse) StartChat(systemPrompt, model string) gollm.Chat {
	return &mockGollmChatWithStreamResponse{text: m.responseText}
}

func (m *mockGollmClientWithStreamResponse) GenerateCompletion(ctx context.Context, req *gollm.CompletionRequest) (gollm.CompletionResponse, error) {
	return nil, nil
}

func (m *mockGollmClientWithStreamResponse) SetResponseSchema(schema *gollm.Schema) error { return nil }

func (m *mockGollmClientWithStreamResponse) ListModels(ctx context.Context) ([]string, error) {
	return nil, nil
}

type mockGollmChatWithStreamResponse struct{ text string }

func (m *mockGollmChatWithStreamResponse) Send(ctx context.Context, contents ...any) (gollm.ChatResponse, error) {
	return nil, nil
}

func (m *mockGollmChatWithStreamResponse) SendStreaming(ctx context.Context, contents ...any) (gollm.ChatResponseIterator, error) {
	resp := &mockChatResponse{text: m.text}
	return func(yield func(gollm.ChatResponse, error) bool) {
		yield(resp, nil)
	}, nil
}

func (m *mockGollmChatWithStreamResponse) SetFunctionDefinitions(functionDefinitions []*gollm.FunctionDefinition) error {
	return nil
}

func (m *mockGollmChatWithStreamResponse) IsRetryableError(err error) bool { return false }

func (m *mockGollmChatWithStreamResponse) Initialize(messages []*api.Message) error {
	return nil
}

// mockGollmClientWithFailingStream returns a chat that errors on SendStreaming (for ExecuteAgent error path).
type mockGollmClientWithFailingStream struct {
	streamErr error
}

func (m *mockGollmClientWithFailingStream) Close() error { return nil }

func (m *mockGollmClientWithFailingStream) StartChat(systemPrompt, model string) gollm.Chat {
	return &mockGollmChatWithStreamError{err: m.streamErr}
}

func (m *mockGollmClientWithFailingStream) GenerateCompletion(ctx context.Context, req *gollm.CompletionRequest) (gollm.CompletionResponse, error) {
	return nil, nil
}

func (m *mockGollmClientWithFailingStream) SetResponseSchema(schema *gollm.Schema) error { return nil }

func (m *mockGollmClientWithFailingStream) ListModels(ctx context.Context) ([]string, error) {
	return nil, nil
}

type mockGollmChatWithStreamError struct {
	err error
}

func (m *mockGollmChatWithStreamError) Send(ctx context.Context, contents ...any) (gollm.ChatResponse, error) {
	return nil, m.err
}

func (m *mockGollmChatWithStreamError) SendStreaming(ctx context.Context, contents ...any) (gollm.ChatResponseIterator, error) {
	return nil, m.err
}

func (m *mockGollmChatWithStreamError) SetFunctionDefinitions(functionDefinitions []*gollm.FunctionDefinition) error {
	return nil
}

func (m *mockGollmChatWithStreamError) IsRetryableError(err error) bool { return false }

func (m *mockGollmChatWithStreamError) Initialize(messages []*api.Message) error {
	return nil
}

func (m *mockGollmChat) SetFunctionDefinitions(functionDefinitions []*gollm.FunctionDefinition) error {
	return nil
}

func (m *mockGollmChat) IsRetryableError(err error) bool { return false }

func (m *mockGollmChat) Initialize(messages []*api.Message) error {
	return nil
}
