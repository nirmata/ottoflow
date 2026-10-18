# OttoFlow Workflow Examples

This directory contains example Workflow and WorkflowRun resources for testing OttoFlow. Each file consolidates the Workflow definition with one or more example WorkflowRun(s) as `---`-separated YAML documents.

**Execution model:** In-cluster `WorkflowRun` execution always happens through a runner `Job`.

**CLI:** `ottoflow run` submits a `WorkflowRun` to the cluster; the controller runs the workflow in a Job.

**Note:** Agent and MCP examples require the Agent and MCPServer CRDs to be installed first (see Prerequisites section).

## Layout

Samples are organized into three directories by purpose:

| Directory | What lives here |
|---|---|
| [`production/`](production/) | Complete, real-world automations you can deploy as-is: cost reporting and optimization, resource hygiene and cleanup, policy recommendations, pod triage, workload troubleshooting — plus the support resources (Agent, MCPServer, ConfigMap) they require. |
| [`features/`](features/) | One file per engine feature or integration: step types, triggers, retry policies, LLM providers, MCP auth variants, multi-cluster runs, CEL libraries. Named after the feature they demonstrate. |
| [`testing/`](testing/) | Trivial scratch workflows used for engine smoke-testing (`basic-test`, `test-*`). |

Reusable `StepTemplate` resources live in the sibling [`../steptemplates/`](../steptemplates/) directory. Workflows that use `stepTemplateRef` (e.g. `stale-image-check`, `cost-optimization`, `steptemplate-example`, `top-pods-cpu-analysis`) need those templates applied too — in local mode, pass the parent directory: `--workflow-dir samples`.

### `production/` (17)

`cloud-cost-daily` (+ `cloud-cost-agent`, `aws-pricing-mcp`, `cloud-cost-optimization-state`), `cost-analyzer`, `cost-optimization`, `cost-optimization-eks`, `cost-optimization-eks-optimized`, `cost-optimization-eks-prometheus`, `resource-cleanup`, `resource-hygiene`, `policy-recommendations`, `policy-recommendations-assessment-only`, `pod-triage`, `workload-troubleshooter`, `cluster-overview`, `stale-image-check`

### `features/` (67)

- **Basics:** `hello-world`, `simple-greeting`, `expressions-workflow`, `step-dependencies`, `complex-workflow`, `conditional-execution`, `workflow-variables`, `workflow-reference`, `foreach-example`, `json-output-example`, `steptemplate-example`
- **Agents / LLM providers:** `agent-basic`, `agent-step`, `agent-config`, `agent-openai`, `agent-azure-openai`, `agent-gemini`, `agent-local-llamacpp`, `agent-custom-endpoint`, `agent-mcp-combined`, `simple-agent-question`, `pod-diagnostics-agent`, `pod-diagnostics-with-mcp`, `kagent-integration`
- **MCP:** `mcpserver-config`, `mcpserver-auth-variants`, `mcp-tool-call`, `mcp-with-auth`, `mcp-oauth2-auth`
- **Triggers:** `cron-trigger`, `cron-trigger-timezone`, `cron-trigger-replace`, `event-trigger`, `complex-triggers`, `argocd-event-trigger`, `fluxcd-event-trigger`, `github-actions-webhook`
- **Retries:** `retry-exponential-backoff`, `retry-linear-backoff`, `retry-max-attempts`, `retry-with-conditions`, `complex-workflow-with-retry`
- **Resource queries & metrics:** `resource-query-example`, `resource-list`, `crd-resource-query`, `advanced-field-paths`, `resource-metrics-example`, `gpu-metrics-example`, `pod-health-metrics`, `prometheus-query-example`, `prometheus-query-simple`, `namespace-resource-summary`, `pod-health-check`, `pod-restart-analysis`, `pod-resource-analysis`, `top-pods-cpu-analysis`
- **CEL:** `kubernetes-cel-libraries`, `kyverno-cel-libraries`, `practical-cel-examples`
- **Other step types & patterns:** `mutate-step-example`, `open-report-example`, `wait-for-callback`, `argocd-cluster-discovery`, `multi-cluster-workflowrun`, `multi-cluster-workflowrun-filepath`, `multi-cluster-workflowrun-csi`, `job-customized-workflowrun`

### `testing/` (5)

`basic-test`, `test-time`, `test-time-namespace`, `test-native-types`, `test-image-metadata`

## Examples

### 1. Simple Greeting (`simple-greeting.yaml`)
Basic workflow demonstrating:
- Input parameters with defaults
- Simple output expression
- String concatenation

**Usage:**
```bash
kubectl apply -f simple-greeting.yaml
# Create a run: ottoflow run simple-greeting --input name=World
```

### 2. Expressions Workflow (`expressions-workflow.yaml`)
Demonstrates:
- Sequential expression evaluation
- Resource access with `resource.Get()`
- Expression references (`expressions.name`)
- Type checking with `has()`

**Usage:**
```bash
kubectl apply -f expressions-workflow.yaml
```

### 3. Step Dependencies (`step-dependencies.yaml`)
Demonstrates:
- Step output references
- Parallel step execution (step-b and step-c both depend on step-a)
- Multiple dependencies (step-d depends on both step-b and step-c)

