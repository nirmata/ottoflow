# Setting Up Triggers

Triggers let workflows run automatically — on a schedule (cron) or in response to Kubernetes resource changes (event). When a trigger fires, the controller creates a new WorkflowRun without any manual intervention.

## Prerequisites

- OttoFlow controller deployed and running (see [Installation](installation.md))
- `kubectl` configured to access your cluster

## Cron Triggers

Cron triggers execute workflows on a recurring schedule using standard 5-field cron syntax.

### Basic cron trigger

Create a workflow that runs every 5 minutes:

```yaml
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: heartbeat
  namespace: default
spec:
  steps:
    - name: ping
      expressions:
        - name: ts
          expression: 'string(time.now())'
      outputs:
        - name: message
          expression: '"Heartbeat at " + expressions.ts'
  outputs:
    - name: heartbeat
      expression: 'variables.message'
  triggers:
    - cron:
        schedule: "*/5 * * * *"
        concurrencyPolicy: "Forbid"
```

Apply it and watch WorkflowRuns appear:

```bash
kubectl apply -f heartbeat.yaml

# After the first fire (at the next 5-minute mark):
kubectl get workflowruns -l ottoflow.nirmata.io/workflow=heartbeat
```

### Schedule syntax

The `schedule` field uses standard cron format:

```
┌───────────── minute (0–59)
│ ┌───────────── hour (0–23)
│ │ ┌───────────── day of month (1–31)
│ │ │ ┌───────────── month (1–12)
│ │ │ │ ┌───────────── day of week (0–6, Sun=0)
│ │ │ │ │
* * * * *
```

| Expression | Meaning |
|-----------|---------|
| `* * * * *` | Every minute |
| `*/5 * * * *` | Every 5 minutes |
| `0 * * * *` | Every hour, on the hour |
| `0 9 * * *` | Daily at 9:00 AM |
| `0 9 * * 1-5` | Weekdays at 9:00 AM |
| `0 0 1 * *` | First day of every month at midnight |

### Timezone

By default schedules fire in UTC. Use the `timezone` field with an IANA timezone name to fire in a different zone:

```yaml
triggers:
  - cron:
      schedule: "0 9 * * *"
      timezone: "America/New_York"
```

This fires at 9:00 AM Eastern time, adjusting automatically for daylight saving.

### Concurrency policy

The `concurrencyPolicy` field controls what happens when a schedule fires while a previous run is still active (Pending or Running):

| Policy | Behaviour |
|--------|-----------|
| `Forbid` (default) | Skip the new run; the active run continues undisturbed. |
| `Allow` | Create a new run regardless. Multiple runs may execute in parallel. |
| `Replace` | Cancel the active run (set it to Failed) and start a fresh one. |

```yaml
triggers:
  - cron:
      schedule: "*/5 * * * *"
      concurrencyPolicy: "Replace"
```

### Inspecting trigger metadata

Every WorkflowRun created by a cron trigger has the trigger details in its status:

```bash
kubectl get workflowrun <name> -o jsonpath='{.status.trigger}' | jq .
```

```json
{
  "type": "Cron",
  "cronSchedule": "*/5 * * * *",
  "triggeredAt": "2026-03-03T09:00:00Z"
}
```

Trigger metadata is **not** available as a CEL variable inside workflow steps (`status` is not in the CEL context). To pass data from a triggering event into the workflow, use the trigger's `inputMapping` to map fields into workflow inputs.

### Removing a cron trigger

Delete the Workflow to stop future fires:

```bash
kubectl delete workflow heartbeat
```

Any WorkflowRuns already created will remain until they complete or are manually deleted.

## Event Triggers

Event triggers execute workflows when Kubernetes resources are created, updated, or deleted.

### Basic event trigger

Watch for new Pods in the `default` namespace:

```yaml
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: pod-monitor
  namespace: default
spec:
  # Bounds total concurrent runs regardless of how many events fire — a
  # cheap backstop for any event trigger, not just this one. See "Guarding
  # against runaway triggers" below.
  run:
    maxConcurrentRuns: 5
  steps:
    - name: log
      expressions:
        - name: msg
          expression: '"Pod event detected at " + string(time.now())'
      outputs:
        - name: message
          expression: 'expressions.msg'
  outputs:
    - name: event
      expression: 'variables.message'
  triggers:
    - event:
        resources:
          - apiVersion: v1
            kind: Pod
            namespace: default
        operations:
          - CREATE
        # Recommended for any trigger watching a namespace-wide, high-churn
        # kind like Pod or Job: narrow to the workload you actually care
        # about. OttoFlow's own runner Pods/Jobs are excluded automatically
        # (see "Guarding against runaway triggers"), but an unscoped watch
        # still fires on every unrelated Pod create in the namespace.
        labelSelector:
          matchLabels:
            app: my-monitored-app
```

