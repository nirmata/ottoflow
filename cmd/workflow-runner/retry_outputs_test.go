/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/agent"
	workflowexecutor "github.com/nirmata/ottoflow/internal/workflow/executor"
)

// retryClient acts like the real client on a done ctx (the call fails before reaching the server) and
// injects checkpoint ConfigMap write failures: cmMode "ok" (none), "none" (every write fails, so
// no snapshot lands), "stale" (the snapshot taken after p2 fails, so the stored one is p1's).
func retryClient(cmMode string, objs ...client.Object) client.WithWatch {
	cmWrite := func(o client.Object) error {
		cm, ok := o.(*corev1.ConfigMap)
		if !ok || !strings.HasPrefix(cm.Name, "ottoflow-cp-") {
			return nil
		}
		if cmMode == "none" ||
			(cmMode == "stale" && strings.Contains(cm.Data["checkpoint"], `"lastCompletedStep":"p2"`)) {
			return errors.New("injected checkpoint write failure")
		}
		return nil
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&ottoflowv1alpha1.WorkflowRun{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, k client.ObjectKey, o client.Object,
				opts ...client.GetOption) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				return c.Get(ctx, k, o, opts...)
			},
			Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				if err := cmWrite(o); err != nil {
					return err
				}
				return c.Create(ctx, o, opts...)
			},
			Update: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.UpdateOption) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				if err := cmWrite(o); err != nil {
					return err
				}
				return c.Update(ctx, o, opts...)
			},
			Delete: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.DeleteOption) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				return c.Delete(ctx, o, opts...)
			},
			SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, o client.Object,
				opts ...client.SubResourceUpdateOption) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				return c.SubResource(sub).Update(ctx, o, opts...)
			},
		}).Build()
}

// p1 -> p2 -> consumer; consumer's items read both producers' outputs (context "variables").
func retryWorkflow() *ottoflowv1alpha1.Workflow {
	return &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec: ottoflowv1alpha1.WorkflowSpec{Steps: []ottoflowv1alpha1.Step{
			{Name: "p1", Outputs: []ottoflowv1alpha1.Output{{Name: "a", Expression: `"x"`}}},
			{Name: "p2", DependsOn: []string{"p1"},
				Outputs: []ottoflowv1alpha1.Output{{Name: "b", Expression: `"y"`}}},
			{Name: "consumer", DependsOn: []string{"p2"}, ForEach: &ottoflowv1alpha1.StepForEach{
				Items: `[variables.a, variables.b]`, MaxConcurrency: 1,
				Step: &ottoflowv1alpha1.StepForEachStep{
					Outputs: []ottoflowv1alpha1.Output{{Name: "r", Expression: `variables.item`}},
				},
			}},
		}},
	}
}

func retryExecutor(
	t *testing.T, c client.Client, run *ottoflowv1alpha1.WorkflowRun,
) *workflowexecutor.WorkflowExecutor {
	t.Helper()
	exec, err := workflowexecutor.NewWorkflowExecutorWithAgentExecutor(
		c, nil, nil, nil, run, agent.NewMockAgentExecutor(), false, 0, 5, nil)
	if err != nil {
		t.Fatal(err)
	}
	return exec
}

