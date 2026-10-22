/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package webhook

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

// transientErrorClient wraps a client.Client and makes Get return a non-NotFound error
// to test the "transient error → warn and allow" path in WorkflowValidator.
type transientErrorClient struct {
	client.Client
	getErr error
}

func (t *transientErrorClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	return t.getErr
}

func TestWorkflowValidator_ValidateCreate_DAGCycle(t *testing.T) {
	v := &WorkflowValidator{Client: nil}
	w := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{
				{Name: "a", DependsOn: []string{"b"}},
				{Name: "b", DependsOn: []string{"a"}},
			},
		},
	}
	_, err := v.ValidateCreate(context.Background(), w)
	if err == nil {
		t.Fatal("expected error for cycle")
	}
}

func TestWorkflowRunValidator_EmptyWorkflowRef(t *testing.T) {
	v := &WorkflowRunValidator{}
	run := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run", Namespace: "default"},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{},
	}
	_, err := v.ValidateCreate(context.Background(), run)
	if err == nil {
		t.Fatal("expected error for empty workflowRef.name")
	}
	expected := `WorkflowRun "run" spec.workflowRef.name is required`
	if err.Error() != expected {
		t.Errorf("error message: got %q, want %q", err.Error(), expected)
	}
}

func TestValidateStepDependencies_StepsRefWithoutDependsOn(t *testing.T) {
	w := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{
				{Name: "first", Outputs: []ottoflowv1alpha1.Output{{Name: "x", Expression: `"ok"`}}},
				{
					Name: "second",
					// Missing DependsOn: []string{"first"}
					Outputs: []ottoflowv1alpha1.Output{{Name: "out", Expression: `steps.first.outputs.x`}},
				},
			},
		},
	}
	err := ValidateStepDependencies(&w.Spec)
	if err == nil {
		t.Fatal("expected error when step references steps.first without dependsOn")
	}
	if err != nil && err.Error() != "step \"second\" references steps.first but does not list \"first\" in dependsOn (dependencies must be explicit)" {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidateStepDependencies_StepsRefWithDependsOn(t *testing.T) {
	w := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{
				{Name: "first", Outputs: []ottoflowv1alpha1.Output{{Name: "x", Expression: `"ok"`}}},
				{
					Name:      "second",
					DependsOn: []string{"first"},
					Outputs:   []ottoflowv1alpha1.Output{{Name: "out", Expression: `steps.first.outputs.x`}},
				},
			},
		},
	}
	err := ValidateStepDependencies(&w.Spec)
	if err != nil {
		t.Errorf("expected no error when dependsOn is set: %v", err)
	}
}

func TestValidateStepDependencies_VariablesRefFromStepWithoutDependsOn(t *testing.T) {
	w := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{
				{Name: "producer", Outputs: []ottoflowv1alpha1.Output{{Name: "result", Expression: `"done"`}}},
				{
					Name: "consumer",
					// Missing DependsOn: []string{"producer"}
					Outputs: []ottoflowv1alpha1.Output{{Name: "out", Expression: `variables.result`}},
				},
			},
		},
	}
	err := ValidateStepDependencies(&w.Spec)
	if err == nil {
		t.Fatal("expected error when step references variables.result (from producer) without dependsOn")
	}
	for _, want := range []string{`step "consumer"`, "variables.result", "producer", "dependsOn"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// A variable written by more than one step needs a dependency on only one of them.
// Demanding all producers forces edges the workflow does not need and, when the other
// producer runs downstream, turns a valid workflow into a reported cycle.
func TestValidateStepDependencies_MultipleProducersNeedOnlyOne(t *testing.T) {
	w := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{
				{Name: "early", Outputs: []ottoflowv1alpha1.Output{{Name: "region", Expression: `"us-west-2"`}}},
				{
					Name:      "consumer",
					DependsOn: []string{"early"},
					Outputs:   []ottoflowv1alpha1.Output{{Name: "out", Expression: `variables.region`}},
				},
				// Also writes region, and runs after consumer. Requiring a dependency on
				// this one too would be a cycle.
				{
					Name:      "late",
					DependsOn: []string{"consumer"},
					Outputs:   []ottoflowv1alpha1.Output{{Name: "region", Expression: `"eu-west-1"`}},
				},
			},
		},
	}
	if err := ValidateStepDependencies(&w.Spec); err != nil {
		t.Errorf("expected no error when one producer is declared, got: %v", err)
	}
}

