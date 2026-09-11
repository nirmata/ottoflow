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
	"regexp"
	"sort"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/logging"
)

// stuckPodGracePeriod is how long a runner pod may sit in a Waiting container state (or show a
// FailedMount event) before the controller treats it as a permanent failure rather than normal
// image-pull/mount startup latency.
const stuckPodGracePeriod = 30 * time.Second

// unresolvedRefWaitingReasons are container Waiting.Reason values that indicate a pod will
// never start on its own — most commonly, the kubelet could not resolve a Secret or ConfigMap
// key referenced from the pod spec. Whether a referenced Secret/ConfigMap (or key) actually
// exists is not checked when the runner Job is built: collectSecretRefs resolves the
// referencing CRs (Workflow, StepTemplate, MCPServer, Agent) and validateSecretRefPolicy
// applies the namespace policy, but neither reads the Secret itself. A Secret or key that is
// missing at pod start — or removed between Job build and pod start — therefore surfaces only
// as a container Waiting state (for an env reference) or a FailedMount Event (for a volume),
// which detectStuckRunnerPod recognises via this map and its FailedMount branch respectively.
//
// The map value is the WorkflowRunFailureReason each Waiting.Reason classifies as: presence in
// the map means "stuck" (detectStuckRunnerPod returns a non-empty message), while the value
// says whether that symptom CAN ALSO be a genuine reference-resolution failure.
// CreateContainerConfigError can be; CreateContainerError never is — it is still detected as
// stuck (the container runtime's CreateContainer call also wedges the pod in Waiting forever)
// but is returned for reasons unrelated to reference resolution, so it maps to the empty reason
// rather than being misclassified.
//
// A non-empty value here is not the final answer for CreateContainerConfigError: the kubelet
// also uses that same Reason for container-configuration failures with nothing to do with a
// Secret or ConfigMap (for example, a runAsNonRoot securityContext violation), and
// execution.job.env accepts arbitrary corev1.EnvVarSource, not just secretKeyRef/configMapKeyRef.
// detectStuckRunnerPod downgrades this value to "" unless the kubelet's own Waiting.Message
// actually names a Secret or ConfigMap (mentionsSecretOrConfigMapRef) — see its call site.
var unresolvedRefWaitingReasons = map[string]ottoflowv1alpha1.WorkflowRunFailureReason{
	"CreateContainerConfigError": ottoflowv1alpha1.WorkflowRunFailureReasonRunnerRefUnresolved,
	"CreateContainerError":       "",
}

// mentionsSecretOrConfigMapRef reports whether message — a kubelet Waiting.Message or a
// FailedMount Event Message — actually names a Secret or ConfigMap. Both CreateContainerConfigError
// and FailedMount can fire for reasons that have nothing to do with a Secret/ConfigMap reference
// (see unresolvedRefWaitingReasons and detectStuckRunnerPod's FailedMount branch), so
// classification checks the kubelet's own wording rather than assuming every occurrence of these
// Reasons is a reference-resolution failure.
func mentionsSecretOrConfigMapRef(message string) bool {
	lower := strings.ToLower(message)
	return strings.Contains(lower, "secret") || strings.Contains(lower, "configmap")
}

// handleStuckRunnerPods checks every pod in podList for the stuck-Waiting symptom
// handleStuckRunnerPod detects — not just its first element: a Job's pod list is not ordered
// to favor a wedged pod over, say, a previous attempt's already-Failed pod still lingering in
// the list (detectStuckRunnerPod immediately returns "" for a Failed-phase pod, so checking
// only the first entry can miss a genuinely wedged pod sitting elsewhere). Returns as soon as
// one pod is found stuck; see handleStuckRunnerPod's own contract for what (handled, err) means.
func (r *WorkflowRunReconciler) handleStuckRunnerPods(
	ctx context.Context,
	req ctrl.Request,
	workflowRun *ottoflowv1alpha1.WorkflowRun,
	job *batchv1.Job,
	podList *corev1.PodList,
) (bool, error) {
	for i := range podList.Items {
		if handled, err := r.handleStuckRunnerPod(ctx, req, workflowRun, job, &podList.Items[i]); handled {
			return true, err
		}
	}
	return false, nil
}

