# Building and Testing Locally (with Kind)

This document explains how to build OttoFlow, run unit tests, and test against a local [kind](https://kind.sigs.k8s.io/) cluster.

## Prerequisites

- **Go** 1.26+ (see `go.mod` for the exact version)
- **Docker** (for container images and kind)
- **kind** – [install kind](https://kind.sigs.k8s.io/docs/user/quick-start/#installation)
- **kubectl** – [install kubectl](https://kubernetes.io/docs/tasks/tools/)
- **make**

## Build the controller binary

From the repo root:

```bash
# Generate manifests and code, then build the manager binary
make build
```

This runs `manifests`, `generate`, `fmt`, `vet`, `lint`, and builds `bin/manager`.

Other useful targets:

| Command           | Description                          |
|-------------------|--------------------------------------|
| `make manifests`  | Generate CRDs into `config/crd/bases` |
| `make generate`   | Generate deepcopy and other Go code   |
| `make build-cli`  | Build the `ottoflow` CLI in `bin/`    |
| `make run`        | Build and run the controller locally (see below) |

## Unit tests

Unit tests use [envtest](https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/envtest) (no real cluster required):

```bash
make test
```

To run tests for a specific package:

```bash
go test ./internal/workflow/executor/... -v
go test ./internal/workflow/controller/... -v
```

## Run the controller locally (any cluster)

You can run the controller on your host and point it at any cluster in your kubeconfig (including a kind cluster):

```bash
# 1. Ensure your kubeconfig context targets the cluster (e.g. kind-kind)
kubectl config use-context kind-kind

# 2. Install CRDs into the cluster
make install

# 3. Run the controller locally (it will use the current kubeconfig)
make run
```

The controller will reconcile Workflows and WorkflowRuns in the cluster. To use a different context, set `KUBECONFIG` or switch context before `make run`.

Optional environment variables for local runs:

- `METRICS_SERVER_URL` – metrics server URL (e.g. `http://localhost:8080`)
- `PROMETHEUS_URL` – Prometheus URL (e.g. `http://localhost:9090`)

## Kind cluster: create and use

### 1. Create a kind cluster

```bash
kind create cluster --name kind
```

Use `--name` to set the cluster name; the default is `kind`. If you use a different name, set the `KIND_CLUSTER` environment variable when loading images or running e2e tests.

### 2. Build the controller image (ko)

OttoFlow builds container images with [ko](https://github.com/ko-build/ko). For local use with kind, use a repository name that you can load into the cluster (e.g. a fake registry or `ko.local`):

```bash
# Build the manager image; use a tag that you will load into kind
export KO_DOCKER_REPO=example.com/ottoflow
make ko-build-manager
```

ko will print the image reference (e.g. `example.com/ottoflow/cmd-controller-abc1234`). You can tag it for easier use:

```bash
# Optional: tag the image with a fixed name (replace <image-ref> with ko’s output)
docker tag <image-ref> example.com/ottoflow:dev
```

### 3. Load the image into kind

```bash
# If you tagged the image as example.com/ottoflow:dev
kind load docker-image example.com/ottoflow:dev --name kind

# If your cluster has a different name
KIND_CLUSTER=mycluster kind load docker-image example.com/ottoflow:dev --name mycluster
```

### 4. Install CRDs and deploy the controller

```bash
# Install CRDs
make install

# Generate install manifest with your image and deploy
make deploy IMG=example.com/ottoflow:dev
```

The controller will run in the cluster. Check that the manager pod is running:

```bash
kubectl get pods -n ottoflow -l control-plane=controller-manager
```

### 5. Run a workflow

Create a Workflow and WorkflowRun (see `samples/workflows/`), or use the CLI against the same cluster:

```bash
make build-cli
./bin/ottoflow run <workflow-name>
```

## End-to-end tests (e2e) with kind

The e2e tests expect a kind cluster and will build the manager image, load it into kind, install CRDs, deploy the controller, and verify it runs.

1. **Create a kind cluster** (name must be `kind` unless you set `KIND_CLUSTER`):

   ```bash
   kind create cluster --name kind
   ```

2. **Run the e2e suite**:

   ```bash
   make test-e2e
   ```

The test will:

- Build the manager image (via `make docker-build`, which uses ko with `KO_DOCKER_REPO`; the test uses an image name like `example.com/ottoflow:v0.0.1` and expects it to be loadable into kind)
- Load the image into the kind cluster
- Install CRDs and deploy the controller with that image
- Install the Prometheus operator (for metrics tests) and validate the controller pod

If your kind cluster has a different name, set `KIND_CLUSTER`:

```bash
KIND_CLUSTER=mycluster make test-e2e
```

## Summary

| Goal                    | Command / steps |
|-------------------------|------------------|
| Build binary            | `make build` |
| Unit tests              | `make test` |
| Run controller locally  | `make install` then `make run` (kubeconfig = cluster) |
| Kind: run in cluster    | Create kind cluster → build image (ko) → `kind load` → `make install` → `make deploy IMG=<image>` |
| E2E tests               | Create kind cluster (`kind create cluster --name kind`) → `make test-e2e` |
