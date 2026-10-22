/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

func runStepStatusWorkflow(t *testing.T, steps []ottoflowv1alpha1.Step, outputs []ottoflowv1alpha1.Output) (*ottoflowv1alpha1.WorkflowRun, *WorkflowExecutor, error) {
	t.Helper()
	run := newTestWorkflowRun("test-run")
	ex, err := NewWorkflowExecutor(fake.NewClientBuilder().WithScheme(newTestScheme()).Build(), run)
	if err != nil {
		t.Fatal(err)
	}
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec:       ottoflowv1alpha1.WorkflowSpec{Steps: steps, Outputs: outputs},
	}
	return run, ex, ex.ExecuteWorkflow(context.Background(), wf, run)
}

func failingStep(name string) ottoflowv1alpha1.Step {
	return ottoflowv1alpha1.Step{
		Name:          name,
		FailurePolicy: ottoflowv1alpha1.FailurePolicyContinue,
		Outputs:       []ottoflowv1alpha1.Output{{Name: "x", Expression: "undefined_var"}},
	}
}

func readVar(t *testing.T, ex *WorkflowExecutor, name string) interface{} {
	t.Helper()
	cd, err := ex.GetContextManager().ReadContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return cd["variables"].(map[string]interface{})[name]
}

func TestStepStatus_CELEnvDeclaresStepStatus(t *testing.T) {
	env, err := NewValidationCELEnv()
	if err != nil {
		t.Fatal(err)
	}
	if _, iss := env.Compile(`stepStatus.x.failed`); iss != nil && iss.Err() != nil {
		t.Fatalf("stepStatus.x.failed should compile: %v", iss.Err())
	}
}

