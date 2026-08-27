/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package toolloop

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/kubectl-ai/gollm"
	"github.com/GoogleCloudPlatform/kubectl-ai/pkg/api"
	"github.com/GoogleCloudPlatform/kubectl-ai/pkg/tools"
)

// fakeChat is a minimal gollm.Chat scripted with one set of responses per
// SendStreaming call (one entry per loop iteration).
type fakeChat struct {
	turns   [][]fakeResponse
	sendErr error
	calls   int
}

func (c *fakeChat) Send(ctx context.Context, contents ...any) (gollm.ChatResponse, error) {
	return nil, errors.New("fakeChat.Send not implemented")
}

func (c *fakeChat) SendStreaming(ctx context.Context, contents ...any) (gollm.ChatResponseIterator, error) {
	if c.sendErr != nil {
		return nil, c.sendErr
	}
	idx := c.calls
	c.calls++
	if idx >= len(c.turns) {
		return nil, errors.New("fakeChat: no scripted turn for this call")
	}
	turn := c.turns[idx]
	return gollm.ChatResponseIterator(func(yield func(gollm.ChatResponse, error) bool) {
		for _, r := range turn {
			if !yield(r, nil) {
				return
			}
		}
	}), nil
}

func (c *fakeChat) SetFunctionDefinitions(defs []*gollm.FunctionDefinition) error { return nil }
func (c *fakeChat) IsRetryableError(err error) bool                               { return false }
func (c *fakeChat) Initialize(messages []*api.Message) error                      { return nil }

// fakeResponse is a minimal gollm.ChatResponse/Candidate/Part all in one.
type fakeResponse struct {
	text  string
	calls []gollm.FunctionCall
	usage any
}

func (r fakeResponse) UsageMetadata() any            { return r.usage }
func (r fakeResponse) Candidates() []gollm.Candidate { return []gollm.Candidate{fakeCandidate{r}} }

type fakeCandidate struct{ r fakeResponse }

func (c fakeCandidate) String() string      { return c.r.text }
func (c fakeCandidate) Parts() []gollm.Part { return []gollm.Part{fakePart(c)} }

type fakePart struct{ r fakeResponse }

func (p fakePart) AsText() (string, bool) {
	if p.r.text == "" {
		return "", false
	}
	return p.r.text, true
}

func (p fakePart) AsFunctionCalls() ([]gollm.FunctionCall, bool) {
	if len(p.r.calls) == 0 {
		return nil, false
	}
	return p.r.calls, true
}

// emptyCandidatesResponse simulates gollm's Anthropic streaming client, which
// yields a trailing response carrying only usage metadata and no
// text/functionCall once a stream ends (see anthropic.go's SendStreaming).
type emptyCandidatesResponse struct{ usage any }

func (r emptyCandidatesResponse) UsageMetadata() any            { return r.usage }
func (r emptyCandidatesResponse) Candidates() []gollm.Candidate { return nil }

// fakeTool is a minimal tools.Tool that records invocations and delegates to runFunc.
type fakeTool struct {
	name    string
	runFunc func(ctx context.Context, args map[string]any) (any, error)
	calls   []map[string]any
}

func (t *fakeTool) Name() string        { return t.name }
func (t *fakeTool) Description() string { return "fake tool" }
func (t *fakeTool) FunctionDefinition() *gollm.FunctionDefinition {
	return &gollm.FunctionDefinition{Name: t.name}
}
func (t *fakeTool) Run(ctx context.Context, args map[string]any) (any, error) {
	t.calls = append(t.calls, args)
	return t.runFunc(ctx, args)
}
func (t *fakeTool) IsInteractive(args map[string]any) (bool, error)  { return false, nil }
func (t *fakeTool) CheckModifiesResource(args map[string]any) string { return "no" }

