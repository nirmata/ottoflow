/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

func TestBuildWorkflowRunCopiesWorkflowExecution(t *testing.T) {
	wf := &ottoflowv1alpha1.Workflow{
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Execution: &ottoflowv1alpha1.WorkflowRunExecutionSpec{
				Job: &ottoflowv1alpha1.WorkflowRunJobSpec{
					Env: []corev1.EnvVar{{Name: "LLM_TOKEN", Value: "test-value"}},
				},
			},
		},
	}

	run := buildWorkflowRun(wf, "run", nil, ottoflowv1alpha1.TriggerInfo{})
	if diff := cmp.Diff(wf.Spec.Execution, run.Spec.Execution); diff != "" {
		t.Fatalf("run execution (-want +got):\n%s", diff)
	}

	wf.Spec.Execution.Job.Env[0].Value = "mutated"
	if got := run.Spec.Execution.Job.Env[0].Value; got != "test-value" {
		t.Fatalf("run shares memory with the Workflow: env value is %q", got)
	}
}

func TestCreateWorkflowRunFromWebhookCopiesWorkflowExecution(t *testing.T) {
	ctx := context.Background()
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Execution: &ottoflowv1alpha1.WorkflowRunExecutionSpec{
				Job: &ottoflowv1alpha1.WorkflowRunJobSpec{
					Env: []corev1.EnvVar{{Name: "LLM_TOKEN", Value: "test-value"}},
				},
			},
		},
	}
	// Status().Update needs the status subresource registered.
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(&ottoflowv1alpha1.WorkflowRun{}).WithObjects(wf).Build()
	run, _, err := NewTriggerManager(c, unitTestScheme, nil).CreateWorkflowRunFromWebhook(ctx, wf, &ottoflowv1alpha1.WebhookTrigger{}, nil, WebhookRequestMeta{})
	if err != nil {
		t.Fatal(err)
	}
	var got ottoflowv1alpha1.WorkflowRun
	if err := c.Get(ctx, client.ObjectKeyFromObject(run), &got); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(wf.Spec.Execution, got.Spec.Execution); diff != "" {
		t.Fatalf("run execution (-want +got):\n%s", diff)
	}
}

func TestBuildWorkflowRunWithoutWorkflowExecution(t *testing.T) {
	run := buildWorkflowRun(&ottoflowv1alpha1.Workflow{}, "run", nil, ottoflowv1alpha1.TriggerInfo{})
	if run.Spec.Execution != nil {
		t.Fatalf("expected nil run execution, got %+v", run.Spec.Execution)
	}
}
