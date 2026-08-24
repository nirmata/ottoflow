#!/usr/bin/env bash
# Regenerate images/demo.gif from images/demo.tape via VHS.
# Usage: make demo   (or: ./hack/gen-demo-gif.sh)
#
# Prerequisites (this script does NOT create them):
#   - vhs                 brew install vhs   (pulls in ttyd + ffmpeg)
#   - a throwaway cluster with an empty default namespace, e.g.
#       kind create cluster --name ottoflow-demo
#     (a clean default ns matters: pod-triage scans ALL pods in default, so
#      unrelated failing pods would skew the demo)
#   - a llama.cpp-compatible server reachable, with LLAMACPP_HOST exported, e.g.
#       export LLAMACPP_HOST=http://127.0.0.1:11434/
#
# It seeds samples/fixtures/failing-pods.yaml (a crash-looping OOMKilled pod, an
# ImagePullBackOff pod, and a healthy one), waits for the crash-looper to
# accumulate restarts, records the GIF, then removes the fixture.
set -euo pipefail
cd "$(dirname "$0")/.."

command -v vhs >/dev/null 2>&1 || {
  echo "error: vhs is not installed. Install it with: brew install vhs" >&2
  exit 1
}

echo "Seeding demo failure pods into namespace 'default'..."
kubectl apply -f samples/fixtures/failing-pods.yaml
trap 'kubectl delete -f samples/fixtures/failing-pods.yaml --ignore-not-found >/dev/null 2>&1 || true' EXIT

echo "Waiting for 'crashy' to accumulate OOMKilled restarts (>3)..."
for _ in $(seq 1 60); do
  rc=$(kubectl get pod crashy -n default \
        -o jsonpath='{.status.containerStatuses[0].restartCount}' 2>/dev/null || echo 0)
  [ "${rc:-0}" -gt 3 ] && break
  sleep 5
done

vhs images/demo.tape