func TestRun_NoToolCalls_ReturnsFinalAnswer(t *testing.T) {
	chat := &fakeChat{turns: [][]fakeResponse{
		{{text: "the answer", usage: map[string]any{"input_tokens": 10, "output_tokens": 5}}},
	}}

	text, usage, err := Run(context.Background(), chat, []any{"hello"}, nil, Options{})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if text != "the answer" {
		t.Errorf("text = %q, want %q", text, "the answer")
	}
	if usage == nil {
		t.Error("expected usage metadata to be captured, got nil")
	}
}

func TestRun_OneToolCallRoundTrip(t *testing.T) {
	tool := &fakeTool{
		name: "get_weather",
		runFunc: func(ctx context.Context, args map[string]any) (any, error) {
			return "sunny", nil
		},
	}
	chat := &fakeChat{turns: [][]fakeResponse{
		{{calls: []gollm.FunctionCall{{ID: "1", Name: "get_weather", Arguments: map[string]any{"city": "sf"}}}}},
		{{text: "it's sunny in sf"}},
	}}

	text, _, err := Run(context.Background(), chat, []any{"what's the weather"}, map[string]tools.Tool{"get_weather": tool}, Options{})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if text != "it's sunny in sf" {
		t.Errorf("text = %q, want %q", text, "it's sunny in sf")
	}
	if len(tool.calls) != 1 {
		t.Fatalf("tool called %d times, want 1", len(tool.calls))
	}
	if tool.calls[0]["city"] != "sf" {
		t.Errorf("tool called with args %v, want city=sf", tool.calls[0])
	}
	if chat.calls != 2 {
		t.Errorf("chat.SendStreaming called %d times, want 2", chat.calls)
	}
}

func TestRun_MultipleRounds(t *testing.T) {
	tool := &fakeTool{
		name: "step",
		runFunc: func(ctx context.Context, args map[string]any) (any, error) {
			return "ok", nil
		},
	}
	chat := &fakeChat{turns: [][]fakeResponse{
		{{calls: []gollm.FunctionCall{{Name: "step", Arguments: map[string]any{}}}}},
		{{calls: []gollm.FunctionCall{{Name: "step", Arguments: map[string]any{}}}}},
		{{calls: []gollm.FunctionCall{{Name: "step", Arguments: map[string]any{}}}}},
		{{text: "done after three rounds"}},
	}}

	text, _, err := Run(context.Background(), chat, []any{"go"}, map[string]tools.Tool{"step": tool}, Options{})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if text != "done after three rounds" {
		t.Errorf("text = %q, want %q", text, "done after three rounds")
	}
	if len(tool.calls) != 3 {
		t.Errorf("tool called %d times, want 3", len(tool.calls))
	}
}

func TestRun_MaxIterationsReached_ReturnsExplicitError(t *testing.T) {
	tool := &fakeTool{
		name: "loop_forever",
		runFunc: func(ctx context.Context, args map[string]any) (any, error) {
			return "ok", nil
		},
	}
	// Script more turns than MaxIterations so the loop would keep going forever
	// without the cap.
	turns := make([][]fakeResponse, 10)
	for i := range turns {
		turns[i] = []fakeResponse{{calls: []gollm.FunctionCall{{Name: "loop_forever", Arguments: map[string]any{}}}}}
	}
	chat := &fakeChat{turns: turns}

	_, _, err := Run(context.Background(), chat, []any{"go"}, map[string]tools.Tool{"loop_forever": tool}, Options{MaxIterations: 3})
	if err == nil {
		t.Fatal("expected an error when MaxIterations is reached, got nil")
	}
	if !strings.Contains(err.Error(), "max iterations") {
		t.Errorf("error = %q, want it to mention max iterations", err.Error())
	}
	if chat.calls != 3 {
		t.Errorf("chat.SendStreaming called %d times, want exactly 3 (the cap)", chat.calls)
	}
}