**Usage:**
```bash
kubectl apply -f step-dependencies.yaml
```

### 4. Resource List (`resource-list.yaml`)
Demonstrates:
- `resource.List()` function
- List operations and filtering
- Map operations on resource lists

**Usage:**
```bash
kubectl apply -f resource-list.yaml
```

### 5. Complex Workflow (`complex-workflow.yaml`)
Demonstrates:
- Multiple resource operations
- Complex step dependencies
- Aggregation of results
- String formatting

**Usage:**
```bash
kubectl apply -f complex-workflow.yaml
```

### 6. Conditional Execution (`conditional-execution.yaml`)
Demonstrates:
- `matchConditions` for conditional step execution
- Environment-based deployment logic
- Step skipping based on conditions
- Contains two example WorkflowRuns (dev and prod)

**Usage:**
```bash
kubectl apply -f conditional-execution.yaml
# Apply specific run: kubectl apply -f - <<EOF
# ---
# (extract desired WorkflowRun doc from file)
# EOF
ottoflow run conditional-execution --input environment=dev --input deploy-feature=false
ottoflow run conditional-execution --input environment=prod --input deploy-feature=true
```

### 7. Retry with Exponential Backoff (`retry-exponential-backoff.yaml`)
Demonstrates:
- Retry configuration with exponential backoff
- Resource existence checking with retries
- Backoff strategy configuration

**Usage:**
```bash
kubectl apply -f retry-exponential-backoff.yaml
```

### 8. Retry with Linear Backoff (`retry-linear-backoff.yaml`)
Demonstrates:
- Retry with linear (fixed interval) backoff
- Transient error handling
- Custom backoff intervals

**Usage:**
```bash
kubectl apply -f retry-linear-backoff.yaml
```

### 9. Retry with Conditions (`retry-with-conditions.yaml`)
Demonstrates:
- Retry only on specific error types
- Error message pattern matching
- Selective retry logic

**Usage:**
```bash
kubectl apply -f retry-with-conditions.yaml
```

### 10. Retry Max Attempts (`retry-max-attempts.yaml`)
Demonstrates:
- Maximum retry attempts
- Failure policy (Continue)
- Workflow continuation after step failure

**Usage:**
```bash
kubectl apply -f retry-max-attempts.yaml
```

### 11. Complex Workflow with Retry (`complex-workflow-with-retry.yaml`)
Demonstrates:
- Combining retry logic with conditional execution
- Multiple retry strategies in one workflow
- Complex dependency chains with retries

**Usage:**
```bash
kubectl apply -f complex-workflow-with-retry.yaml
```

### 12. Workflow References (Sub-workflows) (`workflow-reference.yaml`)
Demonstrates:
- Calling child workflows as sub-workflows
- Passing inputs from parent to child workflows using CEL expressions
- Accessing child workflow outputs in parent workflow
- Cross-workflow data flow

**Usage:**
```bash
kubectl apply -f workflow-reference.yaml
```

**Note:** This creates both a child workflow and a parent workflow that references it.

### 13. Cron Triggers

**`cron-trigger.yaml`** — Simple heartbeat every 5 minutes with Forbid concurrency.

**`cron-trigger-timezone.yaml`** — Daily cluster summary at 9 AM Eastern (shows IANA timezone support).

**`cron-trigger-replace.yaml`** — Stale pod scan every 6 hours with Replace concurrency (cancels any active run on the next fire).

Demonstrates:
- Automatic workflow execution on a schedule (in-process cron scheduler)
- Cron expression syntax (standard 5-field)
- Concurrency policies: Forbid, Allow, Replace
- Timezone configuration via IANA names (e.g., `America/New_York`)

**Usage:**
```bash
kubectl apply -f cron-trigger.yaml
# Workflow will automatically create WorkflowRuns based on the cron schedule
# Check WorkflowRuns: kubectl get workflowruns -l ottoflow.nirmata.io/trigger=cron
```

**Note:** No WorkflowRun needs to be created manually — the cron trigger creates them automatically.

### 14. Event Trigger (`event-trigger.yaml`)
Demonstrates:
- Automatic workflow execution on Kubernetes events
- Watching specific resource types (Pods, ConfigMaps, etc.)
- Filtering by operations (CREATE, UPDATE, DELETE)
- Label selector filtering

**Usage:**
```bash
kubectl apply -f event-trigger.yaml
# Create a pod with matching labels to trigger the workflow
kubectl run test-pod --image=busybox:latest --labels=app=monitored -- sleep 3600
# Check WorkflowRuns: kubectl get workflowruns -l ottoflow.nirmata.io/trigger=event
```

### 15. Complex Triggers (`complex-triggers.yaml`)
Demonstrates:
- Multiple triggers on the same workflow (cron + event)
- Accessing trigger information in workflow steps
- Different trigger types in one workflow

**Usage:**
```bash
kubectl apply -f complex-triggers.yaml
# Workflow can be triggered by either:
# 1. Cron schedule (every hour)
# 2. ConfigMap events (CREATE/UPDATE with label managed-by=ottoflow)
```