func TestStepStatus_FailedStepReadByLaterStep(t *testing.T) {
	run, ex, err := runStepStatusWorkflow(t, []ottoflowv1alpha1.Step{
		failingStep("first"),
		{
			Name:      "second",
			DependsOn: []string{"first"},
			MatchConditions: []ottoflowv1alpha1.MatchCondition{
				{Name: "ok", Expression: `stepStatus.first.failed == true`},
			},
			Outputs: []ottoflowv1alpha1.Output{
				{Name: "phase", Expression: `stepStatus.first.phase`},
				{Name: "ok", Expression: `stepStatus.first.succeeded`},
				{Name: "hasErr", Expression: `size(stepStatus.first.error) > 0`},
			},
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := run.Status.StepStatuses["second"].Phase; got != ottoflowv1alpha1.StepPhaseSucceeded {
		t.Fatalf("second phase = %s, want Succeeded", got)
	}
	if got := readVar(t, ex, "phase"); got != "Failed" {
		t.Errorf("phase = %v, want Failed", got)
	}
	if got := readVar(t, ex, "ok"); got != false {
		t.Errorf("succeeded = %v, want false", got)
	}
	if got := readVar(t, ex, "hasErr"); got != true {
		t.Errorf("error non-empty = %v, want true", got)
	}
}

func TestStepStatus_WorkflowOutputsReadStepStatus(t *testing.T) {
	run, _, err := runStepStatusWorkflow(t,
		[]ottoflowv1alpha1.Step{failingStep("first")},
		[]ottoflowv1alpha1.Output{
			{Name: "failed", Expression: `stepStatus.first.failed`},
			{Name: "again", Expression: `outputs.failed`},
		})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"failed", "again"} {
		if got := string(run.Status.Outputs[name].Raw); got != "true" {
			t.Errorf("output %s = %q, want true", name, got)
		}
	}
}

// the live context keeps outputs and gets no stepStatus from the outputs path.
func TestStepStatus_OutputsPathKeepsLiveContext(t *testing.T) {
	_, ex, err := runStepStatusWorkflow(t,
		[]ottoflowv1alpha1.Step{failingStep("first")},
		[]ottoflowv1alpha1.Output{{Name: "failed", Expression: `stepStatus.first.failed`}})
	if err != nil {
		t.Fatal(err)
	}
	live := ex.GetContextManager().GetContext()
	if _, ok := live["outputs"]; !ok {
		t.Error("live context lost outputs")
	}
	if _, ok := live["stepStatus"]; ok {
		t.Error("live context must not hold stepStatus")
	}
}

// Skipped and matchCondition-error steps are published.
func TestStepStatus_SkippedAndMatchConditionError(t *testing.T) {
	_, ex, err := runStepStatusWorkflow(t, []ottoflowv1alpha1.Step{
		{
			Name:            "skipped",
			MatchConditions: []ottoflowv1alpha1.MatchCondition{{Name: "no", Expression: `false`}},
		},
		{
			Name:            "broken",
			FailurePolicy:   ottoflowv1alpha1.FailurePolicyContinue,
			MatchConditions: []ottoflowv1alpha1.MatchCondition{{Name: "bad", Expression: `undefined_var`}},
		},
		{
			Name:      "reader",
			DependsOn: []string{"skipped", "broken"},
			Outputs: []ottoflowv1alpha1.Output{
				{Name: "s", Expression: `stepStatus.skipped.phase`},
				{Name: "b", Expression: `stepStatus.broken.failed`},
			},
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := readVar(t, ex, "s"); got != "Skipped" {
		t.Errorf("skipped phase = %v", got)
	}
	if got := readVar(t, ex, "b"); got != true {
		t.Errorf("broken failed = %v", got)
	}
}

// forEach keeps its steps.<name>.failed list and gets a bool stepStatus.
func TestStepStatus_ForEachCompat(t *testing.T) {
	_, ex, err := runStepStatusWorkflow(t, []ottoflowv1alpha1.Step{
		{
			Name: "fe",
			ForEach: &ottoflowv1alpha1.StepForEach{
				Items: `["a", "b"]`,
				Step: &ottoflowv1alpha1.StepForEachStep{
					Outputs: []ottoflowv1alpha1.Output{{Name: "r", Expression: `variables.item`}},
				},
			},
		},
		{
			Name:      "after",
			DependsOn: []string{"fe"},
			Outputs: []ottoflowv1alpha1.Output{
				{Name: "n", Expression: `size(steps.fe.failed)`},
				{Name: "f", Expression: `stepStatus.fe.failed`},
			},
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(readVar(t, ex, "n")); got != "0" {
		t.Errorf("size(steps.fe.failed) = %v", got)
	}
	if got := readVar(t, ex, "f"); got != false {
		t.Errorf("stepStatus.fe.failed = %v", got)
	}
}

// context budgets leave stepStatus alone.
func TestStepStatus_SurvivesContextBudget(t *testing.T) {
	cd := map[string]interface{}{
		"steps":      map[string]interface{}{"a": 1},
		"stepStatus": map[string]interface{}{"a": map[string]interface{}{"failed": false}},
	}
	for name, got := range map[string]map[string]interface{}{
		"omit":  applyOmitBudget(cd),
		"lastN": copyContextWithSteps(cd, map[string]interface{}{}),
	} {
		ss, _ := got["stepStatus"].(map[string]interface{})
		if _, ok := ss["a"]; !ok {
			t.Errorf("%s budget dropped stepStatus", name)
		}
	}
}

// an unfinished step is absent, so the reference errors instead of reading false.
func TestStepStatus_UnfinishedStepIsAbsent(t *testing.T) {
	run := &ottoflowv1alpha1.WorkflowRun{
		Status: ottoflowv1alpha1.WorkflowRunStatus{StepStatuses: map[string]ottoflowv1alpha1.StepStatus{
			"pending": {Phase: ottoflowv1alpha1.StepPhasePending},
			"running": {Phase: ottoflowv1alpha1.StepPhaseRunning},
			"waiting": {Phase: ottoflowv1alpha1.StepPhaseWaiting},
			"done":    {Phase: ottoflowv1alpha1.StepPhaseSucceeded},
		}},
	}
	cm := NewContextManager(run)
	wf := &ottoflowv1alpha1.Workflow{}
	if err := cm.InitializeContext(context.Background(), wf, nil); err != nil {
		t.Fatal(err)
	}
	cd, err := cm.ReadContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ss, _ := cd["stepStatus"].(map[string]interface{})
	if len(ss) != 1 || ss["done"] == nil {
		t.Fatalf("stepStatus keys = %v, want only done", ss)
	}

	ce, err := NewCELEvaluator(fake.NewClientBuilder().WithScheme(newTestScheme()).Build(), run)
	if err != nil {
		t.Fatal(err)
	}
	vars := ce.BuildVariableMap(cd)
	_, err = ce.EvaluateExpression(context.Background(), `stepStatus.pending.failed`, vars)
	if err == nil || !strings.Contains(err.Error(), "no such key") {
		t.Fatalf("pending reference error = %v, want no such key", err)
	}
}

// 40 parallel forEach items must not race with the step-status progress writes (run with -race).
func TestStepStatus_ForEachItemsDoNotRace(t *testing.T) {
	items := make([]string, 40)
	for i := range items {
		items[i] = fmt.Sprintf(`"i%d"`, i)
	}
	_, _, err := runStepStatusWorkflow(t, []ottoflowv1alpha1.Step{
		{
			Name: "fe",
			ForEach: &ottoflowv1alpha1.StepForEach{
				Items:          "[" + strings.Join(items, ",") + "]",
				MaxConcurrency: 10,
				Step: &ottoflowv1alpha1.StepForEachStep{
					Outputs: []ottoflowv1alpha1.Output{{Name: "r", Expression: `variables.item`}},
				},
			},
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
}

func TestStepStatus_ChildWorkflowInScopedCtxReadsOwnStatus(t *testing.T) {
	sub := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "sub", Namespace: "default"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{
				{Name: "collect", Outputs: []ottoflowv1alpha1.Output{{Name: "c", Expression: `"x"`}}},
				{Name: "report", DependsOn: []string{"collect"}, Outputs: []ottoflowv1alpha1.Output{{Name: "r", Expression: `stepStatus.collect.succeeded`}}},
			},
			Outputs: []ottoflowv1alpha1.Output{{Name: "r", Expression: `variables.r`}},
		},
	}
	run := newTestWorkflowRun("parent")
	ex, err := NewWorkflowExecutor(fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(sub).Build(), run)
	if err != nil {
		t.Fatal(err)
	}
	// Frozen parent snapshot as a forEach item holds it: no "collect" key.
	scoped := map[string]interface{}{"inputs": map[string]interface{}{}, "variables": map[string]interface{}{}, "steps": map[string]interface{}{}, "stepStatus": map[string]interface{}{}}
	ctx := context.WithValue(context.Background(), scopedContextKey, scoped)
	out, err := ex.executeWorkflowReference(ctx, run, ottoflowv1alpha1.Step{Name: "call", WorkflowRef: &ottoflowv1alpha1.StepWorkflowRef{Name: "sub"}})
	if err != nil {
		t.Fatal(err)
	}
	if out["r"] != true {
		t.Fatalf("child report step should read its own collect status, got %v", out)
	}
}
