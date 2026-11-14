# OttoFlow Workflow Controller - Requirements and Design Document

## Table of Contents
1. [Overview](#overview)
2. [Goals and Objectives](#goals-and-objectives)
3. [Requirements](#requirements)
4. [Architecture](#architecture)
5. [API Design](#api-design)
6. [Implementation Details](#implementation-details)
7. [Examples](#examples)
8. [Custom Resource Definitions](#custom-resource-definitions) (see [API Reference](../user/reference/api/README.md) for complete specifications)
9. [Prometheus Metrics (Design)](#prometheus-metrics-design)
10. [Future Enhancements](#future-enhancements)

**Other dev docs**: [build.md](build.md) (build and test with kind), [release.md](release.md) (creating a release), [DESIGN_GITOPS_EVENT_TRIGGERS.md](DESIGN_GITOPS_EVENT_TRIGGERS.md) (CEL inputMapping, celFilter, dedup for GitOps triggers — v0.7.0).

---

## Overview

This document describes the requirements and design for the OttoFlow Kubernetes workflow controller that implements a Directed Acyclic Graph (DAG) execution model using kubebuilder. The controller enables declarative workflow orchestration where steps can produce outputs that are consumed by dependent steps, creating a dependency graph that is automatically resolved and executed.

### Two-Resource Model

The design uses a separation between workflow definitions and workflow executions:

- **Workflow (Template)**: Immutable workflow definition containing steps, inputs, and optional triggers. Similar to Kubernetes CronJob or Argo WorkflowTemplate.
- **WorkflowRun (Instance)**: Execution instance that references a Workflow template, provides input values, and tracks execution status. Similar to Kubernetes Job or Argo Workflow.

This separation provides:
- **Reusability**: One Workflow template can be executed multiple times via different WorkflowRuns
- **Clarity**: Clear distinction between definition and execution
- **Observability**: Each execution has its own status and history
- **Auditability**: Trigger information is tracked per execution

### Key Concepts

- **Workflow**: A workflow template that defines steps, inputs, and optional triggers. Acts as a reusable blueprint.
- **WorkflowRun**: An execution instance of a Workflow. Contains input values, trigger information, and execution status.
- **Step**: An atomic unit of work that can produce outputs and depend on outputs from other steps
- **Step Types**: Steps can be expression-based (default), workflow references, agent-powered, MCP tool calls, A2A agent calls, or OpenReports.io report generation
- **Agent Step**: LLM-powered step that references an Agent CRD, which defines the prompt, model, MCP tools, and service account
- **MCP Tool Call Step**: Direct invocation of MCP tools as a client, with CEL-resolved arguments
- **MCPServer**: A CustomResourceDefinition for declaring MCP servers with transport configuration, authentication, and environment variables
- **A2A Agent Call Step**: Calls to other agents using the A2A (Agent-to-Agent) protocol with message-based communication
- **OpenReports.io Report Step**: Generates automated reports using OpenReports.io Report CRDs with CEL-resolved parameters
- **Workflow Reference**: A step type that executes another workflow as a sub-workflow
- **Inputs**: Parameters defined by a workflow template that can be provided when creating a WorkflowRun
- **Shared Context**: A key-value store that accumulates outputs from all completed steps and includes workflow inputs
- **DAG**: Directed Acyclic Graph representing step dependencies, built only from explicit `dependsOn` declarations
- **Workflow Executor**: Component that builds the dependency graph and orchestrates step execution
- **Execution modes**: **Cluster** – the controller creates a Job per WorkflowRun; the runner pod executes the workflow. **Local** – the CLI runs the workflow in-process when `--workflow-dir` is set, using a fake client populated from local YAML for control-plane objects and the current kubeconfig for target-cluster operations; supports WorkflowRef (inline) and in-process agent steps.
- **CEL Expressions**: Common Expression Language expressions that can query Kubernetes resources and perform computations using Kyverno CEL libraries

---

## Goals and Objectives

### Primary Goals

1. **Declarative Workflow Definition**: Enable users to define workflows declaratively using Kubernetes Custom Resources
2. **Dependency Resolution**: Resolve and execute steps in dependency order, based on explicit `dependsOn` declarations
3. **Ordered Execution**: Execute steps in dependency order, one ready step at a time; `forEach` items run concurrently
4. **Output Sharing**: Provide a mechanism for steps to share data through a shared context
5. **Kubernetes Native**: Leverage kubebuilder and controller-runtime for Kubernetes-native implementation

### Design Principles

- **Idempotency**: All operations must be idempotent to support Kubernetes reconciliation patterns
- **Observability**: Provide clear status and events for workflow and step execution
- **Extensibility**: Design for future enhancements (retry, timeout, conditions, etc.)
- **Simplicity**: Keep the initial implementation focused on core functionality

---

## Requirements

### Functional Requirements

#### FR1: Workflow Resource Definition
- **FR1.1**: Users must be able to define workflow templates using a Workflow CustomResourceDefinition (CRD)
- **FR1.2**: A workflow template must contain one or more steps
- **FR1.3**: Each workflow template must have a unique name within a namespace
- **FR1.4**: Workflow templates must support standard Kubernetes metadata (labels, annotations, etc.)
- **FR1.5**: Workflow templates do not have status - they are immutable definitions

#### FR1A: WorkflowRun Resource Definition
- **FR1A.1**: Users must be able to create WorkflowRun instances using a WorkflowRun CustomResourceDefinition (CRD)
- **FR1A.2**: Each WorkflowRun must reference a Workflow template
- **FR1A.3**: WorkflowRuns must provide input values for the referenced workflow template
- **FR1A.4**: WorkflowRuns must track execution status, step statuses, and outputs
- **FR1A.5**: WorkflowRuns must record trigger information (if triggered automatically)
- **FR1A.6**: Multiple WorkflowRuns can reference the same Workflow template

#### FR1B: Agent Resource Definition
- **FR1B.1**: Users must be able to define AI agents using an Agent CustomResourceDefinition (CRD)
- **FR1B.2**: Each Agent must specify prompt template, modelProvider, and modelName
- **FR1B.3**: Agents must support MCP tools configuration (list of allowed MCP tools)
- **FR1B.4**: Agents must support output extraction configuration (JSON, text, regex patterns)
- **FR1B.5**: Agents must support serviceAccount field for per-agent RBAC
- **FR1B.6**: Agents must support executorImage field for custom executor images
- **FR1B.7**: Agents must support executionMode field (enum: `service`, future: `sandbox`, `substrate`) - defaults to `service`
- **FR1B.8**: Agents must support serviceName and serviceNamespace fields for AgentExecutor Service configuration
- **FR1B.9**: Agents must support resources field for resource requests/limits (future: for sandbox mode)
- **FR1B.10**: Agents must be namespace-scoped
- **FR1B.11**: Agent steps must reference Agent CRDs by name
- **FR1B.12**: The controller must validate Agent references in workflow steps
- **FR1B.13**: All execution configuration (executionMode, serviceName, serviceNamespace, resources, serviceAccount) is defined in Agent CRD only - no step-level overrides

#### FR1C: MCPServer Resource Definition
- **FR1C.1**: Users must be able to define MCP servers using an MCPServer CustomResourceDefinition (CRD)
- **FR1C.2**: Each MCPServer must specify the transport type (stdio, http, sse)
- **FR1C.3**: MCPServers must support connection configuration (address, command, authentication)
- **FR1C.4**: MCPServers must support authentication configuration (bearer token, API key, basic auth, OAuth2) with Secret references
- **FR1C.5**: MCPServers must be namespace-scoped or cluster-scoped
- **FR1C.6**: MCP tool call steps must reference MCPServer resources by name
- **FR1C.7**: The controller must validate MCPServer references in workflow steps

#### FR2: Step Definition
- **FR2.1**: Each step must have a unique name within the workflow
- **FR2.2**: Each step must have a `message` field for human-readable description
- **FR2.3**: Each step must support an `outputs` field that defines key-value pairs written to shared context
- **FR2.4**: Steps must be able to reference outputs from other steps using CEL expressions
- **FR2.5**: Steps that reference outputs from other steps must wait for those steps to complete
- **FR2.6**: Steps can be of different types: expression-based (default), workflow reference, agent, mcpToolCall, or externalAgentRef (external A2A agent)

#### FR2A: Agent Step Type
- **FR2A.1**: Steps must support an `agentRef` field that references an Agent CRD for LLM-powered execution
- **FR2A.2**: Agent steps must reference Agent CRDs by name (and optionally namespace)
- **FR2A.3**: Agent steps must support an optional `additionalPrompts` field (array) to append prompts to the agent's system prompt
- **FR2A.4**: Each additional prompt in a step is evaluated as a single CEL expression with access to workflow context (see [Variable Access](../user/reference/cel-reference.md#variable-access)), so plain text must be a quoted CEL string
- **FR2A.5**: Agent steps execute via AgentExecutor Service using A2A protocol (internal subset)
- **FR2A.6**: Agent execution uses execution settings exclusively from Agent CRD (executionMode, serviceName, serviceNamespace, resources, serviceAccount)
- **FR2A.7**: No step-level overrides - all execution configuration must be defined in Agent CRD
- **FR2A.8**: Agent execution must be able to call MCP tools configured in the Agent CRD during LLM interaction
- **FR2A.9**: Agent outputs must be extractable from LLM responses using output extraction patterns from the Agent CRD
- **FR2A.10**: Communication between executor and AgentExecutor Service:
  - Always uses A2A protocol with streaming mode (internal subset)
  - Agent cards derived from Agent CRDs for coordination
  - No agent discovery (agents known via CRDs)

#### FR2B: MCP Tool Call Step Type
- **FR2B.1**: Steps must support an `mcpToolCall` field for direct MCP tool invocation
- **FR2B.2**: MCP tool call steps must specify `server` and `tool` to identify the MCP tool
- **FR2B.3**: MCP tool call steps must support `arguments` field containing tool arguments as CEL expressions
- **FR2B.4**: Tool arguments must be resolved using workflow context (inputs, previous step outputs, expressions) before invocation
- **FR2B.5**: MCP tool call results must be stored in shared context and accessible to subsequent steps
- **FR2B.6**: MCP tool call steps must handle errors and propagate failures appropriately
- **FR2B.7**: Tool call results must be available in output expressions as `toolResult` variable

#### FR2C: A2A Agent Call Step Type
- **FR2C.1**: Steps must support an `externalAgentRef` field for calling other agents via the A2A (Agent-to-Agent) protocol (`protocol: a2a`)
- **FR2C.2**: External agent steps must specify `url` to identify the target A2A agent endpoint
- **FR2C.3**: External agent steps must support `prompt`, a single CEL expression resolved to the task message
- **FR2C.4**: Structured message parts are not supported; the message is the `prompt` text
- **FR2C.5**: The prompt must be resolved using workflow context (inputs, previous step outputs, expressions) before sending
- **FR2C.6**: External agent steps must poll unfinished tasks for completion (`tasks/get`)
- **FR2C.7**: External agent results must be stored in shared context and accessible to subsequent steps
- **FR2C.8**: External agent results must be available in output expressions as the `a2aResult` map
- **FR2C.9**: External agent steps must support authentication and TLS configuration (`auth.secretRef`, `caSecretRef`, `timeout`, `allowInsecureHTTP`)

#### FR2D: OpenReports.io Report Generation Step Type
- **FR2D.1**: Steps must support an `openReport` field for generating reports using OpenReports.io
- **FR2D.2**: OpenReports.io report steps must specify `reportName` to identify the report template
- **FR2D.3**: Report steps must support `namespace` to specify where the OpenReports.io Report CRD should be created
- **FR2D.4**: Report steps must support `parameters` field for passing CEL-resolved data to the report
- **FR2D.5**: Report parameters must be resolved using workflow context (inputs, previous step outputs, expressions) before report creation
- **FR2D.6**: Report steps must create OpenReports.io Report CRD instances and wait for completion
- **FR2D.7**: Report results (report URL, status, data) must be stored in shared context and accessible to subsequent steps
- **FR2D.8**: Report results must be available in output expressions as `reportResult` variable
- **FR2D.9**: Report steps must support timeout configuration for report generation
- **FR2D.10**: Report steps must handle report failures gracefully with configurable failure policies

#### FR3: Dependency Resolution
- **FR3.1**: The controller must build a DAG from explicit `dependsOn` declarations (dependencies are not inferred from CEL expressions)
- **FR3.2**: The controller must detect circular dependencies and reject invalid workflows
- **FR3.3**: Steps without dependencies must be executable immediately
- **FR3.4**: Steps with dependencies must wait for all dependencies to finish (succeed, be skipped, or fail with `failurePolicy: Continue`)

#### FR4: Execution Model
- **FR4.1**: Steps execute in dependency order, one ready step at a time; `forEach` items run concurrently
- **FR4.2**: Step execution must be tracked and persisted in the workflow status
- **FR4.3**: Failed steps must prevent dependent steps from executing (unless failurePolicy is Continue)
- **FR4.4**: The workflow must transition through states: Pending → Running → Succeeded/Failed
- **FR4.5**: Steps must support retry policies with configurable attempts, backoff strategies, and retry conditions (not implemented: the step-level `retry` field is accepted but not applied)
- **FR4.6**: Steps must support timeout configuration to limit execution duration (not implemented: the step-level `timeout` field is accepted but not applied; `waitForCallback.timeout`, `externalAgentRef.timeout` and the agent exec HTTP timeout are applied)
- **FR4.7**: Steps must support failure policies (Fail or Continue) to control workflow behavior on step failure
- **FR4.8**: Retry attempts must be tracked in step status (not implemented: step status `retryCount`, `lastRetryTime` and `nextRetryTime` are never written)
- **FR4.9**: Steps must support conditional execution via `matchConditions` using CEL expressions (similar to Kubernetes ValidatingAdmissionPolicy matchConditions)
- **FR4.10**: Skipped steps must be marked as skipped in status and not prevent dependent steps from executing

#### FR5: Shared Context
- **FR5.1**: Outputs from completed steps must be stored in a shared context
- **FR5.2**: The shared context must be accessible to all steps during execution
- **FR5.3**: Output references must be resolved before step execution begins
- **FR5.4**: Output values must support string, number, boolean, map, and array types with type preservation
- **FR5.5**: Shared context must be stored in-memory during workflow execution for performance
- **FR5.6**: Workflow-level outputs must be exposed directly in WorkflowRun status for observability
- **FR5.7**: Workflows must be idempotent - if the controller restarts, workflows restart from the beginning

#### FR6: Status and Observability
- **FR6.1**: Workflow status must include overall state (Pending, Running, Succeeded, Failed)
- **FR6.2**: Each step status must include state (Pending, Running, Succeeded, Failed, Skipped)
- **FR6.3**: Step execution start and completion times must be tracked
- **FR6.4**: Error messages from failed steps must be captured and exposed
- **FR6.5**: Kubernetes events must be emitted for workflow and step state transitions
- **FR6.6**: Workflow-level outputs must be exposed in WorkflowRun status for observability
- **FR6.7**: WorkflowRun status must include RestartRequired flag to indicate if workflow needs restart due to controller restart

#### FR7: CEL Expression Support
- **FR7.1**: Each step must support an `expressions` field containing CEL expressions
- **FR7.2**: CEL expressions must be evaluated sequentially (in order) before step execution begins
- **FR7.3**: Later expressions in the same step must be able to reference earlier expressions using `expressions.<name>`
- **FR7.4**: CEL expressions must have access to the complete suite of Kyverno CEL libraries (implemented via [Kyverno SDK CEL](https://github.com/kyverno/sdk/tree/main/cel)):
  - Resource library (resource.Get, resource.List, resource.Post)
  - HTTP library (http.Get, http.Post)
  - User library (parseServiceAccount)
  - Image library (image parsing and analysis)
  - ImageData library (OCI registry metadata; image.GetMetadata)
  - GlobalContext library (shared cluster variables)
  - Hash library (md5, sha1, sha256)
  - Math library (math.round)
  - Random library (random string generation)
  - Transform library (listObjToMap)
  - JSON library (json.unmarshal, json.marshal)
  - YAML library (yaml.parse)
  - Time functions (time.now, time.truncate, time.toCron)
  - X509 library (certificate decoding)
- **FR7.5**: CEL expressions must have access to shared context (outputs from previous steps, inputs)
- **FR7.6**: CEL expression results must be available for use in step outputs and subsequent expressions
- **FR7.7**: Invalid CEL expressions must cause step execution to fail with clear error messages
- **FR7.8**: CEL expressions must be evaluated in the context of the workflow's namespace

#### FR8: Workflow Inputs
- **FR8.1**: Each workflow must support an `inputs` section that defines input parameters
- **FR8.2**: Input parameters must be available in the shared context before step execution begins
- **FR8.3**: Input values can be provided when creating a workflow instance
- **FR8.4**: Inputs must be accessible to all steps via `inputs.<name>` in CEL expressions
- **FR8.5**: Missing required inputs must cause workflow validation to fail

#### FR9: Workflow References
- **FR9.1**: Steps must be able to reference other workflows as a step type using `workflowRef`
- **FR9.2**: When referencing a workflow, input values must be provided for each input parameter
- **FR9.3**: Input values can be CEL expressions that reference parent workflow context
- **FR9.4**: Referenced workflows execute as sub-workflows, inline in the parent's runner process and produce outputs
- **FR9.5**: No separate WorkflowRun or Job is created for a sub-workflow, so there is no owner reference to the parent
- **FR9.6**: Sub-workflow outputs must be accessible to parent workflow steps via `variables.<key>`
- **FR9.7**: Sub-workflow execution must be tracked in the parent workflow's status
- **FR9.8**: Failed sub-workflows must cause the parent workflow step to fail
- **FR9.9**: Sub-workflows execute in the same namespace as the referenced workflow (or specified namespace)

#### FR10: Workflow Triggers
- **FR10.1**: Workflow templates must support a `triggers` section for automatic execution
- **FR10.2**: Workflow templates must support cron-based triggers with standard cron syntax
- **FR10.3**: Workflow templates must support Kubernetes event-based triggers
- **FR10.4**: Event triggers must support filtering by resource type, namespace, operation (CREATE, UPDATE, DELETE)
- **FR10.5**: Event triggers must support label and field selectors for resource filtering
- **FR10.6**: Multiple triggers can be defined for a single workflow template (OR logic)
- **FR10.7**: When a trigger fires, a new WorkflowRun must be created
- **FR10.8**: Triggered WorkflowRuns must include event context (resource details) in inputs
- **FR10.9**: Cron triggers must respect timezone configuration
- **FR10.10**: Trigger evaluation must be idempotent to prevent duplicate WorkflowRun instances
- **FR10.11**: WorkflowRuns created by triggers must reference the triggering workflow template
- **FR10.12**: WorkflowRuns must record trigger metadata (trigger type, trigger time, event details)

### Non-Functional Requirements

#### NFR1: Performance
- **NFR1.1**: The controller must handle workflows with up to 1000 steps
- **NFR1.2**: Dependency resolution must complete in < 1 second for typical workflows
- **NFR1.3**: Step execution should not be blocked by controller processing

#### NFR2: Reliability
- **NFR2.1**: The controller must be resilient to pod restarts
- **NFR2.2**: Workflow state must be persisted in Kubernetes resources
- **NFR2.3**: The controller must support reconciliation loops for state recovery

#### NFR3: Usability
- **NFR3.1**: Workflow definitions must be human-readable YAML
- **NFR3.2**: Error messages must clearly indicate the cause of failures
- **NFR3.3**: kubectl commands must provide meaningful output

#### NFR4: Security
- **NFR4.1**: The controller must respect Kubernetes RBAC
- **NFR4.2**: Workflow execution must not bypass Kubernetes security policies

---

## Architecture

### High-Level Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                    Kubernetes Cluster                        │
│                                                               │
│  ┌──────────────────────────────────────────────────────┐  │
│  │              Workflow Controller                      │  │
│  │  ┌──────────────┐  ┌──────────────┐  ┌───────────┐  │  │
│  │  │   Watcher    │  │   Reconciler │  │  Executor │  │  │
│  │  └──────────────┘  └──────────────┘  └───────────┘  │  │
│  │  ┌────────────────────────────────────────────────┐ │  │
│  │  │         CEL Expression Evaluator                │ │  │
│  │  │  (Kyverno CEL Libraries Integration)          │ │  │
│  │  └────────────────────────────────────────────────┘ │  │
│  │  ┌────────────────────────────────────────────────┐ │  │
│  │  │         Trigger Manager                        │ │  │
│  │  │         (Event Watcher +                       │ │  │
│  │  │          Leader Election)                      │ │  │
│  │  └────────────────────────────────────────────────┘ │  │
│  │  ┌────────────────────────────────────────────────┐ │  │
│  │  │         Cron Scheduler                        │ │  │
│  │  │         (In-process, Leader-elected)           │ │  │
│  │  └────────────────────────────────────────────────┘ │  │
│  │         │                 │                 │         │  │
│  └─────────┼─────────────────┼─────────────────┼─────────┘  │
│            │                 │                 │            │
│  ┌─────────▼─────────────────▼─────────────────▼─────────┐  │
│  │              Workflow Custom Resource                  │  │
│  │  ┌─────────────────────────────────────────────────┐ │  │
│  │  │ Spec: Steps, Dependencies                       │ │  │
│  │  └─────────────────────────────────────────────────┘ │  │
│  │  ┌─────────────────────────────────────────────────┐ │  │
│  │  │ Status: State, Step Statuses, Shared Context   │ │  │
│  │  └─────────────────────────────────────────────────┘ │  │
│  └───────────────────────────────────────────────────────┘  │
│                                                               │
│  ┌──────────────────────────────────────────────────────┐  │
│  │              Step Execution Pods                      │  │
│  │  (Future: Steps may execute as Kubernetes Jobs)      │  │
│  └──────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────┘
```

### Component Responsibilities

#### Workflow Controller
- **Watcher**: Monitors Workflow and WorkflowRun CRD instances and triggers reconciliation
- **Workflow Reconciler**: Implements reconciliation for Workflow templates, registers triggers
- **WorkflowRun Reconciler**: Implements reconciliation for WorkflowRun instances, executes workflows
- **Executor**: Builds the DAG, resolves dependencies, and orchestrates step execution
- **CEL Expression Evaluator**: Evaluates CEL expressions using Kyverno CEL libraries, providing access to Kubernetes resources and shared context
- **Trigger Manager**: Manages workflow triggers:
  - **Event Triggers**: Uses leader election to ensure only leader controller watches events (prevents duplicates)
- **Cron Scheduler** ([`scheduler.go`](../../internal/workflow/controller/scheduler.go)): In-process cron engine (robfig/cron) running under leader election. Directly creates WorkflowRun instances when schedules fire; enforces concurrency policies (Allow, Forbid, Replace) and supports IANA timezones
- **OpenReports.io Integration**: Creates and manages OpenReports.io Report CRDs for automated report generation

#### Workflow Resource (Template)
- **Spec**: Contains the declarative workflow definition (steps, inputs, triggers, etc.)
- **Status**: None - workflows are templates without execution status

#### WorkflowRun Resource (Execution Instance)
- **Spec**: References a Workflow template, provides input values, includes trigger information
- **Status**: Contains execution state, step statuses, accumulated outputs, and trigger metadata

### Data Flow

#### Manual WorkflowRun Execution
1. **Workflow Template Creation**: User creates a Workflow CRD instance (template definition)
2. **WorkflowRun Creation**: User creates a WorkflowRun CRD instance that references the Workflow template
3. **Input Validation**: Controller validates that all required inputs are provided in WorkflowRun
4. **Context Initialization**: Input values populate the initial shared context
5. **Reconciliation**: Controller reconciles the WorkflowRun
6. **Validation**: Controller validates the workflow template (no cycles, valid references, valid workflow refs)
7. **DAG Construction**: Executor builds the dependency graph from the referenced Workflow template
8. **Execution**: Executor identifies ready steps and executes them (including sub-workflows)
9. **Output Collection**: Completed step outputs are stored in WorkflowRun status
10. **Dependency Resolution**: Dependent steps become ready as outputs become available
11. **Completion**: WorkflowRun completes when all steps finish

#### Triggered WorkflowRun Execution
1. **Workflow Template Creation**: User creates a Workflow CRD instance with triggers defined
2. **Trigger Registration**: Controller registers cron schedules and event watchers for the Workflow template
3. **Trigger Firing**: Cron schedule fires OR Kubernetes event matches trigger criteria
4. **WorkflowRun Creation**: Controller creates a new WorkflowRun instance with:
   - Reference to the Workflow template
   - Input values from trigger (for event triggers, mapped from event data)
   - Trigger metadata (trigger type, time, event details)
   - Owner reference to the template workflow
   - Unique name (e.g., `<template-name>-<timestamp>` or `<template-name>-<event-uid>`)
5. **WorkflowRun Execution**: New WorkflowRun follows manual execution flow (steps 3-11 above)

---

## API Design

The API design follows Kubernetes-native patterns using Custom Resource Definitions (CRDs). For complete CRD specifications and schema definitions, see [API Reference](../user/reference/api/README.md).

### Overview

The OttoFlow workflow controller defines four main CRDs:

1. **Workflow**: Immutable workflow templates that define steps, inputs, and optional triggers
2. **WorkflowRun**: Execution instances that reference a Workflow template and track execution status
3. **Agent**: AI agent configuration for agent steps (prompt, model, MCP tools, service account)
4. **MCPServer**: MCP server configuration for agent and tool call steps

### Key Concepts

- **Workflow Template**: A reusable workflow definition with steps, inputs, and optional triggers
- **WorkflowRun Instance**: An execution of a Workflow template with specific input values and execution status
- **Step Types**: Steps can be expression-based, workflow references, agent-powered (via Agent CRD references), MCP tool calls, A2A agent calls, or OpenReports.io report generation
- **Shared Context**: An in-memory storage mechanism for passing data between steps during execution
- **CEL Expressions**: Common Expression Language expressions for querying Kubernetes resources and performing computations

For detailed CRD schemas and field specifications, see [API Reference](../user/reference/api/README.md).

---

## Examples

For worked workflows, see [`samples/workflows/`](../../samples/workflows/) (validated in CI by `ottoflow validate --workflow-dir samples`, the `validate-samples` job in `ci.yaml`) and the [Workflow API reference](../user/reference/api/workflow.md).

### CEL Expression Context

CEL expressions have access to the **complete suite of Kyverno CEL libraries** (via [Kyverno SDK CEL](https://github.com/kyverno/sdk/tree/main/cel)) and **Kubernetes CEL libraries**, providing powerful expression capabilities for workflow logic.

#### Kyverno CEL Libraries

1. **Resource Library**:
   - `resource.Get(apiVersion, kind, namespace, name)` - Fetch a single Kubernetes resource
   - `resource.List(apiVersion, kind, namespace, labelSelector)` - List Kubernetes resources
   - `resource.Post(apiVersion, kind, namespace, name, subresource, body)` - Perform API operations (e.g., SubjectAccessReview)

2. **HTTP Library**:
   - `http.Get(url)` - Fetch data from HTTP/HTTPS endpoints
   - `http.Post(url, body, headers)` - Send POST requests to HTTP/HTTPS endpoints
   - Supports CA bundle validation for HTTPS endpoints

3. **User Library**:
   - `parseServiceAccount(username)` - Parse service account information from username

4. **Image Library**:
   - `image(imageString)` - Convert image string to image object
   - `isImage(imageString)` - Check if string is a valid image
   - `image(imageString).registry()` - Get image registry
   - `image(imageString).repository()` - Get image repository path
   - `image(imageString).identifier()` - Get image identifier (tag or digest)
   - `image(imageString).tag()` - Get tag portion
   - `image(imageString).digest()` - Get digest portion
   - `image(imageString).containsDigest()` - Check if image includes digest

5. **ImageData Library**:
   - `image.GetMetadata(imageString)` - Fetch OCI registry metadata (architecture, OS, digests, tags, layers, etc.)

6. **GlobalContext Library**:
   - `globalContext.Get(entryName, key)` - Access shared variables from GlobalContextEntry resources

7. **Hash Library**:
   - `md5(value)` - Compute MD5 hash
   - `sha1(value)` - Compute SHA1 hash
   - `sha256(value)` - Compute SHA256 hash

8. **Math Library**:
   - `math.round(number, precision)` - Round numbers to specific precision (positive or negative)

9. **Random Library**:
   - `random()` - Generate random 8-character alphanumeric string
   - `random(pattern)` - Generate random string matching regex pattern

10. **Transform Library**:
    - `listObjToMap(list1, list2, keyField, valueField)` - Merge two lists into a map

11. **JSON Library**:
    - `json.unmarshal(jsonString)` - Parse JSON string into structured data

12. **YAML Library**:
    - `yaml.parse(yamlString)` - Parse YAML string into structured data

13. **Time Functions**:
    - `time.now()` - Get current timestamp
    - `time.truncate(timestamp, duration)` - Truncate timestamp to duration
    - `time.toCron(timestamp)` - Convert timestamp to cron format
    - `duration(string)` - Create duration (e.g., "24h", "7d")
    - `timestamp(string)` - Parse timestamp string

14. **X509 Library**:
    - `x509.decode(certPEM)` - Decode X.509 certificate or CSR from PEM format
    - Access certificate fields: IsCA, NotBefore, NotAfter, Issuer, Subject, DNSNames, KeyUsage, etc.

15. **Kubernetes CEL Libraries** (from `k8s.io/apiserver/pkg/cel/library`):
    - **List Library**: `indexOf()`, `lastIndexOf()`, `min()`, `max()`, `sum()`, `isSorted()`
    - **Regex Library**: `find()`, `findAll()` for advanced regex operations
    - **URL Library**: `isURL()`, `url().getScheme()`, `url().getHost()`, `url().getPort()`, `url().getEscapedPath()`, `url().getQuery()`
    - **IP Address Library**: `isIP()`, `ip().isCanonical()`, `ip().family()`, `ip().isUnspecified()`, `ip().isLoopback()`, `ip().isLinkLocalMulticast()`, `ip().isLinkLocalUnicast()`, `ip().isGlobalUnicast()`
    - **CIDR Library**: `cidr()`, `isCIDR()`, `cidr().containsIP()`, `cidr().containsCIDR()`, `cidr().ip()`, `cidr().masked()`, `cidr().prefixLength()`
    - **Format Library**: `format.dns1123Label()`, `format.dns1123Subdomain()`, `format.dns1035Label()`, `format.qualifiedName()`, `format.uri()`, `format.uuid()`, `format.byte()`, `format.date()`, `format.datetime()`, `format.<name>().validate(string)`
    - **Quantity Library**: `quantity()`, `isQuantity()`, `quantity().isInteger()`, `quantity().asInteger()`, `quantity().asApproximateFloat()`, `quantity().sign()`, `quantity().add()`, `quantity().sub()`, `quantity().isLessThan()`, `quantity().isGreaterThan()`, `quantity().compareTo()`
    - **Semver Library**: `semver()`, `isSemver()`, `semver().major()`, `semver().minor()`, `semver().patch()`, `semver().isLessThan()`, `semver().isGreaterThan()`, `semver().compareTo()`

16. **Standard CEL Functions**:
    - String operations (`replace()`, `slice()`, `split()`, `join()`, `lowerAscii()`, `upperAscii()`, `trim()`, `charAt()`, `indexOf()`, `lastIndexOf()`, `substring()`)
    - List operations (`map()`, `filter()`, `size()`, `has()`, `exists()`, `contains()`, `all()`, `any()`)
    - Map operations, type conversions (`string()`, `int()`, `bool()`, `double()`, `bytes()`)
    - Math operations and comparisons

16. **Step Context Variables**:
    - `inputs.<name>` - Access workflow input parameters
    - `variables.<key>` - Access outputs from previous steps
    - `expressions.<name>` - Access results from CEL expressions evaluated in the current step
      - **Important**: Expressions are evaluated sequentially, so later expressions can reference earlier ones
      - This allows extracting common sub-expressions (e.g., `resource.Get()`) and reusing them
    - `agentResponse` - For agent steps, the LLM response is available in output expressions
      - This variable contains the raw text response from the LLM
      - Can be parsed using CEL JSON functions (e.g., `json.unmarshal(agentResponse)`)
    - `toolResult` - For mcpToolCall steps, the MCP tool call result is available in output expressions
      - This variable contains the result returned by the MCP tool
      - Can be parsed using CEL JSON functions if the tool returns JSON (e.g., `json.unmarshal(toolResult)`)
    - `a2aResult` - For externalAgentRef steps, the external A2A agent result is available in output expressions
      - This variable contains the result returned by the A2A agent, as a map
    - `reportResult` - For openReport steps, the OpenReports.io report result is available in output expressions
      - Keys: `mode`, `name`, `namespace`, `summary`, `data`
    - `steps` and `stepStatus` - Agent and forEach step results, and per-step status; see [Variable Access](../user/reference/cel-reference.md#variable-access) for the full list of variables

17. **In-Memory Shared Context**:
    - Shared context is stored in-memory during workflow execution for performance
    - Each step receives access to the shared context when it begins execution
    - Expressions are evaluated with access to the shared context (inputs, steps, expressions)
    - Outputs are written to the in-memory shared context after step completion
    - Workflow-level outputs are evaluated at completion and exposed in WorkflowRun status
    - Workflows are idempotent - if the controller restarts, workflows restart from the beginning

### Expression Evaluation Order

1. **Step starts**: Step receives a private copy of the current shared context
2. **Sequential expression evaluation**: CEL expressions in `expressions` are evaluated **sequentially** in order:
   - Each expression can reference previously evaluated expressions using `expressions.<name>`
   - Results are stored in the private context immediately after evaluation
   - This allows extracting common sub-expressions and reusing them
3. **Output evaluation**: All CEL expressions in `outputs` are evaluated using the private context (which includes all expression results)
4. **Context update**: Output results are written back to the workflow's shared context
5. **Step completes**: Private context is discarded, shared context is updated

**Key Point**: Expressions are evaluated sequentially, not in parallel, allowing later expressions to reference earlier ones by name.

---

## Implementation Details

Summary of where and how the controller is implemented. For full code, see the referenced files.

### DAG and execution model

- **DAG** ([`internal/workflow/executor/dag.go`](../../internal/workflow/executor/dag.go)): Nodes = steps; edges only from explicit `dependsOn` (no inference from CEL references or `matchConditions`); cycle detection; the ready-step query (`GetReadySteps`). Run completion is decided in `executor.go`.
- **Step states**: Pending, Running, Succeeded, Failed, Skipped, and Waiting (paused for a callback). A step whose `matchConditions` are false goes straight from Pending to Skipped. Steps with `failurePolicy: Continue` can fail without blocking dependents.
- **Execution flow** ([`internal/workflow/executor/executor.go`](../../internal/workflow/executor/executor.go) – `ExecuteWorkflow()`): (1) Validate inputs and init shared context; (2) Build DAG and validate (no cycles); (3) For each step: wait for dependencies, evaluate `matchConditions` (skip if any false), run it with `executeStep()`, update context; (4) Mark workflow complete when all steps finish. Ready steps run one at a time; only forEach children run in parallel, up to `forEach.maxConcurrency`.

### CEL and context

- **CEL** ([`internal/workflow/executor/cel.go`](../../internal/workflow/executor/cel.go), [`cel_libraries.go`](../../internal/workflow/executor/cel_libraries.go), [`resource_macros.go`](../../internal/workflow/executor/resource_macros.go)): Kyverno CEL libraries via [kyverno/sdk/extensions/cel](https://github.com/kyverno/sdk/tree/main/extensions/cel) + Kubernetes CEL libraries; OttoFlow macros (`resource.Get`/`List`, `resourceLogs`, `resourceEvents`, `resourceMetrics`, `prometheusMetrics`); variable map (see [Variable Access](../user/reference/cel-reference.md#variable-access)). **Important**: Use `EnvSet` (from `k8s.io/apiserver/pkg/cel/environment`) with a single `Extend()` so all options are registered together; do not extend `cel.Env` in multiple steps.
- **Shared context** ([`internal/workflow/executor/context.go`](../../internal/workflow/executor/context.go)): In-memory during execution; structure `inputs`, `variables` (step outputs, flat), `steps` (agentRef and forEach results only), `expressions`; workflow-level outputs written to WorkflowRun status at completion. Key methods: `InitializeContext()`, `ReadContext()`, `WriteStepOutputs()`, `GetContext()`.

### Controllers and triggers

- **Reconciliation** ([`workflow_controller.go`](../../internal/workflow/controller/workflow_controller.go), [`workflowrun_controller.go`](../../internal/workflow/controller/workflowrun_controller.go)): Workflow template registration and trigger wiring; WorkflowRun lifecycle (Pending → Running → Succeeded/Failed), input validation, context init, DAG build, execution, status updates.
- **Multi-cluster execution (first step)**: Optional **WorkflowRun.spec.clusterRef** allows a KubeConfig secret as input to the run. When **clusterRef.kubeConfigSecretRef** is set, the controller loads that Secret (namespace defaults to WorkflowRun namespace), parses the kubeconfig (data key defaults to "config", "kubeconfig", or "value"), and builds a controller-runtime Client for that cluster. The executor uses this client for resource queries, mutate steps, and CEL `resource.*`; when clusterRef is omitted or **clusterRef.local** is true, the in-cluster client is used. See [`internal/workflow/cluster/kubeconfig.go`](../../internal/workflow/cluster/kubeconfig.go). Broader multi-cluster orchestration (fan-out across GitOps-managed clusters) remains future work.
- **Workflow runner Jobs**: In-cluster `WorkflowRun` execution uses a dedicated Kubernetes Job per run. The controller creates and tracks the runner Job, while `cmd/workflow-runner` loads the `WorkflowRun`, resolves cluster access from `clusterRef` (`local`, `kubeConfigSecretRef`, or `kubeConfigFilePath`), and executes the workflow in the runner pod. The executor now separates `controlClient` (OttoFlow CRDs and control-plane objects) from `targetClient` (resource query, mutate, CEL `resource.*`) so hub objects remain centralized while workflow resource operations can target remote clusters.
- **Local execution (CLI)**: When the CLI is run with `--workflow-dir <directory>`, the workflow is executed locally in the same process instead of creating a `WorkflowRun` in the cluster. The CLI loads all Workflow, Agent, MCPServer, and StepTemplate manifests from the directory into a **fake** controller-runtime client (control plane). The same workflow executor runs in-process with this fake client as `controlClient` and the user’s kubeconfig client as `targetClient`. Resource queries, mutate steps, and CEL `resource.*` use the real cluster; Workflow/Agent/MCPServer/StepTemplate lookups use the in-memory objects from the directory. Agent steps run **in-process** (no A2A) when `localExecutionMode` is true. **WorkflowRef** steps run inline in the same process (referenced workflows are collapsed at runtime), so local execution supports WorkflowRef. No `WorkflowRun` or Job is created in the API server; status and outputs are streamed to the terminal. Implementation: [`cli/internal/executor/local_executor.go`](../../cli/internal/executor/local_executor.go) (LoadFromDirectory, ExecuteWorkflow), [`cli/cmd/run.go`](../../cli/cmd/run.go) (runWorkflowLocal). Optional flags: `--max-workers`, `--prometheus-url` for local runs.
- **Triggers** ([`trigger_manager.go`](../../internal/workflow/controller/trigger_manager.go), [`scheduler.go`](../../internal/workflow/controller/scheduler.go)): Cron triggers via an in-process scheduler (robfig/cron) with leader election and concurrency policy enforcement; event triggers with leader election (only leader watches); dynamic watchers and WorkflowRun creation.

### Recovery, resilience, and production considerations

- **Executor / runner restart recovery** ([`executor.go`](../../internal/workflow/executor/executor.go), [`workflowrun_controller.go`](../../internal/workflow/controller/workflowrun_controller.go)): By default, workflow execution context (inputs, variables, step outputs) is in-memory only within a runner process. If a runner pod dies, that in-memory state is lost and the controller marks the run Failed in `WorkflowRun.status`. Checkpointing is opt-in per WorkflowRun (`spec.execution.checkpointing.enabled`, see [`checkpoint.go`](../../internal/workflow/executor/checkpoint.go)): after each step the executor persists a snapshot to a ConfigMap.
- **WorkflowRun cleanup and run policy** ([`workflowrun_controller.go`](../../internal/workflow/controller/workflowrun_controller.go), [`workflow_types.go`](../../api/v1alpha1/workflow_types.go)): Workflow `spec.run` supports **retentionMinutes** (delete completed runs older than N minutes) and **maxAllowed** (keep at most N completed runs per workflow; oldest deleted first). Applied when reconciling completed WorkflowRuns. Omitted fields default to 10080 minutes (7 days) and 50 runs (CRD defaults); an explicit `0` disables the respective deletion or limit.
- **Leader election**: Default is **on** (`--leader-elect` defaults to true in [`cmd/controller/main.go`](../../cmd/controller/main.go)). With a single replica, that instance becomes leader; with multiple replicas, only the leader reconciles. Event triggers run only on the leader.
- **Scalability and hardening**: The step-level `retry` and `timeout` fields are accepted but not applied. Kubernetes API calls use controller-runtime’s client. Agent exec calls reuse a per-`WorkflowExecutor` HTTP client and retry up to three attempts, only after a connection failure; any HTTP response, a 5xx included, is not retried. External A2A calls build a client per step and have no retry. **Metrics**: Workflow failures are recorded via `ottoflow_workflow_runs_total{..., phase="failed"}` and step failures via `ottoflow_workflow_steps_total{..., phase="failed"}` ([`internal/metrics/workflow_metrics.go`](../../internal/metrics/workflow_metrics.go)).

### Step executors (file references)

| Area | Implementation |
|------|----------------|
| Expression / default step | [`executor.go`](../../internal/workflow/executor/executor.go) – expression evaluation, outputs |
| WorkflowRef (sub-workflow) | `executor.go` – `executeWorkflowReference()`; runs referenced workflow inline in the same Job/process (collapsed at runtime for code reuse); CEL inputs, cross-namespace, output extraction; works in cluster and local execution |
| Agent | [`agent_executor.go`](../../internal/workflow/executor/agent_executor.go), [`a2a_client.go`](../../internal/workflow/executor/a2a_client.go); prompts, LLM providers, A2A, output extraction |
| Resource query | [`resource_query_executor.go`](../../internal/workflow/executor/resource_query_executor.go); single/list, selectors, CEL outputs, CRDs |
| Prometheus query | [`prometheus_query_executor.go`](../../internal/workflow/executor/prometheus_query_executor.go); PromQL with `{{.var}}` substitution, Variables (CEL), Outputs over result |
| StepTemplate | [`steptemplate_executor.go`](../../internal/workflow/executor/steptemplate_executor.go); CRD lookup, Go template args, CEL for args |
| MCP tool call | [`mcp_executor.go`](../../internal/workflow/executor/mcp_executor.go); CEL args, MCPServer CRD, stdio/HTTP/SSE, auth |
| A2A agent call | [`external_agent_executor.go`](../../internal/workflow/executor/external_agent_executor.go), [`a2a_client.go`](../../internal/workflow/executor/a2a_client.go); sync/async, agent card, timeout, auth |
| OpenReports.io report | [`openreport_executor.go`](../../internal/workflow/executor/openreport_executor.go) – `executeOpenReportStep()`; falls back to `reportResult.data` when the OpenReports CRD is absent |

### Agent executor: OSS/enterprise split

**Motivation**: the original Nirmata-backed agent executor depended on a private,
proprietary Nirmata module, so `go build ./...` / `go mod download` failed
for anyone outside the Nirmata GitHub org.

- **`AgentExecutor` interface** (`internal/agent/interfaces.go`): the single
  abstraction all agent execution goes through; unchanged by the split, so no
  CRD or call-site changes were needed.
- **`DefaultAgentExecutor`** (`internal/agent/default_executor.go`): a
  provider-agnostic implementation built on the public `gollm.Client`, with
  no private dependency. Ships in the public module and handles every
  provider except `nirmata`.
- **`RoutingAgentExecutor`** (`internal/agent/routing_executor.go`): dispatches
  by `Agent.spec.modelProvider`. `nirmata` routes to an executor supplied by
  the enterprise plugin; every other provider routes to
  `DefaultAgentExecutor`. Wired via `internal/agent/exec_client.go:78` and
  `internal/agent/executor.go:264`.
- **Enterprise injection point**: `NewRoutingAgentExecutorFromExecutors`
  lets the enterprise plugin supply the real `nirmata`-provider executor at
  construction time. In an OSS-only build (no enterprise plugin), the
  `nirmata` provider resolves to a `nirmataUnavailableExecutor` that returns a
  clear error instead of failing to build.
- **`Agent.spec.modelProvider` is now required**, with no default — every
  Agent must explicitly pick a provider, which is what the router dispatches
  on.

---

## Custom Resource Definitions

The complete CRD specifications for all OttoFlow workflow resources have been moved to a separate document for better readability. See [API Reference](../user/reference/api/README.md) for all CRD definitions and schema specifications.

The CRD document includes:
- Workflow CRD (Template)
- MCPServer CRD
- WorkflowRun CRD (Execution Instance)
- All schema definitions (Input, CronTrigger, EventTrigger, Step, MatchCondition, MCPTool, OutputExtraction, StepStatus, TriggerInfo)

---

## Prometheus Metrics (Design)

This section describes the design for two complementary Prometheus metrics features. Both integrate with the existing Prometheus infrastructure (controller-runtime metrics server on `:8080`, ServiceMonitor for scraping).

1. **Built-in Workflow Execution Metrics** – Controller-emitted metrics for workflow and step execution (duration, success/failure counts, active workflows).
2. **Custom Workflow Metrics** – Workflow-defined metrics that workflows can publish at completion, enabling health dashboards, SLO tracking, and business metrics.

### Built-in Workflow Execution Metrics

**Goals**: Operational visibility into workflow execution; dashboards for success rates, latency, throughput; standard Prometheus patterns (counters, histograms, gauges).

| Metric Name | Type | Labels | Description |
|-------------|------|--------|-------------|
| `ottoflow_workflow_runs_total` | Counter | `workflow`, `namespace`, `phase` | Total WorkflowRuns by phase (succeeded, failed) |
| `ottoflow_workflow_trigger_events_dropped_total` | Counter | `workflow`, `namespace`, `reason` | Event-trigger events dropped on a WorkflowRun create-path error (reason: api_error) |
| `ottoflow_workflow_run_duration_seconds` | Histogram | `workflow`, `namespace` | Duration from start to completion |
| `ottoflow_workflow_steps_total` | Counter | `workflow`, `namespace`, `step`, `phase` | Total step executions by phase (succeeded, failed, skipped) |
| `ottoflow_workflow_step_duration_seconds` | Histogram | `workflow`, `namespace`, `step` | Step execution duration |
| `ottoflow_workflow_runs_active` | Gauge | `workflow`, `namespace` | Currently running WorkflowRuns (Running phase) |

**Label conventions**: `workflow` = Workflow name (from `workflowRun.Spec.WorkflowRef.Name`); `namespace` = WorkflowRun namespace; `step` = step name; `phase` = `succeeded`, `failed`, or `skipped`; `reason` = why an event-trigger event was dropped (`api_error`: the WorkflowRun create path returned an error).

**Instrumentation**: (1) Workflow start – increment `ottoflow_workflow_runs_active` when phase → Running; (2) Workflow completion – decrement gauge, increment `ottoflow_workflow_runs_total{phase}`, observe `ottoflow_workflow_run_duration_seconds`; (3) Step completion – increment `ottoflow_workflow_steps_total{phase}`, observe `ottoflow_workflow_step_duration_seconds`; (4) Event-trigger watch loop – increment `ottoflow_workflow_trigger_events_dropped_total{reason}` when the create path returns an error other than a status-update failure.

**Implementation**: Create `internal/metrics/workflow_metrics.go` with metric definitions using `github.com/prometheus/client_golang`; use `prometheus.DefaultRegisterer`; inject a `MetricsRecorder` interface into the executor; record in `ExecuteWorkflow()` and step loop in `executor.go`. Suggested histogram buckets (seconds): `[]float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300}`.

### Custom Workflow Metrics

**Goals**: Allow workflows to publish custom Prometheus metrics from execution context; support counters, gauges, histograms; CEL expressions for output values and metric labels; emit at workflow completion.

**Use cases**: Health dashboards (`ottoflow_workflow_unhealthy_pods_total{workflow="pod-health-check"} 5`), SLO tracking, business metrics.

**API design**: Metric details are declared on the output. Each output can optionally have a `metric` field; when present, the output's evaluated value is published to Prometheus (single source of truth, colocated with output).

```yaml
outputs:
  - name: unhealthyCount
    expression: "variables.unhealthyCount"
    metric:
      name: unhealthy_pods_total
      type: counter
      help: "Number of unhealthy pods detected"
      labels:
        - name: workflow
          value: "pod-health-check"
        - name: namespace
          value: "string(inputs.namespace)"
  - name: summary
    expression: "variables.summary"
    # No metric - status only
```

- **Outputs without `metric`**: Status only (WorkflowRun status).
- **Outputs with `metric`**: Status and Prometheus (evaluated value published).

**Types**: Add `OutputMetric` (Name, Type: counter|gauge|histogram, Help, Labels []MetricLabel, Buckets optional) and `MetricLabel` (Name, Value as CEL expression). Add `Metric *OutputMetric` to `Output` in `workflow_types.go`.

**Rules**: Value = output's evaluated expression; labels = CEL expressions (can reference `variables`, `inputs`, earlier `outputs`). All custom metrics prefixed with `ottoflow_workflow_`; user `name` sanitized; final name `ottoflow_workflow_{sanitized_name}`.

**Implementation**: After `evaluateWorkflowOutputs()`, iterate outputs with `metric` set; use output's evaluated value; create `CustomMetricsRecorder` that evaluates label CEL expressions, converts value to float64 (or []float64 for histogram), uses `GetMetricWith(labels)` for dynamic metrics. Considerations: limit label cardinality; on CEL label failure log and skip metric, do not fail workflow; counters increment per run, gauges overwrite; histogram value can be single number or list.

**Implementation order**: Phase 1 – built-in metrics (add dep, `workflow_metrics.go`, MetricsRecorder, instrument executor, tests). Phase 2 – custom metrics (OutputMetric in API, `custom_metrics.go`, integrate after output evaluation, CEL for labels, sample workflow, docs). Files: `internal/metrics/workflow_metrics.go`, `internal/metrics/custom_metrics.go`, modify `executor.go` and `workflow_types.go`, optional `samples/workflows/pod-health-metrics.yaml`, `docs/user/reference/metrics.md`. Testing: unit tests with mocked MetricsRecorder; integration scrape `/metrics`; E2E workflow with custom metric assertion. References: [Prometheus Go client](https://github.com/prometheus/client_golang), [metric types](https://prometheus.io/docs/concepts/metric_types/), [controller-runtime metrics](https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/metrics).

---

## Multi-Tenant LLM Credentials

### Overview

Runner Jobs can inherit LLM credentials from a well-known Secret in the WorkflowRun's namespace. The feature is opt-in and disabled by default: the well-known Secret name is empty out of the box, so no lookup happens unless the cluster-wide flag/env var or the per-run `spec.execution.llmCredentialsSecret` names a Secret. Once a name is configured and a Secret by that name exists in the namespace, the controller reads its keys, filters them against an allowlist of recognized LLM credential keys, and injects them as `secretKeyRef` env vars into the runner Job before it is created. Namespaces where the feature isn't configured, or where the named Secret doesn't exist, receive no injection.

### Design Choice

Option A (namespace-scoped well-known Secret) was chosen over Option B (per-Agent `secretKeyRef`) and Option C (a new `LLMConfig` CRD). Option A requires no API changes, no per-step author knowledge of credential key names, and naturally scopes credentials to the namespace boundary that Kubernetes RBAC already enforces. Options B and C are deferred: B adds per-Agent boilerplate that most users don't want, and C introduces a new CRD abstraction that isn't needed until credentials require more structure (rotation, expiry, multiple providers per namespace).

No RBAC changes were required: the controller `ClusterRole` already held `secrets: get` cluster-wide.

### Key Constraints

- **Allowlist-filtered**: only keys matching `LLMEnvAllowlist` are injected — `NIRMATA_LLM_TOKEN`, `NIRMATA_LLM_APIKEY`, `NIRMATA_LLM_SERVICEACCOUNT_TOKEN`, `NIRMATA_URL`, and `NIRMATA_LLM_MODEL`. Public-provider keys (`OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `GEMINI_API_KEY`, `GOOGLE_API_KEY` and related) are not forwarded, because nothing on the agent-executor reads a per-run value for them; they come from the agent-executor's own environment. Keys not on the allowlist are skipped to prevent accidental leakage of non-LLM secrets stored in the same Secret; a public-provider key in the Secret is named in the `LLMCredentialInjection` Warning's `ignored:` list, and one set in `spec.execution.job.env` raises a `LLMCredentialIgnored` Warning Event (names only, never values).
- **Fail-silent**: a missing Secret is not an error. The Job is created as normal with no injected env vars. An inaccessible (Forbidden) Secret is handled by the preflight described under "Secret RBAC preflight for Nirmata-provider agent steps" below: benign unless the run demonstrably needs the credentials.
- **Explicit-wins precedence**: any env var already declared in `spec.execution.job.env` on the WorkflowRun overrides an injected value from the well-known Secret.
- **Deterministic Job spec**: injected keys are sorted alphabetically before appending to the env list, so the resulting Job spec is stable across reconciler runs regardless of Go map iteration order.

### Configuration

The Secret name is configurable:

| Flag | Env Var | Default |
|------|---------|---------|
| `--workflow-runner-llm-credentials-secret` | `WORKFLOW_RUNNER_LLM_CREDENTIALS_SECRET` | `""` (disabled) |

The flag/env var is empty by default, so automatic injection is disabled until it is set to a non-empty Secret name. The Helm chart exposes `workflowRunner.llmCredentialsSecret` with the same empty default. A per-run `spec.execution.llmCredentialsSecret` override is also available regardless of the cluster-wide setting. Shipped in v0.6.0.

### Secret RBAC preflight for Nirmata-provider agent steps

The "Fail-silent" constraint above has one exception: a denied read of the well-known LLM
credentials Secret fails the WorkflowRun terminally when, and only when, the denied read would
cost the run its credentials — an UNCONDITIONALLY reachable AgentRef targets an Agent whose
`modelProvider` is `nirmata` (or unset, which the agent-executor treats the same way), and the
run supplies none of `NIRMATA_LLM_TOKEN` / `NIRMATA_LLM_SERVICEACCOUNT_TOKEN` /
`NIRMATA_LLM_APIKEY` itself via `spec.execution.job.env`. Every other case — no agent step, a
non-Nirmata provider, explicit run-supplied credentials, or an agent step whose execution is not
guaranteed (see "Conditional reachability" below) — is benign: injection is skipped, a
`LLMCredentialsForbidden` Warning event is recorded (it is recorded in the terminal case as
well), and the run proceeds.

**Why:** without the preflight, a Forbidden read of the credentials Secret was wrapped in the same
`transientBuildError` a transient API failure uses, so the reconcile requeued it with backoff
indefinitely: the run never reached a terminal status and carried no message saying what was
wrong. A denial is not transient — it is configuration to fix — so the controller now decides
up front whether the denial matters. The predicate is deliberately conservative: it only ever
produces a terminal failure in the one case where the run could not have succeeded anyway (an
unconditionally reachable Nirmata-provider agent step is guaranteed to execute and to hard-fail
the workflow if it errors, and no agent-executor call succeeds without a token). In every other
case the denial is downgraded to a skipped injection plus a Warning event, and the run proceeds.

**Conditional reachability:** a build-time walk over the step tree can prove a step is
*reachable*, never that it is *guaranteed to run*. Three shapes make the difference: a step
behind non-empty `matchConditions` can evaluate false and never run; a step with
`failurePolicy: Continue` can fail outright and still let the workflow succeed; a step inside a
`forEach` can iterate zero times, since `StepForEach.Items` is always a CEL expression string
whose emptiness can never be proven ahead of execution. Crediting any of those as "needs
credentials" would turn a run that can succeed without the credentials into a terminal
build-time failure — exactly the over-reach this predicate must avoid. The predicate
(`workflowNeedsNirmataLLMCredentials` / `agentStepWalker` in
`internal/workflow/controller/secret_needs_creds.go`) therefore returns true only for a step that
is UNCONDITIONALLY reachable: it skips entirely into any step whose own `matchConditions` /
`failurePolicy` mean it may not run, and skips every `forEach` outright, regardless of the child
step's own conditions. A build-time verdict is least trustworthy exactly where execution is
conditional, so the walk declines to render one there.

The check is based on the OUTER, referencing Step's own `matchConditions` / `failurePolicy`
only — never on a StepTemplate body's copy of those same fields. A StepTemplate's own
`matchConditions` / `failurePolicy` are inert at runtime: `executeStepTemplate`
(`steptemplate_executor.go`) merges them into the instantiated step only when the outer step
doesn't set its own, but `checkMatchConditions` and the `failurePolicy: Continue` handling in the
workflow loop (`executor.go`) run once, before `executeStep` is ever called, against the original
referencing Step — never against the instantiated/merged step `executeStepTemplate` builds.
Treating the template's own fields as if they governed execution would exclude steps that in
fact run unconditionally, widening the predicate's coverage gap for no reason.

**SelfSubjectAccessReview gate on the injection read:** before `injectWellKnownLLMCredentials`
issues its Secret `Get`, `canGetSecret` (`internal/workflow/controller/secret_access_review.go`)
asks the API server, via a `SelfSubjectAccessReview`, whether the controller's own identity is
currently allowed to `get` the exact resolved `{namespace, name}` the `Get` is about to use — the
same `key` variable feeds both calls, so a per-run `llmCredentialsSecret` override naming a
different namespace than the run's own cannot authorize one object while reading another. This
classifies into three outcomes:

- **allowed:** proceed to the `Get`. The review is a posture measure, not the security
  control — the `Get`, and the API server behind it, remain authoritative; a grant revoked in the
  window between the review and the `Get` is still decided correctly, because the `Get` itself
  is what actually determines the outcome.
- **denied:** the read is never attempted. The failure carries the same operator remediation a
  `Get`-`Forbidden` denial has (Role name, Secret name, `rbac.secretAccess`, the `kubectl auth
  can-i` command) — only the `(underlying error: ...)` clause differs, naming which check caught
  it (`SecretReadNotAuthorizedFragment` for the gate, the API server's own 403 text for the
  `Get`) — so an operator gets one consistent fix regardless of which check fired. Unlike the
  allowed branch, this window is not self-correcting: a grant applied between the review and this
  decision is not adjudicated here, because the `Get` — the only authoritative check left — is
  never issued (`injectWellKnownLLMCredentials` returns as soon as the gate denies). The run sees
  the stale `Forbidden` result until the next reconcile re-runs the gate. This is not a new class
  of failure: the window is exactly one API round trip wide, and a `Get` issued at that same
  instant with no gate in front of it would also have observed the pre-grant state and returned
  `Forbidden`.
- **inconclusive** (the review's `Create` call itself failed, or its own evaluation could not
  decide): falls through to the `Get` **unconditionally**. No retry budget, no second wait
  mechanism, no predicate walk. This is deliberate, not a shortcut: an authorization-webhook
  outage that makes the review inconclusive makes the `Get` return an Internal Server Error
  (the API server's `withAuthorization` filter answers an authorizer error with a 500 —
  `k8s.io/apiserver/pkg/endpoints/filters/authorization.go`), which the existing `Get`-error
  branch already turns into a `transientBuildError` — a requeue-and-retry. A transient authorizer
  outage is therefore retried by code that already existed, whether or not the run needs the
  credentials, with no new mechanism.

The classification ladder mirrors `k8s.io/apiserver`'s own request-authorization order
(`pkg/endpoints/filters/authorization.go withAuthorization`): an `Allowed` decision wins outright;
otherwise a non-empty `EvaluationError` is treated as inconclusive (matching the filter's own
"evaluation error → 500" branch); otherwise the request is denied. `Status.Denied` is deliberately
**not** consulted as an independent signal: at that filter, an explicit `Deny` and a plain
`NoOpinion` resolve to the identical 403, so `Denied` adds no decision weight the final arm
lacks — while a fail-closed authorization-webhook outage sets `Denied=true` **alongside**
`EvaluationError` (the webhook authorizer returns its `decisionOnError` together with the error,
and the `SelfSubjectAccessReview` handler sets `Denied` from the decision and `EvaluationError`
from the error), which must read as inconclusive, not as a confident denial (a
`Denied`-first ladder would turn an authorizer outage into a spurious terminal
`SecretAccessDenied`). A plain RBAC denial with no error is the zero value of
`SubjectAccessReviewStatus` (`Allowed=false, Denied=false, EvaluationError=""`), which the final
arm covers correctly — this is also why a fail-**open** authorizer outage (the Kubernetes
default, `decisionOnError = NoOpinion`) reads as inconclusive rather than allowed: it sets
`EvaluationError` too, just without `Denied`.

`Spec.ResourceAttributes.Version` is set to `"v1"`, matching the Secret `Get`'s own implicit
authorization attributes. Plain RBAC ignores this field entirely, so setting it costs nothing
there — but it closes the door on an authorization webhook whose `matchConditions` key on the
request's `apiVersion`, which could otherwise authorize a version the review never asked about and
let the review and the read diverge on outcome.

`Status.Reason` on a `SelfSubjectAccessReview` can name the exact RoleBinding, Role, and
ServiceAccount that grant access (on an allow; a plain RBAC denial carries an empty reason, a
denial with role-resolution errors carries the raw error text, and a webhook authorizer may
return anything) — that is exactly the kind of server-influenced content that must never reach a
tenant-visible message unvetted (see the
Diagnostic Secret-rules addendum below for why an *analogous* addendum is safe: it sanitizes
before echoing anything). The gate takes the simpler position and never echoes `Status.Reason` at
all: the denial cause passed into the shared error path is a fixed, package-local sentinel
(`errSecretReadNotAuthorized`), never anything built from the review's own response.

`create selfsubjectaccessreviews.authorization.k8s.io` is granted cluster-wide to
`system:authenticated` by the built-in `system:basic-user` ClusterRole — the same grant the
`SelfSubjectRulesReview` diagnostic below already relies on — so every install can already issue
this review; **no chart change is needed**.

**Cost.** No memoisation; the added cost is exactly one extra `SelfSubjectAccessReview` per
build attempt:

| Outcome | Without the gate | With the gate | Δ |
|---|---|---|---|
| Allowed (common case) | 1 `Get` | 1 review + 1 `Get` | **+1 review** |
| Denied | 1 `Get` (403) + predicate walk | 1 review + predicate walk | **0** — the review *replaces* the denied `Get` |
| Inconclusive (then `Get` allowed) | 1 `Get` | 1 review + 1 `Get` | +1 review, no predicate walk |

A run that is requeued many times before its Job is created pays the additive (allowed) case once
per attempt, alongside the uncached Secret `Get` each attempt issues regardless of the gate — a
small, bounded addition, not a new class of load.

An admission-time review (advisory, warn-only, for apply-time feedback before the controller ever
reconciles) was considered but deferred: it cannot reach cron- or event-triggered runs, which the
controller creates itself with no human present to see a warning, so reconcile-time gating remains
the only mechanism that reaches every run regardless of how it was created.

**Reader consistency:** the predicate's Agent `Get` reads through the controller's direct
(non-cached) reader, the same one `injectWellKnownLLMCredentials` uses for the Secret `Get` it
gates. Reading the Agent through the shared/cached client instead would race a batch apply of
Agent + Workflow + WorkflowRun together: a `NotFound` from a not-yet-synced informer cache would
resolve as "no Nirmata credentials needed," silently defeating the preflight and letting the run
reach the generic agent-executor failure this feature exists to eliminate.

**Uncached Secret reads:** the manager's client is built with Secret and ServiceAccount caching
disabled (`client.CacheOptions.DisableFor` in `cmd/controller/main.go`), so every Secret read the
controller makes is a live API call: the WorkflowRun reconciler's reads go through the manager's
direct API reader (`preferAPIReader`); `DisableFor` routes the remaining Secret `Get`s on the
manager client (the cron trigger's `inputValuesFrom`, the webhook HMAC key) to the API server
rather than the cache; and the certificate manager reads through a plain clientset, which is
uncached by nature. A cache-backed `Get` of a typed object starts a cluster-wide list+watch
informer for that type, so a cached Secret `Get` would require cluster-wide `secrets list, watch`
RBAC no matter how narrow the individual `Get` looks. With caching disabled, each `Get` is a live
API call authorized as `get` on that one object — which is what allows Secret access to be granted
per Secret, per namespace (the shipped chart still grants the controller cluster-wide Secret
access today; narrowing it is a separate change). The
trade-off is latency: every Secret read is an API round trip rather than a cache hit.

**Laziness:** the predicate walks the step tree, so `buildWorkflowRunnerJob` defers it into a
`func() (bool, error)` closure that `injectWellKnownLLMCredentials` calls only from inside
`handleDeniedLLMCredentialsRead` — reached on either of the two denial paths (the
`SelfSubjectAccessReview` gate denying the read outright, or the `Get` itself coming back
`Forbidden`) — never on the happy or inconclusive path, where the Secret read (or the gate's
own fallthrough) simply proceeds and the answer would have gone unused. This keeps the added
walk's cost off every run that isn't actually denied access.

**`FailureReason` value contract:** `WorkflowRunStatus.FailureReason` (`api/v1alpha1/workflowrun_types.go`)
classifies a Failed run's cause for programmatic consumers (alerting, dashboards) that need to
distinguish specific causes without parsing `Status.Message`. It carries two values today —
`SecretAccessDenied` (a Secret read or create the **controller ServiceAccount** performed returned
Forbidden) and `RunnerRefUnresolved` (the kubelet could not resolve a Secret/ConfigMap or one of
their keys referenced by the runner pod; the value is reserved in the API now so the enum is
stable, and the detection that sets it lands in a separate change) — and is deliberately small
and additive: a future cause gets a new value, existing values are never repurposed or removed.
Empty is NOT itself a claim
that the run succeeded or that nothing is classifiable about the failure — it means either the run
has not failed, or it failed for a cause outside this set. `setRunFailed`
(`internal/workflow/controller/workflowrun_controller.go`) writes the field on every terminal
Failed transition, including an empty value, so a classification from an earlier attempt (e.g. a
checkpoint retry that failed for a different reason on retry) can never survive stale. Consumers
must treat an unrecognised value exactly like an empty one — the usual forward-compatibility
posture for an enum-valued status field. The controller sets `SecretAccessDenied` from this
preflight and from `ensureRunnerSecrets` (both of its Secret `Get`s and the cross-namespace
copy's `Create`): every one of those wraps the sentinel `errSecretAccessDenied`, which
`secretAccessDeniedReason` checks with `errors.Is`, so any further wrapping on the way to
`setRunFailed` is harmless.

**Diagnostic Secret-rules addendum (`SelfSubjectRulesReview`):** once a Secret read is denied —
by either the `SelfSubjectAccessReview` gate above or the `Get`'s own `Forbidden` — and the run
has already been decided to fail terminally, the controller separately asks the API server which
Secret rules its own identity holds in the run's namespace and appends what it learns to the
failure message (`internal/workflow/controller/secret_access_advice.go`). This is diagnosis
only — distinct from the gate above, which decides whether the read is even attempted; this
addendum never gates anything, runs strictly after the outcome is already decided, and the
message carries no addendum at all whenever the lookup is skipped or inconclusive. On a
gate-caught denial, `secretAccessAdvice`'s Arm A (a full grant match) becomes the concrete
authorization-webhook or TOCTOU signal Arm A's own doc comment names: the review said denied, yet
the rules lookup says the identity currently holds the grant. It exists because a Role that grants
access to the wrong Secret name produces the exact same bare 403 as no Role at all; naming what is
actually granted, versus what the read needs, tells the two apart. `resourceNames` in an RBAC rule
has no wildcard support: an absent `resourceNames` list grants every name, but a literal `"*"`
entry in that list grants access to a Secret literally named `*` and nothing else — verbs,
apiGroups, and resources all honor `"*"` as a wildcard, so treating `resourceNames` the same way
would misreport that Role as a full grant. The lookup is never issued for any namespace other than
the run's own, so a Secret in the controller's install namespace never has that namespace's RBAC
rules described in a tenant-visible status. And because the API server authorizes a read before
it looks the object up, a 403 says nothing about whether the Secret exists — this addendum can
only ever speak to permission; it is never evidence the Secret exists.

---

## Default (OSS) Agent Executor

Agent steps no longer require a private, proprietary module to build: the previous provider-specific `AgentExecutor` in `internal/agent/executor.go` was replaced with `RoutingAgentExecutor` (`internal/agent/routing_executor.go`), which dispatches `ExecuteAgent` per call based on `Agent.Spec.ModelProvider` — enterprise-only providers route to an enterprise delegate, every other provider routes to `DefaultAgentExecutor` (`internal/agent/default_executor.go`), a provider-agnostic implementation built on the public `GoogleCloudPlatform/kubectl-ai/gollm` client with a ported send/collect-tool-calls/dispatch/repeat loop. An empty `ModelProvider` is routed to the enterprise delegate only as a defensive fallback for programmatically constructed objects; the CRD requires `modelProvider` and defines no default.

In this open-source build the enterprise delegate is a stub that returns an actionable "enterprise plugin required" error rather than failing the build or panicking; the enterprise plugin supplies a real executor via `NewRoutingAgentExecutorFromExecutors`. `go.mod` no longer requires any private module — `go build ./...` and `go mod download` succeed for any external contributor.

---

## Future Enhancements

This section outlines planned enhancements and recommendations for future versions of the OttoFlow workflow controller. These recommendations are prioritized based on their impact on production readiness, security, and scalability.

### Design Extensibility

The OttoFlow workflow controller design is highly extensible to support all planned future enhancements:

**Key Extensibility Strengths**:
- ✅ **Step Schema**: Uses union pattern with optional fields, enabling new step types without breaking changes
- ✅ **StepExecutor Interface**: Pluggable execution pattern supports new execution modes and step types
- ✅ **Optional Fields with Defaults**: All new fields can be optional with defaults matching current behavior
- ✅ **Status Extensibility**: Status fields are additive, allowing new observability fields without breaking changes
- ✅ **Architecture Flexibility**: Component separation enables microservice split without CRD changes

**Extensibility Assessment**: All planned enhancements can be implemented through **additive schema changes** (new optional fields) with **backward-compatible defaults**. No breaking changes are required.

### Critical Priority Enhancements

#### 1. AgentExecutor Service with A2A Protocol — **Implemented**

**Status**: ✅ Implemented (Phase 4). Agent steps execute via AgentExecutor Service using A2A protocol (internal subset) with streaming mode.

**Previous state**: Agent steps executed in controller process, causing scalability, reliability, and security issues.

**Benefits**:
- Improved scalability (shared service pool with horizontal scaling)
- Better reliability (isolated execution prevents agent failures from affecting controller)
- Enhanced security (service-based execution with RBAC)
- Real-time streaming responses
- Standardized A2A protocol for interoperability
- Agent cards for coordination and validation

**API Design**:

*Agent CRD defines all execution configuration:*
```yaml
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Agent
metadata:
  name: code-analyzer-agent
spec:
  prompt: "You are a code analyzer..."
  
  # Execution configuration (used by all steps using this agent)
  executionMode: service  # service (default); sandbox or substrate (future)
  serviceName: ottoflow-agent-executor  # Optional, defaults to ottoflow-agent-executor
  serviceNamespace: ottoflow-system  # Optional, defaults to ottoflow-system
  serviceAccount: agent-executor-sa  # For agent execution (if needed for MCP tools)
```

*Step references agent (uses Agent CRD configuration):*
```yaml
steps:
  - name: ai-analysis
    agentRef:
      name: "code-analyzer-agent"
      # Uses executionMode, serviceName, serviceNamespace, serviceAccount from Agent CRD
      # No overrides allowed - all execution config comes from Agent CRD
      additionalPrompts:
        - "'Analyze this code: ' + string(variables.code)"
```

**Execution Configuration**:
- Execution settings (executionMode, serviceName, serviceNamespace, serviceAccount) are defined exclusively in Agent CRD
- All steps using an Agent CRD use the same execution configuration
- No step-level overrides - all execution config must be in Agent CRD
- If Agent CRD doesn't specify values, system defaults apply:
  - `executionMode`: `service`
  - `serviceName`: `ottoflow-agent-executor`
  - `serviceNamespace`: `ottoflow-system`
  - `serviceAccount`: Controller's service account (if needed)

**Execution Modes**:
- `service`: Execute agent via AgentExecutor Service using A2A protocol (default)
- `sandbox`: Execute agent as Job with Agent Sandbox isolation (future enhancement)
- `substrate`: Execute agent as an Agent Substrate actor (future enhancement; see [PROPOSAL_AGENT_SUBSTRATE.md](PROPOSAL_AGENT_SUBSTRATE.md))

**Default Behavior**:
- Agent steps always execute via AgentExecutor Service (no in-process execution, no Jobs)
- Execution settings come exclusively from Agent CRD (no step-level overrides)
- Always uses A2A protocol with streaming mode (internal subset)

**Communication Mechanism**:
The workflow executor communicates with AgentExecutor Service using A2A protocol:

1. **Agent Card Resolution**:
   - Executor resolves agent card from: `https://{serviceName}.{serviceNamespace}.svc.cluster.local:8443/api/a2a/{namespace}/{agent-name}/.well-known/agent-card.json` (HTTPS required)
   - Agent card is derived from Agent CRD (capabilities, skills, etc.)
   - Used for coordination and validation

2. **Task Execution**:
   - Executor creates A2A client with streaming enabled
   - Builds A2A message with:
     - Text part: Evaluated prompt string
     - Structured part: JSON-encoded workflow context
   - Sends message via streaming: `POST /api/a2a/{namespace}/{agent-name}/tasks`
   - Collects streamed responses in real-time

3. **Result Processing**:
   - Executor collects all streamed response chunks
   - Extracts outputs using Agent CRD's outputExtraction configuration
   - Results are written to workflow context for subsequent steps

**AgentExecutor Service**:

*Service Deployment:*
```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ottoflow-agent-executor
  namespace: ottoflow-system
spec:
  replicas: 3
  selector:
    matchLabels:
      app: ottoflow-agent-executor
  template:
    metadata:
      labels:
        app: ottoflow-agent-executor
    spec:
      serviceAccountName: ottoflow-agent-executor
      containers:
      - name: agent-executor
        image: ghcr.io/nirmata/ottoflow/agent-executor:latest
        ports:
        - containerPort: 8080
          name: http
        readinessProbe:
          httpGet:
            path: /readyz
            port: 8080
        livenessProbe:
          httpGet:
            path: /healthz
            port: 8080
---
apiVersion: v1
kind: Service
metadata:
  name: ottoflow-agent-executor
  namespace: ottoflow-system
spec:
  selector:
    app: ottoflow-agent-executor
  ports:
  - port: 8080
    targetPort: 8080
    protocol: TCP
    name: http
  type: ClusterIP
```

**A2A Protocol (Internal Subset)**:
- **Agent Card Endpoint**: `GET /api/a2a/{namespace}/{agent-name}/.well-known/agent.json`
  - Returns agent card derived from Agent CRD
  - Includes capabilities (streaming: true)
  - Includes skills (derived from MCPTools)
- **Task Execution Endpoint**: `POST /api/a2a/{namespace}/{agent-name}/tasks`
  - Always uses streaming mode
  - JSON-RPC 2.0 protocol
  - A2A message format (message, messageParts)
- **What We Don't Implement** (not needed for internal use):
  - External agent discovery
  - Push notifications
  - State transition history
  - Async polling (use streaming instead)

**Agent Executor Container Behavior**:

*Service Mode (A2A/HTTPS):*
1. Starts TLS server listening on configured port (default: 8443)
2. Waits for task request from executor via A2A protocol
3. Receives task request via `POST /api/a2a/{namespace}/{agent-name}/tasks`:
   - Request contains: prompt, context, agentRef, outputExtraction (A2A message format)
4. Loads Agent CRD using Kubernetes API client
5. Executes agent with LLM provider and MCP tools
6. Extracts outputs using configured output extraction
7. Returns results via A2A streaming response
8. Server continues running (long-lived Deployment)

**Status**: ✅ **Implemented** — Addresses scalability, reliability, and security limitations.

#### 2. Agent Sandbox Integration (Future Enhancement)

**Enhancement**: Add `executionMode: sandbox` for kernel-level isolation of agent execution using the [Kubernetes Agent Sandbox](https://agent-sandbox.sigs.k8s.io/) project ([kubernetes-sigs/agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox)).

**Benefits**:
- Kernel-level isolation (gVisor/Kata Containers) for untrusted code
- Process, storage, and network isolation
- Safe execution of AI-generated code without cluster compromise
- Prevents agent bugs from affecting controller or other steps
- Standardized Kubernetes API: no custom isolation layer; operators choose backend (gVisor, Kata)
- Optional SandboxWarmPool for faster cold start

**Agent Sandbox API** (SIG Apps):
- **Sandbox** (core): stateful, singleton pod with stable identity and persistent storage
- **SandboxTemplate** / **SandboxClaim** (extensions): reusable templates and claim-based creation
- **SandboxWarmPool**: pre-warmed sandboxes for quick allocation

**Implementation Approach**:
```yaml
# Agent CRD (executionMode: sandbox)
spec:
  executionMode: sandbox
  sandbox:
    templateName: "ottoflow-agent-sandbox"   # SandboxTemplate name
    templateNamespace: ""                   # optional
```

[Agent Substrate](https://github.com/agent-substrate/substrate) is a second candidate backend (`executionMode: substrate`); [PROPOSAL_AGENT_SUBSTRATE.md](PROPOSAL_AGENT_SUBSTRATE.md) proposes both backends behind one agent-executor gateway.


**Priority**: 🟡 **High** - Essential for secure execution of untrusted AI-generated code (Phase 2).

### High Priority Enhancements

#### 3. Performance Controls and Rate Limiting

**Current State**: Partly implemented. Shipped (see the [Workflow API reference](../user/reference/api/workflow.md#spec-workflowspec), [forEach](../user/reference/api/workflow.md#foreach), [cron triggers](../user/reference/api/workflow.md#cron-trigger) and [webhook triggers](../user/reference/api/workflow.md#webhook-trigger)):
- `spec.executionLimits.maxConcurrentSteps` caps how many ready steps start per DAG pass (they still run one at a time).
- `spec.executionLimits.outboundRequestsPerMinute` is one token bucket shared by a run's MCP, agent and A2A calls. Each `workflowRef` call builds a new, full bucket from the sub-workflow's own limit (unlimited if unset), not the parent's.
- `forEach.maxConcurrency` sets forEach fan-out per step.
- `spec.run.maxConcurrentRuns` skips creating a cron, event, webhook, MCP or A2A run while that many runs of the Workflow are unfinished (default 10). WorkflowRuns created directly are not gated.
- `triggers[].webhook.rateLimit` limits inbound webhook requests (default 60 per minute, burst 10).
- `ConcurrencyPolicy` is set per cron trigger (`triggers[].cron.concurrencyPolicy`).

**Enhancement**: Add a controller-wide cap across Workflows (a controller flag, not a Workflow field) and separate per-target rate limits (Kubernetes API, MCP, A2A) to prevent overwhelming Kubernetes API server and external services.

**Implementation Approach**:
```yaml
spec:
  executionLimits:
    rateLimits:
      kubernetesAPI: "100 req/s"
      mcpCalls: "50 req/s"
      a2aCalls: "20 req/s"
```

**Benefits**:
- Prevents API server overload
- Protects external services from rate limit violations
- Better resource utilization
- Improved system stability

**Priority**: 🟡 **High** - Important for production scale.

#### 4. Enhanced Observability

**Current State**: Implemented: Prometheus run and step counters, duration histograms and an active-runs gauge, plus custom output metrics ([metrics reference](../user/reference/metrics.md)); OpenTelemetry tracing with a span per step ([DESIGN_OTEL_GENAI.md](DESIGN_OTEL_GENAI.md)); structured logging ([logging reference](../user/reference/logging.md)); status tracking and Kubernetes events. Not implemented: resource-usage metrics, JSON log output, and a UI.

**Enhancement**: Add resource-usage metrics, JSON log output, and a web UI for workflow visualization, monitoring, and debugging.

**Benefits**:
- Better operational visibility
- Easier debugging and troubleshooting
- Performance monitoring and optimization
- User-friendly workflow management

**Priority**: 🟡 **High** - Critical for operations and debugging.
