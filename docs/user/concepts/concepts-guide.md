# OttoFlow: Concept-by-Concept Guide

A bottom-up walkthrough of OttoFlow's execution model — from CRDs to running steps.

---

## Concept 1: The Data Model — CRDs

OttoFlow is built on 5 Kubernetes CRDs (all in `api/v1alpha1/`):

| CRD | File | Role |
|-----|------|------|
| **Workflow** | `workflow_types.go` | The *template* — defines steps, inputs, variables, triggers, outputs |
| **WorkflowRun** | `workflowrun_types.go` | An *instance* of a Workflow — carries input values, tracks status |
| **Agent** | `agent_types.go` | LLM config — provider, model, MCP tools, system prompt |
| **MCPServer** | `mcpserver_types.go` | MCP server config — transport (stdio/http/SSE), auth |
| **StepTemplate** | `steptemplate_types.go` | Reusable parameterized step with `{{.paramName}}` placeholders |

**Key relationship:** `WorkflowRun.Spec.WorkflowRef` → `Workflow`. A Workflow is a blueprint; WorkflowRun is the execution record. This mirrors the Kubernetes `Deployment` (template) vs `Pod` (instance) split.

---

## Concept 2: Step Types — The Action Primitives

Every `Step` has exactly ONE action field. These are the building blocks:

```
Step (api/v1alpha1/workflow_types.go)
├── expressions []Expression     → Pure CEL evaluation (default, no special field)
├── resourceQuery                → Kubernetes GET/LIST via simplified syntax
├── agentRef                     → LLM-powered step using an Agent CRD
├── mcpToolCall                  → Direct MCP tool call, no LLM
├── workflowRef                  → Sub-workflow (runs inline, not a new Job)
├── prometheusQuery              → PromQL query with template variables
├── mutate                       → Kyverno-style CEL/JSONPatch resource patching
├── openReport                   → Emit results as an OpenReports.io Report CRD
├── waitForCallback              → Pause until an external signed callback arrives
├── stepTemplateRef              → Instantiate a parameterized StepTemplate
├── forEach                      → Parallel iteration over a list
└── externalAgentRef             → A2A protocol call to an external agent (kagent, etc.)
```

The step dispatcher in `executeStep()` (`internal/workflow/executor/executor.go`) checks these fields in this exact priority order:

`forEach → stepTemplateRef → workflowRef → agentRef → mcpToolCall → externalAgentRef → resourceQuery → prometheusQuery → mutate → openReport → waitForCallback → expressions (default)`

A step takes exactly ONE action. In particular, `expressions:` are only evaluated on pure expression steps — they are ignored on a step that also has an action field like `resourceQuery`.

---

## Concept 3: CEL Expression System — The Data Layer

CEL (Common Expression Language) is the glue that connects steps. Every output, every condition, every input mapping is a CEL expression. The following variables are available during execution:

```
inputs.*              → Workflow input values (always strings)
variables.*           → Workflow-level variables AND step outputs (flat, no step prefix)
steps.<stepName>.*    → Step-scoped results (e.g. forEach results)
expressions.*         → Current step's inline expression results
item                  → Current item in a ForEach loop
toolResult            → MCP tool call result
agentResponse         → Raw LLM output text
agentOutputs          → Structured extracted outputs from agent
a2aResult             → External A2A agent result
object                → Fetched resource in ResourceQuery steps; target resource in Mutate steps
items                 → List query results in ResourceQuery steps
result                → Prometheus query result (result.samples, result.value, result.type)
```

### CEL Evaluator Architecture (`internal/workflow/executor/cel.go`)

```
CELEvaluator
├── env *cel.Env                        → Pre-built environment with ALL libraries loaded
├── programCache LRU[string, Program]   → Compiled expression cache (default 1000 entries)
├── programOptions []ProgramOption      → Library extension hooks
└── celCostLimit uint64                 → Budget (default 2M units — ~2<<20)
```

Programs are cached in an LRU so the same expression across many WorkflowRuns is compiled only once.

