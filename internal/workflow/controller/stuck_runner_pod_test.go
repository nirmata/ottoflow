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
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

// fakeClientWithEventIndex builds a fake client with the field indexes detectStuckRunnerPod's
// event List needs ("involvedObject.name"/".kind"): the controller-runtime fake client only
// serves a field selector for an explicitly registered index.
func fakeClientWithEventIndex(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().WithScheme(unitTestScheme).
		WithIndex(&corev1.Event{}, "involvedObject.name", func(o client.Object) []string {
			return []string{o.(*corev1.Event).InvolvedObject.Name}
		}).
		WithIndex(&corev1.Event{}, "involvedObject.kind", func(o client.Object) []string {
			return []string{o.(*corev1.Event).InvolvedObject.Kind}
		}).
		WithObjects(objs...).Build()
}

// failedMountEvent returns a FailedMount Event referencing the named pod in defaultNamespace —
// every caller in this package operates in defaultNamespace, so the namespace is not a parameter.
func failedMountEvent(podName string) *corev1.Event {
	return &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: podName + "-evt", Namespace: defaultNamespace},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: podName, Namespace: defaultNamespace},
		Reason:         "FailedMount",
		Message:        `MountVolume.SetUp failed for volume "secret-0": secret "missing" not found`,
	}
}

func TestDetectStuckRunnerPod_CreateContainerConfigError(t *testing.T) {
	ns := defaultNamespace
	r := &WorkflowRunReconciler{Client: fake.NewClientBuilder().WithScheme(unitTestScheme).Build(), Scheme: unitTestScheme}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "run-1-runner-abcde",
			Namespace:         ns,
			CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Minute)),
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "workflow-runner",
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason:  "CreateContainerConfigError",
							Message: `couldn't find key token in Secret default/agent-token`,
						},
					},
				},
			},
		},
	}
	msg, reason := r.detectStuckRunnerPod(context.Background(), pod)
	if msg == "" {
		t.Fatal("expected a non-empty stuck-pod message")
	}
	if !strings.Contains(msg, "CreateContainerConfigError") {
		t.Errorf("expected message to name the reason, got: %s", msg)
	}
	// The kubelet's own message is the part that actually says WHICH ref is broken; the
	// wrapper text must surface it, and must not over-claim "Secret" as the only cause —
	// CreateContainerConfigError fires for missing ConfigMaps/keys too.
	if !strings.Contains(msg, "couldn't find key token in Secret default/agent-token") {
		t.Errorf("expected message to surface the kubelet's own message, got: %s", msg)
	}
	if !strings.Contains(msg, "ConfigMap") {
		t.Errorf("expected diagnosis to cover ConfigMap refs too, got: %s", msg)
	}
	// CreateContainerConfigError IS a reference-resolution failure (unlike CreateContainerError,
	// see TestDetectStuckRunnerPod_CreateContainerError_ReasonEmpty below).
	if reason != ottoflowv1alpha1.WorkflowRunFailureReasonRunnerRefUnresolved {
		t.Errorf("expected reason %q, got %q", ottoflowv1alpha1.WorkflowRunFailureReasonRunnerRefUnresolved, reason)
	}
}

// TestDetectStuckRunnerPod_CreateContainerConfigError_UnrelatedCause_ReasonEmpty proves
// CreateContainerConfigError is NOT unconditionally classified RunnerRefUnresolved: the kubelet
// also returns this Reason for container-configuration failures that name no Secret/ConfigMap at
// all (e.g. a runAsNonRoot securityContext violation), and reporting RunnerRefUnresolved there
// would point an operator at RBAC/reference fixes that cannot help.
func TestDetectStuckRunnerPod_CreateContainerConfigError_UnrelatedCause_ReasonEmpty(t *testing.T) {
	ns := defaultNamespace
	r := &WorkflowRunReconciler{Client: fake.NewClientBuilder().WithScheme(unitTestScheme).Build(), Scheme: unitTestScheme}
	pod := waitingReasonPod(ns, "run-1-runner-abcde", "CreateContainerConfigError",
		"container has runAsNonRoot and image will run as root")
	msg, reason := r.detectStuckRunnerPod(context.Background(), pod)
	if msg == "" {
		t.Fatal("expected a non-empty stuck-pod message")
	}
	if reason != "" {
		t.Errorf("expected an empty reason (message names no Secret/ConfigMap), got %q", reason)
	}
}

