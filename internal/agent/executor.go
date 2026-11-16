/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package agent

import (
	"context"
	"fmt"
	"reflect"
	"regexp"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

// Provider name for the Nirmata LLM.
const providerNirmata = "nirmata"

// Provider names shared with DefaultAgentExecutor's provider-mapping switches.
const (
	providerOpenAI      = "openai"
	providerAnthropic   = "anthropic"
	providerAzureOpenAI = "azure-openai"
	providerGoogle      = "google"
	providerGemini      = "gemini"
	providerLocal       = "local"
)

// IsValidProvider returns true if the provider name is recognized. An empty
// provider is not valid: modelProvider is a required field on Agent, enforced
// by the CRD schema at admission. CLI local mode is the one path that schema
// enforcement can't reach (it reads Agent YAML directly), so callers there
// must reject "" explicitly using this function.
func IsValidProvider(provider string) bool {
	switch provider {
	case providerNirmata, providerOpenAI, providerAnthropic, providerAzureOpenAI, providerGoogle, providerGemini, providerLocal:
		return true
	default:
		return false
	}
}

// nirmataUnavailableExecutor is the routing delegate for ModelProvider "nirmata"
// (and empty, retained only for backward compatibility with Agent objects
// stored before modelProvider became required — CRD required is enforced at
// admission, not retroactively) in this source-available build. The Nirmata LLM
// provider ships in the OttoFlow enterprise plugin, which is not part of this
// module. Selecting it returns a clear, registration-style error rather than a
// build failure or a panic.
type nirmataUnavailableExecutor struct{}

// newNirmataUnavailableExecutor returns the enterprise-required stub delegate.
func newNirmataUnavailableExecutor() *nirmataUnavailableExecutor {
	return &nirmataUnavailableExecutor{}
}

// ExecuteAgent always fails with an actionable error explaining that the
// Nirmata provider requires the enterprise plugin.
func (e *nirmataUnavailableExecutor) ExecuteAgent(_ context.Context, agentCRD *ottoflowv1alpha1.Agent, _ string, _ map[string]interface{}, _ string) (string, AgentTokenUsage, error) {
	provider := agentCRD.Spec.ModelProvider
	if provider == "" {
		provider = providerNirmata
	}
	return "", AgentTokenUsage{}, fmt.Errorf(
		"modelProvider %q is not available in this build: the Nirmata LLM provider is part of the OttoFlow enterprise plugin; "+
			"set an agent modelProvider of openai, anthropic, google/gemini, or local instead",
		provider,
	)
}

// mapBracketRe matches verbose Go map literals (map[...]) in error strings.
var mapBracketRe = regexp.MustCompile(`\bmap\[[^\]]{20,}\]`)

// condenseLLMError strips verbose Go map/slice structures from LLM provider
// errors (e.g., gRPC error details) and truncates to a readable length.
func condenseLLMError(err error) string {
	msg := err.Error()
	msg = mapBracketRe.ReplaceAllString(msg, "(details omitted)")
	const maxLen = 300
	if len(msg) > maxLen {
		msg = msg[:maxLen] + "..."
	}
	return msg
}

// usageTokenCounts extracts (inputTokens, outputTokens) from gollm usage metadata.
// Handles structs with InputTokens/OutputTokens fields (Anthropic, Bedrock *int32) and
// map[string]interface{} used by some provider wrappers.
func usageTokenCounts(usage any) (int64, int64) {
	if usage == nil {
		return 0, 0
	}
	v := reflect.ValueOf(usage)
	if v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return 0, 0
		}
		v = v.Elem()
	}
	if v.Kind() == reflect.Struct {
		in := derefIntField(v, "InputTokens")
		out := derefIntField(v, "OutputTokens")
		if in > 0 || out > 0 {
			return in, out
		}
	}
	if m, ok := usage.(map[string]interface{}); ok {
		return mapInt64(m, "input_tokens"), mapInt64(m, "output_tokens")
	}
	return 0, 0
}

func derefIntField(v reflect.Value, name string) int64 {
	f := v.FieldByName(name)
	if !f.IsValid() {
		return 0
	}
	if f.Kind() == reflect.Ptr {
		if f.IsNil() {
			return 0
		}
		f = f.Elem()
	}
	if f.CanInt() {
		return f.Int()
	}
	return 0
}

func mapInt64(m map[string]interface{}, key string) int64 {
	switch t := m[key].(type) {
	case int:
		return int64(t)
	case int32:
		return int64(t)
	case int64:
		return t
	case float64:
		return int64(t)
	}
	return 0
}
