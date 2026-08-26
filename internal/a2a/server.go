/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

const (
	pollInterval = 1 * time.Second
	// defaultRunTimeout: 10m headroom, since composed workflows chain several
	// sequential agent calls and can exceed the previous 2m budget. Overridable
	// per deployment (A2A_RUN_TIMEOUT) since a workflow's real bound is workflow-specific.
	defaultRunTimeout = 10 * time.Minute
	heartbeatEvery    = 5 * time.Second
	maxBodyBytes      = 1 << 20

	methodStream = "message/stream"
	methodSend   = "message/send"

	// A2A event kind discriminators (match the @a2a-js/sdk type guards the kagent UI uses).
	kindTask           = "task"
	kindStatusUpdate   = "status-update"
	kindArtifactUpdate = "artifact-update"

	// A2A task states (a subset; consistent with internal/workflow/executor/a2a_client.go).
	stateSubmitted = "submitted"
	stateWorking   = "working"
	stateCompleted = "completed"
	stateFailed    = "failed"
)

// Server exposes one OttoFlow Workflow as an A2A agent over HTTP.
type Server struct {
	client     client.Client
	wfName     string
	wfNS       string
	publicURL  string
	runTimeout time.Duration
}

// NewServer builds a Server for the named Workflow. A non-positive runTimeout falls back to
// defaultRunTimeout.
func NewServer(c client.Client, wfName, wfNS, publicURL string, runTimeout time.Duration) *Server {
	if runTimeout <= 0 {
		runTimeout = defaultRunTimeout
	}
	return &Server{client: c, wfName: wfName, wfNS: wfNS, publicURL: publicURL, runTimeout: runTimeout}
}

// errRunStillExecuting signals pollToTerminal gave up at the deadline while the WorkflowRun is
// still executing (as opposed to a real polling error). Callers report this honestly instead of
// as a failed task: the run continues and may still succeed.
var errRunStillExecuting = errors.New("workflowrun still executing at deadline")

// Register wires the A2A routes onto mux.
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc(agentCardPath, s.handleCard)
	mux.HandleFunc("/", s.handleRPC)
}

// agentCardPath matches a2aAgentCardPath in internal/workflow/executor/a2a_client.go.
const agentCardPath = "/.well-known/agent-card.json"

// --- JSON-RPC envelopes ------------------------------------------------------

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  rpcParams       `json:"params"`
}

type rpcParams struct {
	Message struct {
		Parts []struct {
			Kind string `json:"kind"`
			Text string `json:"text"`
		} `json:"parts"`
	} `json:"message"`
}

// rpcEnvelope is the JSON-RPC 2.0 result envelope; each SSE data line is one of these.
type rpcEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result"`
}

