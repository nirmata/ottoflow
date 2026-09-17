# RBAC Generation for OttoFlow Workflows

## Problem

When OttoFlow runs a workflow, it creates a runner pod to execute the steps. That pod's ServiceAccount needs Kubernetes RBAC permissions to do whatever the steps require — listing pods, patching deployments, querying custom resources, and so on. These permissions are workflow-specific: one workflow might list pods in `staging`, another might patch deployments cluster-wide.

This feature is a tool that reads a workflow definition and generates the correct RBAC YAML automatically. The operator takes the output and applies it however they choose.

**Hard constraint:** The tool only produces manifests. It never applies anything to the cluster.

## Two-Layer RBAC Model

OttoFlow uses two layers of RBAC for each workflow run:

**Layer 1 — System (controller-managed):** When a WorkflowRun starts, the controller automatically creates a ServiceAccount named `{workflow-name}-runner` (if it doesn't exist) and binds it to the `ottoflow-runner-role` ClusterRole via a ClusterRoleBinding. This is a narrowed, runner-specific role — it covers OttoFlow's own resources (WorkflowRuns, Agents, MCPServers, checkpoint ConfigMaps, and so on) but deliberately excludes controller-only permissions such as writing ClusterRoleBindings/ServiceAccounts or patching ValidatingWebhookConfigurations, and grants nothing on Secrets: the controller mounts the Secrets a workflow references into the runner Job as files, so the runner needs no Secret read of its own (see `docs/user/rbac-secret-access.md`). A `secrets` rule in `rbac.runnerClusterRole.extraResources` is rejected at render time; if a workflow's `mutate` steps need to write Secrets, grant that through the workflow-specific RBAC in Layer 2, scoped to the namespace that needs it, not through the chart.

**Layer 2 — Workflow-specific (operator-managed):** Steps that access user-defined Kubernetes resources (`resourceQuery`, `mutate`) need additional permissions beyond `ottoflow-runner-role`. The operator generates these with `validate --generate-rbac` and applies them before running the workflow.

Both layers are required for workflows with `resourceQuery` or `mutate` steps. Workflows that only use `agentRef` or `mcpToolCall` steps are typically covered by Layer 1 alone.

## What Gets Generated

For each workflow, the tool produces:

| Object | Name pattern | Condition |
|---|---|---|
| `ServiceAccount` | `{workflow-name}-runner` | Always |
| `Role` | `{workflow-name}-runner` | One per literal namespace referenced in steps |
| `RoleBinding` | `{workflow-name}-runner` | One per Role |
| `ClusterRole` | `{workflow-name}-runner` | When any step is cluster-scoped or has a dynamic namespace |
| `ClusterRoleBinding` | `{workflow-name}-runner` | When ClusterRole is generated |

### Step type → RBAC mapping

| Step type | Verbs | Resources |
|---|---|---|
| `resourceQuery` | `get`, `list` | GVK from step spec |
| `mutate` | `get`, `patch`, `update` | GVK from step spec |
| `agentRef` | `get` | `configmaps/agent-executor-caller` in agent-executor namespace |
| `workflowRef`, `stepTemplateRef`, `mcpToolCall` | *(covered by system `ottoflow-runner-role`)* | — |
| `forEach` | Inherits from inner step | — |

## Command Interface

```bash
# Single workflow, output to stdout
ottoflow validate -f workflow.yaml --generate-rbac --namespace my-namespace

# Write to file
ottoflow validate -f workflow.yaml --generate-rbac --namespace my-namespace --output rbac.yaml

# All workflows in a directory
ottoflow validate --workflow-dir samples/workflows --generate-rbac --namespace my-namespace --output rbac.yaml
```

`--namespace` is required with `--generate-rbac` — the tool errors if omitted. Use the namespace where your WorkflowRuns will be submitted.

`--agent-executor-namespace` (default `ottoflow`) sets the namespace used for `agentRef` RBAC rules. This must match the namespace the agent-executor is deployed in (i.e. `--agent-executor-caller-namespace` on the agent-executor binary). Only change this if your agent-executor runs in a non-default namespace.

RBAC is only generated after all validation checks pass.

## Namespace Handling

Roles are namespace-scoped, so the tool needs to know which namespace each step targets:

| Namespace value | Classification | RBAC generated |
|---|---|---|
| `""` (empty) | Cluster-scoped | ClusterRole |
| `staging` (RFC 1123 label) | Literal | Role in `staging` |
| `"staging"` (quoted CEL string) | Literal (unquoted) | Role in `staging` |
| `inputs.targetNamespace` | Dynamic | ClusterRole + WARNING to stderr |
| `resource.metadata.namespace` | Dynamic | ClusterRole + WARNING to stderr |

Dynamic namespaces (CEL expressions) cannot be resolved at file-read time. The tool generates a ClusterRole as a conservative fallback and prints a warning per affected step:

```
WARNING: step "countPods": namespace "inputs.namespace" is dynamic — generated ClusterRole as conservative fallback
```

The warning goes to stderr; the YAML goes to stdout or `--output`. The operator can apply the output as-is or manually narrow the ClusterRole to specific namespaces if needed.

## Implementation Details

### Generator (`cli/internal/rbac/`)

`Generator.GenerateForWorkflow(wf)` returns `([]byte, []string, error)`:
- `[]byte` — the YAML manifest
- `[]string` — warnings (one per step with a dynamic namespace)
- `error` — only for serialization failures

`Options.AgentExecutorNamespace` controls the namespace for `agentRef` rules. When empty, falls back to `Namespace`.

### Validate command (`cli/cmd/validate.go`)

- `generateRBACBytes(gen, wf)` — calls the generator, prints warnings to stderr, returns bytes
- `writeOutput(path, data)` — writes to `--output` file or stdout

In directory mode, the generator is constructed once and reused. All outputs are accumulated in memory with `---` separators and written in a single call.

## Testing

```bash
# Build the CLI
cd cli && go build -o /tmp/ottoflow .

# --namespace required
/tmp/ottoflow validate -f samples/workflows/features/agent-step.yaml --generate-rbac
# → error: --namespace is required with --generate-rbac

# Happy path: agentRef workflow
/tmp/ottoflow validate -f samples/workflows/features/agent-step.yaml \
  --generate-rbac --namespace my-namespace
# → SA agent-step-workflow-runner in my-namespace
# → Role for configmaps/agent-executor-caller in ottoflow

# Dynamic namespace: ClusterRole with warning
/tmp/ottoflow validate -f samples/workflows/features/pod-health-metrics.yaml \
  --generate-rbac --namespace my-namespace
# → WARNING to stderr, ClusterRole in output

# Validation failure blocks RBAC
/tmp/ottoflow validate -f samples/workflows/features/crd-resource-query.yaml --generate-rbac --namespace my-namespace
# → CEL_SYNTAX_ERROR, no RBAC output

# End-to-end: after controller is deployed, runner pod SA should be {workflow-name}-runner
kubectl get pod -n my-namespace -l app.kubernetes.io/part-of=ottoflow \
  -o jsonpath='{.items[*].spec.serviceAccountName}'
```