### 16. Agent Step (`agent-step.yaml`)
Demonstrates:
- Using AI agents in workflow steps
- Agent CRD configuration
- Prompt templates with CEL expression support
- Output extraction from agent responses
- Combining agent steps with expression-based steps

**Prerequisites:**
```bash
# Apply Agent CRD configuration first
kubectl apply -f agent-config.yaml
```

**Usage:**
```bash
kubectl apply -f agent-step.yaml
```

**Note:** Requires Agent CRD (`agent-config.yaml`) to be created first. The workflow uses an agent to analyze Kubernetes deployments.

### 17. MCP Tool Call Step (`mcp-tool-call.yaml`)
Demonstrates:
- Direct MCP tool invocation (no LLM mediation)
- MCPServer CRD configuration
- CEL-resolved tool arguments
- Tool results in workflow context
- Combining MCP tool calls with expressions

**Prerequisites:**
```bash
# Apply MCPServer CRD configuration first
kubectl apply -f mcpserver-config.yaml
```

**Usage:**
```bash
kubectl apply -f mcp-tool-call.yaml
```

**Note:** Requires MCPServer CRD (`mcpserver-config.yaml`) to be created first. The workflow calls MCP tools directly to query Kubernetes resources.

### 18. Agent and MCP Combined (`agent-mcp-combined.yaml`)
Demonstrates:
- Combining agent steps with MCP tool calls
- Using agent outputs as context for MCP tools
- Using MCP tool results as context for agents
- Prompt overrides at step level
- Complex workflows with AI agents and tools

**Prerequisites:**
```bash
# Apply both Agent and MCPServer CRDs
kubectl apply -f agent-config.yaml
kubectl apply -f mcpserver-config.yaml
```

**Usage:**
```bash
kubectl apply -f agent-mcp-combined.yaml
```

**Note:** This workflow demonstrates a complete AI-powered workflow that:
1. Asks an agent a question
2. Gets deployment info using MCP tools
3. Uses the agent to analyze the deployment with context from both previous steps

### 19. Simple Agent Question (`simple-agent-question.yaml`)
Demonstrates:
- Basic agent step usage
- Simple Q&A workflow
- Agent response extraction
- Workflow-level outputs

**Prerequisites:**
```bash
# Apply Agent CRD configuration
kubectl apply -f agent-config.yaml
```

**Usage:**
```bash
kubectl apply -f simple-agent-question.yaml
```

**Note:** Simplest example of using an agent step. Good starting point for learning agent workflows.

### 20. Basic Agent Step (`agent-basic.yaml`)
Demonstrates:
- A minimal Agent plus the Workflow that calls it, in one file
- `modelProvider: local`, so it runs against a llama.cpp-compatible server
  (llama.cpp, ollama, vLLM, LM Studio) without a cloud API key

