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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/mark3labs/mcp-go/mcp"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

// capEnv is a client whose WorkflowRun reads can be frozen, as a controller cache lags a Create.
type capEnv struct {
	c     client.Client // what the creators under test use
	live  client.Client // where every write lands
	cache client.Client // answers WorkflowRun List while frozen, and Get too unless liveGet
}

type capOpts struct {
	frozen bool
	// liveGet leaves WorkflowRun Get live while List stays frozen.
	liveGet bool
	// suffix gives each created run a unique name, so a same-second cron name cannot hide a create.
	suffix bool
	// createErr, when it returns an error for the 1-based Create call, fails that Create without storing.
	createErr func(call int, obj client.Object) error
	// statusErr fails every status write.
	statusErr error
}

func newCapEnv(o capOpts, objs ...client.Object) *capEnv {
	e := &capEnv{live: makeFakeClient(objs...), cache: makeFakeClient(objs...)}
	var calls atomic.Int32
	e.c = interceptor.NewClient(e.live.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*ottoflowv1alpha1.WorkflowRun); ok && o.frozen && !o.liveGet {
				return e.cache.Get(ctx, key, obj, opts...)
			}
			return c.Get(ctx, key, obj, opts...)
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*ottoflowv1alpha1.WorkflowRunList); ok && o.frozen {
				return e.cache.List(ctx, list, opts...)
			}
			return c.List(ctx, list, opts...)
		},
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*ottoflowv1alpha1.WorkflowRun); ok {
				n := int(calls.Add(1))
				if o.suffix {
					obj.SetName(fmt.Sprintf("%s-%d", obj.GetName(), n))
				}
				if o.createErr != nil {
					if err := o.createErr(n, obj); err != nil {
						return err
					}
				}
			}
			return c.Create(ctx, obj, opts...)
		},
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if o.statusErr != nil {
				return o.statusErr
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	})
	return e
}

func capWorkflow(limit int32) *ottoflowv1alpha1.Workflow {
	wf := newWorkflow(defaultNamespace, "wf")
	wf.Spec.Run = &ottoflowv1alpha1.RunPolicy{MaxConcurrentRuns: &limit}
	return wf
}

func newCapScheduler(c client.Client, wf *ottoflowv1alpha1.Workflow) *Scheduler {
	s := NewScheduler(c, logr.Discard())
	s.ctx = context.Background()
	s.workflows["key"] = wf
	s.cronSpecs["key"] = &ottoflowv1alpha1.CronTrigger{Schedule: "*/5 * * * *"}
	return s
}

func capEventSpec() *ottoflowv1alpha1.EventTrigger {
	return &ottoflowv1alpha1.EventTrigger{
		Resources:  []ottoflowv1alpha1.EventResource{{APIVersion: "v1", Kind: "Pod", Namespace: defaultNamespace}},
		Operations: []string{"CREATE"},
	}
}

// capEvent fires one event for a distinct object, so dedup never drops it.
func capEvent(tm *TriggerManager, wf *ottoflowv1alpha1.Workflow, i int) error {
	obj := makeUnstructured(fmt.Sprintf("p%d", i), defaultNamespace, map[string]interface{}{"apiVersion": "v1", "kind": "Pod"})
	return tm.CreateWorkflowRunFromEvent(context.Background(), "trig", wf, capEventSpec(), obj, watch.Added)
}

func capWebhook(tm *TriggerManager, wf *ottoflowv1alpha1.Workflow) (WebhookFilterResult, error) {
	_, res, err := tm.CreateWorkflowRunFromWebhook(context.Background(), wf, &ottoflowv1alpha1.WebhookTrigger{}, nil, WebhookRequestMeta{})
	return res, err
}

