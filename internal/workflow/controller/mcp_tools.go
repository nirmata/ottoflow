/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

// toolNameSeparator joins a Workflow's namespace and name into one tool name.
// Two underscores, because a namespace and a name are DNS-1123 labels and can
// hold neither: the split back is unambiguous for every legal pair.
const toolNameSeparator = "__"

// Labels on a WorkflowRun this server creates, matching what the webhook
// trigger writes so an operator can tell runs apart by origin.
const (
	mcpTriggerLabelValue   = "mcp"
	mcpManagedByLabelValue = "ottoflow-mcp-server"
)

// toolName addresses one Workflow. Namespace is part of it because the same
// workflow name in two namespaces is two different tools.
func toolName(namespace, name string) string {
	return namespace + toolNameSeparator + name
}

// splitToolName is toolName reversed.
func splitToolName(tool string) (namespace, name string, ok bool) {
	ns, n, found := strings.Cut(tool, toolNameSeparator)
	if !found || ns == "" || n == "" {
		return "", "", false
	}
	return ns, n, true
}

// workflowTool renders one Workflow as an MCP tool.
//
// Every input is a string because Workflow.Spec.Input has no type: it is a
// name, a description, an optional default, and a required flag. Inventing
// richer JSON-schema types here would describe a contract the executor does
// not enforce, since InputValues is map[string]string on the way in.
func workflowTool(wf *ottoflowv1alpha1.Workflow) mcp.Tool {
	description := strings.TrimSpace(wf.Spec.MCPTool.GetDescription())
	if description == "" {
		// A name is a poor description, but an empty one is worse: a client
		// that shows nothing gives the model no basis to choose this tool.
		description = fmt.Sprintf("Run the %s workflow in namespace %s.", wf.Name, wf.Namespace)
	}

	opts := make([]mcp.ToolOption, 0, 2+len(wf.Spec.Inputs))
	opts = append(opts,
		mcp.WithDescription(description),
		mcp.WithTitleAnnotation(fmt.Sprintf("%s/%s", wf.Namespace, wf.Name)),
	)
	for _, in := range wf.Spec.Inputs {
		propOpts := []mcp.PropertyOption{}
		if desc := inputDescription(in); desc != "" {
			propOpts = append(propOpts, mcp.Description(desc))
		}
		if in.Required {
			propOpts = append(propOpts, mcp.Required())
		}
		if in.Default != "" {
			propOpts = append(propOpts, mcp.DefaultString(in.Default))
		}
		opts = append(opts, mcp.WithString(in.Name, propOpts...))
	}

	return mcp.NewTool(toolName(wf.Namespace, wf.Name), opts...)
}

// inputDescription states the default in prose as well as in the schema: a
// model reads the description, and an omitted optional input is the case where
// knowing the default changes what it sends.
func inputDescription(in ottoflowv1alpha1.Input) string {
	switch {
	case in.Description != "" && in.Default != "":
		return fmt.Sprintf("%s (defaults to %q)", in.Description, in.Default)
	case in.Default != "":
		return fmt.Sprintf("Defaults to %q.", in.Default)
	default:
		return in.Description
	}
}

// syncTools makes the registered tool set match the Workflows in the cluster,
// adding what appeared and removing what is gone.
func (s *MCPToolServer) syncTools(ctx context.Context) error {
	var workflows ottoflowv1alpha1.WorkflowList
	if err := s.client.List(ctx, &workflows); err != nil {
		return fmt.Errorf("listing workflows: %w", err)
	}

	tools := make([]mcpserver.ServerTool, 0, len(workflows.Items))
	for i := range workflows.Items {
		wf := &workflows.Items[i]
		if !wf.Spec.MCPTool.IsEnabled() {
			continue
		}
		tools = append(tools, mcpserver.ServerTool{Tool: workflowTool(wf), Handler: s.callWorkflow})
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Tool.Name < tools[j].Tool.Name })

	// Replace the set rather than reconcile it. Diffing would have to notice
	// that an edited workflow's inputs or description changed, and a tool
	// definition that silently describes the previous edit is worse than the
	// cost of rebuilding a list this size.
	s.mcp.SetTools(tools...)
	return nil
}

// callWorkflow runs the Workflow a tool names and waits for it.
func (s *MCPToolServer) callWorkflow(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	logger := log.FromContext(ctx).WithName("mcp-server")

	namespace, name, ok := splitToolName(req.Params.Name)
	if !ok {
		return mcp.NewToolResultErrorf("tool name %q does not address a workflow", req.Params.Name), nil
	}

	var wf ottoflowv1alpha1.Workflow
	if err := s.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &wf); err != nil {
		if apierrors.IsNotFound(err) {
			return mcp.NewToolResultErrorf("workflow %s/%s not found", namespace, name), nil
		}
		return nil, fmt.Errorf("reading workflow %s/%s: %w", namespace, name, err)
	}

	// A workflow that is not exposed is already absent from the registry, so
	// the transport rejects the name before this runs. This covers the window
	// between the two: the registry was rebuilt at the start of the request,
	// and opting out is one write away at any point after that.
	if !wf.Spec.MCPTool.IsEnabled() {
		return mcp.NewToolResultErrorf("workflow %s/%s is not exposed as an MCP tool", namespace, name), nil
	}

	inputs, err := inputValues(&wf, req.GetArguments())
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	if limit := wf.Spec.Run.EffectiveMaxConcurrentRuns(); limit > 0 {
		active, err := countActiveWorkflowRuns(ctx, s.client, &wf)
		if err != nil {
			return nil, fmt.Errorf("counting active runs for %s/%s: %w", namespace, name, err)
		}
		if active >= int(limit) {
			return mcp.NewToolResultErrorf(
				"workflow %s/%s is at its concurrency limit (%d running)", namespace, name, active), nil
		}
	}

	run, err := s.createRun(ctx, &wf, inputs)
	if err != nil {
		return nil, err
	}
	logger.Info("workflow run created from MCP tool call",
		"workflow", fmt.Sprintf("%s/%s", namespace, name), "run", run.Name)

	return s.awaitRun(ctx, run)
}

