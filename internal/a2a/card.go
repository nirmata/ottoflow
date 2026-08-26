/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package a2a

import (
	"fmt"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

const (
	// a2aProtocolVersion matches a2aWireVersion in internal/workflow/executor/a2a_client.go.
	a2aProtocolVersion = "0.3"
	cardAgentVersion   = "0.1.0"
)

// AgentCard is the subset of the A2A AgentCard we serve at /.well-known/agent-card.json.
type AgentCard struct {
	Name               string            `json:"name"`
	Description        string            `json:"description"`
	URL                string            `json:"url"`
	Version            string            `json:"version"`
	ProtocolVersion    string            `json:"protocolVersion"`
	PreferredTransport string            `json:"preferredTransport"`
	Capabilities       AgentCapabilities `json:"capabilities"`
	DefaultInputModes  []string          `json:"defaultInputModes"`
	DefaultOutputModes []string          `json:"defaultOutputModes"`
	Skills             []AgentSkill      `json:"skills"`
}

// AgentCapabilities advertises optional A2A features. We stream, so Streaming is true.
type AgentCapabilities struct {
	Streaming bool `json:"streaming"`
}

// AgentSkill describes one capability of the agent.
type AgentSkill struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
	Examples    []string `json:"examples"`
}

// BuildCard synthesizes an AgentCard from the target Workflow, drawing the human-facing
// name, description, tags and examples from spec.expose.kagent when set. These are the same
// values the API and the samples advertise; before this the card silently served only the
// Workflow name and an empty skill, so an A2A client saw none of them.
func BuildCard(wf *ottoflowv1alpha1.Workflow, url string) AgentCard {
	// Workflow has no top-level Description; fall back to a synthesized sentence.
	name := wf.Name
	desc := fmt.Sprintf("Runs the %s OttoFlow workflow", wf.Name)
	var tags, examples []string
	if k := wf.Spec.Expose.GetKagent(); k != nil {
		if k.DisplayName != "" {
			name = k.DisplayName
		}
		if k.Description != "" {
			desc = k.Description
		}
		tags = k.Tags
		examples = k.Examples
	}
	// The A2A schema requires non-null arrays; keep empty slices rather than nil.
	if tags == nil {
		tags = []string{}
	}
	if examples == nil {
		examples = []string{}
	}

	return AgentCard{
		Name:               name,
		Description:        desc,
		URL:                url,
		Version:            cardAgentVersion,
		ProtocolVersion:    a2aProtocolVersion,
		PreferredTransport: "JSONRPC",
		Capabilities:       AgentCapabilities{Streaming: true},
		DefaultInputModes:  []string{"text"},
		DefaultOutputModes: []string{"text"},
		Skills: []AgentSkill{{
			// ID stays the Workflow name (stable identifier); Name is the human label.
			ID:          wf.Name,
			Name:        name,
			Description: desc,
			Tags:        tags,
			Examples:    examples,
		}},
	}
}
