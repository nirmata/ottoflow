/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/klog/v2"
	metricsclientset "k8s.io/metrics/pkg/client/clientset/versioned"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/logging"
	"github.com/nirmata/ottoflow/internal/secretmount"
	executor "github.com/nirmata/ottoflow/internal/workflow/executor"
)

// RunnerConfig holds controller configuration for the workflow runner Job (images, RBAC, secrets).
// Values are set from process flags in cmd/controller/main.go.
type RunnerConfig struct {
	RunnerImage             string // default: ghcr.io/nirmata/ottoflow/workflow-runner:latest
	RunnerServiceAccount    string // default: controller-manager
	RunnerClusterRole       string // required; validated non-empty at controller startup (no default)
	AgentExecutorCallerRole string // empty disables RBAC-based agent-executor auth
	AgentExecutorCASecret   string // Secret name in run namespace for agent-executor CA (internal TLS); empty disables mount
	AgentExecutorNamespace  string // namespace where agent-executor is deployed; empty uses "ottoflow"
	SecretSourceNamespace   string // empty uses workflow namespace
	PrometheusURL           string // when set, passed to every runner Job (env-specific; not part of workflow spec)
	ImagePullSecrets        string // comma-separated Secret names for runner pod imagePullSecrets; empty disables
	ImagePullPolicy         string // runner Job imagePullPolicy; empty defaults to IfNotPresent
	PodLabelsPartOf         string // value for runner pod label app.kubernetes.io/part-of; empty uses "ottoflow"
	// TTLSecondsAfterFinished: default for runner Job cleanup; 0 means use 3600. Workflow spec can override per run.
	TTLSecondsAfterFinished int32
	// LLMCredentialsSecret is the well-known Secret name resolved in the WorkflowRun's namespace for automatic
	// LLM credential injection into runner Jobs. When set to a non-empty name and the Secret exists, recognized
	// LLM env var keys are injected via secretKeyRef. Explicit spec.execution.job.env entries always take
	// precedence. Empty by default: automatic injection is opt-in and disabled until this is set (via
	// --workflow-runner-llm-credentials-secret / WORKFLOW_RUNNER_LLM_CREDENTIALS_SECRET) or overridden per-run
	// via spec.execution.llmCredentialsSecret.
	LLMCredentialsSecret string
	// SecretRefAllowedNamespaces is the operator allowlist validateSecretRefPolicy checks a
	// cross-namespace secret ref's namespace against. Empty (the default) means same-namespace only.
	SecretRefAllowedNamespaces map[string]struct{}
}

// transientBuildError marks errors from optional pre-build steps (e.g. reading the well-known
// LLM credentials Secret) that should cause a controller requeue rather than permanent WorkflowRun failure.
type transientBuildError struct{ err error }

func (e *transientBuildError) Error() string { return e.err.Error() }
func (e *transientBuildError) Unwrap() error { return e.err }

func (r *RunnerConfig) imagePullPolicy() corev1.PullPolicy {
	switch corev1.PullPolicy(r.ImagePullPolicy) {
	case corev1.PullAlways, corev1.PullNever:
		return corev1.PullPolicy(r.ImagePullPolicy)
	default:
		return corev1.PullIfNotPresent
	}
}

// WorkflowRunReconciler reconciles a WorkflowRun object
type WorkflowRunReconciler struct {
	client.Client
	Scheme              *runtime.Scheme
	MetricsClient       metricsclientset.Interface
	CustomMetricsClient executor.CustomMetricsClient
	PrometheusClient    executor.PrometheusClient
	CELCacheSize        int // Maximum number of compiled CEL expressions to cache
	CELCache            *executor.CELCompilationCache
	EventRecorder       events.EventRecorder
	RunnerConfig        RunnerConfig
	// ControllerNamespace is the namespace where the controller (and the bundled Workflow CRs)
	// are installed. When non-empty, getReferencedWorkflow falls back to this namespace if a
	// Workflow lookup at workflowRef.Namespace returns NotFound. This handles the common
	// non-default-namespace deployment case where an upstream caller hardcodes the original
	// "ottoflow" namespace into workflowRef but the install (and Workflow CRs) live in a
	// different namespace. Empty disables the fallback.
	ControllerNamespace string
}

// Reconcile is part of the main kubernetes reconciliation loop
func (r *WorkflowRunReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the WorkflowRun instance
	workflowRun := &ottoflowv1alpha1.WorkflowRun{}
	if err := r.Get(ctx, req.NamespacedName, workflowRun); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// If already completed, apply run policy (retention / maxAllowed) then skip
	if workflowRun.Status.Phase == ottoflowv1alpha1.WorkflowRunPhaseSucceeded ||
		workflowRun.Status.Phase == ottoflowv1alpha1.WorkflowRunPhaseFailed {
		workflow := &ottoflowv1alpha1.Workflow{}
		workflowNamespace := workflowRun.Spec.WorkflowRef.Namespace
		if workflowNamespace == "" {
			workflowNamespace = workflowRun.Namespace
		}
		workflowKey := types.NamespacedName{Name: workflowRun.Spec.WorkflowRef.Name, Namespace: workflowNamespace}
		if err := r.Get(ctx, workflowKey, workflow); err == nil && workflow.Spec.Run != nil {
			deleted, err := r.applyRunPolicy(ctx, workflow, workflowRun)
			if err != nil {
				logger.Error(err, "run policy cleanup failed", logging.KeyWorkflow, workflowRun.Spec.WorkflowRef.Name, logging.KeyWorkflowRun, req.Name, logging.KeyNamespace, req.Namespace)
				return ctrl.Result{}, err
			}
			if deleted {
				return ctrl.Result{}, nil
			}
		}
		return ctrl.Result{}, nil
	}

	return r.reconcileJobExecution(ctx, req, workflowRun)
}

