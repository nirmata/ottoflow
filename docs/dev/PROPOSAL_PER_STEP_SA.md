# Proposal: Per-step Service Accounts — Least-Privilege Identity for Each Step

**Status**: Draft  
**Issue**: Tracked internally

## Problem

All steps in a WorkflowRun execute under a single Kubernetes identity: the runner Job's
ServiceAccount. A workflow that queries read-only cluster state, calls an external MCP tool,
and then mutates resources runs all three operations with the same credential — violating
least-privilege and producing ambiguous audit logs where all Kubernetes API calls appear under
one identity regardless of what they were doing.

The concrete failure modes are:

1. **Over-privilege**: A step that only needs to list Pods carries the same RBAC rights as a
   step that patches Deployments. A bug or injected prompt in the read-only step can perform
   write operations it was never intended to perform.

2. **Ambiguous audit trail**: Kubernetes audit logs record `serviceAccountName:
   my-workflow-runner` for every API call, making it impossible to attribute which step
   triggered which operation in a post-incident review.

3. **Blast radius on credential compromise**: Compromising the runner Job (e.g., via a
   malicious MCP tool response) exposes the full RBAC scope of the runner SA for the entire
   workflow duration. A per-step SA whose token expires 5 minutes after the step completes
   limits exposure to the step's window.

`DEVELOPER.md` lists "Per-step security (service accounts)" under **Planned** in its Status &
Roadmap section.

## Options Considered

### Option A: Token-per-step via TokenRequest API (recommended)

Before each step with a `serviceAccountRef`, the runner mints a short-lived token against the
specified ServiceAccount using the Kubernetes TokenRequest API (`POST
/api/v1/namespaces/{ns}/serviceaccounts/{name}/token`). The token is used to build a scoped
`client.Client` and a new `CELEvaluator` (so that `resource.get` / `resource.list` CEL macros
also use the per-step identity). The step executes on a throwaway child executor. The token
expires 5 minutes after minting and is never stored beyond the step's execution.

The runner pod's SA — which already has `create` on `serviceaccounts` — additionally needs
`create` on the `serviceaccounts/token` subresource.

**Pros**: Tokens are minimal-exposure and time-bounded. Works with conditional steps (skipped
steps never mint tokens). Tokens expire 5 minutes after minting, and because they are bound to
the runner pod (`BoundObjectRef`) they are also invalidated as soon as that pod is deleted —
note this is pod deletion, not Job completion, which does not necessarily delete the pod
immediately. Full identity isolation: all Kubernetes calls within the step, including CEL
macros, use the per-step SA.  
**Cons**: Requires adding `targetRESTConfig *rest.Config` to `WorkflowExecutor` so the minted
bearer token can be used to construct a new scoped `client.Client`.  
**Decision**: Recommended — not yet implemented (design below).

---

### Option B: Pre-mint all tokens at workflow start

Scan all steps at workflow initialization, mint tokens for every step with a
`serviceAccountRef`, and store a `map[stepName]*client.Client` for use during dispatch.

**Pros**: Token minting logic is centralized in one place; dispatch loop has no per-step
overhead.  
**Cons**: 5-minute TTL means tokens for late steps in long workflows expire before those steps
run. Tokens are also minted for steps that are subsequently skipped via `matchConditions` —
wasting credentials and generating unnecessary audit events. For forEach steps, the number of
items is not known at initialization time.  
**Decision**: Rejected. TTL expiry on long workflows is a silent correctness failure. Minting
tokens for skipped steps violates least-privilege intent.

---

### Option C: ServiceAccount impersonation via rest.Config

Set `rest.Config.Impersonate` with the SA's identity instead of minting a token. The runner SA
would need the `impersonate` verb on `serviceaccounts`.

