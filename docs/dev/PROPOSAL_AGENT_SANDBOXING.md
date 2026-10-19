# Proposal: Agent Execution Sandboxing

**Status**: Draft  
**Issue**: Tracked internally

## Problem

OttoFlow agent steps run LLM-driven tool loops that call Model Context Protocol (MCP) tools. The workflow author fixes what can run: each stdio server's command (e.g. `uvx <package>`) is set in `MCPServer.spec.transport.command`, an agent's callable tools are an allowlist (`Agent.spec.mcpTools`, enforced at dispatch), and direct `MCPToolCall` steps name the server and tool with no LLM involvement. The LLM does not choose which package or binary runs — only which allowlisted tool to call and the arguments to pass.

The execution is still untrusted for two reasons: (a) supply chain — an author-configured command without a version pin (`uvx pkg` vs `uvx pkg==1.2.3`) can fetch and run different, mutable code on each pod start; and (b) untrusted inputs — an allowlisted tool runs with arguments that may be LLM-generated or CEL-derived from upstream context, so a tool that mishandles those inputs is a live injection surface. The loop that calls the model and parses the next action is trusted machinery and is out of scope here.

That untrusted execution runs today with limited isolation. Three attack surfaces matter, in rough order of practical risk:

- **(A) Reaching other workloads.** A hostile tool can attempt to talk to other pods/services in the cluster, or to external endpoints.
- **(B) Credential blast radius.** The pod that runs the tool also holds credentials the tool can read and reuse to move laterally.
- **(C) Container escape.** A hostile tool could try to exploit a kernel bug to break out of the container to the node.

Where untrusted code runs today (both paths run under restricted Pod Security Standards only, with no `runtimeClassName`):

1. `AgentRef` steps POST to the shared agent-executor Service; stdio MCP subprocesses spawn in the agent-executor pod (`cmd/agent-executor/main.go`, `internal/agent/mcp_client_impl.go`).
2. Direct `MCPToolCall` steps spawn stdio MCP subprocesses in the runner Job (`internal/workflow/executor/executor.go`).

The **agent-executor pod is the weak link for surface (B)**: it mounts its SA token, holds the agent-executor TLS key, and receives forwarded LLM credentials (`internal/workflow/executor/exec_client.go:57-63`) — all co-resident with the untrusted tool. Its Secret access is zero by default; any named Secret an operator grants it under `rbac.secretAccess.agentExecutor` is reachable with that token as well (`charts/ottoflow/templates/rbac-secret-access-role.yaml:26-29`).

## Current Controls

OttoFlow already ships partial mitigations; this proposal builds on them rather than starting from zero. File and line references in this document are to `main` at `9238d134`.

- **Runner identity separate from the controller** — by default each runner Job runs as a per-Workflow ServiceAccount, which the controller binds through a ClusterRoleBinding to the aggregated `<release>-runner-role` ClusterRole instead of the controller's role (`internal/workflow/controller/workflowrun_controller.go:1057-1068`, `:501-511`; `charts/ottoflow/templates/clusterrole.yaml:502-518`). The operator can configure one shared runner ServiceAccount instead, and a WorkflowRun can name its own with `spec.execution.job.serviceAccountName`, which the controller does not bind (`:461-464`, `:1092-1094`).
- **No Secret RBAC for runners by default** — neither ClusterRole the chart labels to aggregate into the runner role has a `secrets` rule (`charts/ottoflow/templates/clusterrole.yaml:257`, `:424`); `rbac.*.extraResources` refuses `secrets` at render time (`charts/ottoflow/templates/_helpers.tpl:344-356`); and the opt-in named, namespaced, get-only grants under `rbac.secretAccess` exist only for the controller and the agent-executor (`charts/ottoflow/templates/rbac-secret-access-role.yaml:26-29`). An operator's own ClusterRole carrying the runner aggregation label can still add Secret access (`charts/ottoflow/README.md:221`). MCP env/auth Secrets a workflow references reach the runner pod as Secret volumes the controller adds to the Job (`internal/workflow/controller/secret_refs.go:656`), and the runner reads them from those files rather than through the Secret API (`internal/workflow/executor/executor.go:59-61`); LLM credentials arrive as `secretKeyRef` environment variables (`internal/workflow/controller/workflowrun_controller.go:756-764`).
- **A2A transport checks** — an `externalAgentRef` URL must be `https://`, or `http://` only with `allowInsecureHTTP`, no `auth.secretRef` or `caSecretRef`, and a cluster-local host (`.svc`, `.svc.cluster.local`, `localhost`, `127.0.0.1`, `::1`) (`internal/workflow/executor/a2a_url.go:29-64`); the A2A clients refuse redirects (`internal/workflow/executor/a2a_client.go:165-167`, `:193-195`). These check the URL, not the destination address: any HTTPS host is accepted, and loopback is reachable over HTTP under those `http://` conditions.
- **NetworkPolicy** — a chart NetworkPolicy exists (`charts/ottoflow/templates/networkpolicy.yaml`), but it is created in the install namespace and selects only the controller pod (`networkpolicy.yaml:6`, `:11-13`); the agent-executor and runner pods are **not** covered, and its egress allows TCP 443 to any destination (`charts/ottoflow/values.yaml:552-555`).

