# MCP Servers and MCP Tool Calls

This guide explains how to configure **MCP (Model Context Protocol) servers** in OttoFlow and how to call MCP tools from workflow steps.

## Overview

- **MCPServer** – A Custom Resource that defines how to connect to an MCP server (transport, address, auth, timeout). You create one MCPServer per MCP backend.
- **MCP tool call step** – A workflow step that invokes a single MCP tool by name, with arguments resolved from the workflow context. No LLM is involved; the step calls the tool directly and exposes the result as `toolResult`.

This guide is about OttoFlow as an MCP *client*. For the other direction — serving this
cluster's Workflows as tools an agent framework can call — see
[Serving Workflows as MCP Tools](workflows-as-mcp-tools.md).

MCP servers can be used in two ways in OttoFlow:

1. **Direct tool calls** – Use the `mcpToolCall` step type to call a specific tool with CEL-resolved arguments. The result is available as `toolResult` in that step’s outputs.
2. **Agent steps** – Agents (see the [Agent API reference](../reference/api/agent.md)) can be configured with `spec.mcpTools` (e.g. `"server-name:tool-name"`). Those tools are registered with the LLM for the duration of the agent step so the LLM can invoke them during execution. This works for both in-cluster runs (via the agent-executor service) and local CLI runs (`--workflow-dir`). This doc focuses on **direct** MCP tool calls and MCPServer configuration.

## Prerequisites

- OttoFlow CRDs installed (including `mcpservers.ottoflow.nirmata.io`)
- An MCP server you can connect to (stdio command, or HTTP/SSE endpoint)

---

## Configuring MCP Servers

Create an **MCPServer** resource in the same namespace as your WorkflowRuns. Workflow steps reference the server by **name** (e.g. `server: kubernetes-mcp`).

### Transport types

The `spec.transport` field defines how OttoFlow connects to the MCP server.

| Type     | Use case              | Required fields       | Optional fields   |
|----------|------------------------|------------------------|-------------------|
| `stdio`  | Local process (e.g. npx) | `command` (array)   | —                 |
| `http`   | HTTP API               | `address` (URL)        | `headers`         |
| `sse`    | Server-Sent Events     | `address` (URL)        | `headers`         |

**Example: stdio (run a process)**

```yaml
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: MCPServer
metadata:
  name: kubernetes-mcp
  namespace: default
spec:
  transport:
    type: stdio
    command:
      - "npx"
      - "-y"
      - "@modelcontextprotocol/server-kubernetes"
  timeout: "30s"
  auth:
    type: none
  env: []
```

**Example: HTTP with optional headers**

```yaml
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: MCPServer
metadata:
  name: http-mcp-server
  namespace: default
spec:
  transport:
    type: http
    address: "https://api.example.com/mcp"
    headers:
      Content-Type: "application/json"
  timeout: "60s"
  auth:
    type: bearer
    secretRef:
      name: mcp-auth-secret
      namespace: default
      key: token
  env:
    - name: API_ENV
      value: "production"
```

### Authentication

Use `spec.auth` to configure how the client authenticates to the MCP server. **Credentials must be stored in Kubernetes Secrets** and referenced via `secretRef` (or OAuth2 secret refs). Inline credentials are not supported.

| Type      | Description                    | Required / Typical fields                                                                 |
|-----------|--------------------------------|-------------------------------------------------------------------------------------------|
| `none`    | No authentication              | —                                                                                         |
| `bearer`  | Bearer token                   | `secretRef` (Secret key holding the token)                                               |
| `apiKey`  | API key                        | `secretRef` (Secret key holding the API key)                                              |
| `basic`   | Basic auth                     | `secretRef` (Secret must contain keys `username` and `password`)                          |
| `oauth2`  | OAuth 2.0 client credentials   | `oauth2.tokenURL`, and either `oauth2.clientCredentialsRef` or `clientId` + `clientSecretRef`; optional `scopes` |

**Bearer token from a Secret**

```yaml
spec:
  auth:
    type: bearer
    secretRef:
      name: mcp-auth-secret
      namespace: default
      key: token
```

**OAuth2 client credentials (machine-to-machine)**

```yaml
spec:
  auth:
    type: oauth2
    oauth2:
      tokenURL: "https://auth.example.com/oauth/token"
      clientCredentialsRef:
        name: mcp-oauth2-credentials
        namespace: default
      scopes:
        - "mcp:read"
        - "mcp:tools"
```

Store credentials in a Secret in the same namespace (e.g. `client_id` and `client_secret` for OAuth2, or a single `token` key for bearer). See [mcp-with-auth.yaml](../../../samples/workflows/features/mcp-with-auth.yaml) and [mcp-oauth2-auth.yaml](../../../samples/workflows/features/mcp-oauth2-auth.yaml) for full examples.

### Other fields

- **timeout** – Connection/call timeout (e.g. `"30s"`, `"5m"`). Optional. Keep it above 5s: a server that ignores the protocol probe takes 5s to fall back. See [Upgrading: MCP protocol negotiation and the `/mcp` Host check](../upgrades/mcp-protocol-negotiation.md).
- **env** – List of `name`/`value` environment variables for the MCP server process (mainly relevant for stdio). Optional.

---

## Using MCP tool calls in steps