func capMCPServer(t *testing.T, c client.Client) *MCPToolServer {
	t.Helper()
	s, err := NewMCPToolServer(c, noAuth{}, ":0")
	if err != nil {
		t.Fatalf("NewMCPToolServer: %v", err)
	}
	s.callTimeout = 10 * time.Millisecond
	s.pollInterval = time.Millisecond
	return s
}

func capMCPCall(t *testing.T, s *MCPToolServer) *mcp.CallToolResult {
	t.Helper()
	result, err := s.callWorkflow(context.Background(), callToolRequest("default__wf", nil))
	if err != nil {
		t.Fatalf("callWorkflow: %v", err)
	}
	return result
}

// Three cron fires against a cache that lags the creates stay at the cap of two.
func TestRunCapCronSite(t *testing.T) {
	t.Parallel() // each admitted fire spends ~0.8s in the status retries
	wf := capWorkflow(2)
	env := newCapEnv(capOpts{frozen: true, suffix: true}, wf)
	s := newCapScheduler(env.c, wf)
	for range 3 {
		s.handleCronFire("key")
	}
	if n := countRuns(t, env.live); n != 2 {
		t.Fatalf("runs = %d, want 2", n)
	}
}

// Three events against a lagging cache stay at the cap of two.
func TestRunCapEventSite(t *testing.T) {
	t.Parallel() // each created run spends ~0.8s in the status retries
	wf := capWorkflow(2)
	env := newCapEnv(capOpts{frozen: true}, wf)
	tm := NewTriggerManager(env.c, unitTestScheme, nil)
	for i := range 3 {
		_ = capEvent(tm, wf, i)
	}
	if n := countRuns(t, env.live); n != 2 {
		t.Fatalf("runs = %d, want 2", n)
	}
}

// Cron and event triggers built on one scheduler share a cap.
func TestRunCapCronAndEventShareLedger(t *testing.T) {
	t.Parallel()
	constructors := map[string]func(*testing.T, client.Client, *Scheduler) *TriggerManager{
		"NewTriggerManager": func(_ *testing.T, c client.Client, s *Scheduler) *TriggerManager {
			return NewTriggerManager(c, unitTestScheme, s)
		},
		"NewTriggerManagerWithConfig": func(t *testing.T, c client.Client, s *Scheduler) *TriggerManager {
			tm, err := NewTriggerManagerWithConfig(c, unitTestScheme, &rest.Config{Host: "http://127.0.0.1:0"}, s)
			if err != nil {
				t.Fatal(err)
			}
			return tm
		},
	}
	for name, newTM := range constructors {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			wf := capWorkflow(1)
			env := newCapEnv(capOpts{createErr: failCreateOnce(capTimeout)}, wf)
			s := newCapScheduler(env.c, wf)
			tm := newTM(t, env.c, s)
			s.handleCronFire("key")
			if err := capEvent(tm, wf, 0); err != nil {
				t.Fatalf("event err = %v, want nil (dropped at the cap)", err)
			}
			if n := countRuns(t, env.live); n != 0 {
				t.Fatalf("runs = %d, want 0", n)
			}
		})
	}
}

// Concurrent events against a lagging cache stay at the cap.
func TestRunCapConcurrentEvents(t *testing.T) {
	t.Parallel() // each created run spends ~0.8s in the status retries
	wf := capWorkflow(2)
	env := newCapEnv(capOpts{frozen: true}, wf)
	tm := NewTriggerManager(env.c, unitTestScheme, nil)
	var wg sync.WaitGroup
	for i := range 10 {
		wg.Go(func() { _ = capEvent(tm, wf, i) })
	}
	wg.Wait()
	if n := countRuns(t, env.live); n != 2 {
		t.Fatalf("runs = %d, want 2", n)
	}
}