For per-provider configuration see `agent-openai.yaml`, `agent-azure-openai.yaml`,
`agent-gemini.yaml`, `agent-local-llamacpp.yaml`, and `agent-custom-endpoint.yaml`
(which documents exactly which `spec.config` fields each provider honours, and that
`spec.config.endpoint` needs its origin on the operator's `AGENT_LLM_ENDPOINT_ALLOWLIST`).

**Prerequisites:**
```bash
# agent-basic.yaml contains Workflow, WorkflowRun, and Agent in one file
kubectl apply -f agent-basic.yaml
```

**Usage:**
```bash
```

**Note:** Shows when `spec.config.endpoint` is honored (only for an origin the operator has allowlisted via Helm `agentExecutor.llmEndpointAllowlist`; otherwise the run fails), and that `spec.config.skipVerifySSL: "true"` is rejected for every provider.

### 21. MCP with Authentication (`mcp-with-auth.yaml`)
Demonstrates:
- MCP server with bearer token authentication
- Kubernetes Secret integration for credentials
- HTTP transport with custom headers
- Environment variable configuration

**Prerequisites:**
```bash
# Create secret and MCPServer (includes Secret example)
kubectl apply -f mcp-with-auth.yaml
```

**Usage:**
```bash
```

**Note:** Shows how to securely configure MCP servers with authentication using Kubernetes Secrets.

### 22. Pod Diagnostics with Agent (`pod-diagnostics-agent.yaml`)
Demonstrates:
- Comprehensive pod information collection using CEL expressions
- Pod status, events, and resource analysis
- AI-powered pod diagnostics using LLM agent
- Structured JSON output extraction
- Complex CEL expressions for pod analysis

**Prerequisites:**
```bash
# Apply Agent CRD configuration
kubectl apply -f agent-config.yaml
```

**Usage:**
```bash
# Ensure you have a pod to diagnose
kubectl create deployment nginx --image=nginx:latest

# Run diagnostics
kubectl apply -f pod-diagnostics-agent.yaml
```

**Note:** This workflow collects comprehensive pod information including:
- Pod phase, readiness, restart count
- Container images and resource requests/limits
- Security context settings
- Recent events and errors
- Then uses an AI agent to analyze and provide recommendations

### 23. Pod Diagnostics with MCP (`pod-diagnostics-with-mcp.yaml`)
Demonstrates:
- Combining CEL expressions with MCP tool calls
- Getting pod logs using MCP tools
- Getting pod description using MCP tools
- Agent analysis with MCP-provided data
- Hybrid workflow using both CEL and MCP

**Prerequisites:**
```bash
# Apply Agent and MCPServer CRDs
kubectl apply -f agent-config.yaml
kubectl apply -f mcpserver-config.yaml
```

**Usage:**
```bash
# Ensure you have a pod to diagnose
kubectl create deployment nginx --image=nginx:latest

# Run diagnostics with MCP
kubectl apply -f pod-diagnostics-with-mcp.yaml
```

**Note:** This workflow demonstrates a complete diagnostic workflow that:
1. Gets basic pod info using CEL
2. Retrieves pod logs using MCP tools
3. Gets detailed pod description using MCP tools
4. Analyzes everything with an AI agent

### 24. Pod Diagnostics on Restart Event
Demonstrates:
- **Event-triggered workflow** that automatically runs on pod UPDATE events
- Automatic detection of pod restarts
- Comprehensive diagnostics triggered by restart events
- Input mapping from event object to workflow inputs
- Filtering restart-related events
- AI-powered analysis focused on restart root causes

**Prerequisites:**
```bash
# Apply Agent CRD configuration
kubectl apply -f agent-config.yaml
```

**Usage:**
```bash
# Apply the workflow (it will watch for pod updates automatically)
# See event-trigger.yaml and pod-restart-analysis.yaml for related examples

# Create a pod that will restart (for testing)
kubectl run test-pod --image=nginx:latest --restart=Never
# Or trigger a restart manually
kubectl delete pod test-pod && kubectl run test-pod --image=nginx:latest --restart=Never

# The workflow will automatically create a WorkflowRun when a pod is updated
# Check WorkflowRuns to see the diagnostics
kubectl get workflowruns -w
```

**Key Features:**
- **Automatic Triggering**: No need to manually create WorkflowRuns - they're created automatically when pods are updated
- **Restart Detection**: Checks for restart counts and recently terminated containers
- **Event Filtering**: Focuses on restart-related events (Started, Created, Killing, BackOff, CrashLoop)
- **Root Cause Analysis**: Agent prompt specifically focuses on identifying restart root causes
- **Namespace Flexibility**: Can watch all namespaces (empty namespace) or specific namespaces
- **Label Filtering**: Optional label selector to only monitor specific pods (commented out in example)

**Note:** This workflow will trigger on ANY pod UPDATE event. To filter for only restarts, you can:
1. Use field selectors (commented in example)
2. Add logic in the first step to skip diagnostics if no restart detected
3. Use label selectors to only monitor specific pods

## Configuration Files

### Agent Configuration (`agent-config.yaml`)
Contains example Agent CRDs:
- `analysis-agent`: Analyzes Kubernetes deployments with structured JSON output
- `simple-agent`: Simple Q&A agent with text output

**Note:** Agent system prompts in the Agent CRD are static. To add dynamic content:
- Use `additionalPrompts` at the step level with CEL expressions
- Each prompt in the array is evaluated and appended to the agent's system prompt
- Example: `additionalPrompts: ['string("Context: ") + inputs.context']`

### MCPServer Configuration (`mcpserver-config.yaml`)
Contains example MCPServer CRDs:
- `kubernetes-mcp`: stdio-based MCP server for Kubernetes operations
- `http-mcp-server`: HTTP-based MCP server with authentication

**Note:** MCP tool calls use the kubectl-ai MCP client with stdio, HTTP, and SSE transport support.

## Testing All Examples

To test all workflows:

```bash
# First, install CRDs
kubectl apply -f config/crd/bases/

# Apply configuration files (Agent and MCPServer CRDs)
kubectl apply -f samples/workflows/features/agent-config.yaml
kubectl apply -f samples/workflows/features/mcpserver-config.yaml

# Apply StepTemplates (required by workflows using stepTemplateRef)
kubectl apply -f samples/steptemplates/

# Apply all sample workflows (recursive: production/, features/, testing/)
kubectl apply -R -f samples/workflows/

# Watch workflow runs
kubectl get workflowruns -w

# Check specific workflow run status
kubectl get workflowrun <workflowrun-name> -o yaml

# Check workflow outputs
kubectl get workflowrun <workflowrun-name> -o jsonpath='{.status.outputs}' | jq

# Check agent and MCP server resources
kubectl get agents
kubectl get mcpservers
```

### 28. Resource Query Example (`resource-query-example.yaml`)
Demonstrates:
- **Resource Query DSL**: Simplified syntax for Kubernetes resource queries
- Single resource queries using `resourceQuery` with `name` field
- List queries with label and field selectors
- Structured output extraction

**Usage:**
```bash
kubectl apply -f resource-query-example.yaml
```

---

### 29. Resource Metrics Example (`resource-metrics-example.yaml`)
Demonstrates:
- **resourceMetrics() CEL macro**: Fetches resource usage metrics from Kubernetes metrics API
- CPU and memory metrics for pods
- Timestamp, window, total CPU/memory, and container-level metrics
- Gracefully handles when metrics server is not available

**Key Features:**
- Different from `resource.Get()` - fetches from metrics API, not resource API
- Returns structured map with aggregated totals and per-container metrics
- Currently supports Pod resources (v1/Pod)

**Usage:**
```bash
kubectl apply -f resource-metrics-example.yaml
```

**Note:** Requires Kubernetes metrics server to be installed in the cluster. If not available, the function will return an error.

**Example Output:**
```json
{
  "timestamp": "2026-02-03T10:00:00Z",
  "window": "30s",
  "totalCPU": "100m",
  "totalMemory": "512Mi",
  "containers": [
    {"name": "container1", "cpu": "50m", "memory": "256Mi"},
    {"name": "container2", "cpu": "50m", "memory": "256Mi"}
  ]
}
```

---

### 30. GPU Metrics Example (`gpu-metrics-example.yaml`)
Demonstrates:
- **Extended resourceMetrics()** with Custom Metrics API for GPU metrics
- **prometheusMetrics()** function for Prometheus queries
- Multiple approaches to accessing GPU metrics

**Key Features:**
- Standard CPU/memory metrics (empty `metricName`)
- Custom metrics via Custom Metrics API (e.g., `nvidia_com_gpu_utilization`)
- Prometheus queries for GPU metrics (e.g., `nvidia_gpu_utilization`)

**Usage:**
```bash
kubectl apply -f gpu-metrics-example.yaml
```

**Example: Standard Metrics (CPU/Memory)**
```yaml
expressions:
  - name: metrics
    expression: 'resourceMetrics("v1", "Pod", "default", "my-pod", "")'
```

**Example: Custom Metrics (GPU)**
```yaml
expressions:
  - name: gpuUtilization
    expression: 'resourceMetrics("v1", "Pod", "default", "gpu-pod", "nvidia_com_gpu_utilization")'
```

**Example: Prometheus Metrics**
```yaml
expressions:
  - name: gpuMetrics
    expression: 'prometheusMetrics("nvidia_gpu_utilization{pod=\"gpu-pod\"}", "5m")'
```

**Note:** 
- Custom Metrics API requires a custom metrics adapter (e.g., Prometheus Adapter, KEDA Metrics Adapter)
- Prometheus requires Prometheus to be deployed and accessible
- Both gracefully handle when not available (return errors)

---

### 31. Prometheus Query Step Examples (`prometheus-query-example.yaml`, `prometheus-query-simple.yaml`)
Demonstrates the **prometheusQuery** step type (first-class PromQL queries with template variables).

**prometheus-query-example:** Combines `resourceQuery` (list pods) with `prometheusQuery` (CPU usage). Step 1 runs on any cluster; step 2 requires a Prometheus URL.

**prometheus-query-simple:** Single-step workflow that queries the `up` metric.

**Usage (CLI with Prometheus):**
```bash
# With Prometheus URL (e.g. port-forward or in-cluster)
./bin/ottoflow run prometheus-query-example --input namespace=default
./bin/ottoflow run prometheus-query-simple
```

**Usage (run on kind):**
```bash
# From repo root; prometheusQuery steps will fail without PROMETHEUS_URL
./scripts/run-samples-kind.sh
PROMETHEUS_URL=http://localhost:9090 ./scripts/run-samples-kind.sh   # if Prometheus is port-forwarded
```

---

### 31. Pod Health with Custom Metrics (`pod-health-metrics.yaml`)
Demonstrates:
- **Custom Prometheus metrics** via `outputs[].metric`
- Resource Query DSL for counting pods
- Publishing workflow outputs to Prometheus (gauge type)

**Key Features:**
- Workflow-level output with `metric` field publishes to Prometheus
- Metric value is the output's evaluated expression
- Labels can use CEL expressions (inputs, variables, outputs)
- Metrics are prefixed with `ottoflow_workflow_`

**Usage:**
```bash
kubectl apply -f pod-health-metrics.yaml
# Create a run and check /metrics for ottoflow_workflow_total_pods
```

---

### 32. Top Pods CPU Analysis (`top-pods-cpu-analysis.yaml`)
Demonstrates:
- **Resource Query DSL** for listing pods
- **resourceMetrics()** for getting CPU metrics for multiple pods
- **CEL expressions** for sorting, filtering, and aggregating data
- **Top N analysis** to identify pods with highest CPU usage
- **Recommendations** for CPU scaling

**Key Features:**
- Lists all pods in a namespace using Resource Query DSL
- Gets CPU metrics for each pod using `resourceMetrics()`
- Sorts pods by CPU usage (descending)
- Identifies top N pods by CPU consumption
- Calculates summary statistics (total, average CPU usage)
- Generates recommendations for pods that may need more CPU
- Uses CEL `map()`, `filter()`, `size()` functions and the `.sum()` list method
- Note: CEL doesn't have built-in `sort()`, so uses a filter-based approach to find top N pods

**Usage:**
```bash
# Uses top-pods-by-cpu StepTemplate from samples/steptemplates/
kubectl apply -f ../steptemplates/
kubectl apply -f top-pods-cpu-analysis.yaml
```

**Example Workflow:**
1. Lists all pods in namespace
2. Gets CPU metrics for each pod
3. Identifies top 10 pods by CPU usage (using filter-based approach since CEL doesn't have sort)
4. Identifies pods above average CPU usage
5. Generates scaling recommendations

**Example Output:**
```json
{
  "topPodsSummary": "Found 25 pods using high CPU. Top 10 pods consume 5000m total CPU (avg: 500m). 5 pods may need CPU scaling.",
  "topPodsList": [
    {
      "name": "high-cpu-pod-1",
      "cpuUsage": "1500m",
      "metrics": { ... }
    },
    ...
  ],
  "scalingRecommendations": [
    {
      "podName": "high-cpu-pod-1",
      "currentCPU": "1500m",
      "recommendation": "Consider increasing CPU requests/limits for pod high-cpu-pod-1 (currently using 1500m)",
      "priority": "high"
    }
  ]
}
```

**Note:** Requires Kubernetes metrics server to be installed. The workflow gracefully handles pods without metrics available.

---

### 32. CRD Resource Query Example (`crd-resource-query.yaml`)
Demonstrates:
- **Custom Resource Definition (CRD) support** in Resource Query DSL
- Querying Workflow and WorkflowRun CRDs
- **CEL expressions** with nested access and array indexing
- Filtering CRD resources

**Key Features:**
- Queries OttoFlow's own CRDs (Workflow, WorkflowRun) as examples
- Demonstrates CRD support works with any Custom Resource
- Uses CEL expressions: `object.spec.steps[0].name`, `items.filter(...)`
- Array indexing: `items[0].metadata.name`, `items[size(items)-1].metadata.name`
- Enhanced error messages for CRD-specific issues

**Usage:**
```bash
kubectl apply -f crd-resource-query.yaml
```

**Example: Querying a Workflow CRD**
```yaml
resourceQuery:
  apiVersion: ottoflow.nirmata.io/v1alpha1
  resource: Workflow
  name: 'inputs.workflowName'
  outputs:
    workflowName: object.metadata.name
    firstStepName: object.spec.steps[0].name  # Array indexing
    stepCount: size(object.spec.steps)
```

**Example: Listing CRDs with Filtering**
```yaml
resourceQuery:
  apiVersion: ottoflow.nirmata.io/v1alpha1
  resource: WorkflowRun
  outputs:
    succeededRuns: items.filter(i, i.status.phase == "Succeeded").map(i, i.metadata.name)
    latestRunName: size(items) > 0 ? items[size(items)-1].metadata.name : ""
```

**Note:** This demonstrates that Resource Query DSL now supports arbitrary CRDs, not just core Kubernetes resources.

---

### 33a. Mutate Step Example (`mutate-step-example.yaml`)
Demonstrates:
- **Mutate step** (Kyverno-style) to patch a single Kubernetes resource
- **ApplyConfiguration**: CEL expression returns a partial object that is deep-merged onto the resource
- **JSONPatch**: Static list of RFC 6902 operations (add, replace, etc.)
- Step outputs: CEL over the patched resource (`object`) after mutation
- Dependent steps: add labels first, then add annotation

**Usage:**
```bash
# Create a ConfigMap to mutate
kubectl create configmap my-config --from-literal=key=value -n default

# Run the workflow (adds labels and annotation to the ConfigMap)
kubectl apply -f mutate-step-example.yaml
# Or: ottoflow run mutate-step-example --input configMapName=my-config
```

**Note:** The workflow targets a ConfigMap by name (from inputs). It first adds labels via ApplyConfiguration, then adds an annotation via JSONPatch. For cluster-scoped resources, use an empty namespace (e.g. `namespace: "''"` in CEL).

---

### 33. CEL Expressions Example (`advanced-field-paths.yaml`)
Demonstrates:
- **CEL expressions** in Resource Query DSL
- **Array indexing**: `object.spec.containers[0].image`, `items[0].metadata.name`
- **Nested filtering**: `object.status.conditions.filter(c, c.type == "Ready")[0].status`
- **Complex expressions**: Multiple levels of array access and filtering
- **Safe array access**: Using `size()` checks before indexing

**Key Features:**
- Array indexing for single resources: `object.spec.containers[0].image`
- Array indexing for lists: `items[0].metadata.name`, `items[size(items)-1].metadata.name`
- Nested array access: `items.map(i, i.spec.containers[0].image)`
- Filtering with nested access: `items.filter(i, size(i.spec.containers) > 1)`
- Safe access patterns: `size(items) > 0 ? items[0] : ""`

**Usage:**
```bash
kubectl apply -f advanced-field-paths.yaml
```

**Example: Array Indexing**
```yaml
outputs:
  firstContainerImage: object.spec.containers[0].image
  secondContainerImage: size(object.spec.containers) > 1 ? object.spec.containers[1].image : ""
  firstPodName: size(items) > 0 ? items[0].metadata.name : ""
```

**Example: Nested Filtering**
```yaml
outputs:
  readyCondition: object.status.conditions.filter(c, c.type == "Ready")[0].status
  runningPodNames: items.filter(i, i.status.phase == "Running").map(i, i.metadata.name)
```

**Example: Complex Nested Access**
```yaml
outputs:
  allContainerImages: items.map(i, i.spec.containers.map(c, c.image))
  firstContainerReadyStatus: items.map(i, size(i.status.containerStatuses) > 0 ? i.status.containerStatuses[0].ready : false)
```

---

### 36. Kyverno CEL Libraries Example (`kyverno-cel-libraries.yaml`)
Demonstrates the full suite of Kyverno CEL libraries (provided via [Kyverno SDK CEL](https://github.com/kyverno/sdk/tree/main/cel)):
- **Hash Library**: `md5()`, `sha256()` for computing hash values
- **Image Library**: `image()`, `image().registry()`, `image().repository()`, `image().tag()`, `image().containsDigest()`
- **HTTP Library**: `http.Get()` for fetching external HTTP/HTTPS endpoints
- **JSON Library**: `json.unmarshal()` for parsing JSON strings
- **Math Library**: `math.round()` for rounding numbers with precision
- **Random Library**: `random()` and `random(pattern)` for generating random strings
- **Time Library**: `time.now()`, `time.toCron()`, `duration()` for time operations
- **Transform Library**: `listObjToMap()` for merging lists into maps

**Usage:**
```bash
kubectl apply -f kyverno-cel-libraries.yaml
```

### 37. Kubernetes CEL Libraries Example (`kubernetes-cel-libraries.yaml`)
Demonstrates Kubernetes CEL libraries from `k8s.io/apiserver/pkg/cel/library`:
- **List Library**: `indexOf()`, `lastIndexOf()`, `min()`, `max()`, `sum()`, `isSorted()`
- **Regex Library**: `find()`, `findAll()` for advanced regex operations
- **URL Library**: `isURL()`, `url().getScheme()`, `url().getHost()`, `url().getPort()`, etc.
- **IP Address Library**: `isIP()`, `ip().family()`, `ip().isLoopback()`, `ip().isGlobalUnicast()`, etc.
- **CIDR Library**: `cidr()`, `isCIDR()`, `cidr().containsIP()`, `cidr().prefixLength()`, etc.
- **Format Library**: `format.dns1123Label()`, `format.uuid()`, `format().validate()` for format validation
- **Quantity Library**: `quantity()`, `isQuantity()`, `quantity().asInteger()`, `quantity().add()`, etc.
- **Semver Library**: `semver()`, `isSemver()`, `semver().major()`, `semver().compareTo()`, etc.

**Usage:**
```bash
kubectl apply -f kubernetes-cel-libraries.yaml
```

### 38. Practical CEL Examples (`practical-cel-examples.yaml`)
Demonstrates real-world use cases combining multiple CEL libraries:
- **Image Validation**: Validate container image format, check for digests, extract registry
- **Configuration Parsing**: Parse JSON config, validate semantic versions, work with quantities
- **Health Checks**: Validate URLs, check external service health
- **ID Generation**: Generate unique request IDs, short IDs, compute hashes
- **Format Validation**: Validate DNS labels, UUIDs, get validation errors
- **Network Validation**: Validate IP addresses, check CIDR membership, identify private IPs
- **Version Comparison**: Compare semantic versions, check compatibility ranges
- **Resource Calculations**: Calculate total resources, convert quantities, compare limits
- **Data Aggregation**: Count running pods, calculate CPU averages, find min/max values

**Usage:**
```bash
kubectl apply -f practical-cel-examples.yaml
```

### 39. ArgoCD Cluster Discovery (`argocd-cluster-discovery.yaml`)
Demonstrates **Phase 1** of OttoFlow × ArgoCD integration:
- Listing ArgoCD managed clusters via `resourceQuery` (Secrets with `argocd.argoproj.io/secret-type=cluster`)
- Extracting cluster names from the Hub cluster
- Foundation for multi-cluster workflow orchestration

**Prerequisites:**
- OttoFlow installed on Hub cluster (where ArgoCD runs)
- ArgoCD with managed clusters registered
- OttoFlow RBAC: `get`, `list` on Secrets in `argocd` namespace with label `argocd.argoproj.io/secret-type=cluster`

**Usage:**
```bash
kubectl apply -f argocd-cluster-discovery.yaml
# Or: ottoflow run argocd-list-clusters --input argocdNamespace=argocd
```


### 40. Multi-cluster WorkflowRun (`multi-cluster-workflowrun.yaml`)
Demonstrates running a workflow against a **remote cluster** by providing a **KubeConfig secret** as input to the run:
- **WorkflowRun.spec.clusterRef.kubeConfigSecretRef**: References a Secret containing a kubeconfig file (key `config`, `kubeconfig`, or `value`).
- The `WorkflowRun` still executes through a runner `Job`; the runner resolves the Secret reference and builds the target client.
- Resource queries, mutate steps, and CEL `object.*` execute against that cluster.
- Omit `clusterRef` or set `clusterRef.local: true` to use the Hub (in-cluster) client.

**Prerequisites:**
- A Secret in the same namespace (or the ref namespace) with a kubeconfig file under key `config`, `kubeconfig`, or `value`.
- The workflow (e.g. `resource-query-example`) must exist.

**Usage:**
```bash
# Create the kubeconfig secret first, then the WorkflowRun
kubectl create secret generic my-remote-cluster-kubeconfig --from-file=config=/path/to/kubeconfig -n default
kubectl apply -f multi-cluster-workflowrun.yaml
```

### 41. Job-Customized WorkflowRun (`job-customized-workflowrun.yaml`)
Demonstrates customizing the runner `Job` for a single `WorkflowRun`:
- **WorkflowRun.spec.execution.job** with custom image, service account, env, resources, TTL, and deadline settings.
- Local (Hub) cluster execution through the same runner `Job` model used for all in-cluster runs.

**Prerequisites:**
- The workflow `resource-query-example` must exist.

**Usage:**
```bash
kubectl apply -f resource-query-example.yaml
kubectl apply -f job-customized-workflowrun.yaml
```

### 42. Mounted KubeConfig File WorkflowRun (`multi-cluster-workflowrun-filepath.yaml`)
Demonstrates targeting a remote cluster through a mounted kubeconfig file:
- **WorkflowRun.spec.clusterRef.kubeConfigFilePath** points to the kubeconfig path inside the runner pod.
- **WorkflowRun.spec.execution.job.volumes** and `volumeMounts` mount a Secret into the runner Job.

**Prerequisites:**
- A Secret with a kubeconfig file under key `config`.
- The workflow `resource-query-example` must exist.

**Usage:**
```bash
kubectl create secret generic my-remote-cluster-kubeconfig --from-file=config=/path/to/kubeconfig -n default
kubectl apply -f resource-query-example.yaml
kubectl apply -f multi-cluster-workflowrun-filepath.yaml
```

### 43. CSI-Mounted KubeConfig WorkflowRun (`multi-cluster-workflowrun-csi.yaml`)
Demonstrates targeting a remote cluster with a CSI-mounted kubeconfig:
- **WorkflowRun.spec.clusterRef.kubeConfigFilePath** points to the file provided by the CSI volume.
- **WorkflowRun.spec.execution.job.volumes** defines a CSI volume (for example, Secrets Store CSI Driver).
- Keeps kubeconfig resolution inside the runner pod boundary.

**Prerequisites:**
- A CSI driver that can project kubeconfig content into the pod.
- A `SecretProviderClass` named `remote-cluster-kubeconfig`.
- The workflow `resource-query-example` must exist.

**Usage:**
```bash
kubectl apply -f resource-query-example.yaml
kubectl apply -f multi-cluster-workflowrun-csi.yaml
```

---

Demonstrates:
- **StepTemplate CRD** for reusable step definitions
- **Template instantiation** with parameter substitution
- **Multiple StepTemplates** in a single workflow
- **Composition** of template-based steps

**Key Features:**
- Uses predefined StepTemplates (`collect-pod-info`, `get-pod-metrics`, `get-pod-events`, `list-pods-with-filter`)
- Parameter substitution using Go template syntax (`{{.parameterName}}`)
- Arguments evaluated as CEL expressions in workflow context
- Step-level overrides (dependsOn, etc.) still work
- Reduces workflow verbosity significantly

**Usage:**
```bash
# First, create the StepTemplates
kubectl apply -f ../steptemplates/

# Then create and run the workflow
# Apply StepTemplates first, then workflow
kubectl apply -f ../steptemplates/
kubectl apply -f steptemplate-example.yaml
```

**Example: Using a StepTemplate**
```yaml
steps:
  - name: collectPodInfo
    stepTemplateRef:
      name: collect-pod-info
      arguments:
        namespace: 'inputs.namespace'
        podName: 'inputs.podName'
```

**Example: StepTemplate Definition**
```yaml
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: StepTemplate
metadata:
  name: collect-pod-info
spec:
  parameters:
    - name: namespace
      required: true
    - name: podName
      required: true
  step:
    resourceQuery:
      apiVersion: v1
      resource: Pod
      namespace: '"{{.namespace}}"'
      name: '"{{.podName}}"'
      outputs:
        phase: object.status.phase
        restartCount: object.status.containerStatuses.map(c, int(c.restartCount)).sum()
```

**Note:** StepTemplates enable significant verbosity reduction by encapsulating common step patterns into reusable templates.

---

- List queries with `labelSelector` and `fieldSelector` support
- Output extraction using CEL expressions
- List aggregation functions (`size()`, `map()`, `filter()`)

**Usage:**
```bash
kubectl apply -f resource-query-example.yaml
```

**Key Features:**
- Uses `resourceQuery` step type instead of raw CEL `resource.Get()`/`resource.List()` expressions
- Direct client-go integration for efficient resource queries
- Supports both single resource and list queries
- Label and field selector filtering
- Outputs written to `variables` map (flat namespace)

## Prerequisites

Before running these examples, ensure:
1. OttoFlow controller is running (`make run` or deployed)
2. CRDs are installed (`make install`)
3. Required Kubernetes resources exist (for resource.Get/List examples):
   - Deployments, Pods, Services in the default namespace
4. For Agent and MCP examples:
   - Agent CRDs created (`agent-config.yaml`)
   - MCPServer CRDs created (`mcpserver-config.yaml`)
   - LLM provider credentials configured (for agent steps)
   - MCP servers accessible (for MCP tool calls)

## Creating Test Resources

For testing resource access workflows, create test resources:

```bash
# Create a test deployment
kubectl create deployment nginx --image=nginx:latest

# Create a test service
kubectl create service clusterip nginx --tcp=80:80

# Verify resources exist
kubectl get deployments,services,pods
```
