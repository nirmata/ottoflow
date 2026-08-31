# OttoFlow Helm Chart

This Helm chart installs [OttoFlow](https://github.com/nirmata/ottoflow) - Agentic Workflow Orchestrator for Kubernetes.

## Prerequisites

- Kubernetes 1.29+
- Helm 3.0+
- kubectl configured to access your cluster

## Installation

### Quick Start

From OCI registry (recommended):
```bash
helm install ottoflow oci://ghcr.io/nirmata/ottoflow --version <version> --namespace ottoflow --create-namespace
```

### Install from Local Chart

```bash
# Clone the repository
git clone https://github.com/nirmata/ottoflow.git
cd ottoflow

# Ensure CRDs are synced to chart (if building from source)
make sync-crds

# Install using local chart
helm install ottoflow ./charts/ottoflow --namespace ottoflow --create-namespace
```

### Install with Custom Values

```bash
# Create a values file
cat > my-values.yaml <<EOF
controller:
  image:
    repository: my-registry/controller
    tag: v1.0.0
  replicaCount: 2
  resources:
    limits:
      cpu: 1000m
      memory: 256Mi
    requests:
      cpu: 100m
      memory: 128Mi
EOF

# Install with custom values
helm install ottoflow ./charts/ottoflow -f my-values.yaml --namespace ottoflow --create-namespace
```

## Configuration

The following table lists the configurable parameters and their default values:

### Global Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `global.imageRegistry` | Global Docker image registry | `ghcr.io/nirmata/ottoflow` |
| `global.imagePullPolicy` | Global image pull policy | `IfNotPresent` |
| `global.imagePullSecrets` | Global image pull secrets | `[]` |

### Controller Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `controller.image.registry` | Controller image registry | `ghcr.io/nirmata/ottoflow` |
| `controller.image.repository` | Controller image repository | `controller` |
| `controller.image.tag` | Controller image tag | `latest` |
| `controller.image.fullOverride` | Override full image name | `""` |
| `controller.replicaCount` | Number of controller replicas | `1` |
| `controller.nameOverride` | Override controller name | `""` |
| `controller.fullnameOverride` | Override full name | `""` |
| `controller.serviceAccount.create` | Create service account | `true` |
| `controller.serviceAccount.name` | Service account name | `""` |
| `controller.serviceAccount.annotations` | Service account annotations | `{}` |
| `controller.podSecurityContext` | Pod security context | See values.yaml |
| `controller.securityContext` | Container security context | See values.yaml |
| `controller.resources` | Controller resources | See values.yaml |
| `controller.nodeSelector` | Node selector | `{}` |
| `controller.tolerations` | Tolerations | `[]` |
| `controller.affinity` | Affinity rules | `{}` |
| `controller.podAnnotations` | Pod annotations | `{}` |
| `controller.podLabels` | Pod labels | `{}` |
| `controller.env` | Extra environment variables | `[]` |
| `controller.args` | Command arguments (flags); chart injects runner image, RBAC roles, and agent-executor flags automatically | `["--leader-elect"]` |
| `controller.mcp.enabled` | Serve the cluster's Workflows as MCP tools (`--mcp-addr`) | `false` |
| `controller.mcp.addr` | Listen address for the MCP server; the endpoint is `/mcp` | `:8084` |
| `controller.mcp.service.enabled` | Create a Service for the MCP port | `true` |
| `controller.mcp.service.type` | MCP Service type | `ClusterIP` |
| `controller.mcp.service.port` | MCP Service port | `8084` |
| `controller.livenessProbe` | Liveness probe config | See values.yaml |
| `controller.readinessProbe` | Readiness probe config | See values.yaml |
| `controller.terminationGracePeriodSeconds` | Termination grace period | `10` |
| `controller.metrics.enabled` | Enable metrics: `--metrics-bind-address`, a `metrics` container port, and a metrics Service | `true` |
| `controller.metrics.port` | Port the controller serves `/metrics` on | `8080` |
| `controller.metrics.serviceMonitor.enabled` | Enable ServiceMonitor | `false` |
| `controller.prometheusURL` | Prometheus URL for CEL `prometheusMetrics()`; passed to workflow runner Jobs | `""` |

Controller configuration is passed via **command-line flags** (args). The chart injects `--workflow-runner-image`, `--workflow-runner-service-account`, `--workflow-runner-cluster-role`, and when agent-executor is enabled, `--agent-executor-caller-cluster-role` and (for internal TLS) `--agent-executor-service-name` and `--agent-executor-namespace`. Pass additional flags via `controller.args` (e.g. `--secret-source-namespace`). **Prometheus URL:** set `controller.prometheusURL`; the controller uses it and passes it to every workflow runner Job, so it is environment-specific and does not need to be in the workflow. See [Configuration Reference](../../docs/user/reference/configuration.md) for all controller flags.

**Runner Job secrets**: If the runner Job references Secret-backed volumes (e.g. `ottoflow-agent-executor.*.svc.tls-ca` for agent-executor TLS) that exist in another namespace, the controller copies those secrets into the WorkflowRun's namespace before creating the Job. By default the source namespace is the Workflow's namespace. To copy from a fixed namespace (e.g. the OttoFlow install namespace) instead, pass `--secret-source-namespace=<namespace>` in `controller.args`.

### Workflow Runner Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `workflowRunner.serviceAccountName` | ServiceAccount for runner Jobs. When empty, the controller derives a per-workflow `{workflow-name}-runner` ServiceAccount bound to the narrowed `ottoflow-runner-role` | `""` |
| `workflowRunner.clusterRole` | ClusterRole bound to runner ServiceAccounts | `""` (defaults to `<fullname>-runner-role`) |
| `workflowRunner.imagePullSecrets` | Image pull secrets for the runner Job pod | `[]` (falls back to `global.imagePullSecrets`) |

Runner Jobs run under a least-privilege `ottoflow-runner-role` ClusterRole, separate from the controller's own `ottoflow-role`, so a workflow step cannot escalate to controller-level privileges (writing ServiceAccounts, ClusterRoleBindings, or ValidatingWebhookConfigurations). See [RBAC Customization](#rbac-customization) below to add resources to the runner role, and [RBAC-GENERATION.md](../../docs/dev/RBAC-GENERATION.md) for the full runner RBAC model.

**Setting `rbac.create=false`**: if you manage RBAC objects outside this chart, you must set `workflowRunner.clusterRole` explicitly to the name of the ClusterRole you create — the chart's default (`<fullname>-runner-role`) won't exist when `rbac.create=false`.

### Agent Executor Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `agentExecutor.enabled` | Enable agent-executor service | `true` |
| `agentExecutor.replicaCount` | Number of agent-executor replicas | `1` |
| `agentExecutor.image.registry` | Agent executor image registry | `ghcr.io/nirmata/ottoflow` |
| `agentExecutor.image.repository` | Agent executor image repository | `agent-executor` |
| `agentExecutor.image.tag` | Agent executor image tag | `latest` |
| `agentExecutor.image.fullOverride` | Override full image name | `""` |
| `agentExecutor.serviceAccount.create` | Create service account | `true` |
| `agentExecutor.serviceAccount.name` | Service account name | `""` (auto-generated) |
| `agentExecutor.service.type` | Service type | `ClusterIP` |
| `agentExecutor.service.port` | Service port | `8443` |
| `agentExecutor.callerNamespace` | Namespace for RBAC auth (SubjectAccessReview checks get configmaps/agent-executor-caller in this namespace). Defaults to release namespace. | `""` (defaults to release namespace) |
| `agentExecutor.tls.internal.enabled` | Use internal cert controller (kyverno/pkg) to provision self-signed TLS | `true` |
| `agentExecutor.tls.secretName` | Use existing TLS Secret instead of internal cert generation | `""` |
| `agentExecutor.resources` | Resource limits and requests | `{}` (defaults provided) |

**Note**: The agent-executor serves HTTPS and requires TLS certs. By default, the internal certificate controller (kyverno/pkg) provisions self-signed CA and TLS certs—no cert-manager required.

**Note**: The agent-executor service is enabled by default. To disable it:

```yaml
agentExecutor:
  enabled: false
```

### RBAC Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `rbac.create` | Create RBAC resources | `true` |
| `rbac.leaderElection.create` | Create leader election RBAC | `true` |
| `rbac.coreClusterRole.extraResources` | Additional resources for core ClusterRole | `[]` |
| `rbac.viewClusterRole.extraResources` | Additional resources for view ClusterRole | `[]` |
| `rbac.clusterRole.extraResources` | Additional resources for additional ClusterRole | `[]` |
| `rbac.runnerClusterRole.extraResources` | Additional resources aggregated into the runner-only `ottoflow-runner-role` (never into the controller's own roles) | `[]` |

The controller's `ottoflow-role:core` ClusterRole grants `delete` on ClusterRoleBindings (needed to migrate a runner's binding when its target role changes — RoleRef is immutable, so migration is delete-and-recreate) and `bind` scoped to the runner ClusterRole name (needed so the controller can create ClusterRoleBindings referencing it, including when `rbac.runnerClusterRole.extraResources` grants verbs the controller itself doesn't hold — Kubernetes' RBAC escalation check allows `bind` as an alternative to holding every referenced permission). **Blast radius:** the `delete` grant on ClusterRoleBindings is cluster-wide, so a compromised controller could delete any ClusterRoleBinding in the cluster; it is a temporary grant needed only for this role migration, and is slated for removal once #153 is resolved.

#### RBAC Customization

OttoFlow uses [Kubernetes ClusterRole aggregation](https://kubernetes.io/docs/reference/access-authn-authz/rbac/#aggregated-clusterroles) to allow easy extension of permissions. The controller has four aggregated ClusterRoles:

1. **Core ClusterRole** (`ottoflow-role:core`): OttoFlow-specific permissions (CRDs, ConfigMaps, Jobs)
2. **View ClusterRole** (`ottoflow-role:view`): Read permissions for standard Kubernetes resources
3. **Additional ClusterRole** (`ottoflow-role:additional`): Custom resources and additional permissions
4. **Runner ClusterRole** (`ottoflow-role:runner`, aggregated into `ottoflow-runner-role`): Least-privilege permissions bound to workflow runner Jobs, kept separate from the controller's own `ottoflow-role`

To extend permissions, you can either:

**Option 1: Use Helm values** (recommended for simple cases)

```yaml
rbac:
  viewClusterRole:
    extraResources:
      - apiGroups:
          - mycompany.com
        resources:
          - myresources
        verbs:
          - get
          - list
          - watch
```

**Option 2: Create a custom ClusterRole** (recommended for complex cases)

Create a ClusterRole with **both** the aggregation label `rbac.ottoflow.io/aggregate-to-controller: "true"` and `rbac.ottoflow.io/aggregate-instance: <chart-fullname>` — `<chart-fullname>` is the same name that prefixes this install's `<release>-role` / `<release>-runner-role` ClusterRoles (see the `jsonpath` command below for how to read the exact value):

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: ottoflow:custom-resources
  labels:
    rbac.ottoflow.io/aggregate-to-controller: "true"
    rbac.ottoflow.io/aggregate-instance: <chart-fullname>
rules:
  - apiGroups:
      - mycompany.com
    resources:
      - myresources
    verbs:
      - get
      - list
      - watch
      - create
      - update
      - patch
      - delete
```

The permissions will automatically be aggregated into the main `<release>-role` ClusterRole. `<release>-role` and `<release>-runner-role` are both named from the chart's **fullname** — by default the release name, but overridable via `fullnameOverride`/`nameOverride`, so it is not always the literal name you passed to `helm install`.

To aggregate into the runner role instead, label the custom ClusterRole with **both** `rbac.ottoflow.io/aggregate-to-runner: "true"` **and** `rbac.ottoflow.io/aggregate-instance: <chart-fullname>`. The runner role only aggregates fragments from its own Helm release (so multiple OttoFlow installs on one cluster stay isolated), aggregating into that release's `<release>-runner-role`.

**Verifying Aggregation**

To verify that permissions have been aggregated:

```bash
kubectl get clusterrole <release>-role -o yaml
kubectl get clusterrole <release>-runner-role -o yaml
```

To read the exact `aggregate-instance` value this install expects on custom ClusterRoles:

```bash
kubectl get clusterrole <release>-runner-role -o jsonpath='{.aggregationRule.clusterRoleSelectors[0].matchLabels.rbac\.ottoflow\.io/aggregate-instance}'
```

You should see the aggregated rules from all ClusterRoles with the matching label.

### Validating Webhook Parameters

The chart installs a `ValidatingWebhookConfiguration` that rejects malformed `Workflow`,
`WorkflowRun`, `Agent`, and `MCPServer` resources at admission time. TLS is handled by the
controller's internal cert manager — no cert-manager installation or user-supplied
certificates are required.

| Parameter | Description | Default |
|-----------|-------------|---------|
| `webhook.enabled` | Install the ValidatingWebhookConfiguration | `true` |
| `webhook.failurePolicy` | `Fail` rejects the request when the webhook is unreachable or errors; `Ignore` admits it | `Fail` |
| `webhook.timeoutSeconds` | Admission request timeout | `10` |
| `webhook.caBundle` | Override the CA bundle in the VWC (e.g. custom PKI). When empty, the controller patches it from its internal CA | `""` |
| `webhook.annotations` | Extra annotations on the ValidatingWebhookConfiguration | `{}` |

#### Choosing a failurePolicy

The default `Fail` trades availability for correctness. While the controller is unavailable
(upgrade, eviction, crash loop), `CREATE` and `UPDATE` on the four OttoFlow kinds are rejected
until it recovers. The blast radius is limited to those kinds — the webhook matches only the
`ottoflow.nirmata.io` API group, so nothing else in the cluster is affected.

Choose `Ignore` to invert that tradeoff — useful in a single-replica development cluster where
restarts are frequent. Invalid resources are then admitted whenever the webhook is unreachable
and surface as run-time failures instead of at `kubectl apply`:

```bash
# relax to fail-open
helm upgrade ottoflow ./charts/ottoflow -n ottoflow --set webhook.failurePolicy=Ignore

# or skip admission validation entirely
helm upgrade ottoflow ./charts/ottoflow -n ottoflow --set webhook.enabled=false
```

### Network Policy Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `networkPolicy.create` | Create NetworkPolicy | `true` |
| `networkPolicy.egress` | Egress rules | See values.yaml |

### CRD Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|

### Namespace Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `namespace.name` | Override namespace (if not set, uses `--namespace` from helm) | `""` |

## Examples

### Install with Custom Image

```yaml
controller:
  image:
    registry: ghcr.io/nirmata/ottoflow
    repository: controller
    tag: v1.0.0-alpha
```

### Install with Resource Limits

```yaml
controller:
  resources:
    limits:
      cpu: 2000m
      memory: 512Mi
    requests:
      cpu: 500m
      memory: 256Mi
```

### Install with Node Selector

```yaml
controller:
  nodeSelector:
    kubernetes.io/os: linux
    kubernetes.io/arch: amd64
```

### Install with Tolerations

```yaml
controller:
  tolerations:
  - key: "node-role.kubernetes.io/control-plane"
    operator: "Exists"
    effect: "NoSchedule"
```

### Install with Affinity

```yaml
controller:
  affinity:
    podAntiAffinity:
      preferredDuringSchedulingIgnoredDuringExecution:
      - weight: 100
        podAffinityTerm:
          labelSelector:
            matchExpressions:
            - key: app.kubernetes.io/name
              operator: In
              values:
              - ottoflow
          topologyKey: kubernetes.io/hostname
```

### Install with extra controller args (e.g. Prometheus)

```yaml
controller:
  args:
  - --leader-elect
  - --prometheus-url=http://localhost:9090
```

### Install with Custom Service Account

```yaml
controller:
  serviceAccount:
    create: false
    name: my-custom-service-account
```

### Disable Network Policy

```yaml
networkPolicy:
  create: false
```

### Skip CRD Installation

```bash
helm install ottoflow ./charts/ottoflow --skip-crds
```

## Upgrading

```bash
# Upgrade with default values
helm upgrade ottoflow ottoflow/ottoflow --namespace ottoflow

# Upgrade with custom values
helm upgrade ottoflow ./charts/ottoflow -f my-values.yaml --namespace ottoflow
```

### Runner RBAC change (least-privilege runner role)

Workflow runner Jobs now default to a per-workflow ServiceAccount bound to a narrowed `<release>-runner-role` ClusterRole, instead of the controller's own ServiceAccount and `<release>-role`. This is a **breaking change** for any install that relied on runner Jobs implicitly holding full controller privileges (for example, custom steps that write ServiceAccounts or ClusterRoleBindings).

- **Escape hatch**: to restore the previous behavior, set **both** `workflowRunner.serviceAccountName` to the controller's ServiceAccount name **and** `workflowRunner.clusterRole=<release>-role`. Setting the ServiceAccount name alone is not enough for runners in other namespaces: the controller's existing ClusterRoleBinding to `<release>-role` names a subject by ServiceAccount name *and* namespace (the controller's own install namespace), so it only auto-applies when the runner SA of the same name happens to live in that same namespace — everywhere else you also need `workflowRunner.clusterRole=<release>-role` so the controller binds the runner SA to it explicitly.
- **In-flight runner Jobs**: pods created by the pre-upgrade controller keep running under the controller's ServiceAccount until they complete; this is expected and not disruptive.
- **Legacy ClusterRoleBinding does not migrate automatically — clean it up manually.** Before this change, the default runner ServiceAccount was the controller's own `controller-manager`; after it, the default is a per-workflow `<workflow>-runner` name. The controller only reconciles a runner ClusterRoleBinding whose name it can still derive from a *current* runner ServiceAccount, so the pre-upgrade binding (named from the `controller-manager` SA, e.g. `ottoflow-runner-<namespace>-controller-manager`) matches no derived name anymore and is never revisited. It does **not** migrate to the new role and does **not** expire on its own — `controller-manager` stays bound to the full `<release>-role` ClusterRole indefinitely until you remove it yourself. Use the review-then-delete command below.
- **Orphaned RBAC objects**: in the common case (a per-workflow ServiceAccount in the same namespace as its Workflow, with no `workflowRunner.serviceAccountName` override), the runner ServiceAccount now carries an `ownerReference` to its Workflow and is garbage-collected automatically when the Workflow is deleted. The ClusterRoleBinding remains cluster-scoped and is never owned by a namespaced Workflow, so it is never garbage-collected. Runner ServiceAccounts for Workflows in a different namespace than their WorkflowRuns, or when `workflowRunner.serviceAccountName` is set to a shared name, are also not owned and still accumulate.

#### Cleaning up runner RBAC objects

**Do not run `kubectl delete clusterrolebinding -l app.kubernetes.io/part-of=ottoflow`** — every chart-managed ClusterRoleBinding (`<release>-rolebinding`, `<release>-agent-executor-rolebinding`, `<release>-agent-executor-caller-controller`) carries this same label, so that command deletes the chart's own bindings along with the per-runner ones you actually want to remove.

Review first: the controller-created per-runner and per-runner-caller bindings each have a `<namespace>` segment in their name, which the three chart-managed bindings above do not.

```bash
# Chart-managed bindings (NEVER delete): <release>-rolebinding,
# <release>-agent-executor-rolebinding, <release>-agent-executor-caller-controller.
# Review controller-created per-runner bindings (each contains a <namespace> segment):
kubectl get clusterrolebinding -l app.kubernetes.io/part-of=ottoflow -o name \
  | grep -E '/(ottoflow-runner-|ottoflow-agent-executor-caller-)' \
  | grep -vE '/ottoflow-agent-executor-caller-controller$'
```

Once you've confirmed every listed name contains a `<namespace>` segment (i.e. none of them are the three chart-managed bindings), delete them:

```bash
kubectl get clusterrolebinding -l app.kubernetes.io/part-of=ottoflow -o name \
  | grep -E '/(ottoflow-runner-|ottoflow-agent-executor-caller-)' \
  | grep -vE '/ottoflow-agent-executor-caller-controller$' \
  | xargs -r kubectl delete
```

For installs using `fullnameOverride`, substitute the rendered fullname for the `ottoflow-` prefix in both `grep` patterns above.

Then remove the corresponding orphaned ServiceAccounts per namespace, filtering similarly by name or age before deleting in a shared cluster, since the same label is applied to all OttoFlow-managed objects:

```bash
kubectl get serviceaccount -n <namespace> -l app.kubernetes.io/part-of=ottoflow
```

### Custom aggregated ClusterRoles must be labeled for their release

The aggregation selectors for both `<release>-role` (controller) and
`<release>-runner-role` now require a per-release match label in addition to the
`rbac.ottoflow.io/aggregate-to-controller` / `rbac.ottoflow.io/aggregate-to-runner`
label, so that multiple OttoFlow installs on one cluster no longer aggregate each
other's custom ClusterRoles. The match label's value is the chart's **fullname**
(see the `jsonpath` command below to read the exact value for this install) —
the same name that prefixes `<release>-role` / `<release>-runner-role`.

**Action required if you maintain custom aggregated ClusterRoles.** A custom role
written for an earlier chart version with only:

    metadata:
      labels:
        rbac.ottoflow.io/aggregate-to-controller: "true"

will **silently stop aggregating** after `helm upgrade` — its permissions simply
disappear from `<release>-role`, with no error emitted. Add the release match label:

    metadata:
      labels:
        rbac.ottoflow.io/aggregate-to-controller: "true"
        rbac.ottoflow.io/aggregate-instance: <chart-fullname>

To read the exact value this install expects:

    kubectl get clusterrole <release>-runner-role -o jsonpath='{.aggregationRule.clusterRoleSelectors[0].matchLabels.rbac\.ottoflow\.io/aggregate-instance}'

(Use `rbac.ottoflow.io/aggregate-to-runner: "true"` for the runner role.) Verify
after upgrading:

    kubectl get clusterrole <release>-role -o yaml         # controller aggregate
    kubectl get clusterrole <release>-runner-role -o yaml  # runner aggregate

If a custom role's rules are missing from the output, it is not labeled for this
release.

## CRD Management

CRDs are included in the `crds/` directory and will be installed automatically when you install the chart. 

**Important Notes:**
- CRDs are **generated from Go code** in `api/` using `controller-gen`
- CRDs in `config/crd/bases/` are the **source of truth** (generated by `make manifests`)
- CRDs in `charts/ottoflow/crds/` are **synced** from `config/crd/bases/` (via `make sync-crds`)
- CRDs in the `crds/` directory are installed **before** any templates
- Helm **does not upgrade** CRDs - they must be upgraded manually if needed
- CRDs are **not removed** when uninstalling the chart (by design, to prevent data loss)

### Syncing CRDs

CRDs are automatically synced when running `make manifests`. To sync manually:

```bash
# Generate CRDs from Go code
make manifests

# Or sync existing CRDs to chart
make sync-crds
```

**Do not edit CRDs in `charts/ottoflow/crds/` directly** - they will be overwritten on sync.

### Skip CRD Installation

If CRDs are managed separately (e.g., via GitOps or another tool), skip them with
Helm's own flag:

```bash
helm install ottoflow ./charts/ottoflow --skip-crds
```

### Manual CRD Installation

If you prefer to install CRDs manually:

```bash
# Install CRDs manually from source
kubectl apply -f config/crd/bases/

# Or from chart (after syncing)
kubectl apply -f charts/ottoflow/crds/

# Then install the chart without them
helm install ottoflow ./charts/ottoflow --namespace ottoflow --create-namespace \
  --skip-crds
```

CRDs live in `crds/`, which Helm installs before any template and does not
template itself, so no chart value can gate them. `--skip-crds` is Helm's own
flag and the only way to skip them.

## Uninstallation

```bash
# Uninstall OttoFlow
helm uninstall ottoflow --namespace ottoflow

# Note: CRDs are not removed by default. To remove CRDs manually:
kubectl delete crd workflows.ottoflow.nirmata.io
kubectl delete crd workflowruns.ottoflow.nirmata.io
kubectl delete crd agents.ottoflow.nirmata.io
kubectl delete crd mcpservers.ottoflow.nirmata.io
kubectl delete crd steptemplates.ottoflow.nirmata.io
```

## Troubleshooting

### Check Pod Status

```bash
kubectl get pods --namespace ottoflow -l control-plane=controller-manager
```

### View Controller Logs

```bash
kubectl logs --namespace ottoflow -l control-plane=controller-manager --tail=50
```

### Check RBAC

```bash
kubectl get clusterrole,clusterrolebinding --namespace ottoflow
```

### Verify CRDs

```bash
kubectl get crds | grep ottoflow.nirmata.io
```

## Support

For issues and questions:
- GitHub Issues: https://github.com/nirmata/ottoflow/issues
- Documentation: https://github.com/nirmata/ottoflow/docs

## License

Apache License 2.0