// TestDetectStuckRunnerPod_CreateContainerError_ReasonEmpty proves CreateContainerError is still
// DETECTED as stuck (it wedges the pod in Waiting exactly like CreateContainerConfigError does),
// but is NOT classified as RunnerRefUnresolved: it is returned by the container runtime's
// CreateContainer call for reasons unrelated to Secret/ConfigMap reference resolution, so
// misclassifying it would point an operator at the wrong fix.
func TestDetectStuckRunnerPod_CreateContainerError_ReasonEmpty(t *testing.T) {
	ns := defaultNamespace
	r := &WorkflowRunReconciler{Client: fake.NewClientBuilder().WithScheme(unitTestScheme).Build(), Scheme: unitTestScheme}
	pod := createContainerErrorPod(ns, "run-1-runner-abcde")
	msg, reason := r.detectStuckRunnerPod(context.Background(), pod)
	if msg == "" {
		t.Fatal("expected a non-empty stuck-pod message for CreateContainerError")
	}
	if reason != "" {
		t.Errorf("expected an empty reason for CreateContainerError (not a reference-resolution failure), got %q", reason)
	}
}

func TestDetectStuckRunnerPod_WithinGracePeriod_NotStuck(t *testing.T) {
	ns := defaultNamespace
	r := &WorkflowRunReconciler{Client: fake.NewClientBuilder().WithScheme(unitTestScheme).Build(), Scheme: unitTestScheme}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "run-1-runner-abcde",
			Namespace:         ns,
			CreationTimestamp: metav1.NewTime(time.Now()), // just created
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:  "workflow-runner",
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CreateContainerConfigError"}},
				},
			},
		},
	}
	if msg, _ := r.detectStuckRunnerPod(context.Background(), pod); msg != "" {
		t.Errorf("expected no stuck-pod message within the grace period, got: %s", msg)
	}
}

// TestWorkflowRunReconciler_Reconcile_StuckPod_FailsRunWithActionableMessage exercises the
// full reconcile path: a WorkflowRun whose runner pod is wedged in CreateContainerConfigError
// past the grace period (simulating an ungranted/missing secret ref discovered only after the
// Job was built) must reach a terminal Failed state with an actionable message — not stay
// "Running" forever, since job.Status.Failed never increments for a pod that never actually
// fails.
func TestWorkflowRunReconciler_Reconcile_StuckPod_FailsRunWithActionableMessage(t *testing.T) {
	ctx := context.Background()
	ns := defaultNamespace
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowSpec{Steps: []ottoflowv1alpha1.Step{{Name: "s1", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `"ok"`}}}}},
	}
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: ns}},
		Status: ottoflowv1alpha1.WorkflowRunStatus{
			Phase:     ottoflowv1alpha1.WorkflowRunPhaseRunning,
			Execution: &ottoflowv1alpha1.WorkflowRunExecutionStatus{JobName: "run-1-runner", Phase: "Running"},
		},
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1-runner", Namespace: ns},
		Status:     batchv1.JobStatus{Active: 1},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "run-1-runner-abcde",
			Namespace:         ns,
			Labels:            map[string]string{"job-name": "run-1-runner"},
			CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Minute)),
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "workflow-runner",
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason:  "CreateContainerConfigError",
							Message: "couldn't find key token in Secret default/agent-token",
						},
					},
				},
			},
		},
	}
	clusterRole := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "ottoflow-role"}}
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "controller-manager", Namespace: ns}}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(wr).
		WithObjects(wf, wr, job, pod, clusterRole, sa).Build()
	r := &WorkflowRunReconciler{
		Client:       fakeClient,
		Scheme:       unitTestScheme,
		RunnerConfig: RunnerConfig{RunnerServiceAccount: "controller-manager", RunnerClusterRole: "ottoflow-role"},
	}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: wr.Name, Namespace: ns}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got := &ottoflowv1alpha1.WorkflowRun{}
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: wr.Name, Namespace: ns}, got); err != nil {
		t.Fatalf("get WorkflowRun: %v", err)
	}
	if got.Status.Phase != ottoflowv1alpha1.WorkflowRunPhaseFailed {
		t.Fatalf("Status.Phase = %q, want Failed (run must not stay Running forever on a stuck pod)", got.Status.Phase)
	}
	if !strings.Contains(got.Status.Message, "CreateContainerConfigError") {
		t.Errorf("expected an actionable message naming the stuck reason, got: %q", got.Status.Message)
	}
}

