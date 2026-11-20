# Proposal: OpenShell as the Agent Sandbox Backend

**Status**: Draft  
**Supersedes**: the Agent Substrate proposal in https://github.com/nirmata/ottoflow/pull/271 as the primary path; that design remains the fallback (see [Relationship to the Agent Substrate proposal](#relationship-to-the-agent-substrate-proposal))  
**Issue**: https://github.com/nirmata/ottoflow/issues/272

## Problem

Every `agentRef` step in a cluster runs in one shared, long-lived process: the agent-executor Deployment. The runner Job POSTs the prompt and context to `/api/exec/{agentNamespace}/{agentName}` (`internal/workflow/executor/exec_client.go`), and the agent-executor runs the LLM tool loop and the agent's MCP clients in-process, including stdio MCP subprocesses (`internal/agent/default_executor.go`, `internal/agent/mcp_client_impl.go`).

As a result:

- **Shared credentials.** LLM API keys, MCP credentials, the agent-executor's ServiceAccount token, and its TLS key sit next to every tenant's untrusted tool execution (surface B in [PROPOSAL_AGENT_SANDBOXING.md](PROPOSAL_AGENT_SANDBOXING.md)).
- **No per-execution isolation.** A hostile or buggy MCP tool in one run shares a kernel, a filesystem, and a network identity with every other run on the same replica.

[PROPOSAL_AGENT_SANDBOXING.md](PROPOSAL_AGENT_SANDBOXING.md) Layer 2 recommends per-execution sandboxes through `kubernetes-sigs/agent-sandbox`, and `docs/dev/DESIGN.md` reserves `executionMode: sandbox` for it. This proposal fills that reserved mode with OpenShell.

## Background: OpenShell