### Libraries Wired Into CEL (`internal/workflow/executor/cel_libraries.go`)

OttoFlow embeds Kyverno's CEL libraries:

| Library | What it adds |
|---------|-------------|
| `kyvernoresource` | `resource.Get()`, `resource.List()` macros |
| `kyvernoglobalcontext` | Global K8s context access |
| `kyvernohttp` | HTTP calls from CEL expressions |
| `kyvernoimagedata` | Container image metadata |
| `kyvernojson` / `kyvernoyaml` | Parse and serialize |
| Custom | `format.*`, `string.*`, `list.*`, `json.*` functions |

### Expression Evaluation Flow

```
1. contextManager.ReadContext(ctx)       → current state map
2. celEvaluator.BuildVariableMap(data)  → flat map for CEL variable binding
3. celEvaluator.EvaluateExpression()    → compile-or-cache, then eval
4. contextManager.WriteStepOutputs()    → store result for next steps
```

---

## Concept 4: DAG Execution — How Steps Are Scheduled

**File:** `internal/workflow/executor/dag.go`

```
DAG
├── nodes map[string]*Node     → One node per step
└── edges map[string][]string  → fromStep → []dependentSteps
```

### BuildDAG (`dag.go`)

```
1. Add all steps as nodes
2. For each step with dependsOn: add directed edges (dependency → step)
3. Run DFS cycle detection → return error if cycle found
```

> **Critical constraint:** Dependencies are determined **only** by explicit `dependsOn` field values. Referencing `steps.foo.bar` in a CEL expression does NOT implicitly create an ordering dependency — you must also declare `dependsOn: [foo]`.

### Execution Loop (`ExecuteWorkflow`, `executor.go`)

```
LOOP (max len(steps)*2 iterations as a safety cap):
  completedSteps = {name: isStepDone(step, status)}
  readySteps = dag.GetReadySteps(completedSteps)   ← steps where ALL deps are done
  if maxConcurrentSteps > 0: truncate readySteps slice

  if readySteps is empty:
    if all done → recordWorkflowSuccess → return nil
    if any failed → recordWorkflowFailure → return error

  for each readyStep:
    check matchConditions (CEL bool) → mark Skipped if false
    mark Running → executeStep() → mark Succeeded or Failed
    save checkpoint after each step
```

### Step Phases

```
Pending → Running → Succeeded
                 → Failed      (stops workflow unless failurePolicy: Continue)
                 → Skipped     (matchConditions evaluated to false)
```

`isStepDone()` returns `true` for `Succeeded`, `Skipped`, or `Failed` with `failurePolicy: Continue`.

`failurePolicy` applies to the step it is declared on — it does not change how later steps behave. Inside forEach, `itemFailurePolicy: Fail` fails the whole step when any item fails; `Continue` lets the step succeed and records a failed-item tally on the step message.

---

## Concept 5: Controller Layer — Kubernetes Reconciliation

**File:** `internal/workflow/controller/workflowrun_controller.go`

```
WorkflowRunReconciler
├── Client              → K8s API client
├── CELCache            → Pre-compiled CEL programs (shared across all runs)
├── RunnerConfig        → Image, ServiceAccount, RBAC, secrets configuration
└── ControllerNamespace → Fallback namespace for Workflow lookup
```

### Reconcile Loop (`workflowrun_controller.go`)

```
1. Fetch WorkflowRun
2. If already Succeeded or Failed:
   → applyRunPolicy (retention / maxAllowed cleanup) → done
3. reconcileJobExecution:
   a. getReferencedWorkflow       (with controller-namespace fallback)
   b. ensureRunnerAccess          → ClusterRoleBinding for runner ServiceAccount
   c. ensureAgentExecutorCallerBinding → RBAC for A2A auth
   d. injectWellKnownLLMCredentials   → auto-inject LLM secrets
   e. ensureRunnerSecrets         → copy required secrets to run namespace
   f. buildWorkflowRunnerJob      → construct batch/v1 Job spec
   g. Create Job (or check if existing)
   h. Monitor Job status → update WorkflowRun.Status
```

