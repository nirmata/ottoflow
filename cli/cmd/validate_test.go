/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	cliexec "github.com/nirmata/ottoflow/cli/internal/executor"
	"github.com/nirmata/ottoflow/internal/webhook"
	"github.com/nirmata/ottoflow/internal/workflow/executor"
)

func TestBuildDAG_CycleDetected(t *testing.T) {
	steps := []ottoflowv1alpha1.Step{
		{Name: "a", DependsOn: []string{"b"}},
		{Name: "b", DependsOn: []string{"a"}},
	}
	_, err := executor.BuildDAG(steps)
	if err == nil {
		t.Fatal("expected cycle error, got nil")
	}
	if !strings.Contains(err.Error(), "circular") {
		t.Errorf("expected 'circular' in error, got: %v", err)
	}
}

func TestBuildDAG_MissingDependsOn(t *testing.T) {
	steps := []ottoflowv1alpha1.Step{
		{Name: "a", DependsOn: []string{"nonexistent"}},
	}
	_, err := executor.BuildDAG(steps)
	if err == nil {
		t.Fatal("expected missing-step error, got nil")
	}
	if !strings.Contains(err.Error(), "non-existent") {
		t.Errorf("expected 'non-existent' in error, got: %v", err)
	}
}

func TestValidateStepDependencies_MissingDependsOn(t *testing.T) {
	spec := &ottoflowv1alpha1.WorkflowSpec{
		Steps: []ottoflowv1alpha1.Step{
			{Name: "collect", Outputs: []ottoflowv1alpha1.Output{{Name: "data"}}},
			{
				Name: "report",
				// references steps.collect but missing dependsOn
				Expressions: []ottoflowv1alpha1.Expression{{Expression: "steps.collect.data"}},
			},
		},
	}
	err := webhook.ValidateStepDependencies(spec)
	if err == nil {
		t.Fatal("expected dependency error, got nil")
	}
}

func TestCollectCELExpressions_ExcludesAgentPrompts(t *testing.T) {
	step := &ottoflowv1alpha1.Step{
		Name: "analyze",
		AgentRef: &ottoflowv1alpha1.StepAgentRef{
			Name:              "my-agent",
			AdditionalPrompts: []string{"Please analyze this cluster. It has many pods."},
		},
		Expressions: []ottoflowv1alpha1.Expression{
			{Expression: "inputs.count > 0"},
		},
	}
	exprs := collectCELExpressions(step)
	if len(exprs) != 1 || exprs[0] != "inputs.count > 0" {
		t.Errorf("expected only CEL expression, got: %v", exprs)
	}
}

func TestCELSyntaxCheck_InvalidExpression(t *testing.T) {
	celEnv, err := executor.NewValidationCELEnv()
	if err != nil {
		t.Fatalf("NewValidationCELEnv: %v", err)
	}
	// '!!!' is syntactically invalid CEL
	_, iss := celEnv.Compile("!!!")
	if iss == nil || iss.Err() == nil {
		t.Error("expected compile error for '!!!', got none")
	}
}

func TestCELSyntaxCheck_ValidExpression(t *testing.T) {
	celEnv, err := executor.NewValidationCELEnv()
	if err != nil {
		t.Fatalf("NewValidationCELEnv: %v", err)
	}
	_, iss := celEnv.Compile("inputs.name + ' world'")
	if iss != nil && iss.Err() != nil {
		t.Errorf("expected valid expression to compile, got: %v", iss.Err())
	}
}

