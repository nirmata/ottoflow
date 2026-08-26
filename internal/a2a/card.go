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

// BuildCard synthesizes an AgentCard from the target Workflow.
func BuildCard(wf *ottoflowv1alpha1.Workflow, url string) AgentCard {
	// Workflow has no Description field; synthesize a sane default.
	desc := fmt.Sprintf("Runs the %s OttoFlow workflow", wf.Name)
	return AgentCard{
		Name:               wf.Name,
		Description:        desc,
		URL:                url,
		Version:            cardAgentVersion,
		ProtocolVersion:    a2aProtocolVersion,
		PreferredTransport: "JSONRPC",
		Capabilities:       AgentCapabilities{Streaming: true},
		DefaultInputModes:  []string{"text"},
		DefaultOutputModes: []string{"text"},
		Skills: []AgentSkill{{
			ID:          wf.Name,
			Name:        wf.Name,
			Description: desc,
			Tags:        []string{},
			Examples:    []string{},
		}},
	}
}
