/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"context"
	cryptohmac "crypto/hmac"
	cryptosha256 "crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/watch"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	executor "github.com/nirmata/ottoflow/internal/workflow/executor"
)

var (
	unitTestScheme = runtime.NewScheme()
)

func init() {
	utilruntime.Must(ottoflowv1alpha1.AddToScheme(unitTestScheme))
	utilruntime.Must(clientgoscheme.AddToScheme(unitTestScheme))
}

// fakeWatch implements watch.Interface for tests. Send events on Ch then close Ch to end the watch.
type fakeWatch struct {
	ch   chan watch.Event
	once sync.Once
}

func (f *fakeWatch) Stop() {
	f.once.Do(func() { close(f.ch) })
}

func (f *fakeWatch) ResultChan() <-chan watch.Event {
	return f.ch
}

// fakeWatcherFactory returns a watcher with an open channel; watchEvents blocks until Stop() closes it.
// lastOpts records the ListOptions passed to the most recent Watch call, so tests can
// inspect the computed label selector without a real dynamic client or envtest.
type fakeWatcherFactory struct {
	watcher  *fakeWatch
	lastOpts metav1.ListOptions
}

func (f *fakeWatcherFactory) Watch(_ context.Context, _ schema.GroupVersionResource, _ string, opts metav1.ListOptions) (watch.Interface, error) {
	f.watcher = &fakeWatch{ch: make(chan watch.Event)}
	f.lastOpts = opts
	return f.watcher, nil
}

func TestWorkflowRunnerJobName(t *testing.T) {
	tests := []struct {
		name     string
		runName  string
		wantPref string
	}{
		{"short", "my-run", "my-run-runner"},
		{"underscore", "my_run_name", "my-run-name-runner"},
		{"long truncate", "abcdefghijklmnopqrstuvwxyz-abcdefghijklmnopqrstuvwxyz-abc", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := workflowRunnerJobName(tt.runName)
			if len(got) > 63 {
				t.Errorf("job name must be at most 63 chars, got %d", len(got))
			}
			if tt.wantPref != "" && got != tt.wantPref {
				t.Errorf("got %q want %q", got, tt.wantPref)
			}
		})
	}
}

func TestWorkflowRunnerClusterRoleName(t *testing.T) {
	if got := workflowRunnerClusterRoleName(RunnerConfig{RunnerClusterRole: "custom-role"}); got != "custom-role" {
		t.Errorf("got %q", got)
	}
	if got := workflowRunnerClusterRoleName(RunnerConfig{}); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestAgentExecutorCallerClusterRoleName(t *testing.T) {
	if got := agentExecutorCallerClusterRoleName(RunnerConfig{}); got != "" {
		t.Errorf("got %q", got)
	}
	if got := agentExecutorCallerClusterRoleName(RunnerConfig{AgentExecutorCallerRole: "agent-caller"}); got != "agent-caller" {
		t.Errorf("got %q", got)
	}
}

func TestWorkflowRunnerRoleBindingName(t *testing.T) {
	got := workflowRunnerRoleBindingName("default", "controller-manager")
	if len(got) > 253 {
		t.Errorf("len %d", len(got))
	}
	if got != "" && (len(got) < 3 || got[:3] != "ott") {
		// should contain ottoflow-runner
		if len(got) < 15 {
			t.Errorf("unexpected short name %q", got)
		}
	}
}

func TestAgentExecutorCallerRoleBindingName(t *testing.T) {
	got := agentExecutorCallerRoleBindingName("default", "controller-manager")
	if len(got) > 253 {
		t.Errorf("len %d", len(got))
	}
}

func TestWorkflowRunUsesExplicitRunnerServiceAccount(t *testing.T) {
	if workflowRunUsesExplicitRunnerServiceAccount(nil) {
		t.Error("nil should be false")
	}
	if workflowRunUsesExplicitRunnerServiceAccount(&ottoflowv1alpha1.WorkflowRun{}) {
		t.Error("empty spec should be false")
	}
	if !workflowRunUsesExplicitRunnerServiceAccount(&ottoflowv1alpha1.WorkflowRun{
		Spec: ottoflowv1alpha1.WorkflowRunSpec{
			Execution: &ottoflowv1alpha1.WorkflowRunExecutionSpec{
				Job: &ottoflowv1alpha1.WorkflowRunJobSpec{ServiceAccountName: "my-sa"},
			},
		},
	}) {
		t.Error("explicit SA should be true")
	}
}

func TestRunnerSecretSourceNamespace(t *testing.T) {
	if got := runnerSecretSourceNamespace(RunnerConfig{}, "wf-ns"); got != "wf-ns" {
		t.Errorf("got %q", got)
	}
	if got := runnerSecretSourceNamespace(RunnerConfig{SecretSourceNamespace: "install-ns"}, "wf-ns"); got != "install-ns" {
		t.Errorf("got %q", got)
	}
}

func TestRunnerArgs(t *testing.T) {
	if len(runnerArgs(RunnerConfig{})) != 0 {
		t.Error("empty config should have no args")
	}
	want := []string{"--prometheus-url", "http://prom:9090"}
	got := runnerArgs(RunnerConfig{PrometheusURL: "http://prom:9090"})
	if len(got) != len(want) || (len(got) > 0 && got[0] != want[0]) {
		t.Errorf("got %v", got)
	}
}

func TestScheduler_NeedLeaderElection(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).Build()
	s := NewScheduler(c, logr.Discard())
	if !s.NeedLeaderElection() {
		t.Error("NeedLeaderElection should be true")
	}
}

func TestNewTriggerManager(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).Build()
	tm := NewTriggerManager(c, unitTestScheme, nil)
	if tm == nil {
		t.Fatal("NewTriggerManager returned nil")
	}
	if tm.dynamicClient != nil {
		t.Error("dynamicClient should be nil")
	}
}

func TestGetObjectKind(t *testing.T) {
	if got := getObjectKind(&corev1.Pod{}); got != "Pod" {
		t.Errorf("Pod: got %q", got)
	}
	if got := getObjectKind(&corev1.Service{}); got != "Service" {
		t.Errorf("Service: got %q", got)
	}
	if got := getObjectKind(&corev1.ConfigMap{}); got != "ConfigMap" {
		t.Errorf("ConfigMap: got %q", got)
	}
	if got := getObjectKind(&corev1.Secret{}); got != "Secret" {
		t.Errorf("Secret: got %q", got)
	}
}

func TestCountActiveWorkflowRuns(t *testing.T) {
	ctx := context.Background()
	ns := defaultNamespace
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: ns},
	}
	objs := []client.Object{
		wf,
		&ottoflowv1alpha1.WorkflowRun{
			ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: ns},
			Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: ns}},
			Status:     ottoflowv1alpha1.WorkflowRunStatus{Phase: ottoflowv1alpha1.WorkflowRunPhasePending},
		},
		&ottoflowv1alpha1.WorkflowRun{
			ObjectMeta: metav1.ObjectMeta{Name: "r2", Namespace: ns},
			Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: ns}},
			Status:     ottoflowv1alpha1.WorkflowRunStatus{Phase: ottoflowv1alpha1.WorkflowRunPhaseRunning},
		},
		&ottoflowv1alpha1.WorkflowRun{
			ObjectMeta: metav1.ObjectMeta{Name: "r3", Namespace: ns},
			Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: ns}},
			Status:     ottoflowv1alpha1.WorkflowRunStatus{Phase: ottoflowv1alpha1.WorkflowRunPhaseSucceeded},
		},
	}
	for _, o := range objs {
		if wr, ok := o.(*ottoflowv1alpha1.WorkflowRun); ok {
			wr.Labels = map[string]string{"ottoflow.nirmata.io/workflow": "wf"}
		}
	}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(objs...).Build()
	count, err := countActiveWorkflowRuns(ctx, fakeClient, wf)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Errorf("count = %d want 2", count)
	}
}

func TestScheduler_CancelActiveWorkflowRuns(t *testing.T) {
	ctx := context.Background()
	ns := defaultNamespace
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: ns},
	}
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: ns}},
		Status: ottoflowv1alpha1.WorkflowRunStatus{
			Phase: ottoflowv1alpha1.WorkflowRunPhaseRunning,
			// A live pending callback on the run being replaced: Fix 2b requires the
			// cancellation path to clear this (via setRunFailed) so a Failed run never keeps
			// accepting callbacks it can no longer consume.
			PendingCallback: &ottoflowv1alpha1.CallbackState{
				TokenHash: "irrelevant-for-this-test",
				StepName:  "gate",
				ExpiresAt: time.Now().Add(1 * time.Hour).Unix(),
				CreatedAt: time.Now().Unix(),
			},
		},
	}
	wr.Labels = map[string]string{"ottoflow.nirmata.io/workflow": "wf", "ottoflow.nirmata.io/trigger": "cron"}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(wr).WithObjects(wf, wr).Build()
	s := NewScheduler(fakeClient, logr.Discard())
	if err := s.cancelActiveWorkflowRuns(ctx, wf, logr.Discard()); err != nil {
		t.Fatal(err)
	}
	updated := &ottoflowv1alpha1.WorkflowRun{}
	if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(wr), updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.Phase != ottoflowv1alpha1.WorkflowRunPhaseFailed {
		t.Errorf("Phase = %v want Failed", updated.Status.Phase)
	}
	if updated.Status.Message == "" || !strings.Contains(updated.Status.Message, "Replaced") {
		t.Errorf("Message %q should contain Replaced", updated.Status.Message)
	}
	if updated.Status.PendingCallback != nil {
		t.Errorf("PendingCallback = %+v, want nil after cancellation", updated.Status.PendingCallback)
	}
}

func TestWorkflowRunReconciler_EnsureRunnerAccess_WithAgentExecutorCaller(t *testing.T) {
	ctx := context.Background()
	ns := defaultNamespace
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: ns},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{{Name: "s1", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `"ok"`}}}},
		},
	}
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: ns}},
		Status:     ottoflowv1alpha1.WorkflowRunStatus{Phase: ottoflowv1alpha1.WorkflowRunPhasePending},
	}
	// Pre-create ClusterRoles, SA, and main CRB so ensureRunnerAccess proceeds to ensureAgentExecutorCallerBinding
	clusterRole := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "ottoflow-role"},
	}
	agentCallerRole := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-executor-caller"},
	}
	runnerSA := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: "controller-manager", Namespace: ns, Labels: map[string]string{"app.kubernetes.io/part-of": "ottoflow"}},
	}
	mainCRBName := workflowRunnerRoleBindingName(ns, "controller-manager")
	mainCRB := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: mainCRBName, Labels: map[string]string{"app.kubernetes.io/part-of": "ottoflow"}},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "ottoflow-role"},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: "controller-manager", Namespace: ns}},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(wr).WithObjects(wf, wr, clusterRole, agentCallerRole, runnerSA, mainCRB).Build()
	r := &WorkflowRunReconciler{
		Client:        fakeClient,
		Scheme:        unitTestScheme,
		EventRecorder: nil,
		RunnerConfig: RunnerConfig{
			RunnerServiceAccount:    "controller-manager",
			RunnerClusterRole:       "ottoflow-role",
			AgentExecutorCallerRole: "agent-executor-caller",
		},
	}
	job, err := r.buildWorkflowRunnerJob(context.Background(), wr, &ottoflowv1alpha1.Workflow{})
	if err != nil {
		t.Fatal(err)
	}
	// ensureRunnerAccess creates SA, CRB, and agent-executor caller binding
	if err := r.ensureRunnerAccess(ctx, wr, nil, job.Spec.Template.Spec.ServiceAccountName, false); err != nil {
		t.Fatal(err)
	}
	sa := &corev1.ServiceAccount{}
	if err := fakeClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "controller-manager"}, sa); err != nil {
		t.Fatal(err)
	}
	crb := &rbacv1.ClusterRoleBinding{}
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: workflowRunnerRoleBindingName(ns, "controller-manager")}, crb); err != nil {
		t.Fatal(err)
	}
	callerCRB := &rbacv1.ClusterRoleBinding{}
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: agentExecutorCallerRoleBindingName(ns, "controller-manager")}, callerCRB); err != nil {
		t.Fatal(err)
	}
	if callerCRB.RoleRef.Name != "agent-executor-caller" {
		t.Errorf("caller CRB RoleRef.Name = %q", callerCRB.RoleRef.Name)
	}
}

// TestWorkflowRunReconciler_EnsureRunnerAccess_CreatesCallerBinding_WhenMainBindingAbsent guards F8: the
// main runner ClusterRoleBinding-not-found path must fall through to ensureAgentExecutorCallerBinding
// instead of returning early once the binding is created.
func TestWorkflowRunReconciler_EnsureRunnerAccess_CreatesCallerBinding_WhenMainBindingAbsent(t *testing.T) {
	ctx := context.Background()
	ns := defaultNamespace
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: ns},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{{Name: "s1", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `"ok"`}}}},
		},
	}
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: ns}},
		Status:     ottoflowv1alpha1.WorkflowRunStatus{Phase: ottoflowv1alpha1.WorkflowRunPhasePending},
	}
	// Pre-create ClusterRoles and SA, but deliberately OMIT the main runner ClusterRoleBinding so
	// ensureRunnerAccess takes the not-found/create branch for it.
	clusterRole := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "ottoflow-role"},
	}
	agentCallerRole := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-executor-caller"},
	}
	runnerSA := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: "controller-manager", Namespace: ns, Labels: map[string]string{"app.kubernetes.io/part-of": "ottoflow"}},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(wr).WithObjects(wf, wr, clusterRole, agentCallerRole, runnerSA).Build()
	r := &WorkflowRunReconciler{
		Client:        fakeClient,
		Scheme:        unitTestScheme,
		EventRecorder: nil,
		RunnerConfig: RunnerConfig{
			RunnerServiceAccount:    "controller-manager",
			RunnerClusterRole:       "ottoflow-role",
			AgentExecutorCallerRole: "agent-executor-caller",
		},
	}
	job, err := r.buildWorkflowRunnerJob(context.Background(), wr, &ottoflowv1alpha1.Workflow{})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.ensureRunnerAccess(ctx, wr, nil, job.Spec.Template.Spec.ServiceAccountName, false); err != nil {
		t.Fatal(err)
	}
	callerCRB := &rbacv1.ClusterRoleBinding{}
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: agentExecutorCallerRoleBindingName(ns, "controller-manager")}, callerCRB); err != nil {
		t.Fatalf("expected agent-executor-caller ClusterRoleBinding to be created even though the main binding was absent: %v", err)
	}
	if callerCRB.RoleRef.Name != "agent-executor-caller" {
		t.Errorf("caller CRB RoleRef.Name = %q", callerCRB.RoleRef.Name)
	}
}

