/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

func runnerFailureFixture(checkpointing bool, status batchv1.JobStatus) (*ottoflowv1alpha1.WorkflowRun, *batchv1.Job, *corev1.ConfigMap, client.Client) {
	wf := &ottoflowv1alpha1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"}}
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "startup-retry", Namespace: "default"},
		Spec: ottoflowv1alpha1.WorkflowRunSpec{
			WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: wf.Name},
			Execution: &ottoflowv1alpha1.WorkflowRunExecutionSpec{
				Checkpointing: &ottoflowv1alpha1.CheckpointingConfig{Enabled: checkpointing},
			},
		},
		Status: ottoflowv1alpha1.WorkflowRunStatus{
			Phase: ottoflowv1alpha1.WorkflowRunPhaseRunning,
			Execution: &ottoflowv1alpha1.WorkflowRunExecutionStatus{
				JobName: workflowRunnerJobName("startup-retry"), PodName: "first-pod",
				Phase: "Failed", Message: "transient startup failure", Attempts: 1,
				LastTerminationReason: "Evicted",
			},
		},
	}
	backoff := int32(1)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: wr.Status.Execution.JobName, Namespace: wr.Namespace},
		Spec:       batchv1.JobSpec{BackoffLimit: &backoff},
		Status:     status,
	}
	checkpoint := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "ottoflow-cp-" + wr.Name, Namespace: wr.Namespace}}
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(wr).
		WithObjects(wf, wr, job, checkpoint).Build()
	return wr, job, checkpoint, c
}

func TestReconcileRunnerJobWaitsForTerminalFailure(t *testing.T) {
	for _, checkpointing := range []bool{false, true} {
		for _, tc := range []struct {
			name   string
			status batchv1.JobStatus
		}{
			{"active replacement", batchv1.JobStatus{Active: 1, Failed: 1}},
			{"backoff gap", batchv1.JobStatus{Failed: 1}},
			{"completed after retry", batchv1.JobStatus{Succeeded: 1, Failed: 1, Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
			}}},
			{"false failed condition", batchv1.JobStatus{Failed: 1, Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobFailed, Status: corev1.ConditionFalse},
			}}},
		} {
			t.Run(fmt.Sprintf("checkpointing=%t/%s", checkpointing, tc.name), func(t *testing.T) {
				wr, job, checkpoint, c := runnerFailureFixture(checkpointing, tc.status)
				r := &WorkflowRunReconciler{Client: c, Scheme: unitTestScheme}
				result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(wr)})
				if err != nil {
					t.Fatal(err)
				}
				if result.RequeueAfter != 2*time.Second {
					t.Errorf("RequeueAfter = %v, want continued monitoring", result.RequeueAfter)
				}
				stored := &ottoflowv1alpha1.WorkflowRun{}
				if err := c.Get(context.Background(), client.ObjectKeyFromObject(wr), stored); err != nil {
					t.Fatal(err)
				}
				if stored.Status.Phase != ottoflowv1alpha1.WorkflowRunPhaseRunning || stored.Status.CompletionTime != nil ||
					stored.Status.Execution.Attempts != 1 || stored.Status.Execution.LastTerminationReason != "Evicted" {
					t.Errorf("nonterminal Job was adjudicated: %+v", stored.Status)
				}
				if err := c.Get(context.Background(), client.ObjectKeyFromObject(job), &batchv1.Job{}); err != nil {
					t.Errorf("same Job must remain for replacement/backoff: %v", err)
				}
				if err := c.Get(context.Background(), client.ObjectKeyFromObject(checkpoint), &corev1.ConfigMap{}); err != nil {
					t.Errorf("checkpoint must remain while Job is nonterminal: %v", err)
				}
			})
		}
	}
}

func TestReconcileRunnerJobAdjudicatesTerminalFailure(t *testing.T) {
	for _, checkpointing := range []bool{false, true} {
		for _, tc := range []struct {
			name      string
			condition batchv1.JobConditionType
			reason    string
			failed    int32
		}{
			{"backoff exhausted", batchv1.JobFailed, batchv1.JobReasonBackoffLimitExceeded, 1},
			{"deadline failed", batchv1.JobFailed, batchv1.JobReasonDeadlineExceeded, 1},
			{"deadline failure target before pod exit", batchv1.JobFailureTarget, batchv1.JobReasonDeadlineExceeded, 0},
		} {
			t.Run(fmt.Sprintf("checkpointing=%t/%s", checkpointing, tc.name), func(t *testing.T) {
				wr, job, checkpoint, c := runnerFailureFixture(checkpointing, batchv1.JobStatus{
					Failed:     tc.failed,
					Conditions: []batchv1.JobCondition{{Type: tc.condition, Status: corev1.ConditionTrue, Reason: tc.reason}},
				})
				r := &WorkflowRunReconciler{Client: c, Scheme: unitTestScheme}
				if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(wr)}); err != nil {
					t.Fatal(err)
				}
				stored := &ottoflowv1alpha1.WorkflowRun{}
				if err := c.Get(context.Background(), client.ObjectKeyFromObject(wr), stored); err != nil {
					t.Fatal(err)
				}
				retry := checkpointing && tc.reason != batchv1.JobReasonDeadlineExceeded
				if retry {
					if stored.Status.Phase != ottoflowv1alpha1.WorkflowRunPhasePending || stored.Status.Execution.Attempts != 2 || stored.Status.CompletionTime != nil {
						t.Errorf("failed Job did not retain checkpoint retry policy: %+v", stored.Status)
					}
					if err := c.Get(context.Background(), client.ObjectKeyFromObject(job), &batchv1.Job{}); !apierrors.IsNotFound(err) {
						t.Errorf("failed Job must be deleted for controller retry: %v", err)
					}
					if err := c.Get(context.Background(), client.ObjectKeyFromObject(checkpoint), &corev1.ConfigMap{}); err != nil {
						t.Errorf("retry must retain checkpoint: %v", err)
					}
				} else {
					if stored.Status.Phase != ottoflowv1alpha1.WorkflowRunPhaseFailed || stored.Status.Execution.Attempts != 1 || stored.Status.CompletionTime == nil {
						t.Errorf("terminal failure not recorded: %+v", stored.Status)
					}
					if stored.Status.Execution.Message != "transient startup failure" {
						t.Errorf("startup failure cause lost during terminal Job adjudication: %+v", stored.Status.Execution)
					}
					if tc.reason == batchv1.JobReasonDeadlineExceeded && stored.Status.Execution.LastTerminationReason != tc.reason {
						t.Errorf("deadline reason lost: %+v", stored.Status.Execution)
					}
					if err := c.Get(context.Background(), client.ObjectKeyFromObject(job), &batchv1.Job{}); err != nil {
						t.Errorf("terminal Job must not be retried: %v", err)
					}
					if checkpointing {
						if err := c.Get(context.Background(), client.ObjectKeyFromObject(checkpoint), &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
							t.Errorf("terminal failure must delete checkpoint: %v", err)
						}
					}
				}
			})
		}
	}
}