// TestDetectStuckRunnerPod_RunningPodStaleFailedMount_NotStuck proves a healthy Running pod
// carrying a stale FailedMount event (from a mount attempt that later succeeded) is NOT failed.
func TestDetectStuckRunnerPod_RunningPodStaleFailedMount_NotStuck(t *testing.T) {
	ns := defaultNamespace
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "run-1-runner-abcde",
			Namespace:         ns,
			CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Minute)),
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	r := &WorkflowRunReconciler{
		Client: fakeClientWithEventIndex(failedMountEvent(pod.Name)),
		Scheme: unitTestScheme,
	}
	if msg, _ := r.detectStuckRunnerPod(context.Background(), pod); msg != "" {
		t.Errorf("a Running pod with a stale FailedMount event must NOT be treated as stuck, got: %s", msg)
	}
}

// TestDetectStuckRunnerPod_PendingPodFailedMount_Stuck proves a genuinely stuck Pending pod
// (past grace, with a FailedMount event) is still detected — the Pending gate must not disable
// legitimate detection.
func TestDetectStuckRunnerPod_PendingPodFailedMount_Stuck(t *testing.T) {
	ns := defaultNamespace
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "run-1-runner-abcde",
			Namespace:         ns,
			CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Minute)),
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
	r := &WorkflowRunReconciler{
		Client: fakeClientWithEventIndex(failedMountEvent(pod.Name)),
		Scheme: unitTestScheme,
	}
	msg, reason := r.detectStuckRunnerPod(context.Background(), pod)
	if msg == "" {
		t.Fatal("a Pending pod past grace with a FailedMount event must be detected as stuck")
	}
	if !strings.Contains(msg, "failed to mount") {
		t.Errorf("expected a mount-failure message, got: %s", msg)
	}
	if reason != ottoflowv1alpha1.WorkflowRunFailureReasonRunnerRefUnresolved {
		t.Errorf("expected reason %q for a FailedMount event, got %q", ottoflowv1alpha1.WorkflowRunFailureReasonRunnerRefUnresolved, reason)
	}
}

// TestDetectStuckRunnerPod_FailedMountUnrelatedVolume_ReasonEmpty proves a FailedMount Event is
// NOT unconditionally classified RunnerRefUnresolved: execution.job.volumes accepts any
// corev1.Volume, so a mount failure can be about a volume type (e.g. an unbound
// PersistentVolumeClaim) that has nothing to do with a Secret or ConfigMap reference.
func TestDetectStuckRunnerPod_FailedMountUnrelatedVolume_ReasonEmpty(t *testing.T) {
	ns := defaultNamespace
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "run-1-runner-abcde",
			Namespace:         ns,
			CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Minute)),
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
	evt := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: pod.Name + "-evt", Namespace: ns},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: pod.Name, Namespace: ns},
		Reason:         "FailedMount",
		Message:        `MountVolume.SetUp failed for volume "data-0": PersistentVolumeClaim is not bound`,
	}
	r := &WorkflowRunReconciler{
		Client: fakeClientWithEventIndex(evt),
		Scheme: unitTestScheme,
	}
	msg, reason := r.detectStuckRunnerPod(context.Background(), pod)
	if msg == "" {
		t.Fatal("expected a non-empty stuck-pod message")
	}
	if reason != "" {
		t.Errorf("expected an empty reason (message names no Secret/ConfigMap), got %q", reason)
	}
}

// waitingReasonPod returns a Pending pod, past the stuck-pod grace period, with a single
// container stuck Waiting on the given reason/message — the shared shape behind wedgedPod and
// createContainerErrorPod, which differ only in which Waiting.Reason detectStuckRunnerPod sees.
func waitingReasonPod(namespace, name, reason, message string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         namespace,
			CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Minute)),
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "workflow-runner",
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason:  reason,
							Message: message,
						},
					},
				},
			},
		},
	}
}

