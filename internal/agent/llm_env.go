/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package agent

import (
	"context"
	"os"
)

type llmEnvKey struct{}

// WithLLMEnvOverride attaches a map of env variable names to values (credentials and
// LLM configuration) to the context. When present, GetLLMEnv uses this map instead of
// os.Getenv for the agent-executor path. Used for per-request credential forwarding.
func WithLLMEnvOverride(ctx context.Context, env map[string]string) context.Context {
	if env == nil {
		return ctx
	}
	// Copy so caller cannot mutate after the fact
	cp := make(map[string]string, len(env))
	for k, v := range env {
		cp[k] = v
	}
	return context.WithValue(ctx, llmEnvKey{}, cp)
}

// GetLLMEnv returns the value for the given key from the context's LLM env override
// map if present, otherwise from os.Getenv(key). Used for credentials and LLM config
// (e.g. NIRMATA_LLM_TOKEN, NIRMATA_LLM_MODEL).
func GetLLMEnv(ctx context.Context, key string) string {
	if ctx != nil {
		if m, ok := ctx.Value(llmEnvKey{}).(map[string]string); ok && m != nil {
			if v, ok := m[key]; ok {
				return v
			}
		}
	}
	return os.Getenv(key)
}
