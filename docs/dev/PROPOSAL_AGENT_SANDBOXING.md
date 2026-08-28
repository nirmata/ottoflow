# Proposal: Agent Execution Sandboxing

**Status**: Draft  
**Issue**: Tracked internally

## Problem

OttoFlow agent steps run LLM-driven tool loops that execute Model Context Protocol (MCP)
tools. For stdio-transport MCP servers this launches commands such as `uvx` or `npx` that fetch
and execute code at runtime — code whose selection is driven by the LLM, not pinned by the
workflow author. Today that execution has no kernel-level isolation:

1. **Cluster mode, `AgentRef` steps**: POST to the shared agent-executor Service, and stdio MCP
   subprocesses spawn inside the agent-executor pod (`cmd/agent-executor/main.go`,
   `internal/agent/mcp_client_impl.go`).
2. **Direct `MCPToolCall` steps**: spawn stdio MCP subprocesses inside the runner Job
   (`internal/workflow/executor/executor.go`).
3. **Shared kernel boundary**: both the agent-executor pod
   (`charts/ottoflow/templates/agent-executor-deployment.yaml`) and the runner Job
   (`internal/workflow/controller/workflowrun_controller.go`) run under restricted Pod Security
   Standards (non-root, `seccompProfile: RuntimeDefault`, all capabilities dropped) but share the
   host kernel and carry no `runtimeClassName`. Restricted-PSS is syscall/privilege hardening,
   not a sandbox boundary.

Blast radius: untrusted, runtime-fetched code runs in a pod that also holds the pod's
ServiceAccount token, the agent-executor TLS key, and (via the `X-LLM-Env` header) forwarded LLM
credentials. A compromised or malicious tool could attempt a kernel exploit to break out of the
container and affect the node or co-located workloads.

**Scope note**: default `ko`-built images are static and ship no Python/Node runtime, so
`uvx`/`npx`-based stdio tools require an image carrying those runtimes. The knob differs by path:
for `AgentRef` (agent-executor) it is the agent-executor image (`agentExecutorImage` / the Agent
`executorImage` field); for direct `MCPToolCall` (runner Job) it is the runner image
(`RunnerImage`, or a per-run `spec.execution.job.image` override) — `executorImage` does not
affect the runner Job. The threat applies wherever such tools are enabled.

## Options Considered

### Option A: Native `runtimeClassName` (gVisor / Kata) (recommended — Phase 1)

Run the agent-executor pod and the runner Job under a sandboxed container runtime by setting
`runtimeClassName` on their PodSpecs, gated by a Helm value and plumbed through controller
runner config (default off — see Rollout & Backward Compatibility).

**Pros**: Kernel/node isolation for all processes in the pod, including fork/exec'd
`uvx`/`npx` subprocesses (a pod-level `runtimeClassName` applies to every container and
subprocess). Minimal, reversible change — no data-plane or API contract change. Uses a mature,
widely-deployed runtime (gVisor powers Cloud Run / App Engine gen1); no new control plane.  
**Cons**: Requires a `RuntimeClass` (e.g. `gvisor`/`kata`) installed on the nodes — an operator
prerequisite. gVisor adds syscall/IO overhead and has edge-case incompatibilities (`io_uring`
off by default, partial iptables) — not fatal to stdio pipes + HTTPS egress, but real. Also note
`uvx`/`npx` fetch and install packages at startup — an IO-heavy path gVisor slows; combined with
the existing lazy-connect-on-first-use behavior (present precisely because `uvx` startup is
slow), sandboxing may raise first-tool-call latency and should be measured. Node
isolation only, not intra-pod (see Isolation Scope below).  
**Decision**: Recommended as Phase 1 — highest-value, lowest-cost step.

---

### Option B: `kubernetes-sigs/agent-sandbox` (`executionMode: sandbox`) (recommended — Phase 2)

Adopt the SIG-track agent-sandbox API already named as a future enhancement in
`docs/dev/DESIGN.md`. Provides gVisor/Kata backends, per-execution Sandbox pods, and a
`SandboxWarmPool` for warm starts.

**Pros**: Standards-track, stable-API path (unlike a v0.0.0 project). Per-execution isolation
closes the intra-pod gap — the untrusted tool no longer co-resides with long-lived shared
credentials. Warm pool addresses cold-start; already anticipated by OttoFlow's `executionMode`
design.  
**Cons**: Larger change — new execution mode, per-execution pod lifecycle, controller work.
Still maturing.  
**Decision**: Recommended as Phase 2 — the fuller isolation story; extends the documented
direction.

---

### Option C: Agent Substrate under the exec / A2A seam (deferred)

Host agent execution as Agent Substrate "actors" (per-actor gVisor/micro-VM sandboxes,
snapshot/restore, warm pools) beneath the agent-executor boundary.