// TestWorkflowRunReconciler_EnsureRunnerAccess_RecreateForbidden_ReturnsTerminalError guards F9: a
// Forbidden/Invalid error recreating the runner ClusterRoleBinding after a role migration must be
// classified as terminal, not the transient errRequeueRunnerAccess sentinel.
func TestWorkflowRunReconciler_EnsureRunnerAccess_RecreateForbidden_ReturnsTerminalError(t *testing.T) {
	ctx := context.Background()
	ns := defaultNamespace
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: ns},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{{Name: "s1", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `"ok"`}}}},
		},
	}
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: ns}},
		Status:     ottoflowv1alpha1.WorkflowRunStatus{Phase: ottoflowv1alpha1.WorkflowRunPhasePending},
	}
	runnerSA := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: "controller-manager", Namespace: ns, Labels: map[string]string{"app.kubernetes.io/part-of": "ottoflow"}},
	}
	// Existing binding is bound to the OLD role, so ensureRunnerAccess takes the delete-and-recreate
	// migration path when RunnerClusterRole below points at a different (new) role name.
	mainCRBName := workflowRunnerRoleBindingName(ns, "controller-manager")
	mainCRB := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: mainCRBName, Labels: map[string]string{"app.kubernetes.io/part-of": "ottoflow"}},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "ottoflow-old-role"},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: "controller-manager", Namespace: ns}},
	}
	baseClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(wr).WithObjects(wf, wr, runnerSA, mainCRB).Build()
	fakeClient := interceptor.NewClient(baseClient, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if crb, ok := obj.(*rbacv1.ClusterRoleBinding); ok && crb.Name == mainCRBName && crb.RoleRef.Name == "ottoflow-new-role" {
				return apierrors.NewForbidden(rbacv1.Resource("clusterrolebindings"), crb.Name, errors.New("cannot bind role"))
			}
			return c.Create(ctx, obj, opts...)
		},
	})
	r := &WorkflowRunReconciler{
		Client:        fakeClient,
		Scheme:        unitTestScheme,
		EventRecorder: nil,
		RunnerConfig: RunnerConfig{
			RunnerServiceAccount: "controller-manager",
			RunnerClusterRole:    "ottoflow-new-role",
		},
	}
	job, err := r.buildWorkflowRunnerJob(context.Background(), wr, &ottoflowv1alpha1.Workflow{})
	if err != nil {
		t.Fatal(err)
	}
	err = r.ensureRunnerAccess(ctx, wr, nil, job.Spec.Template.Spec.ServiceAccountName, false)
	if err == nil {
		t.Fatal("expected an error from ensureRunnerAccess, got nil")
	}
	if errors.Is(err, errRequeueRunnerAccess) {
		t.Fatalf("expected a terminal (non-requeue) error for a Forbidden recreate, got the transient sentinel: %v", err)
	}
}

func TestWorkflowRunReconciler_EnsureRunnerAccess_ExplicitSA_Skips(t *testing.T) {
	ctx := context.Background()
	wr := &ottoflowv1alpha1.WorkflowRun{
		Spec: ottoflowv1alpha1.WorkflowRunSpec{
			Execution: &ottoflowv1alpha1.WorkflowRunExecutionSpec{
				Job: &ottoflowv1alpha1.WorkflowRunJobSpec{ServiceAccountName: "my-sa"},
			},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).Build()
	r := &WorkflowRunReconciler{Client: fakeClient, Scheme: unitTestScheme}
	if err := r.ensureRunnerAccess(ctx, wr, nil, "my-sa", true); err != nil {
		t.Fatal(err)
	}
}

func TestWorkflowRunReconciler_EnsureRunnerSecrets_CopiesSecret(t *testing.T) {
	ctx := context.Background()
	ns := defaultNamespace
	sourceNs := "workflow-ns"
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: sourceNs},
	}
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: sourceNs}},
	}
	secretInSource := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "my-secret", Namespace: sourceNs},
		Data:       map[string][]byte{"key": []byte("value")},
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1-runner", Namespace: ns},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Volumes: []corev1.Volume{
						{Name: "vol1", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "my-secret"}}},
					},
				},
			},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(wf, wr, secretInSource).Build()
	r := &WorkflowRunReconciler{Client: fakeClient, Scheme: unitTestScheme}
	if err := r.ensureRunnerSecrets(ctx, wr, job, sourceNs); err != nil {
		t.Fatal(err)
	}
	copied := &corev1.Secret{}
	if err := fakeClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "my-secret"}, copied); err != nil {
		t.Fatal(err)
	}
	if string(copied.Data["key"]) != "value" {
		t.Errorf("copied.Data[key] = %q", copied.Data["key"])
	}
}

func TestWorkflowReconciler_Reconcile_NotFound(t *testing.T) {
	ctx := context.Background()
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).Build()
	r := &WorkflowReconciler{Client: fakeClient, Scheme: unitTestScheme}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "nonexistent"}}
	result, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != 0 {
		t.Error("Requeue should be false for not found")
	}
}

func TestWorkflowReconciler_Reconcile_Deletion_UnregistersAndInvalidates(t *testing.T) {
	ctx := context.Background()
	now := metav1.Now()
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "wf",
			Namespace:         "default",
			DeletionTimestamp: &now,
			Finalizers:        []string{"workflow.ottoflow.nirmata.io/finalizer"},
		},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{{Name: "s1", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `"ok"`}}}},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(wf).Build()
	cache, err := executor.NewCELCompilationCache(fakeClient, nil, nil, nil, logr.Discard())
	if err != nil {
		t.Fatal(err)
	}
	tm := NewTriggerManager(fakeClient, unitTestScheme, nil)
	r := &WorkflowReconciler{Client: fakeClient, Scheme: unitTestScheme, TriggerManager: tm, CELCache: cache}
	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "wf"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != 0 {
		t.Error("Requeue should be false")
	}
}

func TestWorkflowReconciler_Reconcile_RegisterTriggers(t *testing.T) {
	ctx := context.Background()
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{{Name: "s1", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `"ok"`}}}},
			Triggers: []ottoflowv1alpha1.Trigger{
				{Cron: &ottoflowv1alpha1.CronTrigger{Schedule: "0 * * * *"}},
			},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(wf).Build()
	sched := NewScheduler(fakeClient, logr.Discard())
	tm := NewTriggerManager(fakeClient, unitTestScheme, sched)
	r := &WorkflowReconciler{Client: fakeClient, Scheme: unitTestScheme, TriggerManager: tm}
	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "wf"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != 0 {
		t.Error("Requeue should be false")
	}
	key := "default/wf-cron-0"
	if !sched.HasSchedule(key) {
		t.Error("scheduler should have cron schedule")
	}
}

func TestTriggerManager_RegisterWorkflow_EventTrigger_WithWatcherFactory(t *testing.T) {
	ctx := context.Background()
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{{Name: "s1", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `"ok"`}}}},
			Triggers: []ottoflowv1alpha1.Trigger{
				{Event: &ottoflowv1alpha1.EventTrigger{
					Resources:  []ottoflowv1alpha1.EventResource{{APIVersion: "v1", Kind: "Pod", Namespace: "default"}},
					Operations: []string{"CREATE"},
				}},
			},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(wf).Build()
	factory := &fakeWatcherFactory{}
	tm := NewTriggerManagerWithWatcherFactory(fakeClient, unitTestScheme, nil, factory)
	if err := tm.RegisterWorkflow(ctx, wf); err != nil {
		t.Fatal(err)
	}
	// Unregister to stop watchers
	if err := tm.UnregisterWorkflow(ctx, wf); err != nil {
		t.Fatal(err)
	}
}

func TestTriggerManager_RegisterWorkflow_EventTrigger_InvalidAPIVersion(t *testing.T) {
	ctx := context.Background()
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{{Name: "s1", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `"ok"`}}}},
			Triggers: []ottoflowv1alpha1.Trigger{{Event: &ottoflowv1alpha1.EventTrigger{
				Resources: []ottoflowv1alpha1.EventResource{{APIVersion: "invalid/version/bad", Kind: "Pod", Namespace: "default"}},
			}}},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(wf).Build()
	factory := &fakeWatcherFactory{}
	tm := NewTriggerManagerWithWatcherFactory(fakeClient, unitTestScheme, nil, factory)
	err := tm.RegisterWorkflow(ctx, wf)
	if err == nil {
		t.Fatal("expected error for invalid APIVersion")
	}
	if !strings.Contains(err.Error(), "invalid") {
		t.Errorf("error should mention invalid: %v", err)
	}
}

func TestTriggerManager_WatchEvents_ReceivesEvent_CreatesWorkflowRun(t *testing.T) {
	ctx := context.Background()
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default", UID: "wf-uid-123"},
		Spec:       ottoflowv1alpha1.WorkflowSpec{Steps: []ottoflowv1alpha1.Step{{Name: "s1", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `"ok"`}}}}},
	}
	// watchEvents type-asserts to *unstructured.Unstructured (the dynamic client guarantee).
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata":   map[string]interface{}{"name": "p1", "namespace": "default", "uid": "pod-uid-456"},
	}}
	eventSpec := &ottoflowv1alpha1.EventTrigger{
		Resources:  []ottoflowv1alpha1.EventResource{{APIVersion: "v1", Kind: "Pod", Namespace: "default"}},
		Operations: []string{"CREATE"},
	}
	placeholderWR := &ottoflowv1alpha1.WorkflowRun{ObjectMeta: metav1.ObjectMeta{Name: "placeholder", Namespace: "default"}}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(placeholderWR).WithObjects(wf).Build()
	tm := NewTriggerManager(fakeClient, unitTestScheme, nil)

	ch := make(chan watch.Event, 2)
	ch <- watch.Event{Type: watch.Added, Object: obj}
	fw := &fakeWatch{ch: ch}

	stopChan := make(chan struct{})
	go tm.watchEvents(ctx, "test-key", wf, eventSpec, fw, stopChan)

	// Give watchEvents time to process the event
	time.Sleep(400 * time.Millisecond)

	var list ottoflowv1alpha1.WorkflowRunList
	if err := fakeClient.List(ctx, &list, client.InNamespace("default"), client.MatchingLabels{"ottoflow.nirmata.io/workflow": "wf"}); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("expected 1 WorkflowRun created by watchEvents, got %d", len(list.Items))
	}

	// Stop the watcher so watchEvents returns (defer watcher.Stop() will close ch once)
	close(stopChan)
	time.Sleep(50 * time.Millisecond)
}

func TestTriggerManager_CreateWorkflowRunFromEvent_OperationsFilter(t *testing.T) {
	ctx := context.Background()
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{{Name: "s1", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `"ok"`}}}},
		},
	}
	obj := makeUnstructured("p1", "default", nil)
	eventSpec := &ottoflowv1alpha1.EventTrigger{
		Resources:  []ottoflowv1alpha1.EventResource{{APIVersion: "v1", Kind: "Pod", Namespace: "default"}},
		Operations: []string{"UPDATE"}, // only UPDATE; we pass Added
	}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(wf).Build()
	tm := NewTriggerManager(fakeClient, unitTestScheme, nil)
	if err := tm.CreateWorkflowRunFromEvent(ctx, "test-trigger", wf, eventSpec, obj, "ADDED"); err != nil {
		t.Fatal(err)
	}
	var list ottoflowv1alpha1.WorkflowRunList
	if err := fakeClient.List(ctx, &list, client.InNamespace("default")); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 0 {
		t.Errorf("CREATE event should be ignored when only UPDATE is in Operations, got %d runs", len(list.Items))
	}
}

func TestTriggerManager_CreateWorkflowRunFromEvent_MaxConcurrentRuns(t *testing.T) {
	ctx := context.Background()
	maxConcurrent := int32(1)
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{{Name: "s1", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `"ok"`}}}},
			Run:   &ottoflowv1alpha1.RunPolicy{MaxConcurrentRuns: &maxConcurrent},
		},
	}
	existingRun := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "wf-existing", Namespace: "default"},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: "default"}},
		Status:     ottoflowv1alpha1.WorkflowRunStatus{Phase: ottoflowv1alpha1.WorkflowRunPhaseRunning},
	}
	existingRun.Labels = map[string]string{"ottoflow.nirmata.io/workflow": "wf"}
	obj := makeUnstructured("p1", "default", nil)
	eventSpec := &ottoflowv1alpha1.EventTrigger{
		Resources:  []ottoflowv1alpha1.EventResource{{APIVersion: "v1", Kind: "Pod", Namespace: "default"}},
		Operations: []string{"CREATE"},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(existingRun).WithObjects(wf, existingRun).Build()
	tm := NewTriggerManager(fakeClient, unitTestScheme, nil)
	if err := tm.CreateWorkflowRunFromEvent(ctx, "test-trigger", wf, eventSpec, obj, "ADDED"); err != nil {
		t.Fatal(err)
	}
	var list ottoflowv1alpha1.WorkflowRunList
	if err := fakeClient.List(ctx, &list, client.InNamespace("default"), client.MatchingLabels{"ottoflow.nirmata.io/workflow": "wf"}); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Errorf("should not create second run when MaxConcurrentRuns=1 and one is Running, got %d", len(list.Items))
	}
}

func TestDeepCopyExecution(t *testing.T) {
	if deepCopyExecution(nil) != nil {
		t.Error("nil in should return nil")
	}
	in := &ottoflowv1alpha1.WorkflowRunExecutionSpec{
		Job: &ottoflowv1alpha1.WorkflowRunJobSpec{Image: "img"},
	}
	out := deepCopyExecution(in)
	if out == nil {
		t.Fatal("out is nil")
	}
	if out.Job.Image != "img" {
		t.Errorf("out.Job.Image = %q", out.Job.Image)
	}
	out.Job.Image = "other"
	if in.Job.Image != "img" {
		t.Errorf("mutating out should not change in, in.Job.Image = %q", in.Job.Image)
	}
}

func TestWorkflowRunReconciler_EnsureAgentExecutorCallerBinding_UpdateExisting(t *testing.T) {
	ctx := context.Background()
	ns := defaultNamespace
	bindingName := agentExecutorCallerRoleBindingName(ns, "my-sa")
	existing := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: bindingName, Labels: map[string]string{"app.kubernetes.io/part-of": "ottoflow"}},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "old-role"},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: "my-sa", Namespace: ns}},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(existing).Build()
	r := &WorkflowRunReconciler{Client: fakeClient, Scheme: unitTestScheme}
	if err := r.ensureAgentExecutorCallerBinding(ctx, ns, "my-sa", "agent-caller"); err != nil {
		t.Fatal(err)
	}
	updated := &rbacv1.ClusterRoleBinding{}
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: bindingName}, updated); err != nil {
		t.Fatal(err)
	}
	if updated.RoleRef.Name != "agent-caller" {
		t.Errorf("RoleRef.Name = %q", updated.RoleRef.Name)
	}
}

func TestWorkflowRunReconciler_EnsureAgentExecutorCallerBinding_AlreadyCorrect_NoUpdate(t *testing.T) {
	ctx := context.Background()
	ns := defaultNamespace
	bindingName := agentExecutorCallerRoleBindingName(ns, "my-sa")
	existing := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: bindingName, Labels: map[string]string{"app.kubernetes.io/part-of": "ottoflow"}},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "agent-caller"},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: "my-sa", Namespace: ns}},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(existing).Build()
	r := &WorkflowRunReconciler{Client: fakeClient, Scheme: unitTestScheme}
	if err := r.ensureAgentExecutorCallerBinding(ctx, ns, "my-sa", "agent-caller"); err != nil {
		t.Fatal(err)
	}
	// Binding unchanged
	got := &rbacv1.ClusterRoleBinding{}
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: bindingName}, got); err != nil {
		t.Fatal(err)
	}
	if got.RoleRef.Name != "agent-caller" {
		t.Errorf("RoleRef.Name = %q", got.RoleRef.Name)
	}
}

func TestWorkflowRunReconciler_EnsureAgentExecutorCallerBinding_NotManaged_SkipsUpdate(t *testing.T) {
	ctx := context.Background()
	ns := defaultNamespace
	bindingName := agentExecutorCallerRoleBindingName(ns, "my-sa")
	existing := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: bindingName}, // no part-of label
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "other"},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: "my-sa", Namespace: ns}},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(existing).Build()
	r := &WorkflowRunReconciler{Client: fakeClient, Scheme: unitTestScheme}
	if err := r.ensureAgentExecutorCallerBinding(ctx, ns, "my-sa", "agent-caller"); err != nil {
		t.Fatal(err)
	}
	// Binding should be unchanged (we skip update when not managed)
	updated := &rbacv1.ClusterRoleBinding{}
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: bindingName}, updated); err != nil {
		t.Fatal(err)
	}
	if updated.RoleRef.Name != "other" {
		t.Errorf("should not update: RoleRef.Name = %q", updated.RoleRef.Name)
	}
}