// handleStuckRunnerPod checks pod for a stuck-Waiting symptom (see detectStuckRunnerPod) and,
// if found, marks workflowRun terminally Failed and deletes the wedged Job.
// Returns (handled, err): when handled is true, the caller should return (ctrl.Result{}, err)
// immediately — this always resolves to an empty result, unlike handleFailedJob's contract,
// since there is nothing left to requeue once a stuck run has been marked terminally Failed.
func (r *WorkflowRunReconciler) handleStuckRunnerPod(
	ctx context.Context,
	req ctrl.Request,
	workflowRun *ottoflowv1alpha1.WorkflowRun,
	job *batchv1.Job,
	pod *corev1.Pod,
) (bool, error) {
	msg, reason := r.detectStuckRunnerPod(ctx, pod)
	if msg == "" {
		return false, nil
	}
	// Name the workflow field that generated any minted Secret volume this message refers to,
	// when one applies (secretVolumeAttribution, below). job is non-nil here: the only production
	// caller, handleStuckRunnerPods, is reached from reconcileJobExecution, which never passes
	// a nil Job.
	msg += secretVolumeAttribution(job.Annotations[generatedSecretVolumeOriginsAnnotation], msg)

	logger := log.FromContext(ctx)
	logger.Error(errors.New("runner pod stuck waiting"), msg,
		logging.KeyWorkflow, workflowRun.Spec.WorkflowRef.Name, logging.KeyWorkflowRun, req.Name, logging.KeyNamespace, req.Namespace)

	now := metav1.Now()
	setRunFailed(workflowRun, msg, reason)
	workflowRun.Status.CompletionTime = &now
	if workflowRun.Status.Execution == nil {
		workflowRun.Status.Execution = &ottoflowv1alpha1.WorkflowRunExecutionStatus{}
	}
	workflowRun.Status.Execution.Phase = string(ottoflowv1alpha1.WorkflowRunPhaseFailed)
	workflowRun.Status.Execution.Message = msg
	workflowRun.Status.Execution.CompletionTime = &now

	// Persist the terminal status BEFORE deleting the Job, not after: if the Job were
	// deleted first and this Update then failed, the next reconcile would see phase
	// Running with no Job and recreate it — looping unbounded, since
	// maxRestartAttemptsForRun only guards the checkpoint-retry path, not this one. With
	// status persisted first, a failed delete below just leaves the (already-Failed) run
	// with a dangling Job; the next reconcile retries the delete, which is idempotent.
	if err := r.Status().Update(ctx, workflowRun); err != nil {
		return true, err
	}

	// Delete the stuck Job: unlike a Job whose pod actually failed, a Job wedged in Waiting
	// never reaches a Kubernetes-native "finished" state, so TTLSecondsAfterFinished never
	// fires and it would otherwise sit there (with its stuck pod) forever.
	if delErr := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); delErr != nil && !apierrors.IsNotFound(delErr) {
		return true, delErr
	}
	return true, nil
}

