/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	FailurePolicyFail     = "Fail"
	FailurePolicyContinue = "Continue"
)

// WorkflowSpec defines the desired state of Workflow
type WorkflowSpec struct {
	// Inputs defines input parameters for the workflow template.
	// Values are provided when creating a WorkflowRun.
	// +optional
	Inputs []Input `json:"inputs,omitempty"`

	// MCPTool exposes this workflow to MCP clients as a callable tool.
	// Omitted or disabled keeps the workflow off the MCP endpoint entirely.
	// +optional
	MCPTool *MCPTool `json:"mcpTool,omitempty"`

	// Variables defines top-level CEL expressions that are evaluated before steps execute.
	// Variables are shared across all steps and can reference inputs and other variables.
	// Variables are evaluated sequentially, allowing later variables to reference earlier ones.
	// Access variables in expressions using: variables.<name>
	// +optional
	Variables []Variable `json:"variables,omitempty"`

	// Steps defines the workflow steps to execute.
	// +kubebuilder:validation:MinItems=1
	Steps []Step `json:"steps"`

	// Outputs defines workflow-level outputs that are evaluated at completion
	// and added to the WorkflowRun status for observability
	// +optional
	Outputs []Output `json:"outputs,omitempty"`

	// Triggers defines automatic triggers for this workflow template
	// When triggers fire, they automatically create WorkflowRun instances
	// Multiple triggers can be defined (OR logic - any trigger can fire)
	// +optional
	Triggers []Trigger `json:"triggers,omitempty"`

	// Events configures Kubernetes event emission for this workflow.
	// +optional
	Events *EventConfig `json:"events,omitempty"`

	// Run configures retention and scale limits for WorkflowRuns of this workflow.
	// +optional
	Run *RunPolicy `json:"run,omitempty"`

	// CELCostLimit is the maximum cost budget for CEL expression evaluation in this workflow.
	// If unset, a default (~2MB-equivalent) is used. Increase for workflows that do large data joins.
	// +optional
	CELCostLimit *int64 `json:"celCostLimit,omitempty"`

	// Execution is the default execution spec for WorkflowRuns created from this Workflow (e.g. by cron).
	// When the scheduler creates a WorkflowRun, it copies this into the run's spec so runner Jobs get
	// the same env, volumes, etc. Use this to inject Nirmata LLM credentials (valueFrom.secretKeyRef).
	// +optional
	Execution *WorkflowRunExecutionSpec `json:"execution,omitempty"`

	// ExecutionLimits configures concurrency and rate limits for this workflow.
	// +optional
	ExecutionLimits *ExecutionLimits `json:"executionLimits,omitempty"`

	// Expose publishes this Workflow to external agent surfaces (e.g. kagent). Presence of expose.kagent opts in.
	// +optional
	Expose *ExposeSpec `json:"expose,omitempty"`
}

// ExposeSpec configures external surfaces a Workflow is published to. Currently only kagent.
type ExposeSpec struct {
	// Kagent, when set, opts the Workflow into kagent (agent-to-agent) exposure via a kagent BYO Agent.
	// +optional
	Kagent *KagentExposeSpec `json:"kagent,omitempty"`
}

// GetKagent returns the kagent exposure spec, or nil when the Workflow does not opt into
// kagent (A2A) exposure. Nil-safe on the pointer so callers can chain
// wf.Spec.Expose.GetKagent() without a preceding nil check, the same way MCPTool does.
func (e *ExposeSpec) GetKagent() *KagentExposeSpec {
	if e == nil {
		return nil
	}
	return e.Kagent
}

// IsKagentEnabled reports whether the Workflow opts into kagent (A2A) exposure. Nil is the
// common case (most workflows never opt in), so the nil check lives here rather than at each
// call site.
func (e *ExposeSpec) IsKagentEnabled() bool {
	return e.GetKagent() != nil
}

// KagentExposeSpec describes the kagent agent card metadata for a Workflow exposed via kagent.
type KagentExposeSpec struct {
	// DisplayName is a human-friendly name for the agent card. Defaults to the Workflow name.
	// +optional
	DisplayName string `json:"displayName,omitempty"`

	// Description is a human-readable description of what the agent does.
	// +optional
	Description string `json:"description,omitempty"`

	// Examples are sample prompts shown on the agent card.
	// +optional
	Examples []string `json:"examples,omitempty"`

	// Tags are labels shown on the agent card for discovery.
	// +optional
	Tags []string `json:"tags,omitempty"`
}

// ExecutionLimits configures per-workflow concurrency and rate limits.
type ExecutionLimits struct {
	// MaxConcurrentSteps is the maximum number of steps that may run concurrently in a single WorkflowRun.
	// When multiple steps are ready (DAG), only this many are started at once. Zero or nil means no limit.
	// +optional
	MaxConcurrentSteps *int32 `json:"maxConcurrentSteps,omitempty"`

	// OutboundRequestsPerMinute limits the rate of outbound calls (MCP, agent executor) per WorkflowRun.
	// When set, the executor waits as needed before each call. Zero or nil means no rate limit.
	// +optional
	OutboundRequestsPerMinute *int32 `json:"outboundRequestsPerMinute,omitempty"`
}

// RunPolicy configures retention and maximum run count for a workflow.
type RunPolicy struct {
	// RetentionMinutes is how long to keep completed WorkflowRuns (Succeeded or Failed).
	// Completed runs older than this are deleted. Zero or nil means no automatic deletion.
	// +optional
	RetentionMinutes int32 `json:"retentionMinutes,omitempty"`

	// MaxAllowed is the maximum number of WorkflowRuns to retain per workflow (completed runs).
	// When exceeded, oldest completed runs are deleted first. Zero or nil means no limit.
	// Pending and Running runs are not counted toward this limit when deciding deletions.
	// +optional
	MaxAllowed int32 `json:"maxAllowed,omitempty"`

	// MaxConcurrentRuns is the maximum number of WorkflowRuns that may be Pending or Running at once for this workflow.
	// When a trigger (cron, event) would create a new run, creation is skipped if active runs >= MaxConcurrentRuns.
	// Zero or nil means no limit.
	// +optional
	MaxConcurrentRuns *int32 `json:"maxConcurrentRuns,omitempty"`
}

