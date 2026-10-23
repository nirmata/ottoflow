#!/usr/bin/env bash
# Run sample workflows against a local kind cluster.
# Creates kind cluster if missing, installs CRDs, builds CLI, then runs workflows.
# For prometheusQuery steps to succeed, set PROMETHEUS_URL or pass --prometheus-url.

set -e

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"
KIND_CLUSTER="${KIND_CLUSTER:-kind}"
CLI_BIN="$ROOT_DIR/bin/ottoflow"
WORKFLOW_DIR="$ROOT_DIR/samples/workflows"

echo "=== OttoFlow samples on Kind ==="
echo "Root: $ROOT_DIR"
echo "Kind cluster: $KIND_CLUSTER"
echo ""

# Ensure kind cluster exists
if ! kind get clusters 2>/dev/null | grep -q "^${KIND_CLUSTER}$"; then
  echo "Creating kind cluster: $KIND_CLUSTER"
  kind create cluster --name "$KIND_CLUSTER"
else
  echo "Kind cluster '$KIND_CLUSTER' already exists"
fi

# Use kind context
export KUBECONFIG="${KUBECONFIG:-$HOME/.kube/config}"
kubectl config use-context "kind-${KIND_CLUSTER}"

# Install CRDs
echo ""
echo "Installing CRDs..."
make install

# Build CLI
echo ""
echo "Building CLI..."
make build-cli

if [ ! -f "$CLI_BIN" ]; then
  echo "CLI binary not found at $CLI_BIN"
  exit 1
fi

echo ""
echo "Running workflows (workflow-dir=$WORKFLOW_DIR)"
echo ""

# 1. Basic workflow (no Prometheus)
echo "--- 1. basic-test ---"
"$CLI_BIN" run basic-test --workflow-dir "$WORKFLOW_DIR" --input message="Kind" --input number="1" -o table
echo ""

# 2. Resource-query (needs default/test-pod with label app=nginx; seeded idempotently)
echo "--- 2. resource-query-example ---"
kubectl run test-pod --image=nginx --labels=app=nginx --dry-run=client -o yaml | kubectl apply -n default -f -
kubectl wait -n default --for=condition=Ready pod/test-pod --timeout=120s
"$CLI_BIN" run resource-query-example --workflow-dir "$WORKFLOW_DIR" -o table
echo ""

# 3. Prometheus-query-example (the CLI rejects it up front without --prometheus-url)
echo "--- 3. prometheus-query-example ---"
if [ -n "$PROMETHEUS_URL" ]; then
  "$CLI_BIN" run prometheus-query-example --workflow-dir "$WORKFLOW_DIR" --input namespace=default --prometheus-url "$PROMETHEUS_URL" -o table
else
  echo "SKIP: PROMETHEUS_URL not set"
fi
echo ""

# 4. Prometheus-query-simple (single prometheusQuery step)
echo "--- 4. prometheus-query-simple ---"
if [ -n "$PROMETHEUS_URL" ]; then
  "$CLI_BIN" run prometheus-query-simple --workflow-dir "$WORKFLOW_DIR" --prometheus-url "$PROMETHEUS_URL" -o table
else
  echo "SKIP: PROMETHEUS_URL not set"
fi

echo ""
echo "Done. Set PROMETHEUS_URL to run the prometheusQuery samples."