In a Workflow, add a step with **mcpToolCall**:

- **server** – Name of the MCPServer resource (must exist in the WorkflowRun’s namespace).
- **tool** – Name of the tool as exposed by the MCP server.
- **arguments** – Map of argument names to **CEL expressions**. Expressions are evaluated in the workflow context (`inputs.*`, `variables.*`, `expressions.*`), and the resulting values are sent to the tool.

After the tool runs, the step has access to **toolResult** in its outputs. Use it in `outputs[].expression` like any other variable.

### Basic example

```yaml
steps:
  - name: getResourceCount
    message: "Get resource count using MCP tool"
    mcpToolCall:
      server: kubernetes-mcp
      tool: get-resource
      arguments:
        resourceType: 'inputs.resourceType'
        namespace: 'inputs.namespace'
    outputs:
      - name: resourceCount
        expression: 'toolResult.count'
      - name: resources
        expression: 'toolResult.items'
```

- Arguments can reference workflow inputs (e.g. `inputs.resourceType`), outputs from previous steps (`variables.someOutput`), or literals (e.g. `'"default"'` for a string).
- The tool’s return value is in `toolResult`; you can expose fields like `toolResult.count` or the whole `toolResult` in step outputs.

### Chaining with other steps

Use **dependsOn** when a later step needs the MCP tool result:

```yaml
steps:
  - name: getResourceCount
    message: "Get resource count using MCP tool"
    mcpToolCall:
      server: kubernetes-mcp
      tool: get-resource
      arguments:
        resourceType: 'inputs.resourceType'
        namespace: 'inputs.namespace'
    outputs:
      - name: resourceCount
        expression: 'toolResult.count'

  - name: processResults
    message: "Process MCP tool results"
    dependsOn:
      - getResourceCount
    expressions:
      - name: summary
        expression: 'variables.resourceCount'
    outputs:
      - name: totalResources
        expression: 'expressions.summary'
```

### Dynamic tool name from inputs

You can set the tool name from an input (or any CEL expression):

```yaml
mcpToolCall:
  server: authenticated-mcp-server
  tool: 'inputs["tool-name"]'
  arguments:
    resourceType: '"Pod"'
    namespace: '"default"'
```

### Namespace behavior

The MCPServer is looked up in the **WorkflowRun’s namespace**. Create the MCPServer in that namespace (e.g. `default`) so the workflow can resolve `server: <name>`.

---

## Full workflow example

1. Create the MCPServer (once per namespace):

```bash
kubectl apply -f mcpserver-config.yaml
```

2. Create a Workflow that uses an MCP tool:

```yaml
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: mcp-tool-call-workflow
  namespace: default
spec:
  inputs:
    - name: resourceType
      default: "Pod"
    - name: namespace
      default: "default"
  steps:
    - name: getResourceCount
      message: "Get resource count using MCP tool"
      mcpToolCall:
        server: kubernetes-mcp
        tool: get-resource
        arguments:
          resourceType: 'inputs.resourceType'
          namespace: 'inputs.namespace'
      outputs:
        - name: resourceCount
          expression: 'toolResult.count'
        - name: resources
          expression: 'toolResult.items'
  outputs:
    - name: result
      expression: 'variables.resourceCount'
```

3. Run it with a WorkflowRun:

```yaml
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: WorkflowRun
metadata:
  name: mcp-tool-call-workflow-run-001
  namespace: default
spec:
  workflowRef:
    name: mcp-tool-call-workflow
  inputValues:
    resourceType: "Pod"
    namespace: "default"
```

```bash
kubectl apply -f workflow.yaml
kubectl get workflowrun mcp-tool-call-workflow-run-001
kubectl get workflowrun mcp-tool-call-workflow-run-001 -o jsonpath='{.status.outputs}' | jq '.'
```

---

## Reference

| Concept | Description |
|--------|-------------|
| **MCPServer** | Namespaced CR that defines transport, auth, timeout, and env for one MCP server. |
| **mcpToolCall** | Step type: `server` (MCPServer name), `tool` (tool name), `arguments` (map of CEL expressions). |
| **toolResult** | Variable available in that step’s outputs; holds the tool’s return value. |
| **Namespace** | MCPServer must be in the same namespace as the WorkflowRun. |

### Sample workflows

- [mcpserver-config.yaml](../../../samples/workflows/features/mcpserver-config.yaml) – MCPServer examples (stdio and HTTP with auth)
- [mcp-tool-call.yaml](../../../samples/workflows/features/mcp-tool-call.yaml) – Direct MCP tool call and output extraction
- [mcp-with-auth.yaml](../../../samples/workflows/features/mcp-with-auth.yaml) – Bearer token authentication
- [mcp-oauth2-auth.yaml](../../../samples/workflows/features/mcp-oauth2-auth.yaml) – OAuth2 client credentials
- [agent-mcp-combined.yaml](../../../samples/workflows/features/agent-mcp-combined.yaml) – Agent steps and MCP tool calls in the same workflow
- [pod-diagnostics-with-mcp.yaml](../../../samples/workflows/features/pod-diagnostics-with-mcp.yaml) – CEL, MCP tools, and agent together

See the [samples README](../../../samples/workflows/README.md) for the full list and descriptions.