The controller builds a Kubernetes `batch/v1 Job` for each WorkflowRun. That Job runs the `workflow-runner` binary (`cmd/workflow-runner/`), which fetches the Workflow and WorkflowRun from the API server and calls `ExecuteWorkflow()`.

**Two-cluster model:** `controlClient` reads OttoFlow objects; `targetClient` operates on workload resources on the target cluster (enabled by `ClusterRef`).

---

## Concept 6: WorkflowRun Execution Spec

```
WorkflowRunSpec
├── workflowRef                  → Which Workflow template to run
├── inputValues map[string]string → Input overrides
├── clusterRef                   → Optional: target a different cluster
│   ├── local: true              → Use in-cluster config (hub)
│   └── kubeConfigSecretRef      → Secret holding a kubeconfig for a remote cluster
├── events                       → Override event emission level for this run
└── execution
    ├── job                      → Env vars, volumes, resources, TTL for the runner Job
    ├── checkpointing            → Per-step checkpoint ConfigMaps + maxRestartAttempts
    └── llmCredentialsSecret     → Override the cluster-wide well-known LLM creds Secret
```

---

## Concept 7: Checkpointing — Crash Recovery

**File:** `internal/workflow/executor/checkpoint.go`

After each step completes, the executor writes a `CheckpointSnapshot` to a ConfigMap:

```go
CheckpointSnapshot{
  Version:           1,
  LastCompletedStep: stepName,
  StepStatuses:      workflowRun.Status.StepStatuses,
  Context:           contextManager.GetContext(),   // full CEL variable map
}
```

On pod restart (`Phase == Running` or `Attempts > 0`), `loadCheckpointIfNeeded()` restores the context and step statuses — the DAG loop then skips already-completed steps and resumes from the failure point.

**Known limitation:** ForEach inner items are NOT checkpointed. If a pod crashes mid-forEach, all items replay from the beginning on resume.

Enable via:
```yaml
spec:
  execution:
    checkpointing:
      enabled: true
      maxRestartAttempts: 3
```

---

## Concept 8: Agent Layer — LLM Integration

**Files:** `internal/agent/`, `internal/workflow/executor/agent_executor.go`

```
RoutingAgentExecutor (selects an executor per call by provider)
└── DefaultAgentExecutor
    ├── ExecuteAgent(ctx, agentCRD, prompt, workflowContext, namespace) → calls gollm.Client
    ├── mcpProvider                 → MCPClientManager (lazy stdio connections)
    └── OutputExtractor             → json/regex/text extraction from LLM response
```

### MCPClientManager — Lazy Connections

```
MCPClientManager
├── clients map[key]→MCPClient     → LRU cache with idle eviction (5 min)
├── GetClient(ctx, serverName, ns) → create-or-return-cached
└── StartEviction(5min, timeout)   → background goroutine cleans idle stdio servers
```

Stdio MCP servers (e.g. `uvx`-backed tools) spawn child processes on first use. The eviction goroutine kills idle ones to avoid process leaks. This lazy approach also handles slow startup tools — the connection happens on first step that needs the tool, not at executor initialization.

---

## Concept 9: Rate Limiting & Execution Limits

**Source:** `ExecutionLimits` in `workflow_types.go`, enforced in `executor.go`

```yaml
spec:
  executionLimits:
    maxConcurrentSteps: 3          # cap ready-steps batch per DAG iteration
    outboundRequestsPerMinute: 60  # token bucket on MCP/agent calls
```

The rate limiter is constructed fresh per `ExecuteWorkflow()` call (and torn down with `defer`). Burst capacity is `rpm/6 + 1` to allow short bursts while protecting downstream services.

---

## Concept 10: Triggers — Automatic WorkflowRun Creation

**Source:** `WorkflowSpec.Triggers`, `internal/workflow/controller/`

Two trigger types automatically create `WorkflowRun` objects:

