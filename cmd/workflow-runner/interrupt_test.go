/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/agent"
	workflowexecutor "github.com/nirmata/ottoflow/internal/workflow/executor"
)

// ctxAwareClient mimics the real client: a done ctx fails the call before it reaches the server
// (client-go: "client rate limiter Wait returned an error: context canceled").
func ctxAwareClient(objs ...client.Object) (client.WithWatch, *int) {
	updates := 0
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&ottoflowv1alpha1.WorkflowRun{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, k client.ObjectKey, o client.Object,
				opts ...client.GetOption) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				return c.Get(ctx, k, o, opts...)
			},
			SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, o client.Object,
				opts ...client.SubResourceUpdateOption) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				updates++
				return c.SubResource(sub).Update(ctx, o, opts...)
			},
		}).Build()
	return c, &updates
}

func newRun() *ottoflowv1alpha1.WorkflowRun {
	return &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "default"},
		Spec: ottoflowv1alpha1.WorkflowRunSpec{
			WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf"},
			Execution: &ottoflowv1alpha1.WorkflowRunExecutionSpec{
				Checkpointing: &ottoflowv1alpha1.CheckpointingConfig{Enabled: true},
			},
		},
	}
}

// The interrupted status, once persisted and retried without a checkpoint, must run to completion.
func TestInterruptedStatusRetriesWithoutCheckpoint(t *testing.T) {
	for _, tc := range []struct {
		shape   string
		rewrite bool
	}{{"execFail", false}, {"execFail", true}, {"matchCondFail", true}} {
		rewrite := tc.rewrite
		run := newRun()
		wf := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{Steps: []ottoflowv1alpha1.Step{
				{Name: "step1", Expressions: []ottoflowv1alpha1.Expression{{Name: "a", Expression: "1 + 2"}}},
				{Name: "step2", DependsOn: []string{"step1"},
					Expressions: []ottoflowv1alpha1.Expression{{Name: "b", Expression: "3"}}},
			}},
		}
		c, updates := ctxAwareClient(run, wf)

		// Status as the executor leaves it after root-ctx cancel mid-step1 (the execution-failure path).
		signalAt := metav1.NewTime(time.Now())
		done := metav1.NewTime(signalAt.Add(10 * time.Millisecond))
		run.Status.Phase = ottoflowv1alpha1.WorkflowRunPhaseFailed
		run.Status.AuditSnapshotError = "context canceled"
		run.Status.Message = "Step step1 failed: context canceled" // recordWorkflowFailure
		step1 := ottoflowv1alpha1.StepStatus{
			Phase: ottoflowv1alpha1.StepPhaseFailed, Error: "context canceled", Message: "context canceled",
			StartTime: &signalAt, CompletionTime: &done,
		}
		if tc.shape == "matchCondFail" {
			step1 = ottoflowv1alpha1.StepStatus{
				Phase: ottoflowv1alpha1.StepPhaseFailed, Error: "matchCondition evaluation failed: context canceled",
			}
		}
		run.Status.StepStatuses = map[string]ottoflowv1alpha1.StepStatus{
			"step1": step1, "step2": {Phase: ottoflowv1alpha1.StepPhasePending},
		}
		if rewrite {
			markInterrupted(&run.Status)
		}
		writeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := persistWorkflowRunStatus(writeCtx, c, client.ObjectKeyFromObject(run), run.Status); err != nil {
			t.Fatal(err)
		}
		cancel()

		// Assert what the server holds, not the helper's return value.
		stored := &ottoflowv1alpha1.WorkflowRun{}
		_ = c.Get(context.Background(), client.ObjectKeyFromObject(run), stored)
		t.Logf("%s rewrite=%v stored: updates=%d phase=%s runMsg=%q step1=%s msg=%q auditErr=%q", tc.shape,
			rewrite, *updates, stored.Status.Phase, stored.Status.Message, stored.Status.StepStatuses["step1"].Phase,
			stored.Status.StepStatuses["step1"].Message, stored.Status.AuditSnapshotError)
		if rewrite && (stored.Status.Message != interruptedMessage ||
			stored.Status.StepStatuses["step1"].Message != interruptedMessage) {
			t.Errorf("marker not stored")
		}

		// The controller's retryTransientFailure: Phase=Pending, Attempts+1, StepStatuses untouched.
		stored.Status.Phase = ottoflowv1alpha1.WorkflowRunPhasePending
		stored.Status.Execution = &ottoflowv1alpha1.WorkflowRunExecutionStatus{Attempts: 1}

		exec, err := workflowexecutor.NewWorkflowExecutorWithAgentExecutor(
			c, nil, nil, nil, stored, agent.NewMockAgentExecutor(), false, 0, 5, nil)
		if err != nil {
			t.Fatal(err)
		}
		exec.SetCheckpointManager(workflowexecutor.NewCheckpointManager(c, stored)) // no checkpoint CM exists
		err = exec.ExecuteWorkflow(context.Background(), wf, stored)
		t.Logf("%s rewrite=%v retry: err=%v phase=%s step1=%s step2=%s", tc.shape, rewrite, err,
			stored.Status.Phase, stored.Status.StepStatuses["step1"].Phase, stored.Status.StepStatuses["step2"].Phase)
		if rewrite && stored.Status.Phase != ottoflowv1alpha1.WorkflowRunPhaseSucceeded {
			t.Errorf("retry after interrupted write did not complete: phase=%s", stored.Status.Phase)
		}
		if rewrite && stored.Status.AuditSnapshotError != "" {
			t.Errorf("AuditSnapshotError not cleared")
		}
		if !rewrite && stored.Status.Phase == ottoflowv1alpha1.WorkflowRunPhaseSucceeded {
			t.Errorf("control: expected the un-rewritten status to wedge, but it succeeded")
		}
	}
}

