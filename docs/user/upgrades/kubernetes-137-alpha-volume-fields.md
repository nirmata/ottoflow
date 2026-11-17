# Upgrading: Kubernetes 1.37 alpha volume fields

**Once the new CRDs are applied, the Workflow and WorkflowRun CRDs accept four Kubernetes 1.37 alpha fields under `spec.execution.job`. A field takes effect only on a cluster that runs Kubernetes 1.37 or later with its feature gate enabled; the gates are off by default.**

`helm upgrade` does not update CRDs. Apply the CRDs separately (see [Skip CRD Installation](../../../charts/ottoflow/README.md#skip-crd-installation)). Until you do, the fields are not accepted: an earlier release rejected them as unknown fields under a strict `kubectl apply`.

| Field (under `spec.execution.job`) | Kubernetes feature gate |
|---|---|
| `volumeMounts[].bindMountOptions` | `VolumeBindMountOptions` |
| `volumes[].{configMap,secret,downwardAPI,projected}.defaultUser` | `AtomicWriteVolumeUserFields` |
| `user` on every `items[]` entry, and `volumes[].projected.sources[].{clusterTrustBundle,podCertificate,serviceAccountToken}.user` | `AtomicWriteVolumeUserFields` |
| `volumes[].emptyDir.mode` | `EmptyDirVolumeMode` |

The runner Job copies these fields into its pod. On a Kubernetes 1.36 API server, a Job that carries them is created with no error or warning, and the fields are gone from the stored Job. On 1.37 the Kubernetes source clears a field whose gate is off in the same way; this was not measured here.

`bindMountOptions` (`noexec`, `nodev`, `nosuid`) and `emptyDir.mode` are hardening settings: setting one on a cluster where it is dropped leaves the volume unrestricted. Before you depend on any of these fields, check that it takes effect on your cluster.