// Three webhook calls against a lagging cache stay at the cap of two.
func TestRunCapWebhookSite(t *testing.T) {
	t.Parallel()
	wf := capWorkflow(2)
	env := newCapEnv(capOpts{frozen: true}, wf)
	tm := NewTriggerManager(env.c, unitTestScheme, nil)
	for i, want := range []WebhookFilterResult{WebhookRunCreated, WebhookRunCreated, WebhookConcurrencyLimited} {
		if got, err := capWebhook(tm, wf); err != nil || got != want {
			t.Errorf("call %d: result = %v, err = %v, want result %v", i+1, got, err, want)
		}
	}
	if n := countRuns(t, env.live); n != 2 {
		t.Fatalf("runs = %d, want 2", n)
	}
}

// Three MCP tool calls against a lagging cache stay at the cap of two.
func TestRunCapMCPSite(t *testing.T) {
	t.Parallel()
	wf := capWorkflow(2)
	env := newCapEnv(capOpts{frozen: true}, wf)
	s := capMCPServer(t, env.c)
	for range 2 {
		capMCPCall(t, s)
	}
	assertToolError(t, capMCPCall(t, s), "concurrency limit")
	if n := countRuns(t, env.live); n != 2 {
		t.Fatalf("runs = %d, want 2", n)
	}
}

var (
	capInvalid = apierrors.NewInvalid(workflowRunGroupKind, "x", nil)
	capTimeout = apierrors.NewTimeoutError("create timed out", 1)
)

func failCreateOnce(err error) func(int, client.Object) error {
	return func(call int, _ client.Object) error {
		if call == 1 {
			return err
		}
		return nil
	}
}

func failCreateAlways(err error) func(int, client.Object) error {
	return func(int, client.Object) error { return err }
}

// markRunSucceeded moves a stored run out of the active phases.
func markRunSucceeded(t *testing.T, c client.Client, name string) {
	t.Helper()
	var run ottoflowv1alpha1.WorkflowRun
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: defaultNamespace, Name: name}, &run); err != nil {
		t.Fatal(err)
	}
	run.Status.Phase = ottoflowv1alpha1.WorkflowRunPhaseSucceeded
	if err := c.Status().Update(context.Background(), &run); err != nil {
		t.Fatal(err)
	}
}

// A cron Create the API server rejects as invalid frees its slot at once.
func TestRunCapCronReleasesOnRejection(t *testing.T) {
	t.Parallel()
	wf := capWorkflow(1)
	env := newCapEnv(capOpts{createErr: failCreateOnce(capInvalid)}, wf)
	s := newCapScheduler(env.c, wf)
	s.handleCronFire("key")
	s.handleCronFire("key")
	if n := countRuns(t, env.live); n != 1 {
		t.Fatalf("runs = %d, want 1", n)
	}
}

// An event Create rejected as invalid frees its slot at once.
func TestRunCapEventReleasesOnRejection(t *testing.T) {
	t.Parallel()
	wf := capWorkflow(1)
	env := newCapEnv(capOpts{createErr: failCreateOnce(capInvalid)}, wf)
	tm := NewTriggerManager(env.c, unitTestScheme, nil)
	for i := range 2 {
		_ = capEvent(tm, wf, i)
	}
	if n := countRuns(t, env.live); n != 1 {
		t.Fatalf("runs = %d, want 1", n)
	}
}

// A webhook Create rejected as invalid frees its slot at once.
func TestRunCapWebhookReleasesOnRejection(t *testing.T) {
	t.Parallel()
	wf := capWorkflow(1)
	env := newCapEnv(capOpts{createErr: failCreateOnce(capInvalid)}, wf)
	tm := NewTriggerManager(env.c, unitTestScheme, nil)
	for range 2 {
		_, _ = capWebhook(tm, wf)
	}
	if n := countRuns(t, env.live); n != 1 {
		t.Fatalf("runs = %d, want 1", n)
	}
}

