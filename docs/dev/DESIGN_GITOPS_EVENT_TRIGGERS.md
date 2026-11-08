# Design: GitOps Event Triggers — CEL inputMapping, celFilter, and Revision-Based Deduplication

**Date**: June 2026  
**Status**: Implemented (v0.7.0)

---

## Problem

OttoFlow's `EventTrigger` could structurally watch Kubernetes resources, but three gaps made it unusable for GitOps integration:

1. **`inputMapping` was a stub** — the implementation stored raw CEL expression strings as input values instead of evaluating them. A workflow triggered by an ArgoCD sync received `"object.metadata.name"` (the literal string) instead of `"my-app"` (the app name). Inputs were always wrong.

2. **No client-side event filter** — Kubernetes `fieldSelector` cannot filter on CRD status fields; the API server only indexes `metadata.name` and `metadata.namespace` for custom resources. Without an in-process filter, every single UPDATE event on a watched resource (label changes, health updates, annotation patches) would create a WorkflowRun. A busy ArgoCD installation generates hundreds of Application updates per hour.

3. **Burst deduplication missing** — a single ArgoCD sync produces multiple rapid MODIFIED watch events as the Application object moves through state transitions. Without deduplication, one deployment created N WorkflowRuns — one per event in the burst.

---

## Design

### Three-Gate Model

Every incoming watch event passes through three sequential gates before a WorkflowRun is created:

```
Watch event arrives
       │
       ▼
┌─────────────────┐
│  Gate 1         │  celFilter — does this event matter?
│  (drop noise)   │  e.g. object.status.sync.status == "Synced"
└────────┬────────┘
         │ pass
         ▼
┌─────────────────┐
│  Gate 2         │  inputMapping — extract workflow inputs via CEL
│  (extract data) │  e.g. object.metadata.name → inputs.appName
└────────┬────────┘
         │
         ▼
┌─────────────────┐
│  Gate 3         │  deduplication — is this a genuinely new deployment?
│  (dedup)        │  same revision → drop; new revision → proceed
└────────┬────────┘
         │ new deployment
         ▼
    WorkflowRun created
    with real input values
```

### Gate 1: celFilter

A CEL expression evaluated in-process against the event object before any WorkflowRun is created. Must return `bool`. Events where the expression returns `false` or errors are dropped.

**Why not `fieldSelector`?** The Kubernetes API server maintains field selector indexes only for fields it explicitly registers. For CRDs this is `metadata.name` and `metadata.namespace` by default. Status fields (`status.sync.status`, `status.conditions`) are not indexed — the watch call would either reject the selector or return all events unfiltered. `celFilter` runs in the controller process after events arrive, so it can inspect any field.

**Available variable:** `object` — the full triggering resource as `map[string]interface{}` from `unstructured.Unstructured.Object`.

```yaml
celFilter: 'object.status.sync.status == "Synced"'
celFilter: 'object.status.conditions.exists(c, c.type == "Ready" && c.status == "True")'
```

### Gate 2: inputMapping

CEL expressions evaluated against the event object. Each key is a workflow input name; each value is a CEL expression whose result is coerced to string and passed as the input value.

**Why CEL?** OttoFlow already uses CEL uniformly for step expressions, outputs, and `matchConditions`. Using a separate path syntax (JSONPath, dot-paths) for `inputMapping` would introduce a second query language for the same `object` variable users already know. CEL also handles computed values:

```yaml
inputMapping:
  appName:    "object.metadata.name"                           # simple field
  identity:   "object.metadata.namespace + '/' + object.metadata.name"  # concat
  replicas:   "string(object.status.replicas)"                 # type coercion
```

### Gate 3: Deduplication

Prevents multiple WorkflowRuns from being created for the same logical deployment event.

**Revision-based (primary, auto-detected):** The controller probes well-known revision fields on the event object in priority order. If the same object fires again with the same revision value, the event is dropped. When the revision changes, a new WorkflowRun is created regardless of timing.

| Controller | Auto-detected revision field |
|------------|------------------------------|
| ArgoCD `Application` | `status.sync.revision` |
| FluxCD `Kustomization` | `status.lastAppliedRevision` |
| FluxCD `HelmRelease` | `status.lastAttemptedRevision` |
| FluxCD `OCIRepository` / `GitRepository` | `status.artifact.revision` |

