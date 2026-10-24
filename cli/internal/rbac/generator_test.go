/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package rbac

import (
	"bytes"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

// --- helpers -----------------------------------------------------------------

func mustNew(t *testing.T, opts Options) *Generator {
	t.Helper()
	g, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return g
}

func simpleWorkflow(name string, steps ...ottoflowv1alpha1.Step) *ottoflowv1alpha1.Workflow {
	return &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       ottoflowv1alpha1.WorkflowSpec{Steps: steps},
	}
}

func generate(t *testing.T, g *Generator, wf *ottoflowv1alpha1.Workflow) string {
	t.Helper()
	out, _, err := g.GenerateForWorkflow(wf)
	if err != nil {
		t.Fatalf("GenerateForWorkflow: %v", err)
	}
	return string(out)
}

func generateWithWarnings(t *testing.T, g *Generator, wf *ottoflowv1alpha1.Workflow) (string, []string) {
	t.Helper()
	out, warnings, err := g.GenerateForWorkflow(wf)
	if err != nil {
		t.Fatalf("GenerateForWorkflow: %v", err)
	}
	return string(out), warnings
}

func assertContains(t *testing.T, haystack, needle, label string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("%s: expected output to contain %q\n--- output ---\n%s", label, needle, haystack)
	}
}

func assertNotContains(t *testing.T, haystack, needle, label string) {
	t.Helper()
	if strings.Contains(haystack, needle) {
		t.Errorf("%s: expected output NOT to contain %q\n--- output ---\n%s", label, needle, haystack)
	}
}

// --- SA naming ---------------------------------------------------------------

func TestSA_DefaultConvention(t *testing.T) {
	g := mustNew(t, Options{Namespace: "ottoflow"})
	wf := simpleWorkflow("my-workflow")
	out := generate(t, g, wf)
	assertContains(t, out, "name: my-workflow-runner", "default SA name")
}

// --- resourceQuery -----------------------------------------------------------

func TestResourceQuery_NamespaceScopedRole(t *testing.T) {
	g := mustNew(t, Options{Namespace: "ottoflow"})
	wf := simpleWorkflow("wf",
		ottoflowv1alpha1.Step{
			Name: "list-pods",
			ResourceQuery: &ottoflowv1alpha1.StepResourceQuery{
				APIVersion: "v1",
				Resource:   "Pod",
				Namespace:  "default",
				Outputs:    map[string]string{"pods": "items"},
			},
		},
	)
	out := generate(t, g, wf)
	assertContains(t, out, "kind: Role", "namespaced → Role")
	assertNotContains(t, out, "kind: ClusterRole\n", "no ClusterRole for namespaced query")
	assertContains(t, out, "- pods", "resource name")
	assertContains(t, out, "- get", "get verb")
	assertContains(t, out, "- list", "list verb")
	assertContains(t, out, "namespace: default", "Role in correct namespace")
}

func TestResourceQuery_ClusterScopedWhenNoNamespace(t *testing.T) {
	g := mustNew(t, Options{Namespace: "ottoflow"})
	wf := simpleWorkflow("wf",
		ottoflowv1alpha1.Step{
			Name: "list-nodes",
			ResourceQuery: &ottoflowv1alpha1.StepResourceQuery{
				APIVersion: "v1",
				Resource:   "Node",
				Outputs:    map[string]string{"nodes": "items"},
			},
		},
	)
	out := generate(t, g, wf)
	assertContains(t, out, "kind: ClusterRole", "empty namespace → ClusterRole")
	assertContains(t, out, "kind: ClusterRoleBinding", "ClusterRoleBinding emitted")
	assertContains(t, out, "- nodes", "resource name")
}

// --- mutate ------------------------------------------------------------------

func TestMutate_CorrectVerbs(t *testing.T) {
	g := mustNew(t, Options{Namespace: "ottoflow"})
	wf := simpleWorkflow("wf",
		ottoflowv1alpha1.Step{
			Name: "patch-pod",
			Mutate: &ottoflowv1alpha1.StepMutate{
				Target: ottoflowv1alpha1.StepMutateTarget{
					APIVersion: "v1",
					Resource:   "Pod",
					Namespace:  "default",
					Name:       "mypod",
				},
				PatchType:          "JSONPatch",
				ApplyConfiguration: nil,
			},
		},
	)
	out := generate(t, g, wf)
	assertContains(t, out, "kind: Role", "namespaced mutate → Role")
	assertContains(t, out, "- get", "get verb")
	assertContains(t, out, "- patch", "patch verb")
	assertContains(t, out, "- update", "update verb")
}

// --- agentRef ----------------------------------------------------------------