// Still an error when none of the producers is declared.
func TestValidateStepDependencies_MultipleProducersNoneDeclared(t *testing.T) {
	w := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{
				{Name: "a", Outputs: []ottoflowv1alpha1.Output{{Name: "region", Expression: `"us-west-2"`}}},
				{Name: "b", Outputs: []ottoflowv1alpha1.Output{{Name: "region", Expression: `"eu-west-1"`}}},
				{Name: "consumer", Outputs: []ottoflowv1alpha1.Output{{Name: "out", Expression: `variables.region`}}},
			},
		},
	}
	err := ValidateStepDependencies(&w.Spec)
	if err == nil {
		t.Fatal("expected an error when no producer of variables.region is declared")
	}
	// The message must name every candidate so the author can choose.
	for _, want := range []string{"a", "b", "variables.region"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestValidateStepDependencies_VariablesRefFromStepWithDependsOn(t *testing.T) {
	w := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{
				{Name: "producer", Outputs: []ottoflowv1alpha1.Output{{Name: "result", Expression: `"done"`}}},
				{
					Name:      "consumer",
					DependsOn: []string{"producer"},
					Outputs:   []ottoflowv1alpha1.Output{{Name: "out", Expression: `variables.result`}},
				},
			},
		},
	}
	err := ValidateStepDependencies(&w.Spec)
	if err != nil {
		t.Errorf("expected no error when dependsOn is set: %v", err)
	}
}

func TestValidateStepDependencies_WorkflowVariableNoDependsOn(t *testing.T) {
	w := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Variables: []ottoflowv1alpha1.Variable{{Name: "globalCount", Expression: `42`}},
			Steps: []ottoflowv1alpha1.Step{
				{
					Name:    "useGlobal",
					Outputs: []ottoflowv1alpha1.Output{{Name: "out", Expression: `variables.globalCount`}},
				},
			},
		},
	}
	err := ValidateStepDependencies(&w.Spec)
	if err != nil {
		t.Errorf("workflow-level variables do not require step dependency: %v", err)
	}
}

func TestValidateInputRefs(t *testing.T) {
	declared := []ottoflowv1alpha1.Input{{Name: "city"}, {Name: "limit"}}

	cases := []struct {
		name    string
		spec    *ottoflowv1alpha1.WorkflowSpec
		wantErr bool
	}{
		{
			name: "all referenced inputs are declared",
			spec: &ottoflowv1alpha1.WorkflowSpec{
				Inputs: declared,
				Steps: []ottoflowv1alpha1.Step{
					{Name: "s", Expressions: []ottoflowv1alpha1.Expression{
						{Name: "x", Expression: `inputs.city + string(inputs.limit)`},
					}},
				},
			},
		},
		{
			name: "step references undeclared input",
			spec: &ottoflowv1alpha1.WorkflowSpec{
				Inputs: declared,
				Steps: []ottoflowv1alpha1.Step{
					{Name: "s", Expressions: []ottoflowv1alpha1.Expression{
						{Name: "x", Expression: `inputs.city + inputs.unknown`},
					}},
				},
			},
			wantErr: true,
		},
		{
			name: "workflow variable references undeclared input",
			spec: &ottoflowv1alpha1.WorkflowSpec{
				Inputs:    declared,
				Variables: []ottoflowv1alpha1.Variable{{Name: "v", Expression: `inputs.typo`}},
			},
			wantErr: true,
		},
		{
			name: "workflow output references undeclared input",
			spec: &ottoflowv1alpha1.WorkflowSpec{
				Inputs:  declared,
				Outputs: []ottoflowv1alpha1.Output{{Name: "out", Expression: `inputs.notDeclared`}},
			},
			wantErr: true,
		},
		{
			name: "no inputs declared but expressions reference inputs",
			spec: &ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{Name: "s", Expressions: []ottoflowv1alpha1.Expression{
						{Name: "x", Expression: `inputs.whatever`},
					}},
				},
			},
			wantErr: true,
		},
		{
			name: "no inputs referenced — always valid",
			spec: &ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{Name: "s", Expressions: []ottoflowv1alpha1.Expression{
						{Name: "x", Expression: `"static"`},
					}},
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateInputRefs(tc.spec)
			if tc.wantErr && err == nil {
				t.Error("expected validation error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("expected no error, got: %v", err)
			}
		})
	}
}

func TestValidateInputRefs_StringLiteralNotFlagged(t *testing.T) {
	// inputs.foo inside a quoted string must not trigger UNDEFINED_INPUT.
	spec := &ottoflowv1alpha1.WorkflowSpec{
		Inputs: []ottoflowv1alpha1.Input{{Name: "city"}},
		Steps: []ottoflowv1alpha1.Step{
			{Name: "s", Expressions: []ottoflowv1alpha1.Expression{
				{Name: "x", Expression: `"please set inputs.unknown as a plain string"`},
			}},
		},
	}
	if err := ValidateInputRefs(spec); err != nil {
		t.Errorf("string literal inputs.* reference should not produce UNDEFINED_INPUT, got: %v", err)
	}
}