**Time-window fallback (`dedupWindow`):** For resources with no recognizable revision field, a configurable duration gates how soon another WorkflowRun can be created for the same object after the last successful creation.

**Explicit override (`dedupKey`):** A CEL expression that computes the dedup key for controllers not in the built-in list. Example for Rancher Fleet: `dedupKey: "object.status.commit"`.

**Why revision-based over time-window?** A time window is heuristic — too short and burst events create duplicates; too long and legitimate rapid re-deployments get silently dropped. A revision key is exact: same git SHA = same deployment = burst noise. Different SHA = genuinely new deployment = always fire, regardless of elapsed time.

**Dedup state scoping:** State is keyed by `(triggerKey, objectNamespace/objectName)`, not just the object identity. Two Workflows watching the same ArgoCD Application maintain independent dedup state and fire independently.

---

## API Changes

Three new optional fields on `EventTrigger`:

```go
// CELFilter is a CEL expression evaluated against the event object.
// Must return bool. False or error → event dropped.
// Available variable: object (the triggering resource as a dynamic map).
CELFilter string `json:"celFilter,omitempty"`

// DedupKey is a CEL expression override for the dedup key.
// Auto-detection covers ArgoCD and FluxCD; only set for other controllers.
DedupKey string `json:"dedupKey,omitempty"`

// DedupWindow is a time-based dedup fallback when no revision field is found.
DedupWindow *metav1.Duration `json:"dedupWindow,omitempty"`
```

---

## Implementation

### Trigger CEL Evaluator

A minimal `cel.Env` with only `object: DynType` declared — independent of the executor's full Kyverno/resource-macro stack. Trigger expressions do field access on event objects; they don't need Kubernetes macros, HTTP context, or Prometheus.

Programs are compiled once on first use and cached in a `sync.Map` on `TriggerManager` for the lifetime of the controller. Concurrent watch goroutines share the cache safely — `sync.Map.LoadOrStore` handles the rare race where two goroutines compile the same expression simultaneously.

### Resource Name Resolution

`watchResource` previously derived the GVR plural name as `strings.ToLower(Kind) + "s"` with manual special-cases for Pod/Service/Deployment. This worked for ArgoCD (`Application → applications`) and FluxCD (`Kustomization → kustomizations`) by coincidence.

Replaced with REST mapper lookup via `tm.client.RESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version)`, which returns the authoritative plural name from the API server's discovery endpoint. Falls back to naive pluralization only if the mapper call fails.

### WorkflowRun Naming

Names are generated as `{workflowName}-{uid4}-{time6}` where `uid4` is the first 4 chars of the triggering object's UID (for traceability) and `time6` is the lower 24 bits of the nanosecond timestamp formatted as 6 hex chars (for per-invocation uniqueness).

Previously: `{workflowName}-{uid8}` — stable per object, causing "already exists" collisions when a second deployment fired before the first WorkflowRun was cleaned up.

### Goroutine Leak Fix

`registerEventTrigger` previously stopped old watcher goroutines with an exact key match against `tm.stopChans`. Goroutines are stored under resource-specific keys (`eventKey + "-resource-N"`), so the exact match never hit — old goroutines accumulated on every Workflow re-reconcile.

Fixed by extracting `stopWatchersForKey(key)` — a prefix-match helper that was already correctly implemented inline in `unregisterEventTrigger` — and calling it from both sites.

---

## Example: ArgoCD Sync Trigger

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
          appName:      "object.metadata.name"
          appNamespace: "object.spec.destination.namespace"
        # No dedupKey or dedupWindow needed — status.sync.revision auto-detected
  steps:
    - name: runPolicyCheck
      # ... agent or expression steps using inputs.appName, inputs.appNamespace
```

This Workflow is in `ottoflow` and watches `argocd`, so `ottoflow` must be listed in the controller's `--event-trigger-allowed-source-namespaces`; without it admission rejects the Workflow, or the controller skips the resource if the webhook is off.

---

## Relationship to multi-cluster orchestration

Multi-cluster fan-out across GitOps-managed clusters, such as via a future `WorkflowRunGenerator` CRD, remains future work. The existing `WorkflowRun.spec.clusterRef` already supports targeting a single remote cluster.

This design is narrower: it makes the existing `EventTrigger` actually work for GitOps use cases by fixing inputMapping, adding filtering, and adding deduplication. The two designs are complementary — once `WorkflowRunGenerator` ships, it would benefit from the same `celFilter` and `inputMapping` primitives.
