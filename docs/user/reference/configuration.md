# Configuration Reference

OttoFlow components are configured via **command-line flags**. Environment variables are supported as fallbacks (or as flag defaults when running in Kubernetes). This document lists flags and their corresponding environment variables for each binary.

## Component-to-configuration mapping

Each section below applies to a specific component. See [Architecture](../concepts/architecture.md) for the full component overview.

| Section | Component | Kubernetes resource | Configure via |
|---|---|---|---|
| [Controller](#controller) | `ghcr.io/nirmata/ottoflow/controller` | Deployment | `controller.args` in Helm values |
| [Workflow runner](#workflow-runner) | `ghcr.io/nirmata/ottoflow/workflow-runner` | Job pod (ephemeral) | Controller flags + `WorkflowRun.spec.execution.job.env` |
| [Agent executor](#agent-executor) | `ghcr.io/nirmata/ottoflow/agent-executor` | Deployment | `agentExecutor.*` in Helm values |
| [ottoflow CLI](#ottoflow-cli) | local binary | — | Command-line flags and shell env vars |

**Which section to check for a given problem:**
- Workflow not starting, Job not created, trigger not firing → [Controller](#controller)
- CEL expression error, step execution failure, Prometheus query failure → [Workflow runner](#workflow-runner)
- Agent/LLM call failing, TLS error to agent-executor → [Agent executor](#agent-executor)

---

---

## ottoflow CLI

### Global flags

| Flag | Shorthand | Default | Description |
|------|-----------|---------|-------------|
| `--namespace` | `-n` | kubeconfig context namespace, else `ottoflow` | Kubernetes namespace for workflow resources. In local mode (`--workflow-dir`) this must match the workflow's own `metadata.namespace`. |
| `--kubeconfig` | | `$HOME/.kube/config` (honors `$KUBECONFIG`) | Path to kubeconfig file |

### `ottoflow run`

A bare file path or http(s) URL positional argument (e.g. `ottoflow run samples/foo.yaml` or
`ottoflow run https://.../foo.yaml`) is fetched, checked for its `kind`, and routed
automatically: a `WorkflowRun` is applied in-cluster, anything else (typically a `Workflow`) is
executed locally — the same as passing it to `--file`. No `--workflow-dir` or other flag needed.

| Flag | Shorthand | Default | Description |
|------|-----------|---------|-------------|
| `--workflow` | `-w` | (empty) | Name of the workflow to execute |
| `--workflow-dir` | | (empty) | Load workflows from this directory and execute in-process against a fake control plane built from the files there — no controller needed. Referenced StepTemplates (and other OttoFlow objects) must live under the same directory tree. |
| `--file` | `-f` | (empty) | Run a single manifest locally, in-process, from a file, an http(s) URL, or `-` for stdin — no cluster/controller required. Mutually exclusive with `--workflow-dir`. |
| `--allow-insecure-url` | | `false` | Permit plain `http://` (non-TLS) URLs with `--file`, including on redirect |
| `--input` | `-i` | | Workflow input values as `key=value` (repeatable) |
| `--timeout` | | `10m` | Max time to wait for completion (cluster watch, and local execution) |
| `--watch` | `-W` | `true` | Watch execution progress (cluster mode only; local execution always runs to completion) |
| `--output` | `-o` | `table` | Output format: `table`, `json`, `yaml` |
| `--include-inputs` | | `false` | Include `spec.inputValues` in json/yaml output (may contain secrets) |
| `--max-workers` | | `5` | Max concurrent workers for `forEach` steps (local mode only) |
| `--prometheus-url` | | (empty) | Prometheus URL for CEL/prometheus steps (local mode only) |
| `--output-dir` | | (empty) | Save run output (JSON + Markdown) to a directory (created if needed) |
| `--provider` | | (empty) | Override LLM provider for all agent steps (local mode only); e.g. `openai`, `anthropic`, `google`, `local` |
| `--model` | | (empty) | Override LLM model for all agent steps (local mode only) |

`--provider`/`--model`, `--max-workers`, and `--prometheus-url` only apply to local execution
(`--workflow-dir` or `--file`); in cluster mode the CLI warns and ignores them where applicable
(`cli/cmd/run.go`).

### `ottoflow status`

| Flag | Shorthand | Default | Description |
|------|-----------|---------|-------------|
| `--output` | `-o` | `table` | Output format: `table`, `json`, `yaml` |
| `--include-inputs` | | `false` | Include `spec.inputValues` in json/yaml output |

### `ottoflow validate`

Runs static checks (DAG cycle detection, `dependsOn` alignment, undefined `inputs.*`, CEL syntax)
without executing steps or touching a cluster. It does not compile `outputs[].value` expressions,
so it is a strong check rather than a complete one.

| Flag | Shorthand | Default | Description |
|------|-----------|---------|-------------|
| `--file` | `-f` | (empty) | Load a workflow from a YAML file |
| `--workflow-dir` | | (empty) | Load workflows from a directory (local, no cluster) |
| `--generate-rbac` | | `false` | After validation passes, generate RBAC manifests |
| `--output` | | (empty) | Write generated RBAC to a file (only with `--generate-rbac`) |
| `--agent-executor-namespace` | | `ottoflow` | Namespace of the agent-executor Service (for `agentRef` RBAC rules) |

---

## Controller

The controller is the main OttoFlow manager process (e.g. `controller` or `/ko-app/controller`). Configure via **flags** (see `controller.args` in the Helm chart).

| Flag | Env var | Default | Description |
|------|---------|---------|-------------|
| `--metrics-bind-address` | | `:8080` | Metrics endpoint address |
| `--health-probe-bind-address` | | `:8081` | Health/readiness probe address |
| `--leader-elect` | | `true` | Enable leader election (set `false` for local dev only) |
| `--namespace` | | `ottoflow` | Leader election namespace (where the lease is created) |
| `--metrics-secure` | | `false` | Serve metrics over HTTPS |
| `--enable-http2` | | `false` | Enable HTTP/2 on the metrics/webhook servers |
| `--cel-cache-size` | | `1000` | Max compiled CEL expressions cached |
| `--prometheus-url` | | (empty) | Prometheus server URL for CEL `prometheusMetrics()`. When set, the controller uses it and **passes it to every workflow runner Job**, so you configure it once per environment (e.g. in Helm `controller.args`) and it does not need to be in the workflow. |
| `--agent-executor-service-name` | `AGENT_EXECUTOR_SERVICE_NAME` | (empty) | Agent executor Service name for internal TLS cert controller |
| `--agent-executor-namespace` | `AGENT_EXECUTOR_NAMESPACE` | `ottoflow` | Namespace for agent-executor TLS cert controller |
| `--workflow-runner-image` | `WORKFLOW_RUNNER_IMAGE` | (see below) | Image for the workflow runner Job |
| `--workflow-runner-service-account` | `WORKFLOW_RUNNER_SERVICE_ACCOUNT` | (empty) → derived `{workflow}-runner` | Service account for the runner Job |
| `--workflow-runner-cluster-role` | `WORKFLOW_RUNNER_CLUSTER_ROLE` | required (controller refuses to start if unset); the Helm chart sets `<fullname>-runner-role` (narrowed, runner-only role) | ClusterRole name for runner Job RBAC |
| `--agent-executor-caller-cluster-role` | `AGENT_EXECUTOR_CALLER_CLUSTER_ROLE` | (empty) | ClusterRole for agent-executor caller RBAC; empty disables |
| `--workflow-runner-agent-executor-ca-secret` | | (empty) | Secret name in run namespace for agent-executor CA (internal TLS); empty disables CA mount in runner |
| `--secret-source-namespace` | | (empty) | Namespace to copy runner Secret-backed volumes from when missing |
| `--workflow-runner-image-pull-secrets` | `WORKFLOW_RUNNER_IMAGE_PULL_SECRETS` | (empty) | Comma-separated Secret names for runner pod `imagePullSecrets` |
| `--workflow-runner-image-pull-policy` | `WORKFLOW_RUNNER_IMAGE_PULL_POLICY` | `IfNotPresent` | Runner container `imagePullPolicy` |
| `--workflow-runner-pod-labels-part-of` | `WORKFLOW_RUNNER_POD_LABELS_PART_OF` | (empty) | Value for runner pod label `app.kubernetes.io/part-of` (default in code: `ottoflow`) |
| `--workflow-runner-ttl-seconds-after-finished` | | `0` (= 3600) | Seconds before finished runner Jobs are deleted |
| `--workflow-runner-llm-credentials-secret` | `WORKFLOW_RUNNER_LLM_CREDENTIALS_SECRET` | (empty) | Secret for LLM credential injection into runner Jobs; empty disables |
| `--webhook-trigger-addr` | | (empty) | Address for the webhook-trigger HTTP server; empty disables |

Two more env vars are read directly by the controller (not exposed as flags):
`WEBHOOK_SERVICE_NAME` (default `ottoflow-webhook`) and `WEBHOOK_CONFIG_NAME`
(default `ottoflow-validating`).

**Logging:** The controller uses klog. Use `-v` for verbosity (e.g. `-v=2` for more detail). See [Logging](logging.md).

**Workflow runner image default when not set:** `ghcr.io/nirmata/ottoflow/workflow-runner:latest`

### Validating webhooks

When **webhook.enabled** is true (default), the controller serves **validating admission webhooks** on port **9443** with TLS. The controller uses the **internal cert manager** (same mechanism as agent-executor TLS) to generate a CA and serving certificate, then patches the `ValidatingWebhookConfiguration` with the CA bundle. No cert-manager or user-supplied certs are required. `webhook.failurePolicy` defaults to **`Fail`**, which rejects resources on any webhook error. Set it to **`Ignore`** to admit resources when the webhook cannot be reached (at the cost of skipping validation during webhook downtime).

Webhooks validate **Workflow** (DAG/cycles, optional WorkflowRef/AgentRef existence), **WorkflowRun** (`spec.workflowRef.name` required), **Agent**, and **MCPServer** (reserved for future rules).

---

## Tracing

The controller and workflow runner both initialize OpenTelemetry (`internal/tracing`). Tracing is
a no-op until the standard `OTEL_EXPORTER_OTLP_ENDPOINT` environment variable is set (e.g.
`localhost:4317`), which enables an OTLP gRPC exporter with a batch processor. The runner also
honors `TRACEPARENT`/`TRACESTATE` (injected by the controller when it builds the runner Job) to
chain its spans to the controller's.

---

## Workflow runner

The workflow runner is the process that executes a single WorkflowRun inside a Job pod. It is started by the controller with args/env set by the chart or deployment.

| Flag | Env fallback | Description |
|------|--------------|-------------|
| `--workflow-run-name` | `WORKFLOW_RUN_NAME` | WorkflowRun name (required) |
| `--workflow-run-namespace` | `WORKFLOW_RUN_NAMESPACE` | WorkflowRun namespace (required) |
| `--prometheus-url` | | Prometheus server URL for CEL `prometheusMetrics()` (optional) |

**Nirmata credentials** (for agent steps using the Nirmata LLM provider): In-cluster, use a Kubernetes Secret and reference it in `spec.execution.job.env` with `valueFrom.secretKeyRef` (see [Installation — Nirmata LLM credentials](../tasks/installation.md#nirmata-llm-credentials-optional)). Prefer the `NIRMATA_LLM_TOKEN` key; legacy keys `NIRMATA_LLM_SERVICEACCOUNT_TOKEN` and `NIRMATA_LLM_APIKEY` are still supported. For the CLI, set `NIRMATA_URL` and `NIRMATA_LLM_TOKEN` (or the legacy env vars) in your shell.

---

## Agent executor

The agent executor is the HTTPS service that runs agent steps on behalf of the workflow runner.

| Flag | Default | Description |
|------|---------|-------------|
| `--tls-port` | `8443` | TLS server port |
| `--agent-executor-caller-namespace` | `ottoflow` | Namespace for RBAC auth (SubjectAccessReview checks `configmaps/agent-executor-caller` in this namespace) |
| `-v` | `0` | klog verbosity level (e.g. `-v=2` for troubleshooting, `-v=4` for deep debugging). See [Logging](logging.md). |

Nirmata credentials for LLM are used by the **workflow runner** when it executes agent steps (set via env vars in the runner pod), not by the agent-executor binary itself.

---

## Summary: when are environment variables needed?

The **Helm chart** configures the controller with **flags only**; use `controller.args` for optional settings such as `--prometheus-url` or `--secret-source-namespace`. For **Nirmata credentials**: in-cluster use a Kubernetes Secret (reference in WorkflowRun `spec.execution.job.env` with `valueFrom.secretKeyRef`); for the CLI use environment variables (see table below).

| Context | Env vars you might set |
|--------|--------------------------|
| **Workflow runner pod** (optional) | **Nirmata:** Use a Secret with key `NIRMATA_LLM_TOKEN` (or legacy keys) and reference it in WorkflowRun `spec.execution.job.env` with `valueFrom.secretKeyRef`. **Prometheus:** set `--prometheus-url` on the controller; the controller passes it to every runner Job (no need to put it in the workflow). |
| **CLI** | `NIRMATA_URL`, `NIRMATA_LLM_TOKEN` (or legacy `NIRMATA_LLM_SERVICEACCOUNT_TOKEN` / `NIRMATA_LLM_APIKEY`) — set as environment variables in your shell when using `ottoflow run` with agent steps. |

The controller injects `WORKFLOW_RUN_NAME`, `WORKFLOW_RUN_NAMESPACE`, `JOB_NAME`, and `POD_NAME` into every runner pod; you do not set those yourself.
