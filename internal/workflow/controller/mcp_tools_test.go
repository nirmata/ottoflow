/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/mark3labs/mcp-go/mcp"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

func newMCPTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(ottoflowv1alpha1.AddToScheme(s))
	return s
}

// noAuth stands in for the TokenReview/SubjectAccessReview authenticator in
// tests that are about tool behavior rather than about who may call it.
type noAuth struct{}

func (noAuth) Middleware(next http.Handler) http.Handler { return next }

func newTestMCPServer(t *testing.T, objects ...client.Object) *MCPToolServer {
	t.Helper()
	k8s := fake.NewClientBuilder().WithScheme(newMCPTestScheme(t)).WithObjects(objects...).Build()
	s, err := NewMCPToolServer(k8s, noAuth{}, ":0")
	if err != nil {
		t.Fatalf("NewMCPToolServer: %v", err)
	}
	return s
}

func newWorkflow(namespace, name string, inputs ...ottoflowv1alpha1.Input) *ottoflowv1alpha1.Workflow {
	return &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Inputs:  inputs,
			Steps:   []ottoflowv1alpha1.Step{{Name: "noop"}},
			MCPTool: &ottoflowv1alpha1.MCPTool{Enabled: true},
		},
	}
}

// An authenticator is not optional: the endpoint runs every workflow in the
// cluster, and a nil one would serve that to anyone who can reach the port.
func TestNewMCPToolServerRequiresAnAuthenticator(t *testing.T) {
	k8s := fake.NewClientBuilder().WithScheme(newMCPTestScheme(t)).Build()
	s, err := NewMCPToolServer(k8s, nil, ":0")
	if err == nil {
		t.Fatal("NewMCPToolServer succeeded without an authenticator")
	}
	if s != nil {
		t.Error("NewMCPToolServer returned a server alongside an error")
	}
}

func TestToolNameRoundTrips(t *testing.T) {
	tests := []struct {
		namespace string
		name      string
	}{
		{"default", "cost-analyzer"},
		{"team-a", "nightly-report"},
		{"a", "b"},
		// A hyphenated pair on both sides: the separator has to survive names
		// that are themselves full of the character a single separator would use.
		{"my-team-ns", "my-long-workflow-name"},
	}

	for _, tc := range tests {
		t.Run(tc.namespace+"/"+tc.name, func(t *testing.T) {
			ns, name, ok := splitToolName(toolName(tc.namespace, tc.name))
			if !ok {
				t.Fatalf("splitToolName(%q) reported failure", toolName(tc.namespace, tc.name))
			}
			if ns != tc.namespace || name != tc.name {
				t.Errorf("round trip = %s/%s, want %s/%s", ns, name, tc.namespace, tc.name)
			}
		})
	}
}

func TestSplitToolNameRejectsMalformedNames(t *testing.T) {
	for _, name := range []string{"", "no-separator", "__trailing", "leading__"} {
		if _, _, ok := splitToolName(name); ok {
			t.Errorf("splitToolName(%q) accepted a name that addresses no workflow", name)
		}
	}
}

func TestWorkflowToolDerivesTheInputSchema(t *testing.T) {
	wf := newWorkflow("default", "cost-analyzer",
		ottoflowv1alpha1.Input{Name: "namespace", Description: "Namespace to analyze", Required: true},
		ottoflowv1alpha1.Input{Name: "window", Description: "Look-back window", Default: "24h"},
		ottoflowv1alpha1.Input{Name: "bare"},
	)
	wf.Spec.MCPTool.Description = "Right-size workloads in a namespace."

	tool := workflowTool(wf)

	if tool.Name != "default__cost-analyzer" {
		t.Errorf("tool name = %q", tool.Name)
	}
	if tool.Description != "Right-size workloads in a namespace." {
		t.Errorf("description = %q, want the configured one", tool.Description)
	}
	if diff := cmp.Diff([]string{"namespace"}, tool.InputSchema.Required); diff != "" {
		t.Errorf("required inputs (-want +got):\n%s", diff)
	}

	got := make([]string, 0, len(tool.InputSchema.Properties))
	for name := range tool.InputSchema.Properties {
		got = append(got, name)
	}
	sort.Strings(got)
	if diff := cmp.Diff([]string{"bare", "namespace", "window"}, got); diff != "" {
		t.Errorf("schema properties (-want +got):\n%s", diff)
	}

	// Every input is a string, and the default reaches both the schema and the
	// description a model actually reads.
	window, ok := tool.InputSchema.Properties["window"].(map[string]any)
	if !ok {
		t.Fatalf("window property is %T, want a schema object", tool.InputSchema.Properties["window"])
	}
	if window["type"] != "string" {
		t.Errorf("window type = %v, want string", window["type"])
	}
	if window["default"] != "24h" {
		t.Errorf("window default = %v, want 24h", window["default"])
	}
	if desc, _ := window["description"].(string); !strings.Contains(desc, "24h") {
		t.Errorf("window description = %q, want it to name the default", desc)
	}
}