// inputValues maps tool arguments onto the workflow's declared inputs. An
// argument the workflow does not declare is rejected rather than dropped: a
// caller that misspells an input otherwise gets a successful run of the
// default, which reads as the workflow ignoring it.
func inputValues(wf *ottoflowv1alpha1.Workflow, args map[string]any) (map[string]string, error) {
	declared := make(map[string]ottoflowv1alpha1.Input, len(wf.Spec.Inputs))
	for _, in := range wf.Spec.Inputs {
		declared[in.Name] = in
	}

	var unknown []string
	values := make(map[string]string, len(args))
	for key, raw := range args {
		if _, ok := declared[key]; !ok {
			unknown = append(unknown, key)
			continue
		}
		values[key] = fmt.Sprintf("%v", raw)
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("workflow %s/%s declares no input named %s",
			wf.Namespace, wf.Name, strings.Join(quoteAll(unknown), ", "))
	}

	var missing []string
	for _, in := range wf.Spec.Inputs {
		if _, provided := values[in.Name]; provided {
			continue
		}
		if in.Required && in.Default == "" {
			missing = append(missing, in.Name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("missing required input %s", strings.Join(quoteAll(missing), ", "))
	}

	return values, nil
}

func quoteAll(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		out = append(out, fmt.Sprintf("%q", v))
	}
	return out
}

// createRun builds and creates the WorkflowRun for one tool call, named like
// every other triggered run.
func (s *MCPToolServer) createRun(
	ctx context.Context,
	wf *ottoflowv1alpha1.Workflow,
	inputs map[string]string,
) (*ottoflowv1alpha1.WorkflowRun, error) {
	rb := make([]byte, 2)
	if _, err := rand.Read(rb); err != nil {
		return nil, fmt.Errorf("generating run name: %w", err)
	}
	runName := fmt.Sprintf("%s-%s-%08x", wf.Name, hex.EncodeToString(rb), time.Now().UnixNano()&0xFFFFFFFF)

	// Manual is the trigger type: an external caller asked for this run on
	// demand, which is what the enum's Manual already means. The label is what
	// says the ask arrived over MCP.
	run := buildWorkflowRun(wf, runName, inputs, ottoflowv1alpha1.TriggerInfo{
		Type:        "Manual",
		TriggeredAt: metav1.Now(),
	})
	run.Labels["ottoflow.nirmata.io/trigger"] = mcpTriggerLabelValue
	run.Labels["ottoflow.nirmata.io/managed-by"] = mcpManagedByLabelValue

	if err := s.client.Create(ctx, run); err != nil {
		return nil, fmt.Errorf("creating workflow run for %s/%s: %w", wf.Namespace, wf.Name, err)
	}
	return run, nil
}

// awaitRun polls until the run finishes or the call deadline passes.
//
// Every path names the WorkflowRun, including the deadline one: the run is
// still executing when this returns, and the name is what lets a caller go
// look at it rather than guess whether anything happened.
func (s *MCPToolServer) awaitRun(
	ctx context.Context,
	run *ottoflowv1alpha1.WorkflowRun,
) (*mcp.CallToolResult, error) {
	deadline := time.NewTimer(s.callTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(s.pollInterval)
	defer ticker.Stop()

	key := types.NamespacedName{Namespace: run.Namespace, Name: run.Name}
	for {
		var current ottoflowv1alpha1.WorkflowRun
		if err := s.client.Get(ctx, key, &current); err != nil {
			if !apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("reading workflow run %s: %w", run.Name, err)
			}
		} else {
			switch current.Status.Phase {
			case ottoflowv1alpha1.WorkflowRunPhaseSucceeded:
				return succeededResult(&current)
			case ottoflowv1alpha1.WorkflowRunPhaseFailed:
				message := current.Status.Message
				if message == "" {
					message = "no failure message was recorded"
				}
				return mcp.NewToolResultErrorf("workflow run %s failed: %s", current.Name, message), nil
			}
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return mcp.NewToolResultErrorf(
				"workflow run %s did not finish within %s and is still running; "+
					"read its status with: kubectl -n %s get workflowrun %s",
				run.Name, s.callTimeout, run.Namespace, run.Name), nil
		case <-ticker.C:
		}
	}
}

// succeededResult renders a finished run's outputs.
func succeededResult(run *ottoflowv1alpha1.WorkflowRun) (*mcp.CallToolResult, error) {
	outputs := make(map[string]any, len(run.Status.Outputs))
	for name, value := range run.Status.Outputs {
		outputs[name] = decodeOutput(value.Raw)
	}

	result, err := mcp.NewToolResultJSON(map[string]any{
		"workflowRun": run.Name,
		"namespace":   run.Namespace,
		"outputs":     outputs,
	})
	if err != nil {
		return nil, fmt.Errorf("rendering outputs of %s: %w", run.Name, err)
	}
	return result, nil
}

// decodeOutput turns a stored output back into a JSON value, so a tool result
// carries the structure the workflow produced rather than a string holding
// JSON. Bytes that do not parse are surfaced as text instead of dropped.
func decodeOutput(raw []byte) any {
	if len(raw) == 0 {
		return nil
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return string(raw)
	}
	return decoded
}