func (r *WorkflowRunReconciler) reconcileJobExecution(ctx context.Context, req ctrl.Request, workflowRun *ottoflowv1alpha1.WorkflowRun) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Wrap the entire job-execution reconcile in a span so the runner Job can extract
	// the trace context via TRACEPARENT and chain its invoke_workflow span to this root.
	ctx, span := otel.Tracer("ottoflow").Start(ctx, "workflow_run.reconcile",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			attribute.String("workflow.run.name", workflowRun.Name),
			attribute.String("workflow.run.namespace", workflowRun.Namespace),
			attribute.String("workflow.name", workflowRun.Spec.WorkflowRef.Name),
		))
	defer func() { span.End() }()

	workflow, workflowNamespace, err := r.getReferencedWorkflow(ctx, workflowRun)
	if err != nil {
		if apierrors.IsNotFound(err) {
			logger.Error(err, "referenced Workflow not found; failing run", logging.KeyWorkflowRun, req.Name, logging.KeyNamespace, req.Namespace)
			setRunFailed(workflowRun, err.Error(), "")
			if err := r.Status().Update(ctx, workflowRun); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
		logger.Error(err, "failed to resolve referenced Workflow; requeuing", logging.KeyWorkflowRun, req.Name, logging.KeyNamespace, req.Namespace)
		return ctrl.Result{}, err
	}
	// workflow is used below for checkpointing config and run policy

	// --- waitForCallback handling ---
	// If a callback is pending, handle timeout or wait for callback/timeout.
	if workflowRun.Status.PendingCallback != nil {
		res, proceed, err := r.reconcilePendingCallback(ctx, req, workflow, workflowRun)
		if err != nil || !proceed {
			return res, err
		}
		// proceed == true: resume — fall through to the normal Job-creation block below,
		// with PendingCallback still set so the executor consumes the outputs.
	}

	jobName := workflowRunnerJobName(workflowRun.Name)
	job := &batchv1.Job{}
	jobKey := types.NamespacedName{Name: jobName, Namespace: workflowRun.Namespace}
	if err := r.Get(ctx, jobKey, job); err != nil {
		if client.IgnoreNotFound(err) != nil {
			logger.Error(err, "failed to get runner Job", logging.KeyWorkflow, workflowRun.Spec.WorkflowRef.Name, logging.KeyWorkflowRun, req.Name, logging.KeyNamespace, req.Namespace, "job", jobKey)
			return ctrl.Result{}, err
		}

		warnCheckpointForEach(ctx, workflow, workflowRun)

		createdJob, err := r.buildWorkflowRunnerJob(ctx, workflowRun, workflow)
		if err != nil {
			var te *transientBuildError
			if errors.As(err, &te) {
				// Transient error (e.g. API server hiccup on optional credentials Secret lookup).
				// Do not permanently fail — controller-runtime requeues with backoff.
				logger.Error(te.err, "transient error building runner Job, will requeue",
					logging.KeyWorkflow, workflowRun.Spec.WorkflowRef.Name,
					logging.KeyWorkflowRun, req.Name,
					logging.KeyNamespace, req.Namespace)
				return ctrl.Result{}, te.err
			}
			logger.Error(err, "failed to build runner Job", logging.KeyWorkflow, workflowRun.Spec.WorkflowRef.Name, logging.KeyWorkflowRun, req.Name, logging.KeyNamespace, req.Namespace)
			setRunFailed(workflowRun, fmt.Sprintf("Failed to build runner Job: %v", err), "")
			if err := r.Status().Update(ctx, workflowRun); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, err
		}
		if err := r.ensureRunnerAccess(ctx, workflowRun, workflow, createdJob.Spec.Template.Spec.ServiceAccountName, workflowRunUsesExplicitRunnerServiceAccount(workflowRun)); err != nil {
			if errors.Is(err, errRequeueRunnerAccess) {
				logger.Info("runner ClusterRoleBinding migration in progress, requeuing", logging.KeyWorkflow, workflowRun.Spec.WorkflowRef.Name, logging.KeyWorkflowRun, req.Name, logging.KeyNamespace, req.Namespace)
				return ctrl.Result{RequeueAfter: time.Second}, nil
			}
			logger.Error(err, "failed to ensure runner service account access", logging.KeyWorkflow, workflowRun.Spec.WorkflowRef.Name, logging.KeyWorkflowRun, req.Name, logging.KeyNamespace, req.Namespace)
			setRunFailed(workflowRun, fmt.Sprintf("Failed to prepare runner access: %v", err), "")
			if err := r.Status().Update(ctx, workflowRun); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, err
		}
		sourceNamespace := runnerSecretSourceNamespace(r.RunnerConfig, workflowNamespace)
		if err := r.ensureRunnerSecrets(ctx, workflowRun, createdJob, sourceNamespace); err != nil {
			logger.Error(err, "failed to ensure runner secrets", logging.KeyWorkflow, workflowRun.Spec.WorkflowRef.Name, logging.KeyWorkflowRun, req.Name, logging.KeyNamespace, req.Namespace)
			setRunFailed(workflowRun, fmt.Sprintf("Failed to prepare runner secrets: %v", err), "")
			if err := r.Status().Update(ctx, workflowRun); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, createdJob); err != nil {
			if apierrors.IsAlreadyExists(err) {
				// Job was created by another reconciler or manually; re-Get and continue
				if getErr := r.Get(ctx, jobKey, job); getErr != nil {
					logger.Error(getErr, "failed to get runner Job after AlreadyExists", logging.KeyWorkflow, workflowRun.Spec.WorkflowRef.Name, logging.KeyWorkflowRun, req.Name, logging.KeyNamespace, req.Namespace, "job", jobKey)
					return ctrl.Result{}, getErr
				}
			} else {
				logger.Error(err, "failed to create runner Job", logging.KeyWorkflow, workflowRun.Spec.WorkflowRef.Name, logging.KeyWorkflowRun, req.Name, logging.KeyNamespace, req.Namespace, "job", jobKey)
				setRunFailed(workflowRun, fmt.Sprintf("Failed to create runner Job %s: %v", jobName, err), "")
				if err := r.Status().Update(ctx, workflowRun); err != nil {
					return ctrl.Result{}, err
				}
				return ctrl.Result{}, err
			}
		} else {
			job = createdJob
		}
	}

	if workflowRun.Status.Execution == nil || workflowRun.Status.Execution.JobName != jobName {
		now := metav1.Now()
		if workflowRun.Status.Phase == "" {
			workflowRun.Status.Phase = ottoflowv1alpha1.WorkflowRunPhasePending
		}
		prevAttempts := int32(0)
		if workflowRun.Status.Execution != nil {
			prevAttempts = workflowRun.Status.Execution.Attempts
		}
		workflowRun.Status.Execution = &ottoflowv1alpha1.WorkflowRunExecutionStatus{
			Phase:     string(workflowRun.Status.Phase),
			JobName:   jobName,
			Message:   "Runner Job created",
			StartTime: &now,
			Attempts:  prevAttempts,
		}
		if err := r.Status().Update(ctx, workflowRun); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Track whether we modified status this reconcile; only call Status().Update when something changed to reduce API traffic and conflict risk
	statusModified := false

	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.InNamespace(workflowRun.Namespace), client.MatchingLabels{"job-name": jobName}); err == nil {
		if len(podList.Items) > 0 {
			if workflowRun.Status.Execution == nil {
				workflowRun.Status.Execution = &ottoflowv1alpha1.WorkflowRunExecutionStatus{}
			}
			if workflowRun.Status.Execution.PodName != podList.Items[0].Name {
				workflowRun.Status.Execution.PodName = podList.Items[0].Name
				statusModified = true
			}
		}
	}

	if job.Status.Active > 0 && workflowRun.Status.Execution != nil {
		newPhase := string(ottoflowv1alpha1.WorkflowRunPhaseRunning)
		newMsg := "Runner Job is running"
		if workflowRun.Status.Execution.Phase != newPhase || workflowRun.Status.Execution.Message != newMsg {
			workflowRun.Status.Execution.Phase = newPhase
			workflowRun.Status.Execution.Message = newMsg
			statusModified = true
		}
	}

	if job.Status.Failed > 0 && workflowRun.Status.Phase != ottoflowv1alpha1.WorkflowRunPhaseSucceeded && workflowRun.Status.Phase != ottoflowv1alpha1.WorkflowRunPhaseFailed {
		result, handled, err := r.handleFailedJob(ctx, workflowRun, workflow, job, jobName)
		if handled {
			return result, err
		}
	}

	if statusModified {
		if err := r.Status().Update(ctx, workflowRun); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
}

func (r *WorkflowRunReconciler) getReferencedWorkflow(ctx context.Context, workflowRun *ottoflowv1alpha1.WorkflowRun) (*ottoflowv1alpha1.Workflow, string, error) {
	workflowRef := workflowRun.Spec.WorkflowRef
	workflowNamespace := workflowRef.Namespace
	if workflowNamespace == "" {
		workflowNamespace = workflowRun.Namespace
	}

	workflow := &ottoflowv1alpha1.Workflow{}
	workflowKey := types.NamespacedName{
		Name:      workflowRef.Name,
		Namespace: workflowNamespace,
	}
	err := r.Get(ctx, workflowKey, workflow)
	if err == nil {
		return workflow, workflowNamespace, nil
	}

	// Fallback: when the Workflow is not found in the requested namespace and the controller
	// has a ControllerNamespace configured, try that namespace. Common case: an upstream
	// caller hardcodes workflowRef.namespace=ottoflow but the install lives in a
	// different namespace.
	if apierrors.IsNotFound(err) && r.ControllerNamespace != "" && r.ControllerNamespace != workflowNamespace {
		fallbackKey := types.NamespacedName{
			Name:      workflowRef.Name,
			Namespace: r.ControllerNamespace,
		}
		fallback := &ottoflowv1alpha1.Workflow{}
		if fbErr := r.Get(ctx, fallbackKey, fallback); fbErr == nil {
			klog.Warningf("Workflow %s not found; falling back to controller namespace %q. The upstream caller should set workflowRef.namespace=%q to make this lookup explicit.",
				workflowKey, r.ControllerNamespace, r.ControllerNamespace)
			return fallback, r.ControllerNamespace, nil
		} else if !apierrors.IsNotFound(fbErr) {
			// Transient fallback error (e.g. API server timeout): the caller must requeue, not
			// terminally fail the run on what would otherwise be treated as a NotFound.
			return nil, "", fmt.Errorf("failed to get Workflow %s in controller namespace %q: %w", fallbackKey, r.ControllerNamespace, fbErr)
		}
	}

	return nil, "", fmt.Errorf("failed to get Workflow %s: %w", workflowKey, err)
}

func workflowRunnerJobName(runName string) string {
	base := strings.ToLower(fmt.Sprintf("%s-runner", runName))
	base = strings.ReplaceAll(base, "_", "-")
	if len(base) <= 63 {
		return strings.TrimSuffix(base, "-")
	}
	// Truncation: append short hash of full run name to avoid collisions when two runs share the same 63-char prefix
	h := fnv.New64a()
	_, _ = h.Write([]byte(runName))
	// 8 hex chars for uniqueness suffix
	suffix := fmt.Sprintf("%08x", h.Sum64())
	prefix := strings.TrimSuffix(base[:63-len(suffix)-1], "-") // reserve "-" + suffix
	return prefix + "-" + suffix
}

// jobConditionTrue reports whether the Job has a condition of the given type set to True.
func jobConditionTrue(job *batchv1.Job, t batchv1.JobConditionType) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == t && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// RunnerClusterRole is required and validated non-empty at startup (cmd/controller/main.go); no safe default.
func workflowRunnerClusterRoleName(cfg RunnerConfig) string { return cfg.RunnerClusterRole }

// agentExecutorCallerClusterRoleName returns the ClusterRole name for agent-executor caller RBAC.
// When set (e.g. by Helm), the controller creates a ClusterRoleBinding per runner SA to this role
// so runner Jobs can authenticate to the agent executor via SubjectAccessReview.
func agentExecutorCallerClusterRoleName(cfg RunnerConfig) string {
	return cfg.AgentExecutorCallerRole
}

func workflowRunnerRoleBindingName(namespace, serviceAccountName string) string {
	name := strings.ToLower(fmt.Sprintf("ottoflow-runner-%s-%s", namespace, serviceAccountName))
	name = strings.ReplaceAll(name, "_", "-")
	if len(name) > 253 {
		name = name[:253]
	}
	return strings.TrimSuffix(name, "-")
}

// agentExecutorCallerRoleBindingName returns the ClusterRoleBinding name for agent-executor caller for a given runner SA.
func agentExecutorCallerRoleBindingName(namespace, serviceAccountName string) string {
	name := strings.ToLower(fmt.Sprintf("ottoflow-agent-executor-caller-%s-%s", namespace, serviceAccountName))
	name = strings.ReplaceAll(name, "_", "-")
	if len(name) > 253 {
		name = name[:253]
	}
	return strings.TrimSuffix(name, "-")
}

func workflowRunUsesExplicitRunnerServiceAccount(workflowRun *ottoflowv1alpha1.WorkflowRun) bool {
	return workflowRun != nil &&
		workflowRun.Spec.Execution != nil &&
		workflowRun.Spec.Execution.Job != nil &&
		workflowRun.Spec.Execution.Job.ServiceAccountName != ""
}

// errRequeueRunnerAccess signals that a runner ClusterRoleBinding is being migrated to a new
// RoleRef (delete-and-recreate, since RoleRef is immutable) and the caller should requeue and
// retry rather than treat this as a terminal failure.
var errRequeueRunnerAccess = errors.New("runner ClusterRoleBinding migration in progress")

// buildRunnerClusterRoleBinding constructs the ClusterRoleBinding that grants a workflow-runner
// ServiceAccount access to the given ClusterRole.
func buildRunnerClusterRoleBinding(name, roleName, saName, saNamespace string) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"app.kubernetes.io/part-of": "ottoflow",
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     roleName,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      saName,
				Namespace: saNamespace,
			},
		},
	}
}