func TestWorkflowValidator_ValidateUpdate(t *testing.T) {
	v := &WorkflowValidator{Client: nil}
	oldW := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{{Name: "a", Outputs: []ottoflowv1alpha1.Output{{Name: "x", Expression: `"ok"`}}}},
		},
	}
	newW := oldW.DeepCopy()
	newW.Spec.Steps = append(newW.Spec.Steps, ottoflowv1alpha1.Step{
		Name: "b", DependsOn: []string{"a"}, Outputs: []ottoflowv1alpha1.Output{{Name: "y", Expression: `steps.a.outputs.x`}},
	})
	_, err := v.ValidateUpdate(context.Background(), oldW, newW)
	if err != nil {
		t.Errorf("ValidateUpdate: unexpected error: %v", err)
	}
}

func TestWorkflowValidator_ValidateDelete(t *testing.T) {
	v := &WorkflowValidator{}
	w := &ottoflowv1alpha1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"}}
	warnings, err := v.ValidateDelete(context.Background(), w)
	if err != nil {
		t.Errorf("ValidateDelete: unexpected error: %v", err)
	}
	if warnings != nil {
		t.Errorf("ValidateDelete: expected nil warnings, got %v", warnings)
	}
}

func TestWorkflowValidator_ValidateCreate_NilAndEmptySteps(t *testing.T) {
	v := &WorkflowValidator{Client: nil}
	// nil workflow is handled by admission framework; validate with empty steps returns nil
	w := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec:       ottoflowv1alpha1.WorkflowSpec{Steps: []ottoflowv1alpha1.Step{}},
	}
	_, err := v.ValidateCreate(context.Background(), w)
	if err != nil {
		t.Errorf("empty steps should allow validation to pass: %v", err)
	}
}

func TestWorkflowValidator_ValidateCreate_NilWorkflow(t *testing.T) {
	v := &WorkflowValidator{Client: nil}
	warnings, err := v.ValidateCreate(context.Background(), nil)
	if err != nil {
		t.Errorf("nil workflow should return nil error: %v", err)
	}
	if warnings != nil {
		t.Errorf("nil workflow should return nil warnings: %v", warnings)
	}
}

func TestWorkflowValidator_ValidateCreate_WorkflowRefExplicitNamespace(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = ottoflowv1alpha1.AddToScheme(scheme)
	refWf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "sub-wf", Namespace: "other-ns"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{{Name: "s1", Outputs: []ottoflowv1alpha1.Output{{Name: "x", Expression: `"ok"`}}}},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(refWf).Build()
	v := &WorkflowValidator{Client: fakeClient}
	w := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{
				{
					Name:        "ref",
					WorkflowRef: &ottoflowv1alpha1.StepWorkflowRef{Name: "sub-wf", Namespace: "other-ns"},
					Outputs:     []ottoflowv1alpha1.Output{{Name: "x", Expression: `"ok"`}},
				},
			},
		},
	}
	_, err := v.ValidateCreate(context.Background(), w)
	if err != nil {
		t.Errorf("WorkflowRef with explicit namespace should resolve in that namespace: %v", err)
	}
}

func TestWorkflowValidator_ValidateCreate_ClientGetTransientErrorWarns(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = ottoflowv1alpha1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	transientErr := errors.New("server temporarily unavailable")
	wrappingClient := &transientErrorClient{Client: fakeClient, getErr: transientErr}
	v := &WorkflowValidator{Client: wrappingClient}
	w := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{
				{Name: "ref", WorkflowRef: &ottoflowv1alpha1.StepWorkflowRef{Name: "any-wf"}, Outputs: []ottoflowv1alpha1.Output{{Name: "x", Expression: `"ok"`}}},
			},
		},
	}
	warnings, err := v.ValidateCreate(context.Background(), w)
	if err != nil {
		t.Fatalf("transient Get error should not fail validation: %v", err)
	}
	if len(warnings) == 0 {
		t.Fatal("expected at least one warning when Get returns non-NotFound error")
	}
	if !strings.Contains(warnings[0], "could not verify WorkflowRef") {
		t.Errorf("warning should mention WorkflowRef verification: %q", warnings[0])
	}
}

func TestWorkflowValidator_ValidateCreate_AgentRefTransientErrorWarns(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = ottoflowv1alpha1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	wrappingClient := &transientErrorClient{Client: fakeClient, getErr: fmt.Errorf("etcd timeout")}
	v := &WorkflowValidator{Client: wrappingClient}
	w := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{
				{Name: "agent", AgentRef: &ottoflowv1alpha1.StepAgentRef{Name: "my-agent"}, Outputs: []ottoflowv1alpha1.Output{{Name: "x", Expression: `"ok"`}}},
			},
		},
	}
	warnings, err := v.ValidateCreate(context.Background(), w)
	if err != nil {
		t.Fatalf("transient Get error for AgentRef should not fail validation: %v", err)
	}
	if len(warnings) == 0 {
		t.Fatal("expected at least one warning when Get returns non-NotFound error for AgentRef")
	}
	if !strings.Contains(warnings[0], "could not verify AgentRef") {
		t.Errorf("warning should mention AgentRef verification: %q", warnings[0])
	}
}

