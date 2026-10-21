/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

// mountCheckEnv scripts the controller's view of the API: the access review verdict and the
// result of every Secret Get, counting what the check asked for per "namespace/name".
type mountCheckEnv struct {
	review  string // "allow", "deny" or "err"
	getErr  error
	reviews map[string]int
	gets    map[string]int
}

func (e *mountCheckEnv) client(objs ...client.Object) client.Client {
	e.reviews, e.gets = map[string]int{}, map[string]int{}
	base := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(&ottoflowv1alpha1.WorkflowRun{}).WithObjects(objs...).Build()
	return interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			ssar, ok := obj.(*authorizationv1.SelfSubjectAccessReview)
			if !ok {
				return c.Create(ctx, obj, opts...)
			}
			a := ssar.Spec.ResourceAttributes
			e.reviews[a.Namespace+"/"+a.Name]++
			switch e.review {
			case "err":
				return errors.New("review unavailable")
			case "deny":
				ssar.Status = authorizationv1.SubjectAccessReviewStatus{Reason: "reason-sentinel"}
			default:
				ssar.Status = authorizationv1.SubjectAccessReviewStatus{Allowed: true}
			}
			return nil
		},
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.Secret); ok {
				e.gets[key.Namespace+"/"+key.Name]++
				if e.getErr != nil {
					return e.getErr
				}
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
}

func externalAgentFixture() (*ottoflowv1alpha1.Workflow, *ottoflowv1alpha1.WorkflowRun) {
	ns := defaultNamespace
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: ns},
		Spec: ottoflowv1alpha1.WorkflowSpec{Steps: []ottoflowv1alpha1.Step{{
			Name: "callAgent",
			ExternalAgentRef: &ottoflowv1alpha1.StepExternalAgentRef{
				URL: "https://agent.example.com", Prompt: "'hi'",
				Auth: &ottoflowv1alpha1.ExternalAgentAuth{SecretRef: &ottoflowv1alpha1.SecretReference{Name: "agent-token", Key: "token"}},
			},
		}}},
	}
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: ns}},
	}
	return wf, wr
}

func (e *mountCheckEnv) reconciler(rec events.EventRecorder, cfg RunnerConfig, objs ...client.Object) (*WorkflowRunReconciler, client.Client) {
	c := e.client(objs...)
	return &WorkflowRunReconciler{Client: c, Scheme: unitTestScheme, RunnerConfig: cfg, EventRecorder: rec}, c
}

func (e *mountCheckEnv) build(t *testing.T, rec events.EventRecorder, cfg RunnerConfig, wf *ottoflowv1alpha1.Workflow, wr *ottoflowv1alpha1.WorkflowRun, extra ...client.Object) error {
	t.Helper()
	r, _ := e.reconciler(rec, cfg, append([]client.Object{wf, wr}, extra...)...)
	_, err := r.buildWorkflowRunnerJob(context.Background(), wr, wf)
	return err
}

// reconcileRun runs one Reconcile and returns the stored run.
func (e *mountCheckEnv) reconcileRun(t *testing.T, wf *ottoflowv1alpha1.Workflow, wr *ottoflowv1alpha1.WorkflowRun, extra ...client.Object) *ottoflowv1alpha1.WorkflowRun {
	t.Helper()
	r, c := e.reconciler(nil, RunnerConfig{}, append([]client.Object{wf, wr}, extra...)...)
	_, _ = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(wr)})
	got := &ottoflowv1alpha1.WorkflowRun{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(wr), got); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestSecretMountCheck_MissingSecretFailsRunNamingField(t *testing.T) {
	e := &mountCheckEnv{review: "allow"}
	wf, wr := externalAgentFixture()
	err := e.build(t, nil, RunnerConfig{}, wf, wr)
	if err == nil || !strings.Contains(err.Error(), "externalAgentRef.auth.secretRef") {
		t.Fatalf("want an error naming the workflow field, got %v", err)
	}
	if got := secretMountFailureReason(err); got != ottoflowv1alpha1.WorkflowRunFailureReasonRunnerRefUnresolved {
		t.Fatalf("failure reason = %q", got)
	}
	got := e.reconcileRun(t, wf, wr)
	if got.Status.Phase != ottoflowv1alpha1.WorkflowRunPhaseFailed || got.Status.FailureReason != ottoflowv1alpha1.WorkflowRunFailureReasonRunnerRefUnresolved ||
		!strings.Contains(got.Status.Message, "externalAgentRef.auth.secretRef") {
		t.Fatalf("run not failed with RunnerRefUnresolved naming the field: %+v", got.Status)
	}
}