// EventConfig configures event emission for workflow runs.
type EventConfig struct {
	// Enabled turns Kubernetes event emission on or off. When nil or true, events are emitted per Level.
	// When false, no events are emitted.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// Level controls which events are emitted when Enabled is true.
	// - Workflow: workflow-level only (WorkflowRunning, WorkflowSucceeded, WorkflowFailed, WorkflowRestarted)
	// - WorkflowAndSteps: workflow-level and step-level (WorkflowExecution for step started/succeeded/failed/skipped)
	// +kubebuilder:validation:Enum=Workflow;WorkflowAndSteps
	// +kubebuilder:default=WorkflowAndSteps
	// +optional
	Level string `json:"level,omitempty"`
}

// MCPTool exposes a workflow as a tool an MCP client can call, which lets an
// agent framework run it. Exposure is per workflow and opt-in: an endpoint
// that runs every workflow in the cluster is not something a workflow author
// should get by default.
type MCPTool struct {
	// Enabled exposes this workflow on the MCP endpoint. The endpoint itself
	// must also be running (the controller's --mcp-addr); this field decides
	// whether this workflow is one of the tools it serves.
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// Description is what an MCP client shows a model when it decides whether
	// to call this workflow. It is the whole basis for that decision, so write
	// it for that reader: what the workflow does, and when to reach for it.
	// Defaults to a sentence built from the workflow's name.
	// +optional
	Description string `json:"description,omitempty"`
}

// IsEnabled reports whether the workflow is exposed as an MCP tool. Nil is the
// common case — most workflows never opt in — so the nil check lives here
// rather than at each call site.
func (m *MCPTool) IsEnabled() bool {
	return m != nil && m.Enabled
}

// GetDescription returns the configured description, or empty when unset.
func (m *MCPTool) GetDescription() string {
	if m == nil {
		return ""
	}
	return m.Description
}

// Input defines a workflow input parameter
type Input struct {
	// Name is the input parameter name
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Description is a human-readable description of the input
	// +optional
	Description string `json:"description,omitempty"`

	// Default is the default value for the input (optional)
	// +optional
	Default string `json:"default,omitempty"`

	// Required indicates if the input must be provided
	// +optional
	Required bool `json:"required,omitempty"`
}