// A workflow that sets no description still has to say something: an MCP client
// showing an empty description gives a model nothing to choose on.
func TestWorkflowToolFallsBackToAGeneratedDescription(t *testing.T) {
	tool := workflowTool(newWorkflow("team-a", "nightly-report"))
	if tool.Description == "" {
		t.Fatal("tool description is empty")
	}
	for _, want := range []string{"nightly-report", "team-a"} {
		if !strings.Contains(tool.Description, want) {
			t.Errorf("description %q does not name %q", tool.Description, want)
		}
	}
}

func TestSyncToolsFollowsTheWorkflowsInTheCluster(t *testing.T) {
	first := newWorkflow("default", "first")
	second := newWorkflow("team-a", "second")
	s := newTestMCPServer(t, first, second)
	ctx := context.Background()

	if err := s.syncTools(ctx); err != nil {
		t.Fatalf("syncTools: %v", err)
	}
	if diff := cmp.Diff([]string{"default__first", "team-a__second"}, registeredToolNames(s)); diff != "" {
		t.Errorf("tools after the first sync (-want +got):\n%s", diff)
	}

	// A workflow that goes away stops being callable, rather than lingering as
	// a tool whose call would fail at the Get.
	if err := s.client.Delete(ctx, first); err != nil {
		t.Fatalf("deleting workflow: %v", err)
	}
	third := newWorkflow("default", "third")
	if err := s.client.Create(ctx, third); err != nil {
		t.Fatalf("creating workflow: %v", err)
	}
	if err := s.syncTools(ctx); err != nil {
		t.Fatalf("syncTools: %v", err)
	}
	if diff := cmp.Diff([]string{"default__third", "team-a__second"}, registeredToolNames(s)); diff != "" {
		t.Errorf("tools after the second sync (-want +got):\n%s", diff)
	}
}

func registeredToolNames(s *MCPToolServer) []string {
	tools := s.mcp.ListTools()
	names := make([]string, 0, len(tools))
	for name := range tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func TestInputValues(t *testing.T) {
	wf := newWorkflow("default", "wf",
		ottoflowv1alpha1.Input{Name: "namespace", Required: true},
		ottoflowv1alpha1.Input{Name: "window", Default: "24h"},
		ottoflowv1alpha1.Input{Name: "dryRun", Required: true, Default: "true"},
	)

	tests := []struct {
		name    string
		args    map[string]any
		want    map[string]string
		wantErr string
	}{
		{
			name: "required input provided",
			args: map[string]any{"namespace": "prod"},
			want: map[string]string{"namespace": "prod"},
		},
		{
			name: "non-string arguments are rendered, since every input is a string",
			args: map[string]any{"namespace": "prod", "window": 24, "dryRun": false},
			want: map[string]string{"namespace": "prod", "window": "24", "dryRun": "false"},
		},
		{
			name:    "missing required input",
			args:    map[string]any{"window": "1h"},
			wantErr: `missing required input "namespace"`,
		},
		{
			// A misspelled input would otherwise run the default and look like
			// the workflow ignoring what the caller asked for.
			name:    "undeclared input",
			args:    map[string]any{"namespace": "prod", "nmespace": "prod"},
			wantErr: `declares no input named "nmespace"`,
		},
		{
			// Required with a default is satisfiable without the caller: the
			// executor has a value either way.
			name: "required input with a default may be omitted",
			args: map[string]any{"namespace": "prod"},
			want: map[string]string{"namespace": "prod"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := inputValues(wf, tc.args)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("inputValues succeeded, want an error naming %q", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("inputValues: %v", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("input values (-want +got):\n%s", diff)
			}
		})
	}
}

func TestCallWorkflowRejectsUnknownTargets(t *testing.T) {
	s := newTestMCPServer(t, newWorkflow("default", "known"))

	tests := []struct {
		name     string
		tool     string
		args     map[string]any
		wantText string
	}{
		{"tool name that addresses nothing", "not-a-workflow", nil, "does not address a workflow"},
		{"workflow that does not exist", "default__missing", nil, "not found"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := s.callWorkflow(context.Background(), callToolRequest(tc.tool, tc.args))
			if err != nil {
				t.Fatalf("callWorkflow returned a transport error: %v", err)
			}
			assertToolError(t, result, tc.wantText)
		})
	}
}

// The concurrency limit a workflow declares applies however a run is asked
// for, including over MCP.
func TestCallWorkflowHonoursMaxConcurrentRuns(t *testing.T) {
	limit := int32(1)
	wf := newWorkflow("default", "limited")
	wf.Spec.Run = &ottoflowv1alpha1.RunPolicy{MaxConcurrentRuns: &limit}

	running := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "limited-already-running",
			Labels:    map[string]string{"ottoflow.nirmata.io/workflow": "limited"},
		},
		Spec:   ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "limited", Namespace: "default"}},
		Status: ottoflowv1alpha1.WorkflowRunStatus{Phase: ottoflowv1alpha1.WorkflowRunPhaseRunning},
	}

	s := newTestMCPServer(t, wf, running)
	result, err := s.callWorkflow(context.Background(), callToolRequest("default__limited", nil))
	if err != nil {
		t.Fatalf("callWorkflow: %v", err)
	}
	assertToolError(t, result, "concurrency limit")
}

