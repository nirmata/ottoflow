# Developer Guide

This guide is for developers contributing to OttoFlow or integrating it into their systems.

---

## 🏗️ Architecture

### The Three Images

OttoFlow ships three container images. Understanding which image does what is critical for development, debugging, and deployments.

| Image | Binary source | Kubernetes resource | Role |
|---|---|---|---|
| `ghcr.io/nirmata/ottoflow/controller` | `cmd/controller/` | Deployment (persistent) | Kubernetes operator: reconciles CRDs, spawns runner Jobs, manages TLS certs, serves admission webhooks |
| `ghcr.io/nirmata/ottoflow/agent-executor` | `cmd/agent-executor/` | Deployment (persistent) | HTTPS service: executes LLM/agent steps called by runner pods via `POST /api/exec/{ns}/{agent}` |
| `ghcr.io/nirmata/ottoflow/workflow-runner` | `cmd/workflow-runner/` | Job pod (ephemeral) | Workflow execution engine: evaluates CEL, runs all step types, calls agent-executor for LLM steps |

**The workflow-runner image contains all CEL evaluation logic and step execution code.** The controller and agent-executor do not evaluate CEL.

### Execution flow

```
kubectl apply WorkflowRun
        |
        v
Controller (Deployment)
   - Validates CRD, builds DAG
   - Creates Kubernetes Job → runner pod image = --workflow-runner-image flag
        |
        v
Workflow Runner (Job pod, one per WorkflowRun)
   - Loads Workflow + WorkflowRun from API server
   - Executes steps (CEL, resource queries, MCP tool calls, etc.) in-process
   - For agentRef steps: POST https://ottoflow-agent-executor:8443/api/exec/{ns}/{agent}
        |
        v
Agent Executor (Deployment, HTTPS service)
   - Looks up Agent CRD, builds prompt, calls LLM
   - Returns response + extracted outputs
        |
        v
LLM provider (Nirmata, OpenAI, etc.)
```

### Which image to update for a given change

| Change type | Update this image |
|---|---|
| CEL library bug fix or new function | `workflow-runner` |
| Any step execution logic (resource query, mutate, MCP, etc.) | `workflow-runner` |
| LLM call logic, agent prompt building, output extraction | `agent-executor` |
| MCP client/tool registration inside agent steps | `agent-executor` |
| CRD reconciliation, Job spawning, trigger handling | `controller` |
| Admission webhook rules | `controller` |
| TLS certificate management | `controller` |

### Updating the runner image without redeploying the controller

The runner image is passed to the controller via `--workflow-runner-image`. Change this flag and the controller picks it up for subsequent WorkflowRuns without a full redeployment:

```bash
# Via kubectl patch (quick fix)
kubectl patch deployment ottoflow-controller-manager -n ottoflow \
  --type=json \
  -p='[{"op":"replace","path":"/spec/template/spec/containers/0/args","value":["--leader-elect","--workflow-runner-image=ghcr.io/nirmata/ottoflow/workflow-runner:v1.2.3"]}]'

# Via Helm (recommended)
helm upgrade ottoflow ./charts/ottoflow -n ottoflow \
  --set controller.args={"--leader-elect","--workflow-runner-image=ghcr.io/nirmata/ottoflow/workflow-runner:v1.2.3"}
```

See [Architecture](docs/user/concepts/architecture.md) for full details.

### Project Structure

Verified against the working tree (`internal/controller/` and `internal/mcp/` do not
exist — controllers live under `internal/workflow/controller/`, and MCP client code
lives under `internal/agent/`):