func TestWorkflowRunReconciler_EnsureRunnerSecrets_SecretAlreadyExists(t *testing.T) {
	ctx := context.Background()
	ns := defaultNamespace
	wf := &ottoflowv1alpha1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "other-ns"}}
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: "other-ns"}},
	}
	secretInRunner := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "my-secret", Namespace: ns},
		Data:       map[string][]byte{"key": []byte("existing")},
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1-runner", Namespace: ns},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Volumes: []corev1.Volume{
						{Name: "vol1", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "my-secret"}}},
					},
				},
			},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(wf, wr, secretInRunner).Build()
	r := &WorkflowRunReconciler{Client: fakeClient, Scheme: unitTestScheme}
	if err := r.ensureRunnerSecrets(ctx, wr, job, "other-ns"); err != nil {
		t.Fatal(err)
	}
	// Secret should still have existing value (no copy overwrite)
	got := &corev1.Secret{}
	if err := fakeClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "my-secret"}, got); err != nil {
		t.Fatal(err)
	}
	if string(got.Data["key"]) != "existing" {
		t.Errorf("secret should be unchanged: %q", got.Data["key"])
	}
}

func TestWorkflowRunReconciler_BuildWorkflowRunnerJob_WithExecutionOverrides(t *testing.T) {
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "my-run", Namespace: "default"},
		Spec: ottoflowv1alpha1.WorkflowRunSpec{
			WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: "default"},
			Execution: &ottoflowv1alpha1.WorkflowRunExecutionSpec{
				Job: &ottoflowv1alpha1.WorkflowRunJobSpec{
					Image:                   "custom-img",
					ServiceAccountName:      "my-sa",
					BackoffLimit:            ptr.To(int32(3)),
					TTLSecondsAfterFinished: ptr.To(int32(100)),
					Env:                     []corev1.EnvVar{{Name: "X", Value: "y"}},
				},
			},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).Build()
	r := &WorkflowRunReconciler{Client: fakeClient, Scheme: unitTestScheme, RunnerConfig: RunnerConfig{
		AgentExecutorCASecret: "ca-secret",
		ImagePullSecrets:      "pull1,pull2",
		PodLabelsPartOf:       "my-app",
	}}
	job, err := r.buildWorkflowRunnerJob(context.Background(), wr, &ottoflowv1alpha1.Workflow{})
	if err != nil {
		t.Fatal(err)
	}
	if job.Spec.Template.Spec.Containers[0].Image != "custom-img" {
		t.Errorf("Image = %q", job.Spec.Template.Spec.Containers[0].Image)
	}
	if job.Spec.Template.Spec.ServiceAccountName != "my-sa" {
		t.Errorf("ServiceAccountName = %q", job.Spec.Template.Spec.ServiceAccountName)
	}
	if job.Spec.Template.Labels["app.kubernetes.io/part-of"] != "my-app" {
		t.Errorf("part-of = %q", job.Spec.Template.Labels["app.kubernetes.io/part-of"])
	}
	var foundCA bool
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Name == "agent-executor-ca" && v.Secret != nil && v.Secret.SecretName == "ca-secret" {
			foundCA = true
			break
		}
	}
	if !foundCA {
		t.Error("agent-executor-ca volume not found")
	}
	if len(job.Spec.Template.Spec.ImagePullSecrets) != 2 {
		t.Errorf("ImagePullSecrets len = %d", len(job.Spec.Template.Spec.ImagePullSecrets))
	}
}

func TestWorkflowRunReconciler_Reconcile_NotFound_Ignores(t *testing.T) {
	ctx := context.Background()
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).Build()
	r := &WorkflowRunReconciler{Client: fakeClient, Scheme: unitTestScheme}
	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "nonexistent", Namespace: "default"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != 0 {
		t.Error("Requeue should be false")
	}
}

func TestWorkflowRunReconciler_Reconcile_InvalidExecution_SetsFailed(t *testing.T) {
	ctx := context.Background()
	ns := defaultNamespace
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowSpec{Steps: []ottoflowv1alpha1.Step{{Name: "s1", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `"ok"`}}}}},
	}
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: ns},
		Spec: ottoflowv1alpha1.WorkflowRunSpec{
			WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: ns},
			Execution:   &ottoflowv1alpha1.WorkflowRunExecutionSpec{Job: &ottoflowv1alpha1.WorkflowRunJobSpec{BackoffLimit: ptr.To(int32(-1))}},
		},
		Status: ottoflowv1alpha1.WorkflowRunStatus{Phase: ottoflowv1alpha1.WorkflowRunPhasePending},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(wr).WithObjects(wf, wr).Build()
	r := &WorkflowRunReconciler{Client: fakeClient, Scheme: unitTestScheme}
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: wr.Name, Namespace: ns}})
	if err == nil {
		t.Error("expected error when execution is invalid")
	}
	updated := &ottoflowv1alpha1.WorkflowRun{}
	if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(wr), updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.Phase != ottoflowv1alpha1.WorkflowRunPhaseFailed {
		t.Errorf("Phase = %v", updated.Status.Phase)
	}
	if !strings.Contains(updated.Status.Message, "build runner Job") {
		t.Errorf("Message should mention build: %q", updated.Status.Message)
	}
}

func TestWorkflowRunReconciler_Reconcile_WorkflowNotFound_SetsFailed(t *testing.T) {
	ctx := context.Background()
	ns := defaultNamespace
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "nonexistent-wf", Namespace: ns}},
		Status:     ottoflowv1alpha1.WorkflowRunStatus{Phase: ottoflowv1alpha1.WorkflowRunPhasePending},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(wr).WithObjects(wr).Build()
	r := &WorkflowRunReconciler{Client: fakeClient, Scheme: unitTestScheme}
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: wr.Name, Namespace: ns}})
	// A NotFound Workflow reference is a terminal condition, not a transient one: retrying
	// forever would never make the Workflow appear, so Reconcile marks the run Failed and
	// returns a nil error so controller-runtime does not keep re-queuing a dead run.
	if err != nil {
		t.Errorf("expected no error for a terminal NotFound Workflow reference, got: %v", err)
	}
	updated := &ottoflowv1alpha1.WorkflowRun{}
	if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(wr), updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.Phase != ottoflowv1alpha1.WorkflowRunPhaseFailed {
		t.Errorf("Phase = %v", updated.Status.Phase)
	}
}

// TestWorkflowRunReconciler_Reconcile_WorkflowFetchTransientError_Requeues guards against
// treating a transient API-server error (not NotFound) the same as an unresolvable Workflow
// reference. Before this fix, getReferencedWorkflow's caller failed the run unconditionally on
// any error, so a passing API-server hiccup would permanently kill the WorkflowRun instead of
// letting controller-runtime retry with backoff.
func TestWorkflowRunReconciler_Reconcile_WorkflowFetchTransientError_Requeues(t *testing.T) {
	ctx := context.Background()
	ns := defaultNamespace
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowSpec{Steps: []ottoflowv1alpha1.Step{{Name: "s1", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `"ok"`}}}}},
	}
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: ns}},
		Status:     ottoflowv1alpha1.WorkflowRunStatus{Phase: ottoflowv1alpha1.WorkflowRunPhasePending},
	}
	baseClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(wr).WithObjects(wf, wr).Build()
	fakeClient := interceptor.NewClient(baseClient, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*ottoflowv1alpha1.Workflow); ok {
				return apierrors.NewServerTimeout(ottoflowv1alpha1.GroupVersion.WithResource("workflows").GroupResource(), "get", 1)
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	r := &WorkflowRunReconciler{Client: fakeClient, Scheme: unitTestScheme}

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: wr.Name, Namespace: ns}})
	if err == nil {
		t.Fatal("expected a non-nil error so controller-runtime requeues with backoff")
	}
	if apierrors.IsNotFound(err) {
		t.Fatalf("expected a transient error, got NotFound: %v", err)
	}

	updated := &ottoflowv1alpha1.WorkflowRun{}
	if getErr := fakeClient.Get(ctx, client.ObjectKeyFromObject(wr), updated); getErr != nil {
		t.Fatal(getErr)
	}
	if updated.Status.Phase == ottoflowv1alpha1.WorkflowRunPhaseFailed {
		t.Errorf("a transient fetch error must not fail the run; Phase = %v", updated.Status.Phase)
	}
}

func TestScheduler_HandleCronFire_MaxConcurrentRunsSkips(t *testing.T) {
	ctx := context.Background()
	ns := defaultNamespace
	maxConcurrent := int32(1)
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: ns},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{{Name: "s1", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `"ok"`}}}},
			Run:   &ottoflowv1alpha1.RunPolicy{MaxConcurrentRuns: &maxConcurrent},
		},
	}
	existingRun := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "wf-1", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: ns}},
		Status:     ottoflowv1alpha1.WorkflowRunStatus{Phase: ottoflowv1alpha1.WorkflowRunPhaseRunning},
	}
	existingRun.Labels = map[string]string{"ottoflow.nirmata.io/workflow": "wf", "ottoflow.nirmata.io/trigger": "cron"}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(existingRun).WithObjects(wf, existingRun).Build()
	s := NewScheduler(fakeClient, logr.Discard())
	s.mu.Lock()
	s.ctx = ctx
	s.workflows["key"] = wf
	s.cronSpecs["key"] = &ottoflowv1alpha1.CronTrigger{Schedule: "0 * * * *"}
	s.mu.Unlock()
	s.handleCronFire("key")
	var list ottoflowv1alpha1.WorkflowRunList
	if err := fakeClient.List(ctx, &list, client.InNamespace(ns), client.MatchingLabels{"ottoflow.nirmata.io/workflow": "wf"}); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Errorf("should not create second run when at MaxConcurrentRuns, got %d", len(list.Items))
	}
}

func TestScheduler_HandleCronFire_ReplacePolicy_CancelsAndCreates(t *testing.T) {
	ctx := context.Background()
	ns := defaultNamespace
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowSpec{Steps: []ottoflowv1alpha1.Step{{Name: "s1", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `"ok"`}}}}},
	}
	existingRun := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "wf-old", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: ns}},
		Status:     ottoflowv1alpha1.WorkflowRunStatus{Phase: ottoflowv1alpha1.WorkflowRunPhaseRunning},
	}
	existingRun.Labels = map[string]string{"ottoflow.nirmata.io/workflow": "wf", "ottoflow.nirmata.io/trigger": "cron"}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(existingRun).WithObjects(wf, existingRun).Build()
	s := NewScheduler(fakeClient, logr.Discard())
	s.mu.Lock()
	s.ctx = ctx
	s.workflows["replace-key"] = wf
	s.cronSpecs["replace-key"] = &ottoflowv1alpha1.CronTrigger{Schedule: "0 * * * *", ConcurrencyPolicy: "Replace"}
	s.mu.Unlock()
	s.handleCronFire("replace-key")
	var list ottoflowv1alpha1.WorkflowRunList
	if err := fakeClient.List(ctx, &list, client.InNamespace(ns), client.MatchingLabels{"ottoflow.nirmata.io/workflow": "wf"}); err != nil {
		t.Fatal(err)
	}
	// Old run should be failed (replaced), and one new run created
	var failed, created int
	for i := range list.Items {
		if list.Items[i].Status.Phase == ottoflowv1alpha1.WorkflowRunPhaseFailed && list.Items[i].Name == "wf-old" {
			failed++
		} else if list.Items[i].Status.Phase == ottoflowv1alpha1.WorkflowRunPhasePending {
			created++
		}
	}
	if failed != 1 {
		t.Errorf("expected 1 failed (replaced) run, got %d", failed)
	}
	if created != 1 {
		t.Errorf("expected 1 new pending run, got %d", created)
	}
}

func TestWorkflowRunReconciler_EnsureRunnerAccess_UpdatesExistingCRB(t *testing.T) {
	ctx := context.Background()
	ns := defaultNamespace
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowSpec{Steps: []ottoflowv1alpha1.Step{{Name: "s1", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `"ok"`}}}}},
	}
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: ns}},
		Status:     ottoflowv1alpha1.WorkflowRunStatus{Phase: ottoflowv1alpha1.WorkflowRunPhasePending},
	}
	clusterRole := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "ottoflow-role"}}
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: "controller-manager", Namespace: ns, Labels: map[string]string{"app.kubernetes.io/part-of": "ottoflow"}},
	}
	mainCRBName := workflowRunnerRoleBindingName(ns, "controller-manager")
	mainCRB := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: mainCRBName, Labels: map[string]string{"app.kubernetes.io/part-of": "ottoflow"}},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "wrong-role"},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: "controller-manager", Namespace: ns}},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(wr).WithObjects(wf, wr, clusterRole, sa, mainCRB).Build()
	r := &WorkflowRunReconciler{
		Client:       fakeClient,
		Scheme:       unitTestScheme,
		RunnerConfig: RunnerConfig{RunnerServiceAccount: "controller-manager", RunnerClusterRole: "ottoflow-role"},
	}
	job, _ := r.buildWorkflowRunnerJob(context.Background(), wr, &ottoflowv1alpha1.Workflow{})
	if err := r.ensureRunnerAccess(ctx, wr, nil, job.Spec.Template.Spec.ServiceAccountName, false); err != nil {
		t.Fatal(err)
	}
	updated := &rbacv1.ClusterRoleBinding{}
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: mainCRBName}, updated); err != nil {
		t.Fatal(err)
	}
	if updated.RoleRef.Name != "ottoflow-role" {
		t.Errorf("CRB RoleRef.Name = %q", updated.RoleRef.Name)
	}
}

func TestWorkflowRunReconciler_EnsureRunnerAccess_CRBExistsNotManaged_ReturnsError(t *testing.T) {
	ctx := context.Background()
	ns := defaultNamespace
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowSpec{Steps: []ottoflowv1alpha1.Step{{Name: "s1", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `"ok"`}}}}},
	}
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: ns}},
	}
	clusterRole := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "ottoflow-role"}}
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "controller-manager", Namespace: ns}}
	mainCRBName := workflowRunnerRoleBindingName(ns, "controller-manager")
	mainCRB := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: mainCRBName}, // no part-of label - not managed by us
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "other"},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: "controller-manager", Namespace: ns}},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(wf, wr, clusterRole, sa, mainCRB).Build()
	r := &WorkflowRunReconciler{
		Client:       fakeClient,
		Scheme:       unitTestScheme,
		RunnerConfig: RunnerConfig{RunnerServiceAccount: "controller-manager", RunnerClusterRole: "ottoflow-role"},
	}
	job, _ := r.buildWorkflowRunnerJob(context.Background(), wr, &ottoflowv1alpha1.Workflow{})
	err := r.ensureRunnerAccess(ctx, wr, nil, job.Spec.Template.Spec.ServiceAccountName, false)
	if err == nil {
		t.Fatal("expected error when CRB exists but is not managed")
	}
	if !strings.Contains(err.Error(), "refusing to update") {
		t.Errorf("error should mention refusing to update: %v", err)
	}
}

func TestEnsureRunnerAccessRequiresClusterRole(t *testing.T) {
	ctx := context.Background()
	ns := defaultNamespace
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: ns}},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(wr).Build()
	r := &WorkflowRunReconciler{Client: fakeClient, Scheme: unitTestScheme, RunnerConfig: RunnerConfig{}}
	err := r.ensureRunnerAccess(ctx, wr, nil, "some-sa", false)
	if err == nil {
		t.Fatal("expected error when RunnerClusterRole is empty")
	}
	if !strings.Contains(err.Error(), "ClusterRole") {
		t.Errorf("error should mention ClusterRole: %v", err)
	}
}