func (r *WorkflowRunReconciler) ensureRunnerAccess(ctx context.Context, workflowRun *ottoflowv1alpha1.WorkflowRun, workflow *ottoflowv1alpha1.Workflow, serviceAccountName string, explicitServiceAccount bool) error {
	if explicitServiceAccount || workflowRun == nil || serviceAccountName == "" {
		return nil
	}

	if workflowRunnerClusterRoleName(r.RunnerConfig) == "" {
		return fmt.Errorf("runner ClusterRole name is empty; set --workflow-runner-cluster-role")
	}

	serviceAccount := &corev1.ServiceAccount{}
	serviceAccountKey := types.NamespacedName{Namespace: workflowRun.Namespace, Name: serviceAccountName}
	if err := r.Get(ctx, serviceAccountKey, serviceAccount); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return err
		}
		serviceAccount = &corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{
				Name:      serviceAccountName,
				Namespace: workflowRun.Namespace,
				Labels: map[string]string{
					"app.kubernetes.io/part-of": "ottoflow",
				},
			},
		}
		// Own the runner ServiceAccount by the Workflow so it is garbage-collected with the Workflow.
		// Guarded because: (1) the SA name is derived per-Workflow only when RunnerServiceAccount is unset —
		// a config-level shared SA must not be owned by any single Workflow; (2) cross-namespace ownerRefs are
		// invalid and get GC-deleted as orphans, so only own when the Workflow shares the run's namespace.
		if workflow != nil &&
			r.RunnerConfig.RunnerServiceAccount == "" &&
			workflow.Namespace == workflowRun.Namespace {
			if err := ctrl.SetControllerReference(workflow, serviceAccount, r.Scheme); err != nil {
				return err
			}
		}
		if err := r.Create(ctx, serviceAccount); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	}

	clusterRoleBindingName := workflowRunnerRoleBindingName(workflowRun.Namespace, serviceAccountName)
	clusterRoleBindingKey := types.NamespacedName{Name: clusterRoleBindingName}
	clusterRoleBinding := &rbacv1.ClusterRoleBinding{}
	if err := r.Get(ctx, clusterRoleBindingKey, clusterRoleBinding); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return err
		}
		clusterRoleBinding = buildRunnerClusterRoleBinding(clusterRoleBindingName, workflowRunnerClusterRoleName(r.RunnerConfig), serviceAccountName, workflowRun.Namespace)
		if err := r.Create(ctx, clusterRoleBinding); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	} else {
		// Only update bindings that we manage; avoid overwriting user-managed ClusterRoleBindings with the same name
		const partOfLabel = "app.kubernetes.io/part-of"
		if clusterRoleBinding.Labels == nil || clusterRoleBinding.Labels[partOfLabel] != "ottoflow" {
			return fmt.Errorf("ClusterRoleBinding %s exists but is not managed by OttoFlow (missing label %s=ottoflow); refusing to update", clusterRoleBindingName, partOfLabel)
		}

		desiredRoleName := workflowRunnerClusterRoleName(r.RunnerConfig)
		if clusterRoleBinding.RoleRef.Name != desiredRoleName {
			// RoleRef is immutable on an existing ClusterRoleBinding, so migrating to a new
			// (narrower) role requires delete-and-recreate rather than an in-place Update.
			if err := r.Delete(ctx, clusterRoleBinding); err != nil && client.IgnoreNotFound(err) != nil {
				return err
			}
			fresh := buildRunnerClusterRoleBinding(clusterRoleBindingName, desiredRoleName, serviceAccountName, workflowRun.Namespace)
			if err := r.Create(ctx, fresh); err != nil {
				if apierrors.IsAlreadyExists(err) {
					// Another reconcile (or a stale cache) recreated the binding concurrently.
					// Check whether it already landed on the desired role before requeuing.
					recreated := &rbacv1.ClusterRoleBinding{}
					if getErr := r.Get(ctx, clusterRoleBindingKey, recreated); getErr != nil {
						if apierrors.IsNotFound(getErr) {
							return errRequeueRunnerAccess
						}
						return getErr
					}
					if recreated.RoleRef.Name != desiredRoleName {
						return errRequeueRunnerAccess
					}
				} else if apierrors.IsForbidden(err) || apierrors.IsInvalid(err) {
					// A permission or validation failure will not resolve on retry: fail terminally so
					// the caller marks the run Failed instead of requeuing forever.
					return fmt.Errorf("cannot bind runner ClusterRoleBinding %s to role %s (verify the controller holds 'bind' on that role and the role is valid): %w", clusterRoleBindingName, desiredRoleName, err)
				} else {
					// The old binding was already deleted, so any other Create failure (timeout, 5xx,
					// etc.) is transient: terminal-failing here would leave the runner ServiceAccount
					// with no binding at all. errRequeueRunnerAccess is a bare sentinel the caller only
					// logs at Info, so log the real error here before requeuing.
					log.FromContext(ctx).Error(err, "failed to recreate runner ClusterRoleBinding after role migration; requeuing")
					return errRequeueRunnerAccess
				}
			}
		} else if len(clusterRoleBinding.Subjects) != 1 ||
			clusterRoleBinding.Subjects[0].Kind != "ServiceAccount" ||
			clusterRoleBinding.Subjects[0].Name != serviceAccountName ||
			clusterRoleBinding.Subjects[0].Namespace != workflowRun.Namespace {
			clusterRoleBinding.Subjects = []rbacv1.Subject{
				{
					Kind:      "ServiceAccount",
					Name:      serviceAccountName,
					Namespace: workflowRun.Namespace,
				},
			}
			if err := r.Update(ctx, clusterRoleBinding); err != nil {
				return err
			}
		}
	}

	// Ensure agent-executor-caller ClusterRoleBinding so this runner SA can call the agent executor (RBAC auth).
	if callerRoleName := agentExecutorCallerClusterRoleName(r.RunnerConfig); callerRoleName != "" {
		if err := r.ensureAgentExecutorCallerBinding(ctx, workflowRun.Namespace, serviceAccountName, callerRoleName); err != nil {
			return err
		}
	}

	return nil
}