Remaining gaps:

- **Runner ServiceAccounts do not isolate workflows from each other.** Every per-Workflow ServiceAccount is bound to the same ClusterRole (`internal/workflow/controller/workflowrun_controller.go:508`), so each per-Workflow ServiceAccount holds the same permissions in every namespace — for example ConfigMap create, delete, get, list, patch, update and watch (`charts/ottoflow/templates/clusterrole.yaml:448-460`).
- **Mounted credentials are readable by the tool.** A tool in the runner pod can read every Secret mounted into that pod, the LLM credential environment variables, and the pod's SA token, which is mounted (`internal/workflow/controller/workflowrun_controller.go:1273`); the agent-executor pod has the same exposure (see Problem).
- **No Secret-read denylist.** CEL `resource.*`, `resourceQuery` and `mutate` steps fetch whatever kind they are given, Secrets included (`internal/workflow/executor/resource_context.go:56-91`, `:104-156`; `internal/workflow/executor/resource_query_executor.go:252`, `:296-317`; `internal/workflow/executor/mutate_executor.go:80-87`). Such reads are bounded only by the RBAC of the identity the step's client uses. The default runner ServiceAccount has no Secret rule (above); for an explicit `spec.execution.job.serviceAccountName` the controller creates no binding (`internal/workflow/controller/workflowrun_controller.go:461-464`), so its Secret access is whatever its own bindings grant.
- **No outbound address guard.** CEL `http.*` uses `http.DefaultTransport`, or a clone of it when a CA bundle is set (`internal/workflow/executor/http_client_slack_ok.go:89-99`, `:164-186`), and A2A builds a plain `http.Transport` (`internal/workflow/executor/a2a_client.go:155`); neither filters loopback, link-local or cloud-metadata addresses.
- The pods running untrusted code have no NetworkPolicy, and there is no per-execution or per-step isolation.

## Options Considered

The layers below are independent and can land incrementally; each targets a specific surface.

### Layer 1: Network isolation (surface A) (recommended — the NetworkPolicy rules are the cheapest step)

Two parts:

- **NetworkPolicy for the pods that run untrusted code.** The agent-executor runs in the install namespace, so the chart can cover it there. Runner Jobs are created in the WorkflowRun's namespace (`internal/workflow/controller/workflowrun_controller.go:1255`), and a NetworkPolicy selects pods only in its own namespace, so broadening the install-namespace policy's `podSelector` cannot cover runners elsewhere. Proposed: the operator lists the namespaces runner Jobs may run in (a values list, following the per-namespace list pattern of `rbac.secretAccess`); the chart renders a runner NetworkPolicy into each listed namespace; and while this layer is enabled the controller refuses to create a runner Job in an unlisted namespace and fails the WorkflowRun with that reason (fail closed). Each policy replaces broad egress with a default-deny plus an explicit allowlist (API server, DNS, and the LLM/MCP endpoints actually needed). This adds no controller RBAC; alternatives are under Open Questions.
- **Outbound address guard (new code).** A dial-time check on the CEL `http.*` and A2A transports that refuses loopback, link-local (including cloud-metadata) and unspecified addresses, applied to the resolved IP. It covers only these in-process Go clients; MCP stdio subprocesses open their own sockets and are covered only by NetworkPolicy and Layer 2.