// detectStuckRunnerPod reports an actionable message when pod shows a symptom that means it
// will never leave the Waiting state on its own: a container Waiting.Reason that names a
// resolution failure, or a FailedMount pod Event (volume mount failure — e.g. a missing Secret
// or ConfigMap). Returns "" (with an empty reason) when the pod is still within normal startup
// latency (gated by stuckPodGracePeriod, to avoid false positives during a normal image pull or
// mount setup) or shows no such symptom.
//
// The second return value classifies the matched symptom for WorkflowRunStatus.FailureReason.
// FailedMount and CreateContainerConfigError report WorkflowRunFailureReasonRunnerRefUnresolved
// only when the kubelet's own message actually names a Secret or ConfigMap
// (mentionsSecretOrConfigMapRef): both Reasons also fire for causes that have nothing to do with
// reference resolution — e.g. FailedMount for an unrelated volume type in
// execution.job.volumes (a PVC not yet bound, say), or CreateContainerConfigError for a
// runAsNonRoot securityContext violation — and reporting RunnerRefUnresolved there would point
// an operator at the wrong fix. CreateContainerError is also matched here (it too wedges the pod
// in Waiting forever) but is returned by the container runtime's CreateContainer call for
// reasons unrelated to reference resolution, so it always reports an empty reason. detectStuckRunnerPod
// decides all of this itself, at the point it already knows which symptom fired and has the
// kubelet's own message in hand, rather than handleStuckRunnerPod re-deriving it afterward.
func (r *WorkflowRunReconciler) detectStuckRunnerPod(ctx context.Context, pod *corev1.Pod) (string, ottoflowv1alpha1.WorkflowRunFailureReason) {
	if pod.Status.Phase != corev1.PodPending && pod.Status.Phase != corev1.PodRunning {
		return "", ""
	}
	if time.Since(pod.CreationTimestamp.Time) < stuckPodGracePeriod {
		return "", ""
	}

	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Waiting == nil {
			continue
		}
		if reason, stuck := unresolvedRefWaitingReasons[cs.State.Waiting.Reason]; stuck {
			// Lead with the kubelet's own message: CreateContainerConfigError fires for a
			// missing ConfigMap or ConfigMap key (e.g. a bad configMapKeyRef in
			// spec.execution.job.env) just as it does for a missing Secret, so the diagnosis
			// must not assert "Secret" as the only cause.
			msg := fmt.Sprintf(
				"runner container %q is stuck (%s: %s); this usually means a Secret or ConfigMap (or one of "+
					"their keys) referenced by the pod no longer exists — re-run the WorkflowRun to re-resolve "+
					"after fixing the reference",
				cs.Name, cs.State.Waiting.Reason, cs.State.Waiting.Message)
			// CreateContainerConfigError also fires for causes unrelated to a Secret/ConfigMap
			// ref (e.g. a runAsNonRoot securityContext violation) — see
			// unresolvedRefWaitingReasons. Only report RunnerRefUnresolved when the kubelet's
			// own message actually names one.
			if reason != "" && !mentionsSecretOrConfigMapRef(cs.State.Waiting.Message) {
				reason = ""
			}
			return msg, reason
		}
	}

	// A FailedMount event is only actionable while the pod is still Pending. A Running pod has
	// already mounted its volumes, so any FailedMount event on it is stale — left over from an
	// earlier mount attempt that later succeeded — and must not fail a healthy run. (The
	// grace-period gate above does not catch this: a long-Running pod is well past grace, yet a
	// FailedMount event from its startup can linger in the API for the event TTL.)
	if pod.Status.Phase == corev1.PodPending {
		events := &corev1.EventList{}
		// APIReader, not r.Client: a field-selector List through the manager's cached client
		// is served from an informer index, and controller-runtime returns "index with name
		// field:involvedObject.name does not exist" unless that index was registered with
		// GetFieldIndexer — SetupWithManager registers none for Event, and registering one
		// would mean watching and caching every Event in the cluster for this one check. In
		// production the cached client therefore FAILS here, and this function's error branch
		// returns "" — so a pod whose only symptom is a FailedMount Event would never be
		// detected and would stay stuck. involvedObject.name/.kind are server-supported
		// selectors, so the direct reader satisfies them without any index.
		if err := r.directReader().List(ctx, events, client.InNamespace(pod.Namespace), client.MatchingFieldsSelector{
			Selector: fields.AndSelectors(
				fields.OneTermEqualSelector("involvedObject.name", pod.Name),
				fields.OneTermEqualSelector("involvedObject.kind", "Pod"),
			),
		}); err != nil {
			log.FromContext(ctx).Error(err, "failed to list pod events for stuck-pod detection", "pod", pod.Name)
			return "", ""
		}
		for _, ev := range events.Items {
			if ev.Reason == "FailedMount" {
				msg := fmt.Sprintf(
					"runner pod %q failed to mount a volume (%s); this usually means a Secret or ConfigMap referenced "+
						"by the pod does not exist in this namespace (native Secret/ConfigMap volumes are always "+
						"same-namespace) — re-run the WorkflowRun after fixing the reference",
					pod.Name, ev.Message)
				// execution.job.volumes accepts any corev1.Volume, not just Secret/ConfigMap, so
				// a FailedMount Event can be about an unrelated volume type (e.g. an unbound
				// PVC). Only report RunnerRefUnresolved when the kubelet's own Event message
				// actually names a Secret or ConfigMap.
				var reason ottoflowv1alpha1.WorkflowRunFailureReason
				if mentionsSecretOrConfigMapRef(ev.Message) {
					reason = ottoflowv1alpha1.WorkflowRunFailureReasonRunnerRefUnresolved
				}
				return msg, reason
			}
		}
	}
	return "", ""
}