// An MCP Create rejected as invalid frees its slot at once.
func TestRunCapMCPReleasesOnRejection(t *testing.T) {
	t.Parallel()
	wf := capWorkflow(1)
	env := newCapEnv(capOpts{createErr: failCreateOnce(capInvalid)}, wf)
	s := capMCPServer(t, env.c)
	_, _ = s.callWorkflow(context.Background(), callToolRequest("default__wf", nil))
	capMCPCall(t, s)
	if n := countRuns(t, env.live); n != 1 {
		t.Fatalf("runs = %d, want 1", n)
	}
}

// A cron run that was created but whose status write failed keeps its slot.
func TestRunCapCronKeepsSlotOnStatusFailure(t *testing.T) {
	t.Parallel() // the first fire spends ~0.8s in the status retries
	wf := capWorkflow(1)
	env := newCapEnv(capOpts{frozen: true, liveGet: true, suffix: true, statusErr: errors.New("status write failed")}, wf)
	s := newCapScheduler(env.c, wf)
	s.handleCronFire("key")
	s.handleCronFire("key")
	if n := countRuns(t, env.live); n != 1 {
		t.Fatalf("runs = %d, want 1", n)
	}
}

// An event run that was created but whose status write failed keeps its slot.
func TestRunCapEventKeepsSlotOnStatusFailure(t *testing.T) {
	t.Parallel() // the first event spends ~0.8s in the status retries
	wf := capWorkflow(1)
	env := newCapEnv(capOpts{frozen: true, liveGet: true, statusErr: errors.New("status write failed")}, wf)
	tm := NewTriggerManager(env.c, unitTestScheme, nil)
	for i := range 2 {
		_ = capEvent(tm, wf, i)
	}
	if n := countRuns(t, env.live); n != 1 {
		t.Fatalf("runs = %d, want 1", n)
	}
}

// A webhook Create that fails ambiguously keeps its slot, since the run may have been stored.
func TestRunCapWebhookKeepsSlotOnAmbiguousError(t *testing.T) {
	t.Parallel()
	wf := capWorkflow(1)
	env := newCapEnv(capOpts{createErr: failCreateAlways(capTimeout)}, wf)
	tm := NewTriggerManager(env.c, unitTestScheme, nil)
	if _, err := capWebhook(tm, wf); !errors.Is(err, ErrWorkflowRunCreateFailed) {
		t.Fatalf("first call err = %v, want ErrWorkflowRunCreateFailed", err)
	}
	if got, err := capWebhook(tm, wf); err != nil || got != WebhookConcurrencyLimited {
		t.Fatalf("second call: result = %v, err = %v, want WebhookConcurrencyLimited", got, err)
	}
}

// An MCP Create that fails ambiguously keeps its slot.
func TestRunCapMCPKeepsSlotOnAmbiguousError(t *testing.T) {
	t.Parallel()
	wf := capWorkflow(1)
	env := newCapEnv(capOpts{createErr: failCreateAlways(capTimeout)}, wf)
	s := capMCPServer(t, env.c)
	if _, err := s.callWorkflow(context.Background(), callToolRequest("default__wf", nil)); err == nil {
		t.Fatal("first call err = nil, want the create error")
	}
	assertToolError(t, capMCPCall(t, s), "concurrency limit")
}

// A cron fire whose run the cache confirms frees its slot, so the next fire is admitted.
func TestRunCapCronReleasesOnConfirmedCreate(t *testing.T) {
	t.Parallel()
	wf := capWorkflow(1)
	env := newCapEnv(capOpts{suffix: true}, wf)
	s := newCapScheduler(env.c, wf)
	s.handleCronFire("key")
	var runs ottoflowv1alpha1.WorkflowRunList
	if err := env.live.List(context.Background(), &runs); err != nil || len(runs.Items) != 1 {
		t.Fatalf("after first fire: runs = %d, err = %v, want 1", len(runs.Items), err)
	}
	markRunSucceeded(t, env.live, runs.Items[0].Name)
	s.handleCronFire("key")
	if n := countRuns(t, env.live); n != 2 {
		t.Fatalf("runs = %d, want 2", n)
	}
}