// TestCostAnalyzerCELExpressions loads the cost-analyzer workflow YAML and compiles
// every step expression through the validation CEL environment, including type-check
// errors. This catches "expected type 'string' but found 'dyn'" mistakes — the class
// of runtime compilation failure that isCELTypeOnlyError normally suppresses.
func TestCostAnalyzerCELExpressions(t *testing.T) {
	wf, err := loadWorkflowFromFile("../../samples/workflows/production/cost-analyzer.yaml")
	if err != nil {
		t.Fatalf("load cost-analyzer.yaml: %v", err)
	}

	celEnv, err := executor.NewValidationCELEnv()
	if err != nil {
		t.Fatalf("NewValidationCELEnv: %v", err)
	}

	for i := range wf.Spec.Steps {
		step := &wf.Spec.Steps[i]
		for _, expr := range collectCELExpressions(step) {
			_, iss := celEnv.Compile(expr)
			if iss == nil || iss.Err() == nil {
				continue
			}
			// Report ALL compile errors — including type-check errors. Unlike the
			// main validate path (which skips type-only errors as potential false
			// positives), this test enforces that every expression in cost-analyzer
			// compiles cleanly. Runtime failures like "expected type 'string' but
			// found 'dyn'" are compile-time errors and must be fixed with string().
			t.Errorf("step %q: expr compile error: %s\n  expr: %s",
				step.Name, iss.Err(), expr)
		}
	}
}

// buildLocalExecOrFatal builds a LocalWorkflowExecutor from an in-memory YAML manifest stream,
// the same way local --workflow-dir validation does, without touching disk.
func buildLocalExecOrFatal(t *testing.T, yamlDoc string) *cliexec.LocalWorkflowExecutor {
	t.Helper()
	exec := cliexec.NewLocalWorkflowExecutor(nil, "", 0, "", "")
	if err := exec.LoadFromReader(strings.NewReader(yamlDoc)); err != nil {
		t.Fatalf("LoadFromReader: %v", err)
	}
	return exec
}