func TestSecretMountCheck_MissingKeyNamesOnlyThatKey(t *testing.T) {
	e := &mountCheckEnv{review: "allow"}
	wf, wr := externalAgentFixture()
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "agent-token", Namespace: defaultNamespace},
		Data: map[string][]byte{"other": []byte("v-sentinel"), "zz-second": []byte("v2")}}
	err := e.build(t, nil, RunnerConfig{}, wf, wr, s)
	if err == nil || !strings.Contains(err.Error(), `"token"`) {
		t.Fatalf("want an error naming key token, got %v", err)
	}
	got := e.reconcileRun(t, wf, wr, s)
	for _, text := range []string{err.Error(), got.Status.Message} {
		for k, v := range s.Data {
			if strings.Contains(text, k) || strings.Contains(text, string(v)) {
				t.Fatalf("message leaks fixture key or value %q: %s", k, text)
			}
		}
		if strings.Contains(text, "reason-sentinel") {
			t.Fatalf("message carries review reason text: %s", text)
		}
	}
}

func TestSecretMountCheck_PresentSecretAndKeyBuilds(t *testing.T) {
	e := &mountCheckEnv{review: "allow"}
	wf, wr := externalAgentFixture()
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "agent-token", Namespace: defaultNamespace}, Data: map[string][]byte{"token": []byte("v")}}
	rec := events.NewFakeRecorder(10)
	if err := e.build(t, rec, RunnerConfig{}, wf, wr, s); err != nil {
		t.Fatalf("present Secret and key must build: %v", err)
	}
	if got := drain(t, rec); len(got) != 0 {
		t.Fatalf("a fully checked run records no Event, got %v", got)
	}
}

func TestSecretMountCheck_ReadErrorAfterAllowedReviewIsNotChecked(t *testing.T) {
	for name, getErr := range map[string]error{
		"Forbidden": apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "agent-token", errors.New("rbac changed")),
		"500":       apierrors.NewInternalError(errors.New("boom")),
	} {
		t.Run(name, func(t *testing.T) {
			e := &mountCheckEnv{review: "allow", getErr: getErr}
			wf, wr := externalAgentFixture()
			if err := e.build(t, nil, RunnerConfig{}, wf, wr); err != nil {
				t.Fatalf("a %s read after an allowed review must build the Job in the same call, got %v", name, err)
			}
			got := e.reconcileRun(t, wf, wr)
			if got.Status.FailureReason != "" || strings.Contains(got.Status.Message, "rbac.secretAccess") {
				t.Fatalf("a %s read must not fail the run or point at RBAC: %+v", name, got.Status)
			}
		})
	}
}

func TestSecretMountCheck_AllowlistedCrossNamespaceCredentialNeverProbed(t *testing.T) {
	e := &mountCheckEnv{review: "allow"}
	ns, other := defaultNamespace, "other-ns"
	agent := &ottoflowv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Namespace: other, Name: "agent1"},
		Spec: ottoflowv1alpha1.AgentSpec{Prompt: "p", ModelProvider: "openai", MCPTools: []string{"srv:do"}}}
	srv := &ottoflowv1alpha1.MCPServer{ObjectMeta: metav1.ObjectMeta{Namespace: other, Name: "srv"},
		Spec: ottoflowv1alpha1.MCPServerSpec{Auth: &ottoflowv1alpha1.AuthConfig{Type: "bearer",
			SecretRef: &ottoflowv1alpha1.SecretReference{Name: "mcp-token", Key: "token"}}}}
	wf := &ottoflowv1alpha1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: ns},
		Spec: ottoflowv1alpha1.WorkflowSpec{Steps: []ottoflowv1alpha1.Step{{Name: "a",
			AgentRef: &ottoflowv1alpha1.StepAgentRef{Name: "agent1", Namespace: other}}}}}
	wr := &ottoflowv1alpha1.WorkflowRun{ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: ns},
		Spec: ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: ns}}}
	cfg := RunnerConfig{SecretRefAllowedNamespaces: map[string]struct{}{other: {}}}
	if err := e.build(t, nil, cfg, wf, wr, agent, srv); err != nil {
		t.Fatalf("build: %v", err)
	}
	if n, g := e.reviews[other+"/mcp-token"], e.gets[other+"/mcp-token"]; n != 0 || g != 0 {
		t.Fatalf("cross-namespace ref was probed: reviews=%d gets=%d", n, g)
	}
}