// Every non-Pending step goes back to Pending, whatever its phase or FailurePolicy: a retry
// without a checkpoint restore must not skip a step whose outputs it no longer has.
func TestMarkInterruptedResetsEveryNonPendingStep(t *testing.T) {
	st := ottoflowv1alpha1.WorkflowRunStatus{
		Phase: ottoflowv1alpha1.WorkflowRunPhaseFailed,
		StepStatuses: map[string]ottoflowv1alpha1.StepStatus{
			"done":    {Phase: ottoflowv1alpha1.StepPhaseSucceeded},
			"skipped": {Phase: ottoflowv1alpha1.StepPhaseSkipped},
			"soft":    {Phase: ottoflowv1alpha1.StepPhaseFailed, Error: "boom"},
			// matchCondition failure: Failed, Error only, no StartTime/CompletionTime.
			"gate":    {Phase: ottoflowv1alpha1.StepPhaseFailed, Error: "matchCondition evaluation failed: context canceled"},
			"running": {Phase: ottoflowv1alpha1.StepPhaseRunning},
			"waiting": {Phase: ottoflowv1alpha1.StepPhasePending, Message: "keep"},
		},
	}
	markInterrupted(&st)
	for name, s := range st.StepStatuses {
		want := interruptedMessage
		if name == "waiting" {
			want = "keep"
		}
		if s.Phase != ottoflowv1alpha1.StepPhasePending || s.Message != want || s.Error != "" {
			t.Errorf("step %s not reset: %+v", name, s)
		}
	}
	if st.Phase != ottoflowv1alpha1.WorkflowRunPhaseRunning {
		t.Errorf("run phase=%s, want Running", st.Phase)
	}
}

// apiserverStub serves GET and PUT /status for one WorkflowRun and counts the updates it stores.
type apiserverStub struct {
	mu      sync.Mutex
	run     ottoflowv1alpha1.WorkflowRun
	updates int
}

