# CEL Reference

OttoFlow provides powerful CEL (Common Expression Language) capabilities for workflow logic. This document covers:

- **OttoFlow-specific macros** - New functions unique to OttoFlow
- **Kyverno CEL libraries** - Complete suite of Kyverno CEL libraries, provided via [Kyverno SDK CEL](https://github.com/kyverno/sdk/tree/main/cel) (see [Kyverno CEL Libraries](https://kyverno.io/docs/policy-types/cel-libraries/) for behavior and examples)
- **Kubernetes CEL libraries** - Standard Kubernetes libraries (see [Kubernetes CEL Libraries](https://kubernetes.io/docs/reference/using-api/cel/))

---

## OttoFlow Resource Macros

OttoFlow provides several macros for common Kubernetes operations:

### `resourceMetrics(apiVersion, kind, namespace, name, metricName)`

Fetch resource usage metrics from Kubernetes metrics API.

**Parameters:**
- `apiVersion` (string): API version (e.g., `"v1"`)
- `kind` (string): Resource kind (e.g., `"Pod"`)
- `namespace` (string): Namespace
- `name` (string): Resource name
- `metricName` (string): 
  - Empty string for standard CPU/memory metrics
  - Metric name for Custom Metrics API (e.g., `"nvidia_com_gpu_utilization"`)

**Returns:** Map with metrics data:
- **Standard metrics**: `totalCPU`, `totalMemory`, `containerMetrics`, `timestamp`, `window`
- **Custom metrics**: `metricName`, `value`, `timestamp`, `window`

**Example:**
```yaml
expressions:
  # Standard CPU/memory metrics
  - name: metrics
    expression: 'resourceMetrics("v1", "Pod", "default", "my-pod", "")'
  
  # Custom metrics (GPU, etc.)
  - name: gpuMetrics
    expression: 'resourceMetrics("v1", "Pod", "default", "gpu-pod", "nvidia_com_gpu_utilization")'
```

**Use Cases:**
- CPU and memory monitoring
- GPU metrics collection
- Resource usage analysis
- Performance monitoring

**Local mode:** `resourceMetrics()` also works in CLI local mode (`--workflow-dir`); a metrics client is wired from your kubeconfig.

---

### `prometheusMetrics(query, timeRange)`

Query Prometheus directly using PromQL.

**Parameters:**
- `query` (string): PromQL query string
- `timeRange` (string): Time range string (e.g., `"5m"`, `"1h"`)

**Returns:** Prometheus query result as structured map

**Example:**
```yaml
expressions:
  - name: gpuQuery
    expression: >-
      prometheusMetrics(
        'nvidia_gpu_utilization{pod="' + inputs.podName + '"}',
        '5m'
      )
```

**Use Cases:**
- Custom Prometheus queries
- Advanced metrics analysis
- Multi-metric aggregation
- Historical data queries

---

### `resource.GetLogs(namespace, podName, containerName, tailLines)`

Retrieve the last N lines of a pod container's log.

**Parameters:**
- `namespace` (string): Pod namespace. Pass `""` to use the WorkflowRun's namespace.
- `podName` (string): Name of the pod.
- `containerName` (string): Container name. Pass `""` for the pod's default container.
- `tailLines` (int): Lines to return from the end of the log. `0` returns the full log. Values above 10 000 are clamped to 10 000. Responses larger than 4 MB are truncated with a `\n[truncated]` sentinel.

**Returns:** Log text as string, or `""` when the pod has no logs yet (e.g. `ImagePullBackOff`). Returns a CEL error when the pod does not exist.

**Example:**
```yaml
steps:
  - name: collectLogs
    type: expressions
    spec:
      expressions:
        - name: logs
          expression: 'resource.GetLogs("default", "my-pod", "app", 100)'
```

**Use Cases:**
- LLM-powered log diagnosis in agent steps
- Error detection and alerting
- Aggregating logs from multiple pods via `forEach`

---

### `resourceLogs(apiVersion, kind, namespace, name, container)` *(deprecated)*

> **Deprecated.** Use [`resource.GetLogs()`](#resourcegetlogsnamespace-podname-containername-taillines) instead.
> `resourceLogs` now delegates to the same implementation with `tailLines=100`.
> The `apiVersion` and `kind` parameters are accepted for backward compatibility but ignored.

Retrieve the last 100 lines of a pod container's log.

**Parameters:**
- `apiVersion` (string): Ignored (kept for backward compatibility; was `"v1"`)
- `kind` (string): Ignored (kept for backward compatibility; was `"Pod"`)
- `namespace` (string): Namespace (defaults to WorkflowRun namespace when `""`)
- `name` (string): Pod name
- `container` (string): Container name

**Returns:** Logs as string

**Example:**
```yaml
expressions:
  - name: podLogs
    expression: 'resourceLogs("v1", "Pod", "default", "my-pod", "main-container")'
```

---

### `resourceEvents(apiVersion, kind, namespace, name)`

Get Kubernetes events related to a specific resource.

**Parameters:**
- `apiVersion` (string): API version (e.g., `"v1"`)
- `kind` (string): Resource kind (e.g., `"Pod"`)
- `namespace` (string): Namespace
- `name` (string): Resource name

**Returns:** List of Event objects filtered by `involvedObject`

**Example:**
```yaml
expressions:
  - name: podEvents
    expression: 'resourceEvents("v1", "Pod", "default", "my-pod")'
  - name: errorEvents
    expression: 'expressions.podEvents.filter(e, e.type == "Warning")'
```

**Use Cases:**
- Event monitoring
- Error tracking
- Resource lifecycle analysis
- Troubleshooting

---

### `format(formatString, ...args)`

Format strings with CEL expression interpolation (similar to `printf`). CEL does not support true variadic functions, so `format` is implemented with overloads for **2–20 arguments** (format string plus up to 19 values). For more placeholders or when the number of arguments is dynamic, use `formatList`.

**Parameters:**
- `formatString` (string): Format string with placeholders
- `...args`: Up to 19 arguments to interpolate

**Returns:** Formatted string

**Example:**
```yaml
expressions:
  - name: message
    expression: 'format("Pod %s has %d restarts", inputs.podName, variables.restartCount)'
```

**Use Cases:**
- String formatting
- Message generation
- Report creation
- Logging

---

### `formatList(formatString, list)`

Same as `format`, but the substitution values are passed as a single list. Use this when you have more than 19 values or when building the list dynamically (e.g. from a `.map()` or variable).

**Parameters:**
- `formatString` (string): Format string with placeholders
- `list` (list): List of values to interpolate (order must match placeholders)

**Returns:** Formatted string

**Example:**
```yaml
# Many args or dynamic list
expression: 'formatList("%s: %d pods, %d deployments", [ns, size(pods), size(deployments)])'
```

---

## Kyverno CEL Libraries

OttoFlow includes the complete suite of **Kyverno CEL libraries** (Resource, HTTP, User, Image, ImageData, GlobalContext, Hash, Math, Random, Transform, JSON, YAML, Time, X509), implemented via the [Kyverno SDK CEL](https://github.com/kyverno/sdk/tree/main/cel) package. The same function names and behavior apply (e.g. `resource.Get()`, `resource.List()`, `json.unmarshal()`, `yaml.parse()`, `image.GetMetadata()`).

**Note on Resource Library**: OttoFlow provides the ContextInterface implementation for the Resource library using controller-runtime's `client.Client`, so `resource.Get()` and `resource.List()` work in the workflow controller context.

For complete documentation and examples, see: [Kyverno CEL Libraries](https://kyverno.io/docs/policy-types/cel-libraries/)

### HTTP Library

The `http` global is bound into **every** CEL evaluation — there is no dedicated `http` step type. Outbound calls are made from inside a step's `expressions:` block, and the result is used like any other expression output (e.g. read back via `expressions.<name>` or `variables.<name>`).

#### Signatures

| Function | Description |
|----------|-------------|
| `http.Get(url)` | GET request, no custom headers |
| `http.Get(url, headers)` | GET request with a `map[string]string` of headers |
| `http.Post(url, body, headers)` | POST request; `body` is any CEL value, JSON-encoded before sending |
| `http.Client(caBundle)` | Returns a new `http` context that trusts an additional CA bundle (PEM string) for subsequent calls |

camelCase aliases (`http.get`, `http.post`, `http.client`) are also registered and behave identically to the PascalCase names above.

**Example:**
```yaml
expressions:
  - name: statusResp
    expression: 'http.Get("https://api.example.com/status")'
  - name: postResp
    expression: >-
      http.Post(
        "https://api.example.com/notify",
        {"message": "hello"},
        {"Content-Type": "application/json"}
      )
```

#### Response shape

`http.Get` / `http.Post` return a map, never a scalar:

- If the response body decodes as a JSON **object**, `statusCode` is merged directly into it — e.g. `{"foo": "bar", "statusCode": 200}`.
- Otherwise (non-JSON body, JSON array, empty body, etc.) the response is `{"body": <decoded-value-or-null>, "statusCode": <int>}`.

**Non-2xx responses are not CEL errors.** A 404 or 500 is returned as normal data with `statusCode` set accordingly — the expression evaluates successfully either way. Only a connection-level failure (unresolvable host, TLS handshake failure, timeout) produces a CEL error, which fails the step. **Callers must check `statusCode` themselves** to detect application-level failures; a successful expression evaluation does not mean the request succeeded.

OttoFlow additionally normalizes non-JSON **2xx** response bodies into `{"ok": true, "body": "<original body>"}` so that responses like Slack's bare `ok` text don't break JSON decoding. This wrapping only happens for 2xx responses — a non-2xx response passes through with its original body untouched and has no `ok` key. **Never branch on the presence of `ok`; always key on `statusCode`.**

Every `http` call has a hard **30-second timeout**. A stalled endpoint fails the step rather than blocking workflow execution indefinitely.

#### Example: Slack webhook

```yaml
- name: notifySlack
  message: "Send summary to Slack"
  expressions:
    - name: slackResult
      expression: >-
        http.Post(inputs.slackWebhookUrl, {"text": "Workflow run complete"}, {"Content-Type": "application/json"})
  outputs:
    - name: slackNotified
      expression: 'expressions.slackResult.statusCode >= 200 && expressions.slackResult.statusCode < 300'
```

Slack returns the plain-text body `ok` on success, so the response is `{"ok": true, "body": "ok", "statusCode": 200}`. Note the webhook URL arrives as a plain workflow input and is stored in the WorkflowRun spec in cleartext; cron-triggered workflows can inject it from a Secret via `cron.inputValuesFrom.secretRef` ([CronInputFromSecret](api/api-docs.md#croninputfromsecret)), which has no equivalent for other trigger types.

#### Example: Microsoft Teams via Power Automate

Teams' legacy Incoming Webhook connector (`webhook.office.com/webhookb2/...`) was retired in May 2026. Its replacement is a Power Automate flow with a **"When a Teams webhook request is received"** trigger, which still accepts a plain JSON POST carrying an Adaptive Card envelope:

```yaml
- name: notifyTeams
  message: "Send summary to Teams via Power Automate"
  expressions:
    - name: teamsResult
      expression: >-
        http.Post(inputs.teamsWebhookUrl,
        json.unmarshal('{"type": "message", "attachments":
        [{"contentType": "application/vnd.microsoft.card.adaptive",
        "contentUrl": null, "content": {"type": "AdaptiveCard",
        "version": "1.2", "$schema":
        "http://adaptivecards.io/schemas/adaptive-card.json", "body":
        [{"type": "TextBlock", "text": "Workflow run complete", "wrap":
        true}]}}]}'),
        {"Content-Type": "application/json"})
  outputs:
    - name: teamsNotified
      expression: 'expressions.teamsResult.statusCode >= 200 && expressions.teamsResult.statusCode < 300'
```

CEL map literals require a single, consistent value type per map — the Adaptive Card envelope mixes strings, a nested object, and a list, so it can't be written as a plain CEL map/list literal. `json.unmarshal(...)` (from the `kyverno.json` CEL library) parses a JSON string into a `dyn` value instead, sidestepping the homogeneous-type restriction. Keep every line of the JSON string at **exactly the block's base indentation**. A folded scalar (`>-`) joins lines at that base indent with spaces, but any line indented further is YAML's "more-indented" case and keeps its newline literally (YAML 1.2 §8.1.3). A raw newline inside a CEL string literal is a compile error, so an over-indented continuation line breaks the expression even though the YAML itself stays valid.

If the Power Automate flow's trigger is configured with "Anyone" auth mode, do **not** set an `Authorization` header — sending one fails the POST.

---

## Kubernetes CEL Libraries

OttoFlow includes the complete suite of **Kubernetes CEL libraries**, including List, Regex, URL, IP Address, CIDR, Format, Quantity, and Semver libraries.

For complete documentation and examples, see: [Kubernetes CEL Libraries](https://kubernetes.io/docs/reference/using-api/cel/)

---

## Standard CEL Functions

OttoFlow also supports all standard CEL functions:

### Collections
- `map()`, `filter()`, `size()`, `has()`, `exists()`, `exists_one()`, `all()`
- `.sum()`, `.min()`, `.max()`, `.isSorted()` (list methods from the Kubernetes lists library)

### Type Conversion
- `string()`, `int()`, `bool()`, `double()`, `bytes()`

### String Operations
- `contains()`, `replace()`, `split()`, `join()`, `lowerAscii()`, `upperAscii()`, `trim()`, `charAt()`, `indexOf()`, `lastIndexOf()`, `substring()`

### Math
- Standard math operations and comparisons

---

## Common Pitfalls

These are the mistakes most frequently seen in real workflows. All of them were verified against the engine.

- **`now()` does not exist.** Use `time.now()` (Kyverno Time library).
- **`.sum()` is a list method, not a function.** Write `list.sum()`, not `sum(list)`. There is no `len()` — use `size()`, which works both as `size(list)` and as `list.size()`.
- **`resource` is a macro namespace, not your query result.** In a `resourceQuery` step the fetched object is bound to `object` and list results to `items`. `resource.status.phase` can never compile — use `object.status.phase`.
- **`resource.Get()` returns `null` for a missing resource** — not an error and not an empty map. Guard with `!= null` before field access.
- **`resource.List(...)` returns an object that wraps the list.** Iterate `dyn(resource.List(...)).items`, not the return value itself — `size()` of the wrapper counts its keys (e.g. `3`), which is a silently wrong answer.
- **`resource.List()` fails the expression above 10000 items per call.** Narrow the list by namespace or labels, or use a `resourceQuery` step with `limit` (which returns only the first `limit` items).
- **`+` does not work on maps.** To add keys, build a new map literal; for mutate steps return a partial object to deep-merge.
- **Heterogeneous map literals are all-or-nothing on `dyn()`.** The first entry fixes the map's value type; wrap values in `dyn()` if they differ (e.g. `{"a": dyn(1), "b": dyn("x")}`).
- **No int/double division overload.** Cast both sides: `double(a) / double(b)`.
- **Workflow inputs are strings.** A JSON list arrives as text — `json.unmarshal(inputs.myList)` before iterating.
- **`ip(x).isCanonical()` does not exist**; use the static form `ip.isCanonical(x)`. **`quantity(...).sign()` and `.multiply()` do not exist**; use `compareTo()` and `asApproximateFloat()`.
- **Selector values in `resourceQuery` are CEL expressions, always evaluated.** A literal selector must be quoted inside the expression: `fieldSelector: '"status.phase=Running"'`, `labelSelector: {app: '"nginx"'}`.

---

## Variable Access

In CEL expressions, you can access:

- **`inputs.<name>`** - Workflow input parameters (always strings; use `json.unmarshal()` for structured values)
- **`variables.<name>`** - Workflow-level variables and outputs from completed steps (flat namespace, no step prefix)
- **`steps.<stepName>.<key>`** - Step-scoped results (e.g. forEach `steps.<name>.results`)
- **`stepStatus.<stepName>.{phase,succeeded,failed,error}`** - Status of a step that has finished (Succeeded, Failed or Skipped); a step that has not finished is absent (`no such key`). Add the step to `dependsOn`. A forEach item sees the statuses as they were when the forEach step started.
- **`expressions.<name>`** - Current step's expression results
- **`outputs.<name>`** - Earlier workflow-level outputs (only while workflow outputs and metric labels are evaluated)
- **`object`** - Fetched resource in resourceQuery steps; target resource in mutate steps
- **`items`** - List query results in resourceQuery steps
- **`item`** - Current item in forEach loops
- **`agentResponse` / `agentOutputs`** - LLM response text / extracted outputs (in agent step outputs)
- **`toolResult`** - MCP tool call result (in mcpToolCall step outputs)
- **`a2aResult`** - External A2A agent call result (in externalAgentRef step outputs)
- **`result`** - Prometheus query result (`result.type`, `result.samples`, `result.value`)
- **`reportResult`** - OpenReports.io report result (in openReport step outputs)

---

## Complete Documentation

For detailed documentation on Kyverno and Kubernetes CEL libraries:

- **[Kyverno SDK CEL](https://github.com/kyverno/sdk/tree/main/cel)** - CEL library implementation used by OttoFlow
- **[Kyverno CEL Libraries](https://kyverno.io/docs/policy-types/cel-libraries/)** - Complete Kyverno library reference (behavior and examples)
- **[Kubernetes CEL Libraries](https://kubernetes.io/docs/reference/using-api/cel/)** - Complete Kubernetes library reference
- **[CEL Specification](https://github.com/google/cel-spec)** - Common Expression Language specification