**Pros**: No token lifecycle to manage; no RBAC change to `serviceaccounts/token`.  
**Cons**: `impersonate` is a broader privilege than `serviceaccounts/token` — it grants the
ability to act as *any* SA, not just a named one. Enterprise clusters commonly block
impersonation via OPA/Kyverno policies. Kubernetes audit logs for impersonated requests are
harder to attribute clearly than TokenRequest-minted tokens. Not what the issue specified.  
**Decision**: Rejected. Impersonation's policy incompatibility and broader blast radius make it
worse than Option A on every security dimension.

---

## Implementation (Option A)

### CRD changes

`serviceAccountRef` is added as an optional `string` field on `Step`. It holds the name of a
pre-existing ServiceAccount in the WorkflowRun's namespace (no namespace qualifier — the step's
namespace is always the WorkflowRun namespace). An optional `defaultServiceAccount` string is
added to `WorkflowRunExecutionSpec`; it applies to steps that do not declare their own
`serviceAccountRef`.

```yaml
# Step-level override
steps:
  - name: queryNamespaces
    serviceAccountRef: ottoflow-reader
    resourceQuery:
      apiVersion: v1
      resource: Namespace

# Workflow-level default
spec:
  execution:
    defaultServiceAccount: ottoflow-reader
```

### Token minting

The runner calls `targetClient.SubResource("token").Create(ctx, sa, tokenRequest)` — no new
typed Kubernetes clientset is required. The minted bearer token is placed in a shallow copy of
`targetRESTConfig` (stored as a new field on `WorkflowExecutor`) and used to construct a scoped
`client.Client`. A new `CELEvaluator` is built from this scoped client so that CEL macro calls
(`resource.get`, `resource.list`) within the step also execute under the per-step identity.

For local clusters (no `ClusterRef`), the token is bound to the runner pod via
`BoundObjectRef` — it is invalidated automatically when the Job ends. For remote clusters
(kubeconfig via `ClusterRef`), `BoundObjectRef` is omitted (the pod does not exist on the remote
cluster) and expiry relies on `ExpirationSeconds: 300`.

### ForEach

ForEach items run concurrently in goroutines on the same `WorkflowExecutor`. Swapping a shared
`e.client` field is a race condition. The solution: mint the token **once** before
`processItemsConcurrently` begins, then give each item goroutine its own child executor (via an
extension of `newChildExecutor`) that shares the per-step `client.Client` but has its own
`CELEvaluator` and `macroContextHolder`. The `client.Client` itself is safe to share across
goroutines (it is an http.Transport pool). ForEach items inherit the forEach step's
`serviceAccountRef` — there is no per-item SA field.

### RBAC

A `create serviceaccounts/token` rule is added to
`charts/ottoflow/templates/clusterrole.yaml`, scoped to the `""` (core) API group and
`serviceaccounts/token` subresource.

### Backward compatibility

Steps without `serviceAccountRef` and no workflow-level `defaultServiceAccount` continue to
use the pod's mounted SA token exactly as before. No behavioral change, no new required fields.

## Files to Change

| File | Change |
|---|---|
| `api/v1alpha1/workflow_types.go` | Add `ServiceAccountRef` to `Step` |
| `api/v1alpha1/workflowrun_types.go` | Add `DefaultServiceAccount` to `WorkflowRunExecutionSpec` (defined here; `workflow_types.go` only references the type) |
| `api/v1alpha1/zz_generated.deepcopy.go` | Auto-updated via `make generate` |
| `internal/workflow/executor/executor.go` | Add `targetRESTConfig` field; wire per-step executor creation |
| `internal/workflow/executor/step_identity.go` | New — `mintStepToken`, `buildScopedClient`, `newStepExecutor` |
| `internal/workflow/executor/foreach_executor.go` | Mint token once before items; pass per-step client to child executors |
| `charts/ottoflow/templates/clusterrole.yaml` | Add `create serviceaccounts/token` rule |
| `config/crd/bases/` + `charts/ottoflow/crds/` | Auto-updated via `make manifests` |
| `cmd/workflow-runner/main.go` | Pass `targetRESTConfig` to `NewWorkflowExecutor` |