// rpcErrorEnvelope is the JSON-RPC 2.0 error envelope.
type rpcErrorEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Error   rpcError        `json:"error"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// --- A2A event shapes --------------------------------------------------------

type textPart struct {
	Kind string `json:"kind"` // always "text"
	Text string `json:"text"`
}

type taskStatus struct {
	State     string `json:"state"`
	Timestamp string `json:"timestamp,omitempty"`
}

type artifact struct {
	ArtifactID string     `json:"artifactId"`
	Name       string     `json:"name,omitempty"`
	Parts      []textPart `json:"parts"`
}

// taskEvent is the initial Task (kind: "task").
type taskEvent struct {
	ID        string     `json:"id"`
	ContextID string     `json:"contextId"`
	Kind      string     `json:"kind"`
	Status    taskStatus `json:"status"`
}

// statusUpdateEvent is a TaskStatusUpdateEvent (kind: "status-update"); Final=true ends the stream.
type statusUpdateEvent struct {
	TaskID    string     `json:"taskId"`
	ContextID string     `json:"contextId"`
	Kind      string     `json:"kind"`
	Status    taskStatus `json:"status"`
	Final     bool       `json:"final"`
}

// artifactUpdateEvent is a TaskArtifactUpdateEvent (kind: "artifact-update"); LastChunk=true makes the UI render it.
type artifactUpdateEvent struct {
	TaskID    string   `json:"taskId"`
	ContextID string   `json:"contextId"`
	Kind      string   `json:"kind"`
	Artifact  artifact `json:"artifact"`
	Append    bool     `json:"append"`
	LastChunk bool     `json:"lastChunk"`
}

// taskResult is the terminal Task returned by the non-streaming message/send.
type taskResult struct {
	ID        string     `json:"id"`
	ContextID string     `json:"contextId"`
	Kind      string     `json:"kind"`
	Status    taskStatus `json:"status"`
	Artifacts []artifact `json:"artifacts"`
}

// --- handlers ----------------------------------------------------------------

func (s *Server) handleCard(w http.ResponseWriter, r *http.Request) {
	wf, err := s.getWorkflow(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(BuildCard(wf, s.publicURL)); err != nil {
		klog.Errorf("encoding agent card: %v", err)
	}
}

func (s *Server) handleRPC(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		http.Error(w, "reading request body", http.StatusBadRequest)
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "parsing JSON-RPC request", http.StatusBadRequest)
		return
	}
	if len(req.ID) == 0 {
		req.ID = json.RawMessage("null")
	}

	switch req.Method {
	case methodStream:
		s.handleStream(w, r, &req)
	case methodSend:
		s.handleSend(w, r, &req)
	default:
		writeRPCError(w, req.ID, -32601, fmt.Sprintf("unsupported method %q", req.Method))
	}
}

// handleStream runs the workflow and streams A2A SSE events:
// task(submitted) -> status-update(working) [heartbeats] -> artifact-update(lastChunk) -> status-update(final).
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request, req *rpcRequest) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ctx := r.Context()

	run, err := s.createRun(ctx, promptText(req))
	if err != nil {
		writeRPCError(w, req.ID, -32603, err.Error())
		return
	}
	taskID := run.Name
	contextID := string(run.UID)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	send := func(result any) { s.writeSSE(w, flusher, req.ID, result) }
	working := func() { send(buildWorkingEvent(taskID, contextID)) }

	send(buildTaskSubmittedEvent(taskID, contextID))
	working()

	final, err := s.pollToTerminal(ctx, taskID, working)
	if err != nil {
		// A deadline hit is not a failure: the run continues. Emit an honest notice, not "failed".
		if errors.Is(err, errRunStillExecuting) {
			artifactEvt, statusEvt := buildStillRunningEvents(taskID, contextID, s.wfNS, s.runTimeout)
			send(artifactEvt)
			send(statusEvt)
			return
		}
		send(statusUpdateEvent{
			TaskID: taskID, ContextID: contextID, Kind: kindStatusUpdate,
			Status: taskStatus{State: stateFailed, Timestamp: now()}, Final: true,
		})
		return
	}

	artifactEvt, statusEvt := buildTerminalEvents(taskID, contextID, final)
	send(artifactEvt)
	send(statusEvt)
}

// handleSend runs the workflow to completion and returns a single terminal Task
// (non-streaming). Cheap fallback for A2A clients that don't stream.
func (s *Server) handleSend(w http.ResponseWriter, r *http.Request, req *rpcRequest) {
	ctx := r.Context()
	run, err := s.createRun(ctx, promptText(req))
	if err != nil {
		writeRPCError(w, req.ID, -32603, err.Error())
		return
	}
	final, err := s.pollToTerminal(ctx, run.Name, nil)
	if err != nil {
		// A deadline hit is not a failure: return an honest task naming the run, not an RPC error.
		if errors.Is(err, errRunStillExecuting) {
			w.Header().Set("Content-Type", "application/json")
			task := buildStillRunningTask(run.Name, string(run.UID), s.wfNS, s.runTimeout)
			if encErr := json.NewEncoder(w).Encode(rpcEnvelope{JSONRPC: "2.0", ID: req.ID, Result: task}); encErr != nil {
				klog.Errorf("encoding message/send response: %v", encErr)
			}
			return
		}
		writeRPCError(w, req.ID, -32603, err.Error())
		return
	}

	state, text, _ := resolveOutcome(final)

	task := taskResult{
		ID: run.Name, ContextID: string(run.UID), Kind: kindTask,
		Status:    taskStatus{State: state, Timestamp: now()},
		Artifacts: []artifact{{ArtifactID: run.Name + "-result", Name: "result", Parts: []textPart{{Kind: "text", Text: text}}}},
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(rpcEnvelope{JSONRPC: "2.0", ID: req.ID, Result: task}); err != nil {
		klog.Errorf("encoding message/send response: %v", err)
	}
}

// --- pure event builders ------------------------------------------------------
//
// These are the version-fragile part of the A2A wire contract: the exact event kinds,
// field names, and lastChunk/final flags that kagent's UI keys on. They take no I/O so
// the contract can be pinned by unit tests without a live client, goroutines, or timing
// (see server_test.go).

// buildTaskSubmittedEvent is the first event sent once a run is created.
func buildTaskSubmittedEvent(taskID, contextID string) taskEvent {
	return taskEvent{ID: taskID, ContextID: contextID, Kind: kindTask, Status: taskStatus{State: stateSubmitted, Timestamp: now()}}
}

// buildWorkingEvent is sent immediately after the task event, and again on every
// heartbeat while the run is still executing.
func buildWorkingEvent(taskID, contextID string) statusUpdateEvent {
	return statusUpdateEvent{
		TaskID: taskID, ContextID: contextID, Kind: kindStatusUpdate,
		Status: taskStatus{State: stateWorking, Timestamp: now()}, Final: false,
	}
}

// resolveOutcome maps a terminal WorkflowRun to the A2A task state, the rendered text
// for its sole artifact, and that artifact's name.
func resolveOutcome(run *ottoflowv1alpha1.WorkflowRun) (state, text, name string) {
	if run.Status.Phase != ottoflowv1alpha1.WorkflowRunPhaseSucceeded {
		return stateFailed, failureText(run), "error"
	}
	return stateCompleted, RenderOutputs(run.Status.Outputs), "result"
}

// buildTerminalEvents maps a terminal WorkflowRun (Succeeded or Failed) to the final
// artifact-update and status-update events sent once polling completes.
func buildTerminalEvents(taskID, contextID string, run *ottoflowv1alpha1.WorkflowRun) (artifactUpdateEvent, statusUpdateEvent) {
	state, text, name := resolveOutcome(run)

	artifactEvt := artifactUpdateEvent{
		TaskID: taskID, ContextID: contextID, Kind: kindArtifactUpdate,
		Artifact:  artifact{ArtifactID: taskID + "-" + name, Name: name, Parts: []textPart{{Kind: "text", Text: text}}},
		Append:    false,
		LastChunk: true,
	}
	statusEvt := statusUpdateEvent{
		TaskID: taskID, ContextID: contextID, Kind: kindStatusUpdate,
		Status: taskStatus{State: state, Timestamp: now()}, Final: true,
	}
	return artifactEvt, statusEvt
}

// stillRunningText is the human-facing notice for a run that outlived the serve-a2a deadline.
// It names the run and how to check it, and states plainly that the run was not canceled.
func stillRunningText(runName, ns string, d time.Duration) string {
	return fmt.Sprintf(
		"Workflow run %s did not finish within %s and is still executing; it was not canceled. "+
			"Check its status with: kubectl -n %s get workflowrun %s",
		runName, d, ns, runName)
}

// buildStillRunningEvents is the streaming terminal pair for a deadline hit: an artifact carrying
// the honest notice, and a final status that is NOT "failed" — the run continues and may succeed.
func buildStillRunningEvents(taskID, contextID, ns string, d time.Duration) (artifactUpdateEvent, statusUpdateEvent) {
	text := stillRunningText(taskID, ns, d)
	artifactEvt := artifactUpdateEvent{
		TaskID: taskID, ContextID: contextID, Kind: kindArtifactUpdate,
		Artifact:  artifact{ArtifactID: taskID + "-notice", Name: "notice", Parts: []textPart{{Kind: "text", Text: text}}},
		Append:    false,
		LastChunk: true,
	}
	statusEvt := statusUpdateEvent{
		TaskID: taskID, ContextID: contextID, Kind: kindStatusUpdate,
		Status: taskStatus{State: stateCompleted, Timestamp: now()}, Final: true,
	}
	return artifactEvt, statusEvt
}

// buildStillRunningTask is the non-streaming (message/send) equivalent of buildStillRunningEvents.
func buildStillRunningTask(runName, contextID, ns string, d time.Duration) taskResult {
	text := stillRunningText(runName, ns, d)
	return taskResult{
		ID: runName, ContextID: contextID, Kind: kindTask,
		Status:    taskStatus{State: stateCompleted, Timestamp: now()},
		Artifacts: []artifact{{ArtifactID: runName + "-notice", Name: "notice", Parts: []textPart{{Kind: "text", Text: text}}}},
	}
}

// --- workflow plumbing -------------------------------------------------------

func (s *Server) getWorkflow(ctx context.Context) (*ottoflowv1alpha1.Workflow, error) {
	var wf ottoflowv1alpha1.Workflow
	if err := s.client.Get(ctx, client.ObjectKey{Namespace: s.wfNS, Name: s.wfName}, &wf); err != nil {
		return nil, fmt.Errorf("getting workflow %s/%s: %w", s.wfNS, s.wfName, err)
	}
	return &wf, nil
}

// createRun creates a WorkflowRun for the target Workflow. Status is left empty; the
// WorkflowRun controller sets Phase=Pending and drives execution (see workflowrun_controller.go).
func (s *Server) createRun(ctx context.Context, text string) (*ottoflowv1alpha1.WorkflowRun, error) {
	wf, err := s.getWorkflow(ctx)
	if err != nil {
		return nil, err
	}

	// Re-check opt-in at call time. This BYO pod can outlive spec.expose.kagent being cleared
	// (teardown races the reconciler), and getWorkflow only proves the Workflow still exists.
	// Mirrors the MCP tool server's re-check so opting out actually stops new runs.
	if !wf.Spec.Expose.IsKagentEnabled() {
		return nil, fmt.Errorf("workflow %s/%s is not exposed as an A2A agent", s.wfNS, wf.Name)
	}

	// Honor the same per-workflow concurrency cap cron/event/MCP-created runs honor. Without it
	// repeated A2A calls launch an unbounded number of runner Jobs.
	if rp := wf.Spec.Run; rp != nil && rp.MaxConcurrentRuns != nil && *rp.MaxConcurrentRuns > 0 {
		active, err := s.countActiveRuns(ctx, wf.Name)
		if err != nil {
			return nil, fmt.Errorf("counting active runs for %s/%s: %w", s.wfNS, wf.Name, err)
		}
		if active >= int(*rp.MaxConcurrentRuns) {
			return nil, fmt.Errorf("workflow %s/%s is at its concurrency limit (%d running)", s.wfNS, wf.Name, active)
		}
	}

	inputs := map[string]string{}
	// ponytail: single-input shortcut — the whole prompt text maps to the workflow's first
	// input. Empty text is left unset so the input's default applies. Workflows with more than
	// one required input are refused at reconcile time (see WorkflowExposureReconciler), so the
	// A2A call never reaches a workflow this mapping cannot satisfy.
	if text != "" && len(wf.Spec.Inputs) > 0 {
		inputs[wf.Spec.Inputs[0].Name] = text
	}

	run := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: wf.Name + "-a2a-",
			Namespace:    s.wfNS,
			// Own the run by the Workflow so deleting the Workflow garbage-collects its A2A runs,
			// matching the shared run builder used by cron/event/MCP triggers.
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: ottoflowv1alpha1.GroupVersion.String(),
				Kind:       "Workflow",
				Name:       wf.Name,
				UID:        wf.UID,
				Controller: &[]bool{true}[0],
			}},
			Labels: map[string]string{
				"ottoflow.nirmata.io/workflow":   wf.Name,
				"ottoflow.nirmata.io/created-by": "serve-a2a",
			},
		},
		Spec: ottoflowv1alpha1.WorkflowRunSpec{
			WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: wf.Name, Namespace: s.wfNS},
			InputValues: inputs,
			Execution:   wf.Spec.Execution.DeepCopy(),
		},
	}
	if err := s.client.Create(ctx, run); err != nil {
		return nil, fmt.Errorf("creating workflowrun: %w", err)
	}
	klog.Infof("created workflowrun %s/%s", run.Namespace, run.Name)
	return run, nil
}

// countActiveRuns counts Pending+Running WorkflowRuns for the workflow, matching
// countActiveWorkflowRuns in the controller. Requires the serve-a2a role to list WorkflowRuns.
func (s *Server) countActiveRuns(ctx context.Context, wfName string) (int, error) {
	var list ottoflowv1alpha1.WorkflowRunList
	if err := s.client.List(ctx, &list,
		client.InNamespace(s.wfNS),
		client.MatchingLabels{"ottoflow.nirmata.io/workflow": wfName},
	); err != nil {
		return 0, err
	}
	active := 0
	for i := range list.Items {
		switch list.Items[i].Status.Phase {
		case ottoflowv1alpha1.WorkflowRunPhasePending, ottoflowv1alpha1.WorkflowRunPhaseRunning:
			active++
		}
	}
	return active, nil
}

// pollToTerminal polls the WorkflowRun until it reaches Succeeded/Failed or the deadline
// elapses. onHeartbeat (if non-nil) is invoked at most every heartbeatEvery while running.
func (s *Server) pollToTerminal(ctx context.Context, name string, onHeartbeat func()) (*ottoflowv1alpha1.WorkflowRun, error) {
	ctx, cancel := context.WithTimeout(ctx, s.runTimeout)
	defer cancel()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	lastBeat := time.Now()

	for {
		select {
		case <-ctx.Done():
			// Our own deadline elapsed: the run is still executing, not failed. Callers surface
			// this honestly. A parent cancellation (client hung up) stays a plain error.
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, errRunStillExecuting
			}
			return nil, fmt.Errorf("waiting for workflowrun %s: %w", name, ctx.Err())
		case <-ticker.C:
		}

		var run ottoflowv1alpha1.WorkflowRun
		if err := s.client.Get(ctx, client.ObjectKey{Namespace: s.wfNS, Name: name}, &run); err != nil {
			return nil, fmt.Errorf("getting workflowrun %s: %w", name, err)
		}
		switch run.Status.Phase {
		case ottoflowv1alpha1.WorkflowRunPhaseSucceeded, ottoflowv1alpha1.WorkflowRunPhaseFailed:
			return &run, nil
		default:
			if onHeartbeat != nil && time.Since(lastBeat) >= heartbeatEvery {
				onHeartbeat()
				lastBeat = time.Now()
			}
		}
	}
}

// --- small helpers -----------------------------------------------------------

func promptText(req *rpcRequest) string {
	var b strings.Builder
	for _, p := range req.Params.Message.Parts {
		if p.Kind == "text" {
			b.WriteString(p.Text)
		}
	}
	return strings.TrimSpace(b.String())
}

func failureText(run *ottoflowv1alpha1.WorkflowRun) string {
	var parts []string
	if run.Status.Message != "" {
		parts = append(parts, run.Status.Message)
	}
	for name, ss := range run.Status.StepStatuses {
		if ss.Error != "" {
			parts = append(parts, fmt.Sprintf("%s: %s", name, ss.Error))
		}
	}
	if len(parts) == 0 {
		return "workflow failed"
	}
	return strings.Join(parts, "; ")
}

func (s *Server) writeSSE(w http.ResponseWriter, flusher http.Flusher, id json.RawMessage, result any) {
	b, err := json.Marshal(rpcEnvelope{JSONRPC: "2.0", ID: id, Result: result})
	if err != nil {
		klog.Errorf("marshaling SSE event: %v", err)
		return
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
		klog.Errorf("writing SSE event: %v", err)
		return
	}
	flusher.Flush()
}

func writeRPCError(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(rpcErrorEnvelope{
		JSONRPC: "2.0",
		ID:      id,
		Error:   rpcError{Code: code, Message: msg},
	}); err != nil {
		klog.Errorf("encoding JSON-RPC error: %v", err)
	}
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }
