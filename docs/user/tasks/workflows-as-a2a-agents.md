# Serving Workflows as A2A Agents

OttoFlow can expose a Workflow to kagent as its own **A2A** (agent-to-agent) agent,
callable from kagent's UI or any other A2A client. This is the inbound direction:
a Workflow step can also call *out* to an external agent over A2A via
`externalAgentRef` — see the [Workflow API reference](../reference/api/workflow.md)
for that. Both can be on at once; they share nothing but the protocol.

This is the A2A counterpart to
[Serving Workflows as MCP Tools](workflows-as-mcp-tools.md): same idea — a Workflow
opts in and becomes callable from an agent framework — different protocol and a
different exposure mechanism (kagent Agent object instead of a shared HTTP endpoint).

## Opting a workflow in

Exposure is per workflow. A workflow is not an agent until it says so:

```yaml
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: namespace-report
spec:
  expose:
    kagent:
      displayName: "Namespace Report"
      description: >-
        Summarize the workloads running in a Kubernetes namespace: how many pods,
        which are unhealthy, and what images they run.
      examples:
        - "what's running in default?"
      tags:
        - reporting
```

`displayName`, `description`, `examples`, and `tags` populate the A2A agent card kagent
shows a user or model choosing between agents.

## What happens when a workflow opts in

The controller's exposure reconciler watches for Workflows with `spec.expose.kagent`
set and, for each one, creates a kagent BYO (bring-your-own) `Agent` in the Workflow's
namespace. kagent runs that Agent as a small per-workflow `serve-a2a` server, which
turns each A2A `message/stream` call into a `WorkflowRun` and streams the run's
progress and output back as A2A events — the same run-and-report shape kagent's UI
already expects from any other agent.

Editing or removing `spec.expose.kagent` updates or tears down the Agent on the next
reconcile; deleting the Workflow removes it via a finalizer.

## Enabling it

```bash
helm upgrade --install ottoflow ./charts/ottoflow \
  --namespace ottoflow --create-namespace \
  --set serveA2A.enabled=true
```

