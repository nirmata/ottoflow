package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

func drain(t *testing.T, rec *events.FakeRecorder) []string {
	t.Helper()
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

// Unit level: the function fires the Warning with no Secret configured (the default install).
func TestInjectWellKnownLLMCredentials_WarnsOnPublicProviderKeyInJobEnv(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]struct{}
		want string // substring expected in the single Warning; "" = no event at all
	}{
		{"openai key in job.env, no Secret configured", map[string]struct{}{"OPENAI_API_KEY": {}}, "OPENAI_API_KEY"},
		{"two public keys, sorted in one event", map[string]struct{}{"OPENAI_API_KEY": {}, "GEMINI_API_KEY": {}}, "GEMINI_API_KEY, OPENAI_API_KEY"},
		{"nirmata token only", map[string]struct{}{"NIRMATA_LLM_TOKEN": {}}, ""},
		{"empty job.env", map[string]struct{}{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, wr := newTestReconcilerWithSecret(t, nil, "")
			rec := r.EventRecorder.(*events.FakeRecorder)
			extras, err := r.injectWellKnownLLMCredentials(context.Background(), wr, tc.env, staticNeedsCreds(false))
			if err != nil || extras != nil {
				t.Fatalf("extras=%v err=%v", extras, err)
			}
			evs := drain(t, rec)
			if tc.want == "" {
				if len(evs) != 0 {
					t.Fatalf("expected no event, got %v", evs)
				}
				return
			}
			if len(evs) != 1 || !strings.HasPrefix(evs[0], "Warning LLMCredentialIgnored") || !strings.Contains(evs[0], tc.want) {
				t.Fatalf("expected one Warning LLMCredentialIgnored naming %q, got %v", tc.want, evs)
			}
		})
	}
}

// Call-site level: through buildWorkflowRunnerJob, so existingEnvNames really is job.env.
func TestBuildWorkflowRunnerJob_WarnsOnPublicProviderKeyInJobEnv(t *testing.T) {
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "warn-run", Namespace: "team-d"},
		Spec: ottoflowv1alpha1.WorkflowRunSpec{
			WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf"},
			Execution: &ottoflowv1alpha1.WorkflowRunExecutionSpec{
				Job: &ottoflowv1alpha1.WorkflowRunJobSpec{
					Env: []corev1.EnvVar{{Name: "OPENAI_API_KEY", Value: "sk-not-read"}},
				},
			},
		},
	}
	rec := events.NewFakeRecorder(10)
	r := &WorkflowRunReconciler{
		Client:        fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(wr).Build(),
		Scheme:        unitTestScheme,
		EventRecorder: rec,
		RunnerConfig:  RunnerConfig{RunnerServiceAccount: "controller-manager"}, // no well-known Secret
	}
	job, err := r.buildWorkflowRunnerJob(context.Background(), wr, &ottoflowv1alpha1.Workflow{})
	if err != nil {
		t.Fatalf("buildWorkflowRunnerJob: %v", err)
	}
	// Still mounted (job.env is the author's), just warned about.
	var mounted bool
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "OPENAI_API_KEY" {
			mounted = true
		}
	}
	if !mounted {
		t.Fatal("OPENAI_API_KEY from spec.execution.job.env should still be on the runner pod")
	}
	evs := drain(t, rec)
	if len(evs) != 1 || !strings.HasPrefix(evs[0], "Warning LLMCredentialIgnored") || !strings.Contains(evs[0], "OPENAI_API_KEY") {
		t.Fatalf("expected exactly one Warning LLMCredentialIgnored naming OPENAI_API_KEY, got %v", evs)
	}
	if strings.Contains(evs[0], "sk-not-read") {
		t.Fatalf("event must carry names only, got %q", evs[0])
	}
}

// Plan item 3: a public-provider key in the well-known Secret is skipped and named in the
// existing LLMCredentialInjection Warning.
func TestInjectWellKnownLLMCredentials_PublicProviderKeyInSecretIsReportedIgnored(t *testing.T) {
	r, wr := newTestReconcilerWithSecret(t,
		map[string][]byte{testLLMTokenKey: []byte("tok"), "OPENAI_API_KEY": []byte("openai-key")},
		"ottoflow-llm-credentials")
	rec := r.EventRecorder.(*events.FakeRecorder)
	extras, err := r.injectWellKnownLLMCredentials(context.Background(), wr, map[string]struct{}{}, staticNeedsCreds(false))
	if err != nil {
		t.Fatal(err)
	}
	if len(extras) != 1 || extras[0].Name != testLLMTokenKey {
		t.Fatalf("expected only %s injected, got %v", testLLMTokenKey, extras)
	}
	evs := drain(t, rec)
	if len(evs) != 1 || !strings.HasPrefix(evs[0], "Warning LLMCredentialInjection") || !strings.Contains(evs[0], "ignored: OPENAI_API_KEY") {
		t.Fatalf("expected one LLMCredentialInjection Warning naming OPENAI_API_KEY as ignored, got %v", evs)
	}
	if strings.Contains(evs[0], "openai-key") {
		t.Fatalf("event must carry names only, got %q", evs[0])
	}
}
