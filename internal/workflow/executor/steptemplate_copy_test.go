package executor

import (
	"reflect"
	"testing"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

func TestInstantiateStepTemplateKeepsResourceQueryFields(t *testing.T) {
	src := &ottoflowv1alpha1.StepResourceQuery{
		APIVersion:    "v1",
		Resource:      "pods",
		Namespace:     "{{.ns}}",
		Name:          "{{.name}}",
		LabelSelector: map[string]string{"app": "{{.app}}"},
		FieldSelector: "{{.field}}",
		Outputs:       map[string]string{"out": "{{.out}}"},
		Limit:         10,
		PageSize:      5,
	}
	tmpl := ottoflowv1alpha1.StepTemplateStep{ResourceQuery: src}
	args := map[string]interface{}{"ns": "n1", "name": "p1", "app": "web", "field": "f1", "out": "o1"}

	got, err := (&WorkflowExecutor{}).instantiateStepTemplate(tmpl, "s", args)
	if err != nil {
		t.Fatal(err)
	}

	want := src.DeepCopy()
	want.Namespace, want.Name, want.FieldSelector = "n1", "p1", "f1"
	want.LabelSelector = map[string]string{"app": "web"}
	want.Outputs = map[string]string{"out": "o1"}
	if !reflect.DeepEqual(got.ResourceQuery, want) {
		t.Fatalf("got %+v, want %+v", got.ResourceQuery, want)
	}
	if src.LabelSelector["app"] != "{{.app}}" || src.Outputs["out"] != "{{.out}}" {
		t.Fatalf("template maps were mutated: %+v", src)
	}
}

func TestInstantiateStepTemplateKeepsAgentRefFields(t *testing.T) {
	tokens, lastN := int32(100), int32(3)
	src := &ottoflowv1alpha1.StepAgentRef{
		Name:                      "agent",
		Namespace:                 "ns",
		AdditionalPrompts:         []string{"hello {{.who}}"},
		MaxAdditionalPromptTokens: &tokens,
		ContextBudgetMode:         "lastN",
		ContextBudgetLastN:        &lastN,
	}
	tmpl := ottoflowv1alpha1.StepTemplateStep{AgentRef: src}

	got, err := (&WorkflowExecutor{}).instantiateStepTemplate(tmpl, "s", map[string]interface{}{"who": "bob"})
	if err != nil {
		t.Fatal(err)
	}

	want := src.DeepCopy()
	want.AdditionalPrompts = []string{"hello bob"}
	if !reflect.DeepEqual(got.AgentRef, want) {
		t.Fatalf("got %+v, want %+v", got.AgentRef, want)
	}
	if got.AgentRef.MaxAdditionalPromptTokens == src.MaxAdditionalPromptTokens ||
		got.AgentRef.ContextBudgetLastN == src.ContextBudgetLastN {
		t.Fatal("pointer fields are shared with the template, want deep copies")
	}
}