func (r *WorkflowRunReconciler) ensureAgentExecutorCallerBinding(ctx context.Context, namespace, serviceAccountName, roleName string) error {
	bindingName := agentExecutorCallerRoleBindingName(namespace, serviceAccountName)
	key := types.NamespacedName{Name: bindingName}
	existing := &rbacv1.ClusterRoleBinding{}
	if err := r.Get(ctx, key, existing); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return err
		}
		existing = nil
	}
	desired := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: bindingName,
			Labels: map[string]string{
				"app.kubernetes.io/part-of": "ottoflow",
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     roleName,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      serviceAccountName,
				Namespace: namespace,
			},
		},
	}
	if existing == nil {
		return r.Create(ctx, desired)
	}
	const partOfLabel = "app.kubernetes.io/part-of"
	if existing.Labels == nil || existing.Labels[partOfLabel] != "ottoflow" {
		return nil // not managed by us, skip update
	}
	if existing.RoleRef.Name != roleName ||
		len(existing.Subjects) != 1 ||
		existing.Subjects[0].Kind != "ServiceAccount" ||
		existing.Subjects[0].Name != serviceAccountName ||
		existing.Subjects[0].Namespace != namespace {
		existing.RoleRef = desired.RoleRef
		existing.Subjects = desired.Subjects
		return r.Update(ctx, existing)
	}
	return nil
}

// runnerSecretSourceNamespace returns the namespace from which to copy Secret-backed
// volumes when they are missing in the runner namespace. When RunnerConfig.SecretSourceNamespace
// is set (e.g. to the OttoFlow install namespace), that is used; otherwise the Workflow's
// namespace is used.
func runnerSecretSourceNamespace(cfg RunnerConfig, workflowNamespace string) string {
	if cfg.SecretSourceNamespace != "" {
		return cfg.SecretSourceNamespace
	}
	return workflowNamespace
}

// injectWellKnownLLMCredentials looks up the well-known LLM credentials Secret in the
// WorkflowRun's namespace and returns secretKeyRef EnvVars for any recognized LLM keys
// that are not already present in existingEnvNames. Returns nil when the Secret does not
// exist (not an error — the tenant simply hasn't created the well-known Secret).
// Only keys present in executor.LLMEnvAllowlist are ever injected.
func (r *WorkflowRunReconciler) injectWellKnownLLMCredentials(
	ctx context.Context,
	workflowRun *ottoflowv1alpha1.WorkflowRun,
	existingEnvNames map[string]struct{},
) ([]corev1.EnvVar, error) {
	secretName := r.RunnerConfig.LLMCredentialsSecret
	secretNamespace := workflowRun.Namespace

	// Per-run spec overrides the cluster-wide default.
	if workflowRun.Spec.Execution != nil && workflowRun.Spec.Execution.LLMCredentialsSecret != nil {
		ref := workflowRun.Spec.Execution.LLMCredentialsSecret
		secretName = ref.Name
		if ref.Namespace != "" {
			secretNamespace = ref.Namespace
		}
	}

	if secretName == "" {
		return nil, nil
	}

	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: secretNamespace, Name: secretName}
	if err := r.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil // namespace hasn't created a well-known Secret; not an error
		}
		return nil, &transientBuildError{err: fmt.Errorf("get well-known LLM credentials Secret %s: %w", key, err)}
	}

	// Build an O(1) lookup set from the allowlist.
	allowlist := make(map[string]struct{}, len(executor.LLMEnvAllowlist))
	for _, k := range executor.LLMEnvAllowlist {
		allowlist[k] = struct{}{}
	}

	var extras []corev1.EnvVar
	var matched int
	var skippedKeys []string
	for key := range secret.Data {
		if _, allowed := allowlist[key]; !allowed {
			skippedKeys = append(skippedKeys, key)
			continue // not a recognized LLM env var — skip
		}
		matched++
		if _, exists := existingEnvNames[key]; exists {
			continue // explicit spec.execution.job.env entry wins
		}
		extras = append(extras, corev1.EnvVar{
			Name: key,
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
					Key:                  key,
				},
			},
		})
	}

	if len(skippedKeys) > 0 && r.EventRecorder != nil {
		sort.Strings(skippedKeys)
		r.EventRecorder.Eventf(workflowRun, nil, corev1.EventTypeWarning,
			"LLMCredentialInjection", "LLMCredentialInjection",
			"Secret %s/%s contained %d keys, %d matched the LLM allowlist, %d were ignored (ignored: %s)",
			secretNamespace, secretName,
			len(secret.Data), matched, len(skippedKeys),
			strings.Join(skippedKeys, ", "))
	}

	if len(extras) > 0 {
		// Sort for deterministic Job spec output (map iteration order is non-deterministic).
		sort.Slice(extras, func(i, j int) bool { return extras[i].Name < extras[j].Name })
		klog.V(3).InfoS("Injecting LLM credentials from well-known Secret",
			"namespace", secretNamespace, "secret", secretName, "keys", len(extras))
	}
	return extras, nil
}

// ensureRunnerSecrets ensures that every Secret-backed volume referenced by the runner Job
// exists in the WorkflowRun's namespace. If a secret is missing there, it is copied from
// sourceNamespace (e.g. the Workflow's namespace or the OttoFlow install namespace).
// This allows Workflow podTemplates or WorkflowRun execution.job.volumes to reference
// secrets that exist only in the ottoflow namespace (e.g. agent-executor TLS CA).
func (r *WorkflowRunReconciler) ensureRunnerSecrets(ctx context.Context, workflowRun *ottoflowv1alpha1.WorkflowRun, job *batchv1.Job, sourceNamespace string) error {
	runnerNamespace := workflowRun.Namespace
	if sourceNamespace == "" {
		sourceNamespace = runnerNamespace
	}
	for _, vol := range job.Spec.Template.Spec.Volumes {
		if vol.Secret == nil {
			continue
		}
		// NEVER copy a volume buildSecretMounts minted for one of the workflow's own Secret
		// references (see generatedSecretVolumePrefix). collectSecretRefs resolves those refs
		// against the RUN's namespace and validateSecretRefPolicy default-denies any that
		// resolve elsewhere, so a minted volume always names a Secret that is required to live
		// in the runner namespace. Copying it from sourceNamespace on a miss would defeat that
		// policy exactly where it matters most: for a cross-namespace WorkflowRun
		// (workflowRef.namespace != the run's namespace) sourceNamespace is the WORKFLOW's
		// namespace, so a same-namespace-looking ref to a Secret that does not exist in the run
		// namespace would silently pull that Secret — with EVERY key in it, not just the
		// referenced one — out of another namespace and into one the run's author can read.
		// A genuinely missing Secret must instead surface as the FailedMount that
		// detectStuckRunnerPod reports, naming the reference to fix.
		if strings.HasPrefix(vol.Name, generatedSecretVolumePrefix) {
			continue
		}
		secretName := vol.Secret.SecretName
		if secretName == "" {
			continue
		}
		existing := &corev1.Secret{}
		runnerKey := types.NamespacedName{Namespace: runnerNamespace, Name: secretName}
		err := r.Get(ctx, runnerKey, existing)
		if err == nil {
			continue // secret already exists in runner namespace
		}
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get secret %s in runner namespace: %w", runnerKey, err)
		}
		sourceSecret := &corev1.Secret{}
		sourceKey := types.NamespacedName{Namespace: sourceNamespace, Name: secretName}
		if err := r.Get(ctx, sourceKey, sourceSecret); err != nil {
			if apierrors.IsNotFound(err) {
				return fmt.Errorf("secret %q not found in runner namespace %q or source namespace %q", secretName, runnerNamespace, sourceNamespace)
			}
			return fmt.Errorf("get secret %s from source namespace: %w", sourceKey, err)
		}
		copySecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      secretName,
				Namespace: runnerNamespace,
				Labels: map[string]string{
					"app.kubernetes.io/part-of": "ottoflow",
				},
			},
			Data:       sourceSecret.Data,
			StringData: sourceSecret.StringData,
			Type:       sourceSecret.Type,
		}
		if err := ctrl.SetControllerReference(workflowRun, copySecret, r.Scheme); err != nil {
			return fmt.Errorf("set owner reference on secret copy: %w", err)
		}
		if err := r.Create(ctx, copySecret); err != nil {
			if apierrors.IsAlreadyExists(err) {
				continue // another reconciler or user created it
			}
			return fmt.Errorf("create secret %s in runner namespace: %w", runnerKey, err)
		}
	}
	return nil
}