// Step defines a workflow step
type Step struct {
	// Name is the unique name for the step within the workflow
	// Must be camelCase (e.g., collectPodData, not collect-pod-data)
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-z][a-zA-Z0-9]*$`
	Name string `json:"name"`

	// Message is a human-readable description of what the step does
	// +optional
	Message string `json:"message,omitempty"`

	// Expressions are CEL expressions evaluated before step execution
	// +optional
	Expressions []Expression `json:"expressions,omitempty"`

	// Outputs defines key-value pairs written to shared context
	// +optional
	Outputs []Output `json:"outputs,omitempty"`

	// DependsOn explicitly declares step dependencies
	// Steps listed here must complete successfully before this step executes
	// If not specified, dependencies are inferred from output references in expressions
	// +optional
	DependsOn []string `json:"dependsOn,omitempty"`

	// MatchConditions defines conditional execution rules for this step
	// Similar to Kubernetes ValidatingAdmissionPolicy matchConditions
	// Step executes only if ALL conditions evaluate to true
	// If ANY condition evaluates to false, the step is skipped
	// If no matchConditions are specified, the step always executes
	// +optional
	MatchConditions []MatchCondition `json:"matchConditions,omitempty"`

	// Retry defines retry configuration for step execution
	// +optional
	Retry *RetryPolicy `json:"retry,omitempty"`

	// Timeout defines maximum duration for step execution (e.g., "30s", "5m", "1h")
	// Step fails if exceeded
	// +optional
	Timeout string `json:"timeout,omitempty"`

	// FailurePolicy determines workflow behavior on step failure
	// Fail: Step failure causes workflow to fail (default)
	// Continue: Step failure is logged but workflow continues to next step
	// +kubebuilder:validation:Enum=Fail;Continue
	// +kubebuilder:default=Fail
	// +optional
	FailurePolicy string `json:"failurePolicy,omitempty"`

	// WorkflowRef references another workflow to execute as a sub-workflow
	// When specified, this step executes the referenced workflow
	// Input values can be CEL expressions that reference parent workflow context
	// Sub-workflow outputs are accessible via outputs.<stepNameCamelCase>.<outputName>
	// +optional
	WorkflowRef *StepWorkflowRef `json:"workflowRef,omitempty"`

	// AgentRef references an Agent CRD for AI-powered step execution
	// When specified, this step uses an LLM agent to execute the task
	// The agent's prompt can reference workflow context (inputs, previous step outputs)
	// Agent outputs are accessible via outputs.<stepNameCamelCase>.<outputName>
	// +optional
	AgentRef *StepAgentRef `json:"agentRef,omitempty"`

	// MCPToolCall defines a direct MCP tool invocation
	// When specified, this step calls an MCP tool directly without LLM mediation
	// Tool arguments are resolved using CEL expressions with workflow context
	// Tool results are available as `toolResult` variable in output expressions
	// +optional
	MCPToolCall *StepMCPToolCall `json:"mcpToolCall,omitempty"`

	// ResourceQuery defines a simplified resource query that compiles to CEL expressions
	// When specified, this step queries Kubernetes resources using a simplified syntax
	// Supports both single resource queries (with name) and list queries (without name)
	// Outputs are automatically written to variables, consistent with Step.outputs behavior
	// +optional
	ResourceQuery *StepResourceQuery `json:"resourceQuery,omitempty"`

	// PrometheusQuery defines a Prometheus (PromQL) query step
	// When specified, this step runs a PromQL query with template variable substitution,
	// then evaluates outputs over the result (samples, value, etc.)
	// +optional
	PrometheusQuery *StepPrometheusQuery `json:"prometheusQuery,omitempty"`

	// Mutate defines a Kyverno-style mutate step to patch a single resource via CEL or JSONPatch
	// When specified, this step GETs the target resource, evaluates the patch (with "object" in CEL context), and applies it
	// +optional
	Mutate *StepMutate `json:"mutate,omitempty"`

	// StepTemplateRef references a StepTemplate to instantiate
	// When specified, this step instantiates the referenced StepTemplate with provided arguments
	// The template's step definition is expanded with parameter substitution
	// Template arguments are CEL expressions evaluated in the workflow context
	// +optional
	StepTemplateRef *StepTemplateRef `json:"stepTemplateRef,omitempty"`

	// ForEach defines parallel execution over a list of items
	// When specified, this step generates child steps dynamically for each item
	// Results are automatically collected and accessible via steps.<stepName>.results
	// +optional
	ForEach *StepForEach `json:"forEach,omitempty"`

	// ExternalAgentRef defines a call to an external A2A-compatible agent service
	// When specified, this step sends a task to the referenced external agent via A2A protocol
	// The task result is available as `a2aResult` in output CEL expressions
	// +optional
	ExternalAgentRef *StepExternalAgentRef `json:"externalAgentRef,omitempty"`

	// OpenReport defines an OpenReports.io report generation step.
	// When specified, this step emits workflow results as an OpenReports.io Report CRD.
	// If the OpenReports CRD is not installed, the step falls back to storing report data
	// in context as reportResult.data with a Warning event on the WorkflowRun.
	// Report results are available as `reportResult` in output CEL expressions.
	// +optional
	OpenReport *StepOpenReport `json:"openReport,omitempty"`

	// WaitForCallback pauses workflow execution and waits for an external callback.
	// Enables human-in-the-loop and AI-to-human-to-AI workflows.
	// When specified, this step generates a cryptographically secure token, stores it in
	// WorkflowRun.status.pendingCallback, and exits with code 0. The controller recreates
	// the runner Job when callback data is POSTed to the callback endpoint.
	// +optional
	WaitForCallback *WaitForCallbackStep `json:"waitForCallback,omitempty"`
}

// Variable defines a top-level workflow variable
// Variables are evaluated before steps execute and are shared across all steps
type Variable struct {
	// Name is the variable name (accessible as variables.<name> in CEL expressions)
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Expression is the CEL expression to evaluate
	// Variables can reference inputs and other variables (evaluated sequentially)
	// +kubebuilder:validation:Required
	Expression string `json:"expression"`
}

// Expression defines a CEL expression with a name
type Expression struct {
	// Name is the name to store the expression result
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Expression is the CEL expression to evaluate
	// +kubebuilder:validation:Required
	Expression string `json:"expression"`
}

// Output defines an output value written to shared context
type Output struct {
	// Name is the output key name
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Expression is the CEL expression that evaluates to the output value
	// Mutually exclusive with Value field. If both are specified, Value takes precedence.
	// +optional
	Expression string `json:"expression,omitempty"`

	// Value defines a native YAML structure where string values can contain CEL expressions
	// String values that look like CEL expressions (e.g., 'inputs.podName', 'variables.count')
	// are automatically evaluated. If evaluation fails, the literal string value is used.
	// Mutually exclusive with Expression field. If both are specified, Value takes precedence.
	// Stored as raw JSON bytes to support arbitrary YAML structures
	// +optional
	Value *apiextensionsv1.JSON `json:"value,omitempty"`

	// Metric optionally publishes this output's value to Prometheus.
	// Only used for workflow-level outputs (WorkflowSpec.Outputs).
	// The output's evaluated value becomes the metric value.
	// +optional
	Metric *OutputMetric `json:"metric,omitempty"`

	// Sensitive, when true, prevents writing the evaluated value to WorkflowRun status.
	// The value remains in in-memory context for subsequent steps/metrics where applicable,
	// but Status.Outputs will contain a redacted placeholder instead of the raw value.
	// Use for outputs that may contain secrets or PII.
	// +optional
	Sensitive bool `json:"sensitive,omitempty"`
}

// OutputMetric defines Prometheus metric publication for an output
type OutputMetric struct {
	// Name is the metric name (will be prefixed with ottoflow_workflow_)
	// Must be valid Prometheus metric name: [a-zA-Z_:][a-zA-Z0-9_:]*
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Type is the metric type: counter, gauge, or histogram
	// +kubebuilder:validation:Enum=counter;gauge;histogram
	// +kubebuilder:validation:Required
	Type string `json:"type"`

	// Help is the metric description
	// +optional
	Help string `json:"help,omitempty"`

	// Labels are optional label key-value pairs (CEL expressions for values)
	// +optional
	Labels []MetricLabel `json:"labels,omitempty"`

	// Buckets for histogram type (optional, uses default if not specified)
	// +optional
	Buckets []float64 `json:"buckets,omitempty"`
}

// MetricLabel defines a label for a custom metric
type MetricLabel struct {
	// Name is the label name
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Value is a CEL expression for the label value (must evaluate to string)
	// +kubebuilder:validation:Required
	Value string `json:"value"`
}

// MatchCondition defines a conditional execution rule for a step
// Similar to Kubernetes ValidatingAdmissionPolicy matchConditions
// All conditions must evaluate to true for the step to execute
type MatchCondition struct {
	// Name is a unique identifier for this match condition
	// Used for logging and visibility when conditions fail
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Expression is a CEL expression that evaluates to a boolean
	// Step executes only if ALL matchConditions evaluate to true
	// If ANY condition evaluates to false, the step is skipped
	// +kubebuilder:validation:Required
	Expression string `json:"expression"`
}

// StepWorkflowRef references another workflow to execute as a sub-workflow
type StepWorkflowRef struct {
	// Name is the name of the Workflow template to execute
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Namespace is the namespace of the Workflow template
	// Defaults to the parent workflow namespace if not specified
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Inputs provides input values for the referenced workflow
	// Keys match input names defined in the referenced Workflow template
	// Values are CEL expressions that are evaluated in the parent workflow context
	// +optional
	Inputs map[string]string `json:"inputs,omitempty"`
}

// StepAgentRef references an Agent CRD for AI-powered step execution
type StepAgentRef struct {
	// Name is the name of the Agent CRD to use
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Namespace is the namespace of the Agent CRD
	// Defaults to the parent workflow namespace if not specified
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// AdditionalPrompts is an optional list of prompts that are appended to the agent's system prompt
	// Each prompt can contain CEL expressions that are evaluated in the workflow context
	// The prompts are evaluated and concatenated with the agent's base prompt
	// Useful for adding step-specific context or instructions to the agent
	// +optional
	AdditionalPrompts []string `json:"additionalPrompts,omitempty"`

	// MaxAdditionalPromptTokens is an optional token budget for the combined additionalPrompts text.
	// When set, the evaluated additional prompt text is truncated to fit this budget, using a rough
	// heuristic of approximately 3 runes per token (code/YAML tokenizes denser than prose, so this
	// errs conservative). The agent's base prompt is not counted. Nil means no limit. 0 disables the limit.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=10000000
	// +optional
	MaxAdditionalPromptTokens *int32 `json:"maxAdditionalPromptTokens,omitempty"`

	// ContextBudgetMode controls how much of the accumulated step context is visible to
	// CEL evaluation when evaluating additionalPrompts. Applied before BuildVariableMap,
	// so no materialization cost is paid for entries that are filtered out.
	//
	// full: all step outputs are visible (default when omitted — preserves current behavior)
	// lastN: only the N most recently completed step outputs are visible
	// omit: no step outputs are visible
	//
	// Only the step outputs are filtered. Inputs, variables, and expressions remain fully
	// visible in every mode.
	//
	// +kubebuilder:validation:Enum=full;lastN;omit
	// +optional
	ContextBudgetMode string `json:"contextBudgetMode,omitempty"`

	// ContextBudgetLastN is the number of most-recently-completed steps to include when
	// ContextBudgetMode=lastN. Steps beyond the last N are omitted from CEL context.
	// N counts only completed steps that have an entry in the context's steps map (e.g. agent
	// steps that produced a response/output); steps with no steps-map entry are not counted and
	// do not consume a slot in the window.
	// Ignored for other modes. Defaults to 5 when omitted or zero.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1000
	// +optional
	ContextBudgetLastN *int32 `json:"contextBudgetLastN,omitempty"`
}

// StepExternalAgentRef defines a call to an external A2A-compatible agent service
type StepExternalAgentRef struct {
	// URL is the A2A agent card base URL (normally HTTPS, e.g. "https://kagent.example.com").
	// http:// is permitted only when allowInsecureHTTP is set, and only to cluster-local hosts.
	// +kubebuilder:validation:Required
	URL string `json:"url"`

	// Protocol is the agent communication protocol. Currently only "a2a" is supported.
	// +kubebuilder:validation:Enum=a2a
	// +kubebuilder:default=a2a
	// +optional
	Protocol string `json:"protocol,omitempty"`

	// Prompt is a CEL expression evaluated in the workflow context; the result is sent as the task message.
	// Can reference inputs, variables, and previous step outputs (e.g. '"Analyze: " + steps.prev.report').
	// +kubebuilder:validation:Required
	Prompt string `json:"prompt"`

	// Auth defines authentication for the external agent call
	// +optional
	Auth *ExternalAgentAuth `json:"auth,omitempty"`

	// CASecretRef references a Secret containing a CA bundle (key: ca.crt) for TLS verification.
	// When omitted, the system CA pool is used.
	// +optional
	CASecretRef *NamespacedSecretRef `json:"caSecretRef,omitempty"`

	// Timeout is the maximum duration to wait for task completion (e.g. "5m", "30s"). Default: 5m.
	// +optional
	Timeout string `json:"timeout,omitempty"`

	// AllowInsecureHTTP permits http:// URLs, but ONLY to cluster-local hosts
	// (a host ending in .svc or .svc.cluster.local, or exactly localhost / 127.0.0.1 / ::1).
	// http:// to any other host is rejected even when true. https:// ignores this flag.
	// Must NOT be combined with auth.secretRef (a bearer token would be sent in cleartext).
	// Must also NOT be combined with caSecretRef (a CA bundle has no effect over plaintext http).
	// +optional
	AllowInsecureHTTP bool `json:"allowInsecureHTTP,omitempty"`
}

// ExternalAgentAuth defines bearer-token authentication for an external agent call
type ExternalAgentAuth struct {
	// SecretRef references a Secret containing the bearer token.
	// The Key field of SecretRef specifies which key in the Secret holds the token value.
	// +optional
	SecretRef *SecretReference `json:"secretRef,omitempty"`
}

// StepMCPToolCall defines a direct MCP tool invocation
type StepMCPToolCall struct {
	// Server is the name of the MCPServer CRD to use
	// +kubebuilder:validation:Required
	Server string `json:"server"`

	// Tool is the name of the tool within the MCP server
	// +kubebuilder:validation:Required
	Tool string `json:"tool"`

	// Arguments are the tool arguments
	// Keys are argument names, values are CEL expressions evaluated in workflow context
	// +optional
	Arguments map[string]string `json:"arguments,omitempty"`
}

// StepResourceQuery defines a simplified resource query that compiles to CEL expressions
type StepResourceQuery struct {
	// APIVersion is the API version of the resource (e.g., "v1", "apps/v1")
	// +kubebuilder:validation:Required
	APIVersion string `json:"apiVersion"`

	// Resource is the kind of the resource (e.g., "Pod", "Deployment", "Service")
	// +kubebuilder:validation:Required
	Resource string `json:"resource"`

	// Namespace is the namespace to query (can be a CEL expression)
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Name is the name of a specific resource (for single resource queries)
	// If omitted, performs a list query
	// Can be a CEL expression evaluated in workflow context
	// +optional
	Name string `json:"name,omitempty"`

	// LabelSelector is a map of label key-value pairs for filtering list queries
	// Keys are label names, values are CEL expressions evaluated in workflow context
	// Only used when Name is omitted (list queries)
	// +optional
	LabelSelector map[string]string `json:"labelSelector,omitempty"`

	// FieldSelector filters list queries. It is always evaluated as a CEL expression in
	// workflow context and must yield a field selector string, so a literal selector has
	// to be quoted inside the expression: '"status.phase=Running"'. An unquoted
	// status.phase=Running is not valid CEL and fails at runtime.
	// Only used when Name is omitted (list queries)
	// +optional
	FieldSelector string `json:"fieldSelector,omitempty"`

	// Limit caps the number of resources returned for list queries.
	// Resources are fetched in pages of 500; collection stops once this many
	// items have been accumulated. When 0 (default) all pages are fetched.
	// Use this to bound memory usage when querying large clusters.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Limit int64 `json:"limit,omitempty"`

	// PageSize controls how many resources are fetched per API call during list pagination.
	// Defaults to 500 when unset. Reduce this for resource-heavy types (large pod specs,
	// CRDs with large status fields) to keep individual API responses small and reduce
	// the chance of per-page timeouts on loaded API servers.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000
	PageSize int64 `json:"pageSize,omitempty"`

	// Outputs defines the outputs to extract from the resource(s)
	// Keys are output names (written to variables), values are CEL expressions
	// For single resource queries: expressions reference "object", the fetched resource
	//   (e.g., "object.status.phase"). Note "resource" is the CEL macro namespace
	//   (resource.Get/resource.List) and does not support field selection.
	// For list queries: expressions reference "items" (the list) (e.g., "items.map(i, i.metadata.name)")
	//
	// A step's own step-level outputs are evaluated after these and can reference them as
	// variables.<name>. On a name collision the step-level output wins.
	// +kubebuilder:validation:Required
	Outputs map[string]string `json:"outputs"`
}

// StepPrometheusQuery defines a Prometheus (PromQL) query step
type StepPrometheusQuery struct {
	// Query is the PromQL expression. May contain {{.varName}} placeholders substituted from Variables.
	// +kubebuilder:validation:Required
	Query string `json:"query"`

	// TimeRange is the lookback duration for the instant query (e.g., "7d", "1h", "5m").
	// The query is executed at (now - timeRange).
	// +kubebuilder:validation:Required
	TimeRange string `json:"timeRange"`

	// Step is the query resolution step (optional). Reserved for future range-query support.
	// +optional
	Step string `json:"step,omitempty"`

	// Variables provides values for {{.varName}} placeholders in Query.
	// Keys are placeholder names, values are CEL expressions evaluated in workflow context.
	// +optional
	Variables map[string]string `json:"variables,omitempty"`

	// Outputs defines the outputs to extract from the Prometheus result.
	// Keys are output names (written to variables), values are CEL expressions.
	// Expressions can reference "result" with fields: type ("vector"|"scalar"), samples (list of {metric, value, timestamp}), value (scalar).
	// +optional
	Outputs map[string]string `json:"outputs,omitempty"`
}

// StepMutate defines a mutate step that patches a single Kubernetes resource (Kyverno-style)
type StepMutate struct {
	// Target identifies the resource to mutate
	Target StepMutateTarget `json:"target"`

	// PatchType is the type of patch to apply
	// ApplyConfiguration: CEL expression returns a partial object merged onto the resource (merge patch)
	// JSONPatch: CEL expression returns a list of RFC 6902 operations, or use Operations for a static list
	// +kubebuilder:validation:Enum=ApplyConfiguration;JSONPatch
	// +kubebuilder:validation:Required
	PatchType string `json:"patchType"`

	// ApplyConfiguration defines the patch when patchType is ApplyConfiguration.
	// Expression is evaluated with "object" (the current resource) and workflow context; must return a map/object to merge.
	// +optional
	ApplyConfiguration *MutateApplyConfiguration `json:"applyConfiguration,omitempty"`

	// JSONPatch defines the patch when patchType is JSONPatch.
	// Either Expression (CEL returning a list of {op, path, value?}) or Operations (static list) can be set.
	// +optional
	JSONPatch *MutateJSONPatch `json:"jsonPatch,omitempty"`

	// Outputs defines outputs to extract after the mutation (e.g. "resource" for the patched object)
	// Keys are output names, values are CEL expressions; "object" refers to the patched resource
	// +optional
	Outputs map[string]string `json:"outputs,omitempty"`
}

// StepMutateTarget identifies the resource to mutate
type StepMutateTarget struct {
	// APIVersion is the API version of the resource (e.g., "v1", "apps/v1")
	// +kubebuilder:validation:Required
	APIVersion string `json:"apiVersion"`

	// Resource is the kind of the resource (e.g., "Pod", "Deployment", "ConfigMap")
	// +kubebuilder:validation:Required
	Resource string `json:"resource"`

	// Namespace is the namespace of the resource; CEL expression evaluated in workflow context. Omit for cluster-scoped.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Name is the name of the resource; CEL expression evaluated in workflow context
	// +kubebuilder:validation:Required
	Name string `json:"name"`
}

// MutateApplyConfiguration holds a CEL expression that returns the patch object for merge
type MutateApplyConfiguration struct {
	// Expression is a CEL expression evaluated with "object" (current resource) and workflow context.
	// Must return a map/object that will be deep-merged onto the resource.
	// +kubebuilder:validation:Required
	Expression string `json:"expression"`
}

// MutateJSONPatch holds either a CEL expression or a static list of RFC 6902 JSON Patch operations
type MutateJSONPatch struct {
	// Expression is a CEL expression that returns a list of patch operations.
	// Each element must be a map with "op" (add|remove|replace|move|copy|test), "path" (JSON Pointer), and optionally "value" or "from".
	// Evaluated with "object" (current resource) and workflow context.
	// +optional
	Expression string `json:"expression,omitempty"`

	// Operations is a static list of JSON Patch operations when Expression is not set
	// +optional
	Operations []MutateJSONPatchOp `json:"operations,omitempty"`
}

// MutateJSONPatchOp represents one RFC 6902 JSON Patch operation
type MutateJSONPatchOp struct {
	// Op is the operation: add, remove, replace, move, copy, or test
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=add;remove;replace;move;copy;test
	Op string `json:"op"`

	// Path is the JSON Pointer to the target location
	// +kubebuilder:validation:Required
	Path string `json:"path"`

	// Value is the value for add/replace/test (optional for remove, use From for move/copy)
	// +optional
	Value *apiextensionsv1.JSON `json:"value,omitempty"`

	// From is the source path for move/copy operations
	// +optional
	From string `json:"from,omitempty"`
}

// Trigger defines an automatic trigger for workflow execution
type Trigger struct {
	// Cron defines a cron-based trigger
	// +optional
	Cron *CronTrigger `json:"cron,omitempty"`

	// Event defines a Kubernetes event-based trigger
	// +optional
	Event *EventTrigger `json:"event,omitempty"`

	// Webhook defines an HTTP-based trigger that fires a WorkflowRun when a signed
	// POST request is received at /webhooks/{namespace}/{workflowName}.
	// +optional
	Webhook *WebhookTrigger `json:"webhook,omitempty"`
}

// WebhookTrigger defines an HTTP-based trigger that fires a WorkflowRun
// when a signed POST request is received at /webhooks/{namespace}/{workflowName}.
type WebhookTrigger struct {
	// SecretRef references a Kubernetes Secret containing the HMAC signing key.
	// The Secret must have a key named "hmac-key" (or the value of Key) whose value
	// is the shared HMAC-SHA256 secret. Minimum 32 bytes.
	// +required
	SecretRef WebhookSecretRef `json:"secretRef"`

	// CELFilter is an optional CEL boolean expression evaluated against the parsed
	// request body (available as `object`). If false or the expression errors,
	// the request is acknowledged (200) but no WorkflowRun is created.
	// +optional
	CELFilter string `json:"celFilter,omitempty"`

	// InputMapping maps workflow input names to CEL expressions evaluated against
	// the parsed JSON body (available as `object`). Results are coerced to strings.
	// If omitted, no inputs are passed to the WorkflowRun.
	// +optional
	InputMapping map[string]string `json:"inputMapping,omitempty"`

	// DedupKey is a CEL expression evaluated against the request body to extract
	// a deduplication key. Requests with the same key within DedupWindow are dropped.
	// +optional
	DedupKey string `json:"dedupKey,omitempty"`

	// DedupWindow is the time window for deduplication when DedupKey is set.
	// Defaults to 10 minutes if DedupKey is set and DedupWindow is omitted.
	// Maximum 1 hour — enforced by the admission webhook validator because the kubebuilder
	// validation:Maximum marker does not apply to *metav1.Duration fields (the type marshals
	// as a string like "10m", not a numeric value). Use "1h" or shorter; longer windows may
	// cause excessive memory usage in the dedup cache.
	// +optional
	DedupWindow *metav1.Duration `json:"dedupWindow,omitempty"`

	// RateLimit configures per-workflow rate limiting on inbound webhook requests.
	// +optional
	RateLimit *WebhookRateLimit `json:"rateLimit,omitempty"`
}

// WebhookSecretRef references a Kubernetes Secret containing the HMAC signing key.
type WebhookSecretRef struct {
	// Name of the Kubernetes Secret.
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`

	// Namespace of the Secret. In v1, must equal the Workflow's namespace.
	// Cross-namespace references are rejected by the admission webhook.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Key is the data key within the Secret that holds the HMAC signing key.
	// +kubebuilder:default=hmac-key
	// +optional
	Key string `json:"key,omitempty"`
}

