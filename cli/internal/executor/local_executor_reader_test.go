/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// LoadFromReader is the entry point for `ottoflow run -f -/<url>/<path>`: it loads a single
// manifest stream instead of walking a directory. These tests exercise it directly, plus the
// ResolveWorkflow helper that picks which loaded Workflow to run.

func TestLoadFromReader_ResolveWorkflow_NamespaceLessDefaultsToDefault(t *testing.T) {
	manifest := `apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: hello
spec:
  steps:
    - name: step1
      expressions:
        - name: x
          expression: '"hello"'
`
	exec := NewLocalWorkflowExecutor(nil, "", 5, "", "")
	if err := exec.LoadFromReader(strings.NewReader(manifest)); err != nil {
		t.Fatalf("LoadFromReader: %v", err)
	}

	name, ns, err := exec.ResolveWorkflow(context.Background(), "", "")
	if err != nil {
		t.Fatalf("ResolveWorkflow: %v", err)
	}
	if name != "hello" || ns != "default" {
		t.Errorf("expected hello/default, got %s/%s", name, ns)
	}
}

func TestLoadFromReader_MergesWorkflowRunInputs_NamespaceLess(t *testing.T) {
	manifest := `apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: echo
spec:
  outputs:
    - name: echoed
      expression: inputs.msg
  steps:
    - name: step1
      expressions:
        - name: noop
          expression: '"ok"'
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: WorkflowRun
metadata:
  name: echo-run
spec:
  workflowRef:
    name: echo
  inputValues:
    msg: "from-yaml"
`
	exec := NewLocalWorkflowExecutor(fake.NewClientBuilder().Build(), "", 5, "", "")
	if err := exec.LoadFromReader(strings.NewReader(manifest)); err != nil {
		t.Fatalf("LoadFromReader: %v", err)
	}

	name, ns, err := exec.ResolveWorkflow(context.Background(), "", "")
	if err != nil {
		t.Fatalf("ResolveWorkflow: %v", err)
	}
	if ns != "default" {
		t.Fatalf("expected namespace-less Workflow+WorkflowRun to default to \"default\", got %q", ns)
	}

	run, err := exec.ExecuteWorkflow(context.Background(), name, ns, nil)
	if err != nil {
		t.Fatalf("ExecuteWorkflow: %v", err)
	}
	raw, ok := run.Status.Outputs["echoed"]
	if !ok {
		t.Fatalf("expected 'echoed' workflow output, got: %v", run.Status.Outputs)
	}
	if !strings.Contains(string(raw.Raw), "from-yaml") {
		t.Errorf("expected the WorkflowRun's inputValues to reach execution, got output: %s", raw.Raw)
	}
}

func TestLoadFromReader_MergesWorkflowRunInputs_ExplicitNamespace(t *testing.T) {
	manifest := `apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: echo
  namespace: custom-ns
spec:
  outputs:
    - name: echoed
      expression: inputs.msg
  steps:
    - name: step1
      expressions:
        - name: noop
          expression: '"ok"'
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: WorkflowRun
metadata:
  name: echo-run
  namespace: custom-ns
spec:
  workflowRef:
    name: echo
    namespace: custom-ns
  inputValues:
    msg: "from-yaml-explicit-ns"
`
	exec := NewLocalWorkflowExecutor(fake.NewClientBuilder().Build(), "", 5, "", "")
	if err := exec.LoadFromReader(strings.NewReader(manifest)); err != nil {
		t.Fatalf("LoadFromReader: %v", err)
	}

	name, ns, err := exec.ResolveWorkflow(context.Background(), "", "")
	if err != nil {
		t.Fatalf("ResolveWorkflow: %v", err)
	}
	if ns != "custom-ns" {
		t.Fatalf("expected namespace custom-ns, got %q", ns)
	}

	run, err := exec.ExecuteWorkflow(context.Background(), name, ns, nil)
	if err != nil {
		t.Fatalf("ExecuteWorkflow: %v", err)
	}
	raw, ok := run.Status.Outputs["echoed"]
	if !ok {
		t.Fatalf("expected 'echoed' workflow output, got: %v", run.Status.Outputs)
	}
	if !strings.Contains(string(raw.Raw), "from-yaml-explicit-ns") {
		t.Errorf("expected the WorkflowRun's inputValues to reach execution, got output: %s", raw.Raw)
	}
}

