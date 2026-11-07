# Upgrading: default run-history cleanup

`Workflow.spec.run` now defaults `retentionMinutes` to `10080` (7 days) and `maxAllowed` to `50` completed runs. Previously, a Workflow that left these fields unset never had its completed WorkflowRuns deleted.

**Once the new CRD is applied, the controller deletes run history beyond 7 days / 50 runs for each Workflow that leaves these fields unset.** Existing Workflows that have no `spec.run`, or `spec.run: {}`, read back the defaults without being rewritten; a Workflow that sets only one of the two fields gets the default for the other. Reconciling a completed run of that Workflow after the CRD is applied deletes, per Workflow, completed runs older than 7 days, then the oldest completed runs beyond 50. It stops early if it deletes the run being reconciled, so full pruning can take several reconciles. Each deleted run's audit-snapshot ConfigMap is garbage-collected with it through owner references. Cleanup runs when a completed run of the Workflow is reconciled, for example when a run completes, on a controller restart, or on the controller's periodic resync (about every 10 hours).

`helm upgrade` does not update CRDs. Apply the CRDs separately (see [Skip CRD Installation](../../../charts/ottoflow/README.md#skip-crd-installation)). Until you do, the controller treats an unset field as `0` and keeps today's behavior.

**To keep the old behavior,** set `retentionMinutes: 0` (no age limit) and `maxAllowed: 0` (no count limit) explicitly on the Workflow. Setting one of them to `0` disables only that limit. See the [`spec.run` reference](../reference/api/workflow.md).

**Workflows that already set both to `0`.** Before this change, a typed controller write (for example the A2A exposure finalizer on a Workflow with `spec.expose.kagent`) dropped an explicit `0`, so such a Workflow can be stored without the two fields and would get the defaults once the new CRD is applied. Before applying the new CRD, in this order:

1. Find affected Workflows. For Workflows applied with client-side `kubectl apply`, this lists those whose last-applied manifest sets either field to `0` while the stored `spec.run` lacks either field:

   ```bash
   kubectl get workflows.ottoflow.nirmata.io -A -o json | jq -r '.items[] | select((.metadata.annotations["kubectl.kubernetes.io/last-applied-configuration"] // "{}" | fromjson | .spec.run // {}) as $l | ($l.retentionMinutes == 0 or $l.maxAllowed == 0) and ((.spec.run.retentionMinutes // null) == null or (.spec.run.maxAllowed // null) == null)) | "\(.metadata.namespace)/\(.metadata.name)"'
   ```

   This cannot see Workflows without the last-applied annotation, such as those applied with `kubectl apply --server-side`. For those, and for Helm- and Argo CD-applied Workflows, compare the source manifests that set `retentionMinutes: 0` / `maxAllowed: 0` with the stored `spec.run`:

   ```bash
   kubectl get workflows.ottoflow.nirmata.io -A -o json | jq -r '.items[] | "\(.metadata.namespace)/\(.metadata.name) \(.spec.run // {} | tojson)"'
   ```

2. Re-apply each affected Workflow's manifest, unchanged, while the old CRD is still installed.
3. Check that the second command in step 1 shows `{"maxAllowed":0,"retentionMinutes":0}` for each of them. Until the upgrade, do not edit these Workflows (in particular `spec.expose` or `spec.inputs`): the old controller rewrites a Workflow when it adds or removes its finalizer, which drops the `0`s again.
4. Then upgrade the controller first, and the new CRD second. With the old controller and the new CRD, a typed write turns an explicit `0` back into the default.
