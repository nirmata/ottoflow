/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package agent

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

// AgentLLMEndpointAllowlistEnv names the agent-executor process environment variable listing
// the origins ("scheme://host[:port]", comma-separated, origin only) a tenant-authored
// Agent.spec.config.endpoint may name. Unset or empty means no endpoint is accepted at all.
//
// SECURITY: read with os.Getenv, never GetLLMEnv. GetLLMEnv prefers the per-request X-LLM-Env
// map, which a workflow run supplies, so reading the allowlist through it would let a run
// supply its own allowlist. For the same reason the name must never appear in
// executor.LLMEnvAllowlist (enforced by llm_env_invariant_test.go).
const AgentLLMEndpointAllowlistEnv = "AGENT_LLM_ENDPOINT_ALLOWLIST"

// llmOrigin is a normalized scheme, host and port, so an allowlist entry and a tenant endpoint
// are compared on what decides where the request goes, never on a string prefix.
type llmOrigin struct {
	scheme, host, port string
}

// originOf lowercases the host, drops a trailing dot (the same DNS name) and fills in the
// scheme's default port, so "https://LLM.example.:443" and "https://llm.example" compare equal.
// url.Parse already lowercases the scheme.
func originOf(u *url.URL) llmOrigin {
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		}
	}
	return llmOrigin{scheme: u.Scheme, host: strings.ToLower(strings.TrimRight(u.Hostname(), ".")), port: port}
}

// llmEndpointAllowlist parses AGENT_LLM_ENDPOINT_ALLOWLIST. A nil set with a nil error means
// the operator listed nothing: callers must treat that as "no endpoint is permitted", never as
// "no restriction".
func llmEndpointAllowlist() (map[llmOrigin]struct{}, error) {
	var origins map[llmOrigin]struct{}
	for _, e := range strings.Split(os.Getenv(AgentLLMEndpointAllowlistEnv), ",") {
		if e = strings.TrimSpace(e); e == "" {
			continue
		}
		u, err := url.Parse(e)
		if err != nil {
			return nil, fmt.Errorf("entry %q: %w", e, err)
		}
		if u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("entry %q must include scheme and host (e.g. https://llm.example.com)", e)
		}
		if err := requireASCIIHost(u); err != nil {
			return nil, fmt.Errorf("entry %q: %w", e, err)
		}
		// Origin only. A path would look like a credential boundary and is not one: an LLM
		// client may keep only scheme and host and append its own API path. Query, fragment
		// and userinfo play no part in matching. Fail at boot rather than admit an entry whose
		// meaning differs from what the operator wrote.
		if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return nil, fmt.Errorf("entry %q must be an origin only (scheme://host[:port]): a path is not a "+
				"credential boundary, and query, fragment and userinfo are ignored when matching", e)
		}
		if origins == nil {
			origins = map[llmOrigin]struct{}{}
		}
		origins[originOf(u)] = struct{}{}
	}
	return origins, nil
}

// allowedLLMEndpoint returns the parsed endpoint when the operator has allowlisted its origin
// and it carries nothing beyond that origin.
func allowedLLMEndpoint(raw string) (*url.URL, error) {
	origins, err := llmEndpointAllowlist()
	if err != nil {
		return nil, fmt.Errorf("%s is misconfigured: %w", AgentLLMEndpointAllowlistEnv, err)
	}
	if origins == nil {
		return nil, fmt.Errorf("the operator has allowlisted no LLM endpoints (%s is unset)",
			AgentLLMEndpointAllowlistEnv)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}
	if err := requireASCIIHost(parsed); err != nil {
		return nil, err
	}
	if _, ok := origins[originOf(parsed)]; !ok {
		return nil, fmt.Errorf("origin of %q is not in the operator's allowlist", raw)
	}
	// Go turns userinfo into an "Authorization: Basic" header (net/http), so it would put a
	// tenant-chosen header on the request that carries the operator's credential.
	if parsed.User != nil {
		return nil, fmt.Errorf("userinfo is not permitted in spec.config.endpoint: it becomes an " +
			"Authorization: Basic header on the request that carries the operator's LLM credential")
	}
	// The LLM client keeps the endpoint's path and query as the root it joins API paths onto, so
	// on a path-routing gateway they would choose which backend behind the allowlisted origin
	// receives the operator's credential.
	if (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("path/query/fragment are not permitted in spec.config.endpoint: the LLM "+
			"client forwards them, so they could route the request carrying the operator's LLM credential "+
			"to a different backend behind the allowlisted origin (%q)", raw)
	}
	return parsed, nil
}

// ValidateLLMEndpointAllowlist reports a malformed AGENT_LLM_ENDPOINT_ALLOWLIST at process
// startup, so a typo fails loudly at boot instead of refusing every agent run later.
func ValidateLLMEndpointAllowlist() error {
	_, err := llmEndpointAllowlist()
	return err
}

// requireASCIIHost refuses a non-ASCII hostname: case folding can map a non-ASCII rune onto an
// ASCII letter (U+0130 "İ" lowercases to "i", U+212A Kelvin sign to "k"), so a lookalike could
// match an allowlisted name while the transport dials a different one. Use the punycode form.
func requireASCIIHost(u *url.URL) error {
	for _, r := range u.Hostname() {
		if r > 127 {
			return fmt.Errorf("host %q must be ASCII (use the punycode form for an internationalized name)", u.Hostname())
		}
	}
	return nil
}