func TestAgentRef_CallerRule(t *testing.T) {
	g := mustNew(t, Options{Namespace: "ottoflow"})
	wf := simpleWorkflow("wf",
		ottoflowv1alpha1.Step{
			Name:     "run-agent",
			AgentRef: &ottoflowv1alpha1.StepAgentRef{Name: "my-agent"},
		},
	)
	out := generate(t, g, wf)
	assertContains(t, out, "gate.ottoflow.nirmata.io", "caller API group in rules")
	assertContains(t, out, "callers", "callers resource in rules")
	assertContains(t, out, "agent-executor-caller", "resourceNames restriction")
	assertContains(t, out, "- get", "get verb")
}

// --- mcpToolCall -------------------------------------------------------------

func TestMCPToolCall_NoExtraRules(t *testing.T) {
	g := mustNew(t, Options{Namespace: "ottoflow"})
	wf := simpleWorkflow("wf",
		ottoflowv1alpha1.Step{
			Name:        "call-tool",
			MCPToolCall: &ottoflowv1alpha1.StepMCPToolCall{Server: "my-server", Tool: "mytool"},
		},
	)
	out := generate(t, g, wf)
	// mcpToolCall RBAC is covered by the default OttoFlow ClusterRole (Secrets); no TODO needed
	assertNotContains(t, out, "TODO", "no TODO for mcpToolCall")
	assertNotContains(t, out, "kind: Role", "no Role generated for mcpToolCall")
}

// --- stepTemplateRef ---------------------------------------------------------

func TestStepTemplateRef_NoExtraRules(t *testing.T) {
	g := mustNew(t, Options{Namespace: "ottoflow"})
	wf := simpleWorkflow("wf",
		ottoflowv1alpha1.Step{
			Name:            "use-template",
			StepTemplateRef: &ottoflowv1alpha1.StepTemplateRef{Name: "my-template"},
		},
	)
	out := generate(t, g, wf)
	// stepTemplateRef read-only RBAC is covered by the default OttoFlow view ClusterRole; no TODO needed
	assertNotContains(t, out, "TODO", "no TODO for stepTemplateRef")
	assertNotContains(t, out, "kind: Role", "no Role generated for stepTemplateRef")
}

// --- forEach -----------------------------------------------------------------

func TestForEach_InlineStep_RulesCollected(t *testing.T) {
	g := mustNew(t, Options{Namespace: "ottoflow"})
	wf := simpleWorkflow("wf",
		ottoflowv1alpha1.Step{
			Name: "iterate",
			ForEach: &ottoflowv1alpha1.StepForEach{
				Items: "inputs.namespaces",
				Step: &ottoflowv1alpha1.StepForEachStep{
					ResourceQuery: &ottoflowv1alpha1.StepResourceQuery{
						APIVersion: "apps/v1",
						Resource:   "Deployment",
						Namespace:  "staging",
						Outputs:    map[string]string{"deps": "items"},
					},
				},
			},
		},
	)
	out := generate(t, g, wf)
	assertContains(t, out, "deployments", "deployment resource from forEach inner step")
	assertContains(t, out, "apps", "apps API group")
	assertContains(t, out, "namespace: staging", "correct namespace from forEach step")
}

// --- multi-namespace ---------------------------------------------------------

func TestMultiNamespace_SeparateRoles(t *testing.T) {
	g := mustNew(t, Options{Namespace: "ottoflow"})
	wf := simpleWorkflow("wf",
		ottoflowv1alpha1.Step{
			Name: "query-monitoring",
			ResourceQuery: &ottoflowv1alpha1.StepResourceQuery{
				APIVersion: "v1",
				Resource:   "Pod",
				Namespace:  "monitoring",
				Outputs:    map[string]string{"pods": "items"},
			},
		},
		ottoflowv1alpha1.Step{
			Name: "mutate-default",
			Mutate: &ottoflowv1alpha1.StepMutate{
				Target: ottoflowv1alpha1.StepMutateTarget{
					APIVersion: "v1",
					Resource:   "ConfigMap",
					Namespace:  "default",
					Name:       "mycm",
				},
				PatchType: "JSONPatch",
			},
		},
	)
	out := generate(t, g, wf)
	if strings.Count(out, "kind: Role\n") < 2 {
		t.Errorf("expected at least 2 Role objects for 2 namespaces\n--- output ---\n%s", out)
	}
	assertContains(t, out, "namespace: monitoring", "monitoring namespace Role")
	assertContains(t, out, "namespace: default", "default namespace Role")
}