func TestWorkflowRunReconciler_EnsureRunnerAccess_OwnsRunnerSAByWorkflow(t *testing.T) {
	ctx := context.Background()
	ns := defaultNamespace
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: ns, UID: "wf-uid"},
		Spec:       ottoflowv1alpha1.WorkflowSpec{Steps: []ottoflowv1alpha1.Step{{Name: "s1", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `"ok"`}}}}},
	}
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "wf-run-1", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: ns}},
	}
	clusterRole := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "ottoflow-role"}}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(wf, wr, clusterRole).Build()
	r := &WorkflowRunReconciler{
		Client:       fakeClient,
		Scheme:       unitTestScheme,
		RunnerConfig: RunnerConfig{RunnerClusterRole: "ottoflow-role"},
	}
	saName := "wf-runner"
	if err := r.ensureRunnerAccess(ctx, wr, wf, saName, false); err != nil {
		t.Fatal(err)
	}
	sa := &corev1.ServiceAccount{}
	if err := fakeClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: saName}, sa); err != nil {
		t.Fatal(err)
	}
	if len(sa.OwnerReferences) != 1 {
		t.Fatalf("expected 1 owner reference, got %d", len(sa.OwnerReferences))
	}
	owner := sa.OwnerReferences[0]
	if owner.Kind != "Workflow" || owner.Name != wf.Name || owner.Controller == nil || !*owner.Controller {
		t.Errorf("unexpected owner reference: %+v", owner)
	}
}

func TestWorkflowRunReconciler_Reconcile_JobExists_RunningUpdatesStatus(t *testing.T) {
	ctx := context.Background()
	ns := defaultNamespace
	jobName := "run-1-runner"
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowSpec{Steps: []ottoflowv1alpha1.Step{{Name: "s1", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `"ok"`}}}}},
	}
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: ns}},
		Status: ottoflowv1alpha1.WorkflowRunStatus{
			Phase:     ottoflowv1alpha1.WorkflowRunPhasePending,
			Execution: &ottoflowv1alpha1.WorkflowRunExecutionStatus{JobName: jobName, Phase: "Pending", Message: "Runner Job created"},
		},
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: ns},
		Status:     batchv1.JobStatus{Active: 1},
	}
	runnerPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1-runner-abc", Namespace: ns, Labels: map[string]string{"job-name": jobName}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img"}}},
	}
	clusterRole := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "ottoflow-role"}}
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "controller-manager", Namespace: ns}}
	mainCRB := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: workflowRunnerRoleBindingName(ns, "controller-manager"), Labels: map[string]string{"app.kubernetes.io/part-of": "ottoflow"}},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "ottoflow-role"},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: "controller-manager", Namespace: ns}},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(wr).WithObjects(wf, wr, job, runnerPod, clusterRole, sa, mainCRB).Build()
	r := &WorkflowRunReconciler{Client: fakeClient, Scheme: unitTestScheme, RunnerConfig: RunnerConfig{RunnerServiceAccount: "controller-manager", RunnerClusterRole: "ottoflow-role"}}
	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: wr.Name, Namespace: ns}})
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter <= 0 {
		t.Error("expected RequeueAfter for running job")
	}
	updated := &ottoflowv1alpha1.WorkflowRun{}
	if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(wr), updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.Execution == nil || updated.Status.Execution.Phase != string(ottoflowv1alpha1.WorkflowRunPhaseRunning) {
		t.Errorf("Execution.Phase = %v", updated.Status.Execution)
	}
	if updated.Status.Execution.PodName != "run-1-runner-abc" {
		t.Errorf("Execution.PodName = %q", updated.Status.Execution.PodName)
	}
}

func TestWorkflowRunReconciler_EnsureRunnerSecrets_SkipsEmptySecretName(t *testing.T) {
	ctx := context.Background()
	ns := defaultNamespace
	wf := &ottoflowv1alpha1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: ns}}
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: ns}},
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1-runner", Namespace: ns},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Volumes: []corev1.Volume{
						{Name: "vol1", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: ""}}},
					},
				},
			},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(wf, wr).Build()
	r := &WorkflowRunReconciler{Client: fakeClient, Scheme: unitTestScheme}
	if err := r.ensureRunnerSecrets(ctx, wr, job, ns); err != nil {
		t.Fatal(err)
	}
}

func TestWorkflowRunReconciler_EnsureRunnerSecrets_SecretNotFoundInSource_ReturnsError(t *testing.T) {
	ctx := context.Background()
	ns := defaultNamespace
	sourceNs := "other"
	wf := &ottoflowv1alpha1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: sourceNs}}
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: sourceNs}},
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1-runner", Namespace: ns},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Volumes: []corev1.Volume{
						{Name: "vol1", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "missing-secret"}}},
					},
				},
			},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(wf, wr).Build()
	r := &WorkflowRunReconciler{Client: fakeClient, Scheme: unitTestScheme}
	err := r.ensureRunnerSecrets(ctx, wr, job, sourceNs)
	if err == nil {
		t.Fatal("expected error when secret missing in source namespace")
	}
	if !strings.Contains(err.Error(), "missing-secret") {
		t.Errorf("error should mention secret: %v", err)
	}
}

func TestWorkflowReconciler_Reconcile_WithCELCacheCompileErrors(t *testing.T) {
	ctx := context.Background()
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{
				{Name: "s1", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `invalid CEL !!!`}}},
			},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(wf).Build()
	cache, err := executor.NewCELCompilationCache(fakeClient, nil, nil, nil, logr.Discard())
	if err != nil {
		t.Fatal(err)
	}
	r := &WorkflowReconciler{Client: fakeClient, Scheme: unitTestScheme, CELCache: cache}
	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "wf"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != 0 {
		t.Error("Requeue should be false")
	}
}

func TestWorkflowRunReconciler_Reconcile_AlreadySucceeded_WorkflowGone_Skips(t *testing.T) {
	ctx := context.Background()
	ns := defaultNamespace
	completed := metav1.Now()
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "deleted-wf", Namespace: ns}},
		Status:     ottoflowv1alpha1.WorkflowRunStatus{Phase: ottoflowv1alpha1.WorkflowRunPhaseSucceeded, CompletionTime: &completed},
	}
	// Workflow not in client (deleted)
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(wr).WithObjects(wr).Build()
	r := &WorkflowRunReconciler{Client: fakeClient, Scheme: unitTestScheme}
	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: wr.Name, Namespace: ns}})
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != 0 {
		t.Error("Requeue should be false")
	}
}

func TestWorkflowRunReconciler_Reconcile_AlreadySucceeded_NoRunPolicy_Skips(t *testing.T) {
	ctx := context.Background()
	ns := defaultNamespace
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowSpec{Steps: []ottoflowv1alpha1.Step{{Name: "s1", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `"ok"`}}}}},
	}
	completed := metav1.Now()
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: ns}},
		Status:     ottoflowv1alpha1.WorkflowRunStatus{Phase: ottoflowv1alpha1.WorkflowRunPhaseSucceeded, CompletionTime: &completed},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(wr).WithObjects(wf, wr).Build()
	r := &WorkflowRunReconciler{Client: fakeClient, Scheme: unitTestScheme}
	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: wr.Name, Namespace: ns}})
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != 0 {
		t.Error("Requeue should be false for completed run with no run policy")
	}
}

func TestWorkflowRunReconciler_Reconcile_AlreadySucceeded_RunPolicyZeroRetention_KeepsRun(t *testing.T) {
	ctx := context.Background()
	ns := defaultNamespace
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: ns},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{{Name: "s1", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `"ok"`}}}},
			Run:   &ottoflowv1alpha1.RunPolicy{RetentionMinutes: 0, MaxAllowed: 0},
		},
	}
	completed := metav1.Now()
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: ns}},
		Status:     ottoflowv1alpha1.WorkflowRunStatus{Phase: ottoflowv1alpha1.WorkflowRunPhaseSucceeded, CompletionTime: &completed},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(wr).WithObjects(wf, wr).Build()
	r := &WorkflowRunReconciler{Client: fakeClient, Scheme: unitTestScheme}
	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: wr.Name, Namespace: ns}})
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != 0 {
		t.Error("Requeue should be false")
	}
	got := &ottoflowv1alpha1.WorkflowRun{}
	if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(wr), got); err != nil {
		t.Fatal("run should still exist (no retention/maxAllowed)", err)
	}
	if got.Name != "run-1" {
		t.Errorf("run name = %q", got.Name)
	}
}

// TestWorkflowRunReconciler_ApplyRunPolicy_MaxAllowed_DeletesOldest exercises listCompletedRunsForWorkflow
// and applyRunPolicy with MaxAllowed: keep only 2 completed runs; oldest is deleted.
func TestWorkflowRunReconciler_ApplyRunPolicy_MaxAllowed_DeletesOldest(t *testing.T) {
	ctx := context.Background()
	ns := defaultNamespace
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: ns},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{{Name: "s1", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `"ok"`}}}},
			Run:   &ottoflowv1alpha1.RunPolicy{MaxAllowed: 2},
		},
	}
	oldTime := metav1.NewTime(metav1.Now().Add(-2 * time.Hour))
	newTime := metav1.Now()
	run1 := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-oldest", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: ns}},
		Status:     ottoflowv1alpha1.WorkflowRunStatus{Phase: ottoflowv1alpha1.WorkflowRunPhaseSucceeded, CompletionTime: &oldTime},
	}
	run2 := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-mid", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: ns}},
		Status:     ottoflowv1alpha1.WorkflowRunStatus{Phase: ottoflowv1alpha1.WorkflowRunPhaseFailed, CompletionTime: &newTime},
	}
	run3 := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-current", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: ns}},
		Status:     ottoflowv1alpha1.WorkflowRunStatus{Phase: ottoflowv1alpha1.WorkflowRunPhaseSucceeded, CompletionTime: &newTime},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(run1, run2, run3).WithObjects(wf, run1, run2, run3).Build()
	r := &WorkflowRunReconciler{Client: fakeClient, Scheme: unitTestScheme}
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: run3.Name, Namespace: ns}})
	if err != nil {
		t.Fatal(err)
	}
	// applyRunPolicy should delete run-oldest (oldest of 3 completed); run-mid and run-current kept
	if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(run1), &ottoflowv1alpha1.WorkflowRun{}); err == nil {
		t.Error("run-oldest should have been deleted by MaxAllowed")
	}
	if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(run2), &ottoflowv1alpha1.WorkflowRun{}); err != nil {
		t.Error("run-mid should still exist", err)
	}
	if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(run3), &ottoflowv1alpha1.WorkflowRun{}); err != nil {
		t.Error("run-current should still exist", err)
	}
}

// TestWorkflowRunReconciler_ApplyRunPolicy_Retention_DeletesOld exercises retention path and re-list in applyRunPolicy.
func TestWorkflowRunReconciler_ApplyRunPolicy_Retention_DeletesOld(t *testing.T) {
	ctx := context.Background()
	ns := defaultNamespace
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: ns},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{{Name: "s1", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `"ok"`}}}},
			Run:   &ottoflowv1alpha1.RunPolicy{RetentionMinutes: 60}, // delete completed older than 1h
		},
	}
	oldTime := metav1.NewTime(metav1.Now().Add(-2 * time.Hour))
	currentTime := metav1.Now()
	runOld := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-old", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: ns}},
		Status:     ottoflowv1alpha1.WorkflowRunStatus{Phase: ottoflowv1alpha1.WorkflowRunPhaseSucceeded, CompletionTime: &oldTime},
	}
	runCurrent := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-current", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: ns}},
		Status:     ottoflowv1alpha1.WorkflowRunStatus{Phase: ottoflowv1alpha1.WorkflowRunPhaseSucceeded, CompletionTime: &currentTime},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(runOld, runCurrent).WithObjects(wf, runOld, runCurrent).Build()
	r := &WorkflowRunReconciler{Client: fakeClient, Scheme: unitTestScheme}
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: runCurrent.Name, Namespace: ns}})
	if err != nil {
		t.Fatal(err)
	}
	if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(runOld), &ottoflowv1alpha1.WorkflowRun{}); err == nil {
		t.Error("run-old should have been deleted by retention")
	}
	if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(runCurrent), &ottoflowv1alpha1.WorkflowRun{}); err != nil {
		t.Error("run-current should still exist", err)
	}
}

// TestListCompletedRunsForWorkflow_Filtering exercises listCompletedRunsForWorkflow filtering by refName, refNamespace, Phase.
func TestListCompletedRunsForWorkflow_Filtering(t *testing.T) {
	ctx := context.Background()
	ns := defaultNamespace
	completed := metav1.Now()
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowSpec{Steps: []ottoflowv1alpha1.Step{{Name: "s1", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `"ok"`}}}}},
	}
	// Same workflow ref, succeeded -> included
	run1 := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: ns}},
		Status:     ottoflowv1alpha1.WorkflowRunStatus{Phase: ottoflowv1alpha1.WorkflowRunPhaseSucceeded, CompletionTime: &completed},
	}
	// Different WorkflowRef.Name -> excluded
	run2 := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-2", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "other-wf", Namespace: ns}},
		Status:     ottoflowv1alpha1.WorkflowRunStatus{Phase: ottoflowv1alpha1.WorkflowRunPhaseSucceeded, CompletionTime: &completed},
	}
	// Running -> excluded
	run3 := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-3", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: ns}},
		Status:     ottoflowv1alpha1.WorkflowRunStatus{Phase: ottoflowv1alpha1.WorkflowRunPhaseRunning},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(run1, run2, run3).WithObjects(wf, run1, run2, run3).Build()
	r := &WorkflowRunReconciler{Client: fakeClient, Scheme: unitTestScheme}
	list := r.listCompletedRunsForWorkflow(ctx, ns, "wf", ns)
	if len(list) != 1 {
		t.Fatalf("expected 1 completed run for wf/default, got %d", len(list))
	}
	if list[0].Name != "run-1" {
		t.Errorf("expected run-1, got %s", list[0].Name)
	}
}

func TestTriggerManager_CreateWorkflowRunFromEvent_SuccessCreatesRun(t *testing.T) {
	ctx := context.Background()
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec:       ottoflowv1alpha1.WorkflowSpec{Steps: []ottoflowv1alpha1.Step{{Name: "s1", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `"ok"`}}}}},
	}
	// Use Unstructured so CEL inputMapping can traverse the object fields.
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]interface{}{
			"name":      "p1",
			"namespace": "default",
			"uid":       "uid-12345",
		},
	}}
	eventSpec := &ottoflowv1alpha1.EventTrigger{
		Resources:    []ottoflowv1alpha1.EventResource{{APIVersion: "v1", Kind: "Pod", Namespace: "default"}},
		Operations:   []string{"CREATE"},
		InputMapping: map[string]string{"podName": `object.metadata.name`},
	}
	// Status().Update requires status subresource; use a placeholder so the builder registers it.
	placeholderWR := &ottoflowv1alpha1.WorkflowRun{ObjectMeta: metav1.ObjectMeta{Name: "placeholder", Namespace: "default"}}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(placeholderWR).WithObjects(wf).Build()
	tm := NewTriggerManager(fakeClient, unitTestScheme, nil)
	if err := tm.CreateWorkflowRunFromEvent(ctx, "test-trigger", wf, eventSpec, obj, "ADDED"); err != nil {
		t.Fatal(err)
	}
	var list ottoflowv1alpha1.WorkflowRunList
	if err := fakeClient.List(ctx, &list, client.InNamespace("default"), client.MatchingLabels{"ottoflow.nirmata.io/workflow": "wf"}); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("expected 1 WorkflowRun, got %d", len(list.Items))
	}
	run := &list.Items[0]
	// CEL evaluated object.metadata.name → "p1"
	if run.Spec.InputValues["podName"] != "p1" {
		t.Errorf("InputValues[podName] = %q, want %q", run.Spec.InputValues["podName"], "p1")
	}
	if run.Status.Trigger == nil || run.Status.Trigger.Type != "Event" {
		t.Errorf("Trigger = %v", run.Status.Trigger)
	}
}

