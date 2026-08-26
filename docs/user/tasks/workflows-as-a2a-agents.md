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

This is off by default, and needs two things to actually run:

1. **`serveA2A.enabled=true`** — without it, the controller does not create kagent
   Agents for any Workflow, even one with `spec.expose.kagent` set. Enabling it also
   makes the chart create the shared serve-a2a `ClusterRole` that every per-workflow
   Agent's ServiceAccount binds to.
2. **A `serve-a2a` image.** It publishes starting with the next OttoFlow release tag.
   Only set `serveA2A.enabled: true` once that image exists for the version you're
   deploying, or point `serveA2A.image.fullOverride` (in `values.yaml`) at your own
   build in the meantime.

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