func TestResourceQuery_CELNamespace_GeneratesClusterRoleWithWarning(t *testing.T) {
	g := mustNew(t, Options{Namespace: "ottoflow"})
	wf := simpleWorkflow("wf", ottoflowv1alpha1.Step{
		Name: "fetch",
		ResourceQuery: &ottoflowv1alpha1.StepResourceQuery{
			APIVersion: "v1",
			Resource:   "Pod",
			Namespace:  "inputs.targetNamespace",
		},
	})
	out, warnings := generateWithWarnings(t, g, wf)
	assertContains(t, out, "kind: ClusterRole", "ClusterRole generated for dynamic namespace")
	assertNotContains(t, out, "kind: Role\n", "no namespaced Role for dynamic namespace")
	if len(warnings) == 0 {
		t.Fatal("expected warning for dynamic namespace, got none")
	}
	if !strings.Contains(warnings[0], "inputs.targetNamespace") {
		t.Errorf("warning should mention the offending expression, got: %v", warnings[0])
	}
}

func TestMutate_CELNamespace_GeneratesClusterRoleWithWarning(t *testing.T) {
	g := mustNew(t, Options{Namespace: "ottoflow"})
	wf := simpleWorkflow("wf", ottoflowv1alpha1.Step{
		Name: "patch",
		Mutate: &ottoflowv1alpha1.StepMutate{
			Target: ottoflowv1alpha1.StepMutateTarget{
				APIVersion: "apps/v1",
				Resource:   "Deployment",
				Namespace:  "inputs.targetNS",
				Name:       "my-deploy",
			},
			PatchType: "JSONPatch",
		},
	})
	out, warnings := generateWithWarnings(t, g, wf)
	assertContains(t, out, "kind: ClusterRole", "ClusterRole generated for dynamic namespace")
	if len(warnings) == 0 {
		t.Fatal("expected warning for dynamic namespace, got none")
	}
	if !strings.Contains(warnings[0], "inputs.targetNS") {
		t.Errorf("warning should mention the offending expression, got: %v", warnings[0])
	}
}

func TestForEach_CELNamespace_GeneratesClusterRoleWithWarning(t *testing.T) {
	g := mustNew(t, Options{Namespace: "ottoflow"})
	wf := simpleWorkflow("wf", ottoflowv1alpha1.Step{
		Name: "loop",
		ForEach: &ottoflowv1alpha1.StepForEach{
			Items: `["a","b"]`,
			Step: &ottoflowv1alpha1.StepForEachStep{
				ResourceQuery: &ottoflowv1alpha1.StepResourceQuery{
					APIVersion: "v1",
					Resource:   "Pod",
					Namespace:  "inputs.dynamicNS",
				},
			},
		},
	})
	out, warnings := generateWithWarnings(t, g, wf)
	assertContains(t, out, "kind: ClusterRole", "ClusterRole generated for dynamic namespace in forEach")
	if len(warnings) == 0 {
		t.Fatal("expected warning for dynamic namespace in forEach, got none")
	}
	if !strings.Contains(warnings[0], "loop") {
		t.Errorf("warning should mention the step name, got: %v", warnings[0])
	}
	if !strings.Contains(warnings[0], "inputs.dynamicNS") {
		t.Errorf("warning should mention the offending expression, got: %v", warnings[0])
	}
}

func TestCELStringLiteralNamespace_TreatedAsLiteral(t *testing.T) {
	g := mustNew(t, Options{Namespace: "ottoflow"})
	wf := simpleWorkflow("wf",
		ottoflowv1alpha1.Step{
			Name: "query-quoted-ns",
			ResourceQuery: &ottoflowv1alpha1.StepResourceQuery{
				APIVersion: "v1",
				Resource:   "Pod",
				Namespace:  `"staging"`,
				Outputs:    map[string]string{"pods": "items"},
			},
		},
	)
	out := generate(t, g, wf)
	assertContains(t, out, "namespace: staging", "quoted CEL string literal resolved to literal namespace")
	assertNotContains(t, out, "TODO", "no TODO for a resolvable CEL string literal")
}

// --- verb merging ------------------------------------------------------------

func TestVerbMerging_NoDuplicates(t *testing.T) {
	g := mustNew(t, Options{Namespace: "ottoflow"})
	wf := simpleWorkflow("wf",
		ottoflowv1alpha1.Step{
			Name: "query-1",
			ResourceQuery: &ottoflowv1alpha1.StepResourceQuery{
				APIVersion: "v1",
				Resource:   "Pod",
				Namespace:  "default",
				Outputs:    map[string]string{"pods": "items"},
			},
		},
		ottoflowv1alpha1.Step{
			Name: "query-2",
			ResourceQuery: &ottoflowv1alpha1.StepResourceQuery{
				APIVersion: "v1",
				Resource:   "Pod",
				Namespace:  "default",
				Outputs:    map[string]string{"pods2": "items"},
			},
		},
		ottoflowv1alpha1.Step{
			Name: "mutate-pod",
			Mutate: &ottoflowv1alpha1.StepMutate{
				Target: ottoflowv1alpha1.StepMutateTarget{
					APIVersion: "v1",
					Resource:   "Pod",
					Namespace:  "default",
					Name:       "mypod",
				},
				PatchType: "JSONPatch",
			},
		},
	)
	out := generate(t, g, wf)
	if strings.Count(out, "- pods") != 1 {
		t.Errorf("expected 1 rule block for pods, got %d\n--- output ---\n%s", strings.Count(out, "- pods"), out)
	}
	assertContains(t, out, "- get", "get verb present")
	assertContains(t, out, "- list", "list verb present")
	assertContains(t, out, "- patch", "patch verb present")
	assertContains(t, out, "- update", "update verb present")
}

