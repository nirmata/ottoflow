/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/metrics"
)

var runsGR = schema.GroupResource{Group: "ottoflow.nirmata.io", Resource: "workflowruns"}

type createRetryEnv struct {
	tm    *TriggerManager
	c     client.Client // the fake, for listing runs
	wf    *ottoflowv1alpha1.Workflow
	spec  *ottoflowv1alpha1.EventTrigger
	obj   *unstructured.Unstructured
	calls int
}

// newCreateRetryEnv builds a TriggerManager whose WorkflowRun Create goes through fn, which gets
// the 1-based call number and the real Create (nil = plain Create).
func newCreateRetryEnv(fn func(ctx context.Context, call int, create func() error) error) *createRetryEnv {
	env := &createRetryEnv{
		wf: &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
			Spec:       ottoflowv1alpha1.WorkflowSpec{Steps: []ottoflowv1alpha1.Step{{Name: "s1", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `"ok"`}}}}},
		},
		spec: &ottoflowv1alpha1.EventTrigger{
			Resources:  []ottoflowv1alpha1.EventResource{{APIVersion: "v1", Kind: "Pod", Namespace: "default"}},
			Operations: []string{"CREATE"},
		},
		obj: makeUnstructured("p1", "default", map[string]interface{}{"apiVersion": "v1", "kind": "Pod"}),
	}
	env.c = makeFakeClient(env.wf)
	c := interceptor.NewClient(env.c.(client.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*ottoflowv1alpha1.WorkflowRun); !ok || fn == nil {
				return c.Create(ctx, obj, opts...)
			}
			env.calls++
			return fn(ctx, env.calls, func() error { return c.Create(ctx, obj, opts...) })
		},
	})
	env.tm = NewTriggerManager(c, unitTestScheme, nil)
	return env
}

func (e *createRetryEnv) create(ctx context.Context) error {
	return e.tm.CreateWorkflowRunFromEvent(ctx, "trig", e.wf, e.spec, e.obj, watch.Added)
}

// Mutant: isTransientCreateErr always false.
// Rows conn refused, http2 lost and dial timeout each go red when only their utilnet check is
// removed. The conn reset row has no check of its own: utilnet.IsProbableEOF matches its text.
func TestCreateWorkflowRunFromEvent_RetriesTransientCreateError(t *testing.T) {
	for name, transient := range map[string]error{
		"internal error": apierrors.NewInternalError(errors.New("webhook down")),
		"conn reset":     &url.Error{Op: "Post", Err: syscall.ECONNRESET},
		"eof":            &url.Error{Op: "Post", Err: io.EOF},
		"conn refused":   &url.Error{Op: "Post", Err: syscall.ECONNREFUSED},
		"http2 lost":     &url.Error{Op: "Post", Err: errors.New("http2: client connection lost")},
		"dial timeout":   &url.Error{Op: "Post", Err: &net.OpError{Op: "dial", Err: os.ErrDeadlineExceeded}},
	} {
		t.Run(name, func(t *testing.T) {
			env := newCreateRetryEnv(func(_ context.Context, call int, create func() error) error {
				if call == 1 {
					return transient
				}
				return create()
			})
			if err := env.create(context.Background()); err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if n := countRuns(t, env.c); n != 1 {
				t.Fatalf("runs = %d, want 1", n)
			}
		})
	}
}

// The Create lands but its response is lost; the retry must find the same
// run (AlreadyExists, success) and dedup must be recorded.
// Mutants: (i) time8 regenerated per attempt; (ii) no DeepCopy per attempt; (iii) AlreadyExists
// not treated as success; (iv) server timeout row only: IsServerTimeout removed from the predicate.
func TestCreateWorkflowRunFromEvent_CreateLandedResponseLost(t *testing.T) {
	for name, lost := range map[string]error{
		"unexpected eof": &url.Error{Op: "Post", Err: io.ErrUnexpectedEOF},
		"server timeout": apierrors.NewServerTimeout(runsGR, "create", 0),
	} {
		t.Run(name, func(t *testing.T) {
			env := newCreateRetryEnv(func(_ context.Context, call int, create func() error) error {
				if err := create(); err != nil || call > 1 {
					return err
				}
				return lost
			})
			ctx := context.Background()
			if err := env.create(ctx); err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if err := env.create(ctx); err != nil {
				t.Fatalf("second call err = %v", err)
			}
			if n := countRuns(t, env.c); n != 1 {
				t.Fatalf("runs = %d, want 1 (dedup must be recorded)", n)
			}
		})
	}
}

