# API Reference

## Packages
- [ottoflow.nirmata.io/v1alpha1](#ottoflownirmataiov1alpha1)


## ottoflow.nirmata.io/v1alpha1

Package v1alpha1 contains API Schema definitions for the ottoflow v1alpha1 API group

### Resource Types
- [Agent](#agent)
- [MCPServer](#mcpserver)
- [StepTemplate](#steptemplate)
- [Workflow](#workflow)
- [WorkflowRun](#workflowrun)



#### Agent



Agent is the Schema for the agents API
Agent defines a reusable AI agent configuration for workflow steps





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `ottoflow.nirmata.io/v1alpha1` | | |
| `kind` _string_ | `Agent` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.29/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[AgentSpec](#agentspec)_ |  |  |  |
| `status` _[AgentStatus](#agentstatus)_ |  |  |  |


#### AgentPhase

_Underlying type:_ _string_

AgentPhase represents the phase of an Agent

_Validation:_
- Enum: [Ready NotReady]

_Appears in:_
- [AgentStatus](#agentstatus)

| Field | Description |
| --- | --- |
| `Ready` | AgentPhaseReady indicates the agent is ready to use<br /> |
| `NotReady` | AgentPhaseNotReady indicates the agent is not ready<br /> |


#### AgentSpec



AgentSpec defines the desired state of Agent



_Appears in:_
- [Agent](#agent)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `prompt` _string_ | Prompt is the system prompt for the agent<br />This is a static prompt that defines the agent's role and base instructions<br />For dynamic content, use additionalPrompts in the workflow step's agentRef |  | Required: \{\} <br /> |
| `modelProvider` _string_ | ModelProvider is the LLM provider to use. This field is required.<br />Options: nirmata, openai, anthropic, azure-openai, google, gemini, local |  | Enum: [nirmata openai anthropic azure-openai google gemini local] <br />Required: \{\} <br /> |
| `modelName` _string_ | ModelName is the specific model to use (e.g., "gpt-4", "claude-3-opus")<br />Default depends on provider |  | Optional: \{\} <br /> |
| `mcpTools` _string array_ | MCPTools is a list of MCP tools the agent can use<br />Each tool is specified as "server:tool" (e.g., "kubernetes-mcp:get-resource") |  | Optional: \{\} <br /> |
| `outputExtraction` _[OutputExtraction](#outputextraction)_ | OutputExtraction defines how to extract outputs from agent responses |  | Optional: \{\} <br /> |
| `serviceAccount` _string_ | ServiceAccount is the Kubernetes service account to use for agent execution<br />Used for RBAC and authentication |  | Optional: \{\} <br /> |
| `executorImage` _string_ | ExecutorImage is a custom container image for agent execution<br />If not specified, uses the default executor image (ghcr.io/nirmata/ottoflow/agent-executor:latest) |  | Optional: \{\} <br /> |
| `serviceName` _string_ | ServiceName is the name of the AgentExecutor Service<br />If not specified, uses default: ottoflow-agent-executor |  | Optional: \{\} <br /> |
| `serviceNamespace` _string_ | ServiceNamespace is the namespace of the AgentExecutor Service<br />Defaults to ottoflow |  | Optional: \{\} <br /> |
| `resources` _[ResourceRequirements](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.29/#resourcerequirements-v1-core)_ | Resources defines resource requests/limits for agent execution<br />Currently used for future sandbox mode<br />All steps using this agent use these resources |  | Optional: \{\} <br /> |
| `config` _object (keys:string, values:string)_ | Config contains provider-specific client options. The executor reads exactly<br />two keys (internal/agent/default_executor.go):<br />- endpoint: custom base URL passed to gollm as ClientOptions.URL; with the pinned<br />gollm honored ONLY by modelProvider "azure-openai" (overrides AZURE_OPENAI_ENDPOINT);<br />"openai" reads OPENAI_ENDPOINT/OPENAI_API_BASE from the process environment instead;<br />"google", "gemini" and "anthropic" ignore it; "local" uses LLAMACPP_HOST.<br />- skipVerifySSL: "true" disables TLS verification; honored by openai, anthropic,<br />azure-openai and local.<br />API keys are NOT read from here; they come from the agent-executor process<br />environment (OPENAI_API_KEY, ANTHROPIC_API_KEY, GEMINI_API_KEY, AZURE_OPENAI_API_KEY). |  | Optional: \{\} <br /> |


#### AgentStatus



AgentStatus defines the observed state of Agent



_Appears in:_
- [Agent](#agent)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `phase` _[AgentPhase](#agentphase)_ | Phase represents the current phase of the Agent |  | Enum: [Ready NotReady] <br />Optional: \{\} <br /> |
| `message` _string_ | Message provides additional information about the agent status |  | Optional: \{\} <br /> |
| `lastChecked` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.29/#time-v1-meta)_ | LastChecked is when the agent configuration was last validated |  | Optional: \{\} <br /> |


#### AuthConfig



AuthConfig defines authentication configuration for MCP server.
Credentials must be provided via SecretRef (or OAuth2 secret refs); inline credentials are not supported.



_Appears in:_
- [MCPServerSpec](#mcpserverspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `type` _string_ | Type is the authentication type<br />Options: none, bearer, apiKey, basic, oauth2 | none | Enum: [none bearer apiKey basic oauth2] <br />Optional: \{\} <br /> |
| `secretRef` _[SecretReference](#secretreference)_ | SecretRef references a Kubernetes Secret containing credentials.<br />Required for bearer, apiKey, and basic auth. For bearer/apiKey the secret key holds the token.<br />For basic auth the secret must contain keys "username" and "password". |  | Optional: \{\} <br /> |
| `oauth2` _[OAuth2Config](#oauth2config)_ | OAuth2 config for OAuth 2.0/2.1 client credentials flow (machine-to-machine)<br />Used when type is oauth2 |  | Optional: \{\} <br /> |


#### BackoffConfig



BackoffConfig defines backoff strategy for retries



_Appears in:_
- [RetryPolicy](#retrypolicy)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `strategy` _string_ | Strategy is the backoff strategy<br />none: immediate retry<br />linear: fixed interval<br />exponential: exponential backoff | exponential | Enum: [none linear exponential] <br />Optional: \{\} <br /> |
| `initialInterval` _string_ | InitialInterval is the initial wait time before first retry (e.g., "1s", "100ms") | 1s | Optional: \{\} <br /> |
| `maxInterval` _string_ | MaxInterval is the maximum wait time between retries (e.g., "5m", "30s") | 5m | Optional: \{\} <br /> |
| `multiplier` _float_ | Multiplier is the multiplier for exponential backoff (only used if strategy is exponential) | 2 | Optional: \{\} <br /> |


#### CallbackState



CallbackState represents the pending state of a waitForCallback step.
Stored in WorkflowRun.status.pendingCallback.



_Appears in:_
- [WorkflowRunStatus](#workflowrunstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `tokenHash` _string_ | TokenHash is the SHA256 hex digest of the callback token (64 lowercase hex chars).<br />The plaintext token is never stored in status; it is only available in the step's<br />in-memory output context (key: "callbackToken") and in controller logs.<br />Storing the hash prevents token theft by any principal with get/list workflowruns RBAC. |  |  |
| `stepName` _string_ | StepName is the name of the step waiting for the callback. |  |  |
| `expiresAt` _integer_ | ExpiresAt is the deadline for the callback (step timeout).<br />If the callback is not received by this time, the step fails. |  |  |
| `outputs` _[JSON](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.29/#json-v1-apiextensions-k8s-io)_ | Outputs contains the callback payload once received.<br />Initially empty; populated when the callback arrives. |  | Optional: \{\} <br /> |
| `createdAt` _integer_ | CreatedAt is the timestamp when the callback token was generated. |  | Optional: \{\} <br /> |


#### CheckpointingConfig



CheckpointingConfig configures per-step checkpointing for crash recovery.

Known limitations:
  - Checkpoint data is stored in a ConfigMap (plain-text, not encrypted). Avoid enabling
    on workflows that handle secrets or PII in step outputs until Secret storage is added.
  - ForEach steps are NOT checkpointed at the inner-item level. If a pod crashes
    mid-ForEach, all inner items replay from the beginning on resume. Enabling
    checkpointing on workflows with ForEach steps is safe but provides no partial-progress
    guarantee inside the ForEach. A Warning is logged when this combination is detected.



_Appears in:_
- [WorkflowRunExecutionSpec](#workflowrunexecutionspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `enabled` _boolean_ | Enabled turns on per-step checkpointing. After each step completes, the executor<br />writes a checkpoint ConfigMap so the run can resume after a transient pod crash.<br />Default: false — preserves existing behavior for all existing workflows. |  |  |
| `maxRestartAttempts` _integer_ | MaxRestartAttempts is the maximum number of times the controller will create a new<br />runner Job after a transient pod failure. Deterministic failures (OOMKilled) never<br />retry regardless of this value.<br />Default: 3. |  | Maximum: 20 <br />Minimum: 0 <br />Optional: \{\} <br /> |


#### ClusterRef



ClusterRef indicates which cluster to use for workflow execution.
Exactly one cluster source should be set when ClusterRef is present.



_Appears in:_
- [WorkflowRunSpec](#workflowrunspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `local` _boolean_ | Local, when true, uses the in-cluster configuration (the cluster where the controller runs).<br />Use this to explicitly target the local (controller) cluster when ClusterRef is set. |  | Optional: \{\} <br /> |
| `kubeConfigSecretRef` _[KubeConfigSecretRef](#kubeconfigsecretref)_ | KubeConfigSecretRef references a Secret containing a kubeconfig file. The workflow<br />runs against the cluster defined in that kubeconfig. Secret data key defaults to<br />"config", "kubeconfig", or "value" if Key is empty. Namespace defaults to the<br />WorkflowRun namespace if empty. |  | Optional: \{\} <br /> |
| `kubeConfigFilePath` _string_ | KubeConfigFilePath points to a kubeconfig file mounted into the runner pod.<br />This is intended for Secret, projected, or CSI volume mounts. |  | Optional: \{\} <br /> |


#### CronInputFromSecret



CronInputFromSecret maps a workflow input name to a secret key.



_Appears in:_
- [CronTrigger](#crontrigger)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `inputName` _string_ | InputName is the workflow input parameter name (e.g. slackWebhookUrl). |  | Required: \{\} <br /> |
| `secretRef` _[CronSecretKeyRef](#cronsecretkeyref)_ | SecretRef references the secret and key. |  | Required: \{\} <br /> |


#### CronSecretKeyRef



CronSecretKeyRef references a key in a Secret.



_Appears in:_
- [CronInputFromSecret](#croninputfromsecret)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the Secret name. |  | Required: \{\} <br /> |
| `namespace` _string_ | Namespace is the Secret namespace. If empty, the workflow's namespace is used. |  | Optional: \{\} <br /> |
| `key` _string_ | Key is the secret data key. |  | Required: \{\} <br /> |


#### CronTrigger



CronTrigger defines a cron-based trigger



_Appears in:_
- [Trigger](#trigger)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `schedule` _string_ | Schedule is a cron expression (e.g., "0 0 * * *" for daily at midnight) |  | Required: \{\} <br /> |
| `timezone` _string_ | Timezone is the timezone for the cron schedule (e.g., "America/New_York")<br />Defaults to UTC if not specified |  | Optional: \{\} <br /> |
| `concurrencyPolicy` _string_ | ConcurrencyPolicy determines how to handle concurrent executions<br />Allow: Allow concurrent runs<br />Forbid: Skip if previous run is active (default)<br />Replace: Cancel previous run and start new one | Forbid | Enum: [Allow Forbid Replace] <br />Optional: \{\} <br /> |
| `startingDeadlineSeconds` _integer_ | StartingDeadlineSeconds is an optional deadline in seconds for starting the workflow<br />if it misses its scheduled time |  | Optional: \{\} <br /> |
| `inputValuesFrom` _[CronInputFromSecret](#croninputfromsecret) array_ | InputValuesFrom injects input values from Secrets when the scheduler creates a WorkflowRun.<br />Each entry maps one workflow input name to a secret key. Secret is read in the workflow's<br />namespace (or the specified namespace). Use for e.g. slackWebhookUrl from a Secret. |  | Optional: \{\} <br /> |


#### EventConfig



EventConfig configures event emission for workflow runs.



_Appears in:_
- [WorkflowRunSpec](#workflowrunspec)
- [WorkflowSpec](#workflowspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `enabled` _boolean_ | Enabled turns Kubernetes event emission on or off. When nil or true, events are emitted per Level.<br />When false, no events are emitted. |  | Optional: \{\} <br /> |
| `level` _string_ | Level controls which events are emitted when Enabled is true.<br />- Workflow: workflow-level only (WorkflowRunning, WorkflowSucceeded, WorkflowFailed, WorkflowRestarted)<br />- WorkflowAndSteps: workflow-level and step-level (WorkflowExecution for step started/succeeded/failed/skipped) | WorkflowAndSteps | Enum: [Workflow WorkflowAndSteps] <br />Optional: \{\} <br /> |


#### EventResource



EventResource defines a resource type to watch for events



_Appears in:_
- [EventTrigger](#eventtrigger)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | APIVersion is the API version of the resource (e.g., "apps/v1") |  | Required: \{\} <br /> |
| `kind` _string_ | Kind is the kind of the resource (e.g., "Deployment") |  | Required: \{\} <br /> |
| `namespace` _string_ | Namespace is the namespace to watch (empty string for cluster-scoped resources) |  | Optional: \{\} <br /> |


#### EventResourceInfo



EventResourceInfo contains information about the Kubernetes resource that triggered an event



_Appears in:_
- [TriggerInfo](#triggerinfo)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | APIVersion is the API version of the resource |  |  |
| `kind` _string_ | Kind is the kind of the resource |  |  |
| `name` _string_ | Name is the name of the resource |  |  |
| `namespace` _string_ | Namespace is the namespace of the resource (empty for cluster-scoped) |  | Optional: \{\} <br /> |


#### EventTrigger



EventTrigger defines a Kubernetes event-based trigger



_Appears in:_
- [Trigger](#trigger)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `resources` _[EventResource](#eventresource) array_ | Resources defines the resources to watch for events |  | Required: \{\} <br /> |
| `operations` _string array_ | Operations defines the operations to trigger on (CREATE, UPDATE, DELETE)<br />If empty, all operations trigger |  | Optional: \{\} <br /> |
| `labelSelector` _[LabelSelector](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.29/#labelselector-v1-meta)_ | LabelSelector is a label selector to filter resources |  | Optional: \{\} <br /> |
| `fieldSelector` _string_ | FieldSelector is a field selector to filter resources |  | Optional: \{\} <br /> |
| `inputMapping` _object (keys:string, values:string)_ | InputMapping maps event data to workflow input values<br />Keys are workflow input names, values are CEL expressions evaluated on the event object.<br />Available variable: object (the triggering resource as a dynamic map).<br />Example: appName: 'object.metadata.name' |  | Optional: \{\} <br /> |
| `celFilter` _string_ | CELFilter is a CEL expression evaluated against the event object before creating a WorkflowRun.<br />Must return bool. Events where the filter returns false or errors are dropped.<br />Available variable: object (the triggering resource as a dynamic map).<br />Example: 'object.status.sync.status == "Synced"' |  | Optional: \{\} <br /> |
| `dedupKey` _string_ | DedupKey is an optional CEL expression override for the deduplication key.<br />By default, OttoFlow auto-detects the revision field for known GitOps controllers<br />(ArgoCD Application, FluxCD Kustomization/HelmRelease). Only set this for controllers<br />not in the built-in list (e.g. Rancher Fleet: 'object.status.commit').<br />Available variable: object (the triggering resource as a dynamic map). |  | Optional: \{\} <br /> |
| `dedupWindow` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.29/#duration-v1-meta)_ | DedupWindow is the fallback deduplication window used when no revision field<br />is auto-detected and DedupKey is not set. Events for the same object (matched<br />by UID) within this window after the last WorkflowRun creation are dropped.<br />Defaults to 10 minutes if omitted; set to suppress repeat events on a single<br />flapping object (e.g. a Pod cycling through the same status repeatedly).<br />This field only dedupes repeat events for an object already seen — it does<br />not, and structurally cannot, bound the number of runs created by triggers<br />that observe a stream of distinct new objects (e.g. a Kind: WorkflowRun<br />trigger watching its own runs' Pods/Jobs), since each such object is a<br />first-sight miss against dedup state and is never suppressed by it. Use<br />labelSelector to exclude OttoFlow-managed objects and/or MaxConcurrentRuns on<br />the Workflow to bound overall run volume for that case. |  | Optional: \{\} <br /> |


#### ExecutionLimits



ExecutionLimits configures per-workflow concurrency and rate limits.



_Appears in:_
- [WorkflowSpec](#workflowspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `maxConcurrentSteps` _integer_ | MaxConcurrentSteps is the maximum number of steps that may run concurrently in a single WorkflowRun.<br />When multiple steps are ready (DAG), only this many are started at once. Zero or nil means no limit. |  | Optional: \{\} <br /> |
| `outboundRequestsPerMinute` _integer_ | OutboundRequestsPerMinute limits the rate of outbound calls (MCP, agent executor) per WorkflowRun.<br />When set, the executor waits as needed before each call. Zero or nil means no rate limit. |  | Optional: \{\} <br /> |


#### ExposeSpec



ExposeSpec configures external surfaces a Workflow is published to. Currently only kagent.



_Appears in:_
- [WorkflowSpec](#workflowspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `kagent` _[KagentExposeSpec](#kagentexposespec)_ | Kagent, when set, opts the Workflow into kagent (agent-to-agent) exposure via a kagent BYO Agent. |  | Optional: \{\} <br /> |


#### Expression



Expression defines a CEL expression with a name



_Appears in:_
- [Step](#step)
- [StepForEachStep](#stepforeachstep)
- [StepTemplateStep](#steptemplatestep)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the name to store the expression result |  | Required: \{\} <br /> |
| `expression` _string_ | Expression is the CEL expression to evaluate |  | Required: \{\} <br /> |


#### ExternalAgentAuth



ExternalAgentAuth defines bearer-token authentication for an external agent call



_Appears in:_
- [StepExternalAgentRef](#stepexternalagentref)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `secretRef` _[SecretReference](#secretreference)_ | SecretRef references a Secret containing the bearer token.<br />The Key field of SecretRef specifies which key in the Secret holds the token value. |  | Optional: \{\} <br /> |


#### Input



Input defines a workflow input parameter



_Appears in:_
- [WorkflowSpec](#workflowspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the input parameter name |  | Required: \{\} <br /> |
| `description` _string_ | Description is a human-readable description of the input |  | Optional: \{\} <br /> |
| `default` _string_ | Default is the default value for the input (optional) |  | Optional: \{\} <br /> |
| `required` _boolean_ | Required indicates if the input must be provided |  | Optional: \{\} <br /> |


#### KagentExposeSpec



KagentExposeSpec describes the kagent agent card metadata for a Workflow exposed via kagent.



_Appears in:_
- [ExposeSpec](#exposespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `displayName` _string_ | DisplayName is a human-friendly name for the agent card. Defaults to the Workflow name. |  | Optional: \{\} <br /> |
| `description` _string_ | Description is a human-readable description of what the agent does. |  | Optional: \{\} <br /> |
| `examples` _string array_ | Examples are sample prompts shown on the agent card. |  | Optional: \{\} <br /> |
| `tags` _string array_ | Tags are labels shown on the agent card for discovery. |  | Optional: \{\} <br /> |


#### KubeConfigSecretRef



KubeConfigSecretRef references a Secret that holds a kubeconfig file.



_Appears in:_
- [ClusterRef](#clusterref)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the name of the Secret. |  | Required: \{\} <br /> |
| `namespace` _string_ | Namespace is the namespace of the Secret. Defaults to the WorkflowRun namespace if empty. |  | Optional: \{\} <br /> |
| `key` _string_ | Key is the key in Secret.Data containing the kubeconfig (e.g. "config", "kubeconfig", "value").<br />If empty, the helper tries "config", "kubeconfig", then "value". |  | Optional: \{\} <br /> |


#### LLMCredentialsSecretRef



LLMCredentialsSecretRef identifies a Secret containing LLM credentials to inject into
the runner Job. Only keys present in the LLM env allowlist are injected.
The controller reads the Secret directly; the runner Job never requires Secret RBAC.



_Appears in:_
- [WorkflowRunExecutionSpec](#workflowrunexecutionspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the Secret name. |  | Required: \{\} <br /> |
| `namespace` _string_ | Namespace is the Secret namespace. Must be empty or match the WorkflowRun's namespace.<br />Cross-namespace references are rejected at admission because SecretKeyRef in the runner<br />pod spec is namespace-scoped and cannot reference Secrets in other namespaces. |  | Optional: \{\} <br /> |


#### MCPServer



MCPServer is the Schema for the mcpservers API
MCPServer defines an MCP (Model Context Protocol) server configuration





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `ottoflow.nirmata.io/v1alpha1` | | |
| `kind` _string_ | `MCPServer` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.29/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[MCPServerSpec](#mcpserverspec)_ |  |  |  |
| `status` _[MCPServerStatus](#mcpserverstatus)_ |  |  |  |


#### MCPServerPhase

_Underlying type:_ _string_

MCPServerPhase represents the phase of an MCPServer

_Validation:_
- Enum: [Ready NotReady]

_Appears in:_
- [MCPServerStatus](#mcpserverstatus)

| Field | Description |
| --- | --- |
| `Ready` | MCPServerPhaseReady indicates the server is ready to use<br /> |
| `NotReady` | MCPServerPhaseNotReady indicates the server is not ready<br /> |


#### MCPServerSpec



MCPServerSpec defines the desired state of MCPServer



_Appears in:_
- [MCPServer](#mcpserver)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `transport` _[TransportConfig](#transportconfig)_ | Transport defines how to connect to the MCP server |  | Required: \{\} <br /> |
| `timeout` _string_ | Timeout is the connection timeout (e.g., "30s", "5m") |  | Optional: \{\} <br /> |
| `env` _[EnvVar](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.29/#envvar-v1-core) array_ | Env is a list of environment variables to set for the MCP server process |  | Optional: \{\} <br /> |
| `auth` _[AuthConfig](#authconfig)_ | Auth defines authentication configuration |  | Optional: \{\} <br /> |


#### MCPServerStatus



MCPServerStatus defines the observed state of MCPServer



_Appears in:_
- [MCPServer](#mcpserver)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `phase` _[MCPServerPhase](#mcpserverphase)_ | Phase represents the current phase of the MCPServer |  | Enum: [Ready NotReady] <br />Optional: \{\} <br /> |
| `message` _string_ | Message provides additional information about the server status |  | Optional: \{\} <br /> |
| `lastConnected` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.29/#time-v1-meta)_ | LastConnected is when the server was last successfully connected |  | Optional: \{\} <br /> |
| `availableTools` _string array_ | AvailableTools is a list of tools available from this MCP server |  | Optional: \{\} <br /> |


#### MCPTool



MCPTool exposes a workflow as a tool an MCP client can call, which lets an
agent framework run it. Exposure is per workflow and opt-in: an endpoint
that runs every workflow in the cluster is not something a workflow author
should get by default.



_Appears in:_
- [WorkflowSpec](#workflowspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `enabled` _boolean_ | Enabled exposes this workflow on the MCP endpoint. The endpoint itself<br />must also be running (the controller's --mcp-addr); this field decides<br />whether this workflow is one of the tools it serves. |  | Optional: \{\} <br /> |
| `description` _string_ | Description is what an MCP client shows a model when it decides whether<br />to call this workflow. It is the whole basis for that decision, so write<br />it for that reader: what the workflow does, and when to reach for it.<br />Defaults to a sentence built from the workflow's name. |  | Optional: \{\} <br /> |


#### MatchCondition



MatchCondition defines a conditional execution rule for a step
Similar to Kubernetes ValidatingAdmissionPolicy matchConditions
All conditions must evaluate to true for the step to execute



_Appears in:_
- [Step](#step)
- [StepForEachStep](#stepforeachstep)
- [StepTemplateStep](#steptemplatestep)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is a unique identifier for this match condition<br />Used for logging and visibility when conditions fail |  | Required: \{\} <br /> |
| `expression` _string_ | Expression is a CEL expression that evaluates to a boolean<br />Step executes only if ALL matchConditions evaluate to true<br />If ANY condition evaluates to false, the step is skipped |  | Required: \{\} <br /> |


#### MetricLabel



MetricLabel defines a label for a custom metric



_Appears in:_
- [OutputMetric](#outputmetric)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the label name |  | Required: \{\} <br /> |
| `value` _string_ | Value is a CEL expression for the label value (must evaluate to string) |  | Required: \{\} <br /> |


#### MutateApplyConfiguration



MutateApplyConfiguration holds a CEL expression that returns the patch object for merge



_Appears in:_
- [StepMutate](#stepmutate)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `expression` _string_ | Expression is a CEL expression evaluated with "object" (current resource) and workflow context.<br />Must return a map/object that will be deep-merged onto the resource. |  | Required: \{\} <br /> |


#### MutateJSONPatch



MutateJSONPatch holds either a CEL expression or a static list of RFC 6902 JSON Patch operations



_Appears in:_
- [StepMutate](#stepmutate)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `expression` _string_ | Expression is a CEL expression that returns a list of patch operations.<br />Each element must be a map with "op" (add\|remove\|replace\|move\|copy\|test), "path" (JSON Pointer), and optionally "value" or "from".<br />Evaluated with "object" (current resource) and workflow context. |  | Optional: \{\} <br /> |
| `operations` _[MutateJSONPatchOp](#mutatejsonpatchop) array_ | Operations is a static list of JSON Patch operations when Expression is not set |  | Optional: \{\} <br /> |


#### MutateJSONPatchOp



MutateJSONPatchOp represents one RFC 6902 JSON Patch operation



_Appears in:_
- [MutateJSONPatch](#mutatejsonpatch)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `op` _string_ | Op is the operation: add, remove, replace, move, copy, or test |  | Enum: [add remove replace move copy test] <br />Required: \{\} <br /> |
| `path` _string_ | Path is the JSON Pointer to the target location |  | Required: \{\} <br /> |
| `value` _[JSON](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.29/#json-v1-apiextensions-k8s-io)_ | Value is the value for add/replace/test (optional for remove, use From for move/copy) |  | Optional: \{\} <br /> |
| `from` _string_ | From is the source path for move/copy operations |  | Optional: \{\} <br /> |


#### NamespacedSecretRef



NamespacedSecretRef references a Secret by name and namespace (no specific key)



_Appears in:_
- [OAuth2Config](#oauth2config)
- [StepExternalAgentRef](#stepexternalagentref)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ |  |  |  |
| `namespace` _string_ |  |  |  |


#### OAuth2Config



OAuth2Config defines OAuth 2.0 client credentials flow configuration
Used for machine-to-machine auth when connecting to MCP servers



_Appears in:_
- [AuthConfig](#authconfig)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `tokenURL` _string_ | TokenURL is the OAuth2 token endpoint (e.g., https://auth.example.com/oauth/token) |  | Required: \{\} <br /> |
| `clientId` _string_ | ClientID is the OAuth2 client ID (alternative to ClientCredentialsRef) |  | Optional: \{\} <br /> |
| `clientSecretRef` _[SecretReference](#secretreference)_ | ClientSecretRef references a Secret for credentials<br />For client credentials flow: use key "client_secret" (with ClientID above), or<br />use ClientCredentialsRef for both client_id and client_secret |  | Optional: \{\} <br /> |
| `clientCredentialsRef` _[NamespacedSecretRef](#namespacedsecretref)_ | ClientCredentialsRef references a Secret with keys "client_id" and "client_secret"<br />Namespace defaults to MCPServer namespace |  | Optional: \{\} <br /> |
| `scopes` _string array_ | Scopes are optional OAuth2 scopes (space-separated) |  | Optional: \{\} <br /> |


#### Output



Output defines an output value written to shared context



_Appears in:_
- [Step](#step)
- [StepForEachStep](#stepforeachstep)
- [StepTemplateStep](#steptemplatestep)
- [WorkflowSpec](#workflowspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the output key name |  | Required: \{\} <br /> |
| `expression` _string_ | Expression is the CEL expression that evaluates to the output value<br />Mutually exclusive with Value field. If both are specified, Value takes precedence. |  | Optional: \{\} <br /> |
| `value` _[JSON](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.29/#json-v1-apiextensions-k8s-io)_ | Value defines a native YAML structure where string values can contain CEL expressions<br />String values that look like CEL expressions (e.g., 'inputs.podName', 'variables.count')<br />are automatically evaluated. If evaluation fails, the literal string value is used.<br />Mutually exclusive with Expression field. If both are specified, Value takes precedence.<br />Stored as raw JSON bytes to support arbitrary YAML structures |  | Optional: \{\} <br /> |
| `metric` _[OutputMetric](#outputmetric)_ | Metric optionally publishes this output's value to Prometheus.<br />Only used for workflow-level outputs (WorkflowSpec.Outputs).<br />The output's evaluated value becomes the metric value. |  | Optional: \{\} <br /> |
| `sensitive` _boolean_ | Sensitive, when true, prevents writing the evaluated value to WorkflowRun status.<br />The value remains in in-memory context for subsequent steps/metrics where applicable,<br />but Status.Outputs will contain a redacted placeholder instead of the raw value.<br />Use for outputs that may contain secrets or PII. |  | Optional: \{\} <br /> |


#### OutputExtraction



OutputExtraction defines how to extract structured outputs from agent responses



_Appears in:_
- [AgentSpec](#agentspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `type` _string_ | Type is the extraction method<br />Options: json, regex, text | json | Enum: [json regex text] <br />Optional: \{\} <br /> |
| `pattern` _string_ | Pattern is the extraction pattern<br />For json: JSONPath expression (e.g., "$.result.recommendations"). A pattern that<br />  matches nothing fails the step. Omit to return the whole JSON object.<br />For regex: Regular expression with capture groups<br />For text: unused - the full response is always returned. Use type regex to select<br />  a substring. |  | Optional: \{\} <br /> |
| `schema` _integer array_ | Schema defines the expected output schema (for JSON extraction)<br />Stored as raw JSON bytes |  | Optional: \{\} <br /> |


#### OutputMetric



OutputMetric defines Prometheus metric publication for an output



_Appears in:_
- [Output](#output)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the metric name (will be prefixed with ottoflow_workflow_)<br />Must be valid Prometheus metric name: [a-zA-Z_:][a-zA-Z0-9_:]* |  | Required: \{\} <br /> |
| `type` _string_ | Type is the metric type: counter, gauge, or histogram |  | Enum: [counter gauge histogram] <br />Required: \{\} <br /> |
| `help` _string_ | Help is the metric description |  | Optional: \{\} <br /> |
| `labels` _[MetricLabel](#metriclabel) array_ | Labels are optional label key-value pairs (CEL expressions for values) |  | Optional: \{\} <br /> |
| `buckets` _float array_ | Buckets for histogram type (optional, uses default if not specified) |  | Optional: \{\} <br /> |


#### RetryCondition



RetryCondition defines conditions that trigger a retry



_Appears in:_
- [RetryPolicy](#retrypolicy)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `errorType` _string_ | ErrorType is the error type to retry on (e.g., "NetworkError", "TimeoutError", "TransientError") |  | Optional: \{\} <br /> |
| `httpStatus` _integer array_ | HTTPStatus is an array of HTTP status codes to retry on (e.g., [500, 502, 503, 504]) |  | Optional: \{\} <br /> |
| `errorMessage` _string_ | ErrorMessage is an error message pattern to match (regex) |  | Optional: \{\} <br /> |


#### RetryPolicy



RetryPolicy defines retry configuration for step execution



_Appears in:_
- [Step](#step)
- [StepForEachStep](#stepforeachstep)
- [StepTemplateStep](#steptemplatestep)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `attempts` _integer_ | Attempts is the maximum number of retry attempts (including initial attempt) | 1 | Minimum: 1 <br />Optional: \{\} <br /> |
| `backoff` _[BackoffConfig](#backoffconfig)_ | Backoff defines the backoff strategy for retries |  | Optional: \{\} <br /> |
| `retryOn` _[RetryCondition](#retrycondition) array_ | RetryOn defines conditions that trigger a retry<br />If empty, retry on all errors |  | Optional: \{\} <br /> |


#### RunPolicy



RunPolicy configures retention and maximum run count for a workflow.



_Appears in:_
- [WorkflowSpec](#workflowspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `retentionMinutes` _integer_ | RetentionMinutes is how long to keep completed WorkflowRuns (Succeeded or Failed).<br />Completed runs older than this are deleted. Zero or nil means no automatic deletion. |  | Optional: \{\} <br /> |
| `maxAllowed` _integer_ | MaxAllowed is the maximum number of WorkflowRuns to retain per workflow (completed runs).<br />When exceeded, oldest completed runs are deleted first. Zero or nil means no limit.<br />Pending and Running runs are not counted toward this limit when deciding deletions. |  | Optional: \{\} <br /> |
| `maxConcurrentRuns` _integer_ | MaxConcurrentRuns is the maximum number of WorkflowRuns that may be Pending or Running at once for this workflow.<br />When a trigger (cron, event) would create a new run, creation is skipped if active runs >= MaxConcurrentRuns.<br />Zero or nil means no limit. |  | Optional: \{\} <br /> |


#### SecretReference



SecretReference references a Kubernetes Secret



_Appears in:_
- [AuthConfig](#authconfig)
- [ExternalAgentAuth](#externalagentauth)
- [OAuth2Config](#oauth2config)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the name of the Secret |  | Required: \{\} <br /> |
| `namespace` _string_ | Namespace is the namespace of the Secret. Defaults to the namespace of the<br />resource that references this Secret (MCPServer, Workflow, etc.). |  | Optional: \{\} <br /> |
| `key` _string_ | Key is the key in the Secret |  | Required: \{\} <br /> |


#### Step



Step defines a workflow step



_Appears in:_
- [WorkflowSpec](#workflowspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the unique name for the step within the workflow<br />Must be camelCase (e.g., collectPodData, not collect-pod-data) |  | Pattern: `^[a-z][a-zA-Z0-9]*$` <br />Required: \{\} <br /> |
| `message` _string_ | Message is a human-readable description of what the step does |  | Optional: \{\} <br /> |
| `expressions` _[Expression](#expression) array_ | Expressions are CEL expressions evaluated before step execution |  | Optional: \{\} <br /> |
| `outputs` _[Output](#output) array_ | Outputs defines key-value pairs written to shared context |  | Optional: \{\} <br /> |
| `dependsOn` _string array_ | DependsOn explicitly declares step dependencies<br />Steps listed here must complete successfully before this step executes<br />If not specified, dependencies are inferred from output references in expressions |  | Optional: \{\} <br /> |
| `matchConditions` _[MatchCondition](#matchcondition) array_ | MatchConditions defines conditional execution rules for this step<br />Similar to Kubernetes ValidatingAdmissionPolicy matchConditions<br />Step executes only if ALL conditions evaluate to true<br />If ANY condition evaluates to false, the step is skipped<br />If no matchConditions are specified, the step always executes |  | Optional: \{\} <br /> |
| `retry` _[RetryPolicy](#retrypolicy)_ | Retry defines retry configuration for step execution |  | Optional: \{\} <br /> |
| `timeout` _string_ | Timeout defines maximum duration for step execution (e.g., "30s", "5m", "1h")<br />Step fails if exceeded |  | Optional: \{\} <br /> |
| `failurePolicy` _string_ | FailurePolicy determines workflow behavior on step failure<br />Fail: Step failure causes workflow to fail (default)<br />Continue: Step failure is logged but workflow continues to next step | Fail | Enum: [Fail Continue] <br />Optional: \{\} <br /> |
| `workflowRef` _[StepWorkflowRef](#stepworkflowref)_ | WorkflowRef references another workflow to execute as a sub-workflow<br />When specified, this step executes the referenced workflow<br />Input values can be CEL expressions that reference parent workflow context<br />Sub-workflow outputs are accessible via outputs.<stepNameCamelCase>.<outputName> |  | Optional: \{\} <br /> |
| `agentRef` _[StepAgentRef](#stepagentref)_ | AgentRef references an Agent CRD for AI-powered step execution<br />When specified, this step uses an LLM agent to execute the task<br />The agent's prompt can reference workflow context (inputs, previous step outputs)<br />Agent outputs are accessible via outputs.<stepNameCamelCase>.<outputName> |  | Optional: \{\} <br /> |
| `mcpToolCall` _[StepMCPToolCall](#stepmcptoolcall)_ | MCPToolCall defines a direct MCP tool invocation<br />When specified, this step calls an MCP tool directly without LLM mediation<br />Tool arguments are resolved using CEL expressions with workflow context<br />Tool results are available as `toolResult` variable in output expressions |  | Optional: \{\} <br /> |
| `resourceQuery` _[StepResourceQuery](#stepresourcequery)_ | ResourceQuery defines a simplified resource query that compiles to CEL expressions<br />When specified, this step queries Kubernetes resources using a simplified syntax<br />Supports both single resource queries (with name) and list queries (without name)<br />Outputs are automatically written to variables, consistent with Step.outputs behavior |  | Optional: \{\} <br /> |
| `prometheusQuery` _[StepPrometheusQuery](#stepprometheusquery)_ | PrometheusQuery defines a Prometheus (PromQL) query step<br />When specified, this step runs a PromQL query with template variable substitution,<br />then evaluates outputs over the result (samples, value, etc.) |  | Optional: \{\} <br /> |
| `mutate` _[StepMutate](#stepmutate)_ | Mutate defines a Kyverno-style mutate step to patch a single resource via CEL or JSONPatch<br />When specified, this step GETs the target resource, evaluates the patch (with "object" in CEL context), and applies it |  | Optional: \{\} <br /> |
| `stepTemplateRef` _[StepTemplateRef](#steptemplateref)_ | StepTemplateRef references a StepTemplate to instantiate<br />When specified, this step instantiates the referenced StepTemplate with provided arguments<br />The template's step definition is expanded with parameter substitution<br />Template arguments are CEL expressions evaluated in the workflow context |  | Optional: \{\} <br /> |
| `forEach` _[StepForEach](#stepforeach)_ | ForEach defines parallel execution over a list of items<br />When specified, this step generates child steps dynamically for each item<br />Results are automatically collected and accessible via steps.<stepName>.results |  | Optional: \{\} <br /> |
| `externalAgentRef` _[StepExternalAgentRef](#stepexternalagentref)_ | ExternalAgentRef defines a call to an external A2A-compatible agent service<br />When specified, this step sends a task to the referenced external agent via A2A protocol<br />The task result is available as `a2aResult` in output CEL expressions |  | Optional: \{\} <br /> |
| `openReport` _[StepOpenReport](#stepopenreport)_ | OpenReport defines an OpenReports.io report generation step.<br />When specified, this step emits workflow results as an OpenReports.io Report CRD.<br />If the OpenReports CRD is not installed, the step falls back to storing report data<br />in context as reportResult.data with a Warning event on the WorkflowRun.<br />Report results are available as `reportResult` in output CEL expressions. |  | Optional: \{\} <br /> |
| `waitForCallback` _[WaitForCallbackStep](#waitforcallbackstep)_ | WaitForCallback pauses workflow execution and waits for an external callback.<br />Enables human-in-the-loop and AI-to-human-to-AI workflows.<br />When specified, this step generates a cryptographically secure token, stores it in<br />WorkflowRun.status.pendingCallback, and exits with code 0. The controller recreates<br />the runner Job when callback data is POSTed to the callback endpoint. |  | Optional: \{\} <br /> |


#### StepAgentRef



StepAgentRef references an Agent CRD for AI-powered step execution



_Appears in:_
- [Step](#step)
- [StepForEachStep](#stepforeachstep)
- [StepTemplateStep](#steptemplatestep)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the name of the Agent CRD to use |  | Required: \{\} <br /> |
| `namespace` _string_ | Namespace is the namespace of the Agent CRD<br />Defaults to the parent workflow namespace if not specified |  | Optional: \{\} <br /> |
| `additionalPrompts` _string array_ | AdditionalPrompts is an optional list of prompts that are appended to the agent's system prompt<br />Each prompt can contain CEL expressions that are evaluated in the workflow context<br />The prompts are evaluated and concatenated with the agent's base prompt<br />Useful for adding step-specific context or instructions to the agent |  | Optional: \{\} <br /> |
| `maxAdditionalPromptTokens` _integer_ | MaxAdditionalPromptTokens is an optional token budget for the combined additionalPrompts text.<br />When set, the evaluated additional prompt text is truncated to fit this budget, using a rough<br />heuristic of approximately 3 runes per token (code/YAML tokenizes denser than prose, so this<br />errs conservative). The agent's base prompt is not counted. Nil means no limit. 0 disables the limit. |  | Maximum: 1e+07 <br />Minimum: 0 <br />Optional: \{\} <br /> |
| `contextBudgetMode` _string_ | ContextBudgetMode controls how much of the accumulated step context is visible to<br />CEL evaluation when evaluating additionalPrompts. Applied before BuildVariableMap,<br />so no materialization cost is paid for entries that are filtered out.<br />full: all step outputs are visible (default when omitted — preserves current behavior)<br />lastN: only the N most recently completed step outputs are visible<br />omit: no step outputs are visible<br />Only the step outputs are filtered. Inputs, variables, and expressions remain fully<br />visible in every mode. |  | Enum: [full lastN omit] <br />Optional: \{\} <br /> |
| `contextBudgetLastN` _integer_ | ContextBudgetLastN is the number of most-recently-completed steps to include when<br />ContextBudgetMode=lastN. Steps beyond the last N are omitted from CEL context.<br />N counts only completed steps that have an entry in the context's steps map (e.g. agent<br />steps that produced a response/output); steps with no steps-map entry are not counted and<br />do not consume a slot in the window.<br />Ignored for other modes. Defaults to 5 when omitted or zero. |  | Maximum: 1000 <br />Minimum: 0 <br />Optional: \{\} <br /> |


#### StepExternalAgentRef



StepExternalAgentRef defines a call to an external A2A-compatible agent service



_Appears in:_
- [Step](#step)
- [StepForEachStep](#stepforeachstep)
- [StepTemplateStep](#steptemplatestep)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `url` _string_ | URL is the A2A agent card base URL (normally HTTPS, e.g. "https://kagent.example.com").<br />http:// is permitted only when allowInsecureHTTP is set, and only to cluster-local hosts. |  | Required: \{\} <br /> |
| `protocol` _string_ | Protocol is the agent communication protocol. Currently only "a2a" is supported. | a2a | Enum: [a2a] <br />Optional: \{\} <br /> |
| `prompt` _string_ | Prompt is a CEL expression evaluated in the workflow context; the result is sent as the task message.<br />Can reference inputs, variables, and previous step outputs (e.g. '"Analyze: " + steps.prev.report'). |  | Required: \{\} <br /> |
| `auth` _[ExternalAgentAuth](#externalagentauth)_ | Auth defines authentication for the external agent call |  | Optional: \{\} <br /> |
| `caSecretRef` _[NamespacedSecretRef](#namespacedsecretref)_ | CASecretRef references a Secret containing a CA bundle (key: ca.crt) for TLS verification.<br />When omitted, the system CA pool is used. |  | Optional: \{\} <br /> |
| `timeout` _string_ | Timeout is the maximum duration to wait for task completion (e.g. "5m", "30s"). Default: 5m. |  | Optional: \{\} <br /> |
| `allowInsecureHTTP` _boolean_ | AllowInsecureHTTP permits http:// URLs, but ONLY to cluster-local hosts<br />(a host ending in .svc or .svc.cluster.local, or exactly localhost / 127.0.0.1 / ::1).<br />http:// to any other host is rejected even when true. https:// ignores this flag.<br />Must NOT be combined with auth.secretRef (a bearer token would be sent in cleartext).<br />Must also NOT be combined with caSecretRef (a CA bundle has no effect over plaintext http). |  | Optional: \{\} <br /> |


#### StepForEach



StepForEach defines parallel execution over a list of items



_Appears in:_
- [Step](#step)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `items` _string_ | Items is a CEL expression that evaluates to a list<br />Each item in the list will be processed by a child step |  | Required: \{\} <br /> |
| `itemVariable` _string_ | ItemVariable is the variable name to use for the current item in child steps<br />Default: "item" |  | Optional: \{\} <br /> |
| `step` _[StepForEachStep](#stepforeachstep)_ | Step defines the step to execute for each item (inline definition)<br />Mutually exclusive with StepTemplateRef |  | Optional: \{\} <br /> |
| `stepTemplateRef` _[StepForEachTemplateRef](#stepforeachtemplateref)_ | StepTemplateRef references a StepTemplate to use for each item<br />Mutually exclusive with Step |  | Optional: \{\} <br /> |
| `maxConcurrency` _integer_ | MaxConcurrency limits the number of concurrent child step executions<br />Default: 5 |  | Minimum: 1 <br />Optional: \{\} <br /> |
| `itemFailurePolicy` _string_ | ItemFailurePolicy determines behavior when a child step fails<br />Continue: Continue processing other items, collect failures<br />Fail: Fail the forEach step (and workflow) on first failure<br />Default: Continue |  | Enum: [Continue Fail] <br />Optional: \{\} <br /> |


#### StepForEachStep



StepForEachStep defines an inline step definition for forEach execution



_Appears in:_
- [StepForEach](#stepforeach)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `expressions` _[Expression](#expression) array_ | Expressions are CEL expressions evaluated before step execution<br />Can reference the current item via itemVariable (default: "item") |  | Optional: \{\} <br /> |
| `outputs` _[Output](#output) array_ | Outputs defines key-value pairs written to shared context<br />Can reference the current item via itemVariable (default: "item") |  | Optional: \{\} <br /> |
| `matchConditions` _[MatchCondition](#matchcondition) array_ | MatchConditions defines conditional execution rules for this step |  | Optional: \{\} <br /> |
| `retry` _[RetryPolicy](#retrypolicy)_ | Retry defines retry configuration for step execution |  | Optional: \{\} <br /> |
| `timeout` _string_ | Timeout defines maximum duration for step execution (e.g., "30s", "5m", "1h") |  | Optional: \{\} <br /> |
| `failurePolicy` _string_ | FailurePolicy determines behavior on step failure |  | Enum: [Fail Continue] <br />Optional: \{\} <br /> |
| `resourceQuery` _[StepResourceQuery](#stepresourcequery)_ | ResourceQuery defines a simplified resource query |  | Optional: \{\} <br /> |
| `prometheusQuery` _[StepPrometheusQuery](#stepprometheusquery)_ | PrometheusQuery defines a Prometheus (PromQL) query step |  | Optional: \{\} <br /> |
| `mutate` _[StepMutate](#stepmutate)_ | Mutate defines a mutate step to patch a single resource via CEL or JSONPatch |  | Optional: \{\} <br /> |
| `agentRef` _[StepAgentRef](#stepagentref)_ | AgentRef references an Agent CRD for AI-powered step execution |  | Optional: \{\} <br /> |
| `mcpToolCall` _[StepMCPToolCall](#stepmcptoolcall)_ | MCPToolCall defines a direct MCP tool invocation |  | Optional: \{\} <br /> |
| `workflowRef` _[StepWorkflowRef](#stepworkflowref)_ | WorkflowRef references another workflow to execute as a sub-workflow |  | Optional: \{\} <br /> |
| `externalAgentRef` _[StepExternalAgentRef](#stepexternalagentref)_ | ExternalAgentRef defines a call to an external A2A-compatible agent service |  | Optional: \{\} <br /> |
| `openReport` _[StepOpenReport](#stepopenreport)_ | OpenReport defines an OpenReports.io report generation step |  | Optional: \{\} <br /> |


#### StepForEachTemplateRef



StepForEachTemplateRef references a StepTemplate to use for each item



_Appears in:_
- [StepForEach](#stepforeach)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the name of the StepTemplate to use |  | Required: \{\} <br /> |
| `namespace` _string_ | Namespace is the namespace of the StepTemplate<br />Defaults to the parent workflow namespace if not specified |  | Optional: \{\} <br /> |
| `arguments` _object (keys:string, values:string)_ | Arguments provides template arguments<br />Keys match parameter names defined in the StepTemplate<br />Values are CEL expressions evaluated in workflow context<br />Can reference the current item via itemVariable (default: "item") |  | Optional: \{\} <br /> |


#### StepMCPToolCall



StepMCPToolCall defines a direct MCP tool invocation



_Appears in:_
- [Step](#step)
- [StepForEachStep](#stepforeachstep)
- [StepTemplateStep](#steptemplatestep)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `server` _string_ | Server is the name of the MCPServer CRD to use |  | Required: \{\} <br /> |
| `tool` _string_ | Tool is the name of the tool within the MCP server |  | Required: \{\} <br /> |
| `arguments` _object (keys:string, values:string)_ | Arguments are the tool arguments<br />Keys are argument names, values are CEL expressions evaluated in workflow context |  | Optional: \{\} <br /> |


#### StepMutate



StepMutate defines a mutate step that patches a single Kubernetes resource (Kyverno-style)



_Appears in:_
- [Step](#step)
- [StepForEachStep](#stepforeachstep)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `target` _[StepMutateTarget](#stepmutatetarget)_ | Target identifies the resource to mutate |  |  |
| `patchType` _string_ | PatchType is the type of patch to apply<br />ApplyConfiguration: CEL expression returns a partial object merged onto the resource (merge patch)<br />JSONPatch: CEL expression returns a list of RFC 6902 operations, or use Operations for a static list |  | Enum: [ApplyConfiguration JSONPatch] <br />Required: \{\} <br /> |
| `applyConfiguration` _[MutateApplyConfiguration](#mutateapplyconfiguration)_ | ApplyConfiguration defines the patch when patchType is ApplyConfiguration.<br />Expression is evaluated with "object" (the current resource) and workflow context; must return a map/object to merge. |  | Optional: \{\} <br /> |
| `jsonPatch` _[MutateJSONPatch](#mutatejsonpatch)_ | JSONPatch defines the patch when patchType is JSONPatch.<br />Either Expression (CEL returning a list of \{op, path, value?\}) or Operations (static list) can be set. |  | Optional: \{\} <br /> |
| `outputs` _object (keys:string, values:string)_ | Outputs defines outputs to extract after the mutation (e.g. "resource" for the patched object)<br />Keys are output names, values are CEL expressions; "object" refers to the patched resource |  | Optional: \{\} <br /> |


#### StepMutateTarget



StepMutateTarget identifies the resource to mutate



_Appears in:_
- [StepMutate](#stepmutate)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | APIVersion is the API version of the resource (e.g., "v1", "apps/v1") |  | Required: \{\} <br /> |
| `resource` _string_ | Resource is the kind of the resource (e.g., "Pod", "Deployment", "ConfigMap") |  | Required: \{\} <br /> |
| `namespace` _string_ | Namespace is the namespace of the resource; CEL expression evaluated in workflow context. Omit for cluster-scoped. |  | Optional: \{\} <br /> |
| `name` _string_ | Name is the name of the resource; CEL expression evaluated in workflow context |  | Required: \{\} <br /> |


#### StepOpenReport



StepOpenReport defines an OpenReports.io report generation step.

The executor checks whether the openreports.io/v1alpha1 Report CRD is installed in the
cluster. If it is, a Report CRD object is created with the evaluated results. If it is
not, the report data is stored in context as reportResult.data and a Warning Kubernetes
Event is emitted on the WorkflowRun — the step still succeeds so downstream steps can
continue.

After execution, reportResult is available in CEL output expressions:

	reportResult.mode      — "crd" if a Report CRD was created, "data" if OpenReports is absent
	reportResult.name      — CRD name (mode=crd) or empty string (mode=data)
	reportResult.namespace — CRD namespace (mode=crd) or empty string (mode=data)
	reportResult.summary   — map with pass/fail/warn/error/skip integer counts
	reportResult.data      — the raw results list (always present in both modes)



_Appears in:_
- [Step](#step)
- [StepForEachStep](#stepforeachstep)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `reportName` _string_ | ReportName is the name of the Report CRD to create. |  | MinLength: 1 <br /> |
| `namespace` _string_ | Namespace where the Report CRD is created.<br />Defaults to the WorkflowRun's namespace if omitted. |  | Optional: \{\} <br /> |
| `source` _string_ | Source identifies the producer of this report (e.g. "ottoflow", "kyverno").<br />Defaults to "ottoflow" if omitted. |  | Optional: \{\} <br /> |
| `scopeExpression` _string_ | ScopeExpression is an optional CEL expression evaluating to an object reference map<br />with keys: apiVersion, kind, name, namespace. Used to associate the report with a<br />specific Kubernetes resource (e.g. a Deployment or Namespace). |  | Optional: \{\} <br /> |
| `resultsExpression` _string_ | ResultsExpression is a CEL expression evaluating to a list of policy check results.<br />Each item must match the OpenReports ReportResult schema:<br />  \{policy, result, scored, timestamp?, source?, rule?, severity?, message?, ...\}<br />The result field must be one of: pass, fail, warn, error, skip. |  | MinLength: 1 <br /> |
| `summaryExpression` _string_ | SummaryExpression is an optional CEL expression evaluating to a summary map with<br />integer keys: pass, fail, warn, error, skip.<br />If omitted, the executor auto-computes the summary by counting result values in<br />the ResultsExpression output. |  | Optional: \{\} <br /> |


#### StepPhase

_Underlying type:_ _string_

StepPhase represents the phase of a workflow step

_Validation:_
- Enum: [Pending Running Succeeded Failed Skipped Waiting]

_Appears in:_
- [StepStatus](#stepstatus)

| Field | Description |
| --- | --- |
| `Pending` | StepPhasePending indicates the step is pending execution<br /> |
| `Running` | StepPhaseRunning indicates the step is currently running<br /> |
| `Succeeded` | StepPhaseSucceeded indicates the step completed successfully<br /> |
| `Failed` | StepPhaseFailed indicates the step failed<br /> |
| `Skipped` | StepPhaseSkipped indicates the step was skipped<br /> |
| `Waiting` | StepPhaseWaiting indicates the step is paused waiting for an external callback<br /> |


#### StepPrometheusQuery



StepPrometheusQuery defines a Prometheus (PromQL) query step



_Appears in:_
- [Step](#step)
- [StepForEachStep](#stepforeachstep)
- [StepTemplateStep](#steptemplatestep)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `query` _string_ | Query is the PromQL expression. May contain \{\{.varName\}\} placeholders substituted from Variables. |  | Required: \{\} <br /> |
| `timeRange` _string_ | TimeRange is the lookback duration for the instant query (e.g., "7d", "1h", "5m").<br />The query is executed at (now - timeRange). |  | Required: \{\} <br /> |
| `step` _string_ | Step is the query resolution step (optional). Reserved for future range-query support. |  | Optional: \{\} <br /> |
| `variables` _object (keys:string, values:string)_ | Variables provides values for \{\{.varName\}\} placeholders in Query.<br />Keys are placeholder names, values are CEL expressions evaluated in workflow context. |  | Optional: \{\} <br /> |
| `outputs` _object (keys:string, values:string)_ | Outputs defines the outputs to extract from the Prometheus result.<br />Keys are output names (written to variables), values are CEL expressions.<br />Expressions can reference "result" with fields: type ("vector"\|"scalar"), samples (list of \{metric, value, timestamp\}), value (scalar). |  | Optional: \{\} <br /> |


#### StepResourceQuery



StepResourceQuery defines a simplified resource query that compiles to CEL expressions



_Appears in:_
- [Step](#step)
- [StepForEachStep](#stepforeachstep)
- [StepTemplateStep](#steptemplatestep)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | APIVersion is the API version of the resource (e.g., "v1", "apps/v1") |  | Required: \{\} <br /> |
| `resource` _string_ | Resource is the kind of the resource (e.g., "Pod", "Deployment", "Service") |  | Required: \{\} <br /> |
| `namespace` _string_ | Namespace is the namespace to query (can be a CEL expression) |  | Optional: \{\} <br /> |
| `name` _string_ | Name is the name of a specific resource (for single resource queries)<br />If omitted, performs a list query<br />Can be a CEL expression evaluated in workflow context |  | Optional: \{\} <br /> |
| `labelSelector` _object (keys:string, values:string)_ | LabelSelector is a map of label key-value pairs for filtering list queries<br />Keys are label names, values are CEL expressions evaluated in workflow context<br />Only used when Name is omitted (list queries) |  | Optional: \{\} <br /> |
| `fieldSelector` _string_ | FieldSelector filters list queries. It is always evaluated as a CEL expression in<br />workflow context and must yield a field selector string, so a literal selector has<br />to be quoted inside the expression: '"status.phase=Running"'. An unquoted<br />status.phase=Running is not valid CEL and fails at runtime.<br />Only used when Name is omitted (list queries) |  | Optional: \{\} <br /> |
| `limit` _integer_ | Limit caps the number of resources returned for list queries.<br />Resources are fetched in pages of 500; collection stops once this many<br />items have been accumulated. When 0 (default) all pages are fetched.<br />Use this to bound memory usage when querying large clusters. |  | Minimum: 0 <br />Optional: \{\} <br /> |
| `pageSize` _integer_ | PageSize controls how many resources are fetched per API call during list pagination.<br />Defaults to 500 when unset. Reduce this for resource-heavy types (large pod specs,<br />CRDs with large status fields) to keep individual API responses small and reduce<br />the chance of per-page timeouts on loaded API servers. |  | Maximum: 1000 <br />Minimum: 1 <br />Optional: \{\} <br /> |
| `outputs` _object (keys:string, values:string)_ | Outputs defines the outputs to extract from the resource(s)<br />Keys are output names (written to variables), values are CEL expressions<br />For single resource queries: expressions reference "object", the fetched resource<br />  (e.g., "object.status.phase"). Note "resource" is the CEL macro namespace<br />  (resource.Get/resource.List) and does not support field selection.<br />For list queries: expressions reference "items" (the list) (e.g., "items.map(i, i.metadata.name)")<br />A step's own step-level outputs are evaluated after these and can reference them as<br />variables.<name>. On a name collision the step-level output wins. |  | Required: \{\} <br /> |


#### StepStatus



StepStatus represents the status of a workflow step



_Appears in:_
- [WorkflowRunStatus](#workflowrunstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `phase` _[StepPhase](#stepphase)_ | Phase represents the current phase of the step |  | Enum: [Pending Running Succeeded Failed Skipped Waiting] <br /> |
| `startTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.29/#time-v1-meta)_ | StartTime is when the step execution started |  | Optional: \{\} <br /> |
| `completionTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.29/#time-v1-meta)_ | CompletionTime is when the step execution completed |  | Optional: \{\} <br /> |
| `message` _string_ | Message provides additional information about the step status |  | Optional: \{\} <br /> |
| `error` _string_ | Error contains error information if the step failed |  | Optional: \{\} <br /> |
| `retryCount` _integer_ | RetryCount is the number of retry attempts made (0 = initial attempt, 1+ = retries) |  | Optional: \{\} <br /> |
| `lastRetryTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.29/#time-v1-meta)_ | LastRetryTime is the timestamp of last retry attempt |  | Optional: \{\} <br /> |
| `nextRetryTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.29/#time-v1-meta)_ | NextRetryTime is the timestamp when next retry will be attempted (if step is retrying) |  | Optional: \{\} <br /> |


#### StepTemplate



StepTemplate is the Schema for the steptemplates API
StepTemplate defines a reusable step definition that can be instantiated with parameters





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `ottoflow.nirmata.io/v1alpha1` | | |
| `kind` _string_ | `StepTemplate` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.29/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[StepTemplateSpec](#steptemplatespec)_ |  |  |  |
| `status` _[StepTemplateStatus](#steptemplatestatus)_ |  |  |  |


#### StepTemplateParameter



StepTemplateParameter defines a parameter for the template



_Appears in:_
- [StepTemplateSpec](#steptemplatespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the parameter name (used in placeholders: \{\{.name\}\}) |  | Required: \{\} <br /> |
| `description` _string_ | Description provides a human-readable description of the parameter |  | Optional: \{\} <br /> |
| `default` _string_ | Default is the default value for the parameter (CEL expression)<br />Evaluated in workflow context if not provided in arguments |  | Optional: \{\} <br /> |
| `required` _boolean_ | Required indicates if the parameter must be provided |  | Optional: \{\} <br /> |


#### StepTemplateRef



StepTemplateRef references a StepTemplate to instantiate



_Appears in:_
- [Step](#step)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the name of the StepTemplate to use |  | Required: \{\} <br /> |
| `namespace` _string_ | Namespace is the namespace of the StepTemplate<br />Defaults to the parent workflow namespace if not specified |  | Optional: \{\} <br /> |
| `arguments` _object (keys:string, values:string)_ | Arguments provides parameter values for the template<br />Keys match parameter names defined in the StepTemplate<br />Values are CEL expressions that are evaluated in the workflow context |  | Optional: \{\} <br /> |


#### StepTemplateSpec



StepTemplateSpec defines the desired state of StepTemplate



_Appears in:_
- [StepTemplate](#steptemplate)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `step` _[StepTemplateStep](#steptemplatestep)_ | Step defines the step template that will be instantiated<br />The step.name field will be replaced with the actual step name when instantiated<br />Parameter placeholders in expressions/outputs use CEL variable syntax: \{\{.parameterName\}\} |  | Required: \{\} <br /> |
| `parameters` _[StepTemplateParameter](#steptemplateparameter) array_ | Parameters defines the parameters that can be provided when instantiating this template |  | Optional: \{\} <br /> |
| `description` _string_ | Description provides a human-readable description of what this template does |  | Optional: \{\} <br /> |


#### StepTemplateStatus



StepTemplateStatus defines the observed state of StepTemplate



_Appears in:_
- [StepTemplate](#steptemplate)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.29/#condition-v1-meta) array_ | Conditions represent the latest available observations of the template's state |  | Optional: \{\} <br /> |


#### StepTemplateStep



StepTemplateStep defines a step that can be parameterized
This is similar to Step but allows parameter placeholders



_Appears in:_
- [StepTemplateSpec](#steptemplatespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `message` _string_ | Message is a human-readable description of what the step does<br />Can contain parameter placeholders: \{\{.parameterName\}\} |  | Optional: \{\} <br /> |
| `expressions` _[Expression](#expression) array_ | Expressions are CEL expressions evaluated before step execution<br />Can contain parameter placeholders: \{\{.parameterName\}\} |  | Optional: \{\} <br /> |
| `outputs` _[Output](#output) array_ | Outputs defines key-value pairs written to shared context<br />Expression values can contain parameter placeholders: \{\{.parameterName\}\} |  | Optional: \{\} <br /> |
| `dependsOn` _string array_ | DependsOn explicitly declares step dependencies |  | Optional: \{\} <br /> |
| `matchConditions` _[MatchCondition](#matchcondition) array_ | MatchConditions defines conditional execution rules for this step<br />Expression values can contain parameter placeholders: \{\{.parameterName\}\} |  | Optional: \{\} <br /> |
| `retry` _[RetryPolicy](#retrypolicy)_ | Retry defines retry configuration for step execution |  | Optional: \{\} <br /> |
| `timeout` _string_ | Timeout defines maximum duration for step execution |  | Optional: \{\} <br /> |
| `failurePolicy` _string_ | FailurePolicy determines workflow behavior on step failure | Fail | Enum: [Fail Continue] <br />Optional: \{\} <br /> |
| `resourceQuery` _[StepResourceQuery](#stepresourcequery)_ | ResourceQuery defines a simplified resource query<br />Field values can contain parameter placeholders: \{\{.parameterName\}\} |  | Optional: \{\} <br /> |
| `prometheusQuery` _[StepPrometheusQuery](#stepprometheusquery)_ | PrometheusQuery defines a Prometheus (PromQL) query step<br />Field values can contain parameter placeholders: \{\{.parameterName\}\} |  | Optional: \{\} <br /> |
| `agentRef` _[StepAgentRef](#stepagentref)_ | AgentRef references an Agent CRD for AI-powered step execution<br />AdditionalPrompts can contain parameter placeholders: \{\{.parameterName\}\} |  | Optional: \{\} <br /> |
| `mcpToolCall` _[StepMCPToolCall](#stepmcptoolcall)_ | MCPToolCall defines a direct MCP tool invocation<br />Argument values can contain parameter placeholders: \{\{.parameterName\}\} |  | Optional: \{\} <br /> |
| `workflowRef` _[StepWorkflowRef](#stepworkflowref)_ | WorkflowRef references another workflow to execute as a sub-workflow<br />Input values can contain parameter placeholders: \{\{.parameterName\}\} |  | Optional: \{\} <br /> |
| `externalAgentRef` _[StepExternalAgentRef](#stepexternalagentref)_ | ExternalAgentRef defines a call to an external A2A-compatible agent service<br />Prompt can contain parameter placeholders: \{\{.parameterName\}\} |  | Optional: \{\} <br /> |


#### StepWorkflowRef



StepWorkflowRef references another workflow to execute as a sub-workflow



_Appears in:_
- [Step](#step)
- [StepForEachStep](#stepforeachstep)
- [StepTemplateStep](#steptemplatestep)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the name of the Workflow template to execute |  | Required: \{\} <br /> |
| `namespace` _string_ | Namespace is the namespace of the Workflow template<br />Defaults to the parent workflow namespace if not specified |  | Optional: \{\} <br /> |
| `inputs` _object (keys:string, values:string)_ | Inputs provides input values for the referenced workflow<br />Keys match input names defined in the referenced Workflow template<br />Values are CEL expressions that are evaluated in the parent workflow context |  | Optional: \{\} <br /> |


#### TransportConfig



TransportConfig defines the transport mechanism for MCP server connection



_Appears in:_
- [MCPServerSpec](#mcpserverspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `type` _string_ | Type is the transport type<br />Options: stdio, http, sse |  | Enum: [stdio http sse] <br />Required: \{\} <br /> |
| `address` _string_ | Address is the server address (for http/sse) |  | Optional: \{\} <br /> |
| `command` _string array_ | Command is the command to execute (for stdio) |  | Optional: \{\} <br /> |
| `headers` _object (keys:string, values:string)_ | Headers are HTTP headers (for http/sse) |  | Optional: \{\} <br /> |


#### Trigger



Trigger defines an automatic trigger for workflow execution



_Appears in:_
- [WorkflowSpec](#workflowspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `cron` _[CronTrigger](#crontrigger)_ | Cron defines a cron-based trigger |  | Optional: \{\} <br /> |
| `event` _[EventTrigger](#eventtrigger)_ | Event defines a Kubernetes event-based trigger |  | Optional: \{\} <br /> |
| `webhook` _[WebhookTrigger](#webhooktrigger)_ | Webhook defines an HTTP-based trigger that fires a WorkflowRun when a signed<br />POST request is received at /webhooks/\{namespace\}/\{workflowName\}. |  | Optional: \{\} <br /> |


#### TriggerInfo



TriggerInfo contains information about what triggered a WorkflowRun



_Appears in:_
- [WorkflowRunStatus](#workflowrunstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `type` _string_ | Type is the type of trigger (Manual, Cron, Event, Webhook) |  | Enum: [Manual Cron Event Webhook] <br /> |
| `cronSchedule` _string_ | CronSchedule is the cron schedule if triggered by cron |  | Optional: \{\} <br /> |
| `triggeredAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.29/#time-v1-meta)_ | TriggeredAt is when the trigger fired |  | Optional: \{\} <br /> |
| `eventResource` _[EventResourceInfo](#eventresourceinfo)_ | EventResource contains information about the event that triggered this WorkflowRun |  | Optional: \{\} <br /> |
| `webhookRequest` _[WebhookRequestInfo](#webhookrequestinfo)_ | WebhookRequest contains metadata about the HTTP request that triggered this WorkflowRun |  | Optional: \{\} <br /> |


#### Variable



Variable defines a top-level workflow variable
Variables are evaluated before steps execute and are shared across all steps



_Appears in:_
- [WorkflowSpec](#workflowspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the variable name (accessible as variables.<name> in CEL expressions) |  | Required: \{\} <br /> |
| `expression` _string_ | Expression is the CEL expression to evaluate<br />Variables can reference inputs and other variables (evaluated sequentially) |  | Required: \{\} <br /> |


#### WaitForCallbackStep



WaitForCallbackStep defines a step that pauses execution and waits for an external callback.
This enables human-in-the-loop and AI-to-human-to-AI workflows.



_Appears in:_
- [Step](#step)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `timeout` _string_ | Timeout is the maximum duration to wait for the callback (e.g., "24h", "30m").<br />If the callback is not received within this duration, the step fails. |  | Pattern: `^\d+(ns\|us\|ms\|s\|m\|h)$` <br />Required: \{\} <br /> |
| `callbackRef` _string_ | CallbackRef is a reference identifier for the callback (used for logging and documentation).<br />This is a semantic label, not a cryptographic identifier. |  | Optional: \{\} <br /> |
| `outputSchema` _[JSON](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.29/#json-v1-apiextensions-k8s-io)_ | OutputSchema defines the expected structure of the callback payload.<br />The callback payload must conform to this schema before the step resumes.<br />Schema is a JSON schema object (properties, type, etc.). |  | Optional: \{\} <br /> |
| `message` _string_ | Message is a human-readable message displayed to users awaiting callback.<br />Can include instructions for calling the callback endpoint. |  | Optional: \{\} <br /> |
| `failurePolicy` _string_ | FailurePolicy determines workflow behavior when the callback timeout is exceeded.<br />Continue: proceed to the next step; the gate resumes with empty outputs (not Failed).<br />Fail: Workflow fails (default) | Fail | Enum: [Continue Fail] <br />Optional: \{\} <br /> |


#### WebhookRateLimit



WebhookRateLimit configures per-workflow rate limiting for webhook requests.



_Appears in:_
- [WebhookTrigger](#webhooktrigger)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `requestsPerMinute` _integer_ | RequestsPerMinute is the maximum number of accepted requests per minute. | 60 | Maximum: 3600 <br />Minimum: 1 <br />Optional: \{\} <br /> |
| `burst` _integer_ | Burst is the maximum number of requests allowed in a short burst above the<br />per-minute average. Accommodates retry storms (e.g. GitHub Actions 3-retry policy). | 10 | Maximum: 100 <br />Minimum: 1 <br />Optional: \{\} <br /> |


#### WebhookRequestInfo



WebhookRequestInfo records metadata about the HTTP request that triggered the run.



_Appears in:_
- [TriggerInfo](#triggerinfo)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `remoteAddr` _string_ | RemoteAddr is the caller's IP address (best-effort; may be proxy IP). |  | Optional: \{\} <br /> |
| `requestId` _string_ | RequestID is a unique ID generated per request for tracing. |  | Optional: \{\} <br /> |


#### WebhookSecretRef



WebhookSecretRef references a Kubernetes Secret containing the HMAC signing key.



_Appears in:_
- [WebhookTrigger](#webhooktrigger)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name of the Kubernetes Secret. |  | MinLength: 1 <br />Required: \{\} <br /> |
| `namespace` _string_ | Namespace of the Secret. In v1, must equal the Workflow's namespace.<br />Cross-namespace references are rejected by the admission webhook. |  | Optional: \{\} <br /> |
| `key` _string_ | Key is the data key within the Secret that holds the HMAC signing key. | hmac-key | Optional: \{\} <br /> |


#### WebhookTrigger



WebhookTrigger defines an HTTP-based trigger that fires a WorkflowRun
when a signed POST request is received at /webhooks/{namespace}/{workflowName}.



_Appears in:_
- [Trigger](#trigger)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `secretRef` _[WebhookSecretRef](#webhooksecretref)_ | SecretRef references a Kubernetes Secret containing the HMAC signing key.<br />The Secret must have a key named "hmac-key" (or the value of Key) whose value<br />is the shared HMAC-SHA256 secret. Minimum 32 bytes. |  | Required: \{\} <br /> |
| `celFilter` _string_ | CELFilter is an optional CEL boolean expression evaluated against the parsed<br />request body (available as `object`). If false or the expression errors,<br />the request is acknowledged (200) but no WorkflowRun is created. |  | Optional: \{\} <br /> |
| `inputMapping` _object (keys:string, values:string)_ | InputMapping maps workflow input names to CEL expressions evaluated against<br />the parsed JSON body (available as `object`). Results are coerced to strings.<br />If omitted, no inputs are passed to the WorkflowRun. |  | Optional: \{\} <br /> |
| `dedupKey` _string_ | DedupKey is a CEL expression evaluated against the request body to extract<br />a deduplication key. Requests with the same key within DedupWindow are dropped. |  | Optional: \{\} <br /> |
| `dedupWindow` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.29/#duration-v1-meta)_ | DedupWindow is the time window for deduplication when DedupKey is set.<br />Defaults to 10 minutes if DedupKey is set and DedupWindow is omitted.<br />Maximum 1 hour — enforced by the admission webhook validator because the kubebuilder<br />validation:Maximum marker does not apply to *metav1.Duration fields (the type marshals<br />as a string like "10m", not a numeric value). Use "1h" or shorter; longer windows may<br />cause excessive memory usage in the dedup cache. |  | Optional: \{\} <br /> |
| `rateLimit` _[WebhookRateLimit](#webhookratelimit)_ | RateLimit configures per-workflow rate limiting on inbound webhook requests. |  | Optional: \{\} <br /> |


#### Workflow



Workflow is the Schema for the workflows API
Workflow is an immutable template that defines steps, inputs, and optional triggers.
It has no execution status - it acts as a reusable blueprint.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `ottoflow.nirmata.io/v1alpha1` | | |
| `kind` _string_ | `Workflow` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.29/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[WorkflowSpec](#workflowspec)_ |  |  |  |


#### WorkflowRef



WorkflowRef references a Workflow template



_Appears in:_
- [WorkflowRunSpec](#workflowrunspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the name of the Workflow template |  | Required: \{\} <br /> |
| `namespace` _string_ | Namespace is the namespace of the Workflow template.<br />Defaults to the WorkflowRun namespace if not specified. |  | Optional: \{\} <br /> |


#### WorkflowRun



WorkflowRun is the Schema for the workflowruns API
WorkflowRun represents an execution instance of a Workflow template.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `ottoflow.nirmata.io/v1alpha1` | | |
| `kind` _string_ | `WorkflowRun` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.29/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[WorkflowRunSpec](#workflowrunspec)_ |  |  |  |
| `status` _[WorkflowRunStatus](#workflowrunstatus)_ |  |  |  |


#### WorkflowRunExecutionSpec



WorkflowRunExecutionSpec configures the runner Job used for in-cluster execution.



_Appears in:_
- [WorkflowRunSpec](#workflowrunspec)
- [WorkflowSpec](#workflowspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `job` _[WorkflowRunJobSpec](#workflowrunjobspec)_ | Job configures the runner Job used to execute this WorkflowRun. |  | Optional: \{\} <br /> |
| `checkpointing` _[CheckpointingConfig](#checkpointingconfig)_ | Checkpointing configures per-step checkpointing for crash recovery.<br />When enabled, the executor writes a ConfigMap checkpoint after each successful step.<br />The controller retries transient pod failures (eviction, node drain) up to<br />MaxRestartAttempts times; deterministic failures (OOMKilled) never retry. |  | Optional: \{\} <br /> |
| `llmCredentialsSecret` _[LLMCredentialsSecretRef](#llmcredentialssecretref)_ | LLMCredentialsSecret overrides the cluster-wide well-known Secret for LLM credentials.<br />When set, the controller injects env vars from this Secret into the runner Job instead of<br />the cluster-wide default configured via --workflow-runner-llm-credentials-secret.<br />Explicit spec.execution.job.env entries always take precedence over injected values. |  | Optional: \{\} <br /> |


#### WorkflowRunExecutionStatus



WorkflowRunExecutionStatus reports the status of the runner Job.



_Appears in:_
- [WorkflowRunStatus](#workflowrunstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `phase` _string_ | Phase is the current execution phase for the runner workload. |  | Optional: \{\} <br /> |
| `jobName` _string_ | JobName is the name of the runner Job for this WorkflowRun. |  | Optional: \{\} <br /> |
| `podName` _string_ | PodName is the active or last-known runner pod name for this WorkflowRun. |  | Optional: \{\} <br /> |
| `message` _string_ | Message provides additional runner Job status information. |  | Optional: \{\} <br /> |
| `startTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.29/#time-v1-meta)_ | StartTime is when runner Job execution started. |  | Optional: \{\} <br /> |
| `completionTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.29/#time-v1-meta)_ | CompletionTime is when runner Job execution completed. |  | Optional: \{\} <br /> |
| `attempts` _integer_ | Attempts counts controller-triggered retries; 0 on the initial execution.<br />Incremented only when a transient pod failure causes the controller to spawn a<br />replacement runner Job. Use Attempts+1 to get the total number of Job spawns. |  | Optional: \{\} <br /> |
| `lastTerminationReason` _string_ | LastTerminationReason records the container termination reason from the most<br />recent failed pod (e.g. OOMKilled, Evicted). Used by the controller to decide<br />whether to retry. |  | Optional: \{\} <br /> |


#### WorkflowRunJobSpec



WorkflowRunJobSpec configures the runner Job pod.



_Appears in:_
- [WorkflowRunExecutionSpec](#workflowrunexecutionspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `image` _string_ | Image overrides the default workflow-runner image. |  | Optional: \{\} <br /> |
| `serviceAccountName` _string_ | ServiceAccountName overrides the default service account for the runner Job. |  | Optional: \{\} <br /> |
| `env` _[EnvVar](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.29/#envvar-v1-core) array_ | Env provides additional environment variables for the runner container. |  | Optional: \{\} <br /> |
| `resources` _[ResourceRequirements](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.29/#resourcerequirements-v1-core)_ | Resources configures requests/limits for the runner container. |  | Optional: \{\} <br /> |
| `volumes` _[Volume](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.29/#volume-v1-core) array_ | Volumes defines additional pod volumes for the runner Job. |  | Optional: \{\} <br /> |
| `volumeMounts` _[VolumeMount](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.29/#volumemount-v1-core) array_ | VolumeMounts defines additional container volume mounts for the runner Job. |  | Optional: \{\} <br /> |
| `nodeSelector` _object (keys:string, values:string)_ | NodeSelector constrains runner Job scheduling to nodes with matching labels. |  | Optional: \{\} <br /> |
| `tolerations` _[Toleration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.29/#toleration-v1-core) array_ | Tolerations configures pod tolerations for the runner Job. |  | Optional: \{\} <br /> |
| `affinity` _[Affinity](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.29/#affinity-v1-core)_ | Affinity configures pod affinity/anti-affinity for the runner Job. |  | Optional: \{\} <br /> |
| `backoffLimit` _integer_ | BackoffLimit is the number of retries before the Job is considered failed.<br />Defaults to 0 when omitted. |  | Optional: \{\} <br /> |
| `ttlSecondsAfterFinished` _integer_ | TTLSecondsAfterFinished controls automatic Job cleanup after completion. |  | Optional: \{\} <br /> |
| `activeDeadlineSeconds` _integer_ | ActiveDeadlineSeconds limits the total runtime of the runner Job. |  | Optional: \{\} <br /> |


#### WorkflowRunPhase

_Underlying type:_ _string_

WorkflowRunPhase represents the phase of a WorkflowRun

_Validation:_
- Enum: [Pending Running Succeeded Failed]

_Appears in:_
- [WorkflowRunStatus](#workflowrunstatus)

| Field | Description |
| --- | --- |
| `Pending` | WorkflowRunPhasePending indicates the workflow is pending execution<br /> |
| `Running` | WorkflowRunPhaseRunning indicates the workflow is currently running<br /> |
| `Succeeded` | WorkflowRunPhaseSucceeded indicates the workflow completed successfully<br /> |
| `Failed` | WorkflowRunPhaseFailed indicates the workflow failed<br /> |


#### WorkflowRunSpec



WorkflowRunSpec defines the desired state of WorkflowRun



_Appears in:_
- [WorkflowRun](#workflowrun)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `workflowRef` _[WorkflowRef](#workflowref)_ | WorkflowRef references the Workflow template to execute |  | Required: \{\} <br /> |
| `inputValues` _object (keys:string, values:string)_ | InputValues provides input values for the referenced workflow template<br />Keys match input names defined in the Workflow template |  | Optional: \{\} <br /> |
| `clusterRef` _[ClusterRef](#clusterref)_ | ClusterRef optionally targets a cluster for this run. When set, resource queries,<br />mutate steps, and CEL resource.* use that cluster. When omitted or local, use the<br />cluster where the controller runs (in-cluster client). This enables multi-cluster<br />workflows by providing a KubeConfig secret as input to the run. |  | Optional: \{\} <br /> |
| `events` _[EventConfig](#eventconfig)_ | Events overrides event emission for this run (defaults to Workflow spec). |  | Optional: \{\} <br /> |
| `execution` _[WorkflowRunExecutionSpec](#workflowrunexecutionspec)_ | Execution configures how the WorkflowRun executes in-cluster. |  | Optional: \{\} <br /> |


#### WorkflowRunStatus



WorkflowRunStatus defines the observed state of WorkflowRun



_Appears in:_
- [WorkflowRun](#workflowrun)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `phase` _[WorkflowRunPhase](#workflowrunphase)_ | Phase represents the current phase of the WorkflowRun |  | Enum: [Pending Running Succeeded Failed] <br />Optional: \{\} <br /> |
| `stepStatuses` _object (keys:string, values:[StepStatus](#stepstatus))_ | StepStatuses tracks the execution status of each step |  | Optional: \{\} <br /> |
| `outputs` _object (keys:string, values:[JSON](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.29/#json-v1-apiextensions-k8s-io))_ | Outputs contains workflow-level outputs evaluated at completion<br />These are defined in the Workflow spec and evaluated using the final context |  | Type: object <br />Optional: \{\} <br /> |
| `startTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.29/#time-v1-meta)_ | StartTime is when the workflow execution started |  | Optional: \{\} <br /> |
| `completionTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.29/#time-v1-meta)_ | CompletionTime is when the workflow execution completed |  | Optional: \{\} <br /> |
| `message` _string_ | Message provides additional information about the workflow status |  | Optional: \{\} <br /> |
| `restartRequired` _boolean_ | RestartRequired indicates that the workflow needs to be restarted.<br />Deprecated: The executor guard that consumed this field has been superseded by<br />per-step checkpointing (see WorkflowRunExecutionSpec.Checkpointing). This field<br />is no longer written by any component and will be removed in a future API version. |  | Optional: \{\} <br /> |
| `trigger` _[TriggerInfo](#triggerinfo)_ | Trigger contains information about what triggered this WorkflowRun |  | Optional: \{\} <br /> |
| `execution` _[WorkflowRunExecutionStatus](#workflowrunexecutionstatus)_ | Execution contains status for the runner Job that executes this WorkflowRun. |  | Optional: \{\} <br /> |
| `pendingCallback` _[CallbackState](#callbackstate)_ | PendingCallback holds the state of an in-progress waitForCallback step.<br />Nil when no callback is pending. |  | Optional: \{\} <br /> |
| `auditSnapshotConfigMap` _string_ | AuditSnapshotConfigMap is the name of the ConfigMap (in this WorkflowRun's<br />namespace) holding a snapshot of the final execution context — the variables<br />and expression outputs the run had computed at its terminal phase (Succeeded<br />or Failed). Unlike the opt-in per-step checkpoint (see<br />WorkflowRunExecutionSpec.Checkpointing), this snapshot is always written once<br />at completion, regardless of whether checkpointing is enabled, so that what a<br />run actually saw/computed can be inspected after the fact. The ConfigMap is<br />owned by this WorkflowRun and is garbage-collected together with it (see<br />Workflow.spec.retentionMinutes/maxAllowed); it is not deleted separately. |  | Optional: \{\} <br /> |
| `auditSnapshotError` _string_ | AuditSnapshotError is set when writing AuditSnapshotConfigMap failed at<br />terminal phase (e.g. the snapshot could not be persisted within the retry<br />window, or a validation error was hit). Empty when the write succeeded or<br />hasn't been attempted yet. Surfaced here (and as a Warning event) so the<br />absence of AuditSnapshotConfigMap is distinguishable from "nothing went<br />wrong" rather than silently looking identical to pre-audit-snapshot behavior. |  | Optional: \{\} <br /> |


#### WorkflowSpec



WorkflowSpec defines the desired state of Workflow



_Appears in:_
- [Workflow](#workflow)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `inputs` _[Input](#input) array_ | Inputs defines input parameters for the workflow template.<br />Values are provided when creating a WorkflowRun. |  | Optional: \{\} <br /> |
| `mcpTool` _[MCPTool](#mcptool)_ | MCPTool exposes this workflow to MCP clients as a callable tool.<br />Omitted or disabled keeps the workflow off the MCP endpoint entirely. |  | Optional: \{\} <br /> |
| `variables` _[Variable](#variable) array_ | Variables defines top-level CEL expressions that are evaluated before steps execute.<br />Variables are shared across all steps and can reference inputs and other variables.<br />Variables are evaluated sequentially, allowing later variables to reference earlier ones.<br />Access variables in expressions using: variables.<name> |  | Optional: \{\} <br /> |
| `steps` _[Step](#step) array_ | Steps defines the workflow steps to execute. |  | MinItems: 1 <br /> |
| `outputs` _[Output](#output) array_ | Outputs defines workflow-level outputs that are evaluated at completion<br />and added to the WorkflowRun status for observability |  | Optional: \{\} <br /> |
| `triggers` _[Trigger](#trigger) array_ | Triggers defines automatic triggers for this workflow template<br />When triggers fire, they automatically create WorkflowRun instances<br />Multiple triggers can be defined (OR logic - any trigger can fire) |  | Optional: \{\} <br /> |
| `events` _[EventConfig](#eventconfig)_ | Events configures Kubernetes event emission for this workflow. |  | Optional: \{\} <br /> |
| `run` _[RunPolicy](#runpolicy)_ | Run configures retention and scale limits for WorkflowRuns of this workflow. |  | Optional: \{\} <br /> |
| `celCostLimit` _integer_ | CELCostLimit is the maximum cost budget for CEL expression evaluation in this workflow.<br />If unset, a default (~2MB-equivalent) is used. Increase for workflows that do large data joins. |  | Optional: \{\} <br /> |
| `execution` _[WorkflowRunExecutionSpec](#workflowrunexecutionspec)_ | Execution is the default execution spec for WorkflowRuns created from this Workflow (e.g. by cron).<br />When the scheduler creates a WorkflowRun, it copies this into the run's spec so runner Jobs get<br />the same env, volumes, etc. Use this to inject Nirmata LLM credentials (valueFrom.secretKeyRef). |  | Optional: \{\} <br /> |
| `executionLimits` _[ExecutionLimits](#executionlimits)_ | ExecutionLimits configures concurrency and rate limits for this workflow. |  | Optional: \{\} <br /> |
| `expose` _[ExposeSpec](#exposespec)_ | Expose publishes this Workflow to external agent surfaces (e.g. kagent). Presence of expose.kagent opts in. |  | Optional: \{\} <br /> |