```
.
├── api/v1alpha1/          # CRD type definitions and OpenAPI schemas
├── cmd/
│   ├── controller/        # controller binary (image: ghcr.io/nirmata/ottoflow/controller)
│   ├── agent-executor/    # agent-executor binary (image: ghcr.io/nirmata/ottoflow/agent-executor)
│   └── workflow-runner/  # runner binary (image: ghcr.io/nirmata/ottoflow/workflow-runner)
├── internal/
│   ├── workflow/
│   │   ├── controller/    # Kubernetes controllers (reconciliation logic), cron scheduler,
│   │   │                  # callback server, webhook trigger server
│   │   ├── executor/      # All CEL + step execution logic (runs in workflow-runner)
│   │   │   ├── cel.go         # CEL expression evaluator
│   │   │   ├── cel_libraries.go # CEL library integration
│   │   │   ├── exec_handler.go  # HTTP handler for agent-executor /api/exec endpoint
│   │   │   ├── agent_executor.go # Agent step execution (runner side)
│   │   │   ├── resource_macros.go # resource.Get() etc.
│   │   │   └── ...
│   │   ├── cluster/       # Target-cluster client resolution (ClusterRef)
│   │   └── token/         # Token handling
│   ├── agent/             # Agent/LLM provider integration (MCP client code lives here too)
│   ├── auth/              # TokenReview + SubjectAccessReview authenticator (agent-executor)
│   ├── certmanager/       # Internal TLS cert bootstrap (no cert-manager dependency)
│   ├── webhook/           # Admission validators
│   ├── metrics/           # Prometheus metric registration
│   ├── tracing/           # OpenTelemetry tracer provider
│   └── logging/           # Structured logging helpers
├── cli/                   # Command-line tool (`ottoflow` CLI)
├── samples/workflows/     # Example workflows
├── config/                # Kustomize configs and CRD manifests
└── docs/                  # Documentation
```

### Key Components

#### Controller (`internal/workflow/controller/`)
- **Reconciler**: Manages Workflow and WorkflowRun lifecycle
- **Event Handler**: Processes Kubernetes events for triggers
- **Cron Manager**: Handles scheduled workflow execution

#### Workflow Executor (`internal/workflow/executor/`)

This package runs inside the **workflow-runner pod** (and the CLI in local mode). It is the only place where CEL is evaluated and steps are executed.

- **CELEvaluator**: Evaluates CEL expressions with workflow context
- **WorkflowExecutor**: Orchestrates all step types (DAG traversal, step dispatch)
- **ResourceQuery**: Handles Kubernetes resource queries
- **ExecHandler / OttoFlowAgentExecutor**: HTTP handler and executor for agent steps — `ExecHandler` runs inside the agent-executor service; `executeAgentViaExecHTTP` in `agent_executor.go` is the caller in the runner

