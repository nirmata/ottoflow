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

// The runner reads Status.Phase and Status.Message from the in-memory WorkflowRun, so the
// guard must record the failure rather than only return an error.
func TestExecuteWorkflow_NoStepsRecordsFailure(t *testing.T) {
	wr := newTestWorkflowRun("run-no-steps")
	k8s := fake.NewClientBuilder().WithScheme(newTestScheme()).
		WithStatusSubresource(wr).WithObjects(wr).Build()
	exec, err := NewWorkflowExecutor(k8s, wr)
	if err != nil {
		t.Fatalf("NewWorkflowExecutor: %v", err)
	}
	wf := &ottoflowv1alpha1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"}}

	err = exec.ExecuteWorkflow(context.Background(), wf, wr)
	if err == nil || !strings.Contains(err.Error(), "workflow has no steps") {
		t.Fatalf("expected a 'workflow has no steps' error, got: %v", err)
	}
	if wr.Status.Phase != ottoflowv1alpha1.WorkflowRunPhaseFailed {
		t.Errorf("Phase = %q, want Failed", wr.Status.Phase)
	}
	if wr.Status.Message != "workflow has no steps" {
		t.Errorf("Message = %q, want %q", wr.Status.Message, "workflow has no steps")
	}
	if wr.Status.CompletionTime == nil {
		t.Error("CompletionTime is nil; retention cleanup needs it on a terminal run")
	}
}
