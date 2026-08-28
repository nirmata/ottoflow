# Proposal: Agent Execution Sandboxing

**Status**: Draft  
**Issue**: Tracked internally

## Problem

OttoFlow agent steps run LLM-driven tool loops that execute Model Context Protocol (MCP) tools. For stdio-transport MCP servers this launches commands such as `uvx` or `npx` that fetch and execute code at runtime — code whose selection is driven by the LLM, not pinned by the workflow author. The tool-call surface is untrusted; the loop that calls the model and parses the next action is trusted machinery and is out of scope here.

That untrusted execution runs today with limited isolation. Three attack surfaces matter, in rough order of practical risk:

- **(A) Reaching other workloads.** A hostile tool can attempt to talk to other pods/services in the cluster, or to external endpoints.
- **(B) Credential blast radius.** The pod that runs the tool also holds credentials the tool can read and reuse to move laterally.
- **(C) Container escape.** A hostile tool could try to exploit a kernel bug to break out of the container to the node.

Where untrusted code runs today (both paths run under restricted Pod Security Standards only, with no `runtimeClassName`):

1. `AgentRef` steps POST to the shared agent-executor Service; stdio MCP subprocesses spawn in the agent-executor pod (`cmd/agent-executor/main.go`, `internal/agent/mcp_client_impl.go`).
2. Direct `MCPToolCall` steps spawn stdio MCP subprocesses in the runner Job (`internal/workflow/executor/executor.go`).

The **agent-executor pod is the weak link for surface (B)**: it runs under a broadly-scoped ServiceAccount, mounts its SA token, holds the agent-executor TLS key, and receives forwarded LLM credentials — all co-resident with the untrusted tool.

## Current Controls

OttoFlow already ships partial mitigations; this proposal builds on them rather than starting from zero.

- **Least-privilege runner identity** — each run's runner Job uses a dedicated per-Workflow ServiceAccount (not the controller's); its secret access is get-by-name only (no list), with a narrow write scope (`charts/ottoflow/templates/clusterrole.yaml`, `internal/workflow/controller/workflowrun_controller.go`).
- **Secret-read denylist** — CEL `resource.*`, `resourceQuery`, and `mutate` steps are blocked from reading core Secret objects (`internal/workflow/executor/resource_denylist.go`).
- **Outbound egress guard** — CEL `http.*` and A2A calls are blocked from dialing loopback, link-local, and cloud-metadata addresses (`internal/workflow/executor/egress_guard.go`). Gap: it does not cover MCP stdio subprocesses (which make their own syscalls), and does not restrict reaching other in-cluster workloads by IP.
- **NetworkPolicy** — a chart NetworkPolicy exists (`charts/ottoflow/templates/networkpolicy.yaml`), but it selects only the controller pod; the agent-executor and runner pods are **not** covered, and its egress is broad.

Remaining gaps: the agent-executor ServiceAccount is broader than it needs to be; SA tokens are mounted in both execution pods; the pods running untrusted code have no NetworkPolicy; there is no per-execution or per-step isolation.

## Options Considered

The layers below are independent and can land incrementally; each targets a specific surface.

### Layer 1: Network isolation (surface A) (recommended — cheapest)

Extend the existing NetworkPolicy `podSelector` to cover the agent-executor and runner pods, and replace broad egress with a default-deny plus an explicit allowlist (API server, DNS, and the LLM/MCP endpoints actually needed).

**Pros**: Reuses a committed pattern; directly limits a hostile tool's reach to other workloads; no new dependency.  
**Cons**: Egress allowlists need care for legitimate `uvx`/`npx` package fetches and MCP/LLM endpoints; does not stop a tool that only uses the paths it is allowed.  
**Decision**: Recommended first — low cost, addresses the highest-practical-risk surface.

---

### Layer 2: Credential blast radius (surface B) (recommended — structural)

Two complementary moves:

- **Tighten the agent-executor identity** to least-privilege (scope down its secret access; stop mounting the SA token where it is not needed).
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
- Per-execution pods + identity tightening (Layer 2) are what shrink the credential blast radius.
- NetworkPolicy + egress restrictions (Layer 1) are what limit cross-workload reach.

## Files to Change

| File | Change |
|---|---|
| `charts/ottoflow/templates/networkpolicy.yaml` | Extend `podSelector` to the agent-executor + runner pods; tighten egress |
| `charts/ottoflow/templates/agent-executor-clusterrole.yaml` | Scope down the agent-executor ServiceAccount |
| `charts/ottoflow/templates/agent-executor-deployment.yaml` | Add `runtimeClassName` (Layer 3); reconsider the SA token mount |
| `internal/workflow/controller/workflowrun_controller.go` | Set `runtimeClassName` on the runner Job PodSpec |
| `charts/ottoflow/templates/deployment.yaml` | Add `--workflow-runner-runtime-class` arg so the value reaches `RunnerConfig` |
| `charts/ottoflow/values.yaml` | New values (network, runtime class), defaults preserving current behavior |
| `docs/dev/DESIGN.md` | On graduation, fold into the existing agent-sandbox section |

## Testing & Verification

- Helm render tests for the NetworkPolicy selector/egress and the `runtimeClassName` passthrough (present when set, absent by default).
- Controller unit test asserting `runtimeClassName` reaches the runner Job PodSpec.
- Manual / real-cluster checks (kind e2e has no `runsc`): a workflow with an MCP stdio tool call still succeeds under the tightened NetworkPolicy and under gVisor, and the agent-executor still functions with reduced SA scope.
- `make lint test`; `make manifests generate` only if an API type is added.

## Rollout & Backward Compatibility

- All layers default off / current-behavior-preserving; opt-in per layer.
- Node prerequisite for Layer 3: a gVisor/Kata `RuntimeClass` (documented for operators).
- Each layer is independently reversible.

## Future Work

- Per-step, short-lived minimal ServiceAccounts (not yet implemented).
- Extending the egress guard to cover MCP stdio subprocesses.
- Agent Substrate: revisit when its API stabilizes; the `ExternalAgentRef`→A2A path is a low-risk interop experiment in the meantime.
