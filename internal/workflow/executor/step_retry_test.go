/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/agent"
)

// flakyAgentExecutor fails the first failFirst calls with failErr, then succeeds.
// When block is true, every call blocks until ctx is done.
type flakyAgentExecutor struct {
	calls     atomic.Int32
	failFirst int32
	failErr   error
	block     bool
}

func (f *flakyAgentExecutor) ExecuteAgent(ctx context.Context, _ *ottoflowv1alpha1.Agent, _ string, _ map[string]interface{}, _ string) (string, agent.AgentTokenUsage, error) {
	n := f.calls.Add(1)
	if f.block {
		<-ctx.Done()
		return "", agent.AgentTokenUsage{}, ctx.Err()
	}
	if n <= f.failFirst {
		return "", agent.AgentTokenUsage{}, f.failErr
	}
	return "ok", agent.AgentTokenUsage{}, nil
}

func newRetryTestExecutor(t *testing.T, agentExec agent.AgentExecutor) (*WorkflowExecutor, *ottoflowv1alpha1.WorkflowRun) {
	t.Helper()
	scheme := runtime.NewScheme()
	utilruntime.Must(ottoflowv1alpha1.AddToScheme(scheme))
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	agentCRD := &ottoflowv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "test-agent", Namespace: "default"},
		Spec:       ottoflowv1alpha1.AgentSpec{Prompt: "p", ModelProvider: "openai", ModelName: "gpt-4"},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(agentCRD).Build()
	run := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "test-run", Namespace: "default"},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "test-workflow"}},
	}
	exec, err := NewWorkflowExecutorWithAgentExecutor(k8sClient, nil, nil, nil, run, agentExec, true, 0, 5, nil) // local mode: call agentExec in-process
	if err != nil {
		t.Fatalf("NewWorkflowExecutorWithAgentExecutor: %v", err)
	}
	return exec, run
}

func agentWorkflow(retry *ottoflowv1alpha1.RetryPolicy, timeout string) *ottoflowv1alpha1.Workflow {
	return &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "test-workflow", Namespace: "default"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{{
				Name:     "analyze",
				AgentRef: &ottoflowv1alpha1.StepAgentRef{Name: "test-agent"},
				Retry:    retry,
				Timeout:  timeout,
			}},
		},
	}
}

var noBackoff = &ottoflowv1alpha1.BackoffConfig{Strategy: "none"}

func TestStepRetry_SucceedsAfterTransientFailures(t *testing.T) {
	agentExec := &flakyAgentExecutor{failFirst: 2, failErr: errors.New("upstream 503")}
	exec, run := newRetryTestExecutor(t, agentExec)

	err := exec.ExecuteWorkflow(context.Background(), agentWorkflow(&ottoflowv1alpha1.RetryPolicy{Attempts: 3, Backoff: noBackoff}, ""), run)
	if err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if got := agentExec.calls.Load(); got != 3 {
		t.Errorf("agent calls = %d, want 3", got)
	}
	st := run.Status.StepStatuses["analyze"]
	if st.Phase != ottoflowv1alpha1.StepPhaseSucceeded {
		t.Errorf("step phase = %s, want Succeeded", st.Phase)
	}
	if st.RetryCount != 2 || st.LastRetryTime == nil {
		t.Errorf("retryCount = %d, lastRetryTime = %v; want 2 and set", st.RetryCount, st.LastRetryTime)
	}
}

func TestStepRetry_ExhaustsAttempts(t *testing.T) {
	agentExec := &flakyAgentExecutor{failFirst: 10, failErr: errors.New("boom")}
	exec, run := newRetryTestExecutor(t, agentExec)

	err := exec.ExecuteWorkflow(context.Background(), agentWorkflow(&ottoflowv1alpha1.RetryPolicy{Attempts: 3, Backoff: noBackoff}, ""), run)
	if err == nil {
		t.Fatal("expected failure after exhausting retries")
	}
	if got := agentExec.calls.Load(); got != 3 {
		t.Errorf("agent calls = %d, want 3", got)
	}
	if st := run.Status.StepStatuses["analyze"]; st.Phase != ottoflowv1alpha1.StepPhaseFailed {
		t.Errorf("step phase = %s, want Failed", st.Phase)
	}
	if run.Status.Phase != ottoflowv1alpha1.WorkflowRunPhaseFailed {
		t.Errorf("run phase = %s, want Failed", run.Status.Phase)
	}
}

