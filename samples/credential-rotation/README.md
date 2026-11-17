# LLM key rotation sample

`rotate-llm-key.sh` fetches an LLM provider key from a token endpoint you run and writes it into the `llm-credentials` Secret. It takes effect only when `agentExecutor.llmCredentialsSecret` names that Secret (see [Rotating LLM provider keys](../../docs/user/tasks/installation.md#rotating-llm-provider-keys-optional)); the agent-executor then reads the key file on every agent call, so nothing restarts.

## Run it

Run it from your own scheduler (cron, a CI schedule) with a kubeconfig that can reach the cluster:

```bash
TOKEN_URL=https://tokens.example.com/anthropic KEY_NAME=ANTHROPIC_API_KEY ./rotate-llm-key.sh
```

| Variable | Default | Meaning |
|---|---|---|
| `TOKEN_URL` | required | Endpoint that returns the key as the plain response body. Must be `https`. |
| `KEY_NAME` | required | The provider variable, for example `ANTHROPIC_API_KEY`. Must match `^[A-Z_][A-Z0-9_]*$`. |
| `SECRET_NAME` | `llm-credentials` | Secret to patch. |
| `NAMESPACE` | `ottoflow` | Namespace of the Secret. |
| `TOKEN_HEADER_FILE` | optional | File with one header line, for example `Authorization: Bearer ...`, sent with the request. Needs curl 7.55 or later. Keep it mode 0600. Its contents never reach the process arguments. |
| `KEY_PATTERN` | optional | Bash (POSIX extended) regex the key must match, for example `^sk-ant-`. An invalid pattern is refused before any request. |

The script never prints the key. It passes it to `kubectl` on stdin, so it is not in the process arguments and no temp file holds it. The script turns shell tracing off because tracing would print the key.

## Rights the caller needs

`get` and `patch` on that one Secret in that namespace. The sample ships no RBAC object; grant them to your scheduler's identity yourself.

## Why it refuses some responses

The script writes nothing unless the checks pass: a 2xx status, a non-empty body, one token of allowed characters (`A-Za-z0-9._~+/=:-`), 20 to 512 characters, and `KEY_PATTERN` if set. These reject empty, multi-line, HTML or JSON responses, but they cannot tell a well-formed wrong key from the right one, so set `KEY_PATTERN` when the provider's keys have a known prefix.

The agent-executor falls back to its environment variable only when the key file is missing, blank or unreadable (see [configuration](../../docs/user/reference/configuration.md#agent-executor)). Any other content is used as the key, so a bad value written into the Secret makes every agent step for that provider fail.

## When to revoke the old key

Not at rotation time. Keep it valid for the overlap window in [In-flight runs during a rotation](../../docs/user/tasks/installation.md#in-flight-runs-during-a-rotation).