// --- workflowRef -------------------------------------------------------------

func TestWorkflowRef_TopLevel_NoExtraRules(t *testing.T) {
	g := mustNew(t, Options{Namespace: "ottoflow"})
	wf := simpleWorkflow("wf",
		ottoflowv1alpha1.Step{
			Name:        "call-sub",
			WorkflowRef: &ottoflowv1alpha1.StepWorkflowRef{Name: "child-workflow"},
		},
	)
	out := generate(t, g, wf)
	// workflowRef RBAC (WorkflowRun create) is covered by the default OttoFlow core ClusterRole; no TODO needed
	assertNotContains(t, out, "TODO", "no TODO for workflowRef")
	assertNotContains(t, out, "kind: Role", "no Role generated for workflowRef")
}

func TestWorkflowRef_ForEach_NoExtraRules(t *testing.T) {
	g := mustNew(t, Options{Namespace: "ottoflow"})
	wf := simpleWorkflow("wf",
		ottoflowv1alpha1.Step{
			Name: "iterate-and-call",
			ForEach: &ottoflowv1alpha1.StepForEach{
				Items: "inputs.items",
				Step: &ottoflowv1alpha1.StepForEachStep{
					WorkflowRef: &ottoflowv1alpha1.StepWorkflowRef{Name: "inner-workflow"},
				},
			},
		},
	)
	out := generate(t, g, wf)
	// workflowRef RBAC (WorkflowRun create) is covered by the default OttoFlow core ClusterRole; no TODO needed
	assertNotContains(t, out, "TODO", "no TODO for forEach workflowRef")
	assertNotContains(t, out, "kind: Role", "no Role generated for forEach workflowRef")
}

// --- pluralization -----------------------------------------------------------

func TestPluralization_Irregulars(t *testing.T) {
	tests := []struct {
		kind     string
		expected string
	}{
		{"Ingress", "ingresses"},
		{"NetworkPolicy", "networkpolicies"},
		{"StorageClass", "storageclasses"},
		{"Pod", "pods"},
		{"Deployment", "deployments"},
		{"ConfigMap", "configmaps"},
		{"Node", "nodes"},
		{"Endpoints", "endpoints"},
	}
	for _, tt := range tests {
		got := pluralizeKind(tt.kind)
		if got != tt.expected {
			t.Errorf("pluralizeKind(%q) = %q, want %q", tt.kind, got, tt.expected)
		}
	}
}

func TestPluralization_AlreadyPluralInput(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"pods", "pods"},
		{"nodes", "nodes"},
		{"deployments", "deployments"},
		{"configmaps", "configmaps"},
		{"networkpolicies", "networkpolicies"},
		{"ingresses", "ingresses"},
	}
	for _, tt := range tests {
		got := pluralizeKind(tt.input)
		if got != tt.expected {
			t.Errorf("pluralizeKind(%q) = %q, want %q (already-plural input should be returned as-is)", tt.input, got, tt.expected)
		}
	}
}

// --- determinism -------------------------------------------------------------

func TestDeterminism_ByteIdenticalOutput(t *testing.T) {
	g := mustNew(t, Options{Namespace: "ottoflow"})
	wf := simpleWorkflow("wf",
		ottoflowv1alpha1.Step{
			Name: "list-pods",
			ResourceQuery: &ottoflowv1alpha1.StepResourceQuery{
				APIVersion: "v1",
				Resource:   "Pod",
				Namespace:  "default",
				Outputs:    map[string]string{"pods": "items"},
			},
		},
		ottoflowv1alpha1.Step{
			Name: "list-nodes",
			ResourceQuery: &ottoflowv1alpha1.StepResourceQuery{
				APIVersion: "v1",
				Resource:   "Node",
				Outputs:    map[string]string{"nodes": "items"},
			},
		},
	)
	out1, _, err := g.GenerateForWorkflow(wf)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	out2, _, err := g.GenerateForWorkflow(wf)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if !bytes.Equal(out1, out2) {
		t.Errorf("output is not deterministic:\n--- call 1 ---\n%s\n--- call 2 ---\n%s", out1, out2)
	}
}
