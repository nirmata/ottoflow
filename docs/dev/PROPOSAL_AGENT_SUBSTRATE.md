# Proposal: Agent Substrate as an Agent Execution Layer

**Status**: Draft  
**Issue**: https://github.com/nirmata/ottoflow/issues/272 (tasks and status)

Keep this proposal and the issue in sync: when a PR changes the design, update this file in the same PR and edit the affected tasks in the issue.

## Problem

Every `agentRef` step in a cluster runs in one shared, long-lived process: the agent-executor Deployment. The runner Job POSTs the prompt and context to `https://{service}.{namespace}.svc:8443/api/exec/{agentNamespace}/{agentName}` (`internal/workflow/executor/exec_client.go:234-242`), and the agent-executor runs the LLM tool loop and the agent's MCP clients in-process, including stdio MCP subprocesses (`internal/agent/default_executor.go`, `internal/agent/mcp_client_impl.go`).

This has three consequences:

- **Shared credentials.** The LLM API keys, the agent-executor's ServiceAccount token, and its TLS key are co-resident with every tenant's untrusted tool execution (see surface B in [PROPOSAL_AGENT_SANDBOXING.md](PROPOSAL_AGENT_SANDBOXING.md)).
- **No per-execution isolation.** A hostile or buggy MCP tool in one run shares a kernel, a filesystem, and a network identity with every other run on the same replica.
- **No durable agent state.** An agent step is a single request and response. Long waits, such as an approval step between two agent steps, can't keep an agent's conversation and tool state warm without holding a pod.

[PROPOSAL_AGENT_SANDBOXING.md](PROPOSAL_AGENT_SANDBOXING.md) addresses the first two with network policy, credential tightening, and per-execution pods through `kubernetes-sigs/agent-sandbox`. It deferred Agent Substrate until its API stabilized. Substrate now declares 1.0 API, CLI, and configuration stability, so this proposal evaluates it as an execution backend.

## Background: Agent Substrate