**Pros**: Reuses a committed pattern; directly limits a hostile tool's reach to other workloads; no new dependency.  
**Cons**: Egress allowlists need care for legitimate `uvx`/`npx` package fetches and MCP/LLM endpoints; the runner-namespace list must track where WorkflowRuns are created; does not stop a tool that only uses the paths it is allowed.  
**Decision**: Recommended first — the NetworkPolicy rules are a chart change, the cheapest step in this proposal, and address the highest-practical-risk surface. The controller's namespace refusal and the address guard are separate new code.

---

### Layer 2: Credential blast radius (surface B) (recommended — structural)

Three complementary moves:

- **Tighten the agent-executor identity** to least-privilege. Its Secret access is already opt-in (named, get-only); beside the TokenReview and SubjectAccessReview create it uses to authenticate and authorize callers (`charts/ottoflow/templates/agent-executor-clusterrole.yaml:27-40`), what remains is its cluster-wide read of Agent and MCPServer objects (`charts/ottoflow/templates/agent-executor-clusterrole.yaml:9-26`) and the SA token mount, which should stop where it is not needed.
- **Secret denylist (new code)** — refuse core Secret objects in CEL `resource.*`, `resourceQuery` and `mutate` whatever the RBAC allows, so a runner identity that can read Secrets does not expose them through these steps.
- **Per-execution isolation** via `kubernetes-sigs/agent-sandbox` (`executionMode: sandbox`, already noted in `docs/dev/DESIGN.md`): run untrusted tool execution in its own short-lived pod so it is not co-resident with long-lived shared credentials. Optionally pair with short-lived per-step identities (a separate, not-yet-implemented proposal).

**Pros**: Directly shrinks what a hostile tool can read and reuse — the highest-value structural change.  
**Cons**: Per-execution pods are a larger change (new lifecycle, controller work); agent-sandbox is still maturing.  
**Decision**: Recommended as the primary structural win; the identity tightening is a cheap immediate step.

---

### Layer 3: Container escape (surface C) (complementary hardening)

Set `runtimeClassName` (gVisor/Kata) on the agent-executor and runner pods, gated by a Helm value (default off) and plumbed through controller runner config (including a `--workflow-runner-runtime-class` arg on the controller Deployment).

**Pros**: Contains kernel/container escape for all in-pod processes, including `uvx`/`npx` subprocesses; small, reversible, mature runtime (gVisor powers Cloud Run / App Engine gen1).  
**Cons**: Node prerequisite (a gVisor/Kata `RuntimeClass`); syscall/IO overhead — `uvx`/`npx` fetch and install packages at startup, the IO-heavy path gVisor slows, which interacts with the existing lazy-connect timeout and should be measured; a global runner setting taxes every runner Job (a per-workflow opt-in via `spec.execution.job` is a possible refinement).  
**Decision**: Worth doing, but as complementary hardening for a narrow surface — **not** the highest-value change.

## Isolation Scope

No single layer is sufficient; the value is in combining them.

- gVisor / `runtimeClassName` contains kernel/container escape only. It does **not** stop a tool from reading credentials in its own pod (surface B) or reaching other workloads (surface A) — it is node/kernel isolation, not intra-pod, not network.
- Per-execution pods, identity tightening and the Secret denylist (Layer 2) are what shrink the credential blast radius.
- NetworkPolicy + egress restrictions (Layer 1) are what limit cross-workload reach.

## Files to Change

| File | Change |
|---|---|
| `charts/ottoflow/templates/networkpolicy.yaml` | Cover the agent-executor pod; render a runner policy into each listed runner namespace; default-deny egress plus allowlist |
| `charts/ottoflow/templates/agent-executor-clusterrole.yaml` | Narrow the cluster-wide Agent/MCPServer reads |
| `charts/ottoflow/templates/agent-executor-deployment.yaml` | Add `runtimeClassName` (Layer 3); reconsider the SA token mount |
| `internal/workflow/controller/workflowrun_controller.go` | Refuse runner Jobs in unlisted namespaces while Layer 1 is enabled; set `runtimeClassName` on the runner Job PodSpec |
| `charts/ottoflow/templates/deployment.yaml` | Pass the runner-namespace list and a `--workflow-runner-runtime-class` arg so both reach `RunnerConfig` |
| `cmd/controller/main.go` | Parse the runner-namespace list and runtime-class flags into `RunnerConfig` |
| `charts/ottoflow/values.yaml` | New values (runner-namespace list, network, runtime class), defaults preserving current behavior |
| `internal/workflow/executor/http_client_slack_ok.go`, `internal/workflow/executor/a2a_client.go` | Dial-time address guard on the CEL `http.*` and A2A transports (new code) |
| `internal/workflow/executor/resource_context.go`, `resource_query_executor.go`, `mutate_executor.go` | Refuse core Secret reads (new code) |
| `docs/dev/DESIGN.md` | On graduation, fold into the existing agent-sandbox section |

