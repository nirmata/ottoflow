# Serving Workflows as kagent Agents (A2A)

These samples show the opposite direction from
[`samples/kagent-workflows-as-tools/`](../kagent-workflows-as-tools/): instead of
OttoFlow calling out to kagent over MCP, a Workflow is exposed *to* kagent as its own
[A2A](https://a2aprotocol.ai/) agent, callable from kagent's UI or any other A2A client.

See [Serving Workflows as MCP Tools](../../docs/user/tasks/workflows-as-mcp-tools.md)
for the other direction, or
[Serving Workflows as A2A Agents](../../docs/user/tasks/workflows-as-a2a-agents.md)
for this feature's full task doc.

## Opting a workflow in

Exposure is per workflow, via `spec.expose.kagent`:

```yaml
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: a2a-proof-greeting
spec:
  expose:
    kagent:
      displayName: "Greeting"
      description: 'A2A plumbing proof (no LLM).  Try: "kagent"'
      examples:
        - "kagent"
      tags:
        - demo
```

`displayName`, `description`, `examples`, and `tags` populate the agent card kagent
shows a user or model choosing between agents — the same idea as
`spec.mcpTool.description` on the MCP-tools side, adapted to A2A's card format.

When a Workflow with `spec.expose.kagent` set is reconciled, OttoFlow creates a kagent
BYO (bring-your-own) `Agent` in the Workflow's namespace, pointing at a small per-workflow
`serve-a2a` server that turns each A2A call into a `WorkflowRun` and streams its progress
and output back as A2A events.

## Enabling it

This is off by default, and needs two things to actually run:

```bash
helm upgrade --install ottoflow ./charts/ottoflow \
  --namespace ottoflow --create-namespace \
  --set serveA2A.enabled=true
```

1. **`serveA2A.enabled=true`** — without it, the controller does not create kagent
   Agents for any Workflow, even one with `spec.expose.kagent` set.
2. **A `serve-a2a` image.** It publishes starting with the next OttoFlow release tag.
   Only set `serveA2A.enabled: true` once that image exists for the version you're
   deploying, or point `serveA2A.image.fullOverride` at your own build in the meantime.

## Samples

| File | What it shows |
|---|---|
| `greeting.yaml` | Minimal no-LLM workflow exposed via A2A — proves the plumbing end to end without an Agent or LLM credentials. |
| `cluster-security-scan.yaml` | Collects Pod security data in-cluster, then delegates analysis to an external agent over A2A (`externalAgentRef`), and is itself exposed back to kagent. |
| `incident-triage.yaml` | Delegates triage to an external agent over A2A, then applies a CEL policy gate over the verdict before returning it. |
| `manifest-review.yaml` | Delegates manifest review to an external agent over A2A. Not itself exposed via `spec.expose.kagent` — a plain internal step. |
| `scale-request-review.yaml` | Multi-input extraction workflow exposed via A2A; does its own LLM step rather than delegating. |

`cluster-security-scan.yaml`, `incident-triage.yaml`, and `manifest-review.yaml` call
an external agent at `http://kagent-controller.kagent.svc:8083/api/a2a/kagent/k8s-agent`
— kagent's own agent-to-agent endpoint — so they assume a kagent installation with a
`k8s-agent` already running. Adjust the URL if your external agent lives elsewhere.

## Apply

```bash
kubectl apply -f samples/kagent-workflows-as-agents/
```
