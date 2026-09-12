# Granting Secret Access (Opt-In RBAC)

OttoFlow ships with **tenant and cluster-wide Secret access starting at zero** for every
component — the controller, the workflow-runner Jobs, and the agent-executor. No OttoFlow
ServiceAccount can read a Secret in a tenant namespace, or across the cluster, out of the
box. The one retained exception is the controller's own install-namespace TLS Role (see
"What is granted automatically" below): an unscoped `create` (resourceNames cannot scope
`create`) plus a name-scoped `get`/`update`/`delete` on the four cert Secrets the built-in
certificate manager owns — no `list` or `watch` anywhere, and confined to the install
namespace, never reaching a tenant namespace.

This is deliberate. A workflow engine that can read every Secret in the cluster is a
high-value target: one compromised step, one over-broad CEL expression, or one bug in
credential handling can exfiltrate every credential in reach. Shipping with no tenant or
cluster-wide Secret access keeps the blast radius of any such flaw off tenant credentials
entirely. You extend it, one named Secret at a time, only where a feature actually needs it.

## The principle: extend via new roles, never modify the shipped roles

This follows the same customization philosophy as Kyverno's RBAC: **the roles the chart
ships are least-privilege and are not meant to be edited. You grant additional access by
adding your own Role and RoleBinding.** Upgrades replace the shipped roles, so any grant you
added by editing them would be silently reverted — a grant in a *separate* Role survives.

OttoFlow's opt-in is deliberately **tighter** than the cluster-wide aggregation Kyverno uses
for its own customization examples:

- **Namespaced, not cluster-wide.** Access is a `Role` in a single namespace, never a
  `ClusterRole`. A grant for one team's namespace can never read another team's Secrets.
- **Named, not blanket.** Each grant lists the exact Secret names it covers
  (`resourceNames`), so it grants `get` on *those* Secrets and nothing else.

A cluster-wide, aggregated `secrets` grant (the shape Kyverno's examples use) would re-open
exactly the blast radius we removed. Keep grants namespaced and named.

## Keep workflow Secrets out of the install namespace

The opt-in model above protects a Secret by granting only a **named `get`**, and that protection
holds **everywhere, the install namespace included**. The controller's built-in TLS certificate
manager (so you don't need external cert-manager) does not need broad Secret access either: it
creates, fills, and rotates its four cert Secrets itself — `create` cannot be scoped by
resourceNames (the object doesn't exist yet at authorization time), so it is its own unrestricted
rule, but everything else (`get`, `update`, `delete`) is scoped by resourceNames to exactly those
four names — **no `list` or `watch` on Secrets anywhere**. So a Secret in the install namespace is
*not* readable by the cert-manager Role just for being there, and the unrestricted `create` grant
lets the Role mint only Secrets whose names it already computes deterministically — it is never
handed an operator-controlled name.

Even so, **keep workflow Secrets in a dedicated, non-install namespace** (a tenant namespace where
your WorkflowRuns live) as blast-radius hygiene: the install namespace is the control plane, and
the control plane should not double as a store for tenant credentials. Grant the named `get` Role
**there**. The manual `Role` + `RoleBinding` below (and the Helm shortcut that generates them) are
both meant to target that tenant namespace, not the install namespace.

> **This is enforced, not just recommended, for the Helm shortcut — whenever the shortcut is
> actually rendering a Role.** `rbac.secretAccess` entries that resolve to the install namespace
> (including an omitted `namespace`, which defaults to it) fail the chart render outright — see
> "The Helm shortcut" below. There is no escape hatch for that combination: an install-namespace
> credentials Secret must be moved to a tenant namespace before the chart will grant access to it
> while it is rendering that component's Role at all. That guard only fires when the component's
> Role would otherwise be rendered: `rbac.create=false` skips the controller's `rbac.secretAccess`
> entries (and the fail-fast guard on them) entirely, and `agentExecutor.enabled=false` does the
> same for `rbac.secretAccess.agentExecutor` — in either case the entries are silently omitted
> rather than checked. Turning off `rbac.create` (or `agentExecutor.enabled`) is not a supported
> way to place a workflow Secret in the install namespace; it just means the Helm shortcut isn't
> rendering anything for that component to check. If your `llmCredentialsSecret` currently lives
> in the install namespace, move it into the **same namespace as the WorkflowRuns that use it** —
> not just any tenant namespace. `spec.execution.llmCredentialsSecret.namespace` must be empty or
> equal to the WorkflowRun's own namespace (enforced at admission,
> `internal/webhook/workflowrun_validator.go`), and the cluster-wide default
> (`--workflow-runner-llm-credentials-secret`) has no namespace field at all — it always resolves
> in the run's own namespace. So a Secret shared across several tenant namespaces via the
> cluster-wide default needs a same-named copy in each of those namespaces, not one copy moved to
> a single namespace elsewhere. If you'd rather keep the Secret in the install namespace, the
> Helm shortcut cannot grant that (see above); hand-write the Role and RoleBinding there instead.

## Features that break by default, and how to turn each one on

Each of these reads a Secret and therefore does nothing until you grant access to the
specific Secret it uses. The Secret must live in the namespace the feature reads it from
(usually the WorkflowRun's own namespace; for cron and webhook triggers, the Workflow's
namespace; for the well-known LLM credentials Secret, the run's namespace unless overridden).

| Feature | What it reads | Symptom when denied |
| --- | --- | --- |
| Automatic LLM credential injection (`spec.execution.llmCredentialsSecret` / the well-known Secret) — an **unconditionally reachable Nirmata-provider agent step** (or `modelProvider` unset), and the run supplies none of `NIRMATA_LLM_TOKEN` / `NIRMATA_LLM_SERVICEACCOUNT_TOKEN` / `NIRMATA_LLM_APIKEY` itself | The named credentials Secret | The WorkflowRun **fails**, terminally, with the RBAC remediation naming the Secret, the namespaced Role to add, `rbac.secretAccess`, and a `kubectl auth can-i` check to confirm it. A `LLMCredentialsForbidden` Warning event is recorded too, but the run does not proceed — this workflow genuinely needed the credentials. There is no flag to opt out of this failure: the only remedies are granting the Role below, or removing the cluster-wide/per-run `llmCredentialsSecret` configuration so the feature has nothing to read. This read is authorization-gated: the controller asks the API server, via a `SelfSubjectAccessReview`, whether it is allowed to `get` this exact Secret *before* attempting the read. A denial reported by that check fails the run the same way, without ever attempting the read; the check being unavailable or inconclusive is not itself a denial — it simply defers to the read, which retries on its own (via the controller's normal backoff) if the authorizer is genuinely down. See "Telling which check caught a Symptom A denial" below for how to tell which check caught a denial. |
| Automatic LLM credential injection — **no agent step**, **or the agent step is behind `matchConditions`, has `failurePolicy: Continue`, or is inside a `forEach`** (none of those are guaranteed to run, so a denied read there can never be distinguished from one that simply never mattered), **or** the run supplies its own Nirmata token via `spec.execution.job.env` | The named credentials Secret | Unchanged: injection is skipped, a `LLMCredentialsForbidden` Warning event is recorded, and the run proceeds — the denied read cannot affect a run that has no guaranteed use for these credentials, or already has its own. |
| Automatic LLM credential injection — **any non-Nirmata provider** (`openai`, `anthropic`, `gemini`, `google`, `azure-openai`, `local`) | The named credentials Secret | Also unchanged: skipped with the Warning event, and this can never regress into a failure — the injected well-known Secret is not a credential source for these providers at all. Their API keys come from the **agent-executor's own process environment**, wired separately via the `agentExecutor.llmCredentialsSecret` Helm value (a distinct Secret, read by a distinct ServiceAccount); see `api/v1alpha1/agent_types.go`'s `Agent.Spec.Config` doc comment: "API keys are NOT read from here; they come from the agent-executor process environment." |
| Runner Secret volume copy (`spec.execution.job.volumes` referencing a Secret) | The referenced Secret | If the Secret already lives in the run namespace, only `secrets get` there is needed. If it lives in a **different** namespace, the controller also has to `create` a copy in the run namespace — see the cross-namespace note below. Either way, denial fails the WorkflowRun **Failed** with an actionable message naming what to grant. |
| Cron trigger `inputValuesFrom` | The Secret backing each cron input | The scheduled fire logs an actionable error and does not create the run for that tick; the schedule keeps ticking. |
| Webhook trigger HMAC verification | The HMAC signing-key Secret | Every delivery returns `401 Unauthorized`. |
| Agent / MCP server credentials, external-agent auth, cluster kubeconfig | The referenced auth Secret | The step fails when it tries to use the credential. |

### The remediation Role

For every feature in the table above, the fix is the same shape: a namespaced
`Role` granting `get` on the named Secret(s), bound to the ServiceAccount that reads it. Read
paths only ever need `get`.

- **Controller-read** features (LLM injection, runner volume copy, cron inputs, webhook HMAC)
  are read by the **controller** ServiceAccount (`controller-manager` in the install
  namespace by default). The Secret is read in the namespace listed above, so the Role goes
  **in that namespace** and binds the controller ServiceAccount across namespaces.
- **Agent / MCP** credentials read from within an agent step are read by the
  **agent-executor** ServiceAccount.

Copy-paste example — grant the controller `get` on two named Secrets in the `team-a` namespace
(where a WorkflowRun and its cron/webhook Secrets live):

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: ottoflow-secret-access
  namespace: team-a
rules:
  - apiGroups: [""]
    resources: ["secrets"]
    verbs: ["get"]
    resourceNames:
      - ottoflow-llm-credentials
      - my-webhook-hmac
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: ottoflow-secret-access
  namespace: team-a
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: ottoflow-secret-access
subjects:
  - kind: ServiceAccount
    name: controller-manager       # the OttoFlow controller ServiceAccount
    namespace: <install-namespace>  # the namespace OttoFlow is installed in
```

For an agent/MCP credential read by the agent-executor, use the same manifest but bind the
**agent-executor** ServiceAccount instead, in the namespace where that Secret lives.

> **Note on `list`/`watch` and `resourceNames`.** An *unconstrained* `list` or `watch` — one
> carrying no field selector — has no per-object name for `resourceNames` to check, so it can
> never be name-scoped. But a `list`/`watch` request that carries an exact-match
> `metadata.name` field selector CAN be authorized by `resourceNames`: kube-apiserver populates
> the request's object name from that selector (the `AuthorizeWithSelectors` feature, GA and
> default-on since Kubernetes 1.34, beta and default-on since 1.32), and RBAC checks it exactly
> like it would for `get`. OttoFlow's per-Secret reads on this page only ever need `get`, so
> this doesn't apply to them.

### Diagnose a denied Secret read

Four different symptoms can show up when a Secret read is denied or missing, and they call for
different fixes. Match the exact text you observe:

- **Symptom A — `.status.message` contains `is Forbidden` and `docs/user/rbac-secret-access.md`.**
  The controller was denied. This is the terminal case: an unconditionally reachable
  Nirmata-provider agent step (or one with `modelProvider` unset) needed the well-known
  credentials Secret, or a `spec.execution.job.volumes` entry needed a Secret in the runner
  namespace, and RBAC said no. Grant the Role below. For the well-known-credentials case, this
  denial may now be reported by the pre-read authorization check with the Secret read never
  attempted at all, rather than by the read itself — see "Telling which check caught a
  Symptom A denial" below for how to tell which; the remediation is identical either way.
- **Symptom B — `.status.message` contains `Nirmata LLM credentials required: set NIRMATA_LLM_TOKEN`.**
  This message text alone does not tell you whether credentials are unconfigured or a configured
  Secret's read was denied — check `kubectl describe workflowrun <name>` for an
  `LLMCredentialsForbidden` Warning Event first:
  - **Event present:** the credentials Secret IS configured, but its read was denied. This can
    still reach the agent-executor even though the run did not fail outright at build time: a
    denied read is only forced to a terminal failure (Symptom A) for an *unconditionally*
    reachable Nirmata-agent step — a step behind `matchConditions`, `failurePolicy: Continue`, or
    a `forEach` stays benign at build time (Symptom C), since a build-time check cannot prove that
    step will actually run. If it DOES run, injection was already skipped and the agent-executor
    finds no token in its environment, producing this same generic message. The fix is to grant
    the Role below, not to touch `llmCredentialsSecret`.
  - **Event absent:** genuinely unconfigured — no credentials Secret was ever supplied, granted or
    not. The fix is to configure `spec.execution.llmCredentialsSecret` (or set `NIRMATA_LLM_TOKEN`
    directly), not to touch RBAC.
- **Symptom C — `kubectl describe workflowrun <name>` shows a `LLMCredentialsForbidden` Warning,
  and the run still succeeded.** The benign branch: either there was no agent step to use the
  credentials, the agent used a non-Nirmata provider (which never reads this Secret in the first
  place — see the table above), or the run already supplied its own Nirmata token. Nothing to fix
  unless you expected injection, in which case grant the Role below.
- **Symptom D — the runner pod is stuck `ContainerCreating` with a `FailedMount` event, for a
  Secret OttoFlow itself minted** (an MCP/agent credential ref the controller resolved via
  `collectSecretRefs`/`buildSecretMounts` — these volumes carry the reserved `ottoflow-secret-`
  prefix). This is **not** the same case as a `spec.execution.job.volumes` entry: that one IS a
  controller-side `get` (see `ensureRunnerSecrets`), already probed at build time, and a denied
  read there produces Symptom A instead, terminally, before the pod is ever created. A
  controller-minted volume is different — the controller never reads that Secret itself; it only
  writes a volume reference for the **kubelet** to resolve, using node credentials, not RBAC. No
  Role granted to the controller or the runner ServiceAccount changes this outcome. Confirm the
  referenced Secret actually exists **in the runner pod's own namespace** (a minted ref is always
  same-namespace by construction); that is the only fix. `.status.message` also names the
  workflow field(s) that generated the failing volume(s) (e.g. an `mcpToolCall` step's
  `auth.secretRef`), appended to the kubelet's own mount-failure text — this is **runtime mount
  validation** of the kubelet's own report, never a pre-check: nothing here runs, or decides
  anything, ahead of the pod actually trying to start. A pod stuck only on
  `CreateContainerError` (not `FailedMount`) instead fails with an empty `.status.failureReason`
  by design — that Waiting reason comes from the container runtime's own `CreateContainer` call,
  not reference resolution, so an empty `failureReason` there is not a claim the run succeeded or
  that RBAC is uninvolved.

For Symptom A, confirm the denial directly, then grant the Role:

```bash
kubectl auth can-i get secrets/<name> -n <namespace> \
  --as=system:serviceaccount:<install-ns>:<controller-sa>
```

Worked example — a WorkflowRun in `team-a` referencing Secret `ottoflow-llm-credentials`, with
OttoFlow installed in namespace `ottoflow` under ServiceAccount `controller-manager`:

```bash
kubectl auth can-i get secrets/ottoflow-llm-credentials -n team-a \
  --as=system:serviceaccount:ottoflow:controller-manager
# no
```

Fix it with `rbac.secretAccess.controller` (the Helm shortcut below) or the hand-written
Role+RoleBinding above — **never** by adding `secrets` to `rbac.*ClusterRole.extraResources`,
which the chart refuses to render (see the caveat under "The Helm shortcut").

#### Telling which check caught a Symptom A denial

For the well-known-credentials case specifically, `.status.message` carries a
`(underlying error: ...)` clause that names which check caught the denial:

- Contains **`SelfSubjectAccessReview reported that the controller identity is not
  authorized`** — the pre-read authorization check denied it. The Secret `Get` was never
  attempted.
- Contains the API server's own 403 text instead — the `Get` itself came back `Forbidden`. This
  happens when the pre-read check was unavailable or inconclusive (and so deferred to the read),
  or when a grant changed in the brief window between the check and the read.

The remediation is identical either way; this only tells you which of the two checks fired.

#### Reading the diagnostic addendum on a Symptom A message

For Symptom A, `.status.message` can also carry an extra sentence appended after the `Confirm
with:` command, built from a `SelfSubjectRulesReview` — a read-only, self-describing check
("what can I do?") the API server answers for the controller's own identity in the WorkflowRun's
namespace. This addendum is diagnostic only: it never changes whether the run fails, only what
the failure message says, and it is silently omitted (the message reads exactly as it would
without this feature) whenever the check itself is inconclusive or fails. When present, it falls
into one of three cases:

- **The API server reports the access IS currently held.** The denial did not come from a
  missing Role after all — look for an authorization webhook, or a grant that changed between
  the check and the read.
- **The API server reports a Secret-related grant that does not cover this read.** This is the
  case worth reading closely: it names the verbs and resource names an existing Role actually
  grants, so you can compare them against what this read needs. A Role that grants access to the
  wrong Secret name — a typo, a stale name after a rename, or (subtly) a Role whose
  `resourceNames` literally contains `"*"` rather than omitting the field — produces exactly the
  same bare 403 as no Role at all; this is where the addendum tells the two apart. `resourceNames`
  has no wildcard support: leaving it out grants every name, but writing `"*"` into it grants
  access to a Secret literally named `*` and nothing else.
- **The API server reports no rule grants any Secret access at all.** Confirms no Role exists yet
  — the remediation above is the fix.

This addendum can only ever speak to **permission**, not existence: a `403 Forbidden` and a `404
Not Found` are different responses, and the controller only ever sees whichever one the API
server chose to return first. If the Secret does not exist at all, RBAC may still report the read
as denied before existence is ever checked, so this can never substitute for confirming the
Secret is actually there.

### Cross-namespace runner Secret-volume copy needs an un-scopable `create`

A `spec.execution.job.volumes` entry can reference a Secret that lives in a namespace other
than the WorkflowRun's own (e.g. the Workflow's namespace, or a shared source namespace). When
that happens, the controller copies the Secret into the run namespace so the runner Job can
mount it there. That copy is a `create` in the run namespace — and `create` is one of the verbs
`resourceNames` cannot scope (the object doesn't exist yet at authorization time, so there's no
name to check against), the same restriction the note above calls out for `list`/`watch`. A Role
granting it necessarily grants the controller `secrets create` for *any* Secret name in that
namespace, not just the one being copied — a materially bigger grant than the named `get`
everywhere else in this doc.

**Recommended:** avoid the copy entirely by placing the Secret directly in the run namespace.
Then the volume reference resolves locally and only the ordinary named `secrets get` (see above)
is required — no `create` grant needed. Use the cross-namespace copy only when the Secret
genuinely has to be sourced from elsewhere (e.g. it's centrally managed) and you've accepted the
broader `create` grant that entails.

## The Helm shortcut

If you install with the chart, you don't have to hand-write the manifest above. Each component
under `rbac.secretAccess` takes a **list** of `{namespace, secretNames}` entries — one per tenant
namespace — and the chart renders a `Role` + `RoleBinding` in each (scoped to those names, `get`
only), with the binding subject pointed at the component ServiceAccount in the install namespace:

```yaml
rbac:
  secretAccess:
    controller:
      - namespace: team-a        # a tenant namespace your Secrets live in
        secretNames:
          - ottoflow-llm-credentials
      - namespace: team-b        # one entry per tenant namespace, one release for all tenants
        secretNames:
          - ottoflow-llm-credentials
    agentExecutor:
      - namespace: team-a
        secretNames:
          - openai-api-key
```

Both lists are empty by default (nothing is rendered). **Each entry's `namespace` must be a tenant
namespace, and it must already exist** at install/upgrade time — Helm does not create tenant
namespaces, and rendering a Role into a missing one fails the install. If you leave `namespace`
empty it resolves to the install namespace, and the chart **fails the render** with a message
telling you to pick a tenant namespace — as blast-radius hygiene, workflow Secrets belong in a
tenant namespace, not in the control-plane (install) namespace (see "Keep workflow Secrets out of
the install namespace" above). One entry per namespace per component; the chart rejects duplicates
(merge their `secretNames` instead).

> **Caveat: keep `secrets` out of every `rbac.*ClusterRole.extraResources` list.** "Secret access
> starts at zero" holds because the shipped roles carry no Secret grant AND the chart refuses to
> render a `secrets` (or wildcard) grant in any of the four `extraResources` lists
> (`rbac.coreClusterRole`, `rbac.clusterRole`, `rbac.viewClusterRole`, `rbac.runnerClusterRole`).
> Every one of them reaches the controller's own identity: the core, additional and view lists
> render into ClusterRoles the controller's aggregated ClusterRole selects, so a grant there lands
> on its ServiceAccount directly; the runner and view lists render into runner-aggregated
> ClusterRoles, and the controller keeps `create`/`update` on ClusterRoleBindings plus `bind` on
> the runner role (needed for the roleRef-immutability migration), so a grant there is one it
> could bind to its own ServiceAccount. Either way the cluster-wide access this hardening removed
> would silently re-open. Grant Secret access only through `rbac.secretAccess` above (named,
> namespaced, `get`-only).

## What is granted automatically

The one Secret grant OttoFlow creates for you is a **namespaced** Role in the install
namespace for the controller's built-in TLS certificate manager, which creates, fills, and
rotates the webhook and agent-executor CA/TLS Secrets. It is scoped to the install namespace and
never reaches tenant namespaces. You don't need to configure it; it is part of the chart.

The Role mirrors Kyverno's own cert-Secret RBAC exactly and is **as tight as RBAC allows**: an
unrestricted `create` rule (resourceNames cannot scope `create` — the object doesn't exist yet at
authorization time), plus a `get`,`update`,`delete` rule scoped by `resourceNames` to exactly the
four cert Secrets the manager owns — the webhook CA+pair and the agent-executor CA+pair (names
follow the kyverno formula `<service>.<namespace>.svc.tls-ca` / `.tls-pair`). **There is no
`list` or `watch` on Secrets anywhere.** Nothing else in the namespace can be read or changed
through this Role, and the unrestricted `create` only ever lets the controller mint Secrets under
names it computes deterministically itself — never an operator- or workflow-controlled name.

This is possible because the controller creates those four Secrets itself, the same way
Kyverno's own controller does when `createSelfSignedCert` is left at its default: at startup it
checks whether each one exists, creates it if not, and fills it with locally generated cert
material — Create when the Secret is absent, Update once it exists (the `kyverno/pkg/tls`
renewer, driven by `internal/certmanager/setup.go`). Renewal is driven by a timer that updates
them by name (so it needs no `list`/`watch` Secret informer).

> **Recovering a deleted cert Secret.** The controller **recreates** a cert Secret an operator
> deletes with `kubectl delete secret …` on its own — no manual re-apply needed. It notices on its
> next bootstrap (a controller restart) or steady-state renewal tick (`CertRenewalInterval`, 12h)
> at the latest; restart the controller if you want it recreated immediately.

> **Caveat: `controller.namespace` must match the install namespace.** The chart renders the
> certmanager Role above into the install namespace (`ottoflow.namespace` / the release
> namespace), and the controller creates the cert Secrets there too. Separately,
> `controller.namespace` sets where the controller does its own leader election and — because the
> controller also bootstraps its **webhook** TLS certs in that same namespace — where it expects
> those webhook cert Secrets to live. If you set `controller.namespace` to something other than the
> install namespace, the shipped Role no longer covers the namespace the controller is actually
> writing webhook certs to, and that bootstrap fails Forbidden. Leave `controller.namespace` unset
> (it defaults to the install namespace) unless you also add an equivalent Role in the namespace
> you point it at.

## The agent-executor CA reaches tenant namespaces as a ConfigMap, not a Secret

A runner Job whose workflow has an agent step verifies the agent-executor's TLS certificate
against the internal CA. That CA is one of the four cert Secrets above, it lives only in the
install namespace, and no Secret access is granted anywhere else — so the runner never reads that
Secret and the controller never copies it. Instead, before it creates the runner Job, the
controller publishes the CA **certificate only** (`tls.crt`; never `tls.key`) as a ConfigMap in
the WorkflowRun's namespace, named after the CA Secret
(`--workflow-runner-agent-executor-ca-secret`, chart value `workflowRunner.agentExecutorCASecret`,
default `<agent-executor>.<install-namespace>.svc.tls-ca`), and mounts that ConfigMap into the
runner Job as `ca.crt`. A certificate is public material; a ConfigMap needs none of the Secret
RBAC this document is about, and the controller's ClusterRole already lets it create, read and
update ConfigMaps. Nothing has to be created or granted in the tenant namespace. A workflow
without an agent step gets neither the ConfigMap nor the mount.

The ConfigMap is labelled `app.kubernetes.io/part-of: ottoflow` and has no owner: it is shared by
every run in the namespace and stays until the namespace goes. The controller rewrites it
whenever the certificate it holds differs from the current CA, so a CA rotation reaches a
namespace on its next run with an agent step (a runner already running at that moment keeps the
CA it loaded at start-up). A ConfigMap of that name **without** the label is neither trusted nor
overwritten: the run fails with a message naming it, so that nobody with `configmaps create` in a
tenant namespace can hand the runners there a CA of their choosing. Rename or delete such a
ConfigMap to let the controller publish the CA under that name.

> **Secret copies left by earlier releases.** Earlier releases delivered the CA to a tenant
> namespace by copying the **whole** CA Secret into it — `tls.crt` and `tls.key`, the CA's
> private key. The controller neither reads nor deletes those copies now: it holds no `secrets
> delete` outside the install namespace and this document grants it none, so they stay until
> removed. Each copy carries an owner reference to the WorkflowRun whose reconcile created it
> and is garbage-collected with that run; until then anyone who can read Secrets in that
> namespace can read the private key. Find them — every row outside the install namespace is a
> leftover:
>
> ```bash
> kubectl get secrets --all-namespaces \
>   --field-selector metadata.name=<agent-executor-ca-secret-name> \
>   -l app.kubernetes.io/part-of=ottoflow \
>   -o custom-columns=NAMESPACE:.metadata.namespace,NAME:.metadata.name,AGE:.metadata.creationTimestamp
> ```
>
> Delete them with your own identity (`kubectl delete secret <name> -n <tenant-namespace>`);
> runners no longer depend on them. If such a copy sat in a namespace whose Secret readers you do
> not trust, treat the CA as exposed and rotate it: delete the CA Secret in the install namespace
> and restart the controller, which regenerates the CA and re-issues the agent-executor
> certificate (see "Recovering a deleted cert Secret" above); the tenant ConfigMaps pick the new
> CA up on their namespaces' next agent-step runs.

## No migration needed for the cert Secrets

No released chart ever shipped the four cert Secrets as manifests, so there is no upgrade or
migration step for them: fresh installs and upgrades alike simply let the controller create and
fill them at startup.

## GitOps (ArgoCD): the cert Secrets are not tracked by the chart

The four cert Secrets are **entirely controller-managed**: the chart declares no manifest for
them, so there is nothing for ArgoCD to diff against, no drift to report or ignore, and nothing
`prune: true` could delete — an auto-sync with selfHeal can never blank a live Secret back to an
empty git value, because there is no git value.