## Testing & Verification

- Helm render tests for the NetworkPolicy selector/egress (one runner policy per listed namespace, none when the list is empty, the agent-executor covered) and the `runtimeClassName` passthrough (present when set, absent by default).
- Controller unit tests: `runtimeClassName` reaches the runner Job PodSpec; with Layer 1 enabled, a WorkflowRun in an unlisted namespace fails without a Job being created, and one in a listed namespace proceeds.
- Unit tests for the address guard (loopback, link-local, the metadata address and a hostname resolving to loopback are refused) and for the Secret denylist (Secret get refused in all three step types, and Secret list refused in CEL `resource.*` and `resourceQuery`).
- Manual / real-cluster checks (kind e2e has no `runsc`): a workflow with an MCP stdio tool call still succeeds under the tightened NetworkPolicy and under gVisor, and the agent-executor still functions with reduced SA scope. Cross-namespace, on a CNI that enforces NetworkPolicy: a runner in a listed namespace other than the install namespace gets the restricted egress, and a WorkflowRun in an unlisted namespace is refused.
- `make lint test`; `make manifests generate` only if an API type is added.

## Rollout & Backward Compatibility

- All layers default off / current-behavior-preserving; opt-in per layer.
- With Layer 1 enabled, runner Jobs run only in the listed namespaces; installs that create WorkflowRuns elsewhere must list those namespaces first.
- Node prerequisite for Layer 3: a gVisor/Kata `RuntimeClass` (documented for operators).
- Each layer is independently reversible.

## Open Questions

1. **Runner NetworkPolicy across namespaces.** Proposed above: an operator-listed set of runner namespaces, a chart-rendered policy in each, and a fail-closed refusal elsewhere. Alternatives: (a) the controller creates a NetworkPolicy in each runner namespace when it creates the runner Job, as it already does for the runner ServiceAccount; this needs cluster-wide NetworkPolicy create/update on the controller identity. (b) Allow runner Jobs only in the install namespace while Layer 1 is enabled; simplest, but drops WorkflowRuns in other namespaces.
2. **Address-guard scope.** Proposed above: refuse loopback, link-local and unspecified addresses on both transports, which also refuses A2A's `allowInsecureHTTP` loopback hosts while the guard is on. Alternatives: keep a loopback exception for `allowInsecureHTTP`; or also refuse private (RFC 1918) ranges, which would typically refuse the in-cluster Service addresses A2A and CEL `http.*` reach today, so this proposal leaves in-cluster reach to NetworkPolicy.
3. **Secret denylist or RBAC alone.** Proposed above: a code-level denylist. Alternative: rely on RBAC alone, since the default runner ServiceAccount has no Secret rule; that leaves runner ServiceAccounts with their own Secret bindings (an explicit `spec.execution.job.serviceAccountName`, a configured shared runner ServiceAccount, or bindings added directly) and operator-added aggregated rules as the exposure.
4. **Defaults for the new code.** This proposal keeps every layer opt-in (Rollout). The address guard and the Secret denylist need no node or cluster prerequisite, so they could instead ship on by default, as the chart already grants runners and the agent-executor no Secret RBAC by default; that would refuse calls that work today.

## Future Work

- Per-step, short-lived minimal ServiceAccounts (not yet implemented).
- Egress filtering for MCP stdio subprocesses: the proposed address guard covers only in-process Go clients, so these subprocesses stay covered only by NetworkPolicy and per-execution pods.
- Agent Substrate: revisit when its API stabilizes; the `ExternalAgentRef`→A2A path is a low-risk interop experiment in the meantime.
