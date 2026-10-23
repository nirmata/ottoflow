# Workflow

**Workflow** is an immutable template that defines steps, inputs, optional triggers, and outputs. It has no execution status and acts as a reusable blueprint. Execution is done by creating a [WorkflowRun](workflowrun.md) that references the Workflow.

- **API Group:** `ottoflow.nirmata.io`
- **Version:** `v1alpha1`
- **Kind:** `Workflow`
- **Scope:** Namespaced
- **Short name:** `flo`

---

## Spec (WorkflowSpec)

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `inputs` | [][Input](#input) | No | Input parameters. Values are provided when creating a WorkflowRun. Input values are always **strings**; use `json.unmarshal()` in CEL to parse structured values. |
| `variables` | [][Variable](#variable) | No | Top-level CEL expressions evaluated before steps. Access as `variables.<name>`. |
| `steps` | [][Step](#step) | Yes | Workflow steps to execute (min 1). |
| `outputs` | [][Output](#output) | No | Workflow-level outputs evaluated at completion, added to WorkflowRun status. |
| `triggers` | [][Trigger](#trigger) | No | Automatic triggers (cron, event, or webhook) that create WorkflowRuns. |
| `events` | [EventConfig](#eventconfig) | No | Kubernetes event emission (enabled, level). |
| `celCostLimit` | integer | No | Max CEL evaluation cost budget per expression (default: 2,097,152 units). |
| `executionLimits` | object | No | `maxConcurrentSteps` (cap on ready-steps batch) and `outboundRequestsPerMinute` (token bucket on MCP/agent/A2A calls). |
| `run` | object | No | Run policy: `retentionMinutes` (delete completed runs older than this; default 10080, i.e. 7 days; 0 = disable age-based deletion), `maxAllowed` (max completed runs kept; default 50; 0 = no limit; keeping all history requires both fields set to 0; see [Upgrading: default run-history cleanup](../../tasks/installation.md#upgrading-default-run-history-cleanup)), `maxConcurrentRuns` (skip trigger-created runs when this many are active). |
| `execution` | object | No | Default runner Job settings copied to trigger-created WorkflowRuns (see [WorkflowRun](workflowrun.md#workflowrunexecutionspec)). |

### Input

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | Yes | Input parameter name. |
| `description` | string | No | Human-readable description. |
| `default` | string | No | Default value. |
| `required` | boolean | No | Whether the input must be provided. |

### Variable

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | Yes | Variable name (access as `variables.<name>` in CEL). |
| `expression` | string | Yes | CEL expression. Can reference inputs and earlier variables. |

### Output

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | Yes | Output key name. |
| `expression` | string | No | CEL expression for the value (mutually exclusive with `value`). |
| `value` | object | No | Native YAML/JSON. String values (recursively through maps/arrays) **are CEL-evaluated**; when evaluation of a string fails, that string **silently falls back to the literal text**. If both set, `value` wins. |
| `metric` | [OutputMetric](#outputmetric) | No | Optional Prometheus metric (workflow-level outputs only). |
| `sensitive` | boolean | No | When `true`, the evaluated value is **not** written to `WorkflowRun.status.outputs`; a redacted placeholder is stored instead. Context for later steps and metrics is unchanged. Use for outputs that may contain secrets or PII. |

### OutputMetric

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | Yes | Metric name (prefixed with `ottoflow_workflow_`). |
| `type` | string | Yes | One of: `counter`, `gauge`, `histogram`. |
| `help` | string | No | Metric description. |
| `labels` | []{name, value} | No | Label key-value pairs (value is CEL expression). |
| `buckets` | []number | No | Histogram buckets (for type `histogram`). |

### EventConfig

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `enabled` | boolean | No | When nil or true, events emitted per Level; when false, none. |
| `level` | string | No | `Workflow` or `WorkflowAndSteps`. Default: `WorkflowAndSteps`. |

---

## Step

Each step has a unique **name** (camelCase) and exactly one execution type (expressions, agentRef, mcpToolCall, workflowRef, forEach, resourceQuery, prometheusQuery, mutate, stepTemplateRef, externalAgentRef, openReport, waitForCallback). Common fields apply to all steps.

### Common step fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | Yes | Unique step name (camelCase, e.g. `collectPodData`). |
| `message` | string | No | Human-readable description. |
| `dependsOn` | []string | No | Step names that must complete before this step. Dependencies are **explicit only** — referencing `variables.x` from another step does not create an ordering dependency; list the producing step here. |
| `expressions` | [][Expression](#expression) | No | CEL expressions for **expression steps only**. A step takes exactly one action: when a step has an action field (resourceQuery, agentRef, etc.), its `expressions` are **not** evaluated. |
| `outputs` | [][Output](#output) | No | Key-value pairs written to shared context (readable as `variables.<name>` by later steps). Evaluated for all step types, including resourceQuery steps (in addition to `resourceQuery.outputs`). |
| `matchConditions` | [][MatchCondition](#matchcondition) | No | Step runs only if ALL conditions evaluate to true. |
| `retry` | [RetryPolicy](#retrypolicy) | No | Retry configuration. |
| `timeout` | string | No | Max duration (e.g. `30s`, `5m`). |
| `failurePolicy` | string | No | `Fail` (default) or `Continue`. Applies to the step it is declared on, not to following steps. |

### Expression

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | Yes | Name to store the result (`expressions.<name>`). |
| `expression` | string | Yes | CEL expression. |

### MatchCondition

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | Yes | Identifier for the condition. |
| `expression` | string | Yes | CEL expression that must evaluate to true. |

### RetryPolicy

| Field | Type | Description |
|-------|------|-------------|
| `attempts` | integer | Max attempts (default 1). |
| `backoff` | object | `strategy` (none, linear, exponential), `initialInterval`, `maxInterval`, `multiplier`. |
| `retryOn` | []object | Optional: match by `errorMessage`, `errorType`, `httpStatus`. |

---

### Step types (one per step)

#### Expression step (default)

No additional fields. Use `expressions` and `outputs` to compute and publish values.

#### agentRef

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `agentRef.name` | string | Yes | Name of the Agent CRD. |
| `agentRef.namespace` | string | No | Namespace of the Agent (default: workflow namespace). |
| `agentRef.additionalPrompts` | []string | No | Prompts appended to agent system prompt; can contain CEL. |

#### mcpToolCall

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `mcpToolCall.server` | string | Yes | Name of the MCPServer CRD. |
| `mcpToolCall.tool` | string | Yes | Tool name within the MCP server. |
| `mcpToolCall.arguments` | map[string]string | No | Argument names to CEL expressions (evaluated in workflow context). |

Result is available as `toolResult` in this step's output expressions.

#### workflowRef

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `workflowRef.name` | string | Yes | Name of the Workflow to run as sub-workflow. |
| `workflowRef.namespace` | string | No | Namespace of the Workflow. |
| `workflowRef.inputs` | map[string]string | No | Input names to CEL expressions (evaluated in parent context). |

The sub-workflow runs inline in the same process (no separate Job or WorkflowRun object). Its workflow-level outputs are written into the parent context as `variables.<outputName>`.

#### forEach

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `forEach.items` | string | Yes | CEL expression that evaluates to a list. |
| `forEach.itemVariable` | string | No | Variable name for current item (default `item`). The current item is available as `variables.<itemVariable>` **and** as the bare `item` variable. |
| `forEach.maxConcurrency` | integer | No | Max concurrent child steps (default 5). |
| `forEach.itemFailurePolicy` | string | No | `Fail`: the forEach step **fails** when any item fails. `Continue`: the step succeeds even with failed items; a failed-item tally is recorded on the step message. |
| `forEach.step` | Step | No | Inline step to run for each item. |
| `forEach.stepTemplateRef` | StepTemplateRef | No | StepTemplate to instantiate for each item. |

Results: `steps.<stepName>.results`.

#### resourceQuery

Simplified Kubernetes resource query.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `apiVersion` | string | Yes | API version (e.g. `v1`, `apps/v1`). |
| `resource` | string | Yes | Resource kind (e.g. `Pod`, `Deployment`). |
| `namespace` | string | No | Namespace (CEL expression). |
| `name` | string | No | Resource name (CEL expression). Omit for a list query. |
| `labelSelector` | map[string]string | No | Label name → **CEL expression** (list queries only). Values are always evaluated as CEL — quote literals: `app: '"nginx"'`. |
| `fieldSelector` | string | No | **CEL expression** yielding a field selector string (list queries only). Always evaluated as CEL — a literal selector must be quoted inside the expression: `'"status.phase=Running"'`. An unquoted `status.phase=Running` is not valid CEL and fails at runtime. |
| `limit` | integer | No | Cap on the number of resources collected for list queries (0 = all). |
| `pageSize` | integer | No | Resources fetched per API call during list pagination (default 500, max 1000). |
| `outputs` | map[string]string | Yes | Output name → CEL expression, written to `variables.<name>`. For single-resource queries the fetched resource is bound to **`object`** (e.g. `object.status.phase`); for list queries the result list is bound to **`items`** (e.g. `items.map(i, i.metadata.name)`). |

A missing single resource is an error for the step. Step-level `outputs:` are also evaluated on resourceQuery steps (with `object`/`items` in scope), but any `expressions:` on the step are **not** evaluated — resourceQuery is the step's one action.

#### prometheusQuery

Prometheus (PromQL) query step. Runs a PromQL query with optional template variable substitution, then evaluates outputs over the result.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `prometheusQuery.query` | string | Yes | PromQL expression. May contain `{{.varName}}` placeholders; values come from `variables`. |
| `prometheusQuery.timeRange` | string | Yes | Lookback for the instant query (e.g. `"7d"`, `"1h"`, `"5m"`). Supports `d` (days). |
| `prometheusQuery.step` | string | No | Reserved for future range-query support. |
| `prometheusQuery.variables` | map[string]string | No | Placeholder name → CEL expression. Evaluated in workflow context; results substituted into `query`. |
| `prometheusQuery.outputs` | map[string]string | No | Output name → CEL expression. Expressions have `result` in scope (`result.type`, `result.samples`, `result.value`). If omitted, step writes full result as `result`. |

**Example:**

```yaml
prometheusQuery:
  query: 'sum by (pod, namespace) (container_cpu_usage_seconds_total{namespace="{{.namespace}}"})'
  timeRange: "7d"
  variables:
    namespace: 'inputs.namespace'
  outputs:
    sampleCount: 'size(result.samples)'
    firstValue: 'size(result.samples) > 0 ? result.samples[0].value : 0.0'
```

#### mutate

Kyverno-style mutate step that patches a single Kubernetes resource. The step GETs the target resource, evaluates the patch (CEL has `object` = current resource + workflow context), and applies it.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `mutate.target` | [StepMutateTarget](#stepmutatetarget) | Yes | Target resource (apiVersion, resource/kind, namespace, name). `namespace` and `name` are CEL expressions. |
| `mutate.patchType` | string | Yes | `ApplyConfiguration` or `JSONPatch`. |
| `mutate.applyConfiguration` | object | When patchType is ApplyConfiguration | `expression`: CEL that returns a **partial object** (map) to deep-merge onto the resource. Note: CEL `+` does not work on maps — build a map literal instead of adding to `object.metadata.labels`. |
| `mutate.jsonPatch` | object | When patchType is JSONPatch | `expression`: CEL that returns a list of `{op, path, value?}` (RFC 6902), or `operations`: static list of ops. |
| `mutate.outputs` | map[string]string | No | Output name to CEL; `object` refers to the patched resource. |

**StepMutateTarget:** `apiVersion`, `resource` (kind), `namespace` (optional CEL; omit for workflow namespace), `name` (CEL).

**Example (ApplyConfiguration – add label):**

```yaml
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

**Example (JSONPatch – add annotation):**

```yaml
mutate:
  target:
    apiVersion: v1
    resource: ConfigMap
    name: my-config
  patchType: JSONPatch
  jsonPatch:
    operations:
      - op: add
        path: /metadata/annotations/example.com~1key
        value: "value"
```

#### stepTemplateRef

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `stepTemplateRef.name` | string | Yes | Name of the StepTemplate. |
| `stepTemplateRef.namespace` | string | No | Namespace of the StepTemplate. |
| `stepTemplateRef.arguments` | map[string]string | No | Parameter names to CEL expressions. |

---

#### waitForCallback

Pauses workflow execution at this step and waits for an external callback before resuming. Enables **human-in-the-loop** and **AI-to-human-to-AI** patterns. The step generates a cryptographically secure token, stores it in `WorkflowRun.status.pendingCallback`, and the runner exits with code 0 (clean pause). The controller recreates the runner Job when callback data is received.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `waitForCallback.timeout` | string | **Yes** | Maximum wait duration (e.g. `"24h"`, `"30m"`). Behavior at expiry depends on `failurePolicy` (below): the step fails, or resumes with empty outputs. |
| `waitForCallback.callbackRef` | string | No | Human-readable label for the callback (used in logs and events). |
| `waitForCallback.message` | string | No | Message shown in logs when the step pauses; can include instructions for the callback caller. |
| `waitForCallback.outputSchema` | JSON Schema object | No | JSON Schema for callback payload validation. Required fields are enforced; missing required fields return 400. If absent, all payloads are accepted. |
| `waitForCallback.failurePolicy` | `Fail` \| `Continue` | No | Behavior on timeout. `Fail` (default): workflow fails. `Continue`: on timeout the gate resumes with empty outputs and the workflow proceeds to downstream steps. |

**Callback endpoint:**
```
POST /api/v1/workflow-runs/{namespace}/{name}/callback/{token}
Content-Type: application/json

{"approved": true, "reviewer": "alice@example.com"}
```

Responses:
- `200 OK` — callback accepted; WorkflowRun will resume on next reconcile.
- `400 Bad Request` — invalid JSON or schema validation failure.
- `401 Unauthorized` — invalid token or expired.
- `404 Not Found` — WorkflowRun not found or no pending callback.

**Status while waiting:**
```yaml
status:
  phase: Running
  stepStatuses:
    awaitApproval:
      phase: Waiting
      message: "Awaiting callback, expires at 2026-06-30T12:00:00Z"
  pendingCallback:
    tokenHash: "sha256hex..."   # SHA256 of the plaintext token; never use this as {token}
    stepName: awaitApproval
    expiresAt: 1751285400
```

> **Token delivery:** The plaintext callback token is available to subsequent steps as
> `steps.awaitApproval.outputs.callbackToken` (in-memory only; never persisted to K8s status).
> Use `${{ steps.awaitApproval.outputs.callbackToken }}` in a notification step to build the
> callback URL for the external caller.

**Accessing callback outputs** in downstream steps:
```
variables.approved    # true/false
variables.reviewer    # "alice@example.com"
```

Callback fields are written flat as `variables.<key>` and overwrite any existing variable of that name. If `outputSchema` declares `properties`, only those keys are written (an empty `properties: {}` therefore drops every key); without a `properties` object (or with an `outputSchema` that is not a JSON object), every top-level key of the callback body is written. A key is also absent when the callback body omits it, or when a `Continue` timeout resumes the step with empty outputs. Declare `properties` to bound what the callback caller can set.

**Constraints:**
- Only one `waitForCallback` can be active at a time per WorkflowRun (single `pendingCallback` slot).
- `waitForCallback` inside `forEach` is rejected at admission.
- Safe resume across a pause requires `WorkflowRun.spec.execution.checkpointing.enabled: true`; without it, steps that ran before the gate re-execute on resume (their outputs are not preserved).
- Token is single-use; a second callback for the same token after outputs are set returns `200 already_accepted`.

**Example:**

```yaml
steps:
  - name: awaitApproval
    waitForCallback:
      timeout: "24h"
      callbackRef: compliance-approval
      message: "Please review the findings and POST your decision to the callback URL."
      outputSchema:
        type: object
        properties:
          approved:
            type: boolean
          reviewer:
            type: string
        required: ["approved"]
      failurePolicy: Fail
```

See `samples/workflows/features/wait-for-callback.yaml` for a complete example.

---

## Triggers

When defined, triggers automatically create WorkflowRun instances. Multiple triggers use OR logic.

### Cron trigger

| Field | Type | Description |
|-------|------|-------------|
| `cron.schedule` | string | Cron expression (e.g. `0 0 * * *`). |
| `cron.timezone` | string | Optional timezone (default UTC). |
| `cron.concurrencyPolicy` | string | `Allow`, `Forbid`, or `Replace`. |
| `cron.inputValuesFrom` | [][CronInputFromSecret](#croninputfromsecret) | Inject input values from Secrets when the scheduler creates a WorkflowRun (e.g. Slack webhook URL). |

#### CronInputFromSecret

| Field | Type | Description |
|-------|------|-------------|
| `inputName` | string | Workflow input parameter name. |
| `secretRef` | [CronSecretKeyRef](#cronsecretkeyref) | Secret name, optional namespace, and data key. |

#### CronSecretKeyRef

| Field | Type | Description |
|-------|------|-------------|
| `name` | string | Secret name. |
| `namespace` | string | Secret namespace; if empty, the workflow's namespace is used. |
| `key` | string | Secret data key. |

### Event trigger

| Field | Type | Description |
|-------|------|-------------|
| `event.resources` | []{apiVersion, kind, namespace} | Resource types to watch. |
| `event.operations` | []string | `CREATE`, `UPDATE`, `DELETE` (empty = all). |
| `event.labelSelector` | object | Match labels (Kubernetes watch selector). |
| `event.fieldSelector` | string | Match fields (Kubernetes watch selector; only API-server-indexed fields, e.g. `metadata.name`). |
| `event.celFilter` | string | CEL boolean over the triggering resource (`object`); false or error drops the event. Use for status fields that fieldSelector cannot match. |
| `event.inputMapping` | map[string]string | Map event data to workflow inputs (CEL over `object`). |
| `event.dedupKey` | string | CEL expression extracting a dedup key (e.g. a revision field); same-key events are dropped. |
| `event.dedupWindow` | duration | Time-window dedup fallback (e.g. `30s`). |

### Webhook trigger

Fires a WorkflowRun on a signed HTTP POST to `/webhooks/{namespace}/{workflowName}` (controller port 8083). See [Setting Up Triggers — Webhook Triggers](../../tasks/triggers.md#webhook-triggers).

| Field | Type | Description |
|-------|------|-------------|
| `webhook.secretRef` | {name, namespace, key} | Secret holding the HMAC-SHA256 signing key (default key: `hmac-key`, min 32 bytes). Must be in the Workflow's namespace. |
| `webhook.celFilter` | string | CEL boolean over the parsed JSON body (`object`); false or error acknowledges without creating a run. |
| `webhook.inputMapping` | map[string]string | Workflow input name → CEL over the parsed body (`object`). |
| `webhook.dedupKey` | string | CEL expression extracting a dedup key from the body. |
| `webhook.dedupWindow` | duration | Dedup window (default 10m when `dedupKey` is set; max 1h). |
| `webhook.rateLimit` | {requestsPerMinute, burst} | Per-workflow rate limit on inbound webhook requests (defaults: 60/min, burst 10). |

---

## Authoritative schema

The full OpenAPI schema for Workflow (including all nested types and enums) is in `config/crd/bases/ottoflow.nirmata.io_workflows.yaml`.
