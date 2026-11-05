# OttoFlow Concepts

This section explains the core concepts and architecture of OttoFlow.

## Table of Contents

- [Architecture](architecture.md) - Component overview, the three images, and which image to update for a given change
- [AI Context Engineering Best Practices](#ai-context-engineering-best-practices)
- [Key Concepts](#key-concepts)
- [How Workflows Work](#how-workflows-work)
- [Step Types](#step-types)
- [Variable Access](#variable-access)
- [Agent Executor Security](agent-executor-security.md)

---

## Architecture Summary

OttoFlow uses **three container images** with distinct responsibilities:

- **`ghcr.io/nirmata/ottoflow/controller`** — Kubernetes operator (Deployment). Reconciles CRDs, spawns runner Jobs, manages TLS certs, serves admission webhooks. Does not execute workflow steps.
- **`ghcr.io/nirmata/ottoflow/agent-executor`** — HTTPS service (Deployment). Executes agent/LLM steps on behalf of runner pods via `POST /api/exec/{namespace}/{agentName}`. Persistent, shared across all WorkflowRuns.
- **`ghcr.io/nirmata/ottoflow/workflow-runner`** — Workflow execution engine (Job pod, ephemeral). Spawned once per WorkflowRun. Contains all CEL evaluation logic, step execution, and calls agent-executor for LLM steps.

**CEL library and execution logic lives in the workflow-runner.** To apply a CEL fix or update step execution behavior, update only the `--workflow-runner-image` flag on the controller — no need to redeploy the controller or agent-executor.

See [Architecture](architecture.md) for the full breakdown including a flow diagram, per-component details, and step-by-step instructions for updating the runner image.

---

---


## AI Engineering Best Practices

OttoFlow promotes AI context engineering best practices by addressing common challenges with LLM-based automation.

**Understanding Common Pitfalls**

A common approach to AI-powered automation involves writing broad, open-ended prompts like "diagnose pod failure using available tools and email results in a PDF". While this approach is great for one-off exploration on a personal device, it does not scale to large-scale production usage due to the following reasons:

- ❌ **Non-deterministic behavior**: LLM behaviors are non-deterministic and can lead to inconsistent results and are difficult to test, debug, and maintain in production
- ❌ **Context bloat**: Including large tool catalogs in prompts increases token usage, leading to higher AI costs, increased hallucinations, and reduced reliability.
- ❌ **Security risks**: Exposing a wide range of tools and permissions to LLMs creates a wide attack surfaces.
  
**OttoFlow's Solution**

OttoFlow addresses these issues through three core design principles:

- ✅ **Deterministic Workflows**: Workflows are defined as explicit, step-by-step DAGs with clear dependencies. Each step has a specific purpose and predictable execution path, ensuring consistent behavior across runs.
- ✅ **Sandboxed and Safe LLM Calls**: Agent steps are isolated within workflow boundaries with explicit permissions, input/output contracts, controlled context passing (only necessary data), and limited tool access scoped to specific steps. LLMs are only used for tasks they excel at i.e., tasks involving unstructured data, transformation, and synthesis.
- ✅ **Direct Tool Calls**: Instead of exposing entire tool catalogs to LLMs, workflows can make direct MCP tool calls as separate steps. This provides precise tool invocation without LLM interpretation, reduced context size and costs, better security through explicit permissions per step, and deterministic tool execution.

By combining deterministic workflow orchestration, sandboxed agent execution, and direct tool calls, OttoFlow enables production-ready AI automation workflows that are predictable, secure, and cost-effective.

## Key Concepts

### Workflow (Template)

A **Workflow** is an immutable template that defines:
- Steps to execute
- Input parameters
- Optional triggers (cron or event-based)
- Output definitions

Think of it as a blueprint that can be executed multiple times.

### WorkflowRun (Instance)

A **WorkflowRun** is an execution instance that:
- References a Workflow template
- Provides input values
- Tracks execution status
- Stores outputs

Each time you want to run a workflow, you create a WorkflowRun.

### Steps

Steps are the atomic units of work in a workflow. Each step:
- Has a unique name within the workflow
- Can produce outputs that other steps can consume
- Can depend on outputs from other steps
- Executes when all dependencies are satisfied

### Dependencies

Steps resolve dependencies based on:
- **Explicit dependencies**: `dependsOn` field (required)

Dependencies must be explicitly declared using the `dependsOn` field. The controller does not automatically infer dependencies from CEL expression references. If a step uses `variables.*` from another step, you must declare the dependency explicitly.

Independent steps (those without dependencies) execute in parallel for efficiency.

### Shared Context

All steps share a common context that includes:
- **Inputs**: Workflow input parameters (`inputs.*`) — always strings; use `json.unmarshal()` for structured values
- **Variables**: Workflow-level variables and outputs from completed steps (`variables.*`, flat namespace)
- **Expressions**: Current step's expression results (`expressions.*`)

### CEL Expressions

OttoFlow uses the Common Expression Language (CEL) for:
- Data transformation
- Kubernetes resource queries
- Conditional logic
- String manipulation

CEL expressions have access to the complete suite of Kyverno CEL libraries (via [Kyverno SDK CEL](https://github.com/kyverno/sdk/tree/main/cel)) and Kubernetes CEL libraries.

### Workflow Triggers

Workflow triggers enable automatic execution of workflows without manually creating WorkflowRun instances. Triggers are defined in the Workflow template and automatically create WorkflowRun instances when their conditions are met.

#### Cron Triggers

Cron triggers execute workflows on a schedule using standard cron syntax.

```yaml
triggers:
  - cron:
      schedule: "0 0 * * *"  # Daily at midnight
      timezone: "America/New_York"  # Optional, defaults to UTC
      concurrencyPolicy: "Forbid"  # Allow, Forbid (default), or Replace
```

**Features:**
- **Schedule**: Standard cron expression (e.g., `"*/5 * * * *"` for every 5 minutes)
- **Timezone**: Optional timezone specification (defaults to UTC)
- **Concurrency Policy**: Controls how concurrent executions are handled
  - `Allow`: Permit multiple concurrent runs
  - `Forbid`: Skip if a previous run is still active (default)
  - `Replace`: Cancel the previous run and start a new one

Cron triggers are managed by an in-process scheduler that runs under leader election, ensuring only the leader pod fires schedules. IANA timezones (e.g., `America/New_York`) are supported via the `timezone` field.

#### Event Triggers

Event triggers execute workflows when specific Kubernetes resources are created, updated, or deleted.

```yaml
triggers:
  - event:
      resources:
        - apiVersion: v1
          kind: Pod
          namespace: default
        - apiVersion: apps/v1
          kind: Deployment  # no namespace: all namespaces, which needs the event-trigger allowlist
      operations:
        - CREATE
        - UPDATE
      labelSelector:
        matchLabels:
          app: monitored
      fieldSelector: "metadata.name=my-pod"  # Optional
      inputMapping:  # Optional - map event data to workflow inputs
        podName: "object.metadata.name"
        namespace: "object.metadata.namespace"
```

**Features:**
- **Resources**: List of resource types to watch (apiVersion, kind, namespace); a namespace other than the Workflow's, an empty one (all namespaces), a cluster-scoped kind or a kind the API server cannot resolve needs the controller allowlist; see [Upgrading: event-trigger watch scope](../tasks/installation.md#upgrading-event-trigger-watch-scope)
- **Operations**: Filter by operation type (CREATE, UPDATE, DELETE). If empty, all operations trigger
- **Label Selector**: Filter resources by labels
- **Field Selector**: Filter resources by field values
- **Input Mapping**: Map event data to workflow input parameters using CEL expressions

Event triggers use Kubernetes watch APIs and are managed with leader election to ensure only one controller watches events.

#### Multiple Triggers

A single workflow can define multiple triggers. When any trigger fires, a new WorkflowRun is created (OR logic).

```yaml
triggers:
  # Trigger 1: Cron schedule
  - cron:
      schedule: "0 * * * *"
  # Trigger 2: ConfigMap events
  - event:
      resources:
        - apiVersion: v1
          kind: ConfigMap  # no namespace: all namespaces, which needs the event-trigger allowlist
      operations:
        - CREATE
        - UPDATE
```

#### Trigger Information

WorkflowRuns created by triggers record trigger metadata in their status. Inspect it with kubectl:

```bash
kubectl get workflowrun <name> -o jsonpath='{.status.trigger}'
```

The trigger information includes:
- **type**: `Manual`, `Cron`, `Event`, or `Webhook`
- **triggeredAt**: When the trigger fired
- **eventResource / webhookRequest**: Details of the triggering resource or HTTP request

Note: trigger metadata is **not** available as a CEL variable inside workflow steps (`status` is not in the CEL context). To pass event data into a workflow, use the trigger's `inputMapping` to map fields from the triggering object into workflow inputs.

---

## How Workflows Work

### Execution Flow

1. **WorkflowRun Created**: A WorkflowRun is created either:
   - **Manually**: User creates a WorkflowRun referencing a Workflow template
   - **Automatically**: A trigger (cron or event) defined in the Workflow template automatically creates a WorkflowRun when its conditions are met
2. **Input Validation**: Controller validates that all required inputs are provided
3. **Context Initialization**: Input values populate the initial shared context (for triggered workflows, inputs may be mapped from trigger data)
4. **DAG Construction**: Controller builds a dependency graph from step references
5. **Step Execution**: Steps execute when dependencies are satisfied (in parallel when possible)
6. **Output Collection**: Completed step outputs are stored in shared context
7. **Completion**: WorkflowRun completes when all steps finish

### Dependency Resolution

Dependencies must be explicitly declared using the `dependsOn` field in each step. The controller:
- Builds a Directed Acyclic Graph (DAG) from explicit `dependsOn` declarations
- Validates that there are no circular dependencies
- Executes steps in the correct order based on the DAG
- Runs independent steps concurrently

**Important**: Dependencies are NOT automatically inferred from CEL expression references. If a step references `variables.*` from another step, you must explicitly declare the dependency using `dependsOn`.

#### Pre-Execution Validation

Before execution begins, the controller validates:
- **Dependency references**: All `dependsOn` entries must reference existing step names. If a step references a non-existent step, the workflow fails immediately.
- **Circular dependencies**: The DAG must be acyclic. If circular dependencies are detected, the workflow fails immediately.

These validation errors are reported in `WorkflowRun.Status.Message` and the workflow phase is set to `Failed`.

#### Runtime Variable Resolution

Variable resolution happens at runtime when CEL expressions are evaluated:
- If a step references `variables.*` that doesn't exist (e.g., missing `dependsOn` or the dependency step hasn't produced the output yet), the CEL evaluation fails.
- Variable resolution errors are caught during step execution and reported in:
  - `StepStatus.Error` - Detailed error message
  - `StepStatus.Message` - Error message (for compatibility)
  - `WorkflowRun.Status.Message` - Summary message
  - `StepStatus.Phase` - Set to `Failed`

If a step fails and `FailurePolicy` is not set to `Continue`, the workflow phase is set to `Failed` and execution stops.

### Example

```yaml
steps:
  - name: stepA
    outputs:
      - name: value
        expression: '"hello"'
  
  - name: stepB
    dependsOn:
      - stepA  # Explicit dependency declaration required
    outputs:
      - name: result
        expression: 'variables.value + " world"'
```

In this example:
- `stepB` explicitly declares a dependency on `stepA` using `dependsOn`
- `stepB` references `variables.value` from `stepA` in its expression
- `stepA` executes first
- `stepB` waits for `stepA` to complete before executing
- The final result stored in `variables.result` is `"hello world"`

---

## Step Types

OttoFlow supports multiple step types, each optimized for different use cases:

### Expression Steps (Default)

Use CEL expressions for data transformation and computation.

```yaml
steps:
  - name: processData
    expressions:
      - name: result
        expression: 'inputs.value * 2'
    outputs:
      - name: doubled
        expression: 'expressions.result'
```

### Mutate Steps

Patch a single Kubernetes resource using CEL (ApplyConfiguration) or RFC 6902 JSON Patch. The patch expression has access to `object` (the current resource) and workflow context. For `ApplyConfiguration`, return a **partial object** to deep-merge onto the resource. (Note: CEL `+` does not work on maps, so build a map literal rather than adding to `object.metadata.labels`.)

```yaml
steps:
  - name: addLabel
    mutate:
      target:
        apiVersion: v1
        resource: ConfigMap
        namespace: default
        name: 'inputs.configMapName'
      patchType: ApplyConfiguration
      applyConfiguration:
        expression: '{"metadata": {"labels": {"managed": "ottoflow"}}}'
```

### Resource Query Steps

Simplified DSL for querying Kubernetes resources. In `outputs`, the fetched object is available as `object` (single-resource queries) or `items` (list queries). Note: `resourceQuery` is the step's one action — any `expressions:` on the same step are not evaluated.

```yaml
steps:
  - name: getPodInfo
    resourceQuery:
      apiVersion: v1
      resource: Pod
      namespace: 'inputs.namespace'
      name: 'inputs.podName'
      outputs:
        phase: object.status.phase
        restartCount: object.status.containerStatuses.map(c, int(c.restartCount)).sum()
```

### Prometheus Query Steps

First-class PromQL query steps with template variable substitution. Use `prometheusQuery` instead of embedding `prometheusMetrics(...)` in CEL for clearer queries and better error messages.

```yaml
steps:
  - name: cpuUsage
    prometheusQuery:
      query: 'sum by (pod) (rate(container_cpu_usage_seconds_total{namespace="{{.namespace}}"}[5m]))'
      timeRange: "7d"
      variables:
        namespace: 'inputs.namespace'
      outputs:
        samples: 'result.samples'
        count: 'size(result.samples)'
```

### Agent Steps

LLM-powered steps using Agent CRDs.

```yaml
steps:
  - name: analyzeIssue
    agentRef:
      name: diagnostic-agent
      additionalPrompts:
        - "'Analyze these logs: ' + string(variables.podLogs)"
    outputs:
      - name: diagnosis
        expression: 'agentOutputs.diagnosis'
```

Output extraction (json/regex/text) is configured on the **Agent CRD** via `spec.outputExtraction`, not on the step. Extracted values are available in the step's outputs as `agentOutputs.<key>`; the raw response text is `agentResponse`.

### MCP Tool Call Steps

Direct invocation of Model Context Protocol tools.

```yaml
steps:
  - name: getPodLogs
    mcpToolCall:
      server: kubernetes-mcp
      tool: get-pod-logs
      arguments:
        namespace: '"default"'
        podName: 'inputs.podName'
    outputs:
      - name: logs
        expression: 'toolResult'
```

### Workflow Reference Steps

Execute other workflows as sub-workflows (inline, in the same process). The sub-workflow's workflow-level outputs are written into the parent context as `variables.<outputName>`.

```yaml
steps:
  - name: callSubWorkflow
    workflowRef:
      name: child-workflow
      inputs:
        message: '"Hello from parent"'
    outputs:
      - name: subResult
        expression: 'variables.childWorkflowOutput'
```

---

## Variable Access

Variables are accessed using dot notation in CEL expressions:

### Inputs

```yaml
expressions:
  - name: greeting
    expression: '"Hello, " + inputs.name'
```

### Step Outputs

```yaml
expressions:
  - name: combined
    expression: 'variables.step1Output + variables.step2Output'
```

### Current Step Expressions

```yaml
expressions:
  - name: pod
    expression: 'resource.Get("v1", "Pod", "default", "my-pod")'
  - name: phase
    expression: 'expressions.pod.status.phase'  # References earlier expression
```

### Special Variables

- **Agent Steps**: `agentResponse` - LLM response text
- **MCP Tool Steps**: `toolResult` - Tool call result
- **OpenReports Steps**: `reportResult` - Report generation result

---

## Next Steps

- Learn how to [create your first workflow](../tasks/getting-started.md)
- Explore step types in detail in the [Workflow API reference](../reference/api/workflow.md#step-types-one-per-step)
- See the [API Reference](../reference/api/README.md) for complete specifications