#### CEL Integration
- Uses `github.com/google/cel-go` for expression evaluation
- Integrates Kyverno CEL libraries via [Kyverno SDK CEL](https://github.com/kyverno/sdk/tree/main/cel) (HTTP, JSON, YAML, Math, etc.)
- Integrates Kubernetes CEL libraries (Lists, Regex, URL, IP, CIDR, etc.)
- Custom resource macros (`resource.Get()`, `resource.List()`, etc.)

---

## 🛠️ Development Setup

### Prerequisites

- Go 1.26+
- Kubernetes cluster (1.20+) or kind/minikube
- `kubectl` configured
- `make` (for build automation)

### Building

```bash
# Build controller binary
make build

# Build CLI tool
make build-cli

# Cross-compile the CLI for linux/darwin/windows into bin/
make build-cli-all

# Build everything
make all

# Run tests
make test

# Generate CRDs from Go types
make manifests

# Generate code (deepcopy, clientset, etc.)
make generate
```

### Building container images (controller with full flags)

The controller image is built with [ko](https://ko.build/) (no Dockerfile). The built image includes all current flags, including `--workflow-runner-image-pull-secrets` (needed for runner Jobs to pull from private registries).

**Prerequisites:** `ko` (e.g. `go install github.com/google/ko@latest`) and Docker (or compatible daemon).

```bash
# Build controller image locally (loads into Docker). Uses KO_DOCKER_REPO for image name.
make ko-build-manager

# Image tag: git tag if on a tag, else 0.0.0-g<short-sha>. Override with IMAGE_TAG=0.0.1
make image-version   # show tag

# Push to a registry (requires login, e.g. docker login ghcr.io)
export KO_DOCKER_REPO=ghcr.io/nirmata/ottoflow   # or your registry
make ko-push-manager

# Use a specific tag
IMAGE_TAG=0.0.1 make ko-push-manager
```

With `KO_DOCKER_REPO=ghcr.io/nirmata/ottoflow` and `IMAGE_TAG=0.0.1`, the image is `ghcr.io/nirmata/ottoflow/controller:0.0.1`. Set `controller.image.fullOverride` in Helm to that image, and you can use `--workflow-runner-image-pull-secrets` in `controller.args`.

To build (or push) all three images — `controller`, `agent-executor`, and `workflow-runner` — at once instead of just the controller, use `make ko-build`/`make ko-push` in place of the `-manager`-suffixed targets above.

### Running Locally

#### Option 1: Run Controller Locally

```bash
# Install CRDs
make install

# Run controller (connects to your kubeconfig cluster)
make run

# Run with metrics server URL
METRICS_SERVER_URL=http://localhost:8080 make run

# Run with custom metrics and Prometheus
METRICS_SERVER_URL=http://localhost:8080 \
PROMETHEUS_URL=http://localhost:9090 \
make run
```

#### Option 2: Use CLI (create WorkflowRun in cluster)

```bash
# Build CLI
make build-cli

# Create a WorkflowRun (controller must be running to execute)
./bin/ottoflow run hello-world \
  --input name="OttoFlow" \
  --namespace default
```

### Testing

```bash
# Run all tests
make test

# Run specific test package
go test ./internal/executor/...

# Run with verbose output
go test -v ./internal/executor/...

# Run integration tests (requires cluster)
make test-integration
```

### What CI does

The `CI` workflow (`.github/workflows/ci.yaml`) runs on pushes to `main`, `v*.*.*` tags, and
pull requests. Its jobs:

- **build** — `make build-cli`, then `helm lint`/`helm template` the chart and the chart RBAC
  assertions (`go test ./test/chart/...`); uploads `bin/ottoflow` as an artifact.
- **lint** — `golangci-lint` (v2.11.4) and a check that all Actions are SHA-pinned.
- **verify-codegen** — `make verify-codegen`; fails if generated CRDs, deepcopy code, or docs
  (CRD API reference, CLI reference) are stale or uncommitted.
- **validate-samples** — builds the CLI, then `ottoflow validate --workflow-dir samples`.
- **test** — `make test` (with cached envtest binaries for Kubernetes 1.29.0).
- **images** — on pushes to `main` (not on PRs or tags), builds and pushes all three
  container images with `ko` to `ghcr.io/nirmata/ottoflow`.

### Code Generation

OttoFlow uses Kubernetes code generators:

```bash
# Generate deepcopy methods, clientsets, informers, listers
make generate

# Generate CRD manifests from Go types (also syncs to Helm chart)
make manifests

# Sync CRDs to Helm chart manually (if needed)
make sync-crds

# Generate OpenAPI schemas
make openapi
```

**CRD Management:**
- CRDs are generated from Go code in `api/` to `config/crd/bases/`
- CRDs are automatically synced to `charts/ottoflow/crds/` when running `make manifests`
- Source of truth: `config/crd/bases/` (generated from Go code)
- Do not edit CRDs in `charts/ottoflow/crds/` directly - they are synced automatically

### Build-time variables (Makefile)

`KO_DOCKER_REPO` and `IMAGE_TAG` are covered above, under "Building container images". A few
more `make` variables are worth knowing about:

| Variable | Default | Purpose |
|---|---|---|
| `IMG` / `WORKFLOW_RUNNER_IMG` / `AGENT_EXECUTOR_IMG` | — | Image overrides for `make generate-manifests`/`make deploy` |
| `HELM_CHART_PATH` | `./charts/ottoflow` | Chart location used by Helm-based `make` targets |
| `HELM_RELEASE_NAME` | `ottoflow` | Release name used by Helm-based `make` targets |
| `HELM_NAMESPACE` | `ottoflow` | Install namespace used by Helm-based `make` targets |
| `ENVTEST_K8S_VERSION` | `1.29.0` | Kubernetes test-binary version `make test` downloads via `setup-envtest` |

---

## 📝 Code Style & Standards

### Go Conventions

- Follow standard Go formatting (`go fmt`)
- Use `golangci-lint` for linting
- Write tests for new features
- Document exported functions and types

### Project-Specific Patterns

#### CEL Expression Evaluation

```go
// Create evaluator with workflow context
evaluator, err := executor.NewCELEvaluator(client, workflowRun)
if err != nil {
    return err
}

// Evaluate expression
result, err := evaluator.EvaluateExpression(ctx, expr, vars)
```

#### Resource Queries

```go
// Execute resource query step
executor := executor.NewStepExecutor(client, evaluator)
result, err := executor.ExecuteResourceQuery(ctx, step, vars)
```

#### Agent Steps

```go
// Execute agent step
agentExecutor := agent.NewExecutor(agentClient)
result, err := agentExecutor.Execute(ctx, step, vars)
```

---

## 🔧 CEL Library Integration

### Architecture

OttoFlow uses `EnvSet` from `k8s.io/apiserver/pkg/cel/environment` to integrate CEL libraries:

```go
// Create base EnvSet
baseEnvSet := apiservercel.MustBaseEnvSet(apiservercel.DefaultCompatibilityVersion())

// Extend with all options in a single call
extendedEnvSet, err := baseEnvSet.Extend(
    apiservercel.VersionedOptions{
        IntroducedVersion: apiservercel.DefaultCompatibilityVersion(),
        EnvOptions:        baseOpts,
    },
    apiservercel.VersionedOptions{
        IntroducedVersion: apiservercel.DefaultCompatibilityVersion(),
        EnvOptions:        kyvernoOpts,
    },
)
```

### Adding New CEL Libraries

1. Add library to `GetKyvernoCELOptionsWithImpls()` in `cel_libraries.go`
2. Declare variables if needed (e.g., `celapi.Variable("http", http.ContextType)`)
3. Provide globals in evaluation context if required
4. Update documentation

See [CEL Reference](docs/user/reference/cel-reference.md) for details.

### Custom Macros

Custom macros are registered in `resource_macros.go`:

```go
// Example: resource.Get macro
func GetResourceMacroOptions(...) ([]celapi.EnvOption, error) {
    return []celapi.EnvOption{
        celapi.Function("resource.Get",
            celapi.Overload("resource_get",
                []*celapi.Type{/* params */},
                celapi.DynType,
                celapi.FunctionBinding(impl),
            ),
        ),
    }, nil
}
```

---

## 🧪 Testing

### Unit Tests

```bash
# Run executor tests
go test ./internal/executor/...

# Run controller tests
go test ./internal/controller/...

# Run with coverage
go test -cover ./...
```

### Integration Tests

```bash
# Requires running Kubernetes cluster
make test-integration

# Test specific workflow (Workflow must exist in cluster)
./bin/ottoflow run test-native-types --namespace default
```

### Test Workflows

Sample workflows in `samples/workflows/` serve as integration tests:

- `39-test-native-types.yaml` - Tests JSON/YAML/HTTP libraries
- `36-kyverno-cel-libraries-example.yaml` - Tests all Kyverno libraries
- `38-practical-cel-examples.yaml` - Tests practical CEL patterns

---

## 🐛 Debugging

### Controller Logs

```bash
# View controller logs
kubectl logs -n ottoflow-system deployment/ottoflow-controller-manager

# Follow logs
kubectl logs -f -n ottoflow-system deployment/ottoflow-controller-manager
```

### WorkflowRun Debugging

```bash
# Get WorkflowRun status
kubectl get workflowrun <name> -o yaml

# Check step status
kubectl get workflowrun <name> -o jsonpath='{.status.steps[*]}'

# View events
kubectl get events --field-selector involvedObject.name=<workflowrun-name>
```

### CEL Expression Debugging

Enable debug logging in executor:

```go
// Add debug logging
log.V(1).Info("Evaluating expression", "expr", expr, "vars", vars)
```

---

## 📚 Documentation

### Developer Documentation

All detailed developer documentation is located in `docs/dev/`:

- **[Design Document](docs/dev/DESIGN.md)** - Architecture and design decisions
- **[Implementation Details](docs/dev/DESIGN.md#implementation-details)** - Implementation summary and file references
- **[CEL Reference](docs/user/reference/cel-reference.md)** - CEL macros, variable access, and upstream library links

### User Documentation

User-facing documentation is located in `docs/user/`:

- **[Concepts](docs/user/concepts/)** - Understanding OttoFlow architecture and key concepts
- **[Tasks](docs/user/tasks/)** - Step-by-step guides for common tasks
- **[Reference](docs/user/reference/)** - Complete API reference and CEL function documentation

### API Documentation

- **[API Reference](docs/user/reference/api/README.md)** - Complete CRD API reference

---

## 🤝 Contributing

### Workflow

1. Fork the repository
2. Create a feature branch (`git checkout -b feature/amazing-feature`)
3. Make your changes
4. Add tests for new functionality
5. Run tests and linting (`make test`, `make lint`)
6. Update documentation if needed
7. Commit your changes (`git commit -m 'Add amazing feature'`)
8. Push to the branch (`git push origin feature/amazing-feature`)
9. Open a Pull Request

### Pull Request Checklist

- [ ] Code follows project style guidelines
- [ ] Tests added/updated and passing
- [ ] Documentation updated
- [ ] No breaking changes (or documented if necessary)
- [ ] CRDs regenerated if types changed (`make manifests`)

### Code Review Process

1. PR is reviewed by maintainers
2. Address review comments
3. Once approved, maintainers will merge

---

## 🔍 Implementation Details

### CEL Environment Setup

OttoFlow uses `EnvSet` to properly integrate CEL libraries:

```go
// Base environment with Kubernetes CEL libraries
baseEnvSet := apiservercel.MustBaseEnvSet(apiservercel.DefaultCompatibilityVersion())

// Extend with OttoFlow-specific options and all Kyverno libraries in single call
extendedEnvSet, err := baseEnvSet.Extend(
    apiservercel.VersionedOptions{
        IntroducedVersion: apiservercel.DefaultCompatibilityVersion(),
        EnvOptions:        baseOpts,  // OttoFlow variables, resource macros, etc.
    },
    apiservercel.VersionedOptions{
        IntroducedVersion: apiservercel.DefaultCompatibilityVersion(),
        EnvOptions:        kyvernoOpts,  // All Kyverno libraries including JSON/YAML
    },
)
```

**Why this works**: Extending `EnvSet` with all options in a single call ensures proper checker initialization and avoids the `extendEnv()` bug. See `docs/KYVERNO_WHY_IT_WORKS.md` for details.

### Workflow Execution Flow

1. **Controller** receives WorkflowRun creation/update
2. **Reconciler** validates and initializes WorkflowRun
3. **DAG Resolver** calculates step execution order
4. **Step Executor** executes steps sequentially (respecting dependencies)
5. **CEL Evaluator** evaluates expressions with workflow context
6. **Status Updater** updates WorkflowRun status after each step

### Variable Resolution

Variables are resolved in this order:
1. `inputs.*` - Workflow input values
2. `variables.*` - Outputs from previous steps (flat namespace)
3. `expressions.*` - Current step's expression results
4. Context variables (`http`, `globalContext`, `image`)

---

## 🚀 Performance Considerations

### CEL Expression Caching

CEL environments are created once per WorkflowRun and reused for all expressions in that run.

### Resource Query Optimization

- Direct `client-go` integration (no API server round-trips for queries)
- Efficient list queries with label selectors
- Single resource queries use direct Get() calls

### Agent Step Optimization

- Connection pooling for LLM providers
- Request batching where supported
- Timeout handling for long-running operations

---

## 📊 Status & Roadmap

**Current Version**: v1.0-alpha

**Implemented**:
- ✅ Core orchestration (DAG, dependencies, execution)
- ✅ CEL expression engine with all Kyverno/Kubernetes libraries
- ✅ Resource Query DSL
- ✅ Agent steps (multi-provider)
- ✅ MCP tool calls
- ✅ Event & Cron triggers
- ✅ Retry logic & conditional execution
- ✅ Workflow references (sub-workflows)
- ✅ Step templates
- ✅ CLI tool

**In Progress**:
- 🚧 Advanced metrics (Prometheus integration)
- 🚧 Job-based execution

**Planned**:
- ⏳ Per-step security (service accounts)
- ⏳ Kueue integration
- ⏳ Agent Sandbox integration
- ⏳ External storage
- ⏳ IDE/editor support for CEL autocomplete

**Shipped**: `go build ./...` and `go mod download` work with no private
dependencies and no Nirmata org access required (requires Go 1.26+).
`internal/agent/` ships a provider-agnostic `DefaultAgentExecutor` behind the
`AgentExecutor` interface (`internal/agent/interfaces.go`); `RoutingAgentExecutor`
dispatches by `Agent.spec.modelProvider` — `nirmata` routes to an executor
injected by the enterprise plugin, every other provider routes to
`DefaultAgentExecutor`. See `docs/dev/DESIGN.md` for the full design.

---

## 🔗 References

- [Kubernetes CEL Libraries](https://kubernetes.io/docs/reference/using-api/cel/)
- [Kyverno SDK CEL](https://github.com/kyverno/sdk/tree/main/cel) (implementation), [Kyverno CEL Libraries](https://kyverno.io/docs/policy-types/cel-libraries/) (docs)
- [Common Expression Language (CEL)](https://github.com/google/cel-spec)
- [Model Context Protocol (MCP)](https://modelcontextprotocol.io/)

---

## 💬 Getting Help

- **Issues**: [GitHub Issues](https://github.com/nirmata/ottoflow/issues)
- **Discussions**: [GitHub Discussions](https://github.com/nirmata/ottoflow/discussions)
- **Documentation**: [docs/](docs/)
