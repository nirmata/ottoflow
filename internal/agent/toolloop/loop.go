/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

// Package toolloop implements a headless tool-calling conversation loop:
// send, detect function calls, dispatch them against a caller-supplied tool
// set, feed the results back, and repeat until a final answer arrives.
//
// It is ported from github.com/GoogleCloudPlatform/kubectl-ai's
// pkg/agent/conversation.go (Apache-2.0), stripped of everything specific to
// that package's interactive REPL: input/output channels, session
// persistence, sandboxed tool execution, and permission-confirmation
// prompts. Callers here run non-interactively and always execute dispatched
// tool calls immediately.
package toolloop

import (
	"context"
	"fmt"

	"github.com/GoogleCloudPlatform/kubectl-ai/gollm"
	"github.com/GoogleCloudPlatform/kubectl-ai/pkg/tools"
)

// DefaultMaxIterations matches kubectl-ai's own CLI --max-iterations default.
const DefaultMaxIterations = 20

// Options configures a single Run of the tool-calling loop.
type Options struct {
	// MaxIterations caps the number of send/dispatch rounds before Run returns
	// an error instead of looping forever. Zero or negative defaults to
	// DefaultMaxIterations.
	MaxIterations int
}

// Run drives chat through a send -> detect-function-calls -> dispatch -> repeat
// loop, starting from initialContent and dispatching any function call the
// model emits against toolset (keyed by tool name, matching gollm.FunctionCall.Name).
// It returns the final text answer once the model responds with no function
// calls, or an error if MaxIterations is reached first.
//
// Callers must have already registered toolset's function definitions on chat
// via chat.SetFunctionDefinitions before calling Run.
//
// lastUsage is whatever UsageMetadata() the most recently seen response
// reported (opaque; pass it to a reflection-based token extractor), or nil if
// no response reported usage.
func Run(ctx context.Context, chat gollm.Chat, initialContent []any, toolset map[string]tools.Tool, opts Options) (finalText string, lastUsage any, err error) {
	maxIterations := opts.MaxIterations
	if maxIterations <= 0 {
		maxIterations = DefaultMaxIterations
	}

	content := initialContent
	var usage any

	for iteration := 0; iteration < maxIterations; iteration++ {
		stream, sendErr := chat.SendStreaming(ctx, content...)
		if sendErr != nil {
			return "", usage, fmt.Errorf("sending chat message: %w", sendErr)
		}

		var text string
		var functionCalls []gollm.FunctionCall
		for response, streamErr := range stream {
			if streamErr != nil {
				return "", usage, fmt.Errorf("reading chat response: %w", streamErr)
			}
			if response == nil {
				break
			}
			if u := response.UsageMetadata(); u != nil {
				usage = u
			}
			candidates := response.Candidates()
			if len(candidates) == 0 {
				// Some providers (e.g. gollm's Anthropic streaming client) emit a
				// trailing response that carries only usage metadata and no
				// content once the stream ends. That's not a broken response --
				// a genuinely empty/blocked model reply surfaces as streamErr
				// above, not as a content-less-but-successful event here.
				continue
			}
			for _, part := range candidates[0].Parts() {
				if t, ok := part.AsText(); ok {
					text += t
				}
				if calls, ok := part.AsFunctionCalls(); ok && len(calls) > 0 {
					functionCalls = append(functionCalls, calls...)
				}
			}
		}

		if len(functionCalls) == 0 {
			return text, usage, nil
		}

		results, dispatchErr := dispatchToolCalls(ctx, functionCalls, toolset)
		if dispatchErr != nil {
			return "", usage, dispatchErr
		}
		content = results
	}

	return "", usage, fmt.Errorf("tool-calling loop reached max iterations (%d) without a final answer", maxIterations)
}

// dispatchToolCalls runs each function call against toolset and returns the
// gollm.FunctionCallResults to feed back into the next chat turn. A tool
// execution error aborts the whole call batch, matching kubectl-ai's
// DispatchToolCalls semantics.
func dispatchToolCalls(ctx context.Context, calls []gollm.FunctionCall, toolset map[string]tools.Tool) ([]any, error) {
	results := make([]any, 0, len(calls))
	for _, call := range calls {
		tool, ok := toolset[call.Name]
		if !ok {
			return nil, fmt.Errorf("model requested unknown tool %q", call.Name)
		}
		output, err := tool.Run(ctx, call.Arguments)
		if err != nil {
			return nil, fmt.Errorf("running tool %q: %w", call.Name, err)
		}
		result, err := tools.ToolResultToMap(output)
		if err != nil {
			return nil, fmt.Errorf("converting result of tool %q: %w", call.Name, err)
		}
		results = append(results, gollm.FunctionCallResult{
			ID:     call.ID,
			Name:   call.Name,
			Result: result,
		})
	}
	return results, nil
}