// WebhookRateLimit configures per-workflow rate limiting for webhook requests.
type WebhookRateLimit struct {
	// RequestsPerMinute is the maximum number of accepted requests per minute.
	// +kubebuilder:default=60
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=3600
	// +optional
	RequestsPerMinute int `json:"requestsPerMinute,omitempty"`

	// Burst is the maximum number of requests allowed in a short burst above the
	// per-minute average. Accommodates retry storms (e.g. GitHub Actions 3-retry policy).
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +optional
	Burst int `json:"burst,omitempty"`
}

// CronTrigger defines a cron-based trigger
type CronTrigger struct {
	// Schedule is a cron expression (e.g., "0 0 * * *" for daily at midnight)
	// +kubebuilder:validation:Required
	Schedule string `json:"schedule"`

	// Timezone is the timezone for the cron schedule (e.g., "America/New_York")
	// Defaults to UTC if not specified
	// +optional
	Timezone string `json:"timezone,omitempty"`

	// ConcurrencyPolicy determines how to handle concurrent executions
	// Allow: Allow concurrent runs
	// Forbid: Skip if previous run is active (default)
	// Replace: Cancel previous run and start new one
	// +kubebuilder:validation:Enum=Allow;Forbid;Replace
	// +kubebuilder:default=Forbid
	// +optional
	ConcurrencyPolicy string `json:"concurrencyPolicy,omitempty"`

	// StartingDeadlineSeconds is an optional deadline in seconds for starting the workflow
	// if it misses its scheduled time
	// +optional
	StartingDeadlineSeconds *int64 `json:"startingDeadlineSeconds,omitempty"`

	// InputValuesFrom injects input values from Secrets when the scheduler creates a WorkflowRun.
	// Each entry maps one workflow input name to a secret key. Secret is read in the workflow's
	// namespace (or the specified namespace). Use for e.g. slackWebhookUrl from a Secret.
	// +optional
	InputValuesFrom []CronInputFromSecret `json:"inputValuesFrom,omitempty"`
}

