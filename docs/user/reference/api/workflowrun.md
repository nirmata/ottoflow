# WorkflowRun

**WorkflowRun** represents an execution instance of a Workflow template. It references a Workflow by name, supplies input values, and tracks execution status and outputs.

- **API Group:** `ottoflow.nirmata.io`
- **Version:** `v1alpha1`
- **Kind:** `WorkflowRun`
- **Scope:** Namespaced
- **Short name:** `florun`

---

## Spec (WorkflowRunSpec)

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `workflowRef` | [WorkflowRef](#workflowref) | Yes | Reference to the Workflow template to execute. |
| `inputValues` | map[string]string | No | Input values for the workflow. Keys match input names in the Workflow spec. |
| `clusterRef` | [ClusterRef](#clusterref) | No | Target cluster configuration for resource operations and CEL `resource.*`. |
| `events` | [EventConfig](#eventconfig) | No | Overrides for event emission for this run (defaults to Workflow spec). |
| `execution` | [WorkflowRunExecutionSpec](#workflowrunexecutionspec) | No | In-cluster runner Job configuration. |

### WorkflowRef

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | Yes | Name of the Workflow template. |
| `namespace` | string | No | Namespace of the Workflow. Defaults to the WorkflowRun namespace. |

### EventConfig

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `enabled` | boolean | No | When nil or true, events are emitted per Level. When false, no events are emitted. |
| `level` | string | No | `Workflow` (workflow-level only) or `WorkflowAndSteps` (workflow + step-level). Default: `WorkflowAndSteps`. |

### ClusterRef

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `local` | boolean | No | Explicitly target the cluster where OttoFlow is executing the run. |
| `kubeConfigSecretRef` | [KubeConfigSecretRef](#kubeconfigsecretref) | No | Reference a Secret containing kubeconfig data. |
| `kubeConfigFilePath` | string | No | Path to a kubeconfig file mounted into the runner pod, including CSI/projected/Secret volume mounts. |

Exactly one cluster source should be set when `clusterRef` is present.

### KubeConfigSecretRef

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | Yes | Secret name containing kubeconfig data. |
| `namespace` | string | No | Secret namespace. Defaults to the WorkflowRun namespace. |
| `key` | string | No | Secret data key. When omitted, OttoFlow tries `config`, `kubeconfig`, then `value`. |

### WorkflowRunExecutionSpec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `job` | [WorkflowRunJobSpec](#workflowrunjobspec) | No | Job and pod settings for the workflow-runner pod. |
| `checkpointing` | object | No | Per-step checkpointing for crash recovery: `enabled` (boolean) and `maxRestartAttempts` (integer). |
| `llmCredentialsSecret` | object | No | Overrides the cluster-wide well-known Secret for LLM credentials injected into the runner pod. |

### WorkflowRunJobSpec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `image` | string | No | Override the runner image. |
| `serviceAccountName` | string | No | Service account for the runner Job. Requires authorization — see [Running as another ServiceAccount](#running-as-another-serviceaccount). |
| `env` | []EnvVar | No | Extra environment variables for the runner container. For credentials (e.g. Nirmata LLM), use `valueFrom.secretKeyRef` to reference a Secret; do not use plain `value`. |
| `resources` | ResourceRequirements | No | Container resource requests and limits. |
| `volumes` | []Volume | No | Extra pod volumes, including CSI/projected/Secret volumes. |
| `volumeMounts` | []VolumeMount | No | Container volume mounts for the runner container. |
| `nodeSelector` | map[string]string | No | Node selector for runner pod scheduling. |
| `tolerations` | []Toleration | No | Tolerations for runner pod scheduling. |
| `affinity` | Affinity | No | Affinity/anti-affinity rules for the runner pod. |
| `backoffLimit` | integer | No | Job retry limit. Defaults to `0` when omitted. |
| `ttlSecondsAfterFinished` | integer | No | Job TTL after completion. |
| `activeDeadlineSeconds` | integer | No | Maximum runtime for the runner Job. |

---

## Status (WorkflowRunStatus)

| Field | Type | Description |
|-------|------|-------------|
| `phase` | string | One of: `Pending`, `Running`, `Succeeded`, `Failed`. |
| `message` | string | Additional information about the workflow status. |
| `startTime` | string (date-time) | When the workflow execution started. |
| `completionTime` | string (date-time) | When the workflow execution completed. |
| `outputs` | object | Workflow-level outputs (from Workflow spec), evaluated at completion. |
| `stepStatuses` | map[string][StepStatus](#stepstatus) | Execution status of each step. |
| `trigger` | [Trigger](#trigger) | What triggered this WorkflowRun (Manual, Cron, Event, Webhook). |
| `restartRequired` | boolean | Legacy restart indicator from earlier inline execution; not used by the Job-based runner path. |
| `execution` | [WorkflowRunExecutionStatus](#workflowrunexecutionstatus) | Runner Job execution metadata. |

### StepStatus

| Field | Type | Description |
|-------|------|-------------|
| `phase` | string | One of: `Pending`, `Running`, `Succeeded`, `Failed`, `Skipped`. |
| `message` | string | Additional information about the step status. |
| `error` | string | Error information if the step failed. |
| `startTime` | string (date-time) | When the step started. |
| `completionTime` | string (date-time) | When the step completed. |
| `retryCount` | integer | Number of retry attempts (0 = initial, 1+ = retries). |
| `lastRetryTime` | string (date-time) | Timestamp of last retry. |
| `nextRetryTime` | string (date-time) | When the next retry will be attempted (if retrying). |

### Trigger

| Field | Type | Description |
|-------|------|-------------|
| `type` | string | One of: `Manual`, `Cron`, `Event`, `Webhook`. |
| `triggeredAt` | string (date-time) | When the trigger fired. |
| `cronSchedule` | string | Cron schedule if triggered by cron. |
| `eventResource` | object | Resource that triggered the run (apiVersion, kind, name, namespace). |
| `webhookRequest` | object | Metadata about the HTTP request that triggered the run (for webhook triggers). |

### WorkflowRunExecutionStatus

| Field | Type | Description |
|-------|------|-------------|
| `phase` | string | Runner workload phase. |
| `jobName` | string | Name of the workflow-runner Job. |
| `podName` | string | Name of the active or last-known runner pod. |
| `message` | string | Additional runner status information. |
| `startTime` | string (date-time) | When runner Job execution started. |
| `completionTime` | string (date-time) | When runner Job execution completed. |

## Running as another ServiceAccount

The runner Job is launched with the token of the ServiceAccount it runs as, so
`spec.execution.job.serviceAccountName` hands the workflow that account's
permissions. Admission therefore checks that whoever submitted the WorkflowRun
is allowed to run as it, with a `SubjectAccessReview` for `use` on that
ServiceAccount. A run naming an account the submitter is not allowed to use is
rejected:

```text
WorkflowRun "nightly": "system:serviceaccount:team-a:submitter" may not use
serviceaccount "privileged-sa" in namespace "team-a"
```

Grant it by naming the accounts a subject may borrow:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: use-build-runner
  namespace: team-a
rules:
  - apiGroups: [""]
    resources: ["serviceaccounts"]
    resourceNames: ["build-runner"]
    verbs: ["use"]
```

then bind it to the users or ServiceAccounts that submit those runs.

A Workflow can declare the account instead, under `spec.execution.job`, and the
same check runs when the Workflow is admitted — that is where the account is
chosen. Runs created from it, including cron runs the scheduler creates, inherit
the account and are not reviewed again.

Runs that set no `serviceAccountName` are unaffected: they get the dedicated
least-privilege runner account, and no review is issued.
