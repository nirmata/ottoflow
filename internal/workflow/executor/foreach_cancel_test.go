/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/agent"
)

// A forEach cancelled after one of three items must fail the step, not report Succeeded.
func TestForEachPartialUnderCancel(t *testing.T) {
	scheme := runtime.NewScheme()
	utilruntime.Must(ottoflowv1alpha1.AddToScheme(scheme))
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	run := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "default"},
		Spec: ottoflowv1alpha1.WorkflowRunSpec{
			WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf"},
			Execution: &ottoflowv1alpha1.WorkflowRunExecutionSpec{
				Checkpointing: &ottoflowv1alpha1.CheckpointingConfig{Enabled: true},
			},
		},
	}
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Variables: []ottoflowv1alpha1.Variable{{Name: "items", Expression: `["a", "b", "c"]`}},
			Steps: []ottoflowv1alpha1.Step{
				{Name: "loop", ForEach: &ottoflowv1alpha1.StepForEach{
					Items: `variables.items`, MaxConcurrency: 1,
					Step: &ottoflowv1alpha1.StepForEachStep{Outputs: []ottoflowv1alpha1.Output{{Name: "r", Expression: `variables.item`}}},
				}},
				{Name: "after", DependsOn: []string{"loop"}, Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: "1"}}},
			},
		},
	}
	exec, err := NewWorkflowExecutorWithAgentExecutor(c, nil, nil, nil, run, agent.NewMockAgentExecutor(), false, 0, 5, nil)
	if err != nil {
		t.Fatal(err)
	}
	cm := NewCheckpointManager(c, run)
	exec.SetCheckpointManager(cm)
	ctx, cancel := context.WithCancel(context.Background())
	exec.SetProgressCallback(func(r *ottoflowv1alpha1.WorkflowRun, _ *ottoflowv1alpha1.Workflow) {
		if r.Status.StepStatuses["loop"].Message == "1/3 items" {
			cancel() // SIGTERM arrives after the first item
		}
	})
	if err := exec.contextManager.InitializeContext(ctx, wf, run.Spec.InputValues); err != nil {
		t.Fatal(err)
	}
	err = exec.ExecuteWorkflow(ctx, wf, run)
	loop, after := run.Status.StepStatuses["loop"], run.Status.StepStatuses["after"]
	t.Logf("ExecuteWorkflow err=%v ctxErr=%v | runPhase=%s | loop=%s msg=%q | after=%s",
		err, ctx.Err(), run.Status.Phase, loop.Phase, loop.Message, after.Phase)
	_ = cm.Flush(context.Background())
	if snap, _ := cm.Load(context.Background()); snap != nil {
		t.Logf("checkpoint: loop=%s lastCompleted=%s", snap.StepStatuses["loop"].Phase, snap.LastCompletedStep)
		if snap.StepStatuses["loop"].Phase == ottoflowv1alpha1.StepPhaseSucceeded {
			t.Errorf("checkpoint records the cancelled forEach as Succeeded")
		}
	} else {
		t.Logf("checkpoint: none")
	}
	if loop.Phase == ottoflowv1alpha1.StepPhaseSucceeded || run.Status.Phase == ottoflowv1alpha1.WorkflowRunPhaseSucceeded || err == nil {
		t.Errorf("cancelled forEach reported success: err=%v loop=%s run=%s", err, loop.Phase, run.Status.Phase)
	}
}
