/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

// The runner persists Status.Phase and Status.Message from the in-memory WorkflowRun, so
// deterministic pre-step failures must be recorded there, not only returned.
func TestExecuteWorkflow_NoStepsRecordsFailure(t *testing.T) {
	assertPreStepFailureRecorded(t, ottoflowv1alpha1.WorkflowSpec{}, "workflow has no steps")
}

func TestExecuteWorkflow_InitContextFailureRecordsFailure(t *testing.T) {
	assertPreStepFailureRecorded(t, ottoflowv1alpha1.WorkflowSpec{
		Inputs: []ottoflowv1alpha1.Input{{Name: "appName", Required: true}},
		Steps:  []ottoflowv1alpha1.Step{{Name: "a"}},
	}, "required input appName not provided")
}

func TestExecuteWorkflow_BuildDAGFailureRecordsFailure(t *testing.T) {
	assertPreStepFailureRecorded(t, ottoflowv1alpha1.WorkflowSpec{
		Steps: []ottoflowv1alpha1.Step{{Name: "a", DependsOn: []string{"missing"}}},
	}, "depends on non-existent step")
}

func assertPreStepFailureRecorded(t *testing.T, spec ottoflowv1alpha1.WorkflowSpec, wantErr string) {
	t.Helper()
	wr := newTestWorkflowRun("run-pre-step")
	k8s := fake.NewClientBuilder().WithScheme(newTestScheme()).
		WithStatusSubresource(wr).WithObjects(wr).Build()
	exec, err := NewWorkflowExecutor(k8s, wr)
	if err != nil {
		t.Fatalf("NewWorkflowExecutor: %v", err)
	}
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec:       spec,
	}

	err = exec.ExecuteWorkflow(context.Background(), wf, wr)
	if err == nil || !strings.Contains(err.Error(), wantErr) {
		t.Fatalf("expected error containing %q, got: %v", wantErr, err)
	}
	if wr.Status.Phase != ottoflowv1alpha1.WorkflowRunPhaseFailed {
		t.Errorf("Phase = %q, want Failed", wr.Status.Phase)
	}
	if !strings.Contains(wr.Status.Message, wantErr) {
		t.Errorf("Message = %q, want it to contain %q", wr.Status.Message, wantErr)
	}
	if wr.Status.CompletionTime == nil {
		t.Error("CompletionTime is nil; retention cleanup needs it on a terminal run")
	}
}