// wedgedPod returns a Pending pod, past the stuck-pod grace period, with a container stuck in
// CreateContainerConfigError — a symptom detectStuckRunnerPod recognizes.
func wedgedPod(namespace, name string) *corev1.Pod {
	return waitingReasonPod(namespace, name, "CreateContainerConfigError", `couldn't find key token in Secret default/agent-token`)
}

// TestHandleStuckRunnerPods_ChecksEveryPodNotJustFirst proves a Failed pod listed first
// (detectStuckRunnerPod always returns "" for it — it only inspects Pending/Running pods) must
// not shadow a genuinely wedged Pending pod listed second.
func TestHandleStuckRunnerPods_ChecksEveryPodNotJustFirst(t *testing.T) {
	ns := defaultNamespace
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: ns}},
		Status:     ottoflowv1alpha1.WorkflowRunStatus{Phase: ottoflowv1alpha1.WorkflowRunPhaseRunning},
	}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "run-1-runner", Namespace: ns}}
	failedPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1-runner-old", Namespace: ns},
		Status:     corev1.PodStatus{Phase: corev1.PodFailed},
	}
	podList := &corev1.PodList{Items: []corev1.Pod{*failedPod, *wedgedPod(ns, "run-1-runner-new")}}

	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(wr).WithObjects(wr, job).Build()
	r := &WorkflowRunReconciler{Client: fakeClient, Scheme: unitTestScheme}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: wr.Name, Namespace: ns}}

	handled, err := r.handleStuckRunnerPods(context.Background(), req, wr, job, podList)
	if !handled {
		t.Fatal("expected the wedged second pod to be detected even though the first pod is Failed (not stuck)")
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wr.Status.Phase != ottoflowv1alpha1.WorkflowRunPhaseFailed {
		t.Errorf("expected WorkflowRun phase Failed, got %q", wr.Status.Phase)
	}
}

// failingStatusWriteClient wraps a client.Client and makes every Status().Update call fail, to
// test ORDERING: does a caller persist status before or after mutating other resources?
type failingStatusWriteClient struct {
	client.Client
}

func (f *failingStatusWriteClient) Status() client.SubResourceWriter {
	return failingSubResourceWriter{}
}

type failingSubResourceWriter struct{}

func (failingSubResourceWriter) Create(context.Context, client.Object, client.Object, ...client.SubResourceCreateOption) error {
	return errors.New("simulated status create failure")
}
func (failingSubResourceWriter) Update(context.Context, client.Object, ...client.SubResourceUpdateOption) error {
	return errors.New("simulated status update failure")
}
func (failingSubResourceWriter) Patch(context.Context, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
	return errors.New("simulated status patch failure")
}
func (failingSubResourceWriter) Apply(context.Context, runtime.ApplyConfiguration, ...client.SubResourceApplyOption) error {
	return errors.New("simulated status apply failure")
}

// TestHandleStuckRunnerPod_PersistsStatusBeforeDeletingJob proves the write ordering: if
// persisting the terminal Failed status fails, the stuck Job must NOT already have been deleted — deleting
// first and then failing to persist status would leave the WorkflowRun looking Running with no
// Job, and the next reconcile would recreate it, looping unbounded (maxRestartAttemptsForRun
// only guards the checkpoint-retry path, not this one).
func TestHandleStuckRunnerPod_PersistsStatusBeforeDeletingJob(t *testing.T) {
	ns := defaultNamespace
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: ns}},
		Status:     ottoflowv1alpha1.WorkflowRunStatus{Phase: ottoflowv1alpha1.WorkflowRunPhaseRunning},
	}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "run-1-runner", Namespace: ns}}
	pod := wedgedPod(ns, "run-1-runner-abcde")

	baseClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(wr).WithObjects(wr, job).Build()
	r := &WorkflowRunReconciler{Client: &failingStatusWriteClient{Client: baseClient}, Scheme: unitTestScheme}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: wr.Name, Namespace: ns}}

	handled, err := r.handleStuckRunnerPod(context.Background(), req, wr, job, pod)
	if !handled {
		t.Fatal("expected handled=true (a stuck symptom was detected)")
	}
	if err == nil {
		t.Fatal("expected the simulated status-update failure to propagate")
	}

	got := &batchv1.Job{}
	getErr := baseClient.Get(context.Background(), types.NamespacedName{Name: "run-1-runner", Namespace: ns}, got)
	if getErr != nil {
		t.Errorf("expected the Job to still exist after a failed status update (status must persist BEFORE delete), got: %v", getErr)
	}
}