func TestStepRetry_NoPolicyRunsOnce(t *testing.T) {
	agentExec := &flakyAgentExecutor{failFirst: 1, failErr: errors.New("boom")}
	exec, run := newRetryTestExecutor(t, agentExec)

	if err := exec.ExecuteWorkflow(context.Background(), agentWorkflow(nil, ""), run); err == nil {
		t.Fatal("expected failure without retry policy")
	}
	if got := agentExec.calls.Load(); got != 1 {
		t.Errorf("agent calls = %d, want 1", got)
	}
}

func TestStepRetry_RetryOnFiltersErrors(t *testing.T) {
	agentExec := &flakyAgentExecutor{failFirst: 1, failErr: errors.New("invalid prompt")}
	exec, run := newRetryTestExecutor(t, agentExec)
	policy := &ottoflowv1alpha1.RetryPolicy{
		Attempts: 3,
		Backoff:  noBackoff,
		RetryOn:  []ottoflowv1alpha1.RetryCondition{{ErrorMessage: "rate limit|503"}},
	}

	if err := exec.ExecuteWorkflow(context.Background(), agentWorkflow(policy, ""), run); err == nil {
		t.Fatal("expected failure: error does not match retryOn")
	}
	if got := agentExec.calls.Load(); got != 1 {
		t.Errorf("agent calls = %d, want 1 (non-matching error must not retry)", got)
	}
}

func TestStepTimeout_FailsHungStep(t *testing.T) {
	agentExec := &flakyAgentExecutor{block: true}
	exec, run := newRetryTestExecutor(t, agentExec)

	start := time.Now()
	err := exec.ExecuteWorkflow(context.Background(), agentWorkflow(nil, "100ms"), run)
	if err == nil {
		t.Fatal("expected timeout failure")
	}
	if !errors.Is(err, ErrStepTimeout) {
		t.Errorf("error = %v, want ErrStepTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("step ran %s, timeout not enforced", elapsed)
	}
	if st := run.Status.StepStatuses["analyze"]; st.Phase != ottoflowv1alpha1.StepPhaseFailed {
		t.Errorf("step phase = %s, want Failed", st.Phase)
	}
}

func TestStepTimeout_IsPerAttempt(t *testing.T) {
	agentExec := &flakyAgentExecutor{block: true}
	exec, run := newRetryTestExecutor(t, agentExec)
	policy := &ottoflowv1alpha1.RetryPolicy{
		Attempts: 2,
		Backoff:  noBackoff,
		RetryOn:  []ottoflowv1alpha1.RetryCondition{{ErrorType: "TimeoutError"}},
	}

	if err := exec.ExecuteWorkflow(context.Background(), agentWorkflow(policy, "50ms"), run); !errors.Is(err, ErrStepTimeout) {
		t.Fatalf("error = %v, want ErrStepTimeout", err)
	}
	if got := agentExec.calls.Load(); got != 2 {
		t.Errorf("agent calls = %d, want 2", got)
	}
}

func TestParseStepTimeout(t *testing.T) {
	if d, err := parseStepTimeout(""); err != nil || d != 0 {
		t.Errorf(`parseStepTimeout("") = %v, %v`, d, err)
	}
	if d, err := parseStepTimeout("2m"); err != nil || d != 2*time.Minute {
		t.Errorf(`parseStepTimeout("2m") = %v, %v`, d, err)
	}
	for _, bad := range []string{"soon", "-1s", "0s"} {
		if _, err := parseStepTimeout(bad); err == nil {
			t.Errorf("parseStepTimeout(%q) expected error", bad)
		}
	}
}

func TestRetryDelay(t *testing.T) {
	tests := []struct {
		name    string
		backoff *ottoflowv1alpha1.BackoffConfig
		attempt int
		want    time.Duration
	}{
		{"defaults first retry", nil, 1, time.Second},
		{"defaults exponential", nil, 3, 4 * time.Second},
		{"none", &ottoflowv1alpha1.BackoffConfig{Strategy: "none"}, 5, 0},
		{"linear", &ottoflowv1alpha1.BackoffConfig{Strategy: "linear", InitialInterval: "3s"}, 4, 3 * time.Second},
		{"exponential custom", &ottoflowv1alpha1.BackoffConfig{Strategy: "exponential", InitialInterval: "2s", Multiplier: 3}, 3, 18 * time.Second},
		{"capped at maxInterval", &ottoflowv1alpha1.BackoffConfig{InitialInterval: "2s", MaxInterval: "10s"}, 10, 10 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := retryDelay(tt.backoff, tt.attempt)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("retryDelay = %s, want %s", got, tt.want)
			}
		})
	}
	if _, err := retryDelay(&ottoflowv1alpha1.BackoffConfig{InitialInterval: "x"}, 1); err == nil {
		t.Error("expected error for invalid initialInterval")
	}
}

