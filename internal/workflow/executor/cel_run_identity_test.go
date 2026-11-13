/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"context"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

// `run` exposes the identity of the WorkflowRun being executed so a workflow can mint ids
// that trace back to the run, or label output with it.

func newRunIdentityEvaluator(t *testing.T, workflowRun *ottoflowv1alpha1.WorkflowRun) *CELEvaluator {
	t.Helper()
	ev, err := NewCELEvaluatorWithMetrics(fake.NewClientBuilder().WithScheme(newTestScheme()).Build(), nil, nil, nil, nil, workflowRun, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func TestRunIdentity_BindsNamespaceNameAndUID(t *testing.T) {
	run := newTestWorkflowRun("nightly-scan")
	run.Namespace = "team-a"
	run.UID = "0f8e6a52-3c1d-4b8e-9a71-5d2c4e6f7a80"
	ev := newRunIdentityEvaluator(t, run)

	for expr, want := range map[string]interface{}{
		`run.namespace`:       "team-a",
		`run.name`:            "nightly-scan",
		`run.uid`:             "0f8e6a52-3c1d-4b8e-9a71-5d2c4e6f7a80",
		`"report-" + run.uid`: "report-0f8e6a52-3c1d-4b8e-9a71-5d2c4e6f7a80",
		`format("%s/%s", run.namespace, run.name)`: "team-a/nightly-scan",
	} {
		got, err := ev.EvaluateExpression(context.Background(), expr, ev.BuildVariableMap(map[string]interface{}{}))
		if err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		if got != want {
			t.Errorf("%s = %v, want %v", expr, got, want)
		}
	}
}

func TestRunIdentity_AllKeysPresentWithoutWorkflowRun(t *testing.T) {
	ev := newRunIdentityEvaluator(t, nil)
	got, err := ev.EvaluateExpression(context.Background(), `[run.namespace, run.name, run.uid]`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []interface{}{"", "", ""}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

// Workflows written before `run` existed may use it as a comprehension variable; the
// comprehension's own binding must still win.
func TestRunIdentity_ComprehensionVariableShadowsRun(t *testing.T) {
	run := newTestWorkflowRun("parent")
	run.UID = "uid-1"
	ev := newRunIdentityEvaluator(t, run)
	got, err := ev.EvaluateExpression(context.Background(), `[1, 2].map(run, run * 10)`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []interface{}{int64(10), int64(20)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestRunIdentity_CompilesInValidationEnv(t *testing.T) {
	env, err := NewValidationCELEnv()
	if err != nil {
		t.Fatal(err)
	}
	if _, iss := env.Compile(`"vex-" + run.namespace + "-" + run.name + "-" + run.uid`); iss.Err() != nil {
		t.Fatalf("run should be declared for admission-time compilation: %v", iss.Err())
	}
}

// An inline sub-workflow runs on an in-memory child WorkflowRun that is never created in the
// API server, so it has no UID. `run` must keep naming the parent WorkflowRun.
func TestRunIdentity_SubWorkflowSeesParentRun(t *testing.T) {
	sub := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "sub", Namespace: "default"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{
				{Name: "id", Outputs: []ottoflowv1alpha1.Output{{Name: "id", Expression: `run.namespace + "/" + run.name + "/" + run.uid`}}},
			},
			Outputs: []ottoflowv1alpha1.Output{{Name: "id", Expression: `variables.id`}},
		},
	}
	run := newTestWorkflowRun("parent")
	run.UID = "parent-uid"
	ex, err := NewWorkflowExecutor(fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(sub).Build(), run)
	if err != nil {
		t.Fatal(err)
	}
	scoped := map[string]interface{}{"inputs": map[string]interface{}{}, "variables": map[string]interface{}{}, "steps": map[string]interface{}{}, "stepStatus": map[string]interface{}{}}
	ctx := context.WithValue(context.Background(), scopedContextKey, scoped)
	out, err := ex.executeWorkflowReference(ctx, run, ottoflowv1alpha1.Step{Name: "call", WorkflowRef: &ottoflowv1alpha1.StepWorkflowRef{Name: "sub"}})
	if err != nil {
		t.Fatal(err)
	}
	if out["id"] != "default/parent/parent-uid" {
		t.Fatalf("sub-workflow run identity = %v, want default/parent/parent-uid", out["id"])
	}
}