// A bare "---" split also matches a markdown table separator inside a value, truncating the
// document mid-input (see cli/cmd/run_yaml_split_test.go for the file-loading equivalent of
// this regression). LoadFromReader must apply the same "\n---" line-start-only split.
func TestLoadFromReader_KeepsInputsAfterMarkdownTable(t *testing.T) {
	manifest := `apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: hello-world
  namespace: ottoflow
spec:
  steps:
    - name: step1
      expressions:
        - name: x
          expression: '"hello"'
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: WorkflowRun
metadata:
  name: split-regression
  namespace: ottoflow
spec:
  workflowRef:
    name: hello-world
  inputValues:
    report: |
      | col | col |
      |---|---|
    afterTable: "must-survive"
`
	exec := NewLocalWorkflowExecutor(nil, "", 5, "", "")
	if err := exec.LoadFromReader(strings.NewReader(manifest)); err != nil {
		t.Fatalf("LoadFromReader: %v", err)
	}

	wr, ok := exec.workflowRuns["ottoflow/hello-world"]
	if !ok {
		t.Fatalf("expected WorkflowRun keyed by 'ottoflow/hello-world', keys: %v", exec.workflowRuns)
	}
	if got := wr.Spec.InputValues["afterTable"]; got != "must-survive" {
		t.Errorf("input after the markdown table was lost: InputValues = %v", wr.Spec.InputValues)
	}
}

func TestResolveWorkflow_WorkflowRunOnlyNamesTheFix(t *testing.T) {
	manifest := `apiVersion: ottoflow.nirmata.io/v1alpha1
kind: WorkflowRun
metadata:
  name: orphan-run
spec:
  workflowRef:
    name: missing-workflow
  inputValues:
    key: value
`
	exec := NewLocalWorkflowExecutor(nil, "", 5, "", "")
	if err := exec.LoadFromReader(strings.NewReader(manifest)); err != nil {
		t.Fatalf("LoadFromReader: %v", err)
	}

	_, _, err := exec.ResolveWorkflow(context.Background(), "", "")
	if err == nil {
		t.Fatal("expected an error when only a WorkflowRun is loaded")
	}
	if !strings.Contains(err.Error(), "include its Workflow") {
		t.Errorf("expected error to tell the user to include the Workflow, got: %v", err)
	}
}

func TestResolveWorkflow_NameNotFoundListsLoadedWorkflows(t *testing.T) {
	manifest := `apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: alpha
  namespace: default
spec:
  steps:
    - name: step1
      expressions:
        - name: x
          expression: '"hello"'
`
	exec := NewLocalWorkflowExecutor(nil, "", 5, "", "")
	if err := exec.LoadFromReader(strings.NewReader(manifest)); err != nil {
		t.Fatalf("LoadFromReader: %v", err)
	}

	_, _, err := exec.ResolveWorkflow(context.Background(), "beta", "")
	if err == nil {
		t.Fatal("expected an error for an unknown workflow name")
	}
	if !strings.Contains(err.Error(), "beta") || !strings.Contains(err.Error(), "alpha") {
		t.Errorf("expected error to name both the requested and the loaded workflows, got: %v", err)
	}
}

// The same workflow name loaded in two namespaces is ambiguous by itself; an explicit namespace
// hint (as the CLI's --namespace/getNamespace() forwards on the -f and --workflow-dir paths)
// must disambiguate it instead of always erroring regardless of what the caller asked for.
func TestResolveWorkflow_NamespaceDisambiguatesMultipleMatches(t *testing.T) {
	manifest := `apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: wf
  namespace: ns-a
spec:
  steps:
    - name: step1
      expressions:
        - name: x
          expression: '"hello"'
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: wf
  namespace: ns-b
spec:
  steps:
    - name: step1
      expressions:
        - name: x
          expression: '"hello"'
`
	exec := NewLocalWorkflowExecutor(nil, "", 5, "", "")
	if err := exec.LoadFromReader(strings.NewReader(manifest)); err != nil {
		t.Fatalf("LoadFromReader: %v", err)
	}

	// No namespace hint: still ambiguous.
	if _, _, err := exec.ResolveWorkflow(context.Background(), "wf", ""); err == nil {
		t.Fatal("expected an error when no namespace hint disambiguates the match")
	}

	// A namespace hint that resolves to exactly one match must succeed.
	name, ns, err := exec.ResolveWorkflow(context.Background(), "wf", "ns-b")
	if err != nil {
		t.Fatalf("ResolveWorkflow with namespace hint: %v", err)
	}
	if name != "wf" || ns != "ns-b" {
		t.Errorf("expected wf/ns-b, got %s/%s", name, ns)
	}
}