func TestWorkflowValidator_ValidateCreate_ValidDAGNoClient(t *testing.T) {
	v := &WorkflowValidator{Client: nil}
	w := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{
				{Name: "a", Outputs: []ottoflowv1alpha1.Output{{Name: "x", Expression: `"v"`}}},
				{Name: "b", DependsOn: []string{"a"}, Outputs: []ottoflowv1alpha1.Output{{Name: "y", Expression: `steps.a.outputs.x`}}},
			},
		},
	}
	_, err := v.ValidateCreate(context.Background(), w)
	if err != nil {
		t.Errorf("valid DAG with no client: %v", err)
	}
}

func TestWorkflowValidator_ValidateCreate_WorkflowRefNotFound(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = ottoflowv1alpha1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	v := &WorkflowValidator{Client: fakeClient}
	w := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{
				{Name: "ref", WorkflowRef: &ottoflowv1alpha1.StepWorkflowRef{Name: "missing-wf"}, Outputs: []ottoflowv1alpha1.Output{{Name: "x", Expression: `"ok"`}}},
			},
		},
	}
	_, err := v.ValidateCreate(context.Background(), w)
	if err == nil {
		t.Fatal("expected error when WorkflowRef not found")
	}
	if err != nil && !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' in error: %v", err)
	}
}

func TestWorkflowValidator_ValidateCreate_AgentRefNotFound(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = ottoflowv1alpha1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	v := &WorkflowValidator{Client: fakeClient}
	w := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{
				{Name: "agent", AgentRef: &ottoflowv1alpha1.StepAgentRef{Name: "missing-agent"}, Outputs: []ottoflowv1alpha1.Output{{Name: "x", Expression: `"ok"`}}},
			},
		},
	}
	_, err := v.ValidateCreate(context.Background(), w)
	if err == nil {
		t.Fatal("expected error when AgentRef not found")
	}
	if err != nil && !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' in error: %v", err)
	}
}

func TestWorkflowValidator_ValidateCreate_WorkflowRefExists(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = ottoflowv1alpha1.AddToScheme(scheme)
	refWf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "sub-wf", Namespace: "default"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{{Name: "s1", Outputs: []ottoflowv1alpha1.Output{{Name: "x", Expression: `"ok"`}}}},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(refWf).Build()
	v := &WorkflowValidator{Client: fakeClient}
	w := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{
				{Name: "ref", WorkflowRef: &ottoflowv1alpha1.StepWorkflowRef{Name: "sub-wf"}, Outputs: []ottoflowv1alpha1.Output{{Name: "x", Expression: `"ok"`}}},
			},
		},
	}
	_, err := v.ValidateCreate(context.Background(), w)
	if err != nil {
		t.Errorf("when WorkflowRef exists validation should pass: %v", err)
	}
}

func TestWorkflowRunValidator_ValidateUpdate(t *testing.T) {
	v := &WorkflowRunValidator{}
	run := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run", Namespace: "default"},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf"}},
	}
	_, err := v.ValidateUpdate(context.Background(), nil, run)
	if err != nil {
		t.Errorf("ValidateUpdate: %v", err)
	}
}

func TestWorkflowRunValidator_ValidateDelete(t *testing.T) {
	v := &WorkflowRunValidator{}
	run := &ottoflowv1alpha1.WorkflowRun{ObjectMeta: metav1.ObjectMeta{Name: "run"}}
	warnings, err := v.ValidateDelete(context.Background(), run)
	if err != nil {
		t.Errorf("ValidateDelete: %v", err)
	}
	if warnings != nil {
		t.Errorf("ValidateDelete: expected nil warnings")
	}
}

func TestWorkflowRunValidator_ValidateCreate_NilRun(t *testing.T) {
	v := &WorkflowRunValidator{}
	// validate with nil run returns nil (handled by admission)
	warnings, err := v.ValidateCreate(context.Background(), nil)
	if err != nil || warnings != nil {
		t.Errorf("nil run: err=%v warnings=%v", err, warnings)
	}
}

func TestAgentValidator_AllMethods(t *testing.T) {
	v := &AgentValidator{}
	a := &ottoflowv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "default"}}
	w, err := v.ValidateCreate(context.Background(), a)
	if err != nil || w != nil {
		t.Errorf("ValidateCreate: w=%v err=%v", w, err)
	}
	w, err = v.ValidateUpdate(context.Background(), nil, a)
	if err != nil || w != nil {
		t.Errorf("ValidateUpdate: w=%v err=%v", w, err)
	}
	w, err = v.ValidateDelete(context.Background(), a)
	if err != nil || w != nil {
		t.Errorf("ValidateDelete: w=%v err=%v", w, err)
	}
}