// createContainerErrorPod returns a Pending pod, past the stuck-pod grace period, with a
// container stuck in CreateContainerError — detected as stuck (it wedges the pod in Waiting
// exactly like CreateContainerConfigError), but NOT a reference-resolution failure.
func createContainerErrorPod(namespace, name string) *corev1.Pod {
	return waitingReasonPod(namespace, name, "CreateContainerError", `failed to create containerd task: OCI runtime create failed`)
}

// TestHandleStuckRunnerPod_FailureReasonClassification proves handleStuckRunnerPod persists
// RunnerRefUnresolved for the two genuine reference-resolution symptoms (FailedMount,
// CreateContainerConfigError) and leaves Status.FailureReason empty for CreateContainerError,
// even though all three are detected as "stuck" and terminally fail the run the same way.
func TestHandleStuckRunnerPod_FailureReasonClassification(t *testing.T) {
	tests := []struct {
		name       string
		buildPod   func(ns, podName string) *corev1.Pod
		withEvent  bool
		wantReason ottoflowv1alpha1.WorkflowRunFailureReason
	}{
		{
			name:       "CreateContainerConfigError classifies RunnerRefUnresolved",
			buildPod:   wedgedPod,
			wantReason: ottoflowv1alpha1.WorkflowRunFailureReasonRunnerRefUnresolved,
		},
		{
			name:       "CreateContainerError leaves reason empty",
			buildPod:   createContainerErrorPod,
			wantReason: "",
		},
		{
			name: "FailedMount event classifies RunnerRefUnresolved",
			buildPod: func(ns, name string) *corev1.Pod {
				return &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:              name,
						Namespace:         ns,
						CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Minute)),
					},
					Status: corev1.PodStatus{Phase: corev1.PodPending},
				}
			},
			withEvent:  true,
			wantReason: ottoflowv1alpha1.WorkflowRunFailureReasonRunnerRefUnresolved,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ns := defaultNamespace
			wr := &ottoflowv1alpha1.WorkflowRun{
				ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: ns},
				Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: ns}},
				Status:     ottoflowv1alpha1.WorkflowRunStatus{Phase: ottoflowv1alpha1.WorkflowRunPhaseRunning},
			}
			job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "run-1-runner", Namespace: ns}}
			pod := tt.buildPod(ns, "run-1-runner-abcde")

			var objs []client.Object
			if tt.withEvent {
				objs = append(objs, failedMountEvent(pod.Name))
			}
			// One client for both Client and APIReader (detectStuckRunnerPod's Event List goes
			// through directReader, which falls back to Client when APIReader is unset — but set
			// both explicitly to match production wiring), carrying the Event field indexes
			// detectStuckRunnerPod's FailedMount lookup needs AND the status subresource
			// r.Status().Update needs.
			fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).
				WithIndex(&corev1.Event{}, "involvedObject.name", func(o client.Object) []string {
					return []string{o.(*corev1.Event).InvolvedObject.Name}
				}).
				WithIndex(&corev1.Event{}, "involvedObject.kind", func(o client.Object) []string {
					return []string{o.(*corev1.Event).InvolvedObject.Kind}
				}).
				WithStatusSubresource(wr).
				WithObjects(append(objs, wr, job)...).
				Build()
			r := &WorkflowRunReconciler{Client: fakeClient, APIReader: fakeClient, Scheme: unitTestScheme}
			req := ctrl.Request{NamespacedName: types.NamespacedName{Name: wr.Name, Namespace: ns}}

			handled, err := r.handleStuckRunnerPod(context.Background(), req, wr, job, pod)
			if !handled {
				t.Fatal("expected handled=true (a stuck symptom was detected)")
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if wr.Status.Phase != ottoflowv1alpha1.WorkflowRunPhaseFailed {
				t.Fatalf("expected phase Failed, got %q", wr.Status.Phase)
			}
			if wr.Status.FailureReason != tt.wantReason {
				t.Errorf("expected FailureReason %q, got %q", tt.wantReason, wr.Status.FailureReason)
			}
		})
	}
}

