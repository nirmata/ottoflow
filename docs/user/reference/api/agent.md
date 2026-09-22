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
| `config` | map[string]string | No | Provider client options. `endpoint` is honored **only** if the agent-executor operator has pre-approved its origin via `AGENT_LLM_ENDPOINT_ALLOWLIST` (Helm: `agentExecutor.llmEndpointAllowlist`) — an unset/empty allowlist rejects any non-empty `endpoint` and fails the agent run, because the agent-executor's LLM API key is process-global and honoring an unapproved CR-supplied endpoint would expose that shared credential to a tenant-chosen destination. Once an origin **is** allowlisted, it is currently effective only for `azure-openai`; `openai` uses `OPENAI_ENDPOINT`/`OPENAI_API_BASE` from the agent-executor environment, and `local` uses `LLAMACPP_HOST`. An allowlisted origin is a provider-agnostic trust decision: any `modelProvider` may be pointed at it, so that host can receive whichever LLM credential is present in the agent-executor pod, together with the full prompt — do not list a host you do not control. `skipVerifySSL` is **rejected** for every provider: setting it to `"true"` fails the agent run, because the agent-executor's LLM credential is process-global and a tenant-authored Agent may not turn off TLS verification for requests carrying it. To reach an endpoint with a private CA, mount the CA bundle and set `SSL_CERT_FILE` or `SSL_CERT_DIR` on the agent-executor process (keeps TLS verification on); as a last resort, `LLM_SKIP_VERIFY_SSL=true` on the agent-executor process disables verification for all LLM egress from that pod, not just this Agent. API keys are never read from `config` — they come from the agent-executor process environment. |
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

`local` targets any llama.cpp-compatible server (llama.cpp, Ollama, vLLM, LM Studio). The server address is read from the **`LLAMACPP_HOST`** environment variable in the process executing the agent step (the agent-executor pod in-cluster, or your shell for CLI local mode). `spec.config.endpoint` is ignored for this provider — see the `config` row above.

---

## Status (AgentStatus)

| Field | Type | Description |
|-------|------|-------------|
| `phase` | string | One of: `Ready`, `NotReady`. |
| `message` | string | Additional information about the agent status. |
| `lastChecked` | string (date-time) | When the agent configuration was last validated. |