### Filtering resources

Use label selectors and field selectors to narrow the watch:

```yaml
triggers:
  - event:
      resources:
        - apiVersion: apps/v1
          kind: Deployment
          namespace: production
      operations:
        - UPDATE
      labelSelector:
        matchLabels:
          team: platform
      fieldSelector: "metadata.name=api-server"
```

### Input mapping

Map data from the triggering resource into workflow inputs:

```yaml
spec:
  inputs:
    - name: podName
      description: "Name of the created pod"
    - name: namespace
      description: "Namespace of the created pod"
  triggers:
    - event:
        resources:
          - apiVersion: v1
            kind: Pod
            namespace: default
        operations:
          - CREATE
        # See "Basic event trigger" above for why a Pod/Job watch should be
        # scoped with labelSelector rather than left unfiltered.
        labelSelector:
          matchLabels:
            app: my-monitored-app
        inputMapping:
          podName: "object.metadata.name"
          namespace: "object.metadata.namespace"
```

### Filtering on resource status with celFilter

`fieldSelector` can only filter on fields that the Kubernetes API server explicitly indexes — for most CRDs this is only `metadata.name` and `metadata.namespace`. Status fields like `status.sync.status` or `status.conditions` are **not** indexed and cannot be filtered at the watch level.

Use `celFilter` instead. It's a CEL expression evaluated in-process against each incoming event object. Events where the expression returns `false` (or errors) are dropped before a WorkflowRun is created.

The variable `object` is the full triggering resource as a dynamic map, identical to what `inputMapping` expressions see.

```yaml
triggers:
  - event:
      resources:
        - apiVersion: argoproj.io/v1alpha1
          kind: Application
          namespace: argocd
      operations:
        - UPDATE
      # Only fire when the sync has completed successfully.
      # This cannot be done with fieldSelector — status fields are not indexable.
      celFilter: 'object.status.sync.status == "Synced"'
```

For FluxCD conditions (array of objects):

```yaml
celFilter: 'object.status.conditions.exists(c, c.type == "Ready" && c.status == "True")'
```

If the `celFilter` expression fails to compile or evaluate, the event is dropped and a debug-level message is logged (visible at controller verbosity ≥1).

### Deduplication

A single ArgoCD sync or FluxCD reconciliation generates multiple rapid UPDATE events as the resource moves through state transitions. Without deduplication, one deployment would create several WorkflowRuns.

**Auto-detection (no configuration required):** OttoFlow probes well-known revision fields in order and uses the first non-empty value as the dedup key:

| Controller | Field |
|------------|-------|
| ArgoCD `Application` | `status.sync.revision` |
| FluxCD `Kustomization` | `status.lastAppliedRevision` |
| FluxCD `HelmRelease` | `status.lastAttemptedRevision` |
| FluxCD `OCIRepository` / `GitRepository` | `status.artifact.revision` |

If the same object fires again with the same revision value, the event is dropped. When the revision changes (new deployment), a new WorkflowRun is created.

**Custom controller override:** For controllers not in the built-in list, set `dedupKey` to a CEL expression that extracts the revision:

```yaml
triggers:
  - event:
      resources:
        - apiVersion: fleet.cattle.io/v1alpha1
          kind: GitRepo
          namespace: fleet-local
      operations:
        - UPDATE
      celFilter: 'object.status.readyClusters == object.status.desiredReadyClusters'
      dedupKey: "object.status.commit"  # Rancher Fleet uses status.commit
```

**Time-window fallback:** For resources with no recognizable revision field, `dedupWindow` prevents burst duplicates by time:

```yaml
triggers:
  - event:
      resources:
        - apiVersion: v1
          kind: ConfigMap
          namespace: default
      operations:
        - UPDATE
      dedupWindow: 30s
```

Events for the same object within 30 seconds of the last WorkflowRun creation are dropped.