[OpenShell](https://github.com/NVIDIA/OpenShell) (Apache-2.0) runs agents in sandboxes that a gateway manages. The facts below reflect v0.1.8 and the spike in this proposal; the project releases about weekly at v0.1.x, so verify them against the release you target.

- **Gateway and drivers.** A stateful gateway exposes a gRPC API (`CreateSandbox`, `DeleteSandbox`, `ExecSandbox`, `ForwardTcp`, providers, and policy). Compute drivers run the sandboxes: Docker, Podman, Kubernetes, and MicroVM today. The Kubernetes driver creates `agent-sandbox` `Sandbox` resources and needs a CNI that enforces NetworkPolicy.
- **Supervisor.** Each sandbox runs a supervisor that enforces policy: an egress proxy with L7 REST inspection, a per-path binary allowlist, and Landlock and seccomp.
- **Providers and profiles.** A *profile* names a credential, its auth style (for example `bearer`), its endpoints with an access preset (`read-only`, `read-write`, or `full`), and the binaries that may use it. A *provider* holds the credential value. The sandbox sees only a placeholder such as `openshell:resolve:env:MCP_TOKEN`, and the proxy swaps in the real value for requests to the bound endpoints only. The gateway can also mint OAuth2 client-credentials tokens itself (`--strategy oauth2-client-credentials`).
- **Go SDK.** `github.com/ai-openshell/sdk-go`, versioned with OpenShell releases. It requires Go 1.26, which OttoFlow already uses.
- **Substrate.** A Substrate compute driver is planned out of tree (https://github.com/NVIDIA/OpenShell/issues/3362). The microVM sandbox class qualifies on Substrate 1.0, and gVisor needs a delegated backend. The API has `SuspendSandbox` and `ResumeSandbox`, but no bundled driver implements them yet.

## Relationship to the Agent Substrate proposal

https://github.com/nirmata/ottoflow/pull/271 proposed running agent steps as Agent Substrate actors behind an agent-executor gateway. This proposal keeps that gateway (Option C there), the standalone executor, and the exec contract, and changes the backend the gateway calls.

The new direction, and why:

- **Credentials leave the sandbox from the first release.** In the Substrate plan, Phase 2 sends LLM keys and MCP credentials into the actor, and Phase 3 moves only Secret-held static headers to egress injection. Per-run tokens and OAuth2 client credentials still reach the actor (https://github.com/nirmata/ottoflow/issues/273). OpenShell injects credentials at its proxy today, and its gateway-side OAuth2 minting and per-sandbox providers may close most of https://github.com/nirmata/ottoflow/issues/273.
- **One integration, several backends.** OpenShell's drivers cover plain Kubernetes with `agent-sandbox` (the Layer 2 plan), microVMs, and, once https://github.com/NVIDIA/OpenShell/issues/3362 lands, Substrate. The operator picks the driver in OpenShell's configuration, so OttoFlow doesn't need a second backend integration to run on Substrate.
- **Less OttoFlow code.** OpenShell provides the egress allowlist, L7 method checks, binary allowlist, and credential resolution that the Substrate plan builds from `CreateActorEgressPolicy` and `replaceHeaders` rules.
- **The spike passed.** A Go client drove OpenShell end to end on kind (see [Spike results](#spike-results)).

What OttoFlow gives up, for now:

- **Warm start and density.** Sandbox creation takes about 5 seconds on the Kubernetes driver, compared with Substrate's warm pool. Suspend isn't available until a driver implements it.
- **API maturity.** OpenShell is at v0.1.x with weekly releases; Substrate declares a 1.0 API.

If the Substrate driver doesn't land, or OpenShell's API doesn't settle, the Substrate design in https://github.com/nirmata/ottoflow/pull/271 remains a direct-integration fallback behind the same gateway.

## Spike results

The spike ran OpenShell v0.1.8 with the Kubernetes driver on a kind cluster, driven by a Go program that uses `sdk-go` v0.1.8 over mTLS.

**A Go service can drive sandboxes.** Create, wait for ready, exec, and delete work through the SDK. An HTTP server inside the sandbox answered requests through `ForwardTcp`, which is the request and response pattern the gateway needs. The first request on a forwarded connection took 7–11 ms, and later ones 5–7 ms. Each exec took about 15 ms.

**Credentials stay outside the sandbox.**

- The sandbox environment held only placeholders. The proxy injected the real bearer token over HTTP to an in-cluster server and over HTTPS to `httpbin.org`.
- The sandbox received `SSL_CERT_FILE` and similar variables for the proxy's CA, and no proxy variables, so interception is transparent to clients.
- The proxy rejected a credential sent to a host its profile doesn't bind, and blocked a host that no policy allows.
- A raw socket to an allowed host's name was allowed, and to its IP address was refused (`EPERM`).
- The `read-only` preset blocked POST, so MCP endpoints need `read-write` or `full`.

**Latency.** Seven create and delete cycles, with the image already on the node:

| Measure | Result |
|---|---|
| Create to ready | 5.1–8.8 s; 5.1–5.7 s after the first runs |
| Delete | 2.5–3.6 s |

**Gaps.**

- The Kubernetes driver returns `compute driver does not support sandbox suspend`.
- The typed SDK can't set an endpoint's access preset or enforcement mode on a profile, so profile import used the raw gRPC types that `sdk-go` also exports.
- Sandbox names are limited to 19 characters.
- One supervisor pod went to `Error` during deletion before the driver removed it.

**Not tested.** Gateway-side OAuth2 minting, per-sandbox providers created and deleted per call, the Go agent-executor binary under the binary allowlist, `uvx` and `npx` stdio servers, sandbox-policy hosts that carry no credential, concurrent sandboxes, cold image pulls, and the Substrate driver.

## Proposed design

### API

Use the `executionMode` value that `docs/dev/DESIGN.md` FR1B.7 already reserves:

```yaml
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Agent
spec:
  modelProvider: anthropic
  executionMode: sandbox        # service (default) | sandbox
```

- `executionMode` defaults to `service`, so existing Agents keep today's behavior.
- The Agent names no image, driver, or OpenShell workspace. Every sandbox runs the operator's executor image, so an Agent author can't swap the image and skip the checks the executor runs.
- The operator maps each Kubernetes namespace to one OpenShell workspace in the gateway's configuration (a chart value). The gateway runs an Agent's sandboxes only in the workspace mapped to the Agent's namespace, and fails the call if there is none.
- A validating webhook rejects `sandbox` when the feature is off.

### Sandbox image: the agent-executor in standalone mode

As in https://github.com/nirmata/ottoflow/pull/271, the sandbox runs the agent-executor binary with a standalone flag. In that mode it has no Kubernetes client, no ServiceAccount token, and no TLS key, listens on plain HTTP on `127.0.0.1`, and accepts a resolved request: today's `ExecRequest` plus the Agent spec and a resolved MCP client configuration for each MCPServer. It runs the same `RoutingAgentExecutor` as `service` mode and returns today's `ExecResponse`. Credentials in the resolved request are OpenShell placeholders, not values.

### Gateway dispatch

For a `sandbox` Agent, the agent-executor's exec handler:

1. Authenticates and authorizes the caller, and reads the Agent and its MCPServers, as today.
2. Looks up the workspace mapped to the Agent's namespace, and fails the call if there is none.
3. Builds the sandbox's profiles and providers:
   - A profile per credentialed endpoint: the LLM provider endpoint and each remote MCPServer with static header auth (`bearer`, `apiKey`, or `basic`), with the auth style, an access preset that allows MCP POST requests, and the executor binary.
   - A provider per profile, holding the value the gateway resolves from the Secret, or from the `X-LLM-Env` header for per-run tokens such as `NIRMATA_LLM_TOKEN`.
   - For an OAuth2 MCPServer, a provider that the OpenShell gateway refreshes with the client-credentials strategy, so the client secret never reaches the sandbox.

   The gateway names these after the sandbox, so they live and die with it.
4. Calls `CreateSandbox` in the workspace with the operator's executor image, the providers above, and a sandbox policy that allows the hosts with no credential: remote MCPServers with no `spec.auth` or `none` auth, package registries that `uvx` and `npx` stdio servers reach, and the extra hosts the operator configures for the namespace. It waits for the sandbox to be ready. Sandbox names use a fixed `of-` prefix and fit OpenShell's 19-character limit.
5. Starts the standalone executor with `ExecSandbox`, and POSTs the resolved request to it through `ForwardTcp`.
6. Deletes the sandbox and its providers and profiles when the call finishes, fails, or is cancelled.
7. Returns the executor's `ExecResponse` to the runner unchanged.

**Timeouts and retries.** The gateway is bounded by the runner's request context, which times out after 30 minutes (`internal/workflow/executor/exec_client.go`). The gateway may retry the setup calls in steps 3 and 4, but once it has sent the request to the sandbox it never re-sends it or creates a second sandbox for the same call, since the exec endpoint isn't idempotent.

**Orphaned sandboxes.** If the gateway restarts during a step, its cleanup never runs. On startup and then periodically, the gateway lists sandboxes and providers with the `of-` prefix in its mapped workspaces and deletes those older than the runner's 30-minute exec timeout.

**Observability.** The gateway reports each failure as an `ExecErrorResponse` whose message names the stage and the sandbox. The runner adds headers with the WorkflowRun namespace and name and the step name, and the gateway logs them with the sandbox name.

### Credentials

| Credential | Where it lives |
|---|---|
| LLM keys from Secrets | OpenShell provider; placeholder in the sandbox |
| Per-run tokens (`X-LLM-Env`) | Per-sandbox provider; placeholder in the sandbox |
| Remote MCP `bearer`, `apiKey`, `basic` | OpenShell provider; placeholder in the sandbox |
| Remote MCP `oauth2` | OpenShell gateway mints the token; nothing in the sandbox |
| stdio MCP Secret env values | Sent in the resolved request; in the sandbox for one step |
| Kubernetes credentials | None; MCP tools that need cluster access don't fit `sandbox` mode yet |

The per-run and OAuth2 rows depend on the follow-up spike (Phase 0). If either fails, that credential reaches the sandbox for one step, as in the Substrate plan, and https://github.com/nirmata/ottoflow/issues/273 stays open for it.

### Network and policy

- The gateway's egress allowlist gains the OpenShell gateway's gRPC endpoint.
- Sandbox egress is enforced by OpenShell's proxy and the NetworkPolicy the Kubernetes driver manages. A sandbox reaches only its Agent's LLM endpoint, its MCP servers, and the configured extra hosts.
- Agents, MCPServers, and Workflows are still CRDs, so admission policies on them apply as in `service` mode. The `mcpTools` allowlist and the executor's other checks run inside the sandbox unchanged.

### Substrate later

When the Substrate driver lands, the operator configures it in OpenShell. OttoFlow doesn't change, because the gateway calls the same OpenShell API. Suspend becomes possible when a driver implements `SuspendSandbox`.

## Phases

0. **Follow-up spike.** Test gateway-side OAuth2 minting, per-sandbox providers, the Go executor and `uvx` and `npx` stdio servers under the binary allowlist, sandbox-policy hosts, concurrent sandboxes, and cold image pulls. Pick the OpenShell release to target.
1. **Step-scoped sandboxes.** The API field, webhook, standalone executor, gateway dispatch with credentials in providers, reaper, and chart values. Each step pays about 5 seconds of startup.
2. **Run-scoped sandboxes.** A `sandboxScope: run` field reuses one sandbox across a run's agent steps, carries the WorkflowRun UID in the exec request, and deletes the sandbox when the run ends. The reaper deletes run-scoped sandboxes whose WorkflowRun has finished or no longer exists.
3. **Substrate and suspend.** Validate the Substrate driver once it exists, and suspend run-scoped sandboxes while a run waits, if the driver supports it.

## Files to change

| File | Change |
|---|---|
| `api/v1alpha1/agent_types.go` | Add `executionMode`; run `make manifests generate codegen-docs` |
| `internal/webhook/agent_mcpserver_validators.go` | Reject `sandbox` when the feature is off |
| `cmd/agent-executor/main.go` | Standalone mode flag; OpenShell gateway configuration |
| `internal/workflow/executor/exec_handler.go` | Branch on `executionMode`; map the namespace to a workspace; build profiles and providers; create, forward to, and delete the sandbox; reap orphans |
| `internal/workflow/executor/exec_client.go` | Send the WorkflowRun and step headers |
| `internal/agent/` | OpenShell client wrapper over `sdk-go` |
| `charts/ottoflow/` | OpenShell endpoint and client certificate, the namespace-to-workspace mapping, extra hosts per namespace, and the executor image digest; all off by default |
| `docs/dev/DESIGN.md` | Mark `sandbox` and the `executionMode` field as implemented, with OpenShell as the backend |
| `samples/workflows/` | One `sandbox` Agent sample |

## Testing and verification

- Unit tests: webhook validation; gateway dispatch with a fake OpenShell client, including cleanup on success, failure, and cancellation, no re-send after the request reaches the sandbox, refusal of an unmapped namespace, the profiles and providers built from the Agent and its MCPServers, and the reaper; the standalone executor with resolved specs and no Kubernetes client.
- e2e on kind with OpenShell's Kubernetes driver: an `agentRef` step on a `sandbox` Agent returns the same outputs and token usage as in `service` mode, only placeholders appear in the sandbox, and no sandbox or provider remains after the run.
- Measure step latency against `service` mode, including `uvx` and `npx` stdio servers.

## Rollout and backward compatibility

- `executionMode` defaults to `service`; Agents without it behave as today.
- `sandbox` mode requires an installed OpenShell gateway and, for the Kubernetes driver, the `agent-sandbox` controller and a CNI that enforces NetworkPolicy. The chart doesn't install them.
- The feature is off in the chart by default; with it off, the webhook rejects `executionMode: sandbox`.
- OttoFlow pins one OpenShell release and `sdk-go` version at a time. Moving to a new release is an explicit upgrade with its own e2e run, because the API changes between v0.1.x releases.

## Open questions

1. **Long-lived or per-sandbox providers.** This proposal creates providers per sandbox, which keeps Secret changes simple but adds API calls to every dispatch. Should a controller instead reconcile long-lived providers for Secret-held credentials?
2. **Typed SDK gaps.** Should OttoFlow contribute access presets and enforcement mode to `sdk-go`'s typed profile import, or use the raw gRPC types?
3. **Default driver.** Should the docs recommend the Kubernetes driver with gVisor or the MicroVM driver for production?

## Future work

- Run direct `MCPToolCall` stdio servers in sandboxes, closing the runner-side surface in [PROPOSAL_AGENT_SANDBOXING.md](PROPOSAL_AGENT_SANDBOXING.md).
- Expose run-scoped sandboxes to `serve-a2a`, so long A2A tasks survive waits.
