# kagent quickstart: call an in-cluster agent over A2A

This bundle wires OttoFlow to a [kagent](https://github.com/kagent-dev/kagent) agent
running **in the same cluster**, reached over plaintext `http://` at its cluster-local
Service address. It is the "no TLS, no bearer token" counterpart to
[`samples/workflows/features/kagent-integration.yaml`](../workflows/features/kagent-integration.yaml),
which calls a remote kagent over `https://` with a bearer token.

Written against **kagent v0.10.x** (CRD group `kagent.dev`, storage version `v1alpha2`).
If your kagent version differs, check the field names in `kagent-modelconfig.yaml` and
`kagent-agent.yaml` against your installed CRDs.

> **Prerequisites:** this bundle configures a kagent agent + an OttoFlow workflow — it does **not** install kagent itself. A running kagent (v0.10.x) must already be in your cluster before you apply these manifests (see step 1).

## Files

| File | What it is |
|------|------------|
| `kagent-modelconfig.yaml` | kagent `ModelConfig` pointing at an Anthropic model |
| `kagent-agent.yaml`       | kagent `Agent` (`k8s-agent`): a declarative cluster-health analyst |
| `workflow.yaml`           | OttoFlow `Workflow` that calls `k8s-agent` over A2A |

## Steps

### 1. Install kagent

This bundle does **not** install kagent — install it first via its official
[install guide](https://kagent.dev/docs). For a quick in-cluster install (adjust the
version to your current kagent v0.10.x release; this bundle was written and validated
against `v0.10.0-rc2`):

```bash
helm install kagent-crds oci://ghcr.io/kagent-dev/kagent/helm/kagent-crds \
  --version 0.10.0-rc2 -n kagent --create-namespace
helm install kagent oci://ghcr.io/kagent-dev/kagent/helm/kagent \
  --version 0.10.0-rc2 -n kagent
```

This bundle assumes kagent runs in the `kagent` namespace and exposes agents at
`http://kagent-controller.kagent.svc:8083/api/a2a/kagent/<agent-name>`.

### 2. Create the Anthropic API key Secret

The `ModelConfig` reads the key `ANTHROPIC_API_KEY` from a Secret named `anthropic-key`
in the `kagent` namespace. Create it with your own key (no Secret manifest is committed
here on purpose):

```bash
kubectl create secret generic anthropic-key -n kagent \
  --from-literal=ANTHROPIC_API_KEY=<your-anthropic-api-key>
```

### 3. Apply the manifests and run the workflow

```bash
kubectl apply -f kagent-modelconfig.yaml
kubectl apply -f kagent-agent.yaml
kubectl apply -f workflow.yaml

ottoflow run kagent-quickstart --input clusterName=my-cluster
# optionally scope to one namespace:
#   ottoflow run kagent-quickstart --input clusterName=my-cluster --input namespace=kube-system
```

The `analyzeCluster` step sends the prompt to the agent and captures its reply from
`a2aResult.artifacts[0].parts[0].text`; `reportResults` formats it into the workflow
outputs `healthReport` and `report`.

## Using a different LLM provider

kagent's `ModelConfig` supports several LLM providers; this bundle defaults to
Anthropic (Claude), but you can point it at any provider kagent supports.

To swap providers, edit `kagent-modelconfig.yaml`: set `spec.provider` to one of
the enum values below, set `spec.model` to a model name for that provider, and
point `spec.apiKeySecret` / `spec.apiKeySecretKey` at a Secret holding your key
(add a provider-specific sub-block like `spec.openAI` or `spec.gemini` only if
you need to set optional tuning fields). Nothing else needs to change — the
`Agent` and `Workflow` reference the `ModelConfig` by name, not by provider.

`spec.provider` enum (kagent v0.10.x, `v1alpha2`): `Anthropic`, `OpenAI`,
`AzureOpenAI`, `Ollama`, `Gemini`, `GeminiVertexAI`, `AnthropicVertexAI`,
`Bedrock`, `SAPAICore`, `Foundry`.

**OpenAI:**

```yaml
spec:
  provider: OpenAI
  model: gpt-4o
  apiKeySecret: openai-key
  apiKeySecretKey: OPENAI_API_KEY
```

```bash
kubectl create secret generic openai-key -n kagent \
  --from-literal=OPENAI_API_KEY=<your-openai-api-key>
```

**Google Gemini:**

```yaml
spec:
  provider: Gemini
  model: gemini-3.6-flash
  apiKeySecret: gemini-key
  apiKeySecretKey: GEMINI_API_KEY
```

```bash
kubectl create secret generic gemini-key -n kagent \
  --from-literal=GEMINI_API_KEY=<your-gemini-api-key>
```

**Ollama** (self-hosted, in-cluster, no API key) is also supported — point
`spec.ollama.host` at your Ollama service instead of setting `apiKeySecret`.

All three example shapes above were validated with
`kubectl apply --dry-run=server` against the kagent v0.10 `ModelConfig` CRD.

## About `allowInsecureHTTP`

`allowInsecureHTTP: true` on the `externalAgentRef` step relaxes OttoFlow's default
HTTPS-only check so it will dial a **cluster-local** host over plaintext `http://` — a
host whose name ends in `.svc` / `.svc.cluster.local`, or is exactly
`localhost` / `127.0.0.1` / `::1`. `http://` to any other host is still rejected, and the
flag cannot be combined with `auth.secretRef` or `caSecretRef` (OttoFlow refuses to send a
bearer token or apply a CA bundle over cleartext http). `https://` ignores the flag entirely.

> **NetworkPolicy is the real network boundary.** `allowInsecureHTTP` only relaxes
> OttoFlow's own scheme check; it does not encrypt or authenticate anything. For
> in-cluster plaintext calls, enforce who may reach the agent with a Kubernetes
> `NetworkPolicy` (or your service mesh). Prefer `https://` with a bearer token
> (`auth.secretRef`) whenever the agent is outside the cluster or crosses a trust
> boundary — see the sibling `kagent-integration.yaml` sample.

## Adding tools to the agent

`k8s-agent` ships with only its system prompt, so it reasons from what you tell it. To
let it inspect the cluster directly, add tools under `spec.declarative.tools` in
`kagent-agent.yaml` (MCP servers or other agents). The exact tool item shape depends on
your kagent version — see the kagent docs.