// A tool call is a WorkflowRun like any other, and carries the labels that say
// where it came from.
func TestCallWorkflowCreatesALabelledRun(t *testing.T) {
	wf := newWorkflow("default", "wf", ottoflowv1alpha1.Input{Name: "namespace", Required: true})
	wf.Spec.Execution = &ottoflowv1alpha1.WorkflowRunExecutionSpec{Job: &ottoflowv1alpha1.WorkflowRunJobSpec{Env: []corev1.EnvVar{{Name: "LLM_TOKEN", Value: "test-value"}}}}
	s := newTestMCPServer(t, wf)
	s.callTimeout = 10 * time.Millisecond
	s.pollInterval = time.Millisecond

	result, err := s.callWorkflow(context.Background(), callToolRequest("default__wf", map[string]any{"namespace": "prod"}))
	if err != nil {
		t.Fatalf("callWorkflow: %v", err)
	}
	// Nothing reconciles the run in this test, so the call reaches its
	// deadline — and must still name the run it left behind.
	assertToolError(t, result, "still running")

	var runs ottoflowv1alpha1.WorkflowRunList
	if err := s.client.List(context.Background(), &runs); err != nil {
		t.Fatalf("listing runs: %v", err)
	}
	if len(runs.Items) != 1 {
		t.Fatalf("created %d workflow runs, want 1", len(runs.Items))
	}
	run := runs.Items[0]
	if got := run.Labels["ottoflow.nirmata.io/trigger"]; got != mcpTriggerLabelValue {
		t.Errorf("trigger label = %q, want %q", got, mcpTriggerLabelValue)
	}
	if got := run.Labels["ottoflow.nirmata.io/managed-by"]; got != mcpManagedByLabelValue {
		t.Errorf("managed-by label = %q, want %q", got, mcpManagedByLabelValue)
	}
	if diff := cmp.Diff(map[string]string{"namespace": "prod"}, run.Spec.InputValues); diff != "" {
		t.Errorf("input values (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(wf.Spec.Execution, run.Spec.Execution); diff != "" {
		t.Errorf("run execution (-want +got):\n%s", diff)
	}
	if !strings.Contains(textOf(result), run.Name) {
		t.Errorf("deadline result %q does not name the run %q", textOf(result), run.Name)
	}
}

func TestAwaitRunReturnsOutputsOnSuccess(t *testing.T) {
	run := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "run-1"},
		Status: ottoflowv1alpha1.WorkflowRunStatus{
			Phase: ottoflowv1alpha1.WorkflowRunPhaseSucceeded,
			Outputs: map[string]apiextensionsv1.JSON{
				"savings": {Raw: []byte(`{"monthly": 42}`)},
				"summary": {Raw: []byte(`"3 workloads resized"`)},
			},
		},
	}
	s := newTestMCPServer(t, run)

	result, err := s.awaitRun(context.Background(), run)
	if err != nil {
		t.Fatalf("awaitRun: %v", err)
	}
	if result.IsError {
		t.Fatalf("succeeded run produced a tool error: %s", textOf(result))
	}

	var payload struct {
		WorkflowRun string         `json:"workflowRun"`
		Namespace   string         `json:"namespace"`
		Outputs     map[string]any `json:"outputs"`
	}
	if err := json.Unmarshal([]byte(textOf(result)), &payload); err != nil {
		t.Fatalf("tool result is not JSON: %v (%s)", err, textOf(result))
	}
	if payload.WorkflowRun != "run-1" || payload.Namespace != "default" {
		t.Errorf("result names %s/%s, want default/run-1", payload.Namespace, payload.WorkflowRun)
	}
	// Structured outputs stay structured: a nested object must not arrive as a
	// string holding JSON.
	savings, ok := payload.Outputs["savings"].(map[string]any)
	if !ok {
		t.Fatalf("savings output is %T, want an object", payload.Outputs["savings"])
	}
	if savings["monthly"] != float64(42) {
		t.Errorf("savings.monthly = %v, want 42", savings["monthly"])
	}
	if payload.Outputs["summary"] != "3 workloads resized" {
		t.Errorf("summary output = %v", payload.Outputs["summary"])
	}
}

func TestAwaitRunReportsFailureMessage(t *testing.T) {
	run := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "run-2"},
		Status: ottoflowv1alpha1.WorkflowRunStatus{
			Phase:   ottoflowv1alpha1.WorkflowRunPhaseFailed,
			Message: "step collectPods failed: forbidden",
		},
	}
	s := newTestMCPServer(t, run)

	result, err := s.awaitRun(context.Background(), run)
	if err != nil {
		t.Fatalf("awaitRun: %v", err)
	}
	assertToolError(t, result, "step collectPods failed: forbidden")
	if !strings.Contains(textOf(result), "run-2") {
		t.Errorf("failure result %q does not name the run", textOf(result))
	}
}

// A run that is Pending when the call starts is waited on rather than reported
// as unfinished on the first read.
func TestAwaitRunWaitsForATerminalPhase(t *testing.T) {
	run := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "run-3"},
		Status:     ottoflowv1alpha1.WorkflowRunStatus{Phase: ottoflowv1alpha1.WorkflowRunPhasePending},
	}

	reads := 0
	base := fake.NewClientBuilder().WithScheme(newMCPTestScheme(t)).WithObjects(run).Build()
	k8s := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := c.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			wr, ok := obj.(*ottoflowv1alpha1.WorkflowRun)
			if !ok {
				return nil
			}
			reads++
			if reads >= 3 {
				wr.Status.Phase = ottoflowv1alpha1.WorkflowRunPhaseSucceeded
			}
			return nil
		},
	})

	s, err := NewMCPToolServer(k8s, noAuth{}, ":0")
	if err != nil {
		t.Fatalf("NewMCPToolServer: %v", err)
	}
	s.pollInterval = time.Millisecond
	s.callTimeout = 5 * time.Second

	result, err := s.awaitRun(context.Background(), run)
	if err != nil {
		t.Fatalf("awaitRun: %v", err)
	}
	if result.IsError {
		t.Fatalf("run that reached Succeeded produced a tool error: %s", textOf(result))
	}
	if reads < 3 {
		t.Errorf("run was read %d times, want it polled until terminal", reads)
	}
}