// The first Create lands and loses its response, and the reconciler writes the run's status before
// the retry finds the run as AlreadyExists. The status write after create keeps that status, adds
// the trigger info, and sets Pending only when the phase is empty.
// Mutants: (i) fetched status replaced by the initial one, or (ii) Pending set even when a phase
// is set -> running and failed rows; (iii) phase never set -> not started; (iv) no Trigger -> all.
func TestCreateWorkflowRunFromEvent_RetryKeepsReconciledStatus(t *testing.T) {
	at := metav1.NewTime(time.Unix(1_800_000_000, 0))
	exec := &ottoflowv1alpha1.WorkflowRunExecutionStatus{Phase: "Running", JobName: "wf-job", StartTime: &at}
	for name, reconciled := range map[string]ottoflowv1alpha1.WorkflowRunStatus{
		"not started": {}, // a real apiserver drops status on Create
		"running":     {Phase: ottoflowv1alpha1.WorkflowRunPhaseRunning, StartTime: &at, Execution: exec},
		"failed": {
			Phase: ottoflowv1alpha1.WorkflowRunPhaseFailed, StartTime: &at, CompletionTime: &at, Execution: exec,
			Message: "step s1 failed", FailureReason: ottoflowv1alpha1.WorkflowRunFailureReasonSecretAccessDenied,
			Outputs: map[string]apiextensionsv1.JSON{"x": {Raw: []byte(`"ok"`)}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			var env *createRetryEnv
			env = newCreateRetryEnv(func(ctx context.Context, call int, create func() error) error {
				if err := create(); err != nil || call > 1 {
					return err
				}
				var l ottoflowv1alpha1.WorkflowRunList
				if err := env.c.List(ctx, &l); err != nil || len(l.Items) != 1 {
					t.Fatalf("list runs: %v, %d items", err, len(l.Items))
				}
				l.Items[0].Status = *reconciled.DeepCopy()
				if err := env.c.Status().Update(ctx, &l.Items[0]); err != nil {
					t.Fatal(err)
				}
				return &url.Error{Op: "Post", Err: io.ErrUnexpectedEOF}
			})
			ctx := context.Background()
			if err := env.create(ctx); err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			var l ottoflowv1alpha1.WorkflowRunList
			if err := env.c.List(ctx, &l); err != nil || len(l.Items) != 1 {
				t.Fatalf("list runs: %v, %d items", err, len(l.Items))
			}
			got := l.Items[0].Status
			if got.Trigger == nil || got.Trigger.Type != "Event" {
				t.Fatalf("Trigger = %+v, want Type Event", got.Trigger)
			}
			got.Trigger = nil
			want := reconciled.DeepCopy()
			if want.Phase == "" {
				want.Phase = ottoflowv1alpha1.WorkflowRunPhasePending
			}
			if diff := cmp.Diff(*want, got); diff != "" {
				t.Fatalf("status (-want +got):\n%s", diff)
			}
		})
	}
}

func assertCreateCalls(t *testing.T, env *createRetryEnv, ctx context.Context) {
	t.Helper()
	if err := env.create(ctx); err == nil {
		t.Fatal("want an error")
	}
	if env.calls != 1 {
		t.Fatalf("Create calls = %d, want 1", env.calls)
	}
}

func alwaysFail(err error) func(context.Context, int, func() error) error {
	return func(context.Context, int, func() error) error { return err }
}

// Mutant: backoff Steps raised (e.g. 1000) -> 6 Create calls over about 8s.
func TestCreateWorkflowRunFromEvent_RetryStopsAfterFourAttempts(t *testing.T) {
	t.Parallel() // ~0.3s in the retry backoff
	env := newCreateRetryEnv(alwaysFail(&url.Error{Op: "Post", Err: syscall.ECONNREFUSED}))
	if err := env.create(context.Background()); !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("err = %v, want conn refused", err)
	}
	if env.calls != 4 {
		t.Fatalf("Create calls = %d, want 4", env.calls)
	}
}