func TestShouldRetry(t *testing.T) {
	gr := schema.GroupResource{Resource: "pods"}
	tests := []struct {
		name   string
		policy *ottoflowv1alpha1.RetryPolicy
		err    error
		want   bool
	}{
		{"nil policy", nil, errors.New("x"), true},
		{"empty retryOn", &ottoflowv1alpha1.RetryPolicy{Attempts: 2}, errors.New("x"), true},
		{"message match", retryOn(ottoflowv1alpha1.RetryCondition{ErrorMessage: "rate.?limit"}), errors.New("hit rate limit"), true},
		{"message miss", retryOn(ottoflowv1alpha1.RetryCondition{ErrorMessage: "rate.?limit"}), errors.New("bad input"), false},
		{"timeout type", retryOn(ottoflowv1alpha1.RetryCondition{ErrorType: "TimeoutError"}), fmt.Errorf("%w: x", ErrStepTimeout), true},
		{"timeout type miss", retryOn(ottoflowv1alpha1.RetryCondition{ErrorType: "TimeoutError"}), errors.New("x"), false},
		{"transient api", retryOn(ottoflowv1alpha1.RetryCondition{ErrorType: "TransientError"}), apierrors.NewServiceUnavailable("down"), true},
		{"transient miss", retryOn(ottoflowv1alpha1.RetryCondition{ErrorType: "TransientError"}), apierrors.NewNotFound(gr, "p"), false},
		{"http status api", retryOn(ottoflowv1alpha1.RetryCondition{HTTPStatus: []int{503}}), apierrors.NewServiceUnavailable("down"), true},
		{"http status api miss", retryOn(ottoflowv1alpha1.RetryCondition{HTTPStatus: []int{503}}), apierrors.NewNotFound(gr, "p"), false},
		{"http status text", retryOn(ottoflowv1alpha1.RetryCondition{HTTPStatus: []int{429}}), errors.New("POST /v1/messages: status 429"), true},
		{"http status text no substring", retryOn(ottoflowv1alpha1.RetryCondition{HTTPStatus: []int{429}}), errors.New("id 14290"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldRetry(tt.policy, tt.err); got != tt.want {
				t.Errorf("shouldRetry = %v, want %v", got, tt.want)
			}
		})
	}
}

func retryOn(conds ...ottoflowv1alpha1.RetryCondition) *ottoflowv1alpha1.RetryPolicy {
	return &ottoflowv1alpha1.RetryPolicy{Attempts: 3, RetryOn: conds}
}

func TestGetReadySteps_Deterministic(t *testing.T) {
	steps := []ottoflowv1alpha1.Step{{Name: "delta"}, {Name: "alpha"}, {Name: "charlie"}, {Name: "bravo"}}
	dag, err := BuildDAG(steps)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"alpha", "bravo", "charlie", "delta"}
	for i := 0; i < 20; i++ {
		got := dag.GetReadySteps(map[string]bool{})
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("GetReadySteps = %v, want %v", got, want)
		}
	}
}

func TestExecuteWorkflow_ResumesInterruptedSteps(t *testing.T) {
	for _, phase := range []ottoflowv1alpha1.StepPhase{ottoflowv1alpha1.StepPhaseRunning, ottoflowv1alpha1.StepPhaseWaiting} {
		t.Run(string(phase), func(t *testing.T) {
			agentExec := &flakyAgentExecutor{}
			exec, run := newRetryTestExecutor(t, agentExec)
			run.Status.StepStatuses = map[string]ottoflowv1alpha1.StepStatus{"analyze": {Phase: phase}}

			if err := exec.ExecuteWorkflow(context.Background(), agentWorkflow(nil, ""), run); err != nil {
				t.Fatalf("ExecuteWorkflow: %v", err)
			}
			if got := agentExec.calls.Load(); got != 1 {
				t.Errorf("agent calls = %d, want 1 (step left %s must re-run)", got, phase)
			}
			if run.Status.Phase != ottoflowv1alpha1.WorkflowRunPhaseSucceeded {
				t.Errorf("run phase = %s, want Succeeded", run.Status.Phase)
			}
		})
	}
}