// runnerArgs returns the command-line args for the workflow-runner container (e.g. --prometheus-url).
// These are environment-specific and set by the controller; they are not part of the WorkflowRun spec.
func runnerArgs(cfg RunnerConfig) []string {
	var args []string
	if cfg.PrometheusURL != "" {
		args = append(args, "--prometheus-url", cfg.PrometheusURL)
	}
	return args
}

func (r *WorkflowRunReconciler) buildWorkflowRunnerJob(ctx context.Context, workflowRun *ottoflowv1alpha1.WorkflowRun, workflow *ottoflowv1alpha1.Workflow) (*batchv1.Job, error) {
	runnerImage := r.RunnerConfig.RunnerImage
	if runnerImage == "" {
		runnerImage = "ghcr.io/nirmata/ottoflow/workflow-runner:latest"
	}
	serviceAccountName := r.RunnerConfig.RunnerServiceAccount
	if serviceAccountName == "" {
		// Derive per-workflow SA name from the workflow being run so each workflow
		// gets minimum-privilege RBAC scoped to its own declared needs.
		base := workflowRun.Spec.WorkflowRef.Name
		const suffix = "-runner"
		if len(base)+len(suffix) > 253 {
			base = base[:253-len(suffix)]
			base = strings.TrimRight(base, "-")
		}
		serviceAccountName = base + suffix
	}
	backoffLimit := int32(0)
	ttlSecondsAfterFinished := int32(3600)
	if r.RunnerConfig.TTLSecondsAfterFinished > 0 {
		ttlSecondsAfterFinished = r.RunnerConfig.TTLSecondsAfterFinished
	}

	jobSpec := workflowRun.Spec.Execution
	var podEnv []corev1.EnvVar
	var volumes []corev1.Volume
	var volumeMounts []corev1.VolumeMount
	var resources corev1.ResourceRequirements
	var nodeSelector map[string]string
	var tolerations []corev1.Toleration
	var affinity *corev1.Affinity
	var activeDeadlineSeconds *int64

	if jobSpec != nil && jobSpec.Job != nil {
		if err := jobSpec.Job.Validate(); err != nil {
			return nil, err
		}
		if jobSpec.Job.Image != "" {
			runnerImage = jobSpec.Job.Image
		}
		if jobSpec.Job.ServiceAccountName != "" {
			serviceAccountName = jobSpec.Job.ServiceAccountName
		}
		if jobSpec.Job.BackoffLimit != nil {
			backoffLimit = *jobSpec.Job.BackoffLimit
		}
		if jobSpec.Job.TTLSecondsAfterFinished != nil {
			ttlSecondsAfterFinished = *jobSpec.Job.TTLSecondsAfterFinished
		}
		activeDeadlineSeconds = jobSpec.Job.ActiveDeadlineSeconds
		podEnv = append(podEnv, jobSpec.Job.Env...)
		volumes = append(volumes, jobSpec.Job.Volumes...)
		volumeMounts = append(volumeMounts, jobSpec.Job.VolumeMounts...)
		resources = jobSpec.Job.Resources
		nodeSelector = jobSpec.Job.NodeSelector
		tolerations = jobSpec.Job.Tolerations
		affinity = jobSpec.Job.Affinity
	}

	// Inject LLM credentials from the well-known namespace-scoped Secret (multi-tenant support).
	// Explicit spec.execution.job.env entries win: we only inject keys not already in podEnv.
	existingEnvNames := make(map[string]struct{}, len(podEnv))
	for _, e := range podEnv {
		existingEnvNames[e.Name] = struct{}{}
	}
	wellKnownEnv, err := r.injectWellKnownLLMCredentials(ctx, workflowRun, existingEnvNames)
	if err != nil {
		return nil, err
	}
	podEnv = append(podEnv, wellKnownEnv...)

	// Optionally mount agent-executor CA so the runner can verify internal TLS (secret must exist in run namespace).
	if caSecret := r.RunnerConfig.AgentExecutorCASecret; caSecret != "" {
		const agentExecutorCAVolumeName = "agent-executor-ca"
		for _, v := range volumes {
			if v.Name == agentExecutorCAVolumeName {
				return nil, fmt.Errorf(
					"cannot mount the agent-executor CA: a volume named %q already exists on the "+
						"runner Job pod spec (likely from spec.execution.job.volumes); rename that "+
						"volume to resolve the conflict", agentExecutorCAVolumeName)
			}
		}
		volumes = append(volumes, corev1.Volume{
			Name: "agent-executor-ca",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: caSecret,
					Items:      []corev1.KeyToPath{{Key: "tls.crt", Path: "ca.crt"}},
				},
			},
		})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      "agent-executor-ca",
			MountPath: "/etc/ottoflow/agent-executor-ca",
			ReadOnly:  true,
		})
	}

	secretRefs, err := collectSecretRefs(ctx, r.Client, workflow, workflowRun)
	if err != nil {
		return nil, err
	}
	if err := validateSecretRefPolicy(secretRefs, workflowRun.Namespace, r.RunnerConfig); err != nil {
		return nil, err
	}
	existingVolumeNames := make(map[string]struct{}, len(volumes))
	for _, v := range volumes {
		existingVolumeNames[v.Name] = struct{}{}
	}
	secretVolumes, secretVolumeMounts, secretMountsJSON, secretVolumeOrigins, err := buildSecretMounts(secretRefs, workflowRun.Namespace, existingVolumeNames)
	if err != nil {
		return nil, err
	}
	volumes = append(volumes, secretVolumes...)
	volumeMounts = append(volumeMounts, secretVolumeMounts...)

	jobName := workflowRunnerJobName(workflowRun.Name)
	runnerEnv := []corev1.EnvVar{
		{Name: "WORKFLOW_RUN_NAME", Value: workflowRun.Name},
		{Name: "WORKFLOW_RUN_NAMESPACE", Value: workflowRun.Namespace},
		{Name: "JOB_NAME", Value: jobName},
		{
			Name: "POD_NAME",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
			},
		},
	}
	if secretMountsJSON != "" {
		runnerEnv = append(runnerEnv, corev1.EnvVar{Name: secretmount.EnvVar, Value: secretMountsJSON})
	}
	if r.RunnerConfig.PrometheusURL != "" {
		runnerEnv = append(runnerEnv, corev1.EnvVar{Name: "PROMETHEUS_URL", Value: r.RunnerConfig.PrometheusURL})
	}
	if r.RunnerConfig.AgentExecutorNamespace != "" {
		runnerEnv = append(runnerEnv, corev1.EnvVar{Name: "AGENT_EXECUTOR_NAMESPACE", Value: r.RunnerConfig.AgentExecutorNamespace})
	}
	runnerEnv = append(runnerEnv, podEnv...)

	// Propagate the controller's trace context into the runner Job via W3C TraceContext
	// env vars (TRACEPARENT / TRACESTATE). The runner extracts these at startup and
	// chains its invoke_workflow span as a child, bridging the process boundary.
	carrier := make(propagation.MapCarrier)
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	for k, v := range carrier {
		// W3C header names are lowercase ("traceparent"); env vars use SCREAMING_SNAKE_CASE.
		runnerEnv = append(runnerEnv, corev1.EnvVar{
			Name:  strings.ToUpper(strings.ReplaceAll(k, "-", "_")),
			Value: v,
		})
	}

	var imagePullSecrets []corev1.LocalObjectReference
	if r.RunnerConfig.ImagePullSecrets != "" {
		for _, name := range strings.Split(r.RunnerConfig.ImagePullSecrets, ",") {
			name = strings.TrimSpace(name)
			if name != "" {
				imagePullSecrets = append(imagePullSecrets, corev1.LocalObjectReference{Name: name})
			}
		}
	}

	partOf := "ottoflow"
	if r.RunnerConfig.PodLabelsPartOf != "" {
		partOf = r.RunnerConfig.PodLabelsPartOf
	}
	runnerPodLabels := map[string]string{
		"app.kubernetes.io/part-of": partOf,
		runnerManagedLabel:          workflowRun.Name,
	}

	workflowName := workflowRun.Spec.WorkflowRef.Name
	if workflowName == "" {
		workflowName = workflowRun.Name // fallback for legacy runs
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: workflowRun.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/part-of":    partOf, // same as pod template so Kyverno restrict-automount-sa-token skips this Job
				runnerManagedLabel:             workflowRun.Name,
				"ottoflow.nirmata.io/workflow": workflowName, // for cleanup: delete job -l ottoflow.nirmata.io/workflow=cloud-cost-daily
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &ttlSecondsAfterFinished,
			ActiveDeadlineSeconds:   activeDeadlineSeconds,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: runnerPodLabels,
				},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					ServiceAccountName:           serviceAccountName,
					AutomountServiceAccountToken: ptr.To(true), // required for agent-executor RBAC auth (Bearer token)
					ImagePullSecrets:             imagePullSecrets,
					NodeSelector:                 nodeSelector,
					Tolerations:                  tolerations,
					Affinity:                     affinity,
					Volumes:                      volumes,
					// Security context for Kyverno / restricted PSS (runAsNonRoot, drop ALL, no privilege escalation)
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: ptr.To(true),
						RunAsUser:    ptr.To(int64(65532)),
						RunAsGroup:   ptr.To(int64(65532)),
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
					Containers: []corev1.Container{
						{
							Name:            "workflow-runner",
							Image:           runnerImage,
							ImagePullPolicy: r.RunnerConfig.imagePullPolicy(),
							Command:         []string{"/ko-app/workflow-runner"},
							Args:            runnerArgs(r.RunnerConfig),
							Env:             runnerEnv,
							Resources:       resources,
							VolumeMounts:    volumeMounts,
							SecurityContext: &corev1.SecurityContext{
								AllowPrivilegeEscalation: ptr.To(false),
								Capabilities: &corev1.Capabilities{
									Drop: []corev1.Capability{"ALL"},
								},
								RunAsNonRoot: ptr.To(true),
								RunAsUser:    ptr.To(int64(65532)),
								SeccompProfile: &corev1.SeccompProfile{
									Type: corev1.SeccompProfileTypeRuntimeDefault,
								},
							},
						},
					},
				},
			},
		},
	}
	if b, jsonErr := json.Marshal(secretVolumeOrigins); jsonErr == nil && len(secretVolumeOrigins) > 0 && len(b) <= maxSecretVolumeOriginsAnnotationBytes {
		if job.Annotations == nil {
			job.Annotations = map[string]string{}
		}
		job.Annotations[generatedSecretVolumeOriginsAnnotation] = string(b)
	}
	if err := ctrl.SetControllerReference(workflowRun, job, r.Scheme); err != nil {
		return nil, err
	}
	return job, nil
}