// CronInputFromSecret maps a workflow input name to a secret key.
type CronInputFromSecret struct {
	// InputName is the workflow input parameter name (e.g. slackWebhookUrl).
	// +kubebuilder:validation:Required
	InputName string `json:"inputName"`
	// SecretRef references the secret and key.
	// +kubebuilder:validation:Required
	SecretRef CronSecretKeyRef `json:"secretRef"`
}

// CronSecretKeyRef references a key in a Secret.
type CronSecretKeyRef struct {
	// Name is the Secret name.
	// +kubebuilder:validation:Required
	Name string `json:"name"`
	// Namespace is the Secret namespace. If empty, the workflow's namespace is used.
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// Key is the secret data key.
	// +kubebuilder:validation:Required
	Key string `json:"key"`
}

// EventTrigger defines a Kubernetes event-based trigger
type EventTrigger struct {
	// Resources defines the resources to watch for events
	// +kubebuilder:validation:Required
	Resources []EventResource `json:"resources"`

	// Operations defines the operations to trigger on (CREATE, UPDATE, DELETE)
	// If empty, all operations trigger
	// +optional
	Operations []string `json:"operations,omitempty"`

	// LabelSelector is a label selector to filter resources
	// +optional
	LabelSelector *metav1.LabelSelector `json:"labelSelector,omitempty"`

	// FieldSelector is a field selector to filter resources
	// +optional
	FieldSelector string `json:"fieldSelector,omitempty"`

	// InputMapping maps event data to workflow input values
	// Keys are workflow input names, values are CEL expressions evaluated on the event object.
	// Available variable: object (the triggering resource as a dynamic map).
	// Example: appName: 'object.metadata.name'
	// +optional
	InputMapping map[string]string `json:"inputMapping,omitempty"`

	// CELFilter is a CEL expression evaluated against the event object before creating a WorkflowRun.
	// Must return bool. Events where the filter returns false or errors are dropped.
	// Available variable: object (the triggering resource as a dynamic map).
	// Example: 'object.status.sync.status == "Synced"'
	// +optional
	CELFilter string `json:"celFilter,omitempty"`

	// DedupKey is an optional CEL expression override for the deduplication key.
	// By default, OttoFlow auto-detects the revision field for known GitOps controllers
	// (ArgoCD Application, FluxCD Kustomization/HelmRelease). Only set this for controllers
	// not in the built-in list (e.g. Rancher Fleet: 'object.status.commit').
	// Available variable: object (the triggering resource as a dynamic map).
	// +optional
	DedupKey string `json:"dedupKey,omitempty"`

	// DedupWindow is the fallback deduplication window used when no revision field
	// is auto-detected and DedupKey is not set. Events for the same object (matched
	// by UID) within this window after the last WorkflowRun creation are dropped.
	// Defaults to 10 minutes if omitted; set to suppress repeat events on a single
	// flapping object (e.g. a Pod cycling through the same status repeatedly).
	// This field only dedupes repeat events for an object already seen — it does
	// not, and structurally cannot, bound the number of runs created by triggers
	// that observe a stream of distinct new objects (e.g. a Kind: WorkflowRun
	// trigger watching its own runs' Pods/Jobs), since each such object is a
	// first-sight miss against dedup state and is never suppressed by it. Use
	// labelSelector to exclude OttoFlow-managed objects and/or MaxConcurrentRuns on
	// the Workflow to bound overall run volume for that case.
	// +optional
	DedupWindow *metav1.Duration `json:"dedupWindow,omitempty"`
}