// TestValidateStepRefs covers every reference kind Check 6 resolves: the direct
// stepTemplateRef step type, forEach.stepTemplateRef, agentRef, mcpToolCall.server, a bare
// step.workflowRef, and forEach.step's own inline agentRef. Each case supplies a
// workflow-only manifest (the reference is missing) and a workflow+referent manifest (the
// reference resolves), asserting REF_NOT_FOUND vs. clean respectively.
func TestValidateStepRefs(t *testing.T) {
	ctx := context.Background()
	celEnv, celEnvErr := executor.NewValidationCELEnv()

	cases := []struct {
		name        string
		missingYAML string
		presentYAML string
		// wantMsgContains, when set, is asserted against the "missing is flagged" subtest's
		// REF_NOT_FOUND message in addition to the shared code/count checks every case gets.
		wantMsgContains []string
	}{
		{
			name:            "forEach stepTemplateRef",
			wantMsgContains: []string{"missing-tpl", "default"},
			missingYAML: `apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: wf
spec:
  steps:
    - name: loop
      forEach:
        items: '[]'
        stepTemplateRef:
          name: missing-tpl
`,
			presentYAML: `apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: wf
spec:
  steps:
    - name: loop
      forEach:
        items: '[]'
        stepTemplateRef:
          name: present-tpl
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: StepTemplate
metadata:
  name: present-tpl
spec:
  step: {}
`,
		},
		{
			name: "direct stepTemplateRef",
			missingYAML: `apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: wf
spec:
  steps:
    - name: instantiate
      stepTemplateRef:
        name: missing-tpl
`,
			presentYAML: `apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: wf
spec:
  steps:
    - name: instantiate
      stepTemplateRef:
        name: present-tpl
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: StepTemplate
metadata:
  name: present-tpl
spec:
  step: {}
`,
		},
		{
			name: "agentRef",
			missingYAML: `apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: wf
spec:
  steps:
    - name: ask
      agentRef:
        name: missing-agent
`,
			presentYAML: `apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: wf
spec:
  steps:
    - name: ask
      agentRef:
        name: present-agent
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Agent
metadata:
  name: present-agent
spec:
  prompt: "answer questions"
  modelProvider: anthropic
  modelName: claude-3-opus
`,
		},
		{
			name: "mcpToolCall.server",
			missingYAML: `apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: wf
spec:
  steps:
    - name: callTool
      mcpToolCall:
        server: missing-server
        tool: some-tool
`,
			presentYAML: `apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: wf
spec:
  steps:
    - name: callTool
      mcpToolCall:
        server: present-server
        tool: some-tool
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: MCPServer
metadata:
  name: present-server
spec:
  transport:
    type: stdio
    command: ["echo"]
`,
		},
		{
			// Bare step.workflowRef (sub-workflow execution), distinct from forEach.step's
			// inline workflowRef covered below.
			name:            "workflowRef",
			wantMsgContains: []string{"missing-workflow", "default"},
			missingYAML: `apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: wf
spec:
  steps:
    - name: sub
      workflowRef:
        name: missing-workflow
`,
			presentYAML: `apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: wf
spec:
  steps:
    - name: sub
      workflowRef:
        name: present-workflow
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: present-workflow
spec: {}
`,
		},
		{
			// forEach.step's own inline agentRef -- previously not statically checked at
			// all (collectStepReferences skipped everything inside an inline forEach.step).
			name:            "forEach.step agentRef",
			wantMsgContains: []string{"missing-agent", "default"},
			missingYAML: `apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: wf
spec:
  steps:
    - name: loop
      forEach:
        items: '[]'
        step:
          agentRef:
            name: missing-agent
`,
			presentYAML: `apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: wf
spec:
  steps:
    - name: loop
      forEach:
        items: '[]'
        step:
          agentRef:
            name: present-agent
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Agent
metadata:
  name: present-agent
spec:
  prompt: "answer questions"
  modelProvider: anthropic
  modelName: claude-3-opus
`,
		},
		{
			// forEach.step's own inline mcpToolCall.server.
			name:            "forEach.step mcpToolCall.server",
			wantMsgContains: []string{"missing-server", "default"},
			missingYAML: `apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: wf
spec:
  steps:
    - name: loop
      forEach:
        items: '[]'
        step:
          mcpToolCall:
            server: missing-server
            tool: some-tool
`,
			presentYAML: `apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: wf
spec:
  steps:
    - name: loop
      forEach:
        items: '[]'
        step:
          mcpToolCall:
            server: present-server
            tool: some-tool
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: MCPServer
metadata:
  name: present-server
spec:
  transport:
    type: stdio
    command: ["echo"]
`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("missing is flagged", func(t *testing.T) {
				exec := buildLocalExecOrFatal(t, tc.missingYAML)
				wf, err := exec.GetWorkflow(ctx, "wf", "default")
				if err != nil {
					t.Fatalf("GetWorkflow: %v", err)
				}
				errs := checkWorkflow(ctx, wf, exec.ControlClient(), celEnv, celEnvErr)
				if len(errs) != 1 || errs[0].code != "REF_NOT_FOUND" {
					t.Fatalf("expected exactly one REF_NOT_FOUND, got: %+v", errs)
				}
				for _, want := range tc.wantMsgContains {
					if !strings.Contains(errs[0].message, want) {
						t.Errorf("expected error message to contain %q, got: %s", want, errs[0].message)
					}
				}
			})

			t.Run("present is clean", func(t *testing.T) {
				exec := buildLocalExecOrFatal(t, tc.presentYAML)
				wf, err := exec.GetWorkflow(ctx, "wf", "default")
				if err != nil {
					t.Fatalf("GetWorkflow: %v", err)
				}
				errs := checkWorkflow(ctx, wf, exec.ControlClient(), celEnv, celEnvErr)
				if len(errs) != 0 {
					t.Errorf("expected no errors, got: %+v", errs)
				}
			})
		})
	}
}