// applyRunPolicy applies retention and maxAllowed from workflow.Spec.Run to completed WorkflowRuns.
// It lists runs for the same workflow (by WorkflowRef), deletes by retention then by maxAllowed, and returns
// true if the current workflowRun was deleted.
func (r *WorkflowRunReconciler) applyRunPolicy(ctx context.Context, workflow *ottoflowv1alpha1.Workflow, current *ottoflowv1alpha1.WorkflowRun) (currentDeleted bool, err error) {
	run := workflow.Spec.Run
	if run == nil {
		return false, nil
	}
	namespace := current.Namespace
	refName := workflow.Name
	refNamespace := workflow.Namespace
	if refNamespace == "" {
		refNamespace = namespace
	}

	completed := r.listCompletedRunsForWorkflow(ctx, namespace, refName, refNamespace)
	if len(completed) == 0 {
		return false, nil
	}

	now := time.Now()

	// 1. Retention: delete completed runs older than retentionMinutes
	if run.RetentionMinutes > 0 {
		cutoffRetention := now.Add(-time.Duration(run.RetentionMinutes) * time.Minute)
		for _, wr := range completed {
			if wr.Status.CompletionTime != nil && wr.Status.CompletionTime.Time.Before(cutoffRetention) {
				if err := r.Delete(ctx, wr); err != nil {
					return false, fmt.Errorf("delete run %s for retention: %w", wr.Name, err)
				}
				if wr.UID == current.UID {
					return true, nil
				}
			}
		}
		// Re-list after retention deletes so maxAllowed operates on current state
		completed = r.listCompletedRunsForWorkflow(ctx, namespace, refName, refNamespace)
	}

	// 2. MaxAllowed: keep only the most recent maxAllowed completed runs; delete oldest first
	if run.MaxAllowed > 0 && len(completed) > int(run.MaxAllowed) {
		sort.Slice(completed, func(i, j int) bool {
			ti, tj := time.Time{}, time.Time{}
			if completed[i].Status.CompletionTime != nil {
				ti = completed[i].Status.CompletionTime.Time
			}
			if completed[j].Status.CompletionTime != nil {
				tj = completed[j].Status.CompletionTime.Time
			}
			return ti.Before(tj)
		})
		toDelete := len(completed) - int(run.MaxAllowed)
		for i := 0; i < toDelete && i < len(completed); i++ {
			wr := completed[i]
			if err := r.Delete(ctx, wr); err != nil {
				return false, fmt.Errorf("delete run %s for maxAllowed: %w", wr.Name, err)
			}
			if wr.UID == current.UID {
				return true, nil
			}
		}
	}

	return false, nil
}

func (r *WorkflowRunReconciler) listCompletedRunsForWorkflow(ctx context.Context, namespace, refName, refNamespace string) []*ottoflowv1alpha1.WorkflowRun {
	var list ottoflowv1alpha1.WorkflowRunList
	if err := r.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil
	}
	var completed []*ottoflowv1alpha1.WorkflowRun
	for i := range list.Items {
		wr := &list.Items[i]
		if wr.Spec.WorkflowRef.Name != refName {
			continue
		}
		wrRefNs := wr.Spec.WorkflowRef.Namespace
		if wrRefNs == "" {
			wrRefNs = namespace
		}
		if wrRefNs != refNamespace {
			continue
		}
		if wr.Status.Phase != ottoflowv1alpha1.WorkflowRunPhaseSucceeded && wr.Status.Phase != ottoflowv1alpha1.WorkflowRunPhaseFailed {
			continue
		}
		completed = append(completed, wr)
	}
	return completed
}

// SetupWithManager sets up the controller with the Manager.
func (r *WorkflowRunReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&ottoflowv1alpha1.WorkflowRun{}).
		Owns(&batchv1.Job{}).
		Complete(r)
}

// checkpointingEnabledForRun returns true when checkpointing is enabled for this WorkflowRun.
// The workflow parameter is reserved for future Workflow.Spec.Execution.OnRestart defaults.
func checkpointingEnabledForRun(workflow *ottoflowv1alpha1.Workflow, workflowRun *ottoflowv1alpha1.WorkflowRun) bool {
	_ = workflow // future: read Workflow.Spec.Execution.OnRestart defaults here
	return workflowRun != nil && workflowRun.CheckpointingEnabled()
}

// maxRestartAttemptsForRun returns the maximum number of runner Job retries for this run.
// Default is 3. The initial attempt counts as attempt 0, so we retry up to MaxRestartAttempts times.
func maxRestartAttemptsForRun(workflowRun *ottoflowv1alpha1.WorkflowRun) int32 {
	const defaultMax = int32(3)
	if workflowRun == nil || workflowRun.Spec.Execution == nil ||
		workflowRun.Spec.Execution.Checkpointing == nil ||
		workflowRun.Spec.Execution.Checkpointing.MaxRestartAttempts == nil {
		return defaultMax
	}
	return *workflowRun.Spec.Execution.Checkpointing.MaxRestartAttempts
}

