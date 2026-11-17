# Proposal: GitHub App Credentials (Controller-Held Key with a Token Broker)

**Status**: Draft for design review. Nothing is implemented.  
**Issue**: [#6](https://github.com/nirmata/ottoflow/issues/6)  
**Related**: [#5](https://github.com/nirmata/ottoflow/issues/5), [#7](https://github.com/nirmata/ottoflow/issues/7), [PROPOSAL_GITHUB_WEBHOOK_TRIGGER.md](PROPOSAL_GITHUB_WEBHOOK_TRIGGER.md), [PROPOSAL_AGENT_SANDBOXING.md](PROPOSAL_AGENT_SANDBOXING.md)

> Pointers in this document were checked against `main` at `5e1217595d129356434f75e8bad1986274015fb1`.
> The issue defers implementation to after v1.0.0.

---

## Problem

#6 asks for a GitHub App credential model: an app ID and private key, exchanged at run time for
short-lived installation tokens scoped to specific repositories, and consumed "the same way they consume
`clusterRef`". Nothing like it exists: a search of the tree at the sha above for `GitHubApp`,
`installation` and `github app` finds no GitHub App code.

Today a workflow that needs GitHub access has to carry a long-lived personal access token or similar
credential. A GitHub App fixes the shape of that problem: the installation token expires one hour after it
is created, can be narrowed to named repositories and a permission set, and cannot hold a permission the
App was not granted. Minting one needs a JWT signed with the App's private key (RS256, `exp` at most 10
minutes ahead).

The hard part is where the private key lives. The key can mint a token for every repository in the
installation, and #7 (a step that commits and pushes with these credentials) runs untrusted code. A key in
a pod that runs such code is a key an attacker can read.

## Current credential model

- **How `clusterRef` is consumed.** It is a field on the WorkflowRun only
  (`api/v1alpha1/workflowrun_types.go:32-37`). The controller collects the kubeconfig Secret
  (`internal/workflow/controller/secret_refs.go:128-141`) and mounts it into the runner Job as a file; it
  never puts a secret value into the Job spec (`secret_refs.go:644-645`). A Secret the runner mounts must
  be in the run's own namespace (`secret_refs.go:574-584`). A stdio MCP server in the runner gets the
  kubeconfig as `KUBECONFIG` (`internal/agent/mcp_client_impl.go:178`).
- **Triggered runs never get `clusterRef`.** The Workflow spec has `execution`
  (`api/v1alpha1/workflow_types.go:73`) and no `clusterRef`. The trigger and cron paths build a
  `WorkflowRunSpec` with only the Workflow reference, the inputs and `Execution`
  (`internal/workflow/controller/trigger_manager.go:685-691`,
  `internal/workflow/controller/scheduler.go:335-341`). A credential that sits only on the run, like
  `clusterRef`, cannot reach runs started by #5's webhook trigger. The scope has to come from the Workflow.
- **Run authors control `spec.execution`.** `WorkflowRun.spec.execution` and `Workflow.spec.execution` are
  the same type, and a run's job settings override the Workflow's. For example, a run's
  `activeDeadlineSeconds` replaces the Workflow's
  (`internal/workflow/controller/workflowrun_controller.go:1340-1344`, `:1359-1361`). Anything placed under
  `execution` is therefore controlled by whoever creates the run.
- **A run can name a Workflow in another namespace.** `WorkflowRef.Namespace` defaults to the run's
  namespace but can name another (`api/v1alpha1/workflowrun_types.go:268-271`). A comment in the
  controller describes the same case (`workflowrun_controller.go:1052-1053`).
- **Secret access today.**
  - The chart grants no Secret access in tenant namespaces by default. Operators add named, get-only Roles in tenant
    namespaces through `rbac.secretAccess.<component>`
    (`charts/ottoflow/templates/rbac-secret-access-role.yaml:1-7`). An entry that resolves to the install
    namespace is refused (`rbac-secret-access-role.yaml:40-42`), and the chart refuses a `secrets` entry in
    any extraResources list (`charts/ottoflow/templates/_helpers.tpl:350-360`).
  - The controller already creates Secrets in run namespaces on one legacy path. `ensureRunnerSecrets`
    (`workflowrun_controller.go:1028-1118`) copies a Secret named by a `spec.execution.job.volumes` entry
    into the run's namespace with `r.Create` (`:1107`), owned by the WorkflowRun (`:1104`). Secret volumes
    the controller minted for the Workflow's own Secret references are never copied (`:1059-1061`).
    `docs/user/rbac-secret-access.md:281-297` says this copy needs `secrets create` for any Secret name in
    that namespace, which `resourceNames` cannot scope, and recommends avoiding the copy.
  - The built-in certificate manager holds an unscoped `secrets create` plus `get`, `update` and `delete`
    limited to four named Secrets, in the install namespace only
    (`charts/ottoflow/templates/certmanager-role.yaml`;
    `docs/user/rbac-secret-access.md:350-367`). It never reaches tenant namespaces.
  - Runner pods have no Secret RBAC by default ([PROPOSAL_AGENT_SANDBOXING.md](PROPOSAL_AGENT_SANDBOXING.md),
    Current Controls).
- **Tools can read mounted credentials.** A tool in the runner pod can read every Secret mounted into
  that pod (the sandboxing proposal's Remaining gaps). MCP tools called from `agentRef` steps run in the
  agent-executor, not the runner (`secret_refs.go:71-78`).
- **Existing TokenReview and SubjectAccessReview pattern.** The controller holds both permissions only
  when `controller.mcp.enabled` is set (`charts/ottoflow/templates/mcp-auth-clusterrole.yaml`). The same
  pattern is in `agent-executor-clusterrole.yaml:31,38` and `a2a-token-reviewer-clusterrole.yaml:18,24`
  in the same directory.
- **Runner pods already carry a pod-bound ServiceAccount token.** The Job sets
  `AutomountServiceAccountToken: ptr.To(true)` (`workflowrun_controller.go:1543`).
- **Existing token exchange.** MCPServer auth `oauth2` exchanges client credentials for a token
  (`api/v1alpha1/mcpserver_types.go:83`, `internal/agent/mcp_client_impl.go:310-347`).
- **Dependencies.** `golang-jwt/jwt/v5 v5.3.1` (`go.mod:115`) and `golang.org/x/oauth2 v0.36.0`
  (`go.mod:176`) are indirect dependencies. An RS256 JWT can also be signed with the standard library's
  `crypto/rsa`.

## Proposed design

The controller holds the App's private key and serves a token broker. A runner never sees the key. When a
step needs GitHub, the runner proves which pod it is, and the controller mints an installation token
narrowed to what that run's Workflow allows.

### API

A Workflow-only field, outside `spec.execution`, in `api/v1alpha1/workflow_types.go`. It holds:

- a reference to the key Secret (app ID or client ID, installation ID, private key);
- a repository allowlist;
- a permission set.

Only this field is authoritative for the broker and the Workflow validator. A WorkflowRun cannot set or
override it, because anything under `spec.execution` is controlled by the run author (see Current
credential model). The trade is that a WorkflowRun cannot narrow the scope in its spec. Narrowing happens
per request, inside the Workflow's allowlist, for example from `inputs.repo`.

### Secret convention

The key Secret lives in the Workflow's tenant namespace, never in the install namespace. The operator lists
it under `rbac.secretAccess.controller`, which gives the controller a named `get` and nothing broader. No
new CRD is needed (Open questions, 2).

**Key isolation is a prerequisite.** The broker serves only runs in this same namespace (Scoping, Namespace
binding), and a run author controls `spec.execution.job.env`, `volumes` and `volumeMounts`. The controller
copies them into the runner Job unchanged (`workflowrun_controller.go:1368-1370`); `WorkflowRunJobSpec.Validate`
checks their names and mount paths, not what they reference (`api/v1alpha1/workflowrun_types.go:203-260`).
The kubelet mounts or injects any Secret the pod spec names, with no Secret RBAC on the runner
ServiceAccount. `ensureRunnerSecrets` only checks that a `secret` volume's Secret exists (`workflowrun_controller.go:1068-1070`),
which the named `get` above makes pass, and it skips `projected` volumes and never reads env `secretKeyRef`
(`:1043-1045`). Where the controller holds the legacy `secrets create` grant, a run that names a Workflow in
another namespace can also get the Secret copied into its own namespace (`:1078-1107`). So, before the
broker ships, the controller refuses to create a runner Job whose pod spec names a key Secret anywhere
(a `secret` volume, a `secret` source in a `projected` volume, an env `secretKeyRef`, or a Secret reference in
another volume type such as a `csi` volume's `nodePublishSecretRef`), whichever field it
came from, and `ensureRunnerSecrets` never copies a key Secret. A reserved name prefix that the Workflow
validator requires for the key Secret keeps that a name check with no extra Secret read.

### Key rotation and revocation

The controller reads the key Secret each time it signs a JWT and keeps no parsed key between mints, so an
updated Secret is used on the next mint with no restart. To rotate, the operator adds the new key to the
App in GitHub, updates the Secret, then deletes the old key in GitHub. To revoke, the operator deletes the
key in GitHub (new mints fail at once) and the Secret. On a key change the controller also revokes the
tokens it holds in its cache (see Token lifetime and refresh); tokens it no longer holds expire within the
hour. The controller has only a named `get` on the key Secret and no watch, so it detects a change on the
next mint: it compares a digest of the key bytes with the one its cached tokens were minted under, and
a difference drops and revokes those tokens. A label or annotation edit changes the Secret's `resourceVersion`
but not the key, so it does not revoke tokens.

### Token flow

1. The runner sends a projected ServiceAccount token whose audience is the broker only, not the API server
   audience, to the controller's token endpoint. The controller adds that projected volume to the runner Job
   (the repository sets no projected token today), and the pod's default API-server token is never sent to
   the broker. The controller does not add the volume to a Job whose run overrides `spec.execution.job.image`
   (Runner image rule). The endpoint is served over TLS with the controller's internal certificate manager.
2. The controller checks the token with TokenReview, setting `spec.audiences` to the broker audience, so a
   token minted for the API server is refused (`TokenReviewAndSARAuthenticator.WithAudiences` already does
   this, `internal/auth/sar.go:77`, `:98`). For a token bound to a pod, the review's
   `status.user.extra` carries `authentication.kubernetes.io/pod-name` and
   `authentication.kubernetes.io/pod-uid` (Kubernetes
   [service accounts admin guide](https://kubernetes.io/docs/reference/access-authn-authz/service-accounts-admin/)).
   The controller then runs a SubjectAccessReview through the existing `TokenReviewAndSARAuthenticator`
   (`internal/auth/sar.go`), as its MCP server does, behind a feature flag. The chart floor is Kubernetes 1.29+ (`charts/ottoflow/README.md:7`).
3. The controller maps the pod to its owning Job and reads the launch record on it (Scoping, Launch record).
   It refuses if a WorkflowRun does not own the Job as its controller, if that WorkflowRun has reached a
   terminal phase, if the record is missing, if the
   recorded Workflow's namespace differs from the Job's, or if the record shows an `image`, `env`,
   `volumes` or `volumeMounts` override or a non-local `clusterRef` (Runner image rule, Runner env and
   volume rule, Cluster kubeconfig rule). The phase is WorkflowRun status, which the runner role can
   write (Threat model, Key theft), including from a run whose image is overridden, so this check
   bounds how long a run mints and is not a trust check: a forged phase cannot widen scope, and at
   most lets a pod whose record passes keep minting for its own recorded Workflow. Forged step or
   callback state cannot steer a run of a `githubApp` Workflow either, because its runner takes no
   resume state from status (Scoping, Run immutability).
4. It mints a token narrowed to the intersection of the request and the **recorded Workflow's** allowlist and
   permission set. It never reads the WorkflowRun spec for scope or trust.
5. The runner asks again before the token expires.

The key stays in the controller, and tokens are never written to etcd. The repository does not read
`authentication.kubernetes.io/*` extras today: `TokenReviewAndSARAuthenticator.AuthenticateRequest`
returns only the username and drops `status.user.extra` (`internal/auth/sar.go:84`, `:145`). The broker
uses a variant that also returns the TokenReview's user info, so pod-name and pod-uid come from the one
TokenReview per request, not a second one. The pod-uid to Job to recorded Workflow lookup is new code.

### Consumers

First consumers: an MCPServer auth type `githubApp`; CEL `http.*`; #7's step; and `agentRef` MCP tools,
which run in the agent-executor.

## Scoping

- **Who can use the App.** Whoever can create or update a Workflow in a namespace can name that namespace's
  key Secret and set its allowlist and permissions, so Workflow create/update RBAC is App scope there. The
  controller can only read a key Secret the operator listed under `rbac.secretAccess.controller`, and the
  installation's own grants cap every token. Operators give Workflow, StepTemplate and MCPServer
  write access only to people they trust with the App (Runner binding). WorkflowRun create or update
  cannot widen that scope, because of the runner image rule, the runner env and volume rule, the
  cluster kubeconfig rule, the launch record, run immutability and the runner binding below.
- **Runner image rule.** A run author can set `spec.execution.job.image`, and the controller uses it after
  a whitespace check (`workflowrun_controller.go:1346-1352`; the configured image is
  `r.RunnerConfig.RunnerImage`, `:1321-1324`). Code the author controls would otherwise hold a real
  broker-audience token and pass the review. So the broker mints only for runs that keep the configured
  runner image. The gate is the broker's, at mint time (see Launch record below): it
  refuses if the record shows an image override, so that check covers the image and no separate
  check on the pod's containers is needed. It holds even if a run adds its own projected token with the
  broker audience through `spec.execution.job.volumes`. Defence in depth: the controller does not mount the
  broker-audience token into a Job whose image is overridden, so the code never gets a broker token to send.
- **Runner env and volume rule.** The configured image still reads its environment and mounts, so a run that
  keeps it could set `HTTPS_PROXY` and `SSL_CERT_FILE` and mount a CA, and the runner's HTTP client (CEL
  `http.*` uses `http.DefaultTransport`) would send the minted installation token through an intercepting
  proxy. So a run of a Workflow with a `githubApp` scope may not supply `spec.execution.job` `env`,
  `volumes` or `volumeMounts`. The gate is the broker's, at mint time: it reads the launch record
  (Launch record below) and refuses to mint if it shows an `image`, `env`, `volumes` or `volumeMounts`
  override. The check is exact, and it closes the case where the scope is
  added after the run starts. Defence in depth, as with the image rule: the controller also refuses such a
  run of a `githubApp` Workflow before it creates the runner Job, so it never mounts the broker-audience
  token. A deny-list of
  proxy and TLS env names is not enough: a volume mounted over the system trust store, or other names such as
  `ALL_PROXY`, keep the same path open. Trade-off: `githubApp` workflows cannot take run-level env or
  volumes; they get their inputs from the Workflow spec.
- **Cluster kubeconfig rule.** `spec.clusterRef` has three sources (`api/v1alpha1/workflowrun_types.go:50-67`):
  `local`, which selects the in-cluster config; `kubeConfigSecretRef`, a Secret that
  `secret_refs.go:128-141` mounts whole, independent of the run-level volumes; and `kubeConfigFilePath`,
  any path in the pod. The runner hands the file's contents to client-go
  (`internal/workflow/cluster/kubeconfig.go:41-46`, `internal/workflow/cluster/resolve.go:61-62`), so a
  kubeconfig the run author supplies could set `tokenFile` to the projected broker token and `server` to an
  attacker endpoint, or name an exec credential plugin. So for a Workflow with a `githubApp` scope the
  controller refuses, before it creates the Job, a run whose `clusterRef` is set and is not `local: true`.
  No run-supplied kubeconfig is then read in a pod that holds the broker token. The controller records
  this on the Job like the overrides (Launch record), so the broker can check it too. Trade-off: in v1 a
  `githubApp` Workflow cannot reach another cluster through `clusterRef`, even with a kubeconfig the operator
  trusts; telling trusted kubeconfigs apart from run-supplied ones is left out.
- **Launch record.** The broker does not trust the WorkflowRun spec, which stays editable after the Job
  exists: `WorkflowRunValidator.ValidateUpdate` ignores `oldRun`
  (`internal/webhook/workflowrun_validator.go:28-29`), and reconciliation keeps the existing Job. When the
  controller creates a runner Job it sets one annotation on it (for example
  `ottoflow.nirmata.io/github-launch`) that the runner cannot change. It records the Workflow the Job was
  launched against (namespace, name and UID), whether the run carried any `image`, `env`, `volumes` or
  `volumeMounts` override, and whether it used a `clusterRef` that is not `local`. The runner role reads
  Jobs and cannot update them (`charts/ottoflow/templates/clusterrole.yaml:463-467`). Anyone else who can update Jobs in that
  namespace could rewrite the record, so, like pod create (Threat model, Pod-create equals key access), Job
  update there is treated as App access. At mint time the
  broker reads the pod's owning Job, checks that a WorkflowRun owns it as its controller and that the
  annotation is present, checks that the recorded Workflow still has the recorded UID, and mints only for
  that Workflow. Later edits to the run (`workflowRef` or the overrides) do not change what the broker
  trusts, and the two rules below keep them from changing what the runner executes. A Job with no
  annotation gets no token.
- **Run immutability.** The runner reads these WorkflowRun spec fields at startup: `workflowRef`
  (name and namespace, `cmd/workflow-runner/main.go:318-323`), `inputValues`
  (`internal/workflow/executor/executor.go:391`), `clusterRef` (`main.go:338`) and `execution`,
  whose `checkpointing` decides whether it saves and restores a checkpoint
  (`internal/workflow/executor/checkpoint.go:87`, `:394-396`) and which it copies into the in-memory
  runs of sub-workflow steps (`executor.go:974-975`); it also reads `events`, which only sets event
  emission. With the broker flag on, the webhook's `ValidateUpdate` compares `oldRun` and refuses
  any change to `workflowRef`, `inputValues`, `clusterRef` or `execution` after create. It applies
  to every run, not only runs of a `githubApp` Workflow, because a Workflow can gain the scope
  later. Run status is a separate subresource and stays writable, and the runner role can write it
  from any run (Token flow, step 3), so on a run of a `githubApp` Workflow the runner takes no
  resume state from it: it ignores the step statuses and callback state it finds in status at
  startup and does not restore a checkpoint. Trade-off: in v1 a `githubApp` Workflow cannot use
  `waitForCallback` (the Workflow validator refuses it, and for a Workflow admitted without the
  webhook the runner refuses one before any step) or checkpointing (the controller refuses the run
  before its Job).
- **Runner binding.** The webhook may not be in the path: the chart can be installed without it
  (`webhook.enabled=false`), and with `webhook.failurePolicy=Ignore` the API server admits a request
  when the webhook is unreachable or errors (the default is `Fail`,
  `charts/ottoflow/README.md:299-300`). So the runner is also bound to the launch record. When the
  controller creates the Job it sets, as env on the runner container, the recorded Workflow's
  namespace, name and UID and a SHA-256 of the run's `inputValues`, `clusterRef` and `execution` as
  it read them, after any run-supplied env and refusing a run-supplied env of those names. The
  runner loads that Workflow by the recorded namespace and name, not by the run's `workflowRef`, and
  exits if the Workflow's UID differs or the run's `inputValues`, `clusterRef` and `execution` no
  longer match the recorded hash, before it resolves `clusterRef` (which it does at startup,
  `cmd/workflow-runner/main.go:167-169`) or runs any step. Its own steps come from the Workflow the
  broker authorized. Three step kinds load more by name when the step runs, with no UID check. A
  sub-workflow step fetches the Workflow it names and runs it in the same pod as a child run in that
  Workflow's namespace (`internal/workflow/executor/executor.go:907-909`, `:947-967`), and a
  `stepTemplateRef` step, or a `forEach` with one, fetches a StepTemplate and runs the step it
  describes (`internal/workflow/executor/steptemplate_executor.go:26-40`, `:110-111`,
  `foreach_executor.go:460-474`), each from the namespace the step names, which the Workflow
  validator allows (`internal/webhook/workflow_validator.go:138`). An `mcpToolCall` step reads the
  MCPServer it names from its run's namespace on each call; a stdio server's command and env come
  from that object and it runs in the pod (`internal/workflow/executor/mcp_executor.go:98`,
  `internal/agent/mcp_client.go:106-114`, `internal/agent/mcp_client_impl.go:161-182`), and one with
  auth type `githubApp` receives the installation token (Consumers). So on a run of a `githubApp`
  Workflow the runner refuses a sub-workflow or StepTemplate reference to another namespace, which
  also keeps every MCPServer read in the run's namespace, and StepTemplate or MCPServer create or
  update in the run's namespace counts as App access, like Workflow write (Who can use the App). The
  broker mints only for a Job that keeps the configured runner image (Runner image rule), so these
  checks run in the shipped runner and a run author cannot swap it out.
- **Authority.** The Workflow's allowlist and permission set are authoritative and are checked at
  admission.
- **Request rules.** The broker, not admission, enforces these: a token request may narrow the scope and
  never widen it. GitHub caps `repositories` and `repository_ids` at 500 per token and refuses a permission
  the App was not granted.
- **Namespace binding.** The scope is honoured only when the WorkflowRun's namespace equals the Workflow's
  namespace. Otherwise a run in namespace A that names a Workflow in namespace B would get B's scope.
- **Authorization key.** Authorize on the pod-uid to Job to recorded Workflow chain, never on the
  ServiceAccount. A WorkflowRun can name its own `serviceAccountName` and the controller does not bind
  it, and an operator can configure one shared runner ServiceAccount (sandboxing proposal, Current
  Controls). A check keyed on the ServiceAccount alone would let one run ask for another Workflow's
  scope.

## Token lifetime and refresh

GitHub installation tokens "expire one hour from the time you create them". By default one runner Job
is capped at 3600 s (`defaultRunnerActiveDeadlineSeconds = 3600`, `workflowrun_controller.go:53-54`), and
the Workflow can override that (`:1340-1344`), then the run can override the Workflow
(`:1359-1361`); the result is set on the Job (`:1532`). A run's total time with GitHub access exceeds an
hour in three ways:

- (a) an explicit `activeDeadlineSeconds` above 3600, which a run author can set;
- (b) a checkpoint-retry Job (`WorkflowRunExecutionSpec.Checkpointing`, `MaxRestartAttempts` up to 20,
  `workflowrun_types.go:142`);
- (c) a `waitForCallback` resume, which starts a new process (the runner exits while the step waits,
  `internal/workflow/executor/wait_for_callback.go:41-43`).

Pulling a token per request covers all three without a rotation loop. In v1 a `githubApp` Workflow
cannot use (b) or (c) (Scoping, Run immutability). The broker needs:

- a retry budget and a per-run token cache for GitHub outages and rate limits on the token endpoint,
  because a failure there fails every GitHub step at once;
- the cache keyed on installation, repository set and permissions together; a cache keyed on the
  installation alone could return a token minted for a wider scope. A cached token counts as expired once
  less than a refresh margin (for example 10 minutes) is left, so the runner's ask before expiry (Token
  flow, step 5) gets a new token in one round trip. The old token is served only while it is still valid
  and the new mint fails;
- revocation when the WorkflowRun reaches a terminal phase: the controller revokes the tokens it minted
  for that run (GitHub's `DELETE /installation/token`, called with the token) and drops them from the
  cache. It is best effort: a failure is logged and the token still expires within the hour;
- one serving broker: the controller runs with leader election by default
  (`cmd/controller/main.go:103-105`) and its callback server opens its port only on the leader (`:326`). The
  broker does the same, so the cache lives in one process. Serving only on the leader also means routing
  runners to it: readiness checks only that the webhook server started (`cmd/controller/main.go:543-550`),
  and the existing Service pattern selects every replica
  (`charts/ottoflow/templates/webhook-trigger-service.yaml:16-17`), so followers, which have no listener,
  would get requests. So the leader labels its own pod (for example
  `ottoflow.nirmata.io/broker-leader: "true"`) when it wins the lease and removes the label when it loses it.
  Every process clears the label at start, so a restarted pod never keeps a stale one. The broker Service
  selects that label, and runners retry on connection refusal during failover. After a failover the new leader starts with an
  empty cache and mints again on the next request, and the runner's retry budget covers the gap. Tokens the
  old leader minted are not revoked by the new one and expire within the hour.

## Threat model

This is surface (B), credential blast radius, in the sandboxing proposal.

- **Key theft.** The key is never in a runner or agent-executor pod the controller creates. A tool that
  reads everything mounted in a runner pod does not find the App key, and the broker-audience token it
  does find asks the broker only for a token within that run's own scope; the broker refuses the API-server
  token. This limits the GitHub App key and the broker's scope, nothing else. Runners can still mount
  configured kubeconfigs and MCP Secrets, and the runner role still reads ConfigMaps and updates
  WorkflowRun status (`charts/ottoflow/templates/clusterrole.yaml:429-458`). The agent-executor serves
  every run, so its own token maps to no WorkflowRun (Open questions, 8).
- **Image override.** A run that overrides the runner image gets no broker token from the controller, and the
  broker refuses to mint for its run anyway (Scoping, Runner image rule), so WorkflowRun create does not reach the App.
- **Env and volume override.** A run that keeps the configured image could set proxy or trust-store env
  and mount a CA to read the installation token in transit. The controller refuses `env`,
  `volumes` and `volumeMounts` on runs of a `githubApp` Workflow, and the broker refuses to mint for a Job
  whose launch record shows any of them, even when the scope was added after the run started (Scoping,
  Runner env and volume rule).
- **Run edited after launch.** A caller with WorkflowRun update access could launch a custom image with its
  own broker-audience projection against a Workflow with no `githubApp` scope, then clear the overrides and
  point `workflowRef` at a Workflow that has one. The broker trusts the launch record on the Job, not the
  run, so that pod's record shows the image override and it gets no token, and the edit changes nothing
  (Scoping, Launch record).
- **Run repointed after launch.** The reverse sequence: a caller with WorkflowRun update access lets
  a clean Job start for a credentialed Workflow, then repoints the pending run's `workflowRef` to a
  Workflow they control in another namespace, so the pod would run that Workflow's steps and stdio
  MCP servers under the broker's authorization for the original scope. The webhook refuses the
  update, and if it is bypassed the runner loads the recorded Workflow and exits on a UID or hash
  mismatch (Scoping, Run immutability and Runner binding). The same two rules cover `inputValues`,
  `clusterRef` and `execution`.
- **Run-supplied kubeconfig.** A kubeconfig the run supplies through `spec.clusterRef` could point
  `tokenFile` at the broker token and `server` at an attacker endpoint, or run a credential plugin. The
  controller refuses a non-local `clusterRef` on a run of a `githubApp` Workflow, and the broker refuses to
  mint for a Job whose record shows one (Scoping, Cluster kubeconfig rule).
- **Pod-create equals key access.** The kubelet mounts any Secret a pod spec names with no Secret RBAC, so
  anyone who can create a pod, or a workload that makes one, in the key Secret's namespace can read the key.
  The controller's refusal (Secret convention) covers only Jobs it creates. Operators treat that namespace
  as the key's trust boundary.
- **Scope escape.** Mitigated by the Workflow-only field, the namespace-equality check and the
  pod-uid to Job to recorded Workflow chain above.
- **Stolen token.** One token is limited to its repository set, its permission set and one hour.
- **New endpoint.** The broker is a new authenticated network path. It is off unless the feature flag is
  set, and it reuses the TokenReview and SubjectAccessReview pattern the controller already uses for MCP
  callers.
- **Availability.** Runner steps that need GitHub depend on the controller being up, and during a failover
  on the new leader labelling its pod (Token lifetime and refresh).

## Relation to #5 and #7

- **#5** brings GitHub events in, and #6 lets the run act on GitHub. Because the scope comes from the
  Workflow, runs triggered by #5 can use #6. A trigger's `inputMapping` scope stays inside the allowlist.
  Open: could the App's own webhook feed a `provider: github` trigger? #5 uses one URL per Workflow.
- **#7** is the main consumer and needs `contents:write` and `pull_requests:write`. It runs untrusted
  code, so the hard requirement is that the key never enters such a pod. #7 stays blocked on the choice of
  key custody and token delivery (Open questions, 1).

## Alternatives considered

### Approach 1: the runner mounts the key and mints its own tokens

A Secret holds the app ID, installation ID and private key. The Workflow-only field references it.
`collectSecretRefs` mounts it as a file, the same way it mounts a kubeconfig, and the runner signs the JWT
and mints a scoped token per call. For `agentRef` MCP tools the agent-executor would need the key too.

- **Code areas:** `api/v1alpha1/workflow_types.go`; `secret_refs.go` (`collectSecretRefs`,
  `buildSecretMounts`); `internal/agent/mcp_client_impl.go`; `mcpserver_types.go`.
- **Trade.** It needs the least new machinery: no endpoint and no new RBAC. In exchange the full key sits
  in the runner pod, where tools can read it.
- **Pre-mortem.** An unpinned `uvx` MCP server reads the key file under `/etc/ottoflow/secrets` and sends
  it out over port 443; runner pods have no NetworkPolicy. The attacker mints tokens for every repository
  in the installation until someone rotates the key, and nothing in OttoFlow notices.
- **Right choice only if** every runner is trusted code with no stdio MCP and #7 is dropped.

### Approach 3: the controller mints a token and pushes it into a rotated Secret

Before it creates the runner Job, the controller mints a token narrowed to the Workflow's allowlist, and
refuses runs whose namespace differs from the Workflow's. It writes the token to a Secret owned by the
WorkflowRun in the run's namespace and mounts it like other Secret references. It re-mints about every 45
minutes. The kubelet refreshes Secret volumes that do not use `subPath`
([Secrets](https://kubernetes.io/docs/concepts/configuration/secret/)), and the controller's mounts do not
use it (no `SubPath` in `internal/workflow/controller/`).

- **Code areas:** `secret_refs.go`; the runner Job build; a rotation requeue; chart RBAC.
- **Trades.**
  - It needs the same discouraged, unscoped `secrets create` grant as the legacy copy path
    (`workflowrun_controller.go:1107`; `docs/user/rbac-secret-access.md:281-297`), in every tenant
    namespace that runs GitHub workflows. It also needs `update` for rotation, and the per-run Secret names
    are not known when the Role is written, so `resourceNames` cannot limit that either.
  - Unlike the certificate manager's unscoped `create`, this one reaches tenant namespaces.
  - Tokens sit in etcd, and the scope is per run, not per step.
- **Pre-mortem.**
  - Late rotation (controller restart or leader change during the requeue, plus the kubelet delay). A run
    on one of the three paths past an hour then gets 401 partway through its pushes and leaves branches
    half-updated across repositories.
  - The unscoped create and update grant in tenant namespaces becomes a way to create or overwrite
    arbitrary Secrets there.
- **Right choice if** reviewers refuse a new controller endpoint and operators accept the unscoped Secret
  write grant in GitHub-enabled namespaces.

### Why not these

Approaches 2 and 3 both keep the private key with the controller. Approach 2 also covers the three paths that
run past an hour, without a Secret write grant in tenant namespaces and without tokens in etcd.
Running the broker in the agent-executor instead of the controller was also rejected: the sandboxing
proposal calls that pod "the weak link for surface (B)".

## Architecture impact

| Area | Change |
|---|---|
| `api/v1alpha1/workflow_types.go`, `zz_generated.deepcopy.go`, CRDs (`config/crd/bases/`, `charts/ottoflow/crds/`) | Workflow-only GitHub App field: key Secret reference, repository allowlist, permission set |
| `internal/webhook/workflow_validator.go` | Validate the field: key Secret reference, non-empty repository allowlist, permission set (Scoping, Authority); refuse a `waitForCallback` step in a `githubApp` Workflow (Scoping, Run immutability) |
| `internal/webhook/workflowrun_validator.go` | With the broker flag on, `ValidateUpdate` compares `oldRun` and refuses a change to `workflowRef`, `inputValues`, `clusterRef` or `execution` (Scoping, Run immutability) |
| `cmd/workflow-runner/main.go`, `internal/workflow/executor/` | Load the Workflow named by the recorded namespace and name in the Job env, not the run's `workflowRef`, and exit before it resolves `clusterRef` or runs any step if its UID differs or the run's `inputValues`, `clusterRef` and `execution` no longer match the recorded hash; on a run of a `githubApp` Workflow, refuse a sub-workflow or StepTemplate reference to another namespace, ignore the step statuses and callback state in run status, refuse a Workflow with a `waitForCallback` step before any step, and do not restore a checkpoint (Scoping, Runner binding and Run immutability) |
| `internal/workflow/controller/` | Token endpoint handler and feature flag; pod to Job to recorded Workflow lookup; namespace-equality check; the launch-record annotation set when the Job is created (recorded Workflow, and whether the run set image, env, volumes or volumeMounts or a non-local `clusterRef`) and the mint-time check of it; the recorded-Workflow and hash env on the runner container, with those names added to the reserved runner env names so a run that sets one is refused before its Job (as `rejectReservedRunnerEnv` does today, `workflowrun_controller.go:1362-1365`); a run of a `githubApp` Workflow with those overrides or a non-local `clusterRef` is also refused before its Job, as is one with checkpointing enabled (Scoping, Run immutability); request narrowing; installation-token minting and cache; the broker-audience projected token volume on the runner Job (not added when the run overrides the image); revoking cached tokens when the key bytes change; revoking a run's tokens at a terminal phase |
| `cmd/controller/main.go` | Register the broker as a leader-elected runnable, so it serves only on the leader (as the callback server does, `:326`); clear the broker-leader label on the controller's own pod at start, set it on winning the lease and remove it on losing it |
| `internal/auth/sar.go` | Reuse `TokenReviewAndSARAuthenticator`, which already gates the controller's MCP server (`cmd/controller/main.go:366`), with a new caller resourceName for the broker and the TokenReview's `spec.audiences` set to the broker audience. Return the TokenReview's `status.user.extra` pod name and pod UID with the ServiceAccount name, so the broker makes one TokenReview per request; `AuthenticateRequest` returns only the username today |
| `api/v1alpha1/mcpserver_types.go`, `internal/agent/mcp_client_impl.go`, `internal/workflow/executor/exec_client.go` | MCPServer auth type `githubApp`; a runner-side broker client that reuses `saAuthTransport` from `exec_client.go`, moved into a package `internal/agent` can import (`exec_client.go` imports `internal/agent`, so the client cannot import it back). The moved transport takes the token path as a parameter (the broker-audience token's mount, not the default API-server token path, `exec_client.go:36`, `:267`) and re-reads the projected ServiceAccount token file per request instead of holding the string read once (`exec_client.go:244-249`, `:266-284`): Kubernetes leaves reloading a rotated token to the application, and a cluster with `--service-account-extend-token-expiration=false` rejects the old token once it expires, which would break path (a) under Token lifetime and refresh |
| Helm chart | Behind the flag, a ClusterRole like `mcp-auth-clusterrole.yaml` (the controller creates TokenReviews and SubjectAccessReviews) and a caller ClusterRole like `mcp-caller-clusterrole.yaml` (invoke on `callers.gate.ottoflow.nirmata.io/<broker name>`, bound to runner ServiceAccounts); a named `get` for the key Secret through `rbac.secretAccess.controller`; a broker Service that selects the broker-leader pod label, and a Role in the install namespace giving the controller `patch` on `pods` for that label (pod names change, so `resourceNames` cannot limit it) |
| `docs/user/` | Setup guide for the App, the key Secret and the grant; regenerated API reference |

No Secret write grant in tenant namespaces and no new Deployment.

## Implementation phases

After this design is reviewed and accepted:

1. **API and admission.** The Workflow-only field, CRD and deepcopy regeneration, validation.
2. **Broker.** Token endpoint, TokenReview (with `spec.audiences`) and SubjectAccessReview, the pod-uid
   chain to the recorded Workflow, the namespace-equality check, the launch record on the Job and
   the mint-time check of it (image, env, volumes, volumeMounts, non-local `clusterRef`), the
   run-immutability check in `ValidateUpdate`, the recorded-Workflow and hash env on the Job and the
   runner's UID and hash checks, the runner's cross-namespace refusal and resume-state rule and the
   controller's checkpointing refusal for `githubApp` runs, the broker-audience projected token
   volume on the
   runner Job, installation-token minting, the keyed cache, revoking cached tokens on a key change and a
   run's tokens at a terminal phase, serving only on the leader with the leader pod label
   (`cmd/controller/main.go`), the feature flag, the chart ClusterRole, and the broker Service and pod
   `patch` Role.
3. **Consumers.** Runner-side client, MCPServer auth `githubApp`, CEL `http.*`, `agentRef` MCP tools.
4. **Docs and samples.** Setup guide and a sample Workflow.
5. **Follow-ups, in separate PRs.** #7's step, per-step scoping if wanted.

## Test strategy

- **Unit, minting.** A JWT with `iat` 60 s in the past and `exp` within 10 minutes; the request body
  carries only the intersection of request and allowlist; a widening request is refused.
- **Unit, authorization.** A token for a pod whose Job no WorkflowRun owns, or whose WorkflowRun has reached a terminal phase, is refused; a run whose namespace
  differs from its Workflow's is refused; two runs with different scopes on a shared ServiceAccount each get
  their own scope; the cache never returns a token minted for a wider scope; a token whose audience is the
  API server is refused; a run that overrides `spec.execution.job.image` gets no broker token mounted, and
  the broker refuses to mint for its run even when it presents a broker-audience token; a run of a
  `githubApp` Workflow with `HTTPS_PROXY` or an extra volume is refused before the Job is created, and the
  broker refuses to mint for a Job whose launch record shows env, volumes or volumeMounts,
  including a run created before its Workflow gained the `githubApp` scope.
- **Unit, launch record.** The update sequence: create a run with a custom image and a broker-audience
  projection against a Workflow with no `githubApp` scope, let the Job start, then clear the overrides and
  change `workflowRef` to a Workflow with the scope; the original pod gets no token, and editing the run
  never changes the record. A Job with no annotation, a Job not owned by a WorkflowRun, and a recorded
  Workflow whose UID has changed are each refused.
- **Admission, run immutability.** With the broker flag on, an update that changes `workflowRef`,
  `inputValues`, `clusterRef` or `execution` is refused, including the reverse sequence (start a Job for a
  credentialed Workflow, then repoint `workflowRef` to a Workflow in another namespace); an update that
  changes only `events` or status is accepted; with the flag off, updates are unchanged.
- **Unit, runner binding.** The runner loads the Workflow named in its env, not the run's
  `workflowRef`: run it with a run whose `workflowRef` was repointed to a Workflow with an
  `mcpToolCall` step on a stdio MCPServer, with the webhook not in the path, and assert that the
  step does not run. It exits before it resolves `clusterRef` or runs any step when the recorded
  Workflow's UID differs or the run's `inputValues`, `clusterRef` or `execution` changed after the
  Job was created. The controller sets the env after run-supplied env and refuses a run-supplied env
  of those names. On a run of a `githubApp` Workflow, a sub-workflow step or `stepTemplateRef` (also
  one in a `forEach`) that names another namespace fails before that step runs.
- **Unit, resume state.** A run of a `githubApp` Workflow with checkpointing enabled is refused
  before the Job is created. On such a run the runner ignores step statuses and callback state
  written to its status before it starts, and a checkpoint ConfigMap that carries the run's UID, and
  runs the Workflow from its first step. A `githubApp` Workflow with a `waitForCallback` step,
  admitted without the webhook, is refused before any step runs.
- **Unit, clusterRef.** A run of a `githubApp` Workflow with `kubeConfigSecretRef` or `kubeConfigFilePath`
  is refused before the Job is created, and one with `local: true` or no `clusterRef` is not; the broker
  refuses a Job whose record shows a non-local `clusterRef`, including a kubeconfig whose `tokenFile` names
  the broker token path or that uses an exec credential plugin.
- **Unit, revocation.** A terminal run's tokens are revoked and dropped from the cache; a change in the key
  bytes drops and revokes the cached tokens, and a label-only edit of the Secret does not.
- **Unit, refresh.** A request when the cached token has less than the refresh margin left mints a new
  token, and the old token is served only while it is still valid and that mint fails; a GitHub 5xx or 429
  follows the retry budget and then fails the step with a clear reason.
- **Admission.** The Workflow validator rejects a missing Secret reference, an empty allowlist and a
  `waitForCallback` step in a `githubApp` Workflow.
- **Unit, API types.** A reflection check in `api/v1alpha1` that neither `WorkflowRunSpec` nor
  `WorkflowRunExecutionSpec` has the field, so a WorkflowRun cannot set it.
- **Integration (envtest).** TokenReview and SubjectAccessReview against the API server with a pod-bound
  broker-audience projected token, and a fake GitHub token endpoint.
- **Multi-replica (kind).** With three controller replicas, only the leader pod
  carries the broker-leader label and every runner request reaches it; after the leader is deleted, the new
  leader labels itself, the old label is gone, and runner requests succeed after retrying connection
  refusals.
- **Manual.** A real App against a test cluster, including a run that crosses the one-hour mark.

## Open questions

1. **Key custody and token delivery.** The key in the runner pod (Approach 1); the key with the controller,
   which pushes a short-lived token Secret to the run (Approach 3); or the key with the controller, which
   hands tokens to the runner through a broker (this proposal). This is the first decision for reviewers.
2. **`GitHubApp` CRD or Secret convention.** A Secret convention needs no new CRD or codegen. A CRD gives a
   status field for health.
3. **Where the reference and scope live.** Proposed: a Workflow-only field. Alternative: put it under
   `execution`, with admission rejecting any `WorkflowRun.spec.execution` value that is not a subset of the
   Workflow's; admission then has to read the Workflow, and that check can go stale when the Workflow is
   edited. Rejected: a run-only field like `clusterRef`, which #5 runs could not use.
4. **Scope bounds.** Is the 500-repository cap acceptable?
5. **First consumers.** Is the list under Consumers the right first set?
6. **Refresh.** Is a per-request pull with a retry budget enough across the three paths past one hour, and
   during GitHub outages and rate limits?
7. **Namespace rule.** Is requiring the key Secret in the Workflow's tenant namespace, behind a named get
   grant, acceptable to operators?
8. **`agentRef` consumers.** The agent-executor serves every run, so its pod token names no WorkflowRun. How does it prove which run a GitHub request is for, without forwarding a runner's API-server token (the agent-executor never does)?
