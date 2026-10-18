# Agent

**Agent** defines a reusable AI agent configuration for workflow steps. It specifies the system prompt, LLM provider and model, optional MCP tools, output extraction, and execution settings.

- **API Group:** `ottoflow.nirmata.io`
- **Version:** `v1alpha1`
- **Kind:** `Agent`
- **Scope:** Namespaced
- **Short name:** `agent`

---

## Spec (AgentSpec)

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `prompt` | string | Yes | System prompt for the agent (role and base instructions). For dynamic content, use `additionalPrompts` in the workflow step's agentRef. |
| `modelProvider` | string | Yes | LLM provider. One of: `nirmata`, `openai`, `anthropic`, `azure-openai`, `google`, `gemini`, `local`. `nirmata` requires the OttoFlow **enterprise plugin** and is unavailable in the open-source build — set an explicit `openai`, `anthropic`, `azure-openai`, `google`/`gemini`, or `local` provider there. There is no bedrock provider. |
| `modelName` | string | No | Model identifier (e.g. `gpt-4`, `claude-3-opus`). Default depends on provider. |
| `mcpTools` | []string | No | List of MCP tools the agent can use. Format: `"server:tool"` (e.g. `kubernetes-mcp:get-resource`). |
| `outputExtraction` | [OutputExtraction](#outputextraction) | No | How to extract outputs from agent responses (json, regex, text). |
| `config` | map[string]string | No | Provider client options. Only `endpoint` and `skipVerifySSL` are read. `endpoint` is accepted **only** if the agent-executor operator has allowlisted its origin via `AGENT_LLM_ENDPOINT_ALLOWLIST` (Helm: `agentExecutor.llmEndpointAllowlist`), and only as an origin (`scheme://host[:port]`; a path, query, fragment or userinfo is rejected). With the allowlist empty (the default), any non-empty `endpoint` fails the agent run, for every provider, because the agent-executor's LLM API keys are shared by every Agent. Once accepted, `endpoint` is effective only for `azure-openai` (where it must be `https://`); `openai` uses `OPENAI_ENDPOINT`/`OPENAI_API_BASE` from the agent-executor environment, and `local` uses `LLAMACPP_HOST`. `skipVerifySSL: "true"` is **rejected** for every provider and fails the agent run; to reach an endpoint with a private CA, set `SSL_CERT_FILE` or `SSL_CERT_DIR` on the agent-executor process (as a last resort, `LLM_SKIP_VERIFY_SSL=true` there disables verification for all its LLM egress). API keys are never read from `config` — they come from the agent-executor process environment. |
| `serviceAccount` | string | No | Kubernetes service account for agent execution (RBAC). |
| `serviceName` | string | No | Name of the AgentExecutor Service. Default: `ottoflow-agent-executor`. |
| `serviceNamespace` | string | No | Namespace of the AgentExecutor Service. Default: `ottoflow`. |
| `resources` | [ResourceRequirements](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#resourcerequirements-v1-core) | No | Requests/limits for agent execution (e.g. for future sandbox mode). |
| `executorImage` | string | No | Custom container image for agent execution. Default: `ghcr.io/nirmata/ottoflow/agent-executor:latest`. |

### OutputExtraction

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `type` | string | No | One of: `json`, `regex`, `text`. Default: `json`. |
| `pattern` | string | No | For `json`: a **JSONPath** applied to the extracted JSON — a pattern that matches nothing is an **error**; if the pattern selects a non-object value it is returned under the key `result`. For `regex`: a regex with capture groups. For `text`: **unused**. |
| `schema` | string (byte) | No | Expected output schema for JSON extraction. |

Extracted values are available in the step's outputs as `agentOutputs.<key>`; the raw response text is `agentResponse`.

> **Behaviour change.** `pattern` was previously ignored for `type: json` — extraction always
> returned the whole JSON object. It is now applied. If you have an Agent that already sets
> `pattern` with `type: json`, check it before upgrading: a matching pattern now narrows the
> result (and a non-object match lands under `result`, so an `agentOutputs.<otherKey>`
> reference that used to resolve may stop resolving), and a pattern that matches nothing now
> fails the step instead of succeeding silently.

### Local provider (`modelProvider: local`)

`local` targets any llama.cpp-compatible server (llama.cpp, Ollama, vLLM, LM Studio). The server address is read from the **`LLAMACPP_HOST`** environment variable in the process executing the agent step (the agent-executor pod in-cluster, or your shell for CLI local mode). `spec.config.endpoint` is ignored for this provider.

---

## Status (AgentStatus)

| Field | Type | Description |
|-------|------|-------------|
| `phase` | string | One of: `Ready`, `NotReady`. |
| `message` | string | Additional information about the agent status. |
| `lastChecked` | string (date-time) | When the agent configuration was last validated. |