// getPodTerminationReason inspects the most recently terminated pod for the given Job
// and returns its container termination reason (e.g. "OOMKilled", "Evicted", "Error").
// Returns empty string if no terminated container state is found.
func getPodTerminationReason(ctx context.Context, k8sClient client.Client, namespace, jobName string) string {
	podList := &corev1.PodList{}
	if err := k8sClient.List(ctx, podList,
		client.InNamespace(namespace),
		client.MatchingLabels{"job-name": jobName},
	); err != nil {
		log.FromContext(ctx).Error(err, "failed to list pods for termination reason", "namespace", namespace, "jobName", jobName)
		return "UnknownTerminationReason"
	}

	var latestPod *corev1.Pod
	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
			if latestPod == nil || pod.CreationTimestamp.After(latestPod.CreationTimestamp.Time) {
				latestPod = pod
			}
		}
	}
	if latestPod == nil {
		return ""
	}

	// Check pod-level reason first (covers Evicted)
	if latestPod.Status.Reason != "" {
		return latestPod.Status.Reason
	}

	// Check container lastState
	for _, cs := range latestPod.Status.ContainerStatuses {
		if cs.LastTerminationState.Terminated != nil && cs.LastTerminationState.Terminated.Reason != "" {
			return cs.LastTerminationState.Terminated.Reason
		}
	}
	// Fall back to current terminated state
	for _, cs := range latestPod.Status.ContainerStatuses {
		if cs.State.Terminated != nil && cs.State.Terminated.Reason != "" {
			return cs.State.Terminated.Reason
		}
	}
	return ""
}

// isDeterministicTermination returns true for termination reasons that indicate
// a non-retryable failure — retrying would produce the same result.
//
// "Error" (generic non-zero container exit) is intentionally NOT listed here:
// a runner panic from a transient apiserver disconnect also surfaces as "Error",
// so pod-level retry absorbs infra blips. Step-level retry config should be used
// for application-layer failures. To change this behaviour, expose the whitelist as
// a CheckpointingConfig field.
func isDeterministicTermination(reason string) bool {
	switch reason {
	case "OOMKilled", "ContainerCannotRun", "CreateContainerError", "CreateContainerConfigError",
		"DeadlineExceeded":
		return true
	default:
		return false
	}
}

// handleFailedJob processes a failed runner Job.
// Returns (result, handled, err): when handled is true, the caller should return (result, err).
func (r *WorkflowRunReconciler) handleFailedJob(ctx context.Context, workflowRun *ottoflowv1alpha1.WorkflowRun, workflow *ottoflowv1alpha1.Workflow, job *batchv1.Job, jobName string) (ctrl.Result, bool, error) {
	terminationReason := ""
	attempts := int32(0)
	if workflowRun.Status.Execution != nil {
		attempts = workflowRun.Status.Execution.Attempts
	}

	if checkpointingEnabledForRun(workflow, workflowRun) {
		terminationReason = getPodTerminationReason(ctx, r.Client, workflowRun.Namespace, jobName)
		maxAttempts := maxRestartAttemptsForRun(workflowRun)
		if !isDeterministicTermination(terminationReason) && attempts < maxAttempts {
			return r.retryTransientFailure(ctx, workflowRun, job, terminationReason, attempts, maxAttempts)
		}
	}

	return r.markRunTerminallyFailed(ctx, workflowRun, jobName, terminationReason, attempts)
}

// setRunFailed transitions a WorkflowRun to the terminal Failed phase with the given message and
// always clears any pending callback. This enforces the invariant that a run in phase Failed never
// keeps a live callback endpoint: the HTTP callback server admits a POST based only on
// Status.PendingCallback (token + expiry) and never checks Status.Phase, so a Failed run that still
// carried a PendingCallback would keep accepting callbacks that can never be consumed (a dead run
// whose endpoint still returns 200). Every terminal Failed transition must go through this helper.
// It deliberately does NOT touch Execution, CompletionTime, or step statuses — call sites set those
// as needed. It must NOT be used on the transient-retry path (retryTransientFailure), which keeps
// PendingCallback so the recreated runner can consume the delivered callback outputs.
//
// reason is the classified cause recorded in Status.FailureReason. "" leaves the field unset,
// which is correct for any failure without a well-known classification; every current caller
// reports such a failure, so every call passes "".
//
//nolint:unparam // every current caller passes "" for reason; see the note above.
func setRunFailed(workflowRun *ottoflowv1alpha1.WorkflowRun, message string, reason ottoflowv1alpha1.WorkflowRunFailureReason) {
	workflowRun.Status.Phase = ottoflowv1alpha1.WorkflowRunPhaseFailed
	workflowRun.Status.Message = message
	workflowRun.Status.FailureReason = reason
	workflowRun.Status.PendingCallback = nil
}

// markRunTerminallyFailed marks a WorkflowRun as Failed and cleans up the checkpoint ConfigMap.
func (r *WorkflowRunReconciler) markRunTerminallyFailed(ctx context.Context, workflowRun *ottoflowv1alpha1.WorkflowRun, jobName, terminationReason string, attempts int32) (ctrl.Result, bool, error) {
	now := metav1.Now()
	var msg string
	switch {
	case isDeterministicTermination(terminationReason):
		msg = fmt.Sprintf("Runner Job %s failed with non-retryable reason: %s", jobName, terminationReason)
	case terminationReason != "":
		msg = fmt.Sprintf("Runner Job %s failed (reason: %s, attempts: %d)", jobName, terminationReason, attempts+1)
	default:
		msg = fmt.Sprintf("Runner Job %s failed", jobName)
	}
	setRunFailed(workflowRun, msg, "")
	workflowRun.Status.CompletionTime = &now
	if workflowRun.Status.Execution == nil {
		workflowRun.Status.Execution = &ottoflowv1alpha1.WorkflowRunExecutionStatus{}
	}
	workflowRun.Status.Execution.Phase = string(ottoflowv1alpha1.WorkflowRunPhaseFailed)
	workflowRun.Status.Execution.JobName = jobName
	workflowRun.Status.Execution.Message = msg
	workflowRun.Status.Execution.CompletionTime = &now
	workflowRun.Status.Execution.LastTerminationReason = terminationReason
	// Delete before update: prevents a racing new runner from reading a stale checkpoint
	// for a run the controller is about to mark Failed.
	executor.DeleteCheckpointForRun(ctx, r.Client, workflowRun)
	if err := r.Status().Update(ctx, workflowRun); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, true, nil
		}
		return ctrl.Result{}, true, err
	}
	return ctrl.Result{}, true, nil
}

// retryTransientFailure deletes the failed Job and resets status so the reconciler creates a fresh one.
func (r *WorkflowRunReconciler) retryTransientFailure(ctx context.Context, workflowRun *ottoflowv1alpha1.WorkflowRun, job *batchv1.Job, terminationReason string, attempts, maxAttempts int32) (ctrl.Result, bool, error) {
	if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, true, err
	}
	if workflowRun.Status.Execution == nil {
		workflowRun.Status.Execution = &ottoflowv1alpha1.WorkflowRunExecutionStatus{}
	}
	workflowRun.Status.Execution.Attempts = attempts + 1
	workflowRun.Status.Execution.LastTerminationReason = terminationReason
	workflowRun.Status.Execution.Message = fmt.Sprintf("Runner pod failed (reason: %s), retrying (attempt %d/%d)", terminationReason, attempts+1, maxAttempts)
	// Reset phase fields consistently so observers don't see contradictory (Failed/Running + Pending) state.
	workflowRun.Status.Phase = ottoflowv1alpha1.WorkflowRunPhasePending
	workflowRun.Status.CompletionTime = nil
	workflowRun.Status.Execution.Phase = string(ottoflowv1alpha1.WorkflowRunPhasePending)
	workflowRun.Status.Execution.CompletionTime = nil
	if err := r.Status().Update(ctx, workflowRun); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, true, nil
		}
		return ctrl.Result{}, true, err
	}
	return ctrl.Result{Requeue: true}, true, nil
}

// warnCheckpointForEach logs a warning when checkpointing is enabled on a workflow containing
// ForEach steps. ForEach inner items are not checkpointed at the item level, so a crash
// mid-iteration replays all items from the start.
func warnCheckpointForEach(ctx context.Context, workflow *ottoflowv1alpha1.Workflow, workflowRun *ottoflowv1alpha1.WorkflowRun) {
	if !workflowRun.CheckpointingEnabled() {
		return
	}
	logger := log.FromContext(ctx)
	for _, step := range workflow.Spec.Steps {
		if step.ForEach != nil {
			logger.Info("checkpointing enabled with ForEach step: inner items are not checkpointed and will replay from the beginning if the runner pod crashes mid-iteration",
				logging.KeyWorkflow, workflowRun.Spec.WorkflowRef.Name, "step", step.Name)
			return
		}
	}
}