// Mutant: the ctx.Err() == nil part of retryWindowOpen removed -> 4 calls
// (utilnet.IsTimeout matches context.DeadlineExceeded).
func TestCreateWorkflowRunFromEvent_NoRetryAfterCallerDeadline(t *testing.T) {
	env := newCreateRetryEnv(func(ctx context.Context, _ int, _ func() error) error {
		<-ctx.Done()
		return ctx.Err()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	assertCreateCalls(t, env, ctx)
}

// One Create call, each row with its own mutant.
//   - invalid: predicate always true -> 4 calls.
//   - retry-after: the Details RetryAfterSeconds exclusion removed.
//   - service unavailable: IsServiceUnavailable re-added to the predicate.
//   - apiserver 504: apierrors.IsTimeout re-added. Status errors return from the Status branch
//     and never reach the transport checks.
func TestCreateWorkflowRunFromEvent_NoRetryOnNonTransientError(t *testing.T) {
	for name, err := range map[string]error{
		"invalid":             apierrors.NewInvalid(workflowRunGroupKind, "x", nil),
		"retry-after":         apierrors.NewServerTimeout(runsGR, "create", 2),
		"service unavailable": apierrors.NewServiceUnavailable("apiserver is shutting down"),
		"apiserver 504":       apierrors.NewTimeoutError("request did not complete within the allotted timeout", 0),
	} {
		t.Run(name, func(t *testing.T) {
			assertCreateCalls(t, newCreateRetryEnv(alwaysFail(err)), context.Background())
		})
	}
}

// Mutant: the time.Since(start) < createRetryBudget part of retryWindowOpen removed -> 4 calls.
// A first failure that outlasts the budget is not repeated.
func TestCreateWorkflowRunFromEvent_NoRetryAfterBudget(t *testing.T) {
	old := createRetryBudget
	createRetryBudget = 20 * time.Millisecond
	t.Cleanup(func() { createRetryBudget = old })
	env := newCreateRetryEnv(func(context.Context, int, func() error) error {
		time.Sleep(50 * time.Millisecond)
		return apierrors.NewInternalError(errors.New("webhook timed out"))
	})
	assertCreateCalls(t, env, context.Background())
}

// Mutant: the budget and ctx recheck before each retried Create removed -> 2 calls.
// The budget runs out during the backoff sleep (first sleep is about 10ms), after the predicate
// passed; no Create may start after it, and the transient error is returned.
func TestCreateWorkflowRunFromEvent_NoCreateAfterBudgetExpiresInBackoff(t *testing.T) {
	old := createRetryBudget
	createRetryBudget = 8 * time.Millisecond
	t.Cleanup(func() { createRetryBudget = old })
	transient := apierrors.NewInternalError(errors.New("webhook down"))
	env := newCreateRetryEnv(alwaysFail(transient))
	if err := env.create(context.Background()); !errors.Is(err, transient) {
		t.Fatalf("err = %v, want the transient error", err)
	}
	if env.calls != 1 {
		t.Fatalf("Create calls = %d, want 1", env.calls)
	}
}

// runWatchOnce feeds one event through watchEvents and waits for it to finish.
func (e *createRetryEnv) runWatchOnce(t *testing.T) {
	t.Helper()
	fw := &fakeWatch{ch: make(chan watch.Event)}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		e.tm.watchEvents(context.Background(), "trig", e.wf, e.spec, fw, stop, nil)
		close(done)
	}()
	fw.ch <- watch.Event{Type: watch.Added, Object: e.obj} // unbuffered: returns once watchEvents has the event
	close(stop)                                            // watchEvents returns after handling it
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watchEvents did not return")
	}
}

func droppedCounter(t *testing.T) float64 {
	t.Helper()
	return counterValue(t, metrics.WorkflowTriggerEventsDroppedTotal, "wf", "default", "api_error")
}

// Mutant: the increment removed.
func TestWatchEvents_CountsDroppedEvent(t *testing.T) {
	env := newCreateRetryEnv(alwaysFail(apierrors.NewInvalid(workflowRunGroupKind, "x", nil)))
	before := droppedCounter(t)
	env.runWatchOnce(t)
	if got := droppedCounter(t); got != before+1 {
		t.Fatalf("dropped counter = %v, want %v", got, before+1)
	}
	if n := countRuns(t, env.c); n != 0 {
		t.Fatalf("runs = %d, want 0", n)
	}
}

// Mutant: CreateWorkflowRunFromEvent drops setStatusAfterCreate's error (returns nil).
func TestCreateWorkflowRunFromEvent_StatusFailureAfterCreateIsSentinel(t *testing.T) {
	t.Parallel() // ~0.8s in the status loop's sleeps; never touches the counter
	env := newCreateRetryEnv(nil)
	env.tm.client = &failingStatusWriteClient{Client: env.tm.client}
	err := env.create(context.Background())
	if !errors.Is(err, errStatusUpdateAfterCreate) {
		t.Fatalf("err = %v, want errStatusUpdateAfterCreate", err)
	}
	if n := countRuns(t, env.c); n != 1 {
		t.Fatalf("runs = %d, want 1", n)
	}
}

// A landed run whose status update failed is not counted as a drop.
// Mutant: the counter increment placed before the sentinel branch.
func TestWatchEvents_StatusFailureAfterCreateNotCounted(t *testing.T) {
	t.Parallel() // ~0.8s in the status loop's sleeps
	env := newCreateRetryEnv(nil)
	env.tm.client = &failingStatusWriteClient{Client: env.tm.client}
	before := droppedCounter(t)
	env.runWatchOnce(t)
	if got := droppedCounter(t); got != before {
		t.Fatalf("dropped counter = %v, want unchanged %v", got, before)
	}
	if n := countRuns(t, env.c); n != 1 {
		t.Fatalf("runs = %d, want 1", n)
	}
}