func TestTriggerManager_CreateWorkflowRunFromEvent_UPDATE_MatchesModified(t *testing.T) {
	ctx := context.Background()
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec:       ottoflowv1alpha1.WorkflowSpec{Steps: []ottoflowv1alpha1.Step{{Name: "s1", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `"ok"`}}}}},
	}
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": "p2", "namespace": "default", "uid": "uid-mod"},
	}}
	eventSpec := &ottoflowv1alpha1.EventTrigger{
		Resources:  []ottoflowv1alpha1.EventResource{{APIVersion: "v1", Kind: "Pod", Namespace: "default"}},
		Operations: []string{"UPDATE"},
	}
	placeholderWR := &ottoflowv1alpha1.WorkflowRun{ObjectMeta: metav1.ObjectMeta{Name: "placeholder", Namespace: "default"}}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(placeholderWR).WithObjects(wf).Build()
	tm := NewTriggerManager(fakeClient, unitTestScheme, nil)
	if err := tm.CreateWorkflowRunFromEvent(ctx, "test-trigger", wf, eventSpec, obj, "MODIFIED"); err != nil {
		t.Fatal(err)
	}
	var list ottoflowv1alpha1.WorkflowRunList
	if err := fakeClient.List(ctx, &list, client.InNamespace("default"), client.MatchingLabels{"ottoflow.nirmata.io/workflow": "wf"}); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Errorf("expected 1 WorkflowRun for UPDATE+MODIFIED, got %d", len(list.Items))
	}
}

// makeFakeClient returns a fake client pre-registered with the WorkflowRun status subresource.
func makeFakeClient(objs ...client.Object) client.Client {
	placeholder := &ottoflowv1alpha1.WorkflowRun{ObjectMeta: metav1.ObjectMeta{Name: "placeholder", Namespace: "default"}}
	return fake.NewClientBuilder().
		WithScheme(unitTestScheme).
		WithStatusSubresource(placeholder).
		WithObjects(objs...).
		Build()
}

// makeUnstructured builds a minimal unstructured object for use in trigger tests.
func makeUnstructured(name, namespace string, extra map[string]interface{}) *unstructured.Unstructured {
	obj := map[string]interface{}{
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": namespace,
			"uid":       name + "-uid",
		},
	}
	for k, v := range extra {
		obj[k] = v
	}
	return &unstructured.Unstructured{Object: obj}
}

func TestTriggerManager_InputMapping_CELEvaluation(t *testing.T) {
	ctx := context.Background()
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec:       ottoflowv1alpha1.WorkflowSpec{Steps: []ottoflowv1alpha1.Step{{Name: "s", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `"ok"`}}}}},
	}
	obj := makeUnstructured("my-app", "production", map[string]interface{}{
		"spec": map[string]interface{}{
			"destination": map[string]interface{}{
				"namespace": "production",
			},
		},
	})
	eventSpec := &ottoflowv1alpha1.EventTrigger{
		Resources: []ottoflowv1alpha1.EventResource{{APIVersion: "argoproj.io/v1alpha1", Kind: "Application"}},
		InputMapping: map[string]string{
			"appName":   `object.metadata.name`,
			"namespace": `object.spec.destination.namespace`,
		},
	}
	fc := makeFakeClient(wf)
	tm := NewTriggerManager(fc, unitTestScheme, nil)
	if err := tm.CreateWorkflowRunFromEvent(ctx, "trigger-1", wf, eventSpec, obj, "MODIFIED"); err != nil {
		t.Fatal(err)
	}
	var list ottoflowv1alpha1.WorkflowRunList
	_ = fc.List(ctx, &list, client.InNamespace("default"), client.MatchingLabels{"ottoflow.nirmata.io/workflow": "wf"})
	if len(list.Items) != 1 {
		t.Fatalf("expected 1 WorkflowRun, got %d", len(list.Items))
	}
	run := list.Items[0]
	if run.Spec.InputValues["appName"] != "my-app" {
		t.Errorf("appName = %q, want %q", run.Spec.InputValues["appName"], "my-app")
	}
	if run.Spec.InputValues["namespace"] != "production" {
		t.Errorf("namespace = %q, want %q", run.Spec.InputValues["namespace"], "production")
	}
}

func TestTriggerManager_CELFilter_DropsNonMatchingEvents(t *testing.T) {
	ctx := context.Background()
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec:       ottoflowv1alpha1.WorkflowSpec{Steps: []ottoflowv1alpha1.Step{{Name: "s", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `"ok"`}}}}},
	}
	eventSpec := &ottoflowv1alpha1.EventTrigger{
		Resources: []ottoflowv1alpha1.EventResource{{APIVersion: "argoproj.io/v1alpha1", Kind: "Application"}},
		// Only fire when sync status is Synced
		CELFilter: `object.status.sync.status == "Synced"`,
	}
	fc := makeFakeClient(wf)
	tm := NewTriggerManager(fc, unitTestScheme, nil)

	// Event that does NOT pass the filter
	syncing := makeUnstructured("app1", "default", map[string]interface{}{
		"status": map[string]interface{}{"sync": map[string]interface{}{"status": "OutOfSync"}},
	})
	if err := tm.CreateWorkflowRunFromEvent(ctx, "t", wf, eventSpec, syncing, "MODIFIED"); err != nil {
		t.Fatal(err)
	}

	// Event that DOES pass the filter
	synced := makeUnstructured("app1", "default", map[string]interface{}{
		"status": map[string]interface{}{"sync": map[string]interface{}{"status": "Synced"}},
	})
	if err := tm.CreateWorkflowRunFromEvent(ctx, "t", wf, eventSpec, synced, "MODIFIED"); err != nil {
		t.Fatal(err)
	}

	var list ottoflowv1alpha1.WorkflowRunList
	_ = fc.List(ctx, &list, client.InNamespace("default"), client.MatchingLabels{"ottoflow.nirmata.io/workflow": "wf"})
	if len(list.Items) != 1 {
		t.Errorf("expected 1 WorkflowRun (only the Synced event), got %d", len(list.Items))
	}
}

func TestTriggerManager_DedupKey_PreventsRunForSameRevision(t *testing.T) {
	ctx := context.Background()
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec:       ottoflowv1alpha1.WorkflowSpec{Steps: []ottoflowv1alpha1.Step{{Name: "s", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `"ok"`}}}}},
	}
	// ArgoCD-style status with sync.revision.
	// Each object gets a distinct UID so WorkflowRun names don't collide.
	appV1 := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": "my-app", "namespace": "default", "uid": "uid-v1"},
		"status":   map[string]interface{}{"sync": map[string]interface{}{"revision": "abc123"}},
	}}
	appV1b := &unstructured.Unstructured{Object: map[string]interface{}{
		// Same revision — should be deduped (same object, same revision)
		"metadata": map[string]interface{}{"name": "my-app", "namespace": "default", "uid": "uid-v1"},
		"status":   map[string]interface{}{"sync": map[string]interface{}{"revision": "abc123"}},
	}}
	appV2 := &unstructured.Unstructured{Object: map[string]interface{}{
		// New revision — should fire (different UID suffix so name doesn't collide)
		"metadata": map[string]interface{}{"name": "my-app", "namespace": "default", "uid": "uid-v2-diff"},
		"status":   map[string]interface{}{"sync": map[string]interface{}{"revision": "def456"}},
	}}

	fc := makeFakeClient(wf)
	tm := NewTriggerManager(fc, unitTestScheme, nil)
	eventSpec := &ottoflowv1alpha1.EventTrigger{
		Resources: []ottoflowv1alpha1.EventResource{{APIVersion: "argoproj.io/v1alpha1", Kind: "Application"}},
	}

	// First event — should create a run
	if err := tm.CreateWorkflowRunFromEvent(ctx, "t", wf, eventSpec, appV1, "MODIFIED"); err != nil {
		t.Fatal(err)
	}
	// Duplicate (same revision) — should be dropped
	if err := tm.CreateWorkflowRunFromEvent(ctx, "t", wf, eventSpec, appV1b, "MODIFIED"); err != nil {
		t.Fatal(err)
	}

	var list ottoflowv1alpha1.WorkflowRunList
	_ = fc.List(ctx, &list, client.InNamespace("default"), client.MatchingLabels{"ottoflow.nirmata.io/workflow": "wf"})
	if len(list.Items) != 1 {
		t.Errorf("same revision: expected 1 WorkflowRun, got %d", len(list.Items))
	}

	// New revision — should create a second run
	if err := tm.CreateWorkflowRunFromEvent(ctx, "t", wf, eventSpec, appV2, "MODIFIED"); err != nil {
		t.Fatal(err)
	}
	_ = fc.List(ctx, &list, client.InNamespace("default"), client.MatchingLabels{"ottoflow.nirmata.io/workflow": "wf"})
	if len(list.Items) != 2 {
		t.Errorf("new revision: expected 2 WorkflowRuns, got %d", len(list.Items))
	}
}

func TestTriggerManager_DedupWindow_FallbackThrottle(t *testing.T) {
	ctx := context.Background()
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec:       ottoflowv1alpha1.WorkflowSpec{Steps: []ottoflowv1alpha1.Step{{Name: "s", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `"ok"`}}}}},
	}
	// Object with no known revision fields — auto-detect returns empty, window applies
	obj := makeUnstructured("app1", "default", nil)
	window := metav1.Duration{Duration: 10 * time.Minute}
	eventSpec := &ottoflowv1alpha1.EventTrigger{
		Resources:   []ottoflowv1alpha1.EventResource{{APIVersion: "v1", Kind: "ConfigMap"}},
		DedupWindow: &window,
	}
	fc := makeFakeClient(wf)
	tm := NewTriggerManager(fc, unitTestScheme, nil)

	// First event — creates a run
	if err := tm.CreateWorkflowRunFromEvent(ctx, "t", wf, eventSpec, obj, "MODIFIED"); err != nil {
		t.Fatal(err)
	}
	// Second event within window — dropped
	if err := tm.CreateWorkflowRunFromEvent(ctx, "t", wf, eventSpec, obj, "MODIFIED"); err != nil {
		t.Fatal(err)
	}
	var list ottoflowv1alpha1.WorkflowRunList
	_ = fc.List(ctx, &list, client.InNamespace("default"), client.MatchingLabels{"ottoflow.nirmata.io/workflow": "wf"})
	if len(list.Items) != 1 {
		t.Errorf("within window: expected 1 WorkflowRun, got %d", len(list.Items))
	}
}

func TestTriggerManager_AutoDetectDedupKey(t *testing.T) {
	cases := []struct {
		name    string
		obj     map[string]interface{}
		wantKey string
	}{
		{
			name: "ArgoCD Application",
			obj: map[string]interface{}{
				"status": map[string]interface{}{"sync": map[string]interface{}{"revision": "sha-argocd"}},
			},
			wantKey: "sha-argocd",
		},
		{
			name: "FluxCD Kustomization",
			obj: map[string]interface{}{
				"status": map[string]interface{}{"lastAppliedRevision": "sha-flux-kustomize"},
			},
			wantKey: "sha-flux-kustomize",
		},
		{
			name: "FluxCD HelmRelease",
			obj: map[string]interface{}{
				"status": map[string]interface{}{"lastAttemptedRevision": "sha-helm"},
			},
			wantKey: "sha-helm",
		},
		{
			name: "FluxCD OCIRepository / GitRepository",
			obj: map[string]interface{}{
				"status": map[string]interface{}{"artifact": map[string]interface{}{"revision": "sha-oci"}},
			},
			wantKey: "sha-oci",
		},
		{
			name:    "unknown controller — no key",
			obj:     map[string]interface{}{"status": map[string]interface{}{"commit": "abc"}},
			wantKey: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := autoDetectDedupKey(tc.obj)
			if got != tc.wantKey {
				t.Errorf("autoDetectDedupKey = %q, want %q", got, tc.wantKey)
			}
		})
	}
}

func TestWorkflowRunReconciler_Reconcile_CreatesRunnerJob(t *testing.T) {
	ctx := context.Background()
	ns := defaultNamespace
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowSpec{Steps: []ottoflowv1alpha1.Step{{Name: "s1", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `"ok"`}}}}},
	}
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: ns}},
		Status:     ottoflowv1alpha1.WorkflowRunStatus{Phase: ottoflowv1alpha1.WorkflowRunPhasePending},
	}
	clusterRole := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "ottoflow-role"}}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(wr).WithObjects(wf, wr, clusterRole).Build()
	r := &WorkflowRunReconciler{
		Client:       fakeClient,
		Scheme:       unitTestScheme,
		RunnerConfig: RunnerConfig{RunnerServiceAccount: "controller-manager", RunnerClusterRole: "ottoflow-role"},
	}
	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: wr.Name, Namespace: ns}})
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter <= 0 {
		t.Error("expected RequeueAfter")
	}
	job := &batchv1.Job{}
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: "run-1-runner", Namespace: ns}, job); err != nil {
		t.Fatal(err)
	}
	if job.Spec.Template.Spec.Containers[0].Name != "workflow-runner" {
		t.Errorf("job container name = %q", job.Spec.Template.Spec.Containers[0].Name)
	}
	updated := &ottoflowv1alpha1.WorkflowRun{}
	if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(wr), updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.Execution == nil || updated.Status.Execution.JobName != "run-1-runner" {
		t.Errorf("Execution = %v", updated.Status.Execution)
	}
}

func TestWorkflowRunReconciler_Reconcile_WorkflowRefEmptyNamespace_UsesRunNamespace(t *testing.T) {
	ctx := context.Background()
	ns := defaultNamespace
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowSpec{Steps: []ottoflowv1alpha1.Step{{Name: "s1", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `"ok"`}}}}},
	}
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-ref-empty-ns", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: ""}},
		Status:     ottoflowv1alpha1.WorkflowRunStatus{Phase: ottoflowv1alpha1.WorkflowRunPhasePending},
	}
	clusterRole := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "ottoflow-role"}}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(wr).WithObjects(wf, wr, clusterRole).Build()
	r := &WorkflowRunReconciler{
		Client:       fakeClient,
		Scheme:       unitTestScheme,
		RunnerConfig: RunnerConfig{RunnerServiceAccount: "controller-manager", RunnerClusterRole: "ottoflow-role"},
	}
	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: wr.Name, Namespace: ns}})
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter <= 0 {
		t.Error("expected RequeueAfter")
	}
	job := &batchv1.Job{}
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: "run-ref-empty-ns-runner", Namespace: ns}, job); err != nil {
		t.Fatal(err)
	}
}

func TestWorkflowRunReconciler_Reconcile_JobAlreadyExists_Continues(t *testing.T) {
	ctx := context.Background()
	ns := defaultNamespace
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowSpec{Steps: []ottoflowv1alpha1.Step{{Name: "s1", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `"ok"`}}}}},
	}
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: ns}},
		Status:     ottoflowv1alpha1.WorkflowRunStatus{Phase: ottoflowv1alpha1.WorkflowRunPhasePending, Execution: &ottoflowv1alpha1.WorkflowRunExecutionStatus{JobName: "run-1-runner"}},
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1-runner", Namespace: ns},
		Status:     batchv1.JobStatus{Active: 0, Succeeded: 1},
	}
	clusterRole := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "ottoflow-role"}}
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "controller-manager", Namespace: ns}}
	mainCRB := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: workflowRunnerRoleBindingName(ns, "controller-manager"), Labels: map[string]string{"app.kubernetes.io/part-of": "ottoflow"}},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "ottoflow-role"},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: "controller-manager", Namespace: ns}},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(wr).WithObjects(wf, wr, job, clusterRole, sa, mainCRB).Build()
	r := &WorkflowRunReconciler{Client: fakeClient, Scheme: unitTestScheme, RunnerConfig: RunnerConfig{RunnerServiceAccount: "controller-manager", RunnerClusterRole: "ottoflow-role"}}
	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: wr.Name, Namespace: ns}})
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter <= 0 {
		t.Error("expected RequeueAfter")
	}
}