func TestMCPServerValidator_AllMethods(t *testing.T) {
	v := &MCPServerValidator{}
	m := &ottoflowv1alpha1.MCPServer{ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"}}
	w, err := v.ValidateCreate(context.Background(), m)
	if err != nil || w != nil {
		t.Errorf("ValidateCreate: w=%v err=%v", w, err)
	}
	w, err = v.ValidateUpdate(context.Background(), nil, m)
	if err != nil || w != nil {
		t.Errorf("ValidateUpdate: w=%v err=%v", w, err)
	}
	w, err = v.ValidateDelete(context.Background(), m)
	if err != nil || w != nil {
		t.Errorf("ValidateDelete: w=%v err=%v", w, err)
	}
}

func TestValidateStepDependencies_ForEachStepRefsWithoutDependsOn(t *testing.T) {
	// ForEach inline step that references steps.other must have dependsOn
	w := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{
				{Name: "other", Outputs: []ottoflowv1alpha1.Output{{Name: "x", Expression: `"v"`}}},
				{
					Name: "fe",
					ForEach: &ottoflowv1alpha1.StepForEach{
						Items: "[1,2,3]",
						Step: &ottoflowv1alpha1.StepForEachStep{
							Expressions: []ottoflowv1alpha1.Expression{{Name: "e", Expression: `steps.other.outputs.x`}},
						},
					},
				},
			},
		},
	}
	err := ValidateStepDependencies(&w.Spec)
	if err == nil {
		t.Fatal("expected error when forEach step references steps.other without dependsOn")
	}
}

func TestValidateStepDependencies_ForEachStepRefsWithDependsOn(t *testing.T) {
	w := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{
				{Name: "other", Outputs: []ottoflowv1alpha1.Output{{Name: "x", Expression: `"v"`}}},
				{
					Name:      "fe",
					DependsOn: []string{"other"},
					ForEach: &ottoflowv1alpha1.StepForEach{
						Items: "[1,2,3]",
						Step: &ottoflowv1alpha1.StepForEachStep{
							Outputs: []ottoflowv1alpha1.Output{{Name: "out", Expression: `variables.x`}},
						},
					},
				},
			},
		},
	}
	err := ValidateStepDependencies(&w.Spec)
	if err != nil {
		t.Errorf("forEach with dependsOn should pass: %v", err)
	}
}

func TestValidateStepDependencies_NilAndEmptySpec(t *testing.T) {
	if err := ValidateStepDependencies(nil); err != nil {
		t.Errorf("nil spec should return nil: %v", err)
	}
	empty := &ottoflowv1alpha1.WorkflowSpec{Steps: []ottoflowv1alpha1.Step{}}
	if err := ValidateStepDependencies(empty); err != nil {
		t.Errorf("empty steps should return nil: %v", err)
	}
}

func TestValidateStepDependencies_StepRefToNonExistentStepSkipped(t *testing.T) {
	// Reference to non-existent step is skipped (DAG validation catches invalid dependsOn)
	w := &ottoflowv1alpha1.WorkflowSpec{
		Steps: []ottoflowv1alpha1.Step{
			{Name: "a", Outputs: []ottoflowv1alpha1.Output{{Name: "x", Expression: `steps.nonexistent.outputs.y`}}},
		},
	}
	err := ValidateStepDependencies(w)
	if err != nil {
		t.Errorf("reference to non-existent step is skipped, should not error: %v", err)
	}
}

func TestValidateStepDependencies_SelfReferenceNoDependsOnRequired(t *testing.T) {
	// Self-reference (steps.self) does not require listing self in dependsOn
	w := &ottoflowv1alpha1.WorkflowSpec{
		Steps: []ottoflowv1alpha1.Step{
			{Name: "self", Outputs: []ottoflowv1alpha1.Output{{Name: "x", Expression: `steps.self.outputs.x`}}},
		},
	}
	err := ValidateStepDependencies(w)
	if err != nil {
		t.Errorf("self-reference should not require dependsOn: %v", err)
	}
}