func (a *apiserverStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r.Method == http.MethodPut {
		var in ottoflowv1alpha1.WorkflowRun
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		a.run.Status = in.Status
		a.updates++
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(&a.run)
}

func realClientFor(t *testing.T, srv *httptest.Server) client.Client {
	t.Helper()
	gvk := ottoflowv1alpha1.GroupVersion.WithKind("WorkflowRun")
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{ottoflowv1alpha1.GroupVersion})
	mapper.Add(gvk, meta.RESTScopeNamespace)
	c, err := client.New(&rest.Config{Host: srv.URL}, client.Options{Scheme: scheme, Mapper: mapper})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// With the real client, a write on a cancelled ctx never reaches the server and must say so
// (client-go's RetryOnConflict alone returns nil). persistBounded must still land after the signal.
func TestPersistAfterSignalReachesServer(t *testing.T) {
	stub := &apiserverStub{run: ottoflowv1alpha1.WorkflowRun{
		TypeMeta:   metav1.TypeMeta{APIVersion: ottoflowv1alpha1.GroupVersion.String(), Kind: "WorkflowRun"},
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "default", ResourceVersion: "1"},
	}}
	srv := httptest.NewServer(stub)
	defer srv.Close()
	c := realClientFor(t, srv)
	key := client.ObjectKey{Name: "r", Namespace: "default"}
	status := ottoflowv1alpha1.WorkflowRunStatus{
		Phase: ottoflowv1alpha1.WorkflowRunPhaseRunning, Message: interruptedMessage,
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := persistWorkflowRunStatus(ctx, c, key, status); err == nil {
		t.Fatalf("persist on a cancelled ctx returned nil, server updates=%d", stub.updates)
	}
	if stub.updates != 0 {
		t.Fatalf("cancelled ctx reached the server: updates=%d", stub.updates)
	}

	if err := persistBounded(ctx, c, key, status); err != nil {
		t.Fatalf("persistBounded after cancel: %v", err)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.updates != 1 || stub.run.Status.Message != interruptedMessage {
		t.Fatalf("server did not store the update: updates=%d status=%+v", stub.updates, stub.run.Status)
	}
}

// A SIGTERM during startup must not write Failed (it would block a checkpoint retry).
func TestUpdateExecutionFailureSkippedWhenInterrupted(t *testing.T) {
	run := newRun()
	c, updates := ctxAwareClient(run)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	updateExecutionFailure(ctx, c, client.ObjectKeyFromObject(run), run, "j", "p", "boom")
	if *updates != 0 || run.Status.Phase == ottoflowv1alpha1.WorkflowRunPhaseFailed {
		t.Fatalf("startup failure recorded under a done ctx: updates=%d phase=%s", *updates, run.Status.Phase)
	}
	updateExecutionFailure(context.Background(), c, client.ObjectKeyFromObject(run), run, "j", "p", "boom")
	if *updates != 1 {
		t.Fatalf("live ctx must still record the failure: updates=%d", *updates)
	}
}

// End to end through runWorkflowToCompletion: a SIGTERM mid-forEach must exit non-zero and leave the
// stored run Running with the in-flight step Pending and marked, so a controller retry re-runs it.
func TestRunWorkflowToCompletionInterrupted(t *testing.T) {
	run := newRun()
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec: ottoflowv1alpha1.WorkflowSpec{Steps: []ottoflowv1alpha1.Step{
			{Name: "loop", ForEach: &ottoflowv1alpha1.StepForEach{
				Items: `["a", "b", "c"]`, MaxConcurrency: 1,
				Step: &ottoflowv1alpha1.StepForEachStep{
					Outputs: []ottoflowv1alpha1.Output{{Name: "r", Expression: `variables.item`}},
				},
			}},
			{Name: "after", DependsOn: []string{"loop"},
				Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: "1"}}},
		}},
	}
	c, _ := ctxAwareClient(run, wf)
	exec, err := workflowexecutor.NewWorkflowExecutorWithAgentExecutor(
		c, nil, nil, nil, run, agent.NewMockAgentExecutor(), false, 0, 5, nil)
	if err != nil {
		t.Fatal(err)
	}
	cm := workflowexecutor.NewCheckpointManager(c, run)
	exec.SetCheckpointManager(cm)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exec.SetProgressCallback(func(r *ottoflowv1alpha1.WorkflowRun, _ *ottoflowv1alpha1.Workflow) {
		if r.Status.StepStatuses["loop"].Message == "1/3 items" {
			cancel() // SIGTERM arrives after the first item
		}
	})
	cfg := runnerConfig{
		runName: "r", runNamespace: "default", runKey: client.ObjectKeyFromObject(run), jobName: "j", podName: "p",
	}

	if code := runWorkflowToCompletion(ctx, exec, cm, wf, run, c, cfg); code != exitInterrupted {
		t.Fatalf("exit code=%d, want %d", code, exitInterrupted)
	}
	stored := &ottoflowv1alpha1.WorkflowRun{}
	if err := c.Get(context.Background(), cfg.runKey, stored); err != nil {
		t.Fatal(err)
	}
	loop := stored.Status.StepStatuses["loop"]
	t.Logf("stored: phase=%s msg=%q loop=%s loopMsg=%q",
		stored.Status.Phase, stored.Status.Message, loop.Phase, loop.Message)
	if stored.Status.Phase != ottoflowv1alpha1.WorkflowRunPhaseRunning || stored.Status.Message != interruptedMessage ||
		loop.Phase != ottoflowv1alpha1.StepPhasePending || loop.Message != interruptedMessage {
		t.Fatalf("interrupted status not stored: %+v", stored.Status)
	}
}