// generatedSecretVolumeNameRE extracts every ottoflow-secret-* token from a kubelet-authored
// message (a FailedMount Event or a container Waiting message) that secretVolumeAttribution can
// then look up against the origins map. Built from generatedSecretVolumePrefix, via string
// concatenation, so the two constants cannot silently drift apart.
//
// LEFT-ANCHORED: the capturing group may only start at the beginning of the message or right
// after a byte OUTSIDE [A-Za-z0-9-]. Plain `ottoflow-secret-[A-Za-z0-9-]+` (no left boundary)
// would tokenize "ottoflow-secret-1" out of an OPERATOR-supplied volume named
// "my-ottoflow-secret-1" — buildSecretMounts' reserved-prefix check
// (existingVolumeNames, secret_refs.go) only rejects a volume that STARTS WITH the prefix
// (strings.HasPrefix), so an operator volume with the prefix embedded mid-name is not prevented
// and would otherwise blame that operator volume's failure on a minted volume's workflow field.
// The left anchor closes that hole: for "my-ottoflow-secret-1", the byte immediately before
// "ottoflow" is "-", which the negated character class does not accept as a boundary (it is
// itself in [A-Za-z0-9-]), so no match starts there.
//
// Greedy on purpose: `[A-Za-z0-9-]+` consumes the longest run available, so "ottoflow-secret-10"
// captures whole, never truncated to "ottoflow-secret-1" by an earlier partial match — see
// secretVolumeAttribution's doc comment for the two substring-matching designs that get this
// wrong.
var generatedSecretVolumeNameRE = regexp.MustCompile(`(?:^|[^A-Za-z0-9-])(` + generatedSecretVolumePrefix + `[A-Za-z0-9-]+)`)

// secretVolumeAttribution maps every generated-volume name a kubelet-authored message mentions
// back to the workflow field that generated it, using originsJSON (buildSecretMounts' return,
// stashed on the Job annotation named by generatedSecretVolumeOriginsAnnotation). Returns "" when
// there is nothing to add: empty/malformed originsJSON, no message, or no token in message that
// is both matched by generatedSecretVolumeNameRE AND an exact key of the origins map.
//
// Tokenize-then-exact-lookup, not substring matching, because "ottoflow-secret-1" is a substring
// of "ottoflow-secret-10": a message naming volume "ottoflow-secret-10" with BOTH names present
// in the origins map must attribute the "-10" volume only, never also "-1" (a plain
// strings.Contains scan over every origins key blames the healthy "-1" volume too). And a single
// strings.Index-plus-boundary-check approach fails the opposite way on the kubelet's aggregate
// "unmounted volumes=[ottoflow-secret-10 ottoflow-secret-1]" form: Index returns only the FIRST
// occurrence, so the boundary check inspects "-10" only and never scans far enough to find the
// genuine, distinct "-1" clause after it — silently dropping a real attribution. Tokenizing with
// generatedSecretVolumeNameRE's greedy, left-anchored match and then requiring an EXACT map-key
// hit gets both directions right: every non-overlapping occurrence is found, each one captures
// its own full token, and a token that happens not to be a real minted volume (an unknown token,
// or one from an unrelated message) simply produces nothing rather than something wrong — the
// fail-safe direction.
//
// Hits are de-duplicated and sorted for deterministic, stable output regardless of the order the
// kubelet's own message lists volumes in.
func secretVolumeAttribution(originsJSON, message string) string {
	if originsJSON == "" || message == "" {
		return ""
	}
	var origins map[string]string
	if err := json.Unmarshal([]byte(originsJSON), &origins); err != nil {
		return ""
	}
	seen := make(map[string]struct{})
	var hits []string
	for _, m := range generatedSecretVolumeNameRE.FindAllStringSubmatch(message, -1) {
		token := m[1]
		if _, dup := seen[token]; dup {
			continue
		}
		if _, ok := origins[token]; !ok {
			continue
		}
		seen[token] = struct{}{}
		hits = append(hits, token)
	}
	if len(hits) == 0 {
		return ""
	}
	sort.Strings(hits)
	var b strings.Builder
	for _, name := range hits {
		fmt.Fprintf(&b, " Volume %q was generated for %s.", name, origins[name])
	}
	return b.String()
}