func TestWorkflowRunReconciler_Reconcile_JobSucceeded_Requeues(t *testing.T) {
	ctx := context.Background()
	ns := defaultNamespace
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowSpec{Steps: []ottoflowv1alpha1.Step{{Name: "s1", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `"ok"`}}}}},
	}
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: ns}},
		Status:     ottoflowv1alpha1.WorkflowRunStatus{Phase: ottoflowv1alpha1.WorkflowRunPhaseRunning, Execution: &ottoflowv1alpha1.WorkflowRunExecutionStatus{JobName: "run-1-runner", Phase: "Running"}},
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1-runner", Namespace: ns},
		Status:     batchv1.JobStatus{Active: 0, Succeeded: 1},
	}
	clusterRole := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "ottoflow-role"}}
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "controller-manager", Namespace: ns}}
	mainCRB := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: workflowRunnerRoleBindingName(ns, "controller-manager"), Labels: map[string]string{"app.kubernetes.io/part-of": "ottoflow"}},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "ottoflow-role"},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: "controller-manager", Namespace: ns}},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(wr).WithObjects(wf, wr, job, clusterRole, sa, mainCRB).Build()
	r := &WorkflowRunReconciler{Client: fakeClient, Scheme: unitTestScheme, RunnerConfig: RunnerConfig{RunnerServiceAccount: "controller-manager", RunnerClusterRole: "ottoflow-role"}}
	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: wr.Name, Namespace: ns}})
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter <= 0 {
		t.Error("expected RequeueAfter when job succeeded (runner may not have set phase yet)")
	}
}

func TestGetReferencedWorkflow_FallbackToControllerNamespace(t *testing.T) {
	ctx := context.Background()
	// Workflow lives in the controller release namespace ("ottoflow-alt"),
	// but the WorkflowRun was created with workflowRef.namespace="ottoflow"
	// (an upstream caller hardcoded the original default). Without the fallback,
	// getReferencedWorkflow would error with NotFound.
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "cost-analyzer", Namespace: "ottoflow-alt"},
		Spec:       ottoflowv1alpha1.WorkflowSpec{Steps: []ottoflowv1alpha1.Step{{Name: "s1", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `"ok"`}}}}},
	}
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: "agents"},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "cost-analyzer", Namespace: "ottoflow"}},
	}
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(wf, wr).Build()
	r := &WorkflowRunReconciler{Client: c, Scheme: unitTestScheme, ControllerNamespace: "ottoflow-alt"}

	got, gotNs, err := r.getReferencedWorkflow(ctx, wr)
	if err != nil {
		t.Fatalf("expected fallback to succeed, got err: %v", err)
	}
	if got == nil || got.Name != "cost-analyzer" {
		t.Fatalf("expected cost-analyzer workflow, got %+v", got)
	}
	if gotNs != "ottoflow-alt" {
		t.Errorf("expected resolved namespace ottoflow-alt, got %q", gotNs)
	}
}

func TestGetReferencedWorkflow_NoFallbackWhenControllerNamespaceUnset(t *testing.T) {
	ctx := context.Background()
	// Same setup as above but ControllerNamespace="" — must NOT fall back.
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "cost-analyzer", Namespace: "ottoflow-alt"},
		Spec:       ottoflowv1alpha1.WorkflowSpec{Steps: []ottoflowv1alpha1.Step{{Name: "s1", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `"ok"`}}}}},
	}
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: "agents"},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "cost-analyzer", Namespace: "ottoflow"}},
	}
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(wf, wr).Build()
	r := &WorkflowRunReconciler{Client: c, Scheme: unitTestScheme}

	if _, _, err := r.getReferencedWorkflow(ctx, wr); err == nil {
		t.Fatal("expected error when ControllerNamespace is empty and Workflow not found in workflowRef.namespace")
	}
}

func TestGetReferencedWorkflow_PrimaryLookupSucceeds(t *testing.T) {
	ctx := context.Background()
	// Workflow exists in workflowRef.namespace; fallback path must NOT be exercised.
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "cost-analyzer", Namespace: "ottoflow"},
		Spec:       ottoflowv1alpha1.WorkflowSpec{Steps: []ottoflowv1alpha1.Step{{Name: "s1", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `"ok"`}}}}},
	}
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: "agents"},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "cost-analyzer", Namespace: "ottoflow"}},
	}
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(wf, wr).Build()
	r := &WorkflowRunReconciler{Client: c, Scheme: unitTestScheme, ControllerNamespace: "ottoflow-alt"}

	_, gotNs, err := r.getReferencedWorkflow(ctx, wr)
	if err != nil {
		t.Fatalf("expected primary lookup to succeed, got err: %v", err)
	}
	if gotNs != "ottoflow" {
		t.Errorf("expected resolved namespace ottoflow (primary), got %q", gotNs)
	}
}

// TestWorkflowRunReconciler_Reconcile_FallbackWorkflowFetchTransientError_Requeues is the
// regression test for Fix 3 (issue #33): the primary Workflow lookup is a genuine NotFound, and
// the ControllerNamespace fallback lookup fails with a transient (non-NotFound) API error. Before
// the fix, getReferencedWorkflow discarded the fallback error and always returned the wrapped
// primary NotFound, so the caller's apierrors.IsNotFound check terminally failed the run instead
// of requeuing on what was really just a passing API-server hiccup on the fallback Get.
func TestWorkflowRunReconciler_Reconcile_FallbackWorkflowFetchTransientError_Requeues(t *testing.T) {
	ctx := context.Background()
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: "agents"},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "cost-analyzer", Namespace: "ottoflow"}},
		Status:     ottoflowv1alpha1.WorkflowRunStatus{Phase: ottoflowv1alpha1.WorkflowRunPhasePending},
	}
	// No Workflow exists anywhere: the primary Get (namespace "ottoflow") is a real NotFound.
	baseClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(wr).WithObjects(wr).Build()
	fakeClient := interceptor.NewClient(baseClient, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*ottoflowv1alpha1.Workflow); ok && key.Namespace == "ottoflow-alt" {
				// The fallback Get (in ControllerNamespace) hits a transient API-server error.
				return apierrors.NewServerTimeout(ottoflowv1alpha1.GroupVersion.WithResource("workflows").GroupResource(), "get", 1)
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	r := &WorkflowRunReconciler{Client: fakeClient, Scheme: unitTestScheme, ControllerNamespace: "ottoflow-alt"}

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: wr.Name, Namespace: "agents"}})
	if err == nil {
		t.Fatal("expected a non-nil error so controller-runtime requeues with backoff")
	}
	if apierrors.IsNotFound(err) {
		t.Fatalf("expected a transient error, got NotFound: %v", err)
	}

	updated := &ottoflowv1alpha1.WorkflowRun{}
	if getErr := fakeClient.Get(ctx, client.ObjectKeyFromObject(wr), updated); getErr != nil {
		t.Fatal(getErr)
	}
	if updated.Status.Phase == ottoflowv1alpha1.WorkflowRunPhaseFailed {
		t.Errorf("a transient fallback fetch error must not fail the run; Phase = %v", updated.Status.Phase)
	}
}

// --- reconcilePendingCallback (waitForCallback timeout / resume) tests ---

// buildCallbackWorkflowAndRun builds a minimal Workflow with a single waitForCallback step named
// "gate" and a WorkflowRun already parked on that step with an EXPIRED PendingCallback, ready to
// exercise the timeout branch of reconcilePendingCallback via a plain Reconcile call.
func buildCallbackWorkflowAndRun(wfName, runName, failurePolicy string) (*ottoflowv1alpha1.Workflow, *ottoflowv1alpha1.WorkflowRun) {
	ns := defaultNamespace
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: wfName, Namespace: ns},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{
				{
					Name: "gate",
					WaitForCallback: &ottoflowv1alpha1.WaitForCallbackStep{
						Timeout:       "1h",
						FailurePolicy: failurePolicy,
					},
				},
			},
		},
	}
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: runName, Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: wfName, Namespace: ns}},
		Status: ottoflowv1alpha1.WorkflowRunStatus{
			Phase: ottoflowv1alpha1.WorkflowRunPhaseRunning,
			PendingCallback: &ottoflowv1alpha1.CallbackState{
				TokenHash: "irrelevant-for-these-tests",
				StepName:  "gate",
				ExpiresAt: time.Now().Add(-1 * time.Hour).Unix(), // already expired
				CreatedAt: time.Now().Add(-2 * time.Hour).Unix(),
			},
		},
	}
	return wf, wr
}

// TestReconcilePendingCallback_TimeoutContinue_RecreatesRunnerJob is the Fix-1 regression test:
// before this fix, a Continue failurePolicy on timeout cleared PendingCallback and marked the
// step Skipped without ever recreating the runner Job, so the run went permanently idle. Continue
// must instead recreate the Job and leave PendingCallback set for the executor's own recovery path.
func TestReconcilePendingCallback_TimeoutContinue_RecreatesRunnerJob(t *testing.T) {
	ctx := context.Background()
	ns := defaultNamespace
	wf, wr := buildCallbackWorkflowAndRun("wf-timeout-continue", "run-timeout-continue", ottoflowv1alpha1.FailurePolicyContinue)
	clusterRole := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "ottoflow-role"}}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(wr).WithObjects(wf, wr, clusterRole).Build()
	r := &WorkflowRunReconciler{
		Client:       fakeClient,
		Scheme:       unitTestScheme,
		RunnerConfig: RunnerConfig{RunnerServiceAccount: "controller-manager", RunnerClusterRole: "ottoflow-role"},
	}

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: wr.Name, Namespace: ns}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	updated := &ottoflowv1alpha1.WorkflowRun{}
	if getErr := fakeClient.Get(ctx, client.ObjectKeyFromObject(wr), updated); getErr != nil {
		t.Fatal(getErr)
	}
	if updated.Status.Phase != ottoflowv1alpha1.WorkflowRunPhaseRunning {
		t.Errorf("Phase = %v, want Running", updated.Status.Phase)
	}
	if updated.Status.PendingCallback == nil {
		t.Fatal("expected PendingCallback to remain set so the recreated runner's executor applies the Continue recovery path")
	}
	if ss, ok := updated.Status.StepStatuses["gate"]; ok {
		t.Errorf("controller must not mark the gate step itself on Continue; got StepStatuses[gate] = %+v", ss)
	}

	job := &batchv1.Job{}
	if getErr := fakeClient.Get(ctx, types.NamespacedName{Name: workflowRunnerJobName(wr.Name), Namespace: ns}, job); getErr != nil {
		t.Fatalf("expected runner Job to be recreated, got: %v", getErr)
	}
}

// TestReconcilePendingCallback_TimeoutContinue_EmitsCallbackTimeoutEvent asserts the CallbackTimeout
// event still fires on Continue even when there is no old runner Job to react to (e.g. its
// completion-TTL already garbage collected it) — the event must not depend on the Job-state switch.
func TestReconcilePendingCallback_TimeoutContinue_EmitsCallbackTimeoutEvent(t *testing.T) {
	ctx := context.Background()
	ns := defaultNamespace
	wf, wr := buildCallbackWorkflowAndRun("wf-timeout-continue-evt", "run-timeout-continue-evt", ottoflowv1alpha1.FailurePolicyContinue)
	clusterRole := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "ottoflow-role"}}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(wr).WithObjects(wf, wr, clusterRole).Build()
	recorder := events.NewFakeRecorder(10)
	r := &WorkflowRunReconciler{
		Client:        fakeClient,
		Scheme:        unitTestScheme,
		EventRecorder: recorder,
		RunnerConfig:  RunnerConfig{RunnerServiceAccount: "controller-manager", RunnerClusterRole: "ottoflow-role"},
	}

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: wr.Name, Namespace: ns}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	select {
	case evt := <-recorder.Events:
		if !strings.Contains(evt, "CallbackTimeout") {
			t.Errorf("event = %q, want CallbackTimeout", evt)
		}
	default:
		t.Error("expected a CallbackTimeout event even when no old runner Job exists to react to")
	}
}

// TestReconcilePendingCallback_TimeoutFail_TerminalFailure covers the Fail (default) failurePolicy
// on timeout: the run must go terminally Failed, PendingCallback must be cleared (so the callback
// endpoint stops accepting POSTs for a dead run), the gate step must be marked Failed, no runner
// Job is created, and a CallbackTimeout event is still recorded.
func TestReconcilePendingCallback_TimeoutFail_TerminalFailure(t *testing.T) {
	ctx := context.Background()
	ns := defaultNamespace
	wf, wr := buildCallbackWorkflowAndRun("wf-timeout-fail", "run-timeout-fail", ottoflowv1alpha1.FailurePolicyFail)
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(wr).WithObjects(wf, wr).Build()
	recorder := events.NewFakeRecorder(10)
	r := &WorkflowRunReconciler{Client: fakeClient, Scheme: unitTestScheme, EventRecorder: recorder}

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: wr.Name, Namespace: ns}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	updated := &ottoflowv1alpha1.WorkflowRun{}
	if getErr := fakeClient.Get(ctx, client.ObjectKeyFromObject(wr), updated); getErr != nil {
		t.Fatal(getErr)
	}
	if updated.Status.Phase != ottoflowv1alpha1.WorkflowRunPhaseFailed {
		t.Errorf("Phase = %v, want Failed", updated.Status.Phase)
	}
	if updated.Status.PendingCallback != nil {
		t.Error("expected PendingCallback to be cleared on terminal failure")
	}
	if ss := updated.Status.StepStatuses["gate"]; ss.Phase != ottoflowv1alpha1.StepPhaseFailed {
		t.Errorf("gate StepStatus.Phase = %v, want Failed", ss.Phase)
	}

	job := &batchv1.Job{}
	getErr := fakeClient.Get(ctx, types.NamespacedName{Name: workflowRunnerJobName(wr.Name), Namespace: ns}, job)
	if !apierrors.IsNotFound(getErr) {
		t.Errorf("expected no runner Job to be created, Get returned: %v", getErr)
	}

	select {
	case evt := <-recorder.Events:
		if !strings.Contains(evt, "CallbackTimeout") {
			t.Errorf("event = %q, want CallbackTimeout", evt)
		}
	default:
		t.Error("expected a CallbackTimeout event to be recorded")
	}
}

// TestReconcilePendingCallback_FallbackNamespacePolicy_ContinueResumes is the regression test for
// Fix 1a: the Workflow here only resolves through the ControllerNamespace fallback (workflowRef
// points at a namespace where it doesn't exist). The old findStepFailurePolicy did its own Get
// without that fallback, so it could never find the step and always fell back to Fail — wrongly
// failing a run that should have resumed under its actual Continue policy.
func TestReconcilePendingCallback_FallbackNamespacePolicy_ContinueResumes(t *testing.T) {
	ctx := context.Background()
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "cost-analyzer", Namespace: "ottoflow-alt"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{
				{
					Name: "gate",
					WaitForCallback: &ottoflowv1alpha1.WaitForCallbackStep{
						Timeout:       "1h",
						FailurePolicy: ottoflowv1alpha1.FailurePolicyContinue,
					},
				},
			},
		},
	}
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: "agents"},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "cost-analyzer", Namespace: "ottoflow"}},
		Status: ottoflowv1alpha1.WorkflowRunStatus{
			Phase: ottoflowv1alpha1.WorkflowRunPhaseRunning,
			PendingCallback: &ottoflowv1alpha1.CallbackState{
				TokenHash: "irrelevant-for-this-test",
				StepName:  "gate",
				ExpiresAt: time.Now().Add(-1 * time.Hour).Unix(),
				CreatedAt: time.Now().Add(-2 * time.Hour).Unix(),
			},
		},
	}
	clusterRole := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "ottoflow-role"}}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(wr).WithObjects(wf, wr, clusterRole).Build()
	r := &WorkflowRunReconciler{
		Client:              fakeClient,
		Scheme:              unitTestScheme,
		ControllerNamespace: "ottoflow-alt",
		RunnerConfig:        RunnerConfig{RunnerServiceAccount: "controller-manager", RunnerClusterRole: "ottoflow-role"},
	}

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "run-1", Namespace: "agents"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	updated := &ottoflowv1alpha1.WorkflowRun{}
	if getErr := fakeClient.Get(ctx, client.ObjectKeyFromObject(wr), updated); getErr != nil {
		t.Fatal(getErr)
	}
	if updated.Status.Phase == ottoflowv1alpha1.WorkflowRunPhaseFailed {
		t.Fatal("a Continue policy resolved via the ControllerNamespace fallback must resume the run, not fail it")
	}
	if updated.Status.Phase != ottoflowv1alpha1.WorkflowRunPhaseRunning {
		t.Errorf("Phase = %v, want Running", updated.Status.Phase)
	}
	if updated.Status.PendingCallback == nil {
		t.Error("expected PendingCallback to remain set")
	}
}