func TestValidateWorkflowRunRefs_MissingAndPresent(t *testing.T) {
	ctx := context.Background()

	t.Run("missing workflow is flagged", func(t *testing.T) {
		exec := buildLocalExecOrFatal(t, `apiVersion: ottoflow.nirmata.io/v1alpha1
kind: WorkflowRun
metadata:
  name: my-run
  namespace: default
spec:
  workflowRef:
    name: missing-workflow
    namespace: default
`)
		results := checkWorkflowRunRefs(ctx, exec)
		if len(results) != 1 {
			t.Fatalf("expected 1 failing WorkflowRun, got %d: %+v", len(results), results)
		}
		if results[0].runName != "my-run" {
			t.Errorf("expected runName %q, got %q", "my-run", results[0].runName)
		}
		if len(results[0].errs) != 1 || !strings.Contains(results[0].errs[0].message, "missing-workflow") {
			t.Errorf("expected REF_NOT_FOUND naming missing-workflow, got: %+v", results[0].errs)
		}
	})

	t.Run("matching workflow is clean", func(t *testing.T) {
		exec := buildLocalExecOrFatal(t, `apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: my-workflow
  namespace: default
spec:
  steps:
    - name: step1
      expressions:
        - name: a
          expression: "'ok'"
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: WorkflowRun
metadata:
  name: my-run
  namespace: default
spec:
  workflowRef:
    name: my-workflow
    namespace: default
`)
		results := checkWorkflowRunRefs(ctx, exec)
		if len(results) != 0 {
			t.Errorf("expected no failing WorkflowRuns, got: %+v", results)
		}
	})
}

// TestValidateWorkflowRunRefs_ResolvesNamespaceFromLoader is a regression test: a WorkflowRun
// declaring no namespace anywhere gets its Namespace field force-defaulted to "default" by
// indexWorkflowRuns, but the loader separately rebinds it (for lookup purposes) into the
// namespace of the single Workflow matching its workflowRef.name. checkWorkflowRunRefs must
// use that rebound ResolvedNamespace, not Run.Namespace, or it reports a false REF_NOT_FOUND
// against a WorkflowRun that is actually fine.
func TestValidateWorkflowRunRefs_ResolvesNamespaceFromLoader(t *testing.T) {
	ctx := context.Background()
	exec := buildLocalExecOrFatal(t, `apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: my-workflow
  namespace: ottoflow
spec:
  steps:
    - name: step1
      expressions:
        - name: a
          expression: "'ok'"
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: WorkflowRun
metadata:
  name: my-run
spec:
  workflowRef:
    name: my-workflow
`)

	lrs := exec.ListWorkflowRuns()
	if len(lrs) != 1 {
		t.Fatalf("expected 1 loaded WorkflowRun, got %d", len(lrs))
	}
	if lrs[0].Run.Namespace != "default" {
		t.Fatalf("test assumption invalid: expected Run.Namespace force-defaulted to %q, got %q",
			"default", lrs[0].Run.Namespace)
	}
	if lrs[0].ResolvedNamespace != "ottoflow" {
		t.Fatalf("expected ResolvedNamespace %q, got %q", "ottoflow", lrs[0].ResolvedNamespace)
	}

	results := checkWorkflowRunRefs(ctx, exec)
	if len(results) != 0 {
		t.Errorf("expected clean (no false positive from using Run.Namespace=default), got: %+v", results)
	}
}

