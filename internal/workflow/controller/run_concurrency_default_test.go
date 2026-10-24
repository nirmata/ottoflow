package controller

import (
	"context"
	"fmt"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

func TestEffectiveMaxConcurrentRuns(t *testing.T) {
	zero, three := ptr.To[int32](0), ptr.To[int32](3)
	for name, tc := range map[string]struct {
		run  *ottoflowv1alpha1.RunPolicy
		want int32
	}{
		"nil run":    {nil, 10},
		"nil field":  {&ottoflowv1alpha1.RunPolicy{}, 10},
		"explicit 0": {&ottoflowv1alpha1.RunPolicy{MaxConcurrentRuns: zero}, 0},
		"explicit 3": {&ottoflowv1alpha1.RunPolicy{MaxConcurrentRuns: three}, 3},
	} {
		t.Run(name, func(t *testing.T) {
			wf := &ottoflowv1alpha1.Workflow{Spec: ottoflowv1alpha1.WorkflowSpec{Run: tc.run}}
			if got := effectiveMaxConcurrentRuns(wf); got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestCountActiveWorkflowRuns_CountsEmptyPhase(t *testing.T) {
	ctx := context.Background()
	wf := &ottoflowv1alpha1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"}}
	runs := []client.Object{
		defaultCapRun("wf-new", ""),
		defaultCapRun("wf-done", ottoflowv1alpha1.WorkflowRunPhaseSucceeded),
	}
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(runs...).Build()
	got, err := countActiveWorkflowRuns(ctx, c, wf)
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Errorf("got %d active runs, want 1 (empty phase counts, Succeeded does not)", got)
	}
}

func defaultCapRun(name string, phase ottoflowv1alpha1.WorkflowRunPhase) *ottoflowv1alpha1.WorkflowRun {
	return &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: map[string]string{"ottoflow.nirmata.io/workflow": "wf"}},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: "default"}},
		Status:     ottoflowv1alpha1.WorkflowRunStatus{Phase: phase},
	}
}

// defaultCapFixture returns a Workflow with no spec.run (or the given policy) and a client holding 10 Running runs for it.
func defaultCapFixture(run *ottoflowv1alpha1.RunPolicy) (*ottoflowv1alpha1.Workflow, client.Client) {
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{{Name: "s1", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `"ok"`}}}},
			Run:   run,
		},
	}
	runs := make([]client.Object, int(ottoflowv1alpha1.DefaultMaxConcurrentRuns))
	for i := range runs {
		runs[i] = defaultCapRun(fmt.Sprintf("wf-%d", i), ottoflowv1alpha1.WorkflowRunPhaseRunning)
	}
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(runs...).WithObjects(wf).WithObjects(runs...).Build()
	return wf, c
}

func TestTriggerManager_CreateWorkflowRunFromEvent_DefaultCap(t *testing.T) {
	zero := ptr.To[int32](0)
	for name, tc := range map[string]struct {
		run  *ottoflowv1alpha1.RunPolicy
		want int
	}{
		"unset run is capped at 10": {nil, 10},
		"explicit 0 is unlimited":   {&ottoflowv1alpha1.RunPolicy{MaxConcurrentRuns: zero}, 11},
	} {
		t.Run(name, func(t *testing.T) {
			wf, c := defaultCapFixture(tc.run)
			eventSpec := &ottoflowv1alpha1.EventTrigger{
				Resources:  []ottoflowv1alpha1.EventResource{{APIVersion: "v1", Kind: "Pod", Namespace: "default"}},
				Operations: []string{"CREATE"},
			}
			tm := NewTriggerManager(c, unitTestScheme, nil)
			if err := tm.CreateWorkflowRunFromEvent(context.Background(), "test-trigger", wf, eventSpec, makeUnstructured("p1", "default", nil), "ADDED"); err != nil {
				t.Fatal(err)
			}
			if got := countRuns(t, c); got != tc.want {
				t.Errorf("got %d runs, want %d", got, tc.want)
			}
		})
	}
}

func TestScheduler_HandleCronFire_DefaultCap(t *testing.T) {
	zero := ptr.To[int32](0)
	for name, tc := range map[string]struct {
		run  *ottoflowv1alpha1.RunPolicy
		want int
	}{
		"unset run is capped at 10": {nil, 10},
		"explicit 0 is unlimited":   {&ottoflowv1alpha1.RunPolicy{MaxConcurrentRuns: zero}, 11},
	} {
		t.Run(name, func(t *testing.T) {
			wf, c := defaultCapFixture(tc.run)
			fireCron(t, c, wf, &ottoflowv1alpha1.CronTrigger{Schedule: "0 * * * *"})
			if got := countRuns(t, c); got != tc.want {
				t.Errorf("got %d runs, want %d", got, tc.want)
			}
		})
	}
}