func TestValidateStepDependencies_ForEachVariableRefWithoutDependsOn(t *testing.T) {
	w := &ottoflowv1alpha1.WorkflowSpec{
		Steps: []ottoflowv1alpha1.Step{
			{Name: "producer", Outputs: []ottoflowv1alpha1.Output{{Name: "data", Expression: `"v"`}}},
			{
				Name: "fe",
				ForEach: &ottoflowv1alpha1.StepForEach{
					Items: "[1,2,3]",
					Step: &ottoflowv1alpha1.StepForEachStep{
						Outputs: []ottoflowv1alpha1.Output{{Name: "out", Expression: `variables.data`}},
					},
				},
			},
		},
	}
	err := ValidateStepDependencies(w)
	if err == nil {
		t.Fatal("expected error when forEach references variables.data (from producer) without dependsOn")
	}
	if err != nil && !strings.Contains(err.Error(), "forEach references variables.data") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestValidateStepDependencies_CollectCELStringsFromStepBranches exercises collectCELStringsFromStep
// by using steps with WorkflowRef inputs, AgentRef prompts, MCPToolCall, ResourceQuery, PrometheusQuery,
// Mutate, StepTemplateRef, ForEach, MatchConditions, and Output Value.Raw.
func TestValidateStepDependencies_CollectCELStringsFromStepBranches(t *testing.T) {
	rawJSON := []byte(`"static"`)
	spec := &ottoflowv1alpha1.WorkflowSpec{
		Steps: []ottoflowv1alpha1.Step{
			{Name: "first", Outputs: []ottoflowv1alpha1.Output{{Name: "x", Expression: `"ok"`}}},
			{
				Name:        "second",
				DependsOn:   []string{"first"},
				Expressions: []ottoflowv1alpha1.Expression{{Name: "e", Expression: `steps.first.outputs.x`}},
				Outputs: []ottoflowv1alpha1.Output{
					{Name: "a", Expression: `"a"`},
					{Name: "b", Value: &apiextensionsv1.JSON{Raw: rawJSON}},
				},
				MatchConditions: []ottoflowv1alpha1.MatchCondition{{Name: "m", Expression: `true`}},
				WorkflowRef:     &ottoflowv1alpha1.StepWorkflowRef{Name: "w", Inputs: map[string]string{"in": `steps.first.outputs.x`}},
				AgentRef:        &ottoflowv1alpha1.StepAgentRef{Name: "ag", AdditionalPrompts: []string{`variables.x`}},
				MCPToolCall:     &ottoflowv1alpha1.StepMCPToolCall{Server: "s", Tool: "t", Arguments: map[string]string{"arg": `"v"`}},
				ResourceQuery: &ottoflowv1alpha1.StepResourceQuery{
					APIVersion: "v1", Resource: "Pod",
					Namespace: "default", Name: "p", FieldSelector: "x", LabelSelector: map[string]string{"l": "v"},
					Outputs: map[string]string{"o": `"v"`},
				},
				PrometheusQuery: &ottoflowv1alpha1.StepPrometheusQuery{
					Query: "up", TimeRange: "5m",
					Variables: map[string]string{"v": `"x"`}, Outputs: map[string]string{"o": `"v"`},
				},
				Mutate: &ottoflowv1alpha1.StepMutate{
					PatchType:          "ApplyConfiguration",
					Target:             ottoflowv1alpha1.StepMutateTarget{APIVersion: "v1", Resource: "Pod", Namespace: "default", Name: "p"},
					ApplyConfiguration: &ottoflowv1alpha1.MutateApplyConfiguration{Expression: `object`},
					Outputs:            map[string]string{"o": `"v"`},
				},
				StepTemplateRef: &ottoflowv1alpha1.StepTemplateRef{Name: "st", Arguments: map[string]string{"a": `"v"`}},
			},
		},
	}
	err := ValidateStepDependencies(spec)
	if err != nil {
		t.Errorf("valid deps with all step fields set: %v", err)
	}
}

// TestValidateStepDependencies_CollectCELStringsFromForEachStepBranches exercises collectCELStringsFromForEachStep.
func TestValidateStepDependencies_CollectCELStringsFromForEachStepBranches(t *testing.T) {
	rawJSON := []byte(`"x"`)
	spec := &ottoflowv1alpha1.WorkflowSpec{
		Steps: []ottoflowv1alpha1.Step{
			{Name: "upstream", Outputs: []ottoflowv1alpha1.Output{{Name: "r", Expression: `"v"`}}},
			{
				Name:      "fe",
				DependsOn: []string{"upstream"},
				ForEach: &ottoflowv1alpha1.StepForEach{
					Items: "variables.r",
					Step: &ottoflowv1alpha1.StepForEachStep{
						Expressions: []ottoflowv1alpha1.Expression{{Name: "e", Expression: `"e"`}},
						Outputs: []ottoflowv1alpha1.Output{
							{Name: "o", Expression: `"o"`},
							{Name: "v", Value: &apiextensionsv1.JSON{Raw: rawJSON}},
						},
						MatchConditions: []ottoflowv1alpha1.MatchCondition{{Name: "m", Expression: `true`}},
						ResourceQuery: &ottoflowv1alpha1.StepResourceQuery{
							APIVersion: "v1", Resource: "Pod", Namespace: "default", Name: "p",
							FieldSelector: "f", Outputs: map[string]string{"o": `"v"`}, LabelSelector: map[string]string{"l": "v"},
						},
						PrometheusQuery: &ottoflowv1alpha1.StepPrometheusQuery{
							Query: "up", TimeRange: "5m",
							Variables: map[string]string{"v": `"x"`}, Outputs: map[string]string{"o": `"v"`},
						},
						Mutate: &ottoflowv1alpha1.StepMutate{
							PatchType:          "ApplyConfiguration",
							Target:             ottoflowv1alpha1.StepMutateTarget{APIVersion: "v1", Resource: "Pod", Namespace: "ns", Name: "n"},
							ApplyConfiguration: &ottoflowv1alpha1.MutateApplyConfiguration{Expression: `object`},
							JSONPatch:          &ottoflowv1alpha1.MutateJSONPatch{Expression: `[]`},
							Outputs:            map[string]string{"o": `"v"`},
						},
						AgentRef:    &ottoflowv1alpha1.StepAgentRef{Name: "a", AdditionalPrompts: []string{`"p"`}},
						MCPToolCall: &ottoflowv1alpha1.StepMCPToolCall{Server: "s", Tool: "t", Arguments: map[string]string{"a": `"v"`}},
						WorkflowRef: &ottoflowv1alpha1.StepWorkflowRef{Name: "w", Inputs: map[string]string{"i": `"v"`}},
					},
				},
			},
		},
	}
	err := ValidateStepDependencies(spec)
	if err != nil {
		t.Errorf("valid forEach step with all inner fields: %v", err)
	}
}

func TestValidateStepDependencies_StepWithMutateJSONPatchExpression(t *testing.T) {
	spec := &ottoflowv1alpha1.WorkflowSpec{
		Steps: []ottoflowv1alpha1.Step{
			{
				Name: "m",
				Mutate: &ottoflowv1alpha1.StepMutate{
					PatchType: "JSONPatch",
					Target:    ottoflowv1alpha1.StepMutateTarget{APIVersion: "v1", Resource: "Pod", Name: "p"},
					JSONPatch: &ottoflowv1alpha1.MutateJSONPatch{Expression: `steps.other.ref`},
				},
			},
			{Name: "other", Outputs: []ottoflowv1alpha1.Output{{Name: "ref", Expression: `"x"`}}},
		},
	}
	err := ValidateStepDependencies(spec)
	if err == nil {
		t.Fatal("expected error when step references steps.other in Mutate.JSONPatch.Expression without dependsOn")
	}
}

func TestWorkflowValidator_ExternalAgentRef_HTTPURLRejected(t *testing.T) {
	v := &WorkflowValidator{Client: nil}
	w := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{
				{
					Name: "callAgent",
					ExternalAgentRef: &ottoflowv1alpha1.StepExternalAgentRef{
						URL:    "http://insecure.example.com",
						Prompt: `"hello"`,
					},
				},
			},
		},
	}
	_, err := v.ValidateCreate(context.Background(), w)
	if err == nil {
		t.Fatal("expected error for http:// URL")
	}
	if !strings.Contains(err.Error(), "HTTPS") {
		t.Errorf("expected HTTPS in error, got: %v", err)
	}
}