// The signal arriving after the run completed (err == nil, ctx done) is a success: exit 0, the
// success write, and no interrupted rewrite.
func TestRunWorkflowToCompletionSignalAfterCompletionIsSuccess(t *testing.T) {
	run := newRun()
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec: ottoflowv1alpha1.WorkflowSpec{Steps: []ottoflowv1alpha1.Step{
			{Name: "only", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: "1"}}},
		}},
	}
	c, _ := ctxAwareClient(run, wf)
	exec, err := workflowexecutor.NewWorkflowExecutorWithAgentExecutor(
		c, nil, nil, nil, run, agent.NewMockAgentExecutor(), false, 0, 5, nil)
	if err != nil {
		t.Fatal(err)
	}
	cm := workflowexecutor.NewCheckpointManager(c, run)
	exec.SetCheckpointManager(cm)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exec.SetProgressCallback(func(r *ottoflowv1alpha1.WorkflowRun, _ *ottoflowv1alpha1.Workflow) {
		if r.Status.StepStatuses["only"].Phase == ottoflowv1alpha1.StepPhaseSucceeded {
			cancel() // SIGTERM lands after the last step succeeded
		}
	})
	cfg := runnerConfig{
		runName: "r", runNamespace: "default", runKey: client.ObjectKeyFromObject(run), jobName: "j", podName: "p",
	}

	code := runWorkflowToCompletion(ctx, exec, cm, wf, run, c, cfg)
	if ctx.Err() == nil {
		t.Fatal("test did not cancel the ctx after the run completed")
	}
	stored := &ottoflowv1alpha1.WorkflowRun{}
	if err := c.Get(context.Background(), cfg.runKey, stored); err != nil {
		t.Fatal(err)
	}
	t.Logf("code=%d stored: phase=%s msg=%q only=%s exec=%+v", code, stored.Status.Phase, stored.Status.Message,
		stored.Status.StepStatuses["only"].Phase, stored.Status.Execution)
	if code != 0 {
		t.Fatalf("exit code=%d, want 0", code)
	}
	if stored.Status.Phase != ottoflowv1alpha1.WorkflowRunPhaseSucceeded ||
		stored.Status.Message == interruptedMessage ||
		stored.Status.StepStatuses["only"].Phase != ottoflowv1alpha1.StepPhaseSucceeded ||
		stored.Status.Execution == nil ||
		stored.Status.Execution.Phase != string(ottoflowv1alpha1.WorkflowRunPhaseSucceeded) {
		t.Fatalf("success path not taken: %+v", stored.Status)
	}
}

