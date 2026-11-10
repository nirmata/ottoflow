# Upgrading: runner ServiceAccount label

The controller now binds an existing runner ServiceAccount only when it carries the label `app.kubernetes.io/part-of: ottoflow`. This covers the derived `<workflow>-runner` account and the shared `workflowRunner.serviceAccountName` account. Accounts the controller created itself already carry the label, so a default install needs no change. An explicit `spec.execution.job.serviceAccountName` is not affected: the controller binds nothing to it.

An unlabelled derived or shared account now fails the run with a `RunnerServiceAccountNotManaged` Warning event, and the controller removes the runner, agent-executor caller and ConfigMap-writer bindings it made for that account earlier, which also breaks runs already in progress on it. On a run whose runner Job already exists, the controller records the same event and creates no ConfigMap-writer RoleBinding, but does not fail the run.

The controller also sweeps its runner and agent-executor caller ClusterRoleBindings when a replica becomes leader and every 10 minutes after. It removes the bindings of a ServiceAccount that no longer exists, and of an unlabelled ServiceAccount that no live run uses.

## Before upgrading

Label every runner account you created yourself, in every namespace where runs use it:

```bash
kubectl label serviceaccount <account> -n <namespace> app.kubernetes.io/part-of=ottoflow --overwrite
```

- **Shared account:** if you set `workflowRunner.serviceAccountName`, label that account in every tenant namespace. Use a dedicated name, never `default`.
- **Controller account as the runner account:** if you set `workflowRunner.serviceAccountName` to the controller's ServiceAccount with `controller.serviceAccount.create=false`, label your own controller ServiceAccount the same way. The chart labels the controller ServiceAccount it creates.
- **Changing the runner ServiceAccount setting:** a run started on an unlabelled account before the change can lose its bindings to the sweep while it is still running. Let those runs finish first, or add the label to the old account and remove it after they end.

Every account a run has already used is the subject of one of the controller's runner ClusterRoleBindings. This lists those accounts, in every namespace, that lack the label:

```bash
kubectl get clusterrolebinding -l 'app.kubernetes.io/part-of=ottoflow,!app.kubernetes.io/component' \
  -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.subjects[0].namespace}{" "}{.subjects[0].name}{"\n"}{end}' \
  | awk '$1 ~ /^ottoflow-runner-/ {print $2, $3}' | sort -u \
  | while read -r ns sa; do
      label=$(kubectl get serviceaccount "$sa" -n "$ns" -o jsonpath='{.metadata.labels.app\.kubernetes\.io/part-of}' 2>/dev/null) || continue
      [ "$label" = ottoflow ] || echo "$ns/$sa"
    done
```

To also find runner accounts in a namespace that no run has used yet:

```bash
kubectl get serviceaccounts -n <namespace> -l 'app.kubernetes.io/part-of!=ottoflow' -o name | grep -- '-runner$'
```

## If you already upgraded

A run on an unlabelled account fails with a `RunnerServiceAccountNotManaged` Warning event on its WorkflowRun:

```bash
kubectl get events -A --field-selector reason=RunnerServiceAccountNotManaged
```

Label the account as above, then start a new run. The failed run stays failed.

See [Runner ServiceAccount label](../../../charts/ottoflow/README.md#runner-serviceaccount-label) in the chart README.