func TestWorkflowValidator_ExternalAgentRef_HTTPSURLAccepted(t *testing.T) {
	v := &WorkflowValidator{Client: nil}
	w := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{
				{
					Name: "callAgent",
					ExternalAgentRef: &ottoflowv1alpha1.StepExternalAgentRef{
						URL:      "https://agent.example.com",
						Protocol: "a2a",
						Prompt:   `"hello"`,
					},
				},
			},
		},
	}
	_, err := v.ValidateCreate(context.Background(), w)
	if err != nil {
		t.Errorf("expected no error for valid externalAgentRef, got: %v", err)
	}
}

func TestWorkflowValidator_ExternalAgentRef_UnsupportedProtocolRejected(t *testing.T) {
	v := &WorkflowValidator{Client: nil}
	w := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{
				{
					Name: "callAgent",
					ExternalAgentRef: &ottoflowv1alpha1.StepExternalAgentRef{
						URL:      "https://agent.example.com",
						Protocol: "grpc",
						Prompt:   `"hello"`,
					},
				},
			},
		},
	}
	_, err := v.ValidateCreate(context.Background(), w)
	if err == nil {
		t.Fatal("expected error for unsupported protocol")
	}
	if !strings.Contains(err.Error(), "not supported") {
		t.Errorf("expected 'not supported' in error, got: %v", err)
	}
}

func TestWorkflowRunValidator_CrossNamespaceLLMSecret_Rejected(t *testing.T) {
	v := &WorkflowRunValidator{}
	run := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run", Namespace: "team-a"},
		Spec: ottoflowv1alpha1.WorkflowRunSpec{
			WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf"},
			Execution: &ottoflowv1alpha1.WorkflowRunExecutionSpec{
				LLMCredentialsSecret: &ottoflowv1alpha1.LLMCredentialsSecretRef{
					Name:      "my-creds",
					Namespace: "team-b",
				},
			},
		},
	}
	_, err := v.ValidateCreate(context.Background(), run)
	if err == nil {
		t.Fatal("expected error for cross-namespace llmCredentialsSecret")
	}
	if !strings.Contains(err.Error(), "cross-namespace") {
		t.Errorf("expected 'cross-namespace' in error, got: %v", err)
	}
}