// A run interrupted and then resumed to success must not keep the interrupt marker at run level.
func TestResumedRunSuccessClearsInterruptedMessage(t *testing.T) {
	run := newRun()
	run.Status.Phase = ottoflowv1alpha1.WorkflowRunPhaseRunning
	run.Status.Message = interruptedMessage
	run.Status.Execution = &ottoflowv1alpha1.WorkflowRunExecutionStatus{Message: interruptedMessage}
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec: ottoflowv1alpha1.WorkflowSpec{Steps: []ottoflowv1alpha1.Step{
			{Name: "only", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: "1"}}},
		}},
	}
	c, _ := ctxAwareClient(run, wf)
	exec, err := workflowexecutor.NewWorkflowExecutorWithAgentExecutor(
		c, nil, nil, nil, run, agent.NewMockAgentExecutor(), false, 0, 5, nil)
	if err != nil {
		t.Fatal(err)
	}
	cm := workflowexecutor.NewCheckpointManager(c, run)
	exec.SetCheckpointManager(cm)
	cfg := runnerConfig{
		runName: "r", runNamespace: "default", runKey: client.ObjectKeyFromObject(run), jobName: "j", podName: "p",
	}

	if code := runWorkflowToCompletion(context.Background(), exec, cm, wf, run, c, cfg); code != 0 {
		t.Fatalf("exit code=%d, want 0", code)
	}
	stored := &ottoflowv1alpha1.WorkflowRun{}
	if err := c.Get(context.Background(), cfg.runKey, stored); err != nil {
		t.Fatal(err)
	}
	if stored.Status.Phase != ottoflowv1alpha1.WorkflowRunPhaseSucceeded ||
		stored.Status.Message == interruptedMessage ||
		stored.Status.Execution == nil || stored.Status.Execution.Message == interruptedMessage {
		t.Fatalf("interrupt marker survived success: %+v exec=%+v", stored.Status, stored.Status.Execution)
	}
}

// The worst case from signal to exit must stay inside the 30s default grace period with margin.
func TestShutdownBudgetFitsGracePeriod(t *testing.T) {
	const grace, margin = 30 * time.Second, 10 * time.Second
	if worst := defaultHardStop + hardStopFlushTimeout; worst > grace-margin {
		t.Fatalf("worst case %s leaves under %s of the %s grace period", worst, margin, grace)
	}
	// Normal interrupted path: shared flush+write budget plus the OTel flush deferred in run().
	if normal := persistTimeout + otelFlushTimeout; normal > defaultHardStop {
		t.Fatalf("normal interrupted path %s outlasts the hard stop %s", normal, defaultHardStop)
	}
}

// A checkpoint save that hangs must not use up the budget of the interrupted-status write: the
// status reaches the server even though Flush times out.
func TestInterruptedStatusSurvivesStalledCheckpointSave(t *testing.T) {
	run := newRun()
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec:       ottoflowv1alpha1.WorkflowSpec{Steps: []ottoflowv1alpha1.Step{{Name: "loop"}}},
	}
	c, _ := ctxAwareClient(run, wf)
	release := make(chan struct{})
	defer close(release)
	stalled := interceptor.NewClient(c, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, k client.ObjectKey, o client.Object,
			opts ...client.GetOption) error {
			if _, ok := o.(*corev1.ConfigMap); ok {
				select {
				case <-release:
				case <-ctx.Done():
				}
				return ctx.Err()
			}
			return c.Get(ctx, k, o, opts...)
		},
	})
	cm := workflowexecutor.NewCheckpointManager(stalled, run)
	cm.SaveAsync(context.Background(), workflowexecutor.CheckpointSnapshot{Version: 1})
	run.Status.StepStatuses = map[string]ottoflowv1alpha1.StepStatus{
		"loop": {Phase: ottoflowv1alpha1.StepPhaseRunning},
	}
	cfg := runnerConfig{runName: "r", runNamespace: "default", runKey: client.ObjectKeyFromObject(run)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	persistInterrupted(ctx, cm, c, cfg, wf, run, context.Canceled)

	stored := &ottoflowv1alpha1.WorkflowRun{}
	if err := c.Get(context.Background(), cfg.runKey, stored); err != nil {
		t.Fatal(err)
	}
	if got := stored.Status.StepStatuses["loop"]; got.Phase != ottoflowv1alpha1.StepPhasePending ||
		got.Message != interruptedMessage {
		t.Fatalf("interrupted status not stored behind a stalled checkpoint save: %+v", stored.Status)
	}
}