// TestReconcilePendingCallback_OutputsDelivered_NoOldJob_CreatesRunnerJob is a regression test for
// the outputs-present branch after extracting resumeRunnerJob: a delivered (non-empty) callback
// payload must still recreate the runner Job and retain PendingCallback even when there is no old
// Job object at all (this reconciler is seeing the run for the first time since the callback landed).
func TestReconcilePendingCallback_OutputsDelivered_NoOldJob_CreatesRunnerJob(t *testing.T) {
	ctx := context.Background()
	ns := defaultNamespace
	wf, wr := buildCallbackWorkflowAndRun("wf-delivered", "run-delivered", ottoflowv1alpha1.FailurePolicyFail)
	// Not expired, and outputs already delivered: the outputs-present branch must win regardless
	// of expiry or failurePolicy.
	wr.Status.PendingCallback.ExpiresAt = time.Now().Add(1 * time.Hour).Unix()
	wr.Status.PendingCallback.Outputs = apiextensionsv1.JSON{Raw: []byte(`{"approved":true}`)}
	clusterRole := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "ottoflow-role"}}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(wr).WithObjects(wf, wr, clusterRole).Build()
	r := &WorkflowRunReconciler{
		Client:       fakeClient,
		Scheme:       unitTestScheme,
		RunnerConfig: RunnerConfig{RunnerServiceAccount: "controller-manager", RunnerClusterRole: "ottoflow-role"},
	}

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: wr.Name, Namespace: ns}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	updated := &ottoflowv1alpha1.WorkflowRun{}
	if getErr := fakeClient.Get(ctx, client.ObjectKeyFromObject(wr), updated); getErr != nil {
		t.Fatal(getErr)
	}
	if updated.Status.Phase != ottoflowv1alpha1.WorkflowRunPhaseRunning {
		t.Errorf("Phase = %v, want Running", updated.Status.Phase)
	}
	if updated.Status.PendingCallback == nil || len(updated.Status.PendingCallback.Outputs.Raw) == 0 {
		t.Error("expected PendingCallback (with its delivered Outputs) to be retained for the executor")
	}

	job := &batchv1.Job{}
	if getErr := fakeClient.Get(ctx, types.NamespacedName{Name: workflowRunnerJobName(wr.Name), Namespace: ns}, job); getErr != nil {
		t.Fatalf("expected runner Job to be created, got: %v", getErr)
	}
}

// TestReconcilePendingCallback_ActiveResumeJobFailedNoCondition_StaysMonitoring is the regression
// test for Fix 1: a recreated resume Job with backoffLimit>0 can report Status.Failed>0 for an
// in-flight pod retry well before Kubernetes sets the terminal JobFailed *condition*. Before the
// fix, resumeRunnerJob's active-Job branch returned proceed=true unconditionally, so the caller
// fell through to the normal Job-creation block's crude `job.Status.Failed>0` check, which
// terminally failed the run (or restarted it) on what was actually just a transient pod failure —
// defeating the JobFailed-condition gate in resumeRunnerJob's own JobFailed case. The fix keeps
// monitoring inside the pending-callback path instead of falling through.
func TestReconcilePendingCallback_ActiveResumeJobFailedNoCondition_StaysMonitoring(t *testing.T) {
	ctx := context.Background()
	ns := defaultNamespace
	wf, wr := buildCallbackWorkflowAndRun("wf-active-resume", "run-active-resume", ottoflowv1alpha1.FailurePolicyFail)
	// Not expired, and outputs already delivered: the outputs-present branch routes straight into
	// resumeRunnerJob regardless of expiry or failurePolicy.
	wr.Status.PendingCallback.ExpiresAt = time.Now().Add(1 * time.Hour).Unix()
	wr.Status.PendingCallback.Outputs = apiextensionsv1.JSON{Raw: []byte(`{"approved":true}`)}

	// The recreated resume Job already exists, is active, and has one failed pod (an in-flight
	// backoffLimit>0 retry) but NO JobFailed condition yet.
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: workflowRunnerJobName(wr.Name), Namespace: ns},
		Spec:       batchv1.JobSpec{BackoffLimit: ptr.To(int32(3))},
		Status:     batchv1.JobStatus{Active: 1, Failed: 1},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(wr).WithObjects(wf, wr, job).Build()
	r := &WorkflowRunReconciler{Client: fakeClient, Scheme: unitTestScheme}

	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: wr.Name, Namespace: ns}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.RequeueAfter != 2*time.Second {
		t.Errorf("RequeueAfter = %v, want 2s", result.RequeueAfter)
	}

	updatedJob := &batchv1.Job{}
	if getErr := fakeClient.Get(ctx, client.ObjectKeyFromObject(job), updatedJob); getErr != nil {
		t.Fatalf("expected resume Job to still exist (not deleted), got: %v", getErr)
	}

	updated := &ottoflowv1alpha1.WorkflowRun{}
	if getErr := fakeClient.Get(ctx, client.ObjectKeyFromObject(wr), updated); getErr != nil {
		t.Fatal(getErr)
	}
	if updated.Status.Phase != ottoflowv1alpha1.WorkflowRunPhaseRunning {
		t.Errorf("Phase = %v, want Running", updated.Status.Phase)
	}
	if updated.Status.PendingCallback == nil {
		t.Error("expected PendingCallback to be retained while monitoring the active resume Job")
	}
}

// --- injectWellKnownLLMCredentials tests ---

const testLLMTokenKey = "NIRMATA_LLM_TOKEN"

func newTestReconcilerWithSecret(t *testing.T, secretData map[string][]byte, secretName string) (*WorkflowRunReconciler, *ottoflowv1alpha1.WorkflowRun) {
	t.Helper()
	ns := "team-a"
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "test-run", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf"}},
	}
	var objs []client.Object
	objs = append(objs, wr)
	if secretData != nil {
		objs = append(objs, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ns},
			Data:       secretData,
		})
	}
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(objs...).Build()
	r := &WorkflowRunReconciler{
		Client:        c,
		Scheme:        unitTestScheme,
		EventRecorder: events.NewFakeRecorder(10),
		RunnerConfig: RunnerConfig{
			LLMCredentialsSecret: secretName,
		},
	}
	return r, wr
}

func TestInjectWellKnownLLMCredentials_SecretAbsent(t *testing.T) {
	r, wr := newTestReconcilerWithSecret(t, nil, "ottoflow-llm-credentials")
	extras, err := r.injectWellKnownLLMCredentials(context.Background(), wr, nil, staticNeedsCreds(false))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(extras) != 0 {
		t.Errorf("expected no env vars when Secret is absent, got %d", len(extras))
	}
}

func TestInjectWellKnownLLMCredentials_FeatureDisabled(t *testing.T) {
	r, wr := newTestReconcilerWithSecret(t, map[string][]byte{testLLMTokenKey: []byte("tok")}, "")
	r.RunnerConfig.LLMCredentialsSecret = "" // disable
	extras, err := r.injectWellKnownLLMCredentials(context.Background(), wr, nil, staticNeedsCreds(false))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(extras) != 0 {
		t.Errorf("expected no env vars when feature is disabled, got %d", len(extras))
	}
}

func TestInjectWellKnownLLMCredentials_SecretPresent(t *testing.T) {
	r, wr := newTestReconcilerWithSecret(t,
		map[string][]byte{testLLMTokenKey: []byte("tok")},
		"ottoflow-llm-credentials",
	)
	extras, err := r.injectWellKnownLLMCredentials(context.Background(), wr, map[string]struct{}{}, staticNeedsCreds(false))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(extras) != 1 {
		t.Fatalf("expected 1 env var, got %d", len(extras))
	}
	ev := extras[0]
	if ev.Name != testLLMTokenKey {
		t.Errorf("expected env var name NIRMATA_LLM_TOKEN, got %q", ev.Name)
	}
	if ev.ValueFrom == nil || ev.ValueFrom.SecretKeyRef == nil {
		t.Fatal("expected secretKeyRef source")
	}
	if ev.ValueFrom.SecretKeyRef.Name != "ottoflow-llm-credentials" {
		t.Errorf("unexpected secretKeyRef.Name: %q", ev.ValueFrom.SecretKeyRef.Name)
	}
	if ev.ValueFrom.SecretKeyRef.Key != testLLMTokenKey {
		t.Errorf("unexpected secretKeyRef.Key: %q", ev.ValueFrom.SecretKeyRef.Key)
	}
}

func TestInjectWellKnownLLMCredentials_ExplicitCredsTakePrecedence(t *testing.T) {
	r, wr := newTestReconcilerWithSecret(t,
		map[string][]byte{testLLMTokenKey: []byte("secret-tok"), "OPENAI_API_KEY": []byte("openai-key")},
		"ottoflow-llm-credentials",
	)
	// NIRMATA_LLM_TOKEN is already set explicitly in spec.execution.job.env
	existing := map[string]struct{}{testLLMTokenKey: {}}
	extras, err := r.injectWellKnownLLMCredentials(context.Background(), wr, existing, staticNeedsCreds(false))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Only OPENAI_API_KEY should be injected; NIRMATA_LLM_TOKEN was already in existingEnvNames
	for _, ev := range extras {
		if ev.Name == testLLMTokenKey {
			t.Errorf("NIRMATA_LLM_TOKEN must not be injected when already explicit in job env")
		}
	}
	found := false
	for _, ev := range extras {
		if ev.Name == "OPENAI_API_KEY" {
			found = true
		}
	}
	if !found {
		t.Error("expected OPENAI_API_KEY to be injected from well-known Secret")
	}
}

func TestInjectWellKnownLLMCredentials_NonAllowlistKeyFiltered(t *testing.T) {
	r, wr := newTestReconcilerWithSecret(t,
		map[string][]byte{
			testLLMTokenKey:     []byte("tok"),
			"DATABASE_PASSWORD": []byte("should-be-ignored"),
			"RANDOM_SECRET_KEY": []byte("also-ignored"),
		},
		"ottoflow-llm-credentials",
	)
	extras, err := r.injectWellKnownLLMCredentials(context.Background(), wr, map[string]struct{}{}, staticNeedsCreds(false))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, ev := range extras {
		inAllowlist := false
		for _, allowed := range executor.LLMEnvAllowlist {
			if ev.Name == allowed {
				inAllowlist = true
				break
			}
		}
		if !inAllowlist {
			t.Errorf("injected env var %q is not in LLMEnvAllowlist", ev.Name)
		}
	}
	if len(extras) != 1 {
		t.Errorf("expected exactly 1 allowlisted key (NIRMATA_LLM_TOKEN), got %d", len(extras))
	}
}

func TestBuildWorkflowRunnerJob_InjectsWellKnownLLMCredentials(t *testing.T) {
	ns := "team-b"
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "cred-run", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf"}},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "ottoflow-llm-credentials", Namespace: ns},
		Data:       map[string][]byte{testLLMTokenKey: []byte("team-b-token")},
	}
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(wr, secret).Build()
	r := &WorkflowRunReconciler{
		Client: c,
		Scheme: unitTestScheme,
		RunnerConfig: RunnerConfig{
			RunnerServiceAccount: "controller-manager",
			LLMCredentialsSecret: "ottoflow-llm-credentials",
		},
	}
	job, err := r.buildWorkflowRunnerJob(context.Background(), wr, &ottoflowv1alpha1.Workflow{})
	if err != nil {
		t.Fatalf("buildWorkflowRunnerJob: %v", err)
	}
	env := job.Spec.Template.Spec.Containers[0].Env
	var injected *corev1.EnvVar
	for i := range env {
		if env[i].Name == testLLMTokenKey {
			injected = &env[i]
			break
		}
	}
	if injected == nil {
		t.Fatal("NIRMATA_LLM_TOKEN not found in runner Job env")
	}
	if injected.ValueFrom == nil || injected.ValueFrom.SecretKeyRef == nil {
		t.Fatal("NIRMATA_LLM_TOKEN must come from secretKeyRef, not plain Value")
	}
	if injected.ValueFrom.SecretKeyRef.Name != "ottoflow-llm-credentials" {
		t.Errorf("secretKeyRef.Name = %q", injected.ValueFrom.SecretKeyRef.Name)
	}
}

func TestBuildWorkflowRunnerJob_ExplicitEnvWinsOverWellKnownSecret(t *testing.T) {
	ns := "team-c"
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "explicit-run", Namespace: ns},
		Spec: ottoflowv1alpha1.WorkflowRunSpec{
			WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf"},
			Execution: &ottoflowv1alpha1.WorkflowRunExecutionSpec{
				Job: &ottoflowv1alpha1.WorkflowRunJobSpec{
					Env: []corev1.EnvVar{
						{Name: testLLMTokenKey, Value: "explicit-token"},
					},
				},
			},
		},
	}
	// Well-known Secret also has NIRMATA_LLM_TOKEN — explicit spec.execution.job.env must win.
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "ottoflow-llm-credentials", Namespace: ns},
		Data:       map[string][]byte{testLLMTokenKey: []byte("secret-token")},
	}
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(wr, secret).Build()
	r := &WorkflowRunReconciler{
		Client: c,
		Scheme: unitTestScheme,
		RunnerConfig: RunnerConfig{
			RunnerServiceAccount: "controller-manager",
			LLMCredentialsSecret: "ottoflow-llm-credentials",
		},
	}
	job, err := r.buildWorkflowRunnerJob(context.Background(), wr, &ottoflowv1alpha1.Workflow{})
	if err != nil {
		t.Fatalf("buildWorkflowRunnerJob: %v", err)
	}
	env := job.Spec.Template.Spec.Containers[0].Env
	var found []corev1.EnvVar
	for _, e := range env {
		if e.Name == testLLMTokenKey {
			found = append(found, e)
		}
	}
	if len(found) != 1 {
		t.Fatalf("expected exactly 1 NIRMATA_LLM_TOKEN in env, got %d", len(found))
	}
	if found[0].Value != "explicit-token" {
		t.Errorf("expected explicit-token to win, got value=%q secretKeyRef=%v", found[0].Value, found[0].ValueFrom)
	}
	if found[0].ValueFrom != nil {
		t.Error("explicit env must not have ValueFrom set (should be plain Value)")
	}
}

func TestInjectWellKnownLLMCredentials_PerRunSecretOverridesDefault(t *testing.T) {
	ns := "team-a"
	customSecretName := "my-custom-creds"
	r, wr := newTestReconcilerWithSecret(t,
		map[string][]byte{testLLMTokenKey: []byte("default-tok")},
		"ottoflow-llm-credentials",
	)
	// Add the custom Secret in the same namespace.
	customSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: customSecretName, Namespace: ns},
		Data:       map[string][]byte{testLLMTokenKey: []byte("custom-tok")},
	}
	if err := r.Create(context.Background(), customSecret); err != nil {
		t.Fatalf("create custom secret: %v", err)
	}
	// Set per-run override on the WorkflowRun.
	wr.Spec.Execution = &ottoflowv1alpha1.WorkflowRunExecutionSpec{
		LLMCredentialsSecret: &ottoflowv1alpha1.LLMCredentialsSecretRef{
			Name: customSecretName,
		},
	}
	extras, err := r.injectWellKnownLLMCredentials(context.Background(), wr, map[string]struct{}{}, staticNeedsCreds(false))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(extras) != 1 {
		t.Fatalf("expected 1 env var, got %d", len(extras))
	}
	if extras[0].ValueFrom.SecretKeyRef.Name != customSecretName {
		t.Errorf("expected secretKeyRef.Name=%q, got %q", customSecretName, extras[0].ValueFrom.SecretKeyRef.Name)
	}
}

