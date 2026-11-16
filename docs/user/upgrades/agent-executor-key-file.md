# Upgrading: Agent-executor LLM key file

**With `agentExecutor.llmCredentialsSecret` set, the agent-executor now also mounts that Secret
and prefers the mounted key file over the same variable in its environment.**

- **Stale key in the Secret:** if the Secret still holds an old `ANTHROPIC_API_KEY` (or another
  provider key) while `agentExecutor.env` overrides it, every agent step now sends the Secret's
  key and may return 401. Update or remove the stale key in the Secret before you upgrade.
- **Rotation:** patch the Secret; no restart is needed. It is eventually consistent, not
  instant: the new key applies after the kubelet refreshes the mount (up to the kubelet sync
  period plus cache propagation).
- **Missing or blank file:** the provider's environment variable is used as before. An
  unreadable file logs one error and falls back to the environment. Any other read error
  fails the step.

The mount is a directory (no `subPath`); keep it that way if you patch the chart, because a
`subPath` mount never receives Secret updates.