**Pros**: Per-actor sandboxing plus warm-restore-from-snapshot and optional persistent actor
memory.  
**Cons**: Agent Substrate is v0.0.0 / not production-ready / API "almost guaranteed to change."
Does not compose with `AgentRef` as-is — the exec URL is hardcoded
(`https://{svc}.{ns}.svc:8443/api/exec/...`, `internal/workflow/executor/exec_client.go`) and
reaching an arbitrary endpoint is blocked by that hardcoding, not by CA exclusivity — the client
appends the controller-generated CA to the system cert pool (trusting system roots plus that CA).
Targeting a Substrate actor therefore requires an in-namespace Service (with a certificate the
client trusts) or code changes. Snapshot/restore
resets live TCP sockets (connected sockets return `ECONNRESET`; apps must reconnect), reducing
the persistent-memory benefit for a session with live MCP/LLM connections. Warm-start and
persistent-memory benefits are marginal here — the agent-executor is a lean, stateless Go binary
(`ExecRequest.Context` is intentionally never populated, `exec_client.go`).  
**Decision**: Deferred — revisit when the API stabilizes. Note: OttoFlow's existing
`ExternalAgentRef` step already calls any external A2A endpoint
(`internal/workflow/executor/external_agent_executor.go`, `a2a_url.go`), providing a
zero-core-change path to experiment with a Substrate-hosted actor that speaks A2A.

---

## Implementation (Option A)

- Add a Helm value (e.g. `agentExecutor.runtimeClassName` and a runner equivalent) — unset by
  default, preserving current behavior.
- Template `runtimeClassName` onto the agent-executor Deployment PodSpec
  (`charts/ottoflow/templates/agent-executor-deployment.yaml`).
- Plumb a runtime-class setting into the controller's runner-Job builder so the runner Job
  PodSpec (`internal/workflow/controller/workflowrun_controller.go`) sets `runtimeClassName`
  when configured.
- Do NOT add a per-Agent CRD field in Phase 1: in shared-service mode one Deployment serves all
  Agents, so a per-Agent runtime class would be dead config. Per-execution runtime selection
  belongs with Phase 2 (`executionMode`).
- Add a `--workflow-runner-runtime-class` arg to the controller Deployment template
  (`charts/ottoflow/templates/deployment.yaml`), mirroring the existing `--workflow-runner-*`
  args, so the value reaches `RunnerConfig` (`cmd/controller/main.go`) and the runner-Job
  builder. Without this template wiring the controller flag stays at its zero value and runner
  Jobs are created unsandboxed.
- A global runner runtime class applies gVisor to every runner Job — including runs that spawn
  no untrusted code (pure ResourceQuery / CEL / Mutate / Prometheus). Phase 1 keeps the global
  setting for simplicity; a per-workflow/per-run opt-in (via `spec.execution.job`) is a possible
  refinement to scope the overhead, called out as a trade-off.

## Isolation Scope

gVisor via `runtimeClassName` provides node/kernel isolation: it contains a container breakout
so untrusted code cannot exploit the host kernel to reach the node or other workloads. It does
NOT provide intra-pod isolation: an untrusted tool still shares its pod with the ServiceAccount
token, the TLS key, and forwarded LLM credentials, and can read them. Closing that residual gap
requires per-execution isolation (Option B) combined with per-step identity (see
`docs/dev/PROPOSAL_PER_STEP_SA.md`). Phase 1 is a real and worthwhile reduction in blast radius,
not a complete isolation solution — stated explicitly to avoid overclaiming.

## Files to Change

| File | Change |
|---|---|
| `charts/ottoflow/values.yaml` | Add `runtimeClassName` value(s), default unset |
| `charts/ottoflow/templates/agent-executor-deployment.yaml` | Template `runtimeClassName` onto the PodSpec |
| `internal/workflow/controller/workflowrun_controller.go` | Set `runtimeClassName` on the runner Job PodSpec from runner config |
| `cmd/controller/main.go` | Plumb the value from controller flags into `RunnerConfig` |
| `charts/ottoflow/templates/deployment.yaml` | Add `--workflow-runner-runtime-class` arg so the runner runtime class reaches `RunnerConfig` |
| user-facing install/operator docs (e.g. under `docs/`) | Document the gVisor/Kata `RuntimeClass` node prerequisite |
| `docs/dev/DESIGN.md` | On graduation, fold into the existing agent-sandbox section |

## Testing & Verification

- Helm template render test asserting `runtimeClassName` appears when the value is set and is
  absent by default.
- Controller unit test on the runner-Job builder asserting `runtimeClassName` passthrough.
- Manual / real-cluster verification (kind e2e has no `runsc`): confirm
  `kubectl get pod -o jsonpath='{.spec.runtimeClassName}'` and that an MCP stdio tool call still
  succeeds under gVisor.
- `make lint test`; `make manifests generate` only if an API type is added (none in Phase 1).

## Rollout & Backward Compatibility

- Default off — existing deployments unchanged.
- Operator prerequisite: install a `gvisor`/`kata` `RuntimeClass` on the nodes (documented).
- Rollback: unset the value; no data-plane or state change.

## Future Work

- Phase 2: `agent-sandbox` `executionMode: sandbox` with `SandboxWarmPool` (per-execution
  isolation + warm start).
- Combine with per-step ServiceAccount identity (`docs/dev/PROPOSAL_PER_STEP_SA.md`) to
  minimize credentials reachable from a sandboxed execution.
- Agent Substrate: revisit when its API stabilizes; the `ExternalAgentRef`→A2A path is the
  low-risk interop experiment in the meantime.