func TestRun_ToolRunError_AbortsLoop(t *testing.T) {
	boom := errors.New("boom")
	tool := &fakeTool{
		name: "flaky",
		runFunc: func(ctx context.Context, args map[string]any) (any, error) {
			return nil, boom
		},
	}
	chat := &fakeChat{turns: [][]fakeResponse{
		{{calls: []gollm.FunctionCall{{Name: "flaky", Arguments: map[string]any{}}}}},
	}}

	_, _, err := Run(context.Background(), chat, []any{"go"}, map[string]tools.Tool{"flaky": tool}, Options{})
	if err == nil {
		t.Fatal("expected an error when a tool's Run fails, got nil")
	}
	if !errors.Is(err, boom) {
		t.Errorf("error = %v, want it to wrap %v", err, boom)
	}
}

func TestRun_UnknownTool_ReturnsError(t *testing.T) {
	chat := &fakeChat{turns: [][]fakeResponse{
		{{calls: []gollm.FunctionCall{{Name: "does_not_exist", Arguments: map[string]any{}}}}},
	}}

	_, _, err := Run(context.Background(), chat, []any{"go"}, nil, Options{})
	if err == nil {
		t.Fatal("expected an error for an unrecognized tool call, got nil")
	}
	if !strings.Contains(err.Error(), "does_not_exist") {
		t.Errorf("error = %q, want it to name the unknown tool", err.Error())
	}
}

func TestRun_SendStreamingError_Propagates(t *testing.T) {
	sendErr := errors.New("network down")
	chat := &fakeChat{sendErr: sendErr}

	_, _, err := Run(context.Background(), chat, []any{"go"}, nil, Options{})
	if !errors.Is(err, sendErr) {
		t.Errorf("error = %v, want it to wrap %v", err, sendErr)
	}
}

// trailingUsageChat scripts a single SendStreaming call that yields text
// followed by a trailing usage-only response with zero candidates, matching
// gollm's Anthropic streaming client's behavior at the end of every stream.
type trailingUsageChat struct{}

func (c *trailingUsageChat) Send(ctx context.Context, contents ...any) (gollm.ChatResponse, error) {
	return nil, errors.New("trailingUsageChat.Send not implemented")
}

func (c *trailingUsageChat) SendStreaming(ctx context.Context, contents ...any) (gollm.ChatResponseIterator, error) {
	return gollm.ChatResponseIterator(func(yield func(gollm.ChatResponse, error) bool) {
		if !yield(fakeResponse{text: "the answer"}, nil) {
			return
		}
		yield(emptyCandidatesResponse{usage: map[string]any{"input_tokens": 10, "output_tokens": 5}}, nil)
	}), nil
}

func (c *trailingUsageChat) SetFunctionDefinitions(defs []*gollm.FunctionDefinition) error { return nil }
func (c *trailingUsageChat) IsRetryableError(err error) bool                               { return false }
func (c *trailingUsageChat) Initialize(messages []*api.Message) error                      { return nil }

func TestRun_TrailingUsageOnlyResponse_SkippedNotFatal(t *testing.T) {
	text, usage, err := Run(context.Background(), &trailingUsageChat{}, []any{"hello"}, nil, Options{})
	if err != nil {
		t.Fatalf("Run returned error: %v, want the trailing zero-candidate usage response to be skipped", err)
	}
	if text != "the answer" {
		t.Errorf("text = %q, want %q", text, "the answer")
	}
	if usage == nil {
		t.Error("expected usage metadata from the trailing response to be captured, got nil")
	}
}

func TestRun_DefaultMaxIterationsUsedWhenUnset(t *testing.T) {
	// Options{} (MaxIterations: 0) should fall back to DefaultMaxIterations,
	// not loop zero times.
	chat := &fakeChat{turns: [][]fakeResponse{{{text: "ok"}}}}
	text, _, err := Run(context.Background(), chat, []any{"go"}, nil, Options{})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if text != "ok" {
		t.Errorf("text = %q, want %q", text, "ok")
	}
}
