/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"context"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

// newWebhookStatusEnv builds a TriggerManager whose WorkflowRun Get returns get(call) when non-nil,
// and whose WorkflowRun Create drops status, as the apiserver does, then calls after. Either may
// be nil. The returned fake client reads runs back.
func newWebhookStatusEnv(
	get func(call int) error,
	after func(ctx context.Context, c client.WithWatch, run *ottoflowv1alpha1.WorkflowRun),
) (*TriggerManager, client.Client, *ottoflowv1alpha1.Workflow) {
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec:       ottoflowv1alpha1.WorkflowSpec{Steps: []ottoflowv1alpha1.Step{{Name: "s1", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `"ok"`}}}}},
	}
	fc := makeFakeClient(wf)
	gets := 0
	c := interceptor.NewClient(fc.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*ottoflowv1alpha1.WorkflowRun); ok && get != nil {
				gets++
				if err := get(gets); err != nil {
					return err
				}
			}
			return c.Get(ctx, key, obj, opts...)
		},
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			run, ok := obj.(*ottoflowv1alpha1.WorkflowRun)
			if ok {
				run.Status = ottoflowv1alpha1.WorkflowRunStatus{} // a real apiserver drops status on Create
			}
			if err := c.Create(ctx, obj, opts...); err != nil {
				return err
			}
			if ok && after != nil {
				after(ctx, c, run)
			}
			return nil
		},
	})
	return NewTriggerManager(c, unitTestScheme, nil), fc, wf
}

func getRunStatus(t *testing.T, c client.Client, run *ottoflowv1alpha1.WorkflowRun) ottoflowv1alpha1.WorkflowRunStatus {
	t.Helper()
	var got ottoflowv1alpha1.WorkflowRun
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(run), &got); err != nil {
		t.Fatal(err)
	}
	return got.Status
}

// The cached client may not see a just-created run yet. The status write must keep retrying past
// NotFound instead of giving up and leaving the run without status.trigger.
// Mutant: stop retrying on the first NotFound.
func TestCreateWorkflowRunFromWebhook_StatusRetriesPastCacheLag(t *testing.T) {
	notFound := apierrors.NewNotFound(runsGR, "")
	tm, c, wf := newWebhookStatusEnv(func(call int) error {
		if call == 1 {
			return notFound
		}
		return nil
	}, nil)

	run, result, err := tm.CreateWorkflowRunFromWebhook(context.Background(), wf, &ottoflowv1alpha1.WebhookTrigger{}, nil, WebhookRequestMeta{RequestID: "req-1"})
	if err != nil || result != WebhookRunCreated {
		t.Fatalf("result = %v, err = %v, want WebhookRunCreated, nil", result, err)
	}
	got := getRunStatus(t, c, run)
	if got.Trigger == nil || got.Trigger.Type != "Webhook" || got.Trigger.WebhookRequest == nil || got.Trigger.WebhookRequest.RequestID != "req-1" {
		t.Fatalf("Trigger = %+v, want Type Webhook with request ID req-1", got.Trigger)
	}
	if got.Phase != ottoflowv1alpha1.WorkflowRunPhasePending {
		t.Fatalf("Phase = %q, want Pending", got.Phase)
	}
}

// The reconciler can start the run before the status write. Only the trigger info is added; the
// phase and execution status it wrote are kept.
// Mutants: replace the whole status; always set Pending.
func TestCreateWorkflowRunFromWebhook_StatusKeepsReconciledStatus(t *testing.T) {
	at := metav1.NewTime(time.Unix(1_800_000_000, 0))
	reconciled := ottoflowv1alpha1.WorkflowRunStatus{
		Phase:     ottoflowv1alpha1.WorkflowRunPhaseRunning,
		StartTime: &at,
		Execution: &ottoflowv1alpha1.WorkflowRunExecutionStatus{Phase: "Running", JobName: "wf-job", StartTime: &at},
	}
	tm, c, wf := newWebhookStatusEnv(nil, func(ctx context.Context, c client.WithWatch, run *ottoflowv1alpha1.WorkflowRun) {
		var stored ottoflowv1alpha1.WorkflowRun
		if err := c.Get(ctx, client.ObjectKeyFromObject(run), &stored); err != nil {
			t.Fatal(err)
		}
		stored.Status = *reconciled.DeepCopy()
		if err := c.Status().Update(ctx, &stored); err != nil {
			t.Fatal(err)
		}
	})

	run, result, err := tm.CreateWorkflowRunFromWebhook(context.Background(), wf, &ottoflowv1alpha1.WebhookTrigger{}, nil, WebhookRequestMeta{})
	if err != nil || result != WebhookRunCreated {
		t.Fatalf("result = %v, err = %v, want WebhookRunCreated, nil", result, err)
	}
	got := getRunStatus(t, c, run)
	if got.Trigger == nil || got.Trigger.Type != "Webhook" {
		t.Fatalf("Trigger = %+v, want Type Webhook", got.Trigger)
	}
	got.Trigger = nil
	if diff := cmp.Diff(reconciled, got); diff != "" {
		t.Fatalf("status (-want +got):\n%s", diff)
	}
}

// A run whose status write failed still exists, so the webhook still reports it as created: an
// error would make the sender retry and create a second run.
// Mutant: return the status write error.
func TestCreateWorkflowRunFromWebhook_StatusFailureStillCreated(t *testing.T) {
	t.Parallel() // ~0.8s in the status loop's sleeps
	tm, c, wf := newWebhookStatusEnv(nil, nil)
	tm.client = &failingStatusWriteClient{Client: tm.client}

	run, result, err := tm.CreateWorkflowRunFromWebhook(context.Background(), wf, &ottoflowv1alpha1.WebhookTrigger{}, nil, WebhookRequestMeta{})
	if err != nil || result != WebhookRunCreated {
		t.Fatalf("result = %v, err = %v, want WebhookRunCreated, nil", result, err)
	}
	if run == nil || run.Name == "" {
		t.Fatalf("run = %+v, want the created run", run)
	}
	if n := countRuns(t, c); n != 1 {
		t.Fatalf("runs = %d, want 1", n)
	}
}