**`dedupWindow` defaults to 10 minutes** when no revision field is auto-detected and `dedupKey` isn't set; set it explicitly to override. Either way, `dedupWindow` only dedupes repeat events for an *object already seen* (keyed by UID) — it cannot bound the number of runs created by a trigger that keeps seeing brand-new objects, because a never-before-seen object is, by definition, never inside any window. If your trigger's resource has no auto-detectable revision field, either set `dedupKey` to a CEL expression, adjust `dedupWindow` for same-object flapping, or rely on `run.maxConcurrentRuns` as a volume backstop — see "Guarding against runaway triggers" below.

### Guarding against runaway triggers

A trigger with no filter can, in principle, match resources that its own WorkflowRuns create — turning every run into more matching events, without bound. OttoFlow closes the two known cases automatically:

- **Runner Pods/Jobs.** The Jobs and Pods that the WorkflowRun controller creates carry the `ottoflow.nirmata.io/workflowrun` label (`internal/workflow/controller/workflowrun_controller.go`). Each event watch adds a `!ottoflow.nirmata.io/workflowrun` label selector so these are filtered out server-side, with an in-controller guard as a backstop (`internal/workflow/controller/trigger_manager.go`: `runnerManagedLabel`). This prevents a Pod- or Job-scoped trigger from firing on the runs it creates.
- **The Workflow's own WorkflowRuns.** A trigger watching `kind: WorkflowRun` directly isn't covered by the label guard above (WorkflowRuns don't carry that label). Instead, an event on a WorkflowRun whose owner reference points back at the triggering Workflow is dropped in-controller, so a Workflow can't re-trigger itself off the runs it created.

Neither guard is a substitute for good trigger design on resources OttoFlow doesn't manage: scope `labelSelector`/`fieldSelector`/`namespace` to the workload you actually intend to watch, and set `run.maxConcurrentRuns` on the Workflow as a hard ceiling on concurrent runs regardless of trigger volume.

### Event trigger metadata

WorkflowRuns created by event triggers include resource details:

```bash
kubectl get workflowrun <name> -o jsonpath='{.status.trigger}' | jq .
```

```json
{
  "type": "Event",
  "triggeredAt": "2026-03-03T10:15:32Z",
  "eventResource": {
    "apiVersion": "v1",
    "kind": "Pod",
    "name": "my-pod",
    "namespace": "default"
  }
}
```

## GitOps Triggers (ArgoCD and FluxCD)

OttoFlow's event trigger natively supports GitOps controllers. No separate notification pipeline is required — OttoFlow watches the controller's CRDs directly and deduplicates by revision automatically.

### ArgoCD: trigger on application sync

```yaml
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: argocd-sync-policy-check
  namespace: ottoflow
spec:
  inputs:
    - name: appName
    - name: appNamespace

  triggers:
    - event:
        resources:
          - apiVersion: argoproj.io/v1alpha1
            kind: Application
            namespace: argocd
        operations:
          - UPDATE
        celFilter: 'object.status.sync.status == "Synced"'
        inputMapping:
          appName: "object.metadata.name"
          appNamespace: "object.spec.destination.namespace"
        # Deduplication by status.sync.revision is automatic — no dedupKey needed.

  steps:
    - name: runPolicyCheck
      agentRef:
        name: policy-agent
      inputs:
        prompt: |
          Check Kyverno policy compliance for {{ inputs.appName }}
          in namespace {{ inputs.appNamespace }}.
```

**RBAC:** The OttoFlow controller needs `get`/`list`/`watch` on `argoproj.io/applications` in the ArgoCD namespace. Use `ottoflow generate rbac` to generate the required ClusterRole.

### FluxCD: trigger on Kustomization reconciliation

```yaml
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: fluxcd-compliance-check
  namespace: ottoflow
spec:
  inputs:
    - name: kustomizationName
    - name: appliedRevision

  triggers:
    - event:
        resources:
          - apiVersion: kustomize.toolkit.fluxcd.io/v1
            kind: Kustomization
            namespace: flux-system
        operations:
          - UPDATE
        celFilter: 'object.status.conditions.exists(c, c.type == "Ready" && c.status == "True")'
        inputMapping:
          kustomizationName: "object.metadata.name"
          appliedRevision: "object.status.lastAppliedRevision"
        # Deduplication by status.lastAppliedRevision is automatic.
```

For `HelmRelease`, use `apiVersion: helm.toolkit.fluxcd.io/v2` and change the `celFilter` condition accordingly. Deduplication is automatic via `status.lastAttemptedRevision`.

### Why not fieldSelector?

You might expect `fieldSelector: "status.sync.status=Synced"` to filter events server-side. This doesn't work for CRD status fields. The Kubernetes API server only maintains field selector indexes for fields that are explicitly registered — for custom resources this means `metadata.name` and `metadata.namespace` only. All other fields, including every field under `status.*`, are silently ignored or rejected. Use `celFilter` instead.

## Webhook Triggers

Webhook triggers allow external systems (GitHub Actions, CI/CD pipelines, Slack bots, SaaS control planes) to fire WorkflowRuns via a signed HTTP POST. No Kubernetes RBAC access is required by the caller.

### Endpoint

```
POST /webhooks/{namespace}/{workflowName}
Content-Type: application/json
X-OttoFlow-Timestamp: <unix-epoch-seconds>
X-OttoFlow-Signature: sha256=<hex>
```

The endpoint listens on port **`:8083`** of the controller pod. The Helm chart creates a `ClusterIP` Service (`<release>-webhook-trigger`) for this port automatically when `controller.webhookTrigger.service.enabled=true` (the default). Route it via an `Ingress` or `LoadBalancer` to make it reachable externally.

> **TLS is required in production.** The server speaks plain HTTP; encrypt with an ingress controller or service mesh sidecar (standard Kubernetes practice).

### Create the HMAC secret

```bash
# 32 bytes minimum (256-bit key per NIST SP 800-107 for HMAC-SHA256)
kubectl create secret generic github-webhook-secret \
  --namespace=ottoflow \
  --from-literal=hmac-key="$(openssl rand -base64 32)"
```

### Workflow definition

```yaml
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: compliance-scan
  namespace: ottoflow
spec:
  triggers:
    - webhook:
        secretRef:
          name: github-webhook-secret   # key defaults to "hmac-key"
        celFilter: 'object.data.severity == "high"'   # optional — skip low-severity events
        inputMapping:
          namespace: object.data.namespace             # CEL on parsed JSON body
          clusterId: object.data.cluster_id
        dedupKey: object.data.run_id                  # optional dedup by payload field
        dedupWindow: 10m                              # max 1h
  inputs:
    - name: namespace
    - name: clusterId
  steps:
    - name: runScan
      expressions:
        - name: result
          expression: '"scanning " + inputs.namespace'
```

### Signing requests

```bash
TIMESTAMP=$(date +%s)
WEBHOOK_PATH="/webhooks/ottoflow/compliance-scan"
BODY='{"data":{"namespace":"production","cluster_id":"us-east","severity":"high","run_id":"abc-123"}}'

SIG=$(printf 'v1:%s:%s:%s' "${TIMESTAMP}" "${WEBHOOK_PATH}" "${BODY}" \
  | openssl dgst -sha256 -hmac "$OTTOFLOW_WEBHOOK_SECRET" \
  | awk '{print "sha256="$2}')

curl -s -X POST \
  -H "Content-Type: application/json" \
  -H "X-OttoFlow-Timestamp: ${TIMESTAMP}" \
  -H "X-OttoFlow-Signature: ${SIG}" \
  -d "${BODY}" \
  "https://ottoflow.example.com${WEBHOOK_PATH}"
# → 202 {"runName":"compliance-scan-a1b2-3c4d5e6f","namespace":"ottoflow","status":"Pending"}
```

**Signed string:** `v1:{timestamp}:{path}:{body}` — the path prevents cross-endpoint replay when two workflows share a secret.

### Response codes

| Code | Meaning |
|------|---------|
| `202 Accepted` | WorkflowRun created |
| `200 OK` | Acknowledged, no run (filtered or deduped) |
| `400 Bad Request` | Invalid JSON or CEL expression error |
| `401 Unauthorized` | Bad HMAC, stale timestamp, or unknown workflow |
| `413 Payload Too Large` | Body exceeds 1 MiB |
| `429 Too Many Requests` | Rate limit or MaxConcurrentRuns exceeded |
| `500 Internal Server Error` | K8s API error — safe to retry |

### Deduplication semantics

- **`dedupKey` + `dedupWindow`**: same key value within the window → drop (200 deduped)
- **`dedupKey` only**: `dedupWindow` defaults to 10 minutes, so the same key value within 10 minutes → drop (`internal/workflow/controller/trigger_manager.go`: `defaultDedupWindow`)
- **`dedupWindow` only**: any request within the window after the first → drop (time-window only)
- **Neither**: every request creates a run

Maximum `dedupWindow` is 1 hour (enforced at admission). Dedup state is in-process: after a controller restart, the new leader starts with empty state.

### Rate limiting

Each workflow gets a per-instance rate limiter. Defaults: 60 requests/minute, burst of 10. Override on the webhook trigger itself:

```yaml
triggers:
  - webhook:
      secretRef:
        name: github-webhook-secret
      rateLimit:
        requestsPerMinute: 120
        burst: 20
```

### Secret rotation

Update the Kubernetes Secret value. The controller fetches the secret on every request — no restart needed.

### High Availability note

The webhook server runs only on the elected leader (enforced by `NeedLeaderElection()=true`). Non-leader replicas do not open port 8083, so requests routed to them will get a connection refused.

**Service routing:** The `<release>-webhook-trigger` Service uses label selectors that match all controller pods (leader and non-leaders). To route only to the leader, configure your Ingress or load balancer to use health checks against `:8083/healthz` — only the leader pod will respond. Alternatively, set `replicaCount: 1` if HA for the webhook trigger is not required.

> **Why not use the readiness probe for routing?** Gating pod readiness on `:8083/healthz` would make non-leader pods permanently NotReady, removing them from **all** Services including the validating webhook Service on port 9443. This breaks Kubernetes admission during leader transitions. The controller's standard readiness probe (`/readyz` on `:8081`) reflects actual controller readiness and is not affected by leader election.

### Webhook trigger metadata

```bash
kubectl get workflowrun <name> -o jsonpath='{.status.trigger}' | jq .
```

```json
{
  "type": "Webhook",
  "triggeredAt": "2026-06-15T14:22:00Z",
  "webhookRequest": {
    "remoteAddr": "10.0.1.42:54321",
    "requestId": "a1b2c3d4e5f6g7h8"
  }
}
```

---

## Multiple Triggers

A single workflow can have multiple triggers. Any trigger firing creates a WorkflowRun (OR logic):

```yaml
triggers:
  - cron:
      schedule: "0 */6 * * *"
      concurrencyPolicy: "Forbid"
  - event:
      resources:
        - apiVersion: v1
          kind: ConfigMap
          namespace: default
      operations:
        - CREATE
        - UPDATE
      labelSelector:
        matchLabels:
          managed-by: ottoflow
```

This workflow runs every 6 hours **and** whenever a matching ConfigMap is created or updated.

## Labels and cleanup

All trigger-created WorkflowRuns carry these labels:

| Label | Value |
|-------|-------|
| `ottoflow.nirmata.io/workflow` | Name of the source Workflow |
| `ottoflow.nirmata.io/trigger` | `cron`, `event`, or `webhook` |
| `ottoflow.nirmata.io/managed-by` | `ottoflow-scheduler` (cron), `ottoflow-webhook-server` (webhook) |

Use these for listing and cleanup:

```bash
# List all cron-triggered runs for a workflow
kubectl get workflowruns -l ottoflow.nirmata.io/workflow=heartbeat,ottoflow.nirmata.io/trigger=cron

# Delete all completed runs for a workflow
kubectl delete workflowruns -l ottoflow.nirmata.io/workflow=heartbeat
```

For automatic cleanup, use the Workflow `spec.run.retentionMinutes` and `spec.run.maxAllowed` fields to limit how many completed runs are kept. By default, completed runs are kept for 7 days and at most 50 are retained; set either field to `0` to disable that limit.

## Samples

See `samples/workflows/features/` for complete examples:

- `cron-trigger.yaml` — Simple heartbeat with Forbid policy
- `cron-trigger-timezone.yaml` — Timezone-aware daily schedule
- `cron-trigger-replace.yaml` — Replace policy for periodic scans
- `event-trigger.yaml` — Pod creation monitor
- `complex-triggers.yaml` — Combined cron + event triggers
- `argocd-event-trigger.yaml` — GitOps trigger: ArgoCD Application sync
- `fluxcd-event-trigger.yaml` — GitOps trigger: FluxCD Kustomization reconciliation
- `github-actions-webhook.yaml` — Webhook trigger fired from GitHub Actions
