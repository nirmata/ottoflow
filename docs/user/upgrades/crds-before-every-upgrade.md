# Upgrading: apply the CRDs before every `helm upgrade`

Helm installs the CRDs in `crds/` on the first install and never upgrades them. Any release can add CRD fields, and a cluster whose CRDs are stale silently drops them.

Before every `helm upgrade`, apply the CRDs for the version you are upgrading to:

```bash
helm show crds oci://ghcr.io/nirmata/ottoflow --version <the version you are upgrading to> | kubectl apply --server-side --force-conflicts -f -
```

`--force-conflicts` is needed because Helm created the CRDs under its own field manager; without it the apply fails with `conflict with "helm": .spec.versions`.

## Check whether the CRDs are current

```bash
helm show crds oci://ghcr.io/nirmata/ottoflow --version <the version you are upgrading to> | kubectl diff --server-side --force-conflicts -f -
```

Exit 1 means the cluster's CRDs are stale, exit 0 means they are current, and any higher code is an error. As a quick secondary signal, this prints nothing when the CRD predates MCP tool exposure (a non-empty result does not prove the CRDs are current):

```bash
kubectl get crd workflows.ottoflow.nirmata.io -o jsonpath='{.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.mcpTool}'
```

## What a stale CRD drops

For example:

- `kagent`, `mcpTool` and `contextBudgetMode` are dropped without any error.
- `allowInsecureHTTP` is dropped too, so a step with an `http://` `externalAgentRef` URL then fails, and the error says the Workflow or StepTemplate CRD may predate the field.
