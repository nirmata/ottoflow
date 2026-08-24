# OttoFlow: AI Workflows for Kubernetes

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="images/brand/ottoflow-horizontal-dark.png">
  <source media="(prefers-color-scheme: light)" srcset="images/brand/ottoflow-horizontal-light.png">
  <img align="center" src="images/brand/ottoflow-horizontal-light.png" alt="OttoFlow Logo">
</picture>
<br></br>

[![Release](https://img.shields.io/github/v/release/nirmata/ottoflow?include_prereleases&sort=semver)](https://github.com/nirmata/ottoflow/releases)
[![License](https://img.shields.io/badge/license-BUSL--1.1-blue.svg)](LICENSE.md)
[![Go](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go&logoColor=white)](https://golang.org/)
[![Kubernetes](https://img.shields.io/badge/Kubernetes-1.29+-326CE5?logo=kubernetes&logoColor=white)](https://kubernetes.io/)

## 🚀 What is OttoFlow?

OttoFlow enables scalable and production-ready AI workflows on Kubernetes.
You define a workflow as a Kubernetes CRD, and OttoFlow executes it as a
DAG (directed acyclic graph) that mixes deterministic CEL, PromQL, K8s API
queries, and LLM agents — with LLM calls constrained to the steps that
actually need AI. Data collection, aggregation, and publication steps
stay deterministic; the LLM is spent only on analysis, and it sees computed
summaries rather than raw tool data that blow your context.

## 🔥 Why OttoFlow?

Kubernetes agentic applications generally follow a predictable pattern. They:

1. **Collect**: query cluster and workload data.
2. **Analyze**: process and synthesize that data, and
3. **Publish**: execute an action, or publish data or an event.

Offloading this entire loop to high-level prompts or unconstrained agents with
cluster access is an anti-pattern: it makes every run non-deterministic, widens
the attack surface, and drives up token cost.

OttoFlow codifies the `Collect → Analyze → Publish` loop into a deterministic
execution pattern, bringing reviewable engineering practices to the AI
orchestration layer. OttoFlow is not a general-purpose agent framework — if you
want free-form agents, use one of those instead.

## ✨ Key Features

- ✅ **Declarative and Deterministic, Kubernetes-native workflows** — define workflows capable of LLM calls, MCP, A2A, CEL, and more as Kubernetes CRDs in YAML.
- ✅ **Fast DAG execution** — explicit dependency resolution with parallel step batches for scalable fleet-wide execution.
- ✅ **Multi-provider LLM support** — use OpenAI, Anthropic, AWS Bedrock, Azure AI, other providers, or local models with vLLM.
- ✅ **Kubernetes and CNCF integrations** — query resources, scrape Prometheus (PromQL), integrate with Kagent using A2A, create OpenReports.io resources, monitor with OpenTelemetry, react to events, and schedule via cron.
- ✅ **Compiled CEL expressions** — fast and sandboxed execution with full Kubernetes and [Kyverno SDK CEL](https://github.com/kyverno/sdk) library support, with per-workflow cost limits.
- ✅ **Multiple step types** — Expressions, ResourceQuery, AgentRef, MCPToolCall, Mutate, ForEach, and more ([full list](docs/user/reference/api/workflow.md#step-types-one-per-step)).
- ✅ **Context optimization** — agent steps receive agregated computed summaries, never raw data dumps. Also replace expensive MCP flows with direct calls to prevent context bloat.
- ✅ **Retry & conditional execution** — configurable automatic retry policies, failure handling, and `matchConditions` gating per step.
- ✅ **CLI with local mode** — execute workflows in-process against your kubecontext for rapid testing, no controller or CRDs required.
- ✅ **Extensive AI-ready samples** — ready-to-run workflows under [`samples/`](samples/) covering cost, security, and compliance. Easy to generate and test with AI coding assistants.

## ⚡ Quick Start - Your first AI workflow in under ~60 seconds

## Install the CLI

```sh
brew install nirmata/tap/ottoflow
```

See [installation guide](docs/user/tasks/installation.md) for other options.

## Execute a CEL workflow

Start with a pure-CEL workflow — no LLM, no API key, nothing to install:

```sh
ottoflow run https://raw.githubusercontent.com/nirmata/ottoflow/refs/heads/main/samples/workflows/production/cluster-overview.yaml
```

This runs in-process against your current kubecontext, read-only — no
controller, no CRDs, nothing installed in your cluster, nothing to uninstall.

For details on the data collection, analysis, and reporting view the [workflow source](https://github.com/nirmata/ottoflow/blob/main/samples/workflows/production/cluster-overview.yaml).

## Execute an AI workflow

### Pick your LLM provider

`pod-triage` adds an LLM step. The sample's `Agent` defaults to **Gemini** —
set an API key and run it by path, no cloning or `--workflow-dir` needed:

```sh
GEMINI_API_KEY=AIza... \
  ottoflow run -f https://raw.githubusercontent.com/nirmata/ottoflow/main/samples/workflows/production/pod-triage.yaml
```

Use <https://aistudio.google.com/api-keys> to get an API key.

Prefer OpenAI, Anthropic, or no cloud key at all? Override with `--provider`/`--model`
and the matching environment variable — no editing the workflow required:

```sh
# OpenAI -- OPENAI_API_KEY must be set; --model optional (defaults to gpt-4o)
OPENAI_API_KEY=sk-... \
  ottoflow run samples/workflows/production/pod-triage.yaml -n ottoflow --provider openai
```

```sh
# Anthropic -- ANTHROPIC_API_KEY must be set; --model optional (defaults to a current Claude model)
ANTHROPIC_API_KEY=sk-ant-... \
  ottoflow run samples/workflows/production/pod-triage.yaml -n ottoflow --provider anthropic
```

```sh
# Local -- no cloud key, no cluster data leaves your machine. --model is required
# here; there is no default local model.
LLAMACPP_HOST=http://127.0.0.1:11434/ \
  ottoflow run samples/workflows/production/pod-triage.yaml -n ottoflow \
  --provider local --model gemma3:4b
```

Point `LLAMACPP_HOST` at any llama.cpp-compatible server — llama.cpp, ollama,
vLLM, or LM Studio. The local run above produces the transcript shown below.

Agent steps need an LLM: set `modelProvider` on the `Agent` CRD to `openai`,
`anthropic`, `azure-openai`, `google`/`gemini`, or `local` (any llama.cpp-compatible
server), or override it per run with `--provider`/`--model` as shown above. API
keys come from the **process environment** — `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`,
`GEMINI_API_KEY`, `AZURE_OPENAI_API_KEY` — not from `Agent.spec.config`. In-cluster,
set them on the agent-executor pod via `agentExecutor.env` in the Helm chart; in
local mode they come from your shell.

<!-- demo GIF: images/demo.gif — not committed yet; record it with `make demo`
     (needs vhs+ttyd+ffmpeg and a kind cluster, see images/demo.tape). Once the
     GIF is recorded and committed, embed it here with:
     <img src="images/demo.gif" alt="OttoFlow's pod-triage workflow prioritizing which failing pod to fix first"> -->

```sh
collectPods                    ✅ Succeeded          24ms
triagePods                     ✅ Succeeded          1.13s
publishTriage                  ✅ Succeeded          414µs

Outputs:
  triageSummary:
  4 pods scanned, 2 flagged unhealthy. Verdict: The crashy pod is the highest
  priority due to its significantly higher restart count (4), indicating a
  persistent issue requiring immediate attention.

  **Next Action:** Investigate the crashy pod's underlying cause by checking
  system logs for crash reasons and potential resource constraints.
```

For details view the complete [workflow source](https://github.com/nirmata/ottoflow/blob/main/samples/workflows/production/pod-triage.yaml).

## 🛠️ Five workflows you'll actually use

All paths are under [`samples/workflows/production/`](samples/workflows/production/).

| Workflow | What it does | You get |
|---|---|---|
| [`cluster-overview.yaml`](samples/workflows/production/cluster-overview.yaml) | Pure-CEL cluster snapshot — pod phases, per-namespace CPU/memory requests and limits, health verdict. No LLM, runs anywhere. | Structured report, zero prerequisites. |
| [`pod-triage.yaml`](samples/workflows/production/pod-triage.yaml) | Collect → Analyze → Publish; CEL extracts per-pod failure signals (restarts, OOMKilled, ImagePullBackOff), the LLM picks the single highest-priority pod and the concrete next action. | Prioritized verdict + next step. |
| [`resource-hygiene.yaml`](samples/workflows/production/resource-hygiene.yaml) | Detects 14 categories of unused or stale resources; LLM writes the cleanup report, Prometheus gauges track it. | Prioritized markdown report + metrics. |
| [`cost-analyzer.yaml`](samples/workflows/production/cost-analyzer.yaml) | Right-sizing from resource specs plus metrics-server/Prometheus P95 usage, per-workload savings. | Markdown report + estimated monthly $ savings. |
| [`workload-troubleshooter.yaml`](samples/workflows/production/workload-troubleshooter.yaml) | One failing pod: events + logs → LLM root-cause. **⚠ in-cluster only** (needs pod logs; not available in CLI local mode). | Root cause + remediation. |

There are 70+ more workflows in [`samples/`](samples/) covering cost, security, and
compliance automation.

## ☸️ Install in your cluster

<!-- TODO (do not merge until stable v0.1.0 is released): only chart 0.1.0-rc1 exists today. -->
```sh
helm install ottoflow oci://ghcr.io/nirmata/ottoflow \
  --version 0.1.0-rc1 --namespace ottoflow --create-namespace

kubectl apply -f samples/workflows/production/cluster-overview.yaml
ottoflow run cluster-overview -n ottoflow
```

The controller reconciles Workflows/WorkflowRuns and runs the leader-elected
scheduler; agent steps execute via the agent-executor pod (set LLM keys via
`agentExecutor.env` in the chart).

## 📚 Documentation · Help · Contributing · License

- [Getting started](docs/user/tasks/getting-started.md) and [installation](docs/user/tasks/installation.md)
- [Concepts](docs/user/concepts/) — architecture and execution model
- [API reference](docs/user/reference/api/) — Workflow, WorkflowRun, Agent, MCPServer, StepTemplate
- [CEL reference](docs/user/reference/cel-reference.md) — available functions, and the pitfalls worth reading first
- [CLI reference](cli/README.md) and [configuration reference](docs/user/reference/configuration.md) — flags, environment variables, and `ottoflow validate`
- [Sample workflows](samples/workflows/README.md) — production use cases, feature demos, and test fixtures
- [Developer guide](DEVELOPER.md) and [design notes](docs/dev/DESIGN.md)
- [Security policy](SECURITY.md) — supply-chain trust and vulnerability disclosure
- [License](LICENSE.md) and [license FAQ](LICENSING-FAQ.md)

Questions? Open a [GitHub Issue](https://github.com/nirmata/ottoflow/issues) —
Discussions are disabled.

Contributions are welcome — see [CONTRIBUTING.md](CONTRIBUTING.md) for setup, style
and the PR process, and [GOVERNANCE.md](GOVERNANCE.md) for how decisions get made.

---

<div align="center">

Built with ❤️ by the Nirmata team

[Report Bug](https://github.com/nirmata/ottoflow/issues) · [Request Feature](https://github.com/nirmata/ottoflow/issues)

</div>