This is off by default, and needs two things to actually run (plus the caller setup in
[Securing A2A callers](#securing-a2a-callers) before anything can call a Workflow):

1. **`serveA2A.enabled=true`** — without it, the controller does not create kagent
   Agents for any Workflow, even one with `spec.expose.kagent` set. Enabling it also
   makes the chart create the shared serve-a2a `ClusterRole` that every per-workflow
   Agent's ServiceAccount binds to.
2. **A `serve-a2a` image.** It publishes starting with the next OttoFlow release tag.
   Only set `serveA2A.enabled: true` once that image exists for the version you're
   deploying, or point `serveA2A.image.fullOverride` (in `values.yaml`) at your own
   build in the meantime.

## Securing A2A callers

serve-a2a authenticates every request except `GET /.well-known/agent-card.json` (kagent's
readiness probe fetches the card, so it stays public). A caller must present
`Authorization: Bearer <token>` where:

1. the token is a ServiceAccount token minted for the audience **`ottoflow-a2a`** (checked with
   a Kubernetes TokenReview; the audience is fixed and not configurable), and
2. that ServiceAccount may `invoke` `callers.gate.ottoflow.nirmata.io/a2a-caller` in the **Workflow's
   namespace** (checked with a SubjectAccessReview; nothing serves this resource).

Any other request gets a bare `401 Unauthorized`; the reason is logged by serve-a2a at verbosity 4.
A token minted with the default audience is rejected. On a cluster that issues legacy
Secret-based ServiceAccount tokens, those are rejected too.

The chart (with `serveA2A.enabled` and `rbac.create`) creates two ClusterRoles but binds neither,
because the ServiceAccounts live in your namespaces. `<fullname>` is the chart's `ottoflow.fullname`:
the release name if it contains `ottoflow`, otherwise `<release>-ottoflow` (for release `myrel`,
`myrel-ottoflow`), unless `controller.fullnameOverride` is set:

| ClusterRole | Grants | Bind it to | With |
|---|---|---|---|
| `<fullname>-a2a-token-reviewer` | `create` `tokenreviews` and `subjectaccessreviews` | the `serve-a2a` ServiceAccount (`serveA2A.serviceAccountName`) in **each namespace that exposes a Workflow** | a **ClusterRoleBinding** (both resources are cluster-scoped; a RoleBinding grants nothing) |
| `<fullname>-a2a-caller` | `invoke` `callers.gate.ottoflow.nirmata.io/a2a-caller` | each caller's ServiceAccount | a **RoleBinding in the Workflow's namespace** |

```bash
# once per namespace that exposes a Workflow
kubectl create clusterrolebinding a2a-token-reviewer-<ns> \
  --clusterrole=<fullname>-a2a-token-reviewer \
  --serviceaccount=<ns>:serve-a2a

# once per caller, in the Workflow's namespace
kubectl -n <ns> create rolebinding a2a-caller-<caller> \
  --clusterrole=<fullname>-a2a-caller \
  --serviceaccount=<caller-ns>:<caller-sa>

# mint a token for the caller
kubectl -n <caller-ns> create token <caller-sa> --audience=ottoflow-a2a --duration=<d>
```

Creating the `a2a-caller-<caller>` RoleBinding above needs `bind` on `<fullname>-a2a-caller` (or `invoke` on `callers.gate.ottoflow.nirmata.io`). A namespace `admin` holds neither, so a cluster administrator creates the binding or grants that `bind` to the namespace admin.

If the `serve-a2a` ServiceAccount lacks the token-reviewer binding, every call is refused with
401. serve-a2a runs a SelfSubjectAccessReview at startup and logs an error naming the missing
permission; check the pod log. Readiness is unaffected.

The maximum `--duration` is capped by the cluster (`--service-account-max-token-expiration`);
that cap is cluster-specific and not checked here.

### Calling from a kagent Agent (tested on the Go runtime only)

In kagent v0.10.3, none of the following sends a Kubernetes-verifiable ServiceAccount token on
remote A2A calls: the UI, a declarative Agent on the Python runtime (the default), or one on the
Go runtime. Agent-to-agent calls go directly pod to pod (`http://<agent>.<ns>:8080`), not through
the kagent controller. The one credential path is a static header on the calling Agent's tool,
`tools[].headersFrom`. Store the token in a Secret **including the `Bearer ` prefix**, because
serve-a2a requires it:

```yaml
apiVersion: v1
kind: Secret
metadata: {name: ottoflow-a2a-token, namespace: <agent-ns>}
stringData:
  authorization: "Bearer <output of kubectl create token ... --audience=ottoflow-a2a>"
---
# on the calling Agent's remote-agent tool
headersFrom:
  - name: Authorization
    valueFrom:
      type: Secret
      name: ottoflow-a2a-token
      key: authorization
```

The `headersFrom` shape follows kagent v0.10.3's `ValueRef` (`name`, `value`, `valueFrom`). The
wiring above was tested on one cluster: single-node kind v0.31.0 (Kubernetes v1.35.0, Calico
v3.29.4), kagent v0.10.3 in unsecure mode, a declarative calling Agent on the Go runtime, and a
mock LLM, with the bindings above in place. With a token minted for `ottoflow-a2a` in the Secret,
the call created a WorkflowRun. With a token minted for another audience (after restarting the
calling Agent pod), or with `headersFrom` removed, the calling Agent got 401 and no run was created.
The Python runtime, a real LLM and kagent's trusted-proxy mode were **not tested**.

**After rotating the token Secret, restart the calling Agent pod** (in that test kagent named the
Deployment after the Agent: `kubectl -n <agent-ns> rollout restart deployment/<agent-name>`). In a
single observation, a call made seconds after the Secret changed still created a run, so the running
pod appears to have kept the earlier token (the header it sent was not captured); after a restart
the call got 401, as expected with the new value. Whether kagent picks up a Secret change on its
own later was not checked.

Go ADK code requests `/.well-known/agent.json` while serve-a2a serves only
`/.well-known/agent-card.json`, so that fetch gets 405 today and 401 after this change, independent
of authentication. That did not stop the Go-runtime caller in the test above; whether it fetched
the card at all was not checked. Not verified for the Python runtime.

### The opt-out: `serveA2A.allowUnauthenticated`

`--set serveA2A.allowUnauthenticated=true` turns authentication off again (it sets
`A2A_ALLOW_UNAUTHENTICATED=true` on each serve-a2a pod, which logs a warning at startup). It is
**off by default** and is meant as a stopgap.

What it re-opens, by kagent mode (kagent v0.10.3):

| kagent mode | With authentication on (default) | With the opt-out |
|---|---|---|
| unsecure (kagent's default `--auth-mode`) | UI chat gets 401 (the UI sends no Authorization header). Agent to workflow works only with the `headersFrom` token. | Works. **Any pod that can reach the serve-a2a pod, directly or through the kagent controller's A2A proxy, can create runs without credentials.** |
| trusted-proxy | UI chat gets 401 (the forwarded user JWT is not a ServiceAccount token for the `ottoflow-a2a` audience). Agents as above. | Works. **Any pod that can reach the serve-a2a pod can create runs without credentials**, as in unsecure mode. |
| `KAGENT_PROPAGATE_TOKEN=true` | No change: it forwards the inbound header, which is the UI's (none, or a user JWT). | No change. |
| OttoFlow `externalAgentRef` calling an exposed Workflow | 401: the runner sends a bearer token only over https, and serve-a2a serves plain HTTP. | Works. |

**For kagent users who need the UI chat, this opt-out is the likely mode, not an edge case.**
In every kagent mode the opt-out leaves serve-a2a reachable without credentials by any pod that can
reach it. In unsecure mode the kagent controller's unauthenticated A2A
proxy is one such path; whether trusted-proxy mode's proxy path adds exposure was not tested.

Restrict who can reach the pods with a NetworkPolicy. **Tested on Calico only** (v3.29.4, on the
single-node kind cluster above, with `serveA2A.allowUnauthenticated=true`); other CNIs were not
tested. Labels are taken from kagent v0.10.3's chart and manifest builder. It must admit the kagent
controller and every calling Agent pod, because agent calls bypass the controller:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: serve-a2a-allow-kagent, namespace: <workflow-ns>}
spec:
  podSelector:
    matchLabels: {app: kagent}          # serve-a2a pods; add your Agent's name label to narrow
  policyTypes: [Ingress]
  ingress:
    - from:
        - namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: <kagent-ns>}}
          podSelector:
            matchLabels:
              app.kubernetes.io/name: kagent
              app.kubernetes.io/instance: <kagent-release>
              app.kubernetes.io/component: controller
        - podSelector: {matchLabels: {app: kagent}}   # calling kagent Agent pods in this namespace