func callToolRequest(name string, args map[string]any) mcp.CallToolRequest {
	var req mcp.CallToolRequest
	req.Params.Name = name
	req.Params.Arguments = args
	return req
}

func textOf(result *mcp.CallToolResult) string {
	var b strings.Builder
	for _, content := range result.Content {
		if text, ok := content.(mcp.TextContent); ok {
			b.WriteString(text.Text)
		}
	}
	return b.String()
}

func assertToolError(t *testing.T, result *mcp.CallToolResult, want string) {
	t.Helper()
	if !result.IsError {
		t.Fatalf("result is not a tool error: %s", textOf(result))
	}
	if !strings.Contains(textOf(result), want) {
		t.Errorf("tool error = %q, want it to contain %q", textOf(result), want)
	}
}

// Exposure is opt-in per workflow: a workflow that has not asked to be a tool
// is neither listed nor callable by name.
func TestOnlyOptedInWorkflowsAreExposed(t *testing.T) {
	exposed := newWorkflow("default", "exposed")
	private := newWorkflow("default", "private")
	private.Spec.MCPTool = nil
	disabled := newWorkflow("default", "disabled")
	disabled.Spec.MCPTool = &ottoflowv1alpha1.MCPTool{Enabled: false}

	s := newTestMCPServer(t, exposed, private, disabled)
	if err := s.syncTools(context.Background()); err != nil {
		t.Fatalf("syncTools: %v", err)
	}
	if diff := cmp.Diff([]string{"default__exposed"}, registeredToolNames(s)); diff != "" {
		t.Errorf("exposed tools (-want +got):\n%s", diff)
	}

	// Calling the handler directly is the only way to reach this check: an
	// unexposed workflow is absent from the registry, so a real request is
	// refused by the transport first. What is asserted here is the guard for
	// the window between the registry being rebuilt and the run being created.
	for _, name := range []string{"default__private", "default__disabled"} {
		result, err := s.callWorkflow(context.Background(), callToolRequest(name, nil))
		if err != nil {
			t.Fatalf("callWorkflow(%s): %v", name, err)
		}
		assertToolError(t, result, "not exposed as an MCP tool")
	}
}

// An edited workflow has to change its tool. A registry that only adds and
// removes by name serves the definition captured when the workflow first
// appeared, so a renamed input or a rewritten description never reaches a
// client.
func TestSyncToolsRefreshesAnEditedWorkflow(t *testing.T) {
	wf := newWorkflow("default", "wf", ottoflowv1alpha1.Input{Name: "namespace", Required: true})
	wf.Spec.MCPTool.Description = "The first description."
	s := newTestMCPServer(t, wf)
	ctx := context.Background()

	if err := s.syncTools(ctx); err != nil {
		t.Fatalf("syncTools: %v", err)
	}

	wf.Spec.MCPTool.Description = "The second description."
	wf.Spec.Inputs = append(wf.Spec.Inputs, ottoflowv1alpha1.Input{Name: "window", Default: "24h"})
	if err := s.client.Update(ctx, wf); err != nil {
		t.Fatalf("updating workflow: %v", err)
	}
	if err := s.syncTools(ctx); err != nil {
		t.Fatalf("syncTools: %v", err)
	}

	tool := s.mcp.GetTool("default__wf")
	if tool == nil {
		t.Fatal("tool disappeared after the workflow was updated")
	}
	if tool.Tool.Description != "The second description." {
		t.Errorf("description = %q, want the edited one", tool.Tool.Description)
	}
	if _, ok := tool.Tool.InputSchema.Properties["window"]; !ok {
		t.Errorf("input added to the workflow is missing from the schema: %v", tool.Tool.InputSchema.Properties)
	}
}
