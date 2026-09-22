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
// the origins ("scheme://host[:port]", comma-separated, ORIGIN ONLY) a tenant-authored
// Agent.spec.config.endpoint may name. Unset or empty => no endpoint is accepted at all.
//
// SECURITY: read with os.Getenv, NEVER agent.GetLLMEnv -- GetLLMEnv prefers the tenant-supplied
// X-LLM-Env override map (llm_env.go GetLLMEnv), which ParseLLMEnvHeader decodes from the
// X-LLM-Env request header with no key filtering. Reading the allowlist through GetLLMEnv
// would let a request supply its own allowlist. The name must also never appear in
// executor.LLMEnvAllowlist (exec_client.go), which it does not today.
const AgentLLMEndpointAllowlistEnv = "AGENT_LLM_ENDPOINT_ALLOWLIST"

// llmOrigin is a normalized scheme+host+port tuple, used so an allowlist entry and a tenant
// endpoint are compared on what actually determines where the request goes -- never on a
// string prefix.
type llmOrigin struct {
	scheme, host, port string
}

func normalizeOrigin(u *url.URL) llmOrigin {
	port := u.Port()
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		port = ""
	}
	return llmOrigin{
		scheme: u.Scheme,
		host:   strings.TrimSuffix(strings.ToLower(u.Hostname()), "."),
		port:   port,
	}
}

// llmEndpointAllowlist parses AGENT_LLM_ENDPOINT_ALLOWLIST into the set of origins a tenant
// Agent's spec.config.endpoint may name. A nil map and nil error means the operator listed
// nothing: callers MUST treat that as "no endpoint is permitted" -- it can never mean "no
// restriction".
func llmEndpointAllowlist() (map[llmOrigin]struct{}, error) {
	var entries []string
	for _, e := range strings.Split(os.Getenv(AgentLLMEndpointAllowlistEnv), ",") {
		if e = strings.TrimSpace(e); e != "" {
			entries = append(entries, e)
		}
	}
	if len(entries) == 0 {
		return nil, nil
	}

	origins := make(map[llmOrigin]struct{}, len(entries))
	for _, e := range entries {
		u, err := url.Parse(e)
		if err != nil {
			return nil, fmt.Errorf("entry %q: %w", e, err)
		}
		// Origin only. A path here would look like a credential boundary and is not one:
		// gollm's anthropic client keeps scheme+host and always POSTs to /v1/messages at the
		// host root, so a listed "/teams/platform" still sends the credential to /v1/messages.
		// Fail at boot rather than admit an entry whose meaning differs from what the
		// operator wrote.
		if u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("entry %q must be an absolute origin (scheme://host[:port])", e)
		}
		if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return nil, fmt.Errorf("entry %q must be an origin only (scheme://host[:port]): a path, "+
				"query, fragment or userinfo is not a credential boundary and would be silently "+
				"admitted rather than enforced", e)
		}
		origins[normalizeOrigin(u)] = struct{}{}
	}
	return origins, nil
}

// allowedLLMEndpoint returns the parsed endpoint when the operator has pre-approved its origin.
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
		return nil, fmt.Errorf("invalid spec.config.endpoint %q: %w", raw, err)
	}
	// Go does not carry userinfo in the URL it sends -- it converts it to a header
	// (net/http/client.go), and the azure client hands this URL over verbatim. That would put
	// a tenant-chosen Authorization header on the same request carrying the operator's
	// credential. The origin being allowlisted does not make the header the operator's choice.
	if parsed.User != nil {
		return nil, fmt.Errorf("userinfo is not permitted in spec.config.endpoint: it becomes an " +
			"Authorization: Basic header on the request that carries the operator's LLM credential")
	}
	// An allowlisted origin still forwards a tenant-supplied path/query/fragment to the LLM
	// client: azopenai's formatURL passes the endpoint as JoinPaths' root, which keeps the
	// root's path as a prefix and re-appends its query, so a tenant naming a path-multiplexing
	// gateway's origin can choose which backend behind it receives the operator's credential.
	if (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("path/query/fragment are not permitted in spec.config.endpoint: an "+
			"allowlisted origin still forwards them to the LLM client, which can steer the request to a "+
			"different backend behind the operator's allowlisted gateway while carrying its LLM credential "+
			"(%q)", raw)
	}
	if _, ok := origins[normalizeOrigin(parsed)]; !ok {
		return nil, fmt.Errorf("spec.config.endpoint origin %s://%s is not in the operator's allowlist (%s)",
			parsed.Scheme, parsed.Host, AgentLLMEndpointAllowlistEnv)
	}
	return parsed, nil
}

// ValidateLLMEndpointAllowlist reports a malformed AGENT_LLM_ENDPOINT_ALLOWLIST at process
// startup, so a typo'd entry fails loudly at boot instead of silently refusing every agent
// run later.
func ValidateLLMEndpointAllowlist() error {
	_, err := llmEndpointAllowlist()
	return err
}