```

In that test the example was applied with only the three placeholders filled in. It admitted the
kagent controller pod, the calling Agent pod and a test pod labelled `app=kagent` (each created a
run without credentials), and blocked pods in two other namespaces and a pod without `app=kagent`
in the Workflow's namespace (the connection timed out and no run was created). Only requests sent
straight to the serve-a2a Service on port 8080 were tested, not the kagent controller's A2A proxy
path.

Every pod the policy admits can still create runs without credentials. **`app: kagent` matches
every pod in `<workflow-ns>` that carries that label**, not only serve-a2a: in the test the
serve-a2a pod and the calling Agent pod both carried `app=kagent` (and `kagent=<agent-name>`), so
`spec.podSelector` selected both, and the `from` peer `podSelector: {matchLabels: {app: kagent}}`
admitted both labelled pods tried in `<workflow-ns>`, including a test pod that kagent did not
create. To narrow it, add `kagent: <agent-name>` to these selectors: the exposed Workflow's Agent
in `spec.podSelector` (the comment on `spec.podSelector` suggests the same), and one `from` peer per
calling Agent. **Narrowing with `kagent: <agent-name>` is untested.**

### Limits that apply either way

- **Concurrency cap.** A Workflow with no `spec.run.maxConcurrentRuns` allows 10 active runs
  (matching the webhook trigger burst); an explicit `0` means unlimited and `N` means N. The same
  rule applies to serve-a2a, cron, event, webhook and MCP runs, and all of them count against it.
  Each serve-a2a pod enforces the cap on its own, so it is best-effort across replicas and across
  runs created by cron, event, webhook or MCP.
- `serve-a2a` sets `ReadHeaderTimeout` to 10 s and gives TokenReview and SubjectAccessReview
  one shared 10 s deadline (not 10 s each).
- Plain HTTP, no TLS: a bearer token crosses the pod network in cleartext. The dedicated audience
  limits what a sniffed token is worth (it is not an apiserver credential); use a service mesh for
  wire encryption.

### Upgrading from an unauthenticated release

Affected releases: v0.1.1, v0.1.2 and v5.0.0-rc.1. They publish a serve-a2a image that accepts unauthenticated calls.

On upgrade, callers get 401 until configured: kagent UI chat (every kagent mode), kagent Agents
calling an exposed Workflow, and `externalAgentRef` calls to one. To restore them:

1. **Agents (tested on kind in two separate runs, not in one upgrade end to end):** the bindings
   and token worked for a direct call after upgrading from a local build of an earlier
   unauthenticated release to an earlier build of this change, and the `headersFrom` step worked
   on a fresh install with a Go-runtime calling Agent. Create the token-reviewer ClusterRoleBinding for each exposed namespace,
   bind `a2a-caller` to each calling ServiceAccount, mint a token with `--audience=ottoflow-a2a`,
   and reference it through `headersFrom` as above.
2. **UI chat:** set `serveA2A.allowUnauthenticated=true` plus the NetworkPolicy, accepting the open
   controller-proxy path.

Rollout: existing serve-a2a pods pick up the new image when the controller re-upserts the Agent, at
controller start or within 10 minutes. A pinned `serveA2A.image.tag` keeps the old, unauthenticated
image. Exposed Workflows with no `maxConcurrentRuns` start being capped at 10.

## Samples

A complete set of sample Workflows exposed this way — including a no-LLM proof of the
plumbing, a workflow that delegates to an external agent over A2A, and one that does
its own LLM step — is in
[`samples/kagent-workflows-as-agents/`](../../../samples/kagent-workflows-as-agents/).

## Limits

- **One Agent per exposed Workflow.** There is no batching of multiple Workflows
  behind a single kagent Agent, unlike the MCP-tools path where every opted-in
  Workflow shares one endpoint.
- **Namespace-scoped RBAC.** The per-workflow Agent's ServiceAccount is bound to the
  shared serve-a2a `ClusterRole` via a RoleBinding created in the Workflow's own
  namespace, not cluster-wide.