func TestInjectWellKnownLLMCredentials_PerRunSecretDisablesDefault(t *testing.T) {
	// Setting spec.execution.llmCredentialsSecret to a non-existent Secret produces no env vars
	// (same as when the cluster-wide Secret is absent).
	r, wr := newTestReconcilerWithSecret(t,
		map[string][]byte{testLLMTokenKey: []byte("default-tok")},
		"ottoflow-llm-credentials",
	)
	wr.Spec.Execution = &ottoflowv1alpha1.WorkflowRunExecutionSpec{
		LLMCredentialsSecret: &ottoflowv1alpha1.LLMCredentialsSecretRef{
			Name: "nonexistent-secret",
		},
	}
	extras, err := r.injectWellKnownLLMCredentials(context.Background(), wr, map[string]struct{}{}, staticNeedsCreds(false))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(extras) != 0 {
		t.Errorf("expected no env vars when per-run Secret does not exist, got %d", len(extras))
	}
}

// --- WebhookServer helper unit tests ---

func computeHMACSig(t *testing.T, secret []byte, ts, path string, body []byte) string {
	t.Helper()
	mac := cryptohmac.New(cryptosha256.New, secret)
	mac.Write([]byte("v1:"))
	mac.Write([]byte(ts))
	mac.Write([]byte(":"))
	mac.Write([]byte(path))
	mac.Write([]byte(":"))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifyHMAC(t *testing.T) {
	secret := make([]byte, 32)
	for i := range secret {
		secret[i] = byte(i + 1)
	}
	ts := "1700000000"
	path := "/webhooks/default/my-workflow"
	body := []byte(`{"key":"value"}`)
	goodSig := computeHMACSig(t, secret, ts, path, body)

	if err := verifyHMAC(secret, ts, path, body, goodSig); err != nil {
		t.Errorf("valid sig rejected: %v", err)
	}
	// Bad sig
	if err := verifyHMAC(secret, ts, path, body, "sha256=deadbeef"); err == nil {
		t.Error("bad sig should be rejected")
	}
	// Missing sha256= prefix
	if err := verifyHMAC(secret, ts, path, body, goodSig[7:]); err == nil {
		t.Error("missing sha256= prefix should be rejected")
	}
	// Tampered body
	if err := verifyHMAC(secret, ts, path, []byte(`{"key":"evil"}`), goodSig); err == nil {
		t.Error("tampered body should be rejected")
	}
	// Different path (cross-endpoint replay prevention)
	if err := verifyHMAC(secret, ts, "/webhooks/other/workflow", body, goodSig); err == nil {
		t.Error("different path should be rejected")
	}
}

func TestVerifyTimestamp(t *testing.T) {
	now := strconv.FormatInt(time.Now().Unix(), 10)
	if err := verifyTimestamp(now); err != nil {
		t.Errorf("current timestamp should pass: %v", err)
	}
	// Stale (10 minutes ago — exceeds webhookTimestampWindow of 5 minutes)
	stale := strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)
	if err := verifyTimestamp(stale); err == nil {
		t.Error("stale timestamp should be rejected")
	}
	// Future (10 minutes ahead — exceeds webhookTimestampWindow of 5 minutes)
	future := strconv.FormatInt(time.Now().Add(10*time.Minute).Unix(), 10)
	if err := verifyTimestamp(future); err == nil {
		t.Error("future timestamp should be rejected")
	}
	// Missing
	if err := verifyTimestamp(""); err == nil {
		t.Error("empty timestamp should be rejected")
	}
	// Non-numeric
	if err := verifyTimestamp("not-a-number"); err == nil {
		t.Error("non-numeric timestamp should be rejected")
	}
}

func TestParseWebhookPath(t *testing.T) {
	tests := []struct {
		path     string
		wantNS   string
		wantName string
		wantOK   bool
	}{
		{"/webhooks/default/my-wf", "default", "my-wf", true},
		{"/webhooks/prod/compliance-scan", "prod", "compliance-scan", true},
		{"/webhooks/ns/name/extra", "", "", false}, // extra segment
		{"/webhooks/ns/", "", "", false},           // empty name
		{"/webhooks//name", "", "", false},         // empty ns
		{"/webhooks/", "", "", false},              // no segments
		{"/other/default/name", "", "", false},     // wrong prefix
		{"webhooks/default/name", "", "", false},   // no leading slash
	}
	for _, tt := range tests {
		ns, name, ok := parseWebhookPath(tt.path)
		if ok != tt.wantOK {
			t.Errorf("parseWebhookPath(%q) ok=%v want %v", tt.path, ok, tt.wantOK)
			continue
		}
		if ok && (ns != tt.wantNS || name != tt.wantName) {
			t.Errorf("parseWebhookPath(%q) = (%q,%q) want (%q,%q)", tt.path, ns, name, tt.wantNS, tt.wantName)
		}
	}
}

// --- Regression tests for self-amplification follow-ups: WorkflowRun self-amplification guard,
// GVK-comparison correctness, and label-selector append safety. ---

// workflowRunEventObject builds an unstructured WorkflowRun event object owned by owner,
// as a real dynamic-client watch event for a WorkflowRun CRD in the given apiVersion
// would look. Pass ottoflowv1alpha1.GroupVersion.String() for OttoFlow's own type, or
// a different apiVersion (e.g. "v1") to build a same-named type in another API group.
func workflowRunEventObject(name string, owner *ottoflowv1alpha1.Workflow, apiVersion string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": apiVersion,
		"kind":       "WorkflowRun",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": owner.Namespace,
			"uid":       name + "-uid",
			"ownerReferences": []interface{}{
				map[string]interface{}{
					"apiVersion": ottoflowv1alpha1.GroupVersion.String(),
					"kind":       "Workflow",
					"name":       owner.Name,
					"uid":        string(owner.UID),
					"controller": true,
				},
			},
		},
	}}
}

// TestCreateWorkflowRunFromEvent_ExcludesOwnWorkflowRun is the regression test for the
// WorkflowRun-kind self-amplification guard: a trigger watching kind: WorkflowRun with
// no filter must not create a new run for a WorkflowRun that this same Workflow already
// created — otherwise every run it spawns re-fires the trigger, unbounded (the same
// self-amplification bug class, but for triggers on WorkflowRun itself rather than the
// Pods/Jobs a run creates). This must be RED without isOwnWorkflowRun's guard call site
// and GREEN with it — see the fix report for the paired before/after `go test` output.
func TestCreateWorkflowRunFromEvent_ExcludesOwnWorkflowRun(t *testing.T) {
	ctx := context.Background()
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default", UID: "wf-uid-abc"},
		Spec:       ottoflowv1alpha1.WorkflowSpec{Steps: []ottoflowv1alpha1.Step{{Name: "s1", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `"ok"`}}}}},
	}
	eventSpec := &ottoflowv1alpha1.EventTrigger{
		Resources:  []ottoflowv1alpha1.EventResource{{APIVersion: ottoflowv1alpha1.GroupVersion.String(), Kind: "WorkflowRun", Namespace: "default"}},
		Operations: []string{"CREATE"},
	}
	// A run "created by" this same workflow (ownerRef points back at wf).
	obj := workflowRunEventObject("wf-abc123-deadbeef", wf, ottoflowv1alpha1.GroupVersion.String())

	fakeClient := makeFakeClient(wf)
	tm := NewTriggerManager(fakeClient, unitTestScheme, nil)
	if err := tm.CreateWorkflowRunFromEvent(ctx, "test-trigger-wr-selfexcl", wf, eventSpec, obj, "ADDED"); err != nil {
		t.Fatal(err)
	}

	var list ottoflowv1alpha1.WorkflowRunList
	if err := fakeClient.List(ctx, &list, client.InNamespace("default"), client.MatchingLabels{"ottoflow.nirmata.io/workflow": "wf"}); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 0 {
		t.Errorf("expected no WorkflowRun created from an event on the workflow's own run, got %d", len(list.Items))
	}
}

// TestCreateWorkflowRunFromEvent_WorkflowRunGuard_DifferentGroup_NotExcluded verifies the
// self-amplification guard compares the full GroupKind, not just the Kind string: a
// same-named "WorkflowRun" kind in a different (here: core, empty-string) API group must
// NOT be treated as OttoFlow's own type, even if it happens to carry a matching
// ownerReference by coincidence. This is the GVK edge-case check called for in the task —
// it is expected to pass both before and after the guard exists (the guard, correctly
// implemented, must never fire here), so it is coverage for guard *specificity*, not a
// red/green regression test.
func TestCreateWorkflowRunFromEvent_WorkflowRunGuard_DifferentGroup_NotExcluded(t *testing.T) {
	ctx := context.Background()
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default", UID: "wf-uid-abc"},
		Spec:       ottoflowv1alpha1.WorkflowSpec{Steps: []ottoflowv1alpha1.Step{{Name: "s1", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `"ok"`}}}}},
	}
	eventSpec := &ottoflowv1alpha1.EventTrigger{
		Resources:  []ottoflowv1alpha1.EventResource{{APIVersion: "v1", Kind: "WorkflowRun", Namespace: "default"}},
		Operations: []string{"CREATE"},
	}
	// Same Kind string ("WorkflowRun") and, deliberately, a matching ownerReference —
	// but apiVersion "v1" means Group == "" (core), not ottoflow.nirmata.io. A guard
	// that compared Kind alone would wrongly exclude this; GroupKind comparison must not.
	obj := workflowRunEventObject("spoofed-core-workflowrun", wf, "v1")

	fakeClient := makeFakeClient(wf)
	tm := NewTriggerManager(fakeClient, unitTestScheme, nil)
	if err := tm.CreateWorkflowRunFromEvent(ctx, "test-trigger-wr-group-edge", wf, eventSpec, obj, "ADDED"); err != nil {
		t.Fatal(err)
	}

	var list ottoflowv1alpha1.WorkflowRunList
	if err := fakeClient.List(ctx, &list, client.InNamespace("default"), client.MatchingLabels{"ottoflow.nirmata.io/workflow": "wf"}); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Errorf("expected a WorkflowRun to be created (different API group must not match the WorkflowRun guard), got %d", len(list.Items))
	}
}

// TestWatchResource_LabelSelectorExclusion_PreservesMatchExpressions verifies the
// runner-label exclusion appended in watchResource is safe for a user-supplied selector
// built from matchExpressions (not just matchLabels): the resulting selector string must
// still parse, must still match objects satisfying the user's matchExpressions, and must
// still exclude OttoFlow-managed objects. This is coverage for an existing, already-
// correct code path (the append operates on the selector's *string* form, which is
// syntax-identical for matchLabels and matchExpressions) — it is expected to pass, not a
// red/green regression test.
func TestWatchResource_LabelSelectorExclusion_PreservesMatchExpressions(t *testing.T) {
	ctx := context.Background()
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec:       ottoflowv1alpha1.WorkflowSpec{Steps: []ottoflowv1alpha1.Step{{Name: "s1", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `"ok"`}}}}},
	}
	eventSpec := &ottoflowv1alpha1.EventTrigger{
		Resources: []ottoflowv1alpha1.EventResource{{APIVersion: "v1", Kind: "Pod", Namespace: "default"}},
		LabelSelector: &metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: "environment", Operator: metav1.LabelSelectorOpIn, Values: []string{"prod", "staging"}},
			},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(wf).Build()
	factory := &fakeWatcherFactory{}
	tm := NewTriggerManagerWithWatcherFactory(fakeClient, unitTestScheme, nil, factory)

	if err := tm.watchResource(ctx, "wf-key-resource-0", "wf-key", wf, eventSpec.Resources[0], eventSpec); err != nil {
		t.Fatalf("watchResource returned error: %v", err)
	}

	got := factory.lastOpts.LabelSelector
	if got == "" {
		t.Fatal("LabelSelector should not be empty")
	}

	parsed, err := labels.Parse(got)
	if err != nil {
		t.Fatalf("computed selector %q must be valid, got parse error: %v", got, err)
	}

	if !strings.Contains(got, "!"+runnerManagedLabel) {
		t.Errorf("computed selector %q must exclude %s", got, runnerManagedLabel)
	}

	matching := labels.Set{"environment": "prod"}
	if !parsed.Matches(matching) {
		t.Errorf("selector %q should still match an object satisfying the user's matchExpressions", got)
	}
	runnerManaged := labels.Set{"environment": "prod", runnerManagedLabel: "some-run"}
	if parsed.Matches(runnerManaged) {
		t.Errorf("selector %q should exclude an OttoFlow-managed object even if it matches the user's matchExpressions", got)
	}
}

// TestWatchResource_LabelSelectorExclusion_WorkflowRunKind verifies the
// WorkflowRun-specific branch in watchResource: when the watched GVK's GroupKind
// matches workflowRunGroupKind, the computed selector must additionally exclude
// this Workflow's own runs via "ottoflow.nirmata.io/workflow!=<workflow.Name>",
// on top of the runner-managed-label exclusion applied to every watch. This is
// coverage for be75f67's server-side loop-prevention guard for triggers that
// watch kind: WorkflowRun directly.
func TestWatchResource_LabelSelectorExclusion_WorkflowRunKind(t *testing.T) {
	ctx := context.Background()
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec:       ottoflowv1alpha1.WorkflowSpec{Steps: []ottoflowv1alpha1.Step{{Name: "s1", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `"ok"`}}}}},
	}
	eventSpec := &ottoflowv1alpha1.EventTrigger{
		Resources: []ottoflowv1alpha1.EventResource{{APIVersion: "ottoflow.nirmata.io/v1alpha1", Kind: "WorkflowRun", Namespace: "default"}},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(wf).Build()
	factory := &fakeWatcherFactory{}
	tm := NewTriggerManagerWithWatcherFactory(fakeClient, unitTestScheme, nil, factory)

	if err := tm.watchResource(ctx, "wf-key-resource-0", "wf-key", wf, eventSpec.Resources[0], eventSpec); err != nil {
		t.Fatalf("watchResource returned error: %v", err)
	}

	got := factory.lastOpts.LabelSelector
	if got == "" {
		t.Fatal("LabelSelector should not be empty")
	}

	parsed, err := labels.Parse(got)
	if err != nil {
		t.Fatalf("computed selector %q must be valid, got parse error: %v", got, err)
	}

	wantExclusion := "ottoflow.nirmata.io/workflow!=" + wf.Name
	if !strings.Contains(got, wantExclusion) {
		t.Errorf("computed selector %q must exclude this Workflow's own runs via %q", got, wantExclusion)
	}

	// A legitimate WorkflowRun belonging to a *different* workflow must still match.
	otherWorkflowRun := labels.Set{"ottoflow.nirmata.io/workflow": "some-other-workflow"}
	if !parsed.Matches(otherWorkflowRun) {
		t.Errorf("selector %q should still match a WorkflowRun that isn't this Workflow's own", got)
	}

	// A WorkflowRun belonging to this Workflow must be excluded.
	ownWorkflowRun := labels.Set{"ottoflow.nirmata.io/workflow": wf.Name}
	if parsed.Matches(ownWorkflowRun) {
		t.Errorf("selector %q should exclude this Workflow's own runs (label ottoflow.nirmata.io/workflow=%s)", got, wf.Name)
	}
}

func staticNeedsCreds(v bool) func() (bool, error) { return func() (bool, error) { return v, nil } }

func secretGetErrorReader(base client.WithWatch, match func(key client.ObjectKey) bool, errFor func(key client.ObjectKey) error) client.WithWatch {
	return interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.Secret); ok && (match == nil || match(key)) {
				return errFor(key)
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
}
