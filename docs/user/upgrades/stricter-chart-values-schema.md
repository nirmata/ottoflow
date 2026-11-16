# Upgrading: stricter chart values schema

`values.schema.json` now rejects unknown keys at the top level and under `webhook`, `namespace` and `controller.image`, so a typo or a misnested key fails `helm install` and `helm upgrade` instead of being ignored (for example, `webhook.metrics` belongs under `controller.metrics`). It also type-checks `webhook` values: `failurePolicy` must be `Fail` or `Ignore` and `timeoutSeconds` an integer. Other subtrees, such as the rest of `controller`, still accept unknown keys.

The values `crds.install`, `crds.mode`, `additionalResources`, `namespace.create` and `controller.image.nameOverride` are removed. They never had any effect: Helm installs everything in `crds/` regardless. Setting any of them now fails validation. To skip CRDs, see [Skip CRD Installation](../../../charts/ottoflow/README.md#skip-crd-installation).

`helm upgrade --reuse-values` from an earlier chart version always fails validation: it re-applies the earlier chart's defaults, which include the removed `crds`, `additionalResources` and `controller.image.nameOverride`. A plain `helm upgrade` with no `-f` or `--set` re-applies the release's stored values (what `helm get values` prints), so it also fails if those values contain a removed or misnested key. Upgrade once with an explicit file instead:

```bash
helm get values <release> -n <namespace> -o yaml > my-values.yaml
# delete the stale keys, and any parent key left empty, from my-values.yaml, then:
helm upgrade <release> <chart> -n <namespace> --reset-values -f my-values.yaml
```
