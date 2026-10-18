/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/agent"
)

// A failure that happened while the run ctx was live carries ErrFailedBeforeCancel even when the cancel
// lands before ExecuteWorkflow returns; a failure the cancel caused does not.
func TestFailureMarkedOnlyWhenCtxLiveAtFailure(t *testing.T) {
	missingKey := []ottoflowv1alpha1.Expression{{Name: "x", Expression: "variables.doesNotExist"}}
	for _, tc := range []struct {
		name string
		step ottoflowv1alpha1.Step
		// cancelWhen is checked from the progress callback.
		cancelWhen func(ottoflowv1alpha1.StepStatus) bool
		wantMarked bool
	}{
		{
			name:       "step error before cancel",
			step:       ottoflowv1alpha1.Step{Name: "s", Expressions: missingKey},
			cancelWhen: func(s ottoflowv1alpha1.StepStatus) bool { return s.Phase == ottoflowv1alpha1.StepPhaseFailed },
			wantMarked: true,
		},
		{
			name: "matchCondition error before cancel",
			step: ottoflowv1alpha1.Step{Name: "s", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: "1"}},
				MatchConditions: []ottoflowv1alpha1.MatchCondition{{Name: "m", Expression: "variables.doesNotExist"}}},
			cancelWhen: func(s ottoflowv1alpha1.StepStatus) bool { return s.Phase == ottoflowv1alpha1.StepPhaseFailed },
			wantMarked: true,
		},
		{
			name: "forEach cancelled mid-loop",
			step: ottoflowv1alpha1.Step{Name: "s", ForEach: &ottoflowv1alpha1.StepForEach{
				Items: `["a", "b", "c"]`, MaxConcurrency: 1,
				Step: &ottoflowv1alpha1.StepForEachStep{
					Outputs: []ottoflowv1alpha1.Output{{Name: "r", Expression: `variables.item`}},
				},
			}},
			cancelWhen: func(s ottoflowv1alpha1.StepStatus) bool { return s.Message == "1/3 items" },
			wantMarked: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			utilruntime.Must(ottoflowv1alpha1.AddToScheme(scheme))
			utilruntime.Must(clientgoscheme.AddToScheme(scheme))
			c := fake.NewClientBuilder().WithScheme(scheme).Build()
			run := &ottoflowv1alpha1.WorkflowRun{
				ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "default"},
				Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf"}},
			}
			wf := &ottoflowv1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
				Spec:       ottoflowv1alpha1.WorkflowSpec{Steps: []ottoflowv1alpha1.Step{tc.step}},
			}
			exec, err := NewWorkflowExecutorWithAgentExecutor(
				c, nil, nil, nil, run, agent.NewMockAgentExecutor(), false, 0, 5, nil)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			exec.SetProgressCallback(func(r *ottoflowv1alpha1.WorkflowRun, _ *ottoflowv1alpha1.Workflow) {
				if tc.cancelWhen(r.Status.StepStatuses["s"]) {
					cancel()
				}
			})
			err = exec.ExecuteWorkflow(ctx, wf, run)
			marked := errors.Is(err, ErrFailedBeforeCancel)
			t.Logf("err=%v ctxErr=%v runPhase=%s marked=%v", err, ctx.Err(), run.Status.Phase, marked)
			if err == nil || ctx.Err() == nil {
				t.Fatalf("setup: want a failure and a cancelled ctx, got err=%v ctxErr=%v", err, ctx.Err())
			}
			if marked != tc.wantMarked {
				t.Fatalf("marked=%v, want %v (err=%v)", marked, tc.wantMarked, err)
			}
		})
	}
}