func TestRetryAfterSIGTERMHasOutputsOfSkippedSteps(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ckpt   bool
		cmMode string
		path   string // "controller": retryTransientFailure; "jobRecreated": new Job, status untouched
	}{
		{"ckptOn/controllerRetry/snapshot", true, "ok", "controller"},
		{"ckptOn/controllerRetry/noSnapshot", true, "none", "controller"},
		{"ckptOn/controllerRetry/staleSnapshot", true, "stale", "controller"},
		{"ckptOn/jobRecreated/snapshot", true, "ok", "jobRecreated"},
		{"ckptOn/jobRecreated/noSnapshot", true, "none", "jobRecreated"},
		{"ckptOn/jobRecreated/staleSnapshot", true, "stale", "jobRecreated"},
		{"ckptOff/jobRecreated", false, "ok", "jobRecreated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bg := context.Background()
			run := &ottoflowv1alpha1.WorkflowRun{
				ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "default", UID: types.UID("uid-r")},
				Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf"}},
				// What the controller stores before the first runner starts (workflowrun_controller.go:268-283).
				Status: ottoflowv1alpha1.WorkflowRunStatus{Phase: ottoflowv1alpha1.WorkflowRunPhasePending},
			}
			if tc.ckpt {
				run.Spec.Execution = &ottoflowv1alpha1.WorkflowRunExecutionSpec{
					Checkpointing: &ottoflowv1alpha1.CheckpointingConfig{Enabled: true}}
			}
			wf := retryWorkflow()
			c := retryClient(tc.cmMode, run, wf)
			key := client.ObjectKeyFromObject(run)
			cfg := runnerConfig{runName: "r", runNamespace: "default", runKey: key, jobName: "j", podName: "p1"}

			// Attempt 1: the runner's startup sequence from run(), then SIGTERM while consumer runs.
			r1 := mustLoadWorkflowRun(bg, c, key)
			mustPersistRunningStatus(bg, c, key, r1, cfg.jobName, cfg.podName)
			exec1 := retryExecutor(t, c, r1)
			cm1 := workflowexecutor.NewCheckpointManager(c, r1)
			exec1.SetCheckpointManager(cm1)
			sigCtx, cancel := context.WithCancel(bg)
			exec1.SetProgressCallback(func(r *ottoflowv1alpha1.WorkflowRun, _ *ottoflowv1alpha1.Workflow) {
				if r.Status.StepStatuses["consumer"].Phase == ottoflowv1alpha1.StepPhaseRunning {
					cancel() // SIGTERM while consumer runs
				}
			})
			if code := runWorkflowToCompletion(sigCtx, exec1, cm1, wf, r1, c, cfg); code != exitInterrupted {
				t.Fatalf("attempt 1 exit=%d, want %d", code, exitInterrupted)
			}
			cancel()
			_ = cm1.Flush(bg) // let attempt 1's async checkpoint saves land (or fail)

			stored := &ottoflowv1alpha1.WorkflowRun{}
			if err := c.Get(bg, key, stored); err != nil {
				t.Fatal(err)
			}

			if tc.path == "controller" {
				// retryTransientFailure, workflowrun_controller.go:1556-1576: step statuses untouched.
				if stored.Status.Execution == nil {
					stored.Status.Execution = &ottoflowv1alpha1.WorkflowRunExecutionStatus{}
				}
				stored.Status.Execution.Attempts++
				stored.Status.Execution.LastTerminationReason = "Error"
				stored.Status.Phase = ottoflowv1alpha1.WorkflowRunPhasePending
				stored.Status.CompletionTime = nil
				stored.Status.Execution.Phase = string(ottoflowv1alpha1.WorkflowRunPhasePending)
				stored.Status.Execution.CompletionTime = nil
				if err := c.Status().Update(bg, stored); err != nil {
					t.Fatal(err)
				}
			}

			// Attempt 2: the new runner pod's startup from run(). ExecuteWorkflow is called directly
			// because runWorkflowToCompletion os.Exits on failure.
			r2 := mustLoadWorkflowRun(bg, c, key)
			mustPersistRunningStatus(bg, c, key, r2, cfg.jobName, "p2")
			exec2 := retryExecutor(t, c, r2)
			exec2.SetCheckpointManager(workflowexecutor.NewCheckpointManager(c, r2))
			err := exec2.ExecuteWorkflow(bg, wf, r2)
			cons := r2.Status.StepStatuses["consumer"]
			if err != nil || r2.Status.Phase != ottoflowv1alpha1.WorkflowRunPhaseSucceeded {
				t.Errorf("retry did not complete: err=%v phase=%s consumer %s: %s", err, r2.Status.Phase, cons.Phase, cons.Error)
			}
		})
	}
}

// The runner's startup write keeps the controller's retry bookkeeping.
func TestMustPersistRunningStatusKeepsAttempts(t *testing.T) {
	run := newRun()
	run.Status.Execution = &ottoflowv1alpha1.WorkflowRunExecutionStatus{Attempts: 2, LastTerminationReason: "Error"}
	c, _ := ctxAwareClient(run)
	key := client.ObjectKeyFromObject(run)

	mustPersistRunningStatus(context.Background(), c, key, run, "j", "p")

	stored := &ottoflowv1alpha1.WorkflowRun{}
	if err := c.Get(context.Background(), key, stored); err != nil {
		t.Fatal(err)
	}
	if e := stored.Status.Execution; e == nil || e.Attempts != 2 || e.LastTerminationReason != "Error" ||
		e.PodName != "p" {
		t.Fatalf("execution not preserved: %+v", e)
	}
}
