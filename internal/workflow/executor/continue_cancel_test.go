package executor

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/agent"
)

func TestContinueStepCancelledIsNotFinal(t *testing.T) {
	cont := ottoflowv1alpha1.FailurePolicyContinue
	missingKey := []ottoflowv1alpha1.Expression{{Name: "x", Expression: "variables.doesNotExist"}}
	one := []ottoflowv1alpha1.Expression{{Name: "x", Expression: "1"}}
	loop := ottoflowv1alpha1.Step{Name: "s", FailurePolicy: cont, ForEach: &ottoflowv1alpha1.StepForEach{
		Items: `["a", "b", "c"]`, MaxConcurrency: 1,
		Step: &ottoflowv1alpha1.StepForEachStep{Outputs: []ottoflowv1alpha1.Output{{Name: "r", Expression: `variables.item`}}},
	}}
	after := ottoflowv1alpha1.Step{Name: "after", DependsOn: []string{"s"}, Expressions: one}
	pre := ottoflowv1alpha1.Step{Name: "p", Expressions: one}
	matchCond := ottoflowv1alpha1.Step{Name: "s", DependsOn: []string{"p"}, FailurePolicy: cont, Expressions: one,
		MatchConditions: []ottoflowv1alpha1.MatchCondition{{Name: "m", Expression: "variables.doesNotExist"}}}
	single := ottoflowv1alpha1.Step{Name: "s", FailurePolicy: cont, Expressions: missingKey}
	oneOfThree := func(ss map[string]ottoflowv1alpha1.StepStatus) bool { return ss["s"].Message == "1/3 items" }

	for _, tc := range []struct {
		name       string
		steps      []ottoflowv1alpha1.Step
		ckpt       bool
		cancelWhen func(map[string]ottoflowv1alpha1.StepStatus) bool
	}{
		{"a_forEach_ckpt", []ottoflowv1alpha1.Step{loop, after}, true, oneOfThree},
		{"b_forEach_nockpt", []ottoflowv1alpha1.Step{loop, after}, false, oneOfThree},
		{"c_single_running", []ottoflowv1alpha1.Step{single, after}, false,
			func(ss map[string]ottoflowv1alpha1.StepStatus) bool {
				return ss["s"].Phase == ottoflowv1alpha1.StepPhaseRunning
			}},
		{"d_matchCondition", []ottoflowv1alpha1.Step{pre, matchCond, after}, false,
			func(ss map[string]ottoflowv1alpha1.StepStatus) bool {
				return ss["p"].Phase == ottoflowv1alpha1.StepPhaseSucceeded
			}},
		{"e_live_continue", []ottoflowv1alpha1.Step{single, after}, false, nil}, // nil: never cancelled
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(newTestScheme()).Build()
			run := newTestWorkflowRun("r")
			if tc.ckpt {
				run.Spec.Execution = &ottoflowv1alpha1.WorkflowRunExecutionSpec{Checkpointing: &ottoflowv1alpha1.CheckpointingConfig{Enabled: true}}
			}
			wf := &ottoflowv1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
				Spec:       ottoflowv1alpha1.WorkflowSpec{Steps: tc.steps},
			}
			exec, err := NewWorkflowExecutorWithAgentExecutor(c, nil, nil, nil, run, agent.NewMockAgentExecutor(), false, 0, 5, nil)
			if err != nil {
				t.Fatal(err)
			}
			var cm *CheckpointManager
			if tc.ckpt {
				cm = NewCheckpointManager(c, run)
				exec.SetCheckpointManager(cm)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			exec.SetProgressCallback(func(r *ottoflowv1alpha1.WorkflowRun, _ *ottoflowv1alpha1.Workflow) {
				if tc.cancelWhen != nil && tc.cancelWhen(r.Status.StepStatuses) {
					cancel()
				}
			})
			err = exec.ExecuteWorkflow(ctx, wf, run)
			s, af := run.Status.StepStatuses["s"], run.Status.StepStatuses["after"]

			if tc.cancelWhen != nil && ctx.Err() == nil {
				t.Fatal("setup: ctx was never cancelled")
			}
			if tc.cancelWhen == nil { // live Continue failure: unchanged, the run carries on and succeeds
				if err != nil || run.Status.Phase != ottoflowv1alpha1.WorkflowRunPhaseSucceeded ||
					s.Phase != ottoflowv1alpha1.StepPhaseFailed || af.Phase != ottoflowv1alpha1.StepPhaseSucceeded {
					t.Fatalf("live Continue failure changed: err=%v run=%s s=%s after=%s", err, run.Status.Phase, s.Phase, af.Phase)
				}
				return
			}
			if err == nil {
				t.Fatal("want err != nil for a Continue step failing under cancel")
			}
			if errors.Is(err, ErrFailedBeforeCancel) {
				t.Fatalf("err must not carry ErrFailedBeforeCancel: %v", err)
			}
			if run.Status.Phase == ottoflowv1alpha1.WorkflowRunPhaseSucceeded {
				t.Fatal("run must not be Succeeded")
			}
			if af.Phase != ottoflowv1alpha1.StepPhasePending {
				t.Fatalf("after=%s, want Pending", af.Phase)
			}
			if cm != nil {
				_ = cm.Flush(context.Background())
				if snap, _ := cm.Load(context.Background()); snap != nil && snap.StepStatuses["s"].Phase == ottoflowv1alpha1.StepPhaseFailed {
					t.Fatal("checkpoint holds the cancelled Continue step as Failed")
				}
			}
		})
	}
}