[Agent Substrate](https://github.com/agent-substrate/substrate) (Apache-2.0) is an agent runtime for Kubernetes. The facts below reflect its documentation as of November 2026; verify them against the release you target.

- **Model.** Substrate schedules many stateful *actors* onto a smaller pool of warm *worker* pods. It snapshots an actor's memory and filesystem to suspend it, and restores it on any free worker. The project claims sub-500 ms resume and 10–30x oversubscription.
- **Isolation.** Each actor runs in gVisor (`runsc`) or a Cloud Hypervisor microVM. Plain Linux containers aren't supported.
- **Resources.** `WorkerPool` (`ate.dev/v1`) is a namespaced CRD for warm workers. `ActorTemplate` and `Actor` are created through the `ateapi` gRPC API, not as CRDs, and both belong to an *atespace*. An `ActorTemplate` requires digest-pinned images and accepts only literal `env` values.
- **Data path.** Callers send HTTP or gRPC to `atenet-router` with the header `ate-target-actor: <atespace>/<actor>`. The router authenticates callers by SPIFFE ID over mTLS, and holds requests rather than failing them while the pool is saturated.
- **Network and identity.** Actors are default-deny: no inbound path except through the router, and egress through `atenet-egress`. Each active actor has a short-lived SPIFFE certificate, and the egress gateway can mint per-actor JWTs and inject credentials into outbound requests, so the sandbox doesn't hold them. Actors can't read Kubernetes Secrets.
- **Lifecycle.** Substrate doesn't suspend idle actors on its own. The caller calls `SuspendActor`, and `DeleteActor` removes the actor and its snapshot.
- **Adoption.** [kagent](https://kagent.dev/docs/kagent/0.x/concepts/agent-substrate/) runs `SandboxAgent` and `AgentHarness` resources on Substrate, and Google's Agent Executor builds on it.

Substrate's main density gain comes from suspending idle actors. Most OttoFlow agent steps are short request and response calls, so for them the gains are per-execution isolation, credentials held outside the sandbox, and warm start. Suspend matters for run-scoped actors that wait on approvals or span several steps (Phase 4).

## Options Considered

### Option A: Call a kagent `SandboxAgent` through `externalAgentRef` (unverified)

Deploy the agent as a kagent `SandboxAgent` on Substrate, and call it from a workflow with an `externalAgentRef` (A2A) step.

**Pros**: No OttoFlow change, if it works.  
**Cons**: kagent documents invoking a `SandboxAgent` only through its UI, not at an A2A URL. A Substrate actor is reachable only through `atenet-router` with the `ate-target-actor` header, and `externalAgentRef` can't set custom headers, so this works only if kagent fronts the actor with an A2A endpoint. The agent is also defined in kagent, not as an OttoFlow `Agent`, so `mcpTools`, `outputExtraction`, token usage, and the agent-executor's authorization checks don't apply, and a plain `http://` URL must be cluster-local, because `externalAgentRef` allows HTTP only with `allowInsecureHTTP`, no `auth.secretRef`, and no `caSecretRef` (`internal/workflow/executor/a2a_url.go`).  
**Decision**: Verify whether kagent exposes an A2A endpoint for `SandboxAgent`s; if it does, document this as an interop pattern. Don't treat it as the execution layer.

### Option B: Runner calls Substrate directly

The runner creates an actor per step through `ateapi` and sends the exec request to `atenet-router`.

**Pros**: No extra hop.  
**Cons**: Every runner identity, in every namespace that runs WorkflowRuns, needs a SPIFFE ID on the router's ingress allowlist and `ateapi` permissions. Substrate credentials then sit in the runner pod, which already runs untrusted direct `MCPToolCall` steps.  
**Decision**: Not recommended.

### Option C: The agent-executor acts as a Substrate gateway (recommended)

The runner is unchanged and keeps calling `/api/exec`. The agent-executor authenticates and authorizes the caller as it does today (TokenReview and SubjectAccessReview), then reads the Agent. When `spec.executionMode` is `substrate`, it forwards the call to an actor instead of executing in-process.

**Pros**: One identity holds Substrate permissions; the runner, its NetworkPolicy, and the exec contract stay the same; `service` mode is untouched.  
**Cons**: One extra hop; the gateway must resolve everything the actor can't read itself (below).  
**Decision**: Recommended.

### Option D: Delegate to kagent (`executionMode: kagent`)

The controller renders each OttoFlow `Agent` as a kagent `SandboxAgent`, and the agent step calls it over A2A. kagent manages the actors.

| | Option C: native gateway | Option D: delegate to kagent |
|---|---|---|
| OttoFlow `Agent` meaning | Kept: same tool loop, `mcpTools` allowlist, `outputExtraction`, and token usage | Translated into kagent's ModelConfig and tool types; `outputExtraction` and token usage must be rebuilt or are lost |
| Dependencies | Substrate | Substrate and kagent; kagent is optional for OttoFlow today |
| Runtime | Every provider OttoFlow supports | kagent's Go declarative runtime only; kagent doesn't support its Python runtime on Substrate yet |
| Lifecycle work | OttoFlow calls `ateapi` and manages actors | kagent manages actors, golden snapshots, and suspend |
| Invocation path | Documented: router plus `ate-target-actor` | Undocumented (see Option A) |
| API maturity | Substrate 1.0 | kagent `v1alpha2`, with Substrate support in alpha |

**Decision**: Not recommended. It gives up the `Agent` behavior workflows rely on and makes kagent a requirement for every install.

## Proposed Design

### API

Add `executionMode` to `AgentSpec` (`api/v1alpha1/agent_types.go`), as `docs/dev/DESIGN.md` FR1B.7 already reserves:

```yaml
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Agent
spec:
  modelProvider: anthropic
  executionMode: substrate      # service (default) | sandbox | substrate
```

The Agent names neither an atespace nor an `ActorTemplate`. Every `substrate` actor runs the operator's executor template (see below), so an Agent author can't pick a different image and bypass the checks the executor runs. The operator maps each Kubernetes namespace to one atespace in the gateway's configuration (a chart value), and the gateway runs an Agent's actors only in the atespace mapped to the Agent's namespace. An Agent in a namespace with no mapping fails with a clear error. This stops an Agent in one namespace from running in another team's atespace and inheriting its egress and credential policy.

- `executionMode` defaults to `service`, so existing Agents keep today's behavior.
- `sandbox` stays reserved for [PROPOSAL_AGENT_SANDBOXING.md](PROPOSAL_AGENT_SANDBOXING.md) Layer 2; both backends share the gateway and standalone executor below.
- A validating webhook rejects `substrate` when the feature is off. The gateway enforces the namespace-to-atespace mapping at dispatch, so the check holds even if the mapping changes after admission.
- Only step-scoped actors are in scope here; a run-scoped `actorScope` field is Phase 4.

### Actor image: the agent-executor in standalone mode

The actor runs the agent-executor binary with a new standalone flag. In that mode it:

- Has no Kubernetes client, no ServiceAccount token, and no TLS server key. It listens on plain HTTP inside the sandbox; the router terminates mTLS.
- Accepts a resolved request: the prompt and context from today's `ExecRequest` (`internal/workflow/executor/exec_http_types.go:12-15`), plus the Agent spec and a resolved MCP client configuration for each MCPServer the agent's `mcpTools` reference. Today the MCP client resolves `spec.auth` Secrets through the Kubernetes client while it builds that configuration (`internal/agent/mcp_client_impl.go:209-215`, `:274-285`), which the actor can't do, so the gateway resolves it and the actor uses it as is. The CLI already runs agents from resolved objects with no cluster (`ottoflow run --workflow-dir`), so the standalone executor reuses that path.
- Runs the same `RoutingAgentExecutor` that `service` mode uses, so provider routing is unchanged (`DefaultAgentExecutor` alone rejects the `nirmata` and empty providers), plus the tool loop, MCP clients, and `OutputExtractor`, and returns today's `ExecResponse`.

The chart publishes an `ActorTemplate` for this image, pinned by digest, in each mapped atespace under one fixed name from the gateway's configuration. It does this as an install hook or a gateway startup step, because `ActorTemplate` isn't a CRD.

### Gateway dispatch

For a `substrate` Agent, the agent-executor's exec handler:

1. Authenticates and authorizes the caller, and reads the Agent and its MCPServers, exactly as today.
2. Looks up the atespace mapped to the Agent's namespace, and fails the call if there is none.
3. Calls `CreateActor` in that atespace from the executor template named in the gateway's configuration, never a name taken from the Agent or the request, with a generated name that starts with a fixed `ottoflow-` prefix.
4. POSTs the resolved request to `atenet-router` with `ate-target-actor: <atespace>/<actor>`, and waits for the response.
5. Calls `DeleteActor` when the call finishes, fails, or is cancelled, which also removes the actor's snapshot.
6. Returns the actor's `ExecResponse` to the runner unchanged.

The gateway needs a Go client for `ateapi`, generated from Substrate's protos, and a SPIFFE client identity on the router's ingress allowlist.

**Timeouts and retries.** `ExecRequest` doesn't carry the step's timeout, so the gateway is bounded only by the runner's request context: the runner's exec HTTP client times out after 30 minutes (`internal/workflow/executor/exec_client.go:38`), and a cancelled request cancels the forward and triggers `DeleteActor`. The exec endpoint isn't idempotent, and the runner retries only connection failures (`isRetryableExecErr`, `internal/workflow/executor/exec_client.go:313-320`). The gateway keeps that contract: it may retry `CreateActor`, since nothing has run yet, but once it has sent the request to the router it never re-sends it or creates a second actor for the same call.

**Orphaned actors.** If the gateway restarts during a step, its `DeleteActor` call never runs. On startup and then periodically, the gateway lists actors in its mapped atespaces and deletes any with the `ottoflow-` prefix that are older than the runner's 30-minute exec timeout, since no call can still be waiting on them. The client spike confirms that `ateapi` can list actors and report their age.

**Observability.** The gateway reports each failure, such as a `CreateActor` error, the router holding the request until the context ends, or the actor failing its wakeup probe, as an `ExecErrorResponse` whose message names the stage and the actor. The message reaches the step's status through the existing error path. The runner adds informational headers with the WorkflowRun namespace and name and the step name, and the gateway logs them with the actor name, so you can map an actor back to its run and step. Actor logs come from Substrate's own logging (see its observability guide).

### Credentials

- **Phase 2.** The gateway resolves LLM keys and MCPServer `spec.auth` Secrets and sends them in the resolved request, so they reach the actor for the life of one step.
- **Phase 3: LLM keys.** Move provider API keys out of the gateway's process environment and into `atenet-egress` `replaceHeaders` rules for the provider endpoints, with `credentialUri` values that the Substrate credential provider resolves (for example `ate-secret://k8s.io/default/<namespace>/<secret>/<key>`, with the atespace granted that namespace). The actor never holds the keys. This covers operator-held keys stored in Kubernetes Secrets.
- **Phase 3: per-run credentials.** Some credentials arrive per run in the `X-LLM-Env` header, not from a Secret. The `nirmata` provider's token is one: `internal/workflow/executor/exec_handler.go` reads `NIRMATA_LLM_TOKEN` from that header. An `ate-secret://` URI can't name a value that exists only in a request, so the gateway keeps passing these values into the step-scoped actor, as in Phase 2. See Open Questions.
- **Phase 3: remote MCP servers.** Static header credentials from MCPServer Secret references (`bearer`, `apiKey`, and `basic` auth) move to egress rules the same way as operator-held LLM keys.
- **Phase 3: OAuth2 MCP servers are an exception.** In `oauth2` mode, the MCP client gets a token URL and client credentials and fetches its own access token (`internal/agent/mcp_client_impl.go:310-348`). Replacing outbound headers can't keep that token out of the actor, so the client credentials keep reaching the step-scoped actor in Phase 3. See Open Questions.
- **Phase 3: actor-side configuration.** Injection replaces only a header the actor already sends, and a request without it is forwarded unchanged. So the gateway sends placeholder credentials in place of real ones, the actor-side LLM and MCP clients send them in the headers the rules name, and the actor never resolves Secrets. For HTTPS, the gateway intercepts TLS, so the `ActorTemplate` mounts the `systemInfo` egress trust bundle and the actor's LLM and MCP clients trust it.
- **stdio MCP servers that need Secret env values.** Substrate has no Secret injection, and env values are captured in snapshots. This proposal doesn't support such servers in `substrate` mode until the Open Question below is settled; the webhook rejects the combination.
- **Kubernetes access.** An actor can't reach the API server by default. MCP tools that need cluster credentials, such as a kubectl MCP server, don't fit `substrate` mode in Phase 2. The MCP client sets `KUBECONFIG` for stdio servers only when its context carries a kubeconfig path (`internal/agent/mcp_client_impl.go:177-178`). The runner sets one for a non-local `clusterRef`, but the agent-executor doesn't, so no kubeconfig reaches the actor.

### Network

The gateway's egress allowlist gains `ateapi` and `atenet-router`. Actor egress is governed by Substrate's egress gateway, not Kubernetes NetworkPolicy; the operator allows only the LLM and MCP endpoints the atespace needs.

### Policy

`ActorTemplate` and `Actor` aren't Kubernetes objects, so admission controllers such as Kyverno don't see them. Policy in `substrate` mode applies at three points:

- **Admission of OttoFlow objects.** Agents, MCPServers, and Workflows are still CRDs, so admission policies on them apply as in `service` mode. Only the gateway creates actors, and only from the chart's `ActorTemplate`, so users can't create actors directly.
- **Inside the actor.** The `mcpTools` allowlist and the other checks the executor runs at dispatch run inside the actor unchanged, because the actor runs the same executor code.
- **Network.** Substrate's egress policy for the atespace is the network control for the actor.

## Interaction with kagent

OttoFlow integrates with kagent in two directions today, and Substrate changes each differently.

- **kagent calls OttoFlow (`spec.expose.kagent`).** The exposure controller publishes each exposed Workflow as a kagent BYO `Agent` running `serve-a2a` (`internal/workflow/controller/exposure_controller.go`). It stays a BYO `Agent`, not a `SandboxAgent`: `serve-a2a` creates WorkflowRuns and calls TokenReview, which need a ServiceAccount token and API server access that an actor doesn't have, and it runs no untrusted code, so a sandbox adds nothing. The controller writes `kind: Agent` explicitly, so a change in kagent's defaults doesn't move it onto Substrate.
- **A kagent `SandboxAgent` calls an exposed Workflow.** The caller's traffic leaves through `atenet-egress`, so its atespace's egress policy must allow the `serve-a2a` Service. kagent doesn't document how the `headersFrom` Secret that carries the caller's token reaches an actor. This path is untested.
- **OttoFlow calls kagent (`externalAgentRef`).** Unchanged for kagent agents that run as Deployments. For `SandboxAgent`s, see Option A.
- **Shared Substrate install.** kagent and OttoFlow can share one `ate-system` control plane. Use separate atespaces so egress and credential policies stay apart. `ActorTemplate`s in different atespaces can share a `WorkerPool`, so OttoFlow can use kagent's `kagent-default` pool; the chart's `workerSelector` on the OttoFlow `ActorTemplate` picks the pool.

## Phases

1. **Interop (no code).** Check whether kagent exposes an A2A endpoint for `SandboxAgent`s, and if it does, document Option A with a sample workflow.
2. **Step-scoped actors.** The API field, webhook validation, standalone executor, gateway dispatch, `ateapi` client, and chart `ActorTemplate`. LLM keys and MCP auth values still reach the actor through the resolved request in this phase, but only for the life of one step, in a gVisor sandbox that holds no other tenant's state.
3. **Credentials outside the sandbox.** Move LLM keys and remote MCP auth to egress injection, send placeholders in the request, and mount the egress trust bundle in the actor.
4. **Run-scoped actors and suspend.** An `actorScope: run` field reuses one actor across a run's steps, suspends it while the run waits, and deletes it when the run finishes. It adds the WorkflowRun UID to the headers the runner sends from Phase 2.

## Files to Change

| File | Change |
|---|---|
| `api/v1alpha1/agent_types.go` | Add `executionMode`; run `make manifests generate codegen-docs` |
| `internal/webhook/agent_mcpserver_validators.go` | Reject `substrate` when the feature is off, and stdio MCP servers with Secret env in `substrate` mode |
| `cmd/agent-executor/main.go` | Standalone mode flag; Substrate gateway configuration |
| `internal/workflow/executor/exec_handler.go` | Branch on `executionMode`; map the namespace to an atespace; resolve Agent and MCPServers; forward to the router; reap orphaned actors |
| `internal/workflow/executor/exec_client.go` | Send the WorkflowRun and step headers |
| `internal/agent/` | New Substrate client (generated `ateapi` stubs plus create, delete, and forward) |
| `charts/ottoflow/` | Values for the router and `ateapi` endpoints, gateway SPIFFE identity, the namespace-to-atespace mapping, `ActorTemplate` publishing; all off by default |
| `docs/dev/DESIGN.md` | Add `substrate` to the execution modes |
| `samples/workflows/` | One `substrate` Agent sample; one Option A sample |

## Testing and Verification

- Unit tests: webhook validation; gateway dispatch with a fake `ateapi` client and router, including `DeleteActor` on success, failure, and cancellation, no re-send after the request reaches the router, refusal of a namespace with no atespace mapping, and the orphan reaper; the standalone executor with resolved Agent and MCPServer specs and no Kubernetes client.
- e2e on kind with Substrate's kind installer and gVisor: an `agentRef` step on a `substrate` Agent returns the same outputs and token usage as in `service` mode, and no actor is left after the run.
- Manual: confirm an actor can't reach the Kubernetes API or another actor.
- Phase 3 e2e: with injection enabled, HTTPS LLM and authenticated MCP calls succeed with only placeholders inside the actor, and fail closed when the atespace isn't granted the Secret's namespace.
- Measure step latency against `service` mode, including `uvx` and `npx` stdio servers under gVisor, and decide whether a golden snapshot (`goldenSnapshotConfig`) taken after warm-up is needed.

## Rollout and Backward Compatibility

- `executionMode` defaults to `service`; Agents without it behave as today.
- `substrate` mode requires an installed Substrate, a `WorkerPool` on gVisor or microVM nodes, and an atespace per tenant; the chart doesn't install Substrate.
- The feature is off in the chart by default; with it off, the webhook rejects `executionMode: substrate`.
- The generated `ateapi` client targets one Substrate minor release at a time, starting with the release the client spike picks. The chart documents the supported release, and moving to a new minor release is an explicit upgrade with its own e2e run.

## Open Questions

1. **Secrets for stdio MCP servers.** Options: send Secret values in the resolved request for step-scoped actors only (they're deleted with the actor, but they enter the sandbox and its snapshot); serve them from a file-based provider through Substrate's `external` volumes (preview); or keep rejecting them.
2. **Credentials Phase 3 doesn't cover.** How should per-run values from `X-LLM-Env`, such as the `nirmata` provider's token, and OAuth2 MCP client credentials stay out of the actor? Options: a custom Substrate credential provider that the gateway registers a per-call value with, a gateway-side token broker that fetches OAuth2 access tokens and injects them as headers, or accepting that these values enter the step-scoped actor.
3. **Backend choice.** Should OttoFlow implement both `sandbox` (one pod per execution, more mature) and `substrate` (shared workers, snapshots), or pick one? This proposal keeps both behind one gateway so the choice is per Agent.
4. **Snapshot storage.** kagent's defaults are GCS paths. Which object stores do installs need, and should Phase 4 be blocked on S3 support?
5. **Worker pool selection.** The chart-set `workerSelector` picks one pool for every OttoFlow Agent. Does any install need a per-Agent pool, for example to separate OttoFlow and kagent capacity, which would add a `workerPool` field?

## Future Work

- Expose run-scoped actors to `serve-a2a`, so long A2A tasks survive waits without holding a pod.
- Run direct `MCPToolCall` stdio servers as actors, closing the runner-side surface in [PROPOSAL_AGENT_SANDBOXING.md](PROPOSAL_AGENT_SANDBOXING.md).
- Per-actor identity in audit logs, using the actor JWT's `sub: actor/<atespace>/<name>`, alongside [PROPOSAL_PER_STEP_SA.md](PROPOSAL_PER_STEP_SA.md).