// reconcilePendingCallback handles the WaitForCallback case:
//   - If the token has expired, apply failurePolicy: Fail marks the step Failed and fails the
//     run; Continue leaves the step's callback armed and recreates the runner Job so the executor
//     resumes it with empty outputs.
//   - If outputs have been set by the callback handler, recreate the runner Job so it can resume.
//   - Otherwise requeue to re-check at expiry time.
//
// Returns (result, proceed, err): when proceed is true, the caller falls through to the normal
// Job-creation block instead of returning immediately. This is what lets a resumed run's runner
// Job actually get (re)created — previously this function never fell through, so once
// PendingCallback was set, every reconcile was permanently routed here and no Job was ever
// created after the old one was deleted (nirmata/ottoflow#18).
func (r *WorkflowRunReconciler) reconcilePendingCallback(ctx context.Context, req ctrl.Request, workflow *ottoflowv1alpha1.Workflow, workflowRun *ottoflowv1alpha1.WorkflowRun) (ctrl.Result, bool, error) {
	logger := log.FromContext(ctx)
	cb := workflowRun.Status.PendingCallback
	now := time.Now()

	// Check outputs FIRST: a callback arriving just before expiry must not be lost by a racing timeout check.
	if len(cb.Outputs.Raw) > 0 {
		return r.resumeRunnerJob(ctx, req, workflow, workflowRun)
	}

	// Emit CallbackTimeout event and apply failurePolicy
	if now.Unix() > cb.ExpiresAt {
		failurePolicy := findStepFailurePolicy(workflow, cb.StepName)

		// The event fires unconditionally regardless of failurePolicy; only the log level and
		// the resulting action differ below.
		if r.EventRecorder != nil {
			r.EventRecorder.Eventf(workflowRun, nil, corev1.EventTypeWarning, "CallbackTimeout",
				"CallbackTimeout", "Callback timeout for step %q", cb.StepName)
		}

		if failurePolicy == ottoflowv1alpha1.FailurePolicyContinue {
			// Do NOT clear PendingCallback and do NOT mark the step here: leaving PendingCallback
			// set is required so resumeRunnerJob recreates the runner Job, whose executor applies
			// the Continue recovery path on resume (empty outputs → step Succeeded → downstream
			// steps run). Clearing it here would mean the recreated runner never re-arms.
			logger.V(1).Info("waitForCallback: token expired, applying Continue; resuming run", "step", cb.StepName)
			return r.resumeRunnerJob(ctx, req, workflow, workflowRun)
		}

		logger.Info("waitForCallback: token expired, applying failurePolicy Fail",
			logging.KeyWorkflowRun, req.Name, "step", cb.StepName)

		if workflowRun.Status.StepStatuses == nil {
			workflowRun.Status.StepStatuses = make(map[string]ottoflowv1alpha1.StepStatus)
		}
		ss := workflowRun.Status.StepStatuses[cb.StepName]
		ss.Phase = ottoflowv1alpha1.StepPhaseFailed
		ss.Error = "callback timeout: no callback received within the configured timeout"
		ss.Message = ss.Error
		workflowRun.Status.StepStatuses[cb.StepName] = ss
		setRunFailed(workflowRun, fmt.Sprintf("Step %q timed out waiting for callback", cb.StepName), "")
		if err := r.Status().Update(ctx, workflowRun); err != nil {
			return ctrl.Result{}, false, err
		}
		return ctrl.Result{}, false, nil
	}

	// Still waiting: requeue at expiry time
	requeueIn := time.Until(time.Unix(cb.ExpiresAt, 0))
	if requeueIn < 0 {
		requeueIn = 0
	}
	logger.V(1).Info("waitForCallback: still waiting for callback",
		logging.KeyWorkflowRun, req.Name, "step", cb.StepName,
		"requeueIn", requeueIn)
	return ctrl.Result{RequeueAfter: requeueIn + time.Second}, false, nil
}

// resumeRunnerJob recreates (or waits out) the runner Job so a paused WorkflowRun can resume after
// a callback was delivered or a Continue failurePolicy applied on timeout. Both outcomes converge
// on the same action: mark the run Running (so the recreated runner restores from checkpoint via
// loadCheckpointIfNeeded instead of re-running completed steps) and proceed to Job creation.
// PendingCallback is intentionally left set for the executor to consume.
func (r *WorkflowRunReconciler) resumeRunnerJob(ctx context.Context, req ctrl.Request, workflow *ottoflowv1alpha1.Workflow, workflowRun *ottoflowv1alpha1.WorkflowRun) (ctrl.Result, bool, error) {
	logger := log.FromContext(ctx)
	cb := workflowRun.Status.PendingCallback
	stepName := ""
	if cb != nil {
		stepName = cb.StepName
	}

	ensureRunning := func() error {
		if workflowRun.Status.Phase != ottoflowv1alpha1.WorkflowRunPhaseRunning {
			workflowRun.Status.Phase = ottoflowv1alpha1.WorkflowRunPhaseRunning
			if uErr := r.Status().Update(ctx, workflowRun); uErr != nil {
				return uErr
			}
		}
		return nil
	}

	jobName := workflowRunnerJobName(workflowRun.Name)
	jobKey := types.NamespacedName{Name: jobName, Namespace: workflowRun.Namespace}
	job := &batchv1.Job{}
	err := r.Get(ctx, jobKey, job)
	switch {
	case apierrors.IsNotFound(err):
		// Old paused Job gone (or none) — ready to resume.
		if err := ensureRunning(); err != nil {
			return ctrl.Result{}, false, err
		}
		return ctrl.Result{}, true, nil // proceed → fall through to the creation block
	case err != nil:
		return ctrl.Result{}, false, err
	case jobConditionTrue(job, batchv1.JobComplete) || job.Status.Succeeded > 0:
		// Old paused runner Job exited 0 (Complete). Delete so we can recreate the same name.
		// Checked before the failure case: a Job that succeeded after an earlier pod retry
		// (backoffLimit>0) reports both Complete and Failed>0, and "completed" must win.
		logger.Info("waitForCallback: recreating runner Job to resume",
			logging.KeyWorkflowRun, req.Name, "step", stepName)
		if job.DeletionTimestamp == nil {
			if dErr := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationForeground)); dErr != nil && !apierrors.IsNotFound(dErr) {
				logger.Error(dErr, "failed to delete old runner Job for callback resume")
				return ctrl.Result{}, false, dErr
			}
		}
		return ctrl.Result{RequeueAfter: 2 * time.Second}, false, nil
	case jobConditionTrue(job, batchv1.JobFailed):
		// A recreated resume Job terminally failed (backoffLimit exhausted). Gated on the
		// JobFailed *condition*, not job.Status.Failed>0, so an in-flight retry is not misread
		// as terminal failure. Route through the standard failure path (attempt cap + terminal
		// fail) to avoid a hot delete/recreate loop.
		res, _, hfErr := r.handleFailedJob(ctx, workflowRun, workflow, job, jobName)
		return res, false, hfErr
	default:
		// Job exists and is still active ⇒ resume already in progress. Do NOT proceed: falling
		// through to the normal Job-creation block would hit its crude job.Status.Failed>0 check
		// instead of the JobFailed-*condition* gate above, so monitoring stays here where that
		// gate is authoritative for backoffLimit>0 (a transient first-pod failure sets Failed>0
		// with no JobFailed condition yet).
		if err := ensureRunning(); err != nil {
			return ctrl.Result{}, false, err
		}
		return ctrl.Result{RequeueAfter: 2 * time.Second}, false, nil
	}
}

// findStepFailurePolicy looks up the failurePolicy for a step by name in the given Workflow.
// Pure in-memory lookup: takes the already-resolved Workflow instead of re-fetching it, so it
// respects whatever namespace resolution (including the ControllerNamespace fallback) the caller
// used to obtain it. Returns FailurePolicyFail if workflow is nil, the step is not found, or the
// step has no policy set.
func findStepFailurePolicy(workflow *ottoflowv1alpha1.Workflow, stepName string) string {
	if workflow == nil {
		return ottoflowv1alpha1.FailurePolicyFail
	}
	for _, step := range workflow.Spec.Steps {
		if step.Name == stepName && step.WaitForCallback != nil && step.WaitForCallback.FailurePolicy != "" {
			return step.WaitForCallback.FailurePolicy
		}
	}
	return ottoflowv1alpha1.FailurePolicyFail
}