// runCapturingExit runs f on its own goroutine and returns the code it exits with, through
// klog.FlushAndExit or by returning.
func runCapturingExit(t *testing.T, f func() int) int {
	t.Helper()
	codes := make(chan int, 1)
	prev := klog.OsExit
	klog.OsExit = func(code int) {
		codes <- code
		runtime.Goexit() // OsExit must not return to its caller
	}
	defer func() { klog.OsExit = prev }()
	go func() { codes <- f() }()
	select {
	case code := <-codes:
		return code
	case <-time.After(30 * time.Second):
		t.Fatal("run did not exit")
		return 0
	}
}

// A step that fails for a real reason and only then sees the signal must be stored Failed with exit 1,
// not rewritten as interrupted: the controller does not retry a Failed run.
func TestRunWorkflowToCompletionFailureBeforeSignalStaysFailed(t *testing.T) {
	missingKey := []ottoflowv1alpha1.Expression{{Name: "x", Expression: "variables.doesNotExist"}}
	for name, step := range map[string]ottoflowv1alpha1.Step{
		"step": {Name: "boom", Expressions: missingKey},
		"matchCondition": {Name: "boom", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: "1"}},
			MatchConditions: []ottoflowv1alpha1.MatchCondition{{Name: "m", Expression: "variables.doesNotExist"}}},
	} {
		t.Run(name, func(t *testing.T) {
			run := newRun()
			wf := &ottoflowv1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
				Spec:       ottoflowv1alpha1.WorkflowSpec{Steps: []ottoflowv1alpha1.Step{step}},
			}
			c, _ := ctxAwareClient(run, wf)
			exec, err := workflowexecutor.NewWorkflowExecutorWithAgentExecutor(
				c, nil, nil, nil, run, agent.NewMockAgentExecutor(), false, 0, 5, nil)
			if err != nil {
				t.Fatal(err)
			}
			cm := workflowexecutor.NewCheckpointManager(c, run)
			exec.SetCheckpointManager(cm)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			exec.SetProgressCallback(func(r *ottoflowv1alpha1.WorkflowRun, _ *ottoflowv1alpha1.Workflow) {
				if r.Status.StepStatuses["boom"].Phase == ottoflowv1alpha1.StepPhaseFailed {
					cancel() // SIGTERM lands after the step has already failed
				}
			})
			cfg := runnerConfig{
				runName: "r", runNamespace: "default", runKey: client.ObjectKeyFromObject(run), jobName: "j", podName: "p",
			}

			code := runCapturingExit(t, func() int { return runWorkflowToCompletion(ctx, exec, cm, wf, run, c, cfg) })
			stored := &ottoflowv1alpha1.WorkflowRun{}
			if err := c.Get(context.Background(), cfg.runKey, stored); err != nil {
				t.Fatal(err)
			}
			boom := stored.Status.StepStatuses["boom"]
			t.Logf("code=%d ctxErr=%v stored: phase=%s msg=%q boom=%s err=%q",
				code, ctx.Err(), stored.Status.Phase, stored.Status.Message, boom.Phase, boom.Error)
			if ctx.Err() == nil {
				t.Fatal("test did not cancel the ctx")
			}
			if code != 1 || stored.Status.Phase != ottoflowv1alpha1.WorkflowRunPhaseFailed ||
				boom.Phase != ottoflowv1alpha1.StepPhaseFailed || !strings.Contains(boom.Error, "doesNotExist") {
				t.Fatalf("real failure not kept: code=%d phase=%s boom=%+v", code, stored.Status.Phase, boom)
			}
		})
	}
}
