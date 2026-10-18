/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

//+kubebuilder:object:root=true
//+kubebuilder:resource:shortName=agent
//+kubebuilder:subresource:status

// Agent is the Schema for the agents API
// Agent defines a reusable AI agent configuration for workflow steps
type Agent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentSpec   `json:"spec,omitempty"`
	Status AgentStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// AgentList contains a list of Agent
type AgentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Agent `json:"items"`
}

// AgentSpec defines the desired state of Agent
type AgentSpec struct {
	// Prompt is the system prompt for the agent
	// This is a static prompt that defines the agent's role and base instructions
	// For dynamic content, use additionalPrompts in the workflow step's agentRef
	// +kubebuilder:validation:Required
	Prompt string `json:"prompt"`

	// ModelProvider is the LLM provider to use. This field is required.
	// Options: nirmata, openai, anthropic, azure-openai, google, gemini, local
	// +kubebuilder:validation:Enum=nirmata;openai;anthropic;azure-openai;google;gemini;local
	// +kubebuilder:validation:Required
	ModelProvider string `json:"modelProvider"`

	// ModelName is the specific model to use (e.g., "gpt-4", "claude-3-opus")
	// Default depends on provider
	// +optional
	ModelName string `json:"modelName,omitempty"`

	// MCPTools is a list of MCP tools the agent can use
	// Each tool is specified as "server:tool" (e.g., "kubernetes-mcp:get-resource")
	// +optional
	MCPTools []string `json:"mcpTools,omitempty"`

	// OutputExtraction defines how to extract outputs from agent responses
	// +optional
	OutputExtraction *OutputExtraction `json:"outputExtraction,omitempty"`

	// ServiceAccount is the Kubernetes service account to use for agent execution
	// Used for RBAC and authentication
	// +optional
	ServiceAccount string `json:"serviceAccount,omitempty"`

	// ExecutorImage is a custom container image for agent execution
	// If not specified, uses the default executor image (ghcr.io/nirmata/ottoflow/agent-executor:latest)
	// +optional
	ExecutorImage string `json:"executorImage,omitempty"`

	// ServiceName is the name of the AgentExecutor Service
	// If not specified, uses default: ottoflow-agent-executor
	// +optional
	ServiceName string `json:"serviceName,omitempty"`

	// ServiceNamespace is the namespace of the AgentExecutor Service
	// Defaults to ottoflow
	// +optional
	ServiceNamespace string `json:"serviceNamespace,omitempty"`

	// Resources defines resource requests/limits for agent execution
	// Currently used for future sandbox mode
	// All steps using this agent use these resources
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// Config contains provider-specific client options. The executor reads exactly
	// two keys (internal/agent/default_executor.go):
	//
	// - endpoint: accepted ONLY if the agent-executor operator has allowlisted its origin
	// via AGENT_LLM_ENDPOINT_ALLOWLIST (Helm: agentExecutor.llmEndpointAllowlist), and only
	// as an origin ("scheme://host[:port]"; a path, query, fragment or userinfo is rejected).
	// Otherwise any non-empty value fails the agent run, for every provider: the
	// agent-executor's LLM API keys are process-wide, so an endpoint the operator has not
	// approved must not receive them. Once accepted, it is passed to gollm as
	// ClientOptions.URL; with the pinned gollm it is honored ONLY by modelProvider
	// "azure-openai" (overrides AZURE_OPENAI_ENDPOINT; the endpoint must be https://);
	// "openai" reads OPENAI_ENDPOINT/OPENAI_API_BASE from the process environment instead;
	// "google", "gemini" and "anthropic" ignore it; "local" uses LLAMACPP_HOST.
	// - skipVerifySSL: "true" is REJECTED for every provider and fails the agent run; "false"
	// is accepted. To reach an endpoint with a private CA, set SSL_CERT_FILE or SSL_CERT_DIR on
	// the agent-executor process (keeps TLS verification on); as a last resort, set
	// LLM_SKIP_VERIFY_SSL=true on that process, a process-wide setting not scoped to one Agent.
	//
	// API keys are NOT read from here; they come from the agent-executor process
	// environment (OPENAI_API_KEY, ANTHROPIC_API_KEY, GEMINI_API_KEY, AZURE_OPENAI_API_KEY).
	// +optional
	Config map[string]string `json:"config,omitempty"`
}

// OutputExtraction defines how to extract structured outputs from agent responses
type OutputExtraction struct {
	// Type is the extraction method
	// Options: json, regex, text
	// +kubebuilder:validation:Enum=json;regex;text
	// +kubebuilder:default=json
	// +optional
	Type string `json:"type,omitempty"`

	// Pattern is the extraction pattern
	// For json: JSONPath expression (e.g., "$.result.recommendations"). A pattern that
	//   matches nothing fails the step. Omit to return the whole JSON object.
	// For regex: Regular expression with capture groups
	// For text: unused - the full response is always returned. Use type regex to select
	//   a substring.
	// +optional
	Pattern string `json:"pattern,omitempty"`

	// Schema defines the expected output schema (for JSON extraction)
	// Stored as raw JSON bytes
	// +optional
	Schema []byte `json:"schema,omitempty"`
}

// AgentStatus defines the observed state of Agent
type AgentStatus struct {
	// Phase represents the current phase of the Agent
	// +kubebuilder:validation:Enum=Ready;NotReady
	// +optional
	Phase AgentPhase `json:"phase,omitempty"`

	// Message provides additional information about the agent status
	// +optional
	Message string `json:"message,omitempty"`

	// LastChecked is when the agent configuration was last validated
	// +optional
	LastChecked *metav1.Time `json:"lastChecked,omitempty"`
}

// AgentPhase represents the phase of an Agent
// +kubebuilder:validation:Enum=Ready;NotReady
type AgentPhase string

const (
	// AgentPhaseReady indicates the agent is ready to use
	AgentPhaseReady AgentPhase = "Ready"

	// AgentPhaseNotReady indicates the agent is not ready
	AgentPhaseNotReady AgentPhase = "NotReady"
)

func init() {
	objectTypes = append(objectTypes,
		&Agent{}, &AgentList{},
		&MCPServer{}, &MCPServerList{},
		&StepTemplate{}, &StepTemplateList{},
	)
}
