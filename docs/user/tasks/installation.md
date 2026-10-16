# Installation

## Prerequisites

- Kubernetes 1.29+
- Helm 3.0+ (for Helm installation)

## Installing the CLI

The `ottoflow` CLI runs workflows locally with no cluster required (`ottoflow run --workflow-dir ...`), or against a cluster (`ottoflow run`, `ottoflow validate`, `ottoflow status`).

**Option A: Homebrew (macOS/Linux, recommended)**

```bash
brew install nirmata/tap/ottoflow
```

**Option B: Prebuilt binary**

Download the archive for your platform (darwin/linux, amd64/arm64) from the
[Releases page](https://github.com/nirmata/ottoflow/releases), verify it against the published
`checksums.txt`, then extract it onto your `PATH`:

```bash
VERSION=v0.1.0-rc1   # replace with the release you want
OS=darwin            # or linux
ARCH=arm64           # or amd64

curl -sSLO "https://github.com/nirmata/ottoflow/releases/download/${VERSION}/ottoflow_${VERSION#v}_${OS}_${ARCH}.tar.gz"
curl -sSLO "https://github.com/nirmata/ottoflow/releases/download/${VERSION}/checksums.txt"

# Linux (GNU coreutils):
sha256sum --ignore-missing -c checksums.txt
# macOS (no sha256sum by default):
grep "${OS}_${ARCH}" checksums.txt | shasum -a 256 -c -

tar -xzf "ottoflow_${VERSION#v}_${OS}_${ARCH}.tar.gz"
sudo mv ottoflow /usr/local/bin/
```

**Option C: Build from source**

```bash
make build-cli          # builds bin/ottoflow
make install-cli        # go install into $GOPATH/bin (or $GOBIN)
make install-cli-local  # copy bin/ottoflow to /usr/local/bin (uses sudo)
```

See [`cli/README.md`](../../../cli/README.md) for CLI usage and flags.

## Installing OttoFlow (in-cluster controller)

**Option A: Via Helm (recommended)**

Use `helm upgrade --install` so installs and upgrades both work:

From OCI registry (recommended, works for public and private repos):

```bash
helm upgrade --install ottoflow oci://ghcr.io/nirmata/ottoflow --version <version> --namespace ottoflow --create-namespace
```

From local chart:

```bash
helm upgrade --install ottoflow ./charts/ottoflow --namespace ottoflow --create-namespace
```

Use `--namespace <name>` to install into a different namespace.

If you see "already exists" errors from a previous partial install, uninstall first:
```bash
helm uninstall ottoflow -n ottoflow
# Then run the install command again
```

**Option B: Via generated manifests**

```bash
make deploy
```

Or manually:
```bash
make generate-manifests
kubectl apply -f config/generated/install.yaml
```

See the [Helm Chart README](../../../charts/ottoflow/README.md) for chart options and the [Configuration Reference](../reference/configuration.md) for all flags and environment variables.

## Upgrading: default run-history cleanup

`Workflow.spec.run` now defaults `retentionMinutes` to `10080` (7 days) and `maxAllowed` to `50` completed runs. Previously, a Workflow that left these fields unset never had its completed WorkflowRuns deleted.

**Once the new CRD is applied, the controller deletes run history beyond 7 days / 50 runs for each Workflow that leaves these fields unset.** Existing Workflows that have no `spec.run`, or `spec.run: {}`, read back the defaults without being rewritten; a Workflow that sets only one of the two fields was not tested. Reconciling a completed run of that Workflow after the CRD is applied deletes, per Workflow, completed runs older than 7 days, then the oldest completed runs beyond 50. It stops early if it deletes the run being reconciled, so full pruning can take several reconciles. Each deleted run's audit-snapshot ConfigMap is garbage-collected with it through owner references. Cleanup runs when a completed run of the Workflow is reconciled, for example when a run completes or on a controller restart.

`helm upgrade` does not update CRDs. Apply the CRDs separately (see [Manual CRD Installation](../../../charts/ottoflow/README.md#manual-crd-installation)). Until you do, the controller treats an unset field as `0` and keeps today's behavior.

**To keep the old behavior,** set `retentionMinutes: 0` (no age limit) and `maxAllowed: 0` (no count limit) explicitly on the Workflow. Setting one of them to `0` disables only that limit. See the [`spec.run` reference](../reference/api/workflow.md).

**Workflows that already set both to `0`.** Before this change, a typed controller write (for example the A2A exposure finalizer on a Workflow with `spec.expose.kagent`) dropped an explicit `0`, so such a Workflow can be stored without the two fields and would get the defaults once the new CRD is applied. Before applying the new CRD, in this order:

1. Find affected Workflows. For Workflows applied with client-side `kubectl apply`, this lists those whose last-applied manifest sets either field to `0` while the stored `spec.run` lacks either field (tested on envtest objects, meaning a local API server, and on synthetic JSON; not on a kind or real cluster):

   ```bash
   kubectl get workflows.ottoflow.nirmata.io -A -o json | jq -r '.items[] | select((.metadata.annotations["kubectl.kubernetes.io/last-applied-configuration"] // "{}" | fromjson | .spec.run // {}) as $l | ($l.retentionMinutes == 0 or $l.maxAllowed == 0) and ((.spec.run.retentionMinutes // null) == null or (.spec.run.maxAllowed // null) == null)) | "\(.metadata.namespace)/\(.metadata.name)"'
   ```

   This cannot see Workflows without the last-applied annotation, such as those applied with `kubectl apply --server-side`. Helm- and Argo CD-applied Workflows were not checked. For those, compare the source manifests that set `retentionMinutes: 0` / `maxAllowed: 0` with the stored `spec.run`:

   ```bash
   kubectl get workflows.ottoflow.nirmata.io -A -o json | jq -r '.items[] | "\(.metadata.namespace)/\(.metadata.name) \(.spec.run // {} | tojson)"'
   ```

2. Re-apply each affected Workflow's manifest, unchanged, while the old CRD is still installed. Re-applying with `kubectl apply -f` and with `kubectl apply --server-side` restored `0` / `0` on envtest and kept it after the new CRD was installed. Re-applying through Helm or Argo CD was not tested.
3. Check that the second command in step 1 shows `{"maxAllowed":0,"retentionMinutes":0}` for each of them. Until the upgrade, do not edit these Workflows (in particular `spec.expose` or `spec.inputs`): the old controller rewrites a Workflow when it adds or removes its finalizer, which drops the `0`s again (from a code read, not tested for `spec.inputs`).
4. Then upgrade the controller first, and the new CRD second. With the old controller and the new CRD, a typed write turns an explicit `0` back into the default.

## Nirmata LLM credentials (optional)

> **Note (provider required):** `Agent.spec.modelProvider` is a required field with no default — every Agent must set it explicitly. Valid values are `nirmata`, `openai`, `anthropic`, `azure-openai`, `google`, `gemini`, and `local`. `nirmata` is served by the **OttoFlow enterprise plugin** and is **not available in the open-source build**; selecting it there returns a clear error asking you to pick a supported provider. The open-source build supports `openai`, `anthropic`, `azure-openai`, `google`/`gemini`, and `local`. The Nirmata LLM credentials below apply only when running with the enterprise plugin.

For **in-cluster** workflow runs that use Nirmata LLM (agent steps), configure credentials securely using a Kubernetes Secret. Create the secret in the same namespace as your WorkflowRuns (or a namespace the runner can reference):

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: nirmata-llm-credentials
  namespace: ottoflow   # or the namespace where you create WorkflowRuns
type: Opaque
stringData:
  NIRMATA_LLM_TOKEN: "<your-token>"   # preferred (single key)
  # Or legacy keys:
  # NIRMATA_LLM_SERVICEACCOUNT_TOKEN: "<your-token>"
  # NIRMATA_LLM_APIKEY: "<your-api-key>"
  # NIRMATA_URL: "https://nirmata.io"   # optional
```

Reference the secret so the workflow-runner pod receives it as environment variables (never put secrets in plain `value`; use `valueFrom.secretKeyRef`).

**Option A – Workflow (recommended for cron-triggered runs)**  

Add `spec.execution` to the **Workflow**. The scheduler copies it into every WorkflowRun it creates (e.g. from cron), so all runs use the secret:

```yaml
# In your Workflow YAML (e.g. cloud-cost-daily)
spec:
  execution:
    job:
      env:
        - name: NIRMATA_LLM_TOKEN
          valueFrom:
            secretKeyRef:
              name: nirmata-llm-credentials
              key: NIRMATA_LLM_TOKEN
        # Or legacy keys: NIRMATA_LLM_SERVICEACCOUNT_TOKEN / NIRMATA_LLM_APIKEY (same secret, different key)
  triggers:
    - cron: ...
  steps: ...
```

Ensure the Secret exists in the **same namespace** as the Workflow (and thus the WorkflowRun).

**Option B – WorkflowRun (for one-off or manually created runs)**  
Add `spec.execution` to the **WorkflowRun** when creating it:

```yaml
spec:
  execution:
    job:
      env:
        - name: NIRMATA_LLM_TOKEN
          valueFrom:
            secretKeyRef:
              name: nirmata-llm-credentials
              key: NIRMATA_LLM_TOKEN
```

For the **CLI** (`ottoflow run`), set the same variable names as environment variables in your shell (e.g. `export NIRMATA_LLM_TOKEN="<your-token>"`). Do not commit tokens to source control.

**Updating the secret to use the new key (`NIRMATA_LLM_TOKEN`)** — use one of these approaches.

1. **Patch the existing secret** (add or replace key in place; replace `NAMESPACE` and token value):

```bash
kubectl patch secret nirmata-llm-credentials -n NAMESPACE -p '{"stringData":{"NIRMATA_LLM_TOKEN":"<your-token>"}}'
```

2. **Replace the secret** (e.g. from a file or literal; base64-encode if using `data`):

```bash
# From literal (stringData avoids base64)
kubectl create secret generic nirmata-llm-credentials \
  --from-literal=NIRMATA_LLM_TOKEN="<your-token>" \
  -n NAMESPACE --dry-run=client -o yaml | kubectl apply -f -
```

3. **Update Workflows/WorkflowRuns** so `spec.execution.job.env` references the new key (see Option A/B above: use `name: NIRMATA_LLM_TOKEN` and `secretKeyRef.key: NIRMATA_LLM_TOKEN`). New runs will then use the new key; no controller or executor restart is required (the executor reads env at runtime in the runner pod).
