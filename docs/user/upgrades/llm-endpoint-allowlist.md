# Upgrading: Agent LLM endpoint allowlist

**An Agent that sets `spec.config.endpoint` or `spec.config.skipVerifySSL: "true"` fails
after upgrade.**

- **`endpoint`:** the run fails until the operator lists the endpoint's origin
  (`scheme://host[:port]`, no path) in `AGENT_LLM_ENDPOINT_ALLOWLIST` (Helm:
  `agentExecutor.llmEndpointAllowlist`). List only origins you control that do not redirect
  to another origin. The field is honored only by `anthropic` and `azure-openai`; for any
  other provider, remove it and set the provider's own variable on the agent-executor
  process instead.
- **`skipVerifySSL: "true"`:** the run fails whether or not an origin is listed. Remove the
  setting and mount the CA bundle, then set `SSL_CERT_FILE` or `SSL_CERT_DIR` on the
  agent-executor process (preferred; verification stays on). As a last resort,
  `LLM_SKIP_VERIFY_SSL=true` on the agent-executor process is a process-wide setting, not
  scoped to one Agent. In CLI local mode, set the same variable in your shell.

See [the Agent reference](../reference/api/agent.md) for the per-provider behavior.