// An event whose run the cache confirms frees its slot, so the next event is admitted.
func TestRunCapEventReleasesOnConfirmedCreate(t *testing.T) {
	t.Parallel()
	wf := capWorkflow(1)
	env := newCapEnv(capOpts{}, wf)
	tm := NewTriggerManager(env.c, unitTestScheme, nil)
	if err := capEvent(tm, wf, 0); err != nil {
		t.Fatalf("first event err = %v", err)
	}
	var runs ottoflowv1alpha1.WorkflowRunList
	if err := env.live.List(context.Background(), &runs); err != nil || len(runs.Items) != 1 {
		t.Fatalf("after first event: runs = %d, err = %v, want 1", len(runs.Items), err)
	}
	markRunSucceeded(t, env.live, runs.Items[0].Name)
	if err := capEvent(tm, wf, 1); err != nil {
		t.Fatalf("second event err = %v", err)
	}
	if n := countRuns(t, env.live); n != 2 {
		t.Fatalf("runs = %d, want 2", n)
	}
}

// A name-already-taken answer keeps the slot until the cache shows the reserved name.
func TestRunCapWebhookAlreadyExistsHoldsUntilNameAppears(t *testing.T) {
	t.Parallel()
	wf := capWorkflow(1)
	var reserved string
	env := newCapEnv(capOpts{frozen: true, createErr: func(call int, obj client.Object) error {
		if call > 1 {
			return nil
		}
		reserved = obj.GetName()
		return apierrors.NewAlreadyExists(runsGR, reserved)
	}}, wf)
	tm := NewTriggerManager(env.c, unitTestScheme, nil)
	if _, err := capWebhook(tm, wf); !errors.Is(err, ErrWorkflowRunCreateFailed) {
		t.Fatalf("first call err = %v, want ErrWorkflowRunCreateFailed", err)
	}
	if got, err := capWebhook(tm, wf); err != nil || got != WebhookConcurrencyLimited {
		t.Fatalf("second call: result = %v, err = %v, want WebhookConcurrencyLimited", got, err)
	}

	stored := buildWorkflowRun(wf, reserved, nil, ottoflowv1alpha1.TriggerInfo{})
	if err := env.cache.Create(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	markRunSucceeded(t, env.cache, reserved)
	if got, err := capWebhook(tm, wf); err != nil || got != WebhookRunCreated {
		t.Fatalf("third call: result = %v, err = %v, want WebhookRunCreated", got, err)
	}
}

// An event Create that fails ambiguously keeps the slot until the cache shows the reserved name.
func TestRunCapEventAmbiguousErrorHoldsUntilNameAppears(t *testing.T) {
	t.Parallel()
	wf := capWorkflow(1)
	var reserved string
	env := newCapEnv(capOpts{createErr: func(call int, obj client.Object) error {
		if call > 1 {
			return nil
		}
		reserved = obj.GetName()
		return capTimeout
	}}, wf)
	tm := NewTriggerManager(env.c, unitTestScheme, nil)
	if err := capEvent(tm, wf, 0); err == nil {
		t.Fatal("first call err = nil, want the create error")
	}
	if err := capEvent(tm, wf, 1); err != nil {
		t.Fatalf("second call err = %v, want nil (dropped at the cap)", err)
	}
	if n := countRuns(t, env.live); n != 0 {
		t.Fatalf("runs = %d after the second call, want 0", n)
	}

	stored := buildWorkflowRun(wf, reserved, nil, ottoflowv1alpha1.TriggerInfo{})
	if err := env.live.Create(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	markRunSucceeded(t, env.live, reserved)
	if err := capEvent(tm, wf, 2); err != nil {
		t.Fatalf("third call err = %v, want nil", err)
	}
	if n := countRuns(t, env.live); n != 2 {
		t.Fatalf("runs = %d, want 2 (the named run and the third event's run)", n)
	}
}
