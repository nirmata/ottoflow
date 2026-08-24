/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	metricsfake "k8s.io/metrics/pkg/client/clientset/versioned/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

func TestLoadFromDirectory_WorkflowRunParsing(t *testing.T) {
	dir := t.TempDir()

	// Multi-document YAML with Workflow + WorkflowRun
	yaml := `apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: my-workflow
  namespace: ottoflow
spec:
  inputs:
    - name: question
      required: true
  steps:
    - name: step1
      expressions:
        - key: answer
          expression: "inputs.question"
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: WorkflowRun
metadata:
  name: my-run
  namespace: ottoflow
spec:
  workflowRef:
    name: my-workflow
  inputValues:
    question: "What is Kubernetes?"
`
	if err := os.WriteFile(filepath.Join(dir, "workflow.yaml"), []byte(yaml), 0600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	exec := NewLocalWorkflowExecutor(nil, "", 5, "", "")
	if err := exec.LoadFromDirectory(dir); err != nil {
		t.Fatalf("LoadFromDirectory: %v", err)
	}

	// WorkflowRun should be stored in workflowRuns map
	if len(exec.workflowRuns) != 1 {
		t.Fatalf("expected 1 WorkflowRun, got %d", len(exec.workflowRuns))
	}
	wr, ok := exec.workflowRuns["ottoflow/my-workflow"]
	if !ok {
		t.Fatalf("expected WorkflowRun keyed by 'ottoflow/my-workflow', keys: %v", slices.Collect(maps.Keys(exec.workflowRuns)))
	}
	if wr.Spec.InputValues["question"] != "What is Kubernetes?" {
		t.Errorf("expected question input, got: %v", wr.Spec.InputValues)
	}

	// Workflow should be loadable from the fake client
	ctx := t.Context()
	wf, err := exec.GetWorkflow(ctx, "my-workflow", "ottoflow")
	if err != nil {
		t.Fatalf("GetWorkflow: %v", err)
	}
	if wf.Name != "my-workflow" {
		t.Errorf("expected workflow name my-workflow, got %s", wf.Name)
	}
}

func TestLoadFromDirectory_WorkflowRunNamespaceFromWorkflowRef(t *testing.T) {
	dir := t.TempDir()

	// WorkflowRun with namespace in workflowRef (not metadata)
	yaml := `apiVersion: ottoflow.nirmata.io/v1alpha1
kind: WorkflowRun
metadata:
  name: my-run
spec:
  workflowRef:
    name: my-workflow
    namespace: custom-ns
  inputValues:
    key: value
`
	if err := os.WriteFile(filepath.Join(dir, "run.yaml"), []byte(yaml), 0600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	exec := NewLocalWorkflowExecutor(nil, "", 5, "", "")
	if err := exec.LoadFromDirectory(dir); err != nil {
		t.Fatalf("LoadFromDirectory: %v", err)
	}

	if _, ok := exec.workflowRuns["custom-ns/my-workflow"]; !ok {
		t.Errorf("expected key 'custom-ns/my-workflow', keys: %v", slices.Collect(maps.Keys(exec.workflowRuns)))
	}
}

func TestLoadFromDirectory_WorkflowRunDefaultNamespace(t *testing.T) {
	dir := t.TempDir()

	// WorkflowRun with no namespace anywhere
	yaml := `apiVersion: ottoflow.nirmata.io/v1alpha1
kind: WorkflowRun
metadata:
  name: my-run
spec:
  workflowRef:
    name: my-workflow
  inputValues:
    key: value
`
	if err := os.WriteFile(filepath.Join(dir, "run.yaml"), []byte(yaml), 0600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	exec := NewLocalWorkflowExecutor(nil, "", 5, "", "")
	if err := exec.LoadFromDirectory(dir); err != nil {
		t.Fatalf("LoadFromDirectory: %v", err)
	}

	if _, ok := exec.workflowRuns["default/my-workflow"]; !ok {
		t.Errorf("expected key 'default/my-workflow', keys: %v", slices.Collect(maps.Keys(exec.workflowRuns)))
	}
}

func TestMergeInputValues_CLIOverridesYAML(t *testing.T) {
	exec := &LocalWorkflowExecutor{
		workflowRuns: map[string]*ottoflowv1alpha1.WorkflowRun{
			"ns/my-wf": {
				Spec: ottoflowv1alpha1.WorkflowRunSpec{
					InputValues: map[string]string{
						"fromYAML":   "yaml-value",
						"overridden": "yaml-original",
					},
				},
			},
		},
	}

	cliInputs := map[string]string{
		"overridden": "cli-wins",
		"cliOnly":    "cli-value",
	}

	merged := exec.mergeInputValues("my-wf", "ns", cliInputs)

	if merged["fromYAML"] != "yaml-value" {
		t.Errorf("expected fromYAML=yaml-value, got %s", merged["fromYAML"])
	}
	if merged["overridden"] != "cli-wins" {
		t.Errorf("expected overridden=cli-wins (CLI wins), got %s", merged["overridden"])
	}
	if merged["cliOnly"] != "cli-value" {
		t.Errorf("expected cliOnly=cli-value, got %s", merged["cliOnly"])
	}
}

func TestMergeInputValues_NoWorkflowRunMatch(t *testing.T) {
	exec := &LocalWorkflowExecutor{
		workflowRuns: map[string]*ottoflowv1alpha1.WorkflowRun{
			"ns/other-wf": {
				Spec: ottoflowv1alpha1.WorkflowRunSpec{
					InputValues: map[string]string{"key": "should-not-appear"},
				},
			},
		},
	}

	cliInputs := map[string]string{"cli": "value"}
	merged := exec.mergeInputValues("my-wf", "ns", cliInputs)

	if len(merged) != 1 || merged["cli"] != "value" {
		t.Errorf("expected only CLI inputs when no match, got: %v", merged)
	}
}

func TestMergeInputValues_EmptyWorkflowRuns(t *testing.T) {
	exec := &LocalWorkflowExecutor{}

	cliInputs := map[string]string{"cli": "value"}
	merged := exec.mergeInputValues("my-wf", "ns", cliInputs)

	if merged["cli"] != "value" {
		t.Errorf("expected CLI inputs passthrough, got: %v", merged)
	}
}

func TestMergeInputValues_YAMLOnlyNoCliInputs(t *testing.T) {
	exec := &LocalWorkflowExecutor{
		workflowRuns: map[string]*ottoflowv1alpha1.WorkflowRun{
			"ns/my-wf": {
				Spec: ottoflowv1alpha1.WorkflowRunSpec{
					InputValues: map[string]string{"fromYAML": "value"},
				},
			},
		},
	}

	merged := exec.mergeInputValues("my-wf", "ns", map[string]string{})

	if merged["fromYAML"] != "value" {
		t.Errorf("expected fromYAML=value, got: %v", merged)
	}
}

func TestProviderOverrideValidation(t *testing.T) {
	dir := t.TempDir()

	// Workflow with agent step referencing an agent with an invalid provider
	yaml := `apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: wf
  namespace: default
spec:
  steps:
    - name: step1
      agentRef:
        name: my-agent
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Agent
metadata:
  name: my-agent
  namespace: default
spec:
  prompt: test
  modelProvider: invalid-provider
`
	if err := os.WriteFile(filepath.Join(dir, "wf.yaml"), []byte(yaml), 0600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	// With valid provider override, agent's invalid provider should be skipped
	exec := NewLocalWorkflowExecutor(nil, "", 5, "openai", "")
	if err := exec.LoadFromDirectory(dir); err != nil {
		t.Fatalf("LoadFromDirectory: %v", err)
	}

	// ExecuteWorkflow will fail later (no real LLM), but should NOT fail at provider validation
	ctx := t.Context()
	_, err := exec.ExecuteWorkflow(ctx, "wf", "default", nil)
	if err == nil {
		t.Fatal("expected error (no LLM client), but not a provider validation error")
	}
	// The error should NOT be about invalid provider (override is valid)
	if strings.Contains(err.Error(), "unknown provider") || strings.Contains(err.Error(), "not valid") {
		t.Errorf("expected no provider validation error with override, got: %v", err)
	}
}

func TestProviderOverrideValidation_MissingModelProviderFailsFast(t *testing.T) {
	dir := t.TempDir()

	// Workflow with agent step referencing an agent that omits modelProvider.
	yaml := `apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: wf
  namespace: default
spec:
  steps:
    - name: step1
      agentRef:
        name: my-agent
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Agent
metadata:
  name: my-agent
  namespace: default
spec:
  prompt: test
`
	if err := os.WriteFile(filepath.Join(dir, "wf.yaml"), []byte(yaml), 0600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	// No provider override, so the fail-fast check must reject the missing modelProvider.
	exec := NewLocalWorkflowExecutor(nil, "", 5, "", "")
	if err := exec.LoadFromDirectory(dir); err != nil {
		t.Fatalf("LoadFromDirectory: %v", err)
	}

	ctx := t.Context()
	_, err := exec.ExecuteWorkflow(ctx, "wf", "default", nil)
	if err == nil {
		t.Fatal("expected error for missing modelProvider, got nil")
	}
	if !strings.Contains(err.Error(), "spec.modelProvider is required") {
		t.Errorf("expected required-provider error, got: %v", err)
	}
}

func TestLoadFromDirectory_SecretLoadedIntoFakeClient(t *testing.T) {
	dir := t.TempDir()

	yaml := `apiVersion: v1
kind: Secret
metadata:
  name: agent-token
  namespace: default
type: Opaque
data:
  token: bXktYmVhcmVyLXRva2Vu
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: wf
  namespace: default
spec:
  steps:
    - name: step1
      expressions:
        - name: x
          expression: '"hello"'
`
	if err := os.WriteFile(filepath.Join(dir, "manifests.yaml"), []byte(yaml), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}

	exec := NewLocalWorkflowExecutor(nil, "", 5, "", "")
	if err := exec.LoadFromDirectory(dir); err != nil {
		t.Fatalf("LoadFromDirectory: %v", err)
	}

	// Secret must be retrievable from the fake controlClient so externalAgentRef auth works
	var secret corev1.Secret
	err := exec.controlClient.Get(context.Background(), types.NamespacedName{Name: "agent-token", Namespace: "default"}, &secret)
	if err != nil {
		t.Fatalf("Secret not found in fake client: %v", err)
	}
	if string(secret.Data["token"]) != "my-bearer-token" {
		t.Errorf("expected token 'my-bearer-token', got %q", string(secret.Data["token"]))
	}
}

// A document declaring `kind: Workflow` whose body does not parse used to be dropped
// silently: decodeObject probed each type and returned nil on no match, so the broken
// workflow simply never entered the loaded set and `validate --workflow-dir` omitted it
// from the results rather than failing.
func TestLoadFromDirectory_MistypedWorkflowBodyIsAnError(t *testing.T) {
	dir := t.TempDir()

	broken := `apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: broken
spec:
  steps: "this-should-be-a-list"
`
	if err := os.WriteFile(filepath.Join(dir, "broken.yaml"), []byte(broken), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}

	exec := NewLocalWorkflowExecutor(nil, "", 5, "", "")
	err := exec.LoadFromDirectory(dir)
	if err == nil {
		t.Fatal("expected LoadFromDirectory to fail on a Workflow with a mistyped body")
	}
	if !strings.Contains(err.Error(), "parse Workflow") {
		t.Errorf("error should name the failed Workflow parse, got: %v", err)
	}
}

// Kinds local mode does not load (a plain ConfigMap alongside the workflows that use it)
// must still be ignored rather than erroring -- that is what the stricter decode above
// must not regress.
func TestLoadFromDirectory_UnknownKindIsIgnored(t *testing.T) {
	dir := t.TempDir()

	manifest := `apiVersion: v1
kind: ConfigMap
metadata:
  name: state
  namespace: default
data:
  key: value
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: wf
  namespace: default
spec:
  steps:
    - name: step1
      expressions:
        - name: x
          expression: '"hello"'
`
	if err := os.WriteFile(filepath.Join(dir, "manifests.yaml"), []byte(manifest), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}

	exec := NewLocalWorkflowExecutor(nil, "", 5, "", "")
	if err := exec.LoadFromDirectory(dir); err != nil {
		t.Fatalf("LoadFromDirectory should ignore unknown kinds, got: %v", err)
	}
	workflows, err := exec.ListWorkflows(context.Background())
	if err != nil {
		t.Fatalf("ListWorkflows: %v", err)
	}
	if len(workflows) != 1 || workflows[0].Name != "wf" {
		t.Errorf("expected the sibling Workflow to load, got %d workflows", len(workflows))
	}
}

// resourceMetrics() needs a metrics.k8s.io client. Local mode used to pass nil, so the
// function failed with "metrics client not available" against even a healthy
// metrics-server. This guards the wiring: with a client set, the call reaches the metrics
// API (a missing pod is then a normal not-found, not a missing-client error).
func TestLocalExecutorUsesMetricsClient(t *testing.T) {
	dir := t.TempDir()
	manifest := `apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: metrics-workflow
  namespace: ottoflow
spec:
  steps:
    - name: readMetrics
      expressions:
        - name: m
          expression: 'resourceMetrics("v1", "Pod", "default", "my-pod", "")'
`
	if err := os.WriteFile(filepath.Join(dir, "workflow.yaml"), []byte(manifest), 0600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	run := func(withClient bool) error {
		exec := NewLocalWorkflowExecutor(fake.NewClientBuilder().Build(), "", 5, "", "")
		if withClient {
			exec.SetMetricsClient(metricsfake.NewSimpleClientset()) //nolint:staticcheck
		}
		if err := exec.LoadFromDirectory(dir); err != nil {
			t.Fatalf("LoadFromDirectory: %v", err)
		}
		_, err := exec.ExecuteWorkflow(context.Background(), "metrics-workflow", "ottoflow", nil)
		return err
	}

	// Without a client the failure is that no client exists at all.
	err := run(false)
	if err == nil || !strings.Contains(err.Error(), "metrics client not available") {
		t.Fatalf("without client: got %v, want a 'metrics client not available' error", err)
	}

	// With one, the call reaches the metrics API and fails on the absent pod instead.
	err = run(true)
	if err == nil {
		t.Fatal("with client: expected a not-found error for the absent pod")
	}
	if strings.Contains(err.Error(), "metrics client not available") {
		t.Errorf("with client: metrics client still not wired through: %v", err)
	}
	if !strings.Contains(err.Error(), "my-pod") {
		t.Errorf("with client: expected a not-found error naming the pod, got: %v", err)
	}
}

// TestListWorkflowRuns_SortedDeterministically is a regression test: ListWorkflowRuns used to
// iterate e.workflowRuns (a Go map) directly, so the order of e.g. `validate --workflow-dir`'s
// "FAIL workflowRun ..." lines was nondeterministic across runs. It must return entries sorted
// by (ResolvedNamespace, Run.Name) regardless of map iteration order.
func TestListWorkflowRuns_SortedDeterministically(t *testing.T) {
	dir := t.TempDir()

	// Three WorkflowRuns whose index keys ("namespace/workflowRef.name") are all distinct, so
	// no two collide and all three are retained. Metadata.name intentionally out of order.
	yaml := `apiVersion: ottoflow.nirmata.io/v1alpha1
kind: WorkflowRun
metadata:
  name: run-z
spec:
  workflowRef:
    name: wf-z
    namespace: zeta
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: WorkflowRun
metadata:
  name: run-b
spec:
  workflowRef:
    name: wf-b
    namespace: alpha
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: WorkflowRun
metadata:
  name: run-a
spec:
  workflowRef:
    name: wf-a
    namespace: alpha
`
	if err := os.WriteFile(filepath.Join(dir, "runs.yaml"), []byte(yaml), 0600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	exec := NewLocalWorkflowExecutor(nil, "", 5, "", "")
	if err := exec.LoadFromDirectory(dir); err != nil {
		t.Fatalf("LoadFromDirectory: %v", err)
	}

	// Run the listing several times: a map-iteration-order bug wouldn't necessarily surface
	// on the first call.
	for i := 0; i < 5; i++ {
		lrs := exec.ListWorkflowRuns()
		if len(lrs) != 3 {
			t.Fatalf("expected 3 WorkflowRuns, got %d", len(lrs))
		}
		wantOrder := []struct {
			ns, name string
		}{
			{"alpha", "run-a"},
			{"alpha", "run-b"},
			{"zeta", "run-z"},
		}
		for i, want := range wantOrder {
			if lrs[i].ResolvedNamespace != want.ns || lrs[i].Run.Name != want.name {
				t.Fatalf("entry %d: expected (%s, %s), got (%s, %s)",
					i, want.ns, want.name, lrs[i].ResolvedNamespace, lrs[i].Run.Name)
			}
		}
	}
}