| Type | Mechanism | Implementation |
|------|-----------|----------------|
| **Cron** | Schedule expression | `robfig/cron/v3`, timezone-aware, leader-elected |
| **Event** | K8s resource watch | Dynamic informer on target resource GVK |

`RunPolicy.MaxConcurrentRuns` prevents trigger storms — if active runs ≥ limit, new run creation is skipped.

---

## Concept 11: CLI Local Mode

**File:** `cli/internal/executor/executor.go`

Workflows can run **without a cluster controller** using `--workflow-dir`:

```bash
ottoflow run <workflow-name> --workflow-dir ./workflows
```

In local mode:
- Workflow YAML is loaded from disk into a fake control-plane client built from the files in that directory — StepTemplates (and other referenced OttoFlow objects) must live under the same directory tree
- `WorkflowExecutor` runs in-process (no Job is created)
- `--namespace` must match the workflow's own `metadata.namespace`, even in local mode
- Agent steps use a local `agentExecutor` (requires LLM credentials)
- MCP servers still connect via stdio/http
- `resourceMetrics()` works: a metrics client is wired from your kubeconfig
- Cannot fetch Pod logs — `resourceQuery` reads live objects only, with no `kubectl logs`
  equivalent, so workflows that need log data (e.g. `workload-troubleshooter.yaml`) require the
  in-cluster controller

The `localExecutionMode=true` flag in `WorkflowExecutor` gates the in-process agent execution path.

---

## Concept 12: Functional Area Map

| Area | What it contains |
|------|-----------------|
| Executor | Core execution engine, all step executors, CEL, DAG |
| Controller | WorkflowRun/Workflow reconcilers, TriggerManager, Scheduler |
| Webhook | Admission webhooks for Workflow/WorkflowRun validation |
| Rbac | RBAC generation (`ottoflow generate rbac`) |
| Workflow-runner | In-Job binary — fetches WorkflowRun and drives executor |
| Agent | Agent executor, MCPClientManager, OutputExtractor |
| Cmd | CLI entry points (run, generate, validate) |
| Metrics | Prometheus metric definitions |
| Display | CLI streaming display, markdown rendering |
| Agent-executor | A2A protocol server with SubjectAccessReview auth |

---

## End-to-End Flow

```
User applies WorkflowRun CR
  │
  ▼
WorkflowRunReconciler.Reconcile()
  ├── getReferencedWorkflow()
  ├── ensureRunnerAccess()           → RBAC ClusterRoleBinding
  ├── injectWellKnownLLMCredentials()
  └── buildWorkflowRunnerJob()
  │
  ▼
batch/v1 Job created → workflow-runner pod starts
  │
  ▼
workflow-runner binary
  ├── Fetches Workflow + WorkflowRun from K8s API
  └── NewWorkflowExecutor(client, workflowRun)
        ├── NewCELEvaluatorWithMetrics()
        ├── NewMCPClientManager()
        └── NewRoutingAgentExecutor(mcpManager)
  │
  ▼
ExecuteWorkflow(ctx, workflow, workflowRun)
  ├── loadCheckpointIfNeeded()       → restore from crash if applicable
  ├── InitializeContext(inputs)
  ├── evaluateWorkflowVariables()    → sequential CEL eval (variables.*)
  ├── BuildDAG(steps)                → explicit dependsOn only, cycle-checked
  └── LOOP:
        readySteps = dag.GetReadySteps(completedSteps)
        for step in readySteps:
          checkMatchConditions()     → skip if false
          executeStep()              → dispatch by step type
            Expression / ResourceQuery / AgentRef / MCPToolCall /
            WorkflowRef / ForEach / ExternalAgentRef / ...
          WriteStepOutputs() → context (available as steps.<name>.*)
          saveCheckpoint()
      evaluateWorkflowOutputs()
      WorkflowRun.Status.Phase = Succeeded
  │
  ▼
WorkflowRunReconciler sees Job succeeded
  ├── Updates WorkflowRun status
  └── applyRunPolicy() → GC old completed runs
```