// EventResource defines a resource type to watch for events
type EventResource struct {
	// APIVersion is the API version of the resource (e.g., "apps/v1")
	// +kubebuilder:validation:Required
	APIVersion string `json:"apiVersion"`

	// Kind is the kind of the resource (e.g., "Deployment")
	// +kubebuilder:validation:Required
	Kind string `json:"kind"`

	// Namespace is the namespace to watch (empty string for cluster-scoped resources)
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// StepForEach defines parallel execution over a list of items
type StepForEach struct {
	// Items is a CEL expression that evaluates to a list
	// Each item in the list will be processed by a child step
	// +kubebuilder:validation:Required
	Items string `json:"items"`

	// ItemVariable is the variable name to use for the current item in child steps
	// Default: "item"
	// +optional
	ItemVariable string `json:"itemVariable,omitempty"`

	// Step defines the step to execute for each item (inline definition)
	// Mutually exclusive with StepTemplateRef
	// +optional
	Step *StepForEachStep `json:"step,omitempty"`

	// StepTemplateRef references a StepTemplate to use for each item
	// Mutually exclusive with Step
	// +optional
	StepTemplateRef *StepForEachTemplateRef `json:"stepTemplateRef,omitempty"`

	// MaxConcurrency limits the number of concurrent child step executions
	// Default: 5
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxConcurrency int `json:"maxConcurrency,omitempty"`

	// ItemFailurePolicy determines behavior when a child step fails
	// Continue: Continue processing other items, collect failures
	// Fail: Fail the forEach step (and workflow) on first failure
	// Default: Continue
	// +kubebuilder:validation:Enum=Continue;Fail
	// +optional
	ItemFailurePolicy string `json:"itemFailurePolicy,omitempty"`
}

// StepForEachStep defines an inline step definition for forEach execution
type StepForEachStep struct {
	// Expressions are CEL expressions evaluated before step execution
	// Can reference the current item via itemVariable (default: "item")
	// +optional
	Expressions []Expression `json:"expressions,omitempty"`

	// Outputs defines key-value pairs written to shared context
	// Can reference the current item via itemVariable (default: "item")
	// +optional
	Outputs []Output `json:"outputs,omitempty"`

	// MatchConditions defines conditional execution rules for this step
	// +optional
	MatchConditions []MatchCondition `json:"matchConditions,omitempty"`

	// Retry defines retry configuration for step execution
	// +optional
	Retry *RetryPolicy `json:"retry,omitempty"`

	// Timeout defines maximum duration for step execution (e.g., "30s", "5m", "1h")
	// +optional
	Timeout string `json:"timeout,omitempty"`

	// FailurePolicy determines behavior on step failure
	// +kubebuilder:validation:Enum=Fail;Continue
	// +optional
	FailurePolicy string `json:"failurePolicy,omitempty"`

	// ResourceQuery defines a simplified resource query
	// +optional
	ResourceQuery *StepResourceQuery `json:"resourceQuery,omitempty"`

	// PrometheusQuery defines a Prometheus (PromQL) query step
	// +optional
	PrometheusQuery *StepPrometheusQuery `json:"prometheusQuery,omitempty"`

	// Mutate defines a mutate step to patch a single resource via CEL or JSONPatch
	// +optional
	Mutate *StepMutate `json:"mutate,omitempty"`

	// AgentRef references an Agent CRD for AI-powered step execution
	// +optional
	AgentRef *StepAgentRef `json:"agentRef,omitempty"`

	// MCPToolCall defines a direct MCP tool invocation
	// +optional
	MCPToolCall *StepMCPToolCall `json:"mcpToolCall,omitempty"`

	// WorkflowRef references another workflow to execute as a sub-workflow
	// +optional
	WorkflowRef *StepWorkflowRef `json:"workflowRef,omitempty"`

	// ExternalAgentRef defines a call to an external A2A-compatible agent service
	// +optional
	ExternalAgentRef *StepExternalAgentRef `json:"externalAgentRef,omitempty"`

	// OpenReport defines an OpenReports.io report generation step
	// +optional
	OpenReport *StepOpenReport `json:"openReport,omitempty"`
}

// StepForEachTemplateRef references a StepTemplate to use for each item
type StepForEachTemplateRef struct {
	// Name is the name of the StepTemplate to use
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Namespace is the namespace of the StepTemplate
	// Defaults to the parent workflow namespace if not specified
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Arguments provides template arguments
	// Keys match parameter names defined in the StepTemplate
	// Values are CEL expressions evaluated in workflow context
	// Can reference the current item via itemVariable (default: "item")
	// +optional
	Arguments map[string]string `json:"arguments,omitempty"`
}

// RetryPolicy defines retry configuration for step execution
type RetryPolicy struct {
	// Attempts is the maximum number of retry attempts (including initial attempt)
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	// +optional
	Attempts int `json:"attempts,omitempty"`

	// Backoff defines the backoff strategy for retries
	// +optional
	Backoff *BackoffConfig `json:"backoff,omitempty"`

	// RetryOn defines conditions that trigger a retry
	// If empty, retry on all errors
	// +optional
	RetryOn []RetryCondition `json:"retryOn,omitempty"`
}

// BackoffConfig defines backoff strategy for retries
type BackoffConfig struct {
	// Strategy is the backoff strategy
	// none: immediate retry
	// linear: fixed interval
	// exponential: exponential backoff
	// +kubebuilder:validation:Enum=none;linear;exponential
	// +kubebuilder:default=exponential
	// +optional
	Strategy string `json:"strategy,omitempty"`

	// InitialInterval is the initial wait time before first retry (e.g., "1s", "100ms")
	// +kubebuilder:default="1s"
	// +optional
	InitialInterval string `json:"initialInterval,omitempty"`

	// MaxInterval is the maximum wait time between retries (e.g., "5m", "30s")
	// +kubebuilder:default="5m"
	// +optional
	MaxInterval string `json:"maxInterval,omitempty"`

	// Multiplier is the multiplier for exponential backoff (only used if strategy is exponential)
	// +kubebuilder:default=2.0
	// +optional
	Multiplier float64 `json:"multiplier,omitempty"`
}

// RetryCondition defines conditions that trigger a retry
type RetryCondition struct {
	// ErrorType is the error type to retry on (e.g., "NetworkError", "TimeoutError", "TransientError")
	// +optional
	ErrorType string `json:"errorType,omitempty"`

	// HTTPStatus is an array of HTTP status codes to retry on (e.g., [500, 502, 503, 504])
	// +optional
	HTTPStatus []int `json:"httpStatus,omitempty"`

	// ErrorMessage is an error message pattern to match (regex)
	// +optional
	ErrorMessage string `json:"errorMessage,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:shortName=flo

// Workflow is the Schema for the workflows API
// Workflow is an immutable template that defines steps, inputs, and optional triggers.
// It has no execution status - it acts as a reusable blueprint.
type Workflow struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec WorkflowSpec `json:"spec,omitempty"`
	// Status is intentionally omitted - Workflow is a template without execution state
}

//+kubebuilder:object:root=true

// WorkflowList contains a list of Workflow
type WorkflowList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Workflow `json:"items"`
}

func init() {
	objectTypes = append(objectTypes, &Workflow{}, &WorkflowList{})
}