func TestSecretMountCheck_DeniedOrInconclusiveReviewIsNotChecked(t *testing.T) {
	for _, review := range []string{"deny", "err"} {
		t.Run(review, func(t *testing.T) {
			e := &mountCheckEnv{review: review}
			wf, wr := externalAgentFixture()
			rec := events.NewFakeRecorder(10)
			if err := e.build(t, rec, RunnerConfig{}, wf, wr); err != nil {
				t.Fatalf("want Job built, got %v", err)
			}
			if g := e.gets[defaultNamespace+"/agent-token"]; g != 0 {
				t.Fatalf("no Get without an allowed review, got %d", g)
			}
			got := drain(t, rec)
			if len(got) != 1 || !strings.HasPrefix(got[0], "Normal SecretMountsNotChecked ") || !strings.Contains(got[0], "externalAgentRef.auth.secretRef") {
				t.Fatalf("want exactly one SecretMountsNotChecked Normal Event naming the field, got %v", got)
			}
			if strings.Contains(got[0], "reason-sentinel") {
				t.Fatalf("Event carries review reason text: %s", got[0])
			}
		})
	}
}

func TestSecretMountCheck_NilRecorderDoesNotPanic(t *testing.T) {
	e := &mountCheckEnv{review: "deny"}
	wf, wr := externalAgentFixture()
	if err := e.build(t, nil, RunnerConfig{}, wf, wr); err != nil {
		t.Fatalf("want Job built, got %v", err)
	}
}

func TestSecretMountCheck_OptionalVolumeSkippedEntirely(t *testing.T) {
	e := &mountCheckEnv{review: "allow"}
	ns := defaultNamespace
	srv := &ottoflowv1alpha1.MCPServer{ObjectMeta: metav1.ObjectMeta{Name: "gh", Namespace: ns},
		Spec: ottoflowv1alpha1.MCPServerSpec{
			Transport: ottoflowv1alpha1.TransportConfig{Type: "stdio", Command: []string{"gh-mcp"}},
			Env: []corev1.EnvVar{{Name: "TOKEN", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "opt-secret"}, Key: "token", Optional: ptr.To(true)}}}},
		}}
	wf := &ottoflowv1alpha1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: ns},
		Spec: ottoflowv1alpha1.WorkflowSpec{Steps: []ottoflowv1alpha1.Step{{Name: "c", MCPToolCall: &ottoflowv1alpha1.StepMCPToolCall{Server: "gh", Tool: "t"}}}}}
	wr := &ottoflowv1alpha1.WorkflowRun{ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: ns},
		Spec: ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: ns}}}
	rec := events.NewFakeRecorder(10)
	// The Secret exists without the key: a mandatory volume would fail the run.
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "opt-secret", Namespace: ns}, Data: map[string][]byte{"x": []byte("y")}}
	if err := e.build(t, rec, RunnerConfig{}, wf, wr, srv, s); err != nil {
		t.Fatalf("optional volume must not fail the run: %v", err)
	}
	if n, g := e.reviews[ns+"/opt-secret"], e.gets[ns+"/opt-secret"]; n != 0 || g != 0 {
		t.Fatalf("optional volume was probed: reviews=%d gets=%d", n, g)
	}
	if got := drain(t, rec); len(got) != 0 {
		t.Fatalf("optional volume must not be reported, got %v", got)
	}
}

func TestSecretMountCheck_EventNoteCapped(t *testing.T) {
	rec := events.NewFakeRecorder(10)
	r := &WorkflowRunReconciler{EventRecorder: rec}
	origins := make([]string, 40)
	for i := range origins {
		origins[i] = fmt.Sprintf("workflowRun.spec.step-%02d -> workflow %q step %q auth.secretRef", i, strings.Repeat("w", 30), strings.Repeat("s", 30))
	}
	r.recordUncheckedSecretMounts(&ottoflowv1alpha1.WorkflowRun{}, origins)
	got := drain(t, rec)
	if len(got) != 1 {
		t.Fatalf("want exactly one Event, got %d", len(got))
	}
	note := strings.TrimPrefix(got[0], "Normal SecretMountsNotChecked ")
	if len(note) > maxUncheckedEventNote || !strings.Contains(note, " more") {
		t.Fatalf("note must be <= %d bytes and end with the remainder count; len=%d %q", maxUncheckedEventNote, len(note), note)
	}
}

func TestCheckSecretMounts_NamespaceIsTheRunsNamespace(t *testing.T) {
	e := &mountCheckEnv{review: "allow"}
	c := e.client()
	r := &WorkflowRunReconciler{Client: c}
	vols := []corev1.Volume{{Name: "v", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "s"}}}}
	if _, err := r.checkSecretMounts(context.Background(), "tenant", vols, map[string]string{"v": "o"}); err == nil {
		t.Fatal("want NotFound failure")
	}
	if e.gets["tenant/s"] != 1 || e.reviews["tenant/s"] != 1 {
		t.Fatalf("review/Get must target the run namespace: %v %v", e.reviews, e.gets)
	}
}
