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
| `modelProvider` | string | Yes | LLM provider. One of: `nirmata`, `openai`, `anthropic`, `azure-openai`, `google`, `gemini`, `local`. `nirmata` requires the OttoFlow **enterprise plugin** and is unavailable in the open-source build — set an explicit `openai`, `anthropic`, `google`/`gemini`, or `local` provider there. `azure-openai` is accepted but not supported (no multi-turn tool use): the Azure OpenAI client in this build does not keep the model's tool calls and replies in the conversation history (single-turn prompts may still work). There is no bedrock provider. |
| `modelName` | string | No | Model identifier (e.g. `gpt-4`, `claude-3-opus`). Default depends on provider. |
| `mcpTools` | []string | No | List of MCP tools the agent can use. Format: `"server:tool"` (e.g. `kubernetes-mcp:get-resource`). |
| `outputExtraction` | [OutputExtraction](#outputextraction) | No | How to extract outputs from agent responses (json, regex, text). |
| `thinking` | [AgentThinking](#agentthinking) | No | Thinking type and effort sent with each LLM request. `modelProvider: anthropic` only: on any other provider the agent run fails (admission warns). |
| `config` | map[string]string | No | Provider client options. Only `endpoint` and `skipVerifySSL` are read. `endpoint` is accepted **only** if the agent-executor operator has allowlisted its origin via `AGENT_LLM_ENDPOINT_ALLOWLIST` (Helm: `agentExecutor.llmEndpointAllowlist`), and only as an origin (`scheme://host[:port]`; a path, query, fragment or userinfo is rejected). With the allowlist empty (the default), any non-empty `endpoint` fails the agent run, for every provider, because the agent-executor's LLM API keys are shared by every Agent. Once accepted, `endpoint` is effective for `azure-openai` (where it must be `https://`); `anthropic` also honors it unless `ANTHROPIC_BASE_URL` is set; `openai` uses `OPENAI_ENDPOINT`/`OPENAI_API_BASE` from the agent-executor environment, and `local` uses `LLAMACPP_HOST`. `google`/`gemini` reject any `endpoint`, allowlisted or not, because their client ignores it; set `GOOGLE_GEMINI_BASE_URL` on the agent-executor instead. `skipVerifySSL: "true"` is **rejected** for every provider and fails the agent run; to reach an endpoint with a private CA, set `SSL_CERT_FILE` or `SSL_CERT_DIR` on the agent-executor process (as a last resort, `LLM_SKIP_VERIFY_SSL=true` there, a process-wide setting not scoped to one Agent). List only origins you control that do not redirect to another origin. API keys are never read from `config` — they come from the agent-executor: the key file mounted from `agentExecutor.llmCredentialsSecret` when present, otherwise its process environment. See [Upgrading: Agent LLM endpoint allowlist](../../upgrades/llm-endpoint-allowlist.md). |
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

### AgentThinking

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `type` | string | No | Sent as the request's `thinking.type`. One of: `adaptive`, `disabled`, `between_tools`. |
| `effort` | string | No | Sent as the request's `output_config.effort`. One of: `low`, `medium`, `high`, `xhigh`, `max`. |

A model that thinks by default can spend its whole `max_tokens` budget on thinking and return no text, which fails the step. Set `type: disabled`, or `between_tools` on a model that rejects `disabled`, to turn up-front thinking off; on a model that accepts neither, lower `effort`. An unset field is left off the request, so the model's default applies. The Anthropic API decides which values a model accepts, and a rejected value fails the agent run with the API's error.

```yaml
spec:
  modelProvider: anthropic
  thinking:
    type: disabled
    effort: high
```

Apply the release's CRDs before upgrading ([CRDs before every upgrade](../../upgrades/crds-before-every-upgrade.md)): an Agent CRD from before this field drops `thinking` without any error.

`max_tokens` is not set per Agent: `ANTHROPIC_MAX_TOKENS` on the agent-executor process (or your shell in CLI local mode) sets it for every Agent, default 16384, and thinking tokens count against it.

### Local provider (`modelProvider: local`)

`local` targets any llama.cpp-compatible server (llama.cpp, Ollama, vLLM, LM Studio). The server address is read from the **`LLAMACPP_HOST`** environment variable in the process executing the agent step (the agent-executor pod in-cluster, or your shell for CLI local mode). `spec.config.endpoint` is ignored for this provider.

---

## Status (AgentStatus)

| Field | Type | Description |
|-------|------|-------------|
| `phase` | string | One of: `Ready`, `NotReady`. |
| `message` | string | Additional information about the agent status. |
| `lastChecked` | string (date-time) | When the agent configuration was last validated. |