func TestWorkflowRunValidator_SameNamespaceLLMSecret_Allowed(t *testing.T) {
	v := &WorkflowRunValidator{}
	run := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run", Namespace: "team-a"},
		Spec: ottoflowv1alpha1.WorkflowRunSpec{
			WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf"},
			Execution: &ottoflowv1alpha1.WorkflowRunExecutionSpec{
				LLMCredentialsSecret: &ottoflowv1alpha1.LLMCredentialsSecretRef{
					Name:      "my-creds",
					Namespace: "team-a",
				},
			},
		},
	}
	_, err := v.ValidateCreate(context.Background(), run)
	if err != nil {
		t.Errorf("expected no error for same-namespace llmCredentialsSecret, got: %v", err)
	}
}

func TestValidateExternalAgentRef_AllowInsecureHTTP(t *testing.T) {
	cases := []struct {
		name      string
		ref       *ottoflowv1alpha1.StepExternalAgentRef
		wantErr   bool
		errSubstr string
	}{
		{
			name:    "http to .svc host with flag is allowed",
			ref:     &ottoflowv1alpha1.StepExternalAgentRef{URL: "http://kagent-controller.kagent.svc:8083", AllowInsecureHTTP: true, Prompt: `"hi"`},
			wantErr: false,
		},
		{
			name:    "http to localhost with flag is allowed",
			ref:     &ottoflowv1alpha1.StepExternalAgentRef{URL: "http://localhost:8080", AllowInsecureHTTP: true, Prompt: `"hi"`},
			wantErr: false,
		},
		{
			name:      "http to external host with flag is rejected",
			ref:       &ottoflowv1alpha1.StepExternalAgentRef{URL: "http://evil.example.com", AllowInsecureHTTP: true, Prompt: `"hi"`},
			wantErr:   true,
			errSubstr: "cluster-local",
		},
		{
			name:      "http without flag is rejected (HTTPS required)",
			ref:       &ottoflowv1alpha1.StepExternalAgentRef{URL: "http://kagent-controller.kagent.svc:8083", Prompt: `"hi"`},
			wantErr:   true,
			errSubstr: "HTTPS",
		},
		{
			name:    "https is always allowed",
			ref:     &ottoflowv1alpha1.StepExternalAgentRef{URL: "https://agent.example.com", Prompt: `"hi"`},
			wantErr: false,
		},
		{
			name: "http to .svc with flag AND auth.secretRef is rejected",
			ref: &ottoflowv1alpha1.StepExternalAgentRef{
				URL: "http://kagent-controller.kagent.svc:8083", AllowInsecureHTTP: true, Prompt: `"hi"`,
				Auth: &ottoflowv1alpha1.ExternalAgentAuth{SecretRef: &ottoflowv1alpha1.SecretReference{Name: "tok", Key: "token"}},
			},
			wantErr:   true,
			errSubstr: "cleartext",
		},
		{
			name: "http to .svc with flag AND caSecretRef is rejected",
			ref: &ottoflowv1alpha1.StepExternalAgentRef{
				URL: "http://kagent-controller.kagent.svc:8083", AllowInsecureHTTP: true, Prompt: `"hi"`,
				CASecretRef: &ottoflowv1alpha1.NamespacedSecretRef{Name: "ca"},
			},
			wantErr:   true,
			errSubstr: "caSecretRef",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateExternalAgentRef("step", tc.ref)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if tc.errSubstr != "" && !strings.Contains(err.Error(), tc.errSubstr) {
					t.Errorf("error %q does not contain %q", err.Error(), tc.errSubstr)
				}
				return
			}
			if err != nil {
				t.Errorf("expected no error, got: %v", err)
			}
		})
	}
}

func stepStatusWorkflow(dependsOn []string) *ottoflowv1alpha1.Workflow {
	return &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{
				{Name: "first", Outputs: []ottoflowv1alpha1.Output{{Name: "x", Expression: `"ok"`}}},
				{
					Name:      "second",
					DependsOn: dependsOn,
					Outputs:   []ottoflowv1alpha1.Output{{Name: "out", Expression: `stepStatus.first.failed`}},
				},
			},
		},
	}
}

func TestValidateStepDependencies_StepStatusRefWithoutDependsOn(t *testing.T) {
	err := ValidateStepDependencies(&stepStatusWorkflow(nil).Spec)
	if err == nil || !strings.Contains(err.Error(), `does not list "first" in dependsOn`) {
		t.Fatalf("expected missing dependsOn error, got %v", err)
	}
}

func TestValidateStepDependencies_StepStatusRefWithDependsOn(t *testing.T) {
	if err := ValidateStepDependencies(&stepStatusWorkflow([]string{"first"}).Spec); err != nil {
		t.Errorf("expected no error when dependsOn is set: %v", err)
	}
}