// TestHandleStuckRunnerPod_FailedMount_AttributesGeneratedVolume proves the attribution clause:
// a Job carrying generatedSecretVolumeOriginsAnnotation, plus a Pending pod past grace with a
// FailedMount event naming a minted volume, produces a status message carrying BOTH the
// kubelet's own text and the attribution clause naming the originating workflow field — on top
// of the RunnerRefUnresolved classification and Job deletion.
//
// Dropping the `msg +=` line in handleStuckRunnerPod fails this test: the attribution clause is
// then absent from the persisted message.
func TestHandleStuckRunnerPod_FailedMount_AttributesGeneratedVolume(t *testing.T) {
	ns := defaultNamespace
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: ns}},
		Status:     ottoflowv1alpha1.WorkflowRunStatus{Phase: ottoflowv1alpha1.WorkflowRunPhaseRunning},
	}
	const origin = `step "callTool" mcpToolCall auth.secretRef`
	originsJSON, err := json.Marshal(map[string]string{"ottoflow-secret-0": origin})
	if err != nil {
		t.Fatalf("marshal origins fixture: %v", err)
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "run-1-runner",
			Namespace:   ns,
			Annotations: map[string]string{generatedSecretVolumeOriginsAnnotation: string(originsJSON)},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "run-1-runner-abcde",
			Namespace:         ns,
			CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Minute)),
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
	evt := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: pod.Name + "-evt", Namespace: ns},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: pod.Name, Namespace: ns},
		Reason:         "FailedMount",
		Message:        `MountVolume.SetUp failed for volume "ottoflow-secret-0": secret "missing" not found`,
	}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).
		WithIndex(&corev1.Event{}, "involvedObject.name", func(o client.Object) []string {
			return []string{o.(*corev1.Event).InvolvedObject.Name}
		}).
		WithIndex(&corev1.Event{}, "involvedObject.kind", func(o client.Object) []string {
			return []string{o.(*corev1.Event).InvolvedObject.Kind}
		}).
		WithStatusSubresource(wr).
		WithObjects(evt, wr, job).
		Build()
	r := &WorkflowRunReconciler{Client: fakeClient, APIReader: fakeClient, Scheme: unitTestScheme}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: wr.Name, Namespace: ns}}

	handled, herr := r.handleStuckRunnerPod(context.Background(), req, wr, job, pod)
	if !handled {
		t.Fatal("expected handled=true (a stuck symptom was detected)")
	}
	if herr != nil {
		t.Fatalf("unexpected error: %v", herr)
	}
	if wr.Status.Phase != ottoflowv1alpha1.WorkflowRunPhaseFailed {
		t.Fatalf("expected phase Failed, got %q", wr.Status.Phase)
	}
	if wr.Status.FailureReason != ottoflowv1alpha1.WorkflowRunFailureReasonRunnerRefUnresolved {
		t.Errorf("expected FailureReason %q, got %q", ottoflowv1alpha1.WorkflowRunFailureReasonRunnerRefUnresolved, wr.Status.FailureReason)
	}
	if !strings.Contains(wr.Status.Message, "failed to mount") {
		t.Errorf("expected the kubelet's own mount-failure text, got: %s", wr.Status.Message)
	}
	if !strings.Contains(wr.Status.Message, `Volume "ottoflow-secret-0" was generated for `+origin) {
		t.Errorf("expected the attribution clause naming the originating workflow field, got: %s", wr.Status.Message)
	}
	got := &batchv1.Job{}
	getErr := fakeClient.Get(context.Background(), types.NamespacedName{Name: job.Name, Namespace: ns}, got)
	if !apierrors.IsNotFound(getErr) {
		t.Errorf("expected the stuck Job to be deleted, got err=%v", getErr)
	}
}
