/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package v1alpha1

import (
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// WorkflowRunSpec defines the desired state of WorkflowRun
type WorkflowRunSpec struct {
	// WorkflowRef references the Workflow template to execute
	// +kubebuilder:validation:Required
	WorkflowRef WorkflowRef `json:"workflowRef"`

	// InputValues provides input values for the referenced workflow template
	// Keys match input names defined in the Workflow template
	// +optional
	InputValues map[string]string `json:"inputValues,omitempty"`

	// ClusterRef optionally targets a cluster for this run. When set, resource queries,
	// mutate steps, and CEL resource.* use that cluster. When omitted or local, use the
	// cluster where the controller runs (in-cluster client). This enables multi-cluster
	// workflows by providing a KubeConfig secret as input to the run.
	// +optional
	ClusterRef *ClusterRef `json:"clusterRef,omitempty"`

	// Events overrides event emission for this run (defaults to Workflow spec).
	// +optional
	Events *EventConfig `json:"events,omitempty"`

	// Execution configures how the WorkflowRun executes in-cluster.
	// +optional
	Execution *WorkflowRunExecutionSpec `json:"execution,omitempty"`
}

// ClusterRef indicates which cluster to use for workflow execution.
// Exactly one cluster source should be set when ClusterRef is present.
type ClusterRef struct {
	// Local, when true, uses the in-cluster configuration (the cluster where the controller runs).
	// Use this to explicitly target the local (controller) cluster when ClusterRef is set.
	// +optional
	Local *bool `json:"local,omitempty"`

	// KubeConfigSecretRef references a Secret containing a kubeconfig file. The workflow
	// runs against the cluster defined in that kubeconfig. Secret data key defaults to
	// "config", "kubeconfig", or "value" if Key is empty. Namespace defaults to the
	// WorkflowRun namespace if empty.
	// +optional
	KubeConfigSecretRef *KubeConfigSecretRef `json:"kubeConfigSecretRef,omitempty"`

	// KubeConfigFilePath points to a kubeconfig file mounted into the runner pod.
	// This is intended for Secret, projected, or CSI volume mounts.
	// +optional
	KubeConfigFilePath string `json:"kubeConfigFilePath,omitempty"`
}

// KubeConfigSecretRef references a Secret that holds a kubeconfig file.
type KubeConfigSecretRef struct {
	// Name is the name of the Secret.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Namespace is the namespace of the Secret. Defaults to the WorkflowRun namespace if empty.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Key is the key in Secret.Data containing the kubeconfig (e.g. "config", "kubeconfig", "value").
	// If empty, the helper tries "config", "kubeconfig", then "value".
	// +optional
	Key string `json:"key,omitempty"`
}

// WorkflowRunExecutionSpec configures the runner Job used for in-cluster execution.
type WorkflowRunExecutionSpec struct {
	// Job configures the runner Job used to execute this WorkflowRun.
	// +optional
	Job *WorkflowRunJobSpec `json:"job,omitempty"`

	// Checkpointing configures per-step checkpointing for crash recovery.
	// When enabled, the executor writes a ConfigMap checkpoint after each successful step.
	// The controller retries transient pod failures (eviction, node drain) up to
	// MaxRestartAttempts times; deterministic failures (OOMKilled) never retry.
	// +optional
	Checkpointing *CheckpointingConfig `json:"checkpointing,omitempty"`

	// LLMCredentialsSecret overrides the cluster-wide well-known Secret for LLM credentials.
	// When set, the controller injects env vars from this Secret into the runner Job instead of
	// the cluster-wide default configured via --workflow-runner-llm-credentials-secret.
	// Explicit spec.execution.job.env entries always take precedence over injected values.
	// +optional
	LLMCredentialsSecret *LLMCredentialsSecretRef `json:"llmCredentialsSecret,omitempty"`
}

// LLMCredentialsSecretRef identifies a Secret containing LLM credentials to inject into
// the runner Job. Only keys present in the LLM env allowlist are injected.
// The controller reads the Secret directly; the runner Job never requires Secret RBAC.
type LLMCredentialsSecretRef struct {
	// Name is the Secret name.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Namespace is the Secret namespace. Must be empty or match the WorkflowRun's namespace.
	// Cross-namespace references are rejected at admission because SecretKeyRef in the runner
	// pod spec is namespace-scoped and cannot reference Secrets in other namespaces.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// CheckpointingConfig configures per-step checkpointing for crash recovery.
//
// Known limitations:
//   - Checkpoint data is stored in a ConfigMap (plain-text, not encrypted). Avoid enabling
//     on workflows that handle secrets or PII in step outputs until Secret storage is added.
//   - ForEach steps are NOT checkpointed at the inner-item level. If a pod crashes
//     mid-ForEach, all inner items replay from the beginning on resume. Enabling
//     checkpointing on workflows with ForEach steps is safe but provides no partial-progress
//     guarantee inside the ForEach. A Warning is logged when this combination is detected.
type CheckpointingConfig struct {
	// Enabled turns on per-step checkpointing. After each step completes, the executor
	// writes a checkpoint ConfigMap so the run can resume after a transient pod crash.
	// Default: false — preserves existing behavior for all existing workflows.
	Enabled bool `json:"enabled,omitempty"`

	// MaxRestartAttempts is the maximum number of times the controller will create a new
	// runner Job after a transient pod failure. Deterministic failures (OOMKilled) never
	// retry regardless of this value.
	// Default: 3.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=20
	MaxRestartAttempts *int32 `json:"maxRestartAttempts,omitempty"`
}

// WorkflowRunJobSpec configures the runner Job pod.
type WorkflowRunJobSpec struct {
	// Image overrides the default workflow-runner image.
	// +optional
	Image string `json:"image,omitempty"`

	// ServiceAccountName overrides the default service account for the runner Job.
	// +optional
	ServiceAccountName string `json:"serviceAccountName,omitempty"`

	// Env provides additional environment variables for the runner container.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// Resources configures requests/limits for the runner container.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// Volumes defines additional pod volumes for the runner Job.
	// +optional
	Volumes []corev1.Volume `json:"volumes,omitempty"`

	// VolumeMounts defines additional container volume mounts for the runner Job.
	// +optional
	VolumeMounts []corev1.VolumeMount `json:"volumeMounts,omitempty"`

	// NodeSelector constrains runner Job scheduling to nodes with matching labels.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Tolerations configures pod tolerations for the runner Job.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// Affinity configures pod affinity/anti-affinity for the runner Job.
	// +optional
	Affinity *corev1.Affinity `json:"affinity,omitempty"`

	// BackoffLimit is the number of retries before the Job is considered failed.
	// Defaults to 0 when omitted.
	// +optional
	BackoffLimit *int32 `json:"backoffLimit,omitempty"`

	// TTLSecondsAfterFinished controls automatic Job cleanup after completion.
	// +optional
	TTLSecondsAfterFinished *int32 `json:"ttlSecondsAfterFinished,omitempty"`

	// ActiveDeadlineSeconds limits the total runtime of the runner Job.
	// +optional
	ActiveDeadlineSeconds *int64 `json:"activeDeadlineSeconds,omitempty"`
}

// Validate checks that the job spec fields are valid. Returns an error listing
// all validation failures.
func (s *WorkflowRunJobSpec) Validate() error {
	var errs []error
	if s == nil {
		return nil
	}
	if strings.TrimSpace(s.Image) != "" && s.Image != strings.TrimSpace(s.Image) {
		errs = append(errs, fmt.Errorf("execution.job.image must not have leading/trailing whitespace"))
	}
	if s.ServiceAccountName != "" {
		if msgs := validation.IsDNS1123Subdomain(s.ServiceAccountName); len(msgs) > 0 {
			errs = append(errs, fmt.Errorf("execution.job.serviceAccountName: %s", strings.Join(msgs, "; ")))
		}
	}
	if s.BackoffLimit != nil && *s.BackoffLimit < 0 {
		errs = append(errs, fmt.Errorf("execution.job.backoffLimit must be >= 0, got %d", *s.BackoffLimit))
	}
	if s.TTLSecondsAfterFinished != nil && *s.TTLSecondsAfterFinished < 0 {
		errs = append(errs, fmt.Errorf("execution.job.ttlSecondsAfterFinished must be >= 0, got %d", *s.TTLSecondsAfterFinished))
	}
	if s.ActiveDeadlineSeconds != nil && *s.ActiveDeadlineSeconds < 0 {
		errs = append(errs, fmt.Errorf("execution.job.activeDeadlineSeconds must be >= 0, got %d", *s.ActiveDeadlineSeconds))
	}
	volumeNames := make(map[string]struct{})
	for i, v := range s.Volumes {
		if v.Name == "" {
			errs = append(errs, fmt.Errorf("execution.job.volumes[%d].name is required", i))
		} else {
			if msgs := validation.IsDNS1123Label(v.Name); len(msgs) > 0 {
				errs = append(errs, fmt.Errorf("execution.job.volumes[%d].name: %s", i, strings.Join(msgs, "; ")))
			}
			if _, ok := volumeNames[v.Name]; ok {
				errs = append(errs, fmt.Errorf("execution.job.volumes: duplicate volume name %q", v.Name))
			}
			volumeNames[v.Name] = struct{}{}
		}
	}
	for i, m := range s.VolumeMounts {
		if m.Name == "" {
			errs = append(errs, fmt.Errorf("execution.job.volumeMounts[%d].name is required", i))
		} else if _, ok := volumeNames[m.Name]; !ok {
			errs = append(errs, fmt.Errorf("execution.job.volumeMounts[%d].name %q must refer to a volume in execution.job.volumes", i, m.Name))
		}
		if m.MountPath == "" {
			errs = append(errs, fmt.Errorf("execution.job.volumeMounts[%d].mountPath is required", i))
		}
	}
	for i, e := range s.Env {
		if e.Name == "" {
			errs = append(errs, fmt.Errorf("execution.job.env[%d].name is required", i))
		} else if msgs := validation.IsCIdentifier(e.Name); len(msgs) > 0 {
			errs = append(errs, fmt.Errorf("execution.job.env[%d].name: %s", i, strings.Join(msgs, "; ")))
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return errors.Join(errs...)
}

// WorkflowRef references a Workflow template
type WorkflowRef struct {
	// Name is the name of the Workflow template
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Namespace is the namespace of the Workflow template.
	// Defaults to the WorkflowRun namespace if not specified.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// WorkflowRunStatus defines the observed state of WorkflowRun
type WorkflowRunStatus struct {
	// Phase represents the current phase of the WorkflowRun
	// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed
	// +optional
	Phase WorkflowRunPhase `json:"phase,omitempty"`

	// StepStatuses tracks the execution status of each step
	// +optional
	StepStatuses map[string]StepStatus `json:"stepStatuses,omitempty"`

	// Outputs contains workflow-level outputs evaluated at completion
	// These are defined in the Workflow spec and evaluated using the final context
	// +kubebuilder:validation:Type=object
	// +kubebuilder:pruning:PreserveUnknownFields
	// +optional
	Outputs map[string]apiextensionsv1.JSON `json:"outputs,omitempty"`

	// StartTime is when the workflow execution started
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// CompletionTime is when the workflow execution completed
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// Message provides additional information about the workflow status
	// +optional
	Message string `json:"message,omitempty"`

	// FailureReason classifies why a Failed WorkflowRun failed, for the subset of causes that
	// have a well-known classification below. Empty means EITHER the run has not failed OR it
	// failed for a cause that is not one of the classified reasons — absence is not itself a
	// claim that nothing went wrong, so consumers must treat an unrecognised value the same as
	// an empty one, not as an error.
	// +kubebuilder:validation:Enum=SecretAccessDenied;RunnerRefUnresolved
	// +optional
	FailureReason WorkflowRunFailureReason `json:"failureReason,omitempty"`

	// RestartRequired indicates that the workflow needs to be restarted.
	// Deprecated: The executor guard that consumed this field has been superseded by
	// per-step checkpointing (see WorkflowRunExecutionSpec.Checkpointing). This field
	// is no longer written by any component and will be removed in a future API version.
	// +optional
	RestartRequired bool `json:"restartRequired,omitempty"`

	// Trigger contains information about what triggered this WorkflowRun
	// +optional
	Trigger *TriggerInfo `json:"trigger,omitempty"`

	// Execution contains status for the runner Job that executes this WorkflowRun.
	// +optional
	Execution *WorkflowRunExecutionStatus `json:"execution,omitempty"`

	// PendingCallback holds the state of an in-progress waitForCallback step.
	// Nil when no callback is pending.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	PendingCallback *CallbackState `json:"pendingCallback,omitempty"`

	// AuditSnapshotConfigMap is the name of the ConfigMap (in this WorkflowRun's
	// namespace) holding a snapshot of the final execution context — the variables
	// and expression outputs the run had computed at its terminal phase (Succeeded
	// or Failed). Unlike the opt-in per-step checkpoint (see
	// WorkflowRunExecutionSpec.Checkpointing), this snapshot is always written once
	// at completion, regardless of whether checkpointing is enabled, so that what a
	// run actually saw/computed can be inspected after the fact. The ConfigMap is
	// owned by this WorkflowRun and is garbage-collected together with it (see
	// Workflow.spec.retentionMinutes/maxAllowed); it is not deleted separately.
	// +optional
	AuditSnapshotConfigMap string `json:"auditSnapshotConfigMap,omitempty"`

	// AuditSnapshotError is set when writing AuditSnapshotConfigMap failed at
	// terminal phase (e.g. the snapshot could not be persisted within the retry
	// window, or a validation error was hit). Empty when the write succeeded or
	// hasn't been attempted yet. Surfaced here (and as a Warning event) so the
	// absence of AuditSnapshotConfigMap is distinguishable from "nothing went
	// wrong" rather than silently looking identical to pre-audit-snapshot behavior.
	// +optional
	AuditSnapshotError string `json:"auditSnapshotError,omitempty"`
}

// WorkflowRunExecutionStatus reports the status of the runner Job.
type WorkflowRunExecutionStatus struct {
	// Phase is the current execution phase for the runner workload.
	// +optional
	Phase string `json:"phase,omitempty"`

	// JobName is the name of the runner Job for this WorkflowRun.
	// +optional
	JobName string `json:"jobName,omitempty"`

	// PodName is the active or last-known runner pod name for this WorkflowRun.
	// +optional
	PodName string `json:"podName,omitempty"`

	// Message provides additional runner Job status information.
	// +optional
	Message string `json:"message,omitempty"`

	// StartTime is when runner Job execution started.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// CompletionTime is when runner Job execution completed.
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// Attempts counts controller-triggered retries; 0 on the initial execution.
	// Incremented only when a transient pod failure causes the controller to spawn a
	// replacement runner Job. Use Attempts+1 to get the total number of Job spawns.
	// +optional
	Attempts int32 `json:"attempts,omitempty"`

	// LastTerminationReason records the container termination reason from the most
	// recent failed pod (e.g. OOMKilled, Evicted). Used by the controller to decide
	// whether to retry.
	// +optional
	LastTerminationReason string `json:"lastTerminationReason,omitempty"`
}

// TriggerInfo contains information about what triggered a WorkflowRun
type TriggerInfo struct {
	// Type is the type of trigger (Manual, Cron, Event, Webhook)
	// +kubebuilder:validation:Enum=Manual;Cron;Event;Webhook
	Type string `json:"type"`

	// CronSchedule is the cron schedule if triggered by cron
	// +optional
	CronSchedule string `json:"cronSchedule,omitempty"`

	// TriggeredAt is when the trigger fired
	// +optional
	TriggeredAt metav1.Time `json:"triggeredAt,omitempty"`

	// EventResource contains information about the event that triggered this WorkflowRun
	// +optional
	EventResource *EventResourceInfo `json:"eventResource,omitempty"`

	// WebhookRequest contains metadata about the HTTP request that triggered this WorkflowRun
	// +optional
	WebhookRequest *WebhookRequestInfo `json:"webhookRequest,omitempty"`
}

// WebhookRequestInfo records metadata about the HTTP request that triggered the run.
type WebhookRequestInfo struct {
	// RemoteAddr is the caller's IP address (best-effort; may be proxy IP).
	// +optional
	RemoteAddr string `json:"remoteAddr,omitempty"`

	// RequestID is a unique ID generated per request for tracing.
	// +optional
	RequestID string `json:"requestId,omitempty"`
}

// EventResourceInfo contains information about the Kubernetes resource that triggered an event
type EventResourceInfo struct {
	// APIVersion is the API version of the resource
	APIVersion string `json:"apiVersion"`

	// Kind is the kind of the resource
	Kind string `json:"kind"`

	// Name is the name of the resource
	Name string `json:"name"`

	// Namespace is the namespace of the resource (empty for cluster-scoped)
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// WorkflowRunPhase represents the phase of a WorkflowRun
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed
type WorkflowRunPhase string

const (
	// WorkflowRunPhasePending indicates the workflow is pending execution
	WorkflowRunPhasePending WorkflowRunPhase = "Pending"

	// WorkflowRunPhaseRunning indicates the workflow is currently running
	WorkflowRunPhaseRunning WorkflowRunPhase = "Running"

	// WorkflowRunPhaseSucceeded indicates the workflow completed successfully
	WorkflowRunPhaseSucceeded WorkflowRunPhase = "Succeeded"

	// WorkflowRunPhaseFailed indicates the workflow failed
	WorkflowRunPhaseFailed WorkflowRunPhase = "Failed"
)

// WorkflowRunFailureReason classifies a subset of well-known WorkflowRun failure causes, for
// programmatic consumers (alerting, dashboards) that need to distinguish these specific causes
// from every other failure without parsing Status.Message. Not every Failed run gets one — see
// WorkflowRunStatus.FailureReason's doc comment.
// +kubebuilder:validation:Enum=SecretAccessDenied;RunnerRefUnresolved
type WorkflowRunFailureReason string

const (
	// WorkflowRunFailureReasonSecretAccessDenied indicates a Secret operation the controller
	// ServiceAccount had to perform returned Forbidden. This is usually a read (e.g. the
	// well-known LLM credentials Secret, or a spec.execution.job.volumes Secret in the runner
	// namespace needing `get`), but also covers a denied `create`: copying a Secret referenced
	// from another namespace into the runner namespace needs `create` there, which a Role
	// granting only `get` does not satisfy. See docs/user/rbac-secret-access.md for the Role
	// to grant in either case.
	WorkflowRunFailureReasonSecretAccessDenied WorkflowRunFailureReason = "SecretAccessDenied"

	// WorkflowRunFailureReasonRunnerRefUnresolved indicates the runner pod could not start
	// because the kubelet could not resolve a Secret or ConfigMap (or one of their keys)
	// referenced by the pod. This is NOT an RBAC failure: Secret/ConfigMap volumes are resolved
	// by the kubelet using node credentials, not by any Role granted to the controller or
	// runner ServiceAccount, so no RBAC change affects it — the referenced object must exist in
	// the pod's own namespace.
	WorkflowRunFailureReasonRunnerRefUnresolved WorkflowRunFailureReason = "RunnerRefUnresolved"
)

// StepStatus represents the status of a workflow step
type StepStatus struct {
	// Phase represents the current phase of the step
	// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;Skipped;Waiting
	Phase StepPhase `json:"phase"`

	// StartTime is when the step execution started
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// CompletionTime is when the step execution completed
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// Message provides additional information about the step status
	// +optional
	Message string `json:"message,omitempty"`

	// Error contains error information if the step failed
	// +optional
	Error string `json:"error,omitempty"`

	// RetryCount is the number of retry attempts made (0 = initial attempt, 1+ = retries)
	// +optional
	RetryCount int `json:"retryCount,omitempty"`

	// LastRetryTime is the timestamp of last retry attempt
	// +optional
	LastRetryTime *metav1.Time `json:"lastRetryTime,omitempty"`

	// NextRetryTime is the timestamp when next retry will be attempted (if step is retrying)
	// +optional
	NextRetryTime *metav1.Time `json:"nextRetryTime,omitempty"`
}

// StepPhase represents the phase of a workflow step
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;Skipped;Waiting
type StepPhase string

const (
	// StepPhasePending indicates the step is pending execution
	StepPhasePending StepPhase = "Pending"

	// StepPhaseRunning indicates the step is currently running
	StepPhaseRunning StepPhase = "Running"

	// StepPhaseSucceeded indicates the step completed successfully
	StepPhaseSucceeded StepPhase = "Succeeded"

	// StepPhaseFailed indicates the step failed
	StepPhaseFailed StepPhase = "Failed"

	// StepPhaseSkipped indicates the step was skipped
	StepPhaseSkipped StepPhase = "Skipped"

	// StepPhaseWaiting indicates the step is paused waiting for an external callback
	StepPhaseWaiting StepPhase = "Waiting"
)

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:shortName=florun

// WorkflowRun is the Schema for the workflowruns API
// WorkflowRun represents an execution instance of a Workflow template.
type WorkflowRun struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   WorkflowRunSpec   `json:"spec,omitempty"`
	Status WorkflowRunStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// WorkflowRunList contains a list of WorkflowRun
type WorkflowRunList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []WorkflowRun `json:"items"`
}

// CheckpointingEnabled reports whether checkpointing is configured and enabled for this WorkflowRun.
func (r *WorkflowRun) CheckpointingEnabled() bool {
	return r.Spec.Execution != nil &&
		r.Spec.Execution.Checkpointing != nil &&
		r.Spec.Execution.Checkpointing.Enabled
}

func init() {
	objectTypes = append(objectTypes, &WorkflowRun{}, &WorkflowRunList{})
}
