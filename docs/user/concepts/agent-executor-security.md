# Agent Executor Security Model

This document explains how the agent executor authenticates and authorizes callers, and how to configure and harden it in production.

## Overview

The **agent executor** is the service that runs agent steps for workflows. Workflow runner Jobs call it over HTTPS via `POST /api/exec/{namespace}/{agentName}` to execute agent tools. Only trusted identities—the OttoFlow controller and runner ServiceAccounts—should be able to call this service.

Authorization is **permission-based**: the executor allows any ServiceAccount the cluster authorizes for a specific Kubernetes permission, checked via **SubjectAccessReview** (SAR). There is no allowlist of ServiceAccount names; with RBAC, the permission comes from a ClusterRole or Role, bound with a ClusterRoleBinding or a RoleBinding in the checked namespace.

## How Authentication Works

Every request to the agent executor (except health checks) must include a **Bearer token** (the caller’s ServiceAccount token). The executor then:

1. **Validates the token** using the Kubernetes **TokenReview** API. This proves the token is valid and returns the identity (e.g. `system:serviceaccount:namespace:name`).
2. **Checks authorization** using **SubjectAccessReview**: “Can this identity `invoke` `callers.gate.ottoflow.nirmata.io/agent-executor-caller` in namespace N?” (N is configurable, typically the release namespace.) With RBAC alone, only identities that have been granted this permission through a ClusterRole or Role bound with a ClusterRoleBinding or a RoleBinding (the check is namespaced, so either passes) are allowed. The SubjectAccessReview asks the API server's whole authorizer chain, not only RBAC: on a cluster with an authorization webhook ahead of RBAC, a webhook that allows `invoke` on `callers.gate.ottoflow.nirmata.io` lets the caller in with no Role or binding, and one that denies it refuses the caller even when a binding exists.

So: **authentication** is “who are you?” (TokenReview), and **authorization** is “are you allowed to call the agent executor?” (SAR).

## How It’s Set Up

1. **ClusterRole**  
   A ClusterRole grants the narrow permission: `invoke` on `callers` in API group `gate.ottoflow.nirmata.io` with `resourceNames: ["agent-executor-caller"]`. The resource is virtual and nothing serves it; the permission is used only for the SAR check, so ConfigMap read access never passes it. Upgrading: a hand-written Role on `configmaps/agent-executor-caller` no longer works; re-grant it with verb `invoke` on `callers.gate.ottoflow.nirmata.io`. The legacy `get configmaps/agent-executor-caller` rule is kept in the chart role for this release for older images; new images ignore it.

2. **ClusterRoleBindings**  
   - The **controller** ServiceAccount is bound to this ClusterRole (so, with RBAC, the controller can call the agent executor).
   - For each **runner** ServiceAccount (in every namespace where WorkflowRuns run), the OttoFlow controller automatically creates a ClusterRoleBinding to the same role. So, with RBAC, any namespace that can run workflows can also call the agent executor.

3. **Executor behavior**  
   For each request, the executor runs a SubjectAccessReview against the configured namespace. If the SubjectAccessReview allows it, the request proceeds.

## Configuration

| What you control | How |
|------------------|-----|
| Who can call     | With RBAC, ClusterRoleBindings, or RoleBindings in the checked namespace, to the agent-executor-caller ClusterRole (controller creates these for runner SAs when `AGENT_EXECUTOR_CALLER_CLUSTER_ROLE` is set). |
| Namespace for SAR | `agentExecutor.callerNamespace` (Helm; default: release namespace) or `-agent-executor-caller-namespace` (flag). |

With default Helm values, the controller receives `AGENT_EXECUTOR_CALLER_CLUSTER_ROLE` and creates the runner→caller bindings automatically.

## Best Practices

### 1. Restrict Runner Permissions

Runner ServiceAccounts need enough permissions to run workflows (resource queries, mutations, MCP, etc.). Follow least privilege: give the runner ClusterRole only the APIs and verbs your workflows need. The agent-executor-caller role is separate and grants the narrow “invoke callers.gate.ottoflow.nirmata.io/agent-executor-caller” permission used for the SAR check (the legacy `get configmaps/agent-executor-caller` rule is kept for this release for older images; new images ignore it).

### 2. Use TLS and Restrict Network Access

The agent executor serves HTTPS only. In production:

- Use TLS (the chart can provision certs or use your own secret).
- Use NetworkPolicies to restrict which namespaces or pods can reach the agent executor service. Only the controller and runner Jobs need access.

### 3. Run in a Dedicated Namespace

Install OttoFlow (including the agent executor) in a dedicated namespace (e.g. `ottoflow`). Avoid running it in `default` or alongside untrusted workloads. This simplifies RBAC and network policies.

### 4. Align Caller Namespace with Installation

The executor’s “caller namespace” (used in the SAR check) should match where the agent-executor-caller ClusterRole is intended to be used—typically the release namespace. The chart sets this from the release namespace by default; only override `agentExecutor.callerNamespace` if your installation namespace differs.

### 5. Audit Who Has the Caller Role

Periodically list ClusterRoleBindings and RoleBindings (in the checked namespace) that reference the agent-executor-caller ClusterRole, and any Role or ClusterRole granting `invoke` on `callers.gate.ottoflow.nirmata.io`. Only the controller and runner SAs should be bound; ServiceAccounts with wildcard permissions (such as `cluster-admin`) also pass under RBAC. Remove any bindings that are no longer needed (e.g. after decommissioning a namespace). On a cluster with an authorization webhook ahead of RBAC, also review what that webhook allows on `callers.gate.ottoflow.nirmata.io`, by sending a SubjectAccessReview with `kubectl create -f` (`kubectl auth can-i` cannot test a resource nothing serves).

### 6. Multi-Tenant or Multi-Namespace Setups

When WorkflowRuns run in multiple namespaces (e.g. per-tenant namespaces), ensure the controller is deployed with `AGENT_EXECUTOR_CALLER_CLUSTER_ROLE` set (the chart does this when the agent executor is enabled). The controller will create the agent-executor-caller binding for each runner SA it creates. No per-namespace configuration is needed in the executor.

### 7. Explicit Runner ServiceAccounts

If a WorkflowRun specifies `spec.execution.job.serviceAccountName`, the controller does not manage that SA’s bindings (it assumes you manage RBAC for that SA). With RBAC, to let that SA call the agent executor, create a ClusterRoleBinding yourself that binds that ServiceAccount to the agent-executor-caller ClusterRole.

### 8. Secure Agent and MCP Configuration

Agent executor security controls **who can call the service**. The **Agent** and **MCPServer** resources and step definitions control what agents and tools can do. Restrict MCP server credentials, LLM API keys, and agent prompts to what each workflow needs. Prefer narrow, step-specific agents and tools over broad access.

### 9. Monitor and Log

Enable logging and metrics for the agent executor. Monitor for 401s (failed auth) and unexpected callers. Use Kubernetes audit logging if you need to trace who was granted or denied access via SAR.

---

## See Also

- [Agent API reference](../reference/api/agent.md) for agent and executor service configuration.
- [Installation](../tasks/installation.md) and Helm values for deploying the controller and agent executor.