// TestValidateStepRefs_StepTemplateInnerRef verifies checkStepRefs checks one level into a
// resolved stepTemplateRef: the workflow's own reference to the template resolves fine, but
// the template's own step (its Spec.Step) references a missing Agent. Without this check, a
// shared template with a stale internal ref would only fail at run time.
func TestValidateStepRefs_StepTemplateInnerRef(t *testing.T) {
	ctx := context.Background()
	celEnv, celEnvErr := executor.NewValidationCELEnv()

	t.Run("missing inner ref is flagged", func(t *testing.T) {
		exec := buildLocalExecOrFatal(t, `apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: wf
spec:
  steps:
    - name: instantiate
      stepTemplateRef:
        name: shared-tpl
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: StepTemplate
metadata:
  name: shared-tpl
spec:
  step:
    agentRef:
      name: missing-agent
`)
		wf, err := exec.GetWorkflow(ctx, "wf", "default")
		if err != nil {
			t.Fatalf("GetWorkflow: %v", err)
		}
		errs := checkWorkflow(ctx, wf, exec.ControlClient(), celEnv, celEnvErr)
		if len(errs) != 1 || errs[0].code != "REF_NOT_FOUND" {
			t.Fatalf("expected exactly one REF_NOT_FOUND, got: %+v", errs)
		}
		if !strings.Contains(errs[0].message, "missing-agent") || !strings.Contains(errs[0].message, "shared-tpl") {
			t.Errorf("expected error naming both the missing agent and the template it came from, got: %s", errs[0].message)
		}
	})

	t.Run("present inner ref is clean", func(t *testing.T) {
		exec := buildLocalExecOrFatal(t, `apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: wf
spec:
  steps:
    - name: instantiate
      stepTemplateRef:
        name: shared-tpl
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: StepTemplate
metadata:
  name: shared-tpl
spec:
  step:
    agentRef:
      name: present-agent
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Agent
metadata:
  name: present-agent
spec:
  prompt: "answer questions"
  modelProvider: anthropic
  modelName: claude-3-opus
`)
		wf, err := exec.GetWorkflow(ctx, "wf", "default")
		if err != nil {
			t.Fatalf("GetWorkflow: %v", err)
		}
		errs := checkWorkflow(ctx, wf, exec.ControlClient(), celEnv, celEnvErr)
		if len(errs) != 0 {
			t.Errorf("expected no errors, got: %+v", errs)
		}
	})
}

// writeValidateTestFile writes content to a temp file and returns its path, for fileRefWarnings
// tests below which -- unlike buildLocalExecOrFatal -- need a real file on disk to exercise
// loadWorkflowFromFile and fileRefWarnings' independent os.ReadFile.
func writeValidateTestFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wf.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write test file: %v", err)
	}
	return path
}

// TestFileRefWarnings_SelfContainedIsClean covers the "-f with a self-contained file" half of
// finding #4: a file holding the Workflow plus its own StepTemplate has nothing to warn about.
func TestFileRefWarnings_SelfContainedIsClean(t *testing.T) {
	ctx := context.Background()
	path := writeValidateTestFile(t, `apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: wf
spec:
  steps:
    - name: instantiate
      stepTemplateRef:
        name: present-tpl
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: StepTemplate
metadata:
  name: present-tpl
spec:
  step: {}
`)
	wf, err := loadWorkflowFromFile(path)
	if err != nil {
		t.Fatalf("loadWorkflowFromFile: %v", err)
	}
	if warnings := fileRefWarnings(ctx, path, wf); len(warnings) != 0 {
		t.Errorf("expected no warnings for a self-contained file, got: %+v", warnings)
	}
}

// TestFileRefWarnings_MissingRefIsWarningNotError covers the "-f with a workflow referencing
// a missing object -> warning surfaced (non-fatal)" half of finding #4: fileRefWarnings
// reports the unresolved ref, but loadWorkflowFromFile -- the call that determines whether
// `validate -f` fails -- succeeds regardless, so the ref check never becomes fatal in -f mode.
func TestFileRefWarnings_MissingRefIsWarningNotError(t *testing.T) {
	ctx := context.Background()
	path := writeValidateTestFile(t, `apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: wf
spec:
  steps:
    - name: ask
      agentRef:
        name: missing-agent
`)
	wf, err := loadWorkflowFromFile(path)
	if err != nil {
		t.Fatalf("loadWorkflowFromFile: %v (must succeed -- the ref check is a warning, not a load failure)", err)
	}
	warnings := fileRefWarnings(ctx, path, wf)
	if len(warnings) != 1 || warnings[0].code != "REF_NOT_FOUND" {
		t.Fatalf("expected exactly one REF_NOT_FOUND warning, got: %+v", warnings)
	}
	if !strings.Contains(warnings[0].message, "missing-agent") {
		t.Errorf("expected warning naming missing-agent, got: %s", warnings[0].message)
	}
}