// With no Kubernetes client at all, a top-level resourceQuery step must fail with a clear,
// actionable error rather than panicking on a nil client deref.
func TestExecuteWorkflow_NilClientResourceQueryPreflightError(t *testing.T) {
	manifest := `apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: needs-cluster
  namespace: default
spec:
  steps:
    - name: getPod
      resourceQuery:
        apiVersion: v1
        resource: Pod
        name: '"my-pod"'
`
	exec := NewLocalWorkflowExecutor(nil, "", 5, "", "")
	if err := exec.LoadFromReader(strings.NewReader(manifest)); err != nil {
		t.Fatalf("LoadFromReader: %v", err)
	}

	_, err := exec.ExecuteWorkflow(context.Background(), "needs-cluster", "default", nil)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "kubernetes client not available") {
		t.Errorf("expected a 'kubernetes client not available' error, got: %v", err)
	}
}

// The top-level preflight loop only inspects each Step's direct action, not nested steps
// inside a forEach body. A resourceQuery nested under forEach must still be caught -- by the
// executor's own nil-client guard at the point it actually runs -- instead of panicking.
func TestExecuteWorkflow_NilClientForEachResourceQueryFunnelGuard(t *testing.T) {
	manifest := `apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: needs-cluster-foreach
  namespace: default
spec:
  steps:
    - name: queryEach
      forEach:
        items: '[1, 2]'
        itemFailurePolicy: Fail
        step:
          resourceQuery:
            apiVersion: v1
            resource: Pod
            name: '"my-pod"'
`
	exec := NewLocalWorkflowExecutor(nil, "", 5, "", "")
	if err := exec.LoadFromReader(strings.NewReader(manifest)); err != nil {
		t.Fatalf("LoadFromReader: %v", err)
	}

	_, err := exec.ExecuteWorkflow(context.Background(), "needs-cluster-foreach", "default", nil)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "kubernetes client not available") {
		t.Errorf("expected a 'kubernetes client not available' error, got: %v", err)
	}
}

// LoadFromDirectory shares loadDocuments with LoadFromReader; confirm the namespace-less
// default still applies to a directory-loaded manifest (regression guard for the shared path).
func TestLoadFromDirectory_NamespaceLessWorkflowDefaultsToDefault(t *testing.T) {
	dir := t.TempDir()
	manifest := `apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: bare-ns
spec:
  steps:
    - name: step1
      expressions:
        - name: x
          expression: '"hello"'
`
	if err := os.WriteFile(filepath.Join(dir, "wf.yaml"), []byte(manifest), 0600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	exec := NewLocalWorkflowExecutor(nil, "", 5, "", "")
	if err := exec.LoadFromDirectory(dir); err != nil {
		t.Fatalf("LoadFromDirectory: %v", err)
	}

	wf, err := exec.GetWorkflow(context.Background(), "bare-ns", "default")
	if err != nil {
		t.Fatalf("GetWorkflow(bare-ns, default): %v", err)
	}
	if wf.Name != "bare-ns" {
		t.Errorf("expected workflow bare-ns, got %s", wf.Name)
	}
}

// A workflow with no steps must fail, not report Succeeded as a no-op.
func TestExecuteWorkflow_NoStepsFails(t *testing.T) {
	exec := NewLocalWorkflowExecutor(nil, "", 1, "", "")
	manifest := `apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: wf
spec:
  steps: []
`
	if err := exec.LoadFromReader(strings.NewReader(manifest)); err != nil {
		t.Fatalf("LoadFromReader: %v", err)
	}
	run, err := exec.ExecuteWorkflow(context.Background(), "wf", "default", nil)
	if err == nil || !strings.Contains(err.Error(), "workflow has no steps") {
		t.Fatalf("expected a 'workflow has no steps' error, got: %v", err)
	}
	if run == nil || run.Status.Phase != "Failed" {
		t.Fatalf("expected Phase Failed, got: %+v", run)
	}
}
