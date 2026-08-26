/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package a2a

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

// newFakeClient builds a controller-runtime fake client seeded with objs, scoped to the
// ottoflow scheme. Used instead of a real cluster throughout this file.
func newFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	utilruntime.Must(ottoflowv1alpha1.AddToScheme(scheme))
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// eventKind extracts the "kind" discriminator from one of the A2A event structs, the
// field kagent's UI type-switches on.
func eventKind(e any) string {
	switch v := e.(type) {
	case taskEvent:
		return v.Kind
	case statusUpdateEvent:
		return v.Kind
	case artifactUpdateEvent:
		return v.Kind
	default:
		return ""
	}
}

// --- agent card ---------------------------------------------------------------------

func TestAgentCardServedAtWellKnownPath(t *testing.T) {
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "greeter"},
	}
	s := NewServer(newFakeClient(t, wf), "greeter", "default", "http://example.test/", 0)

	mux := http.NewServeMux()
	s.Register(mux)

	req := httptest.NewRequest(http.MethodGet, agentCardPath, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200; body=%s", agentCardPath, rec.Code, rec.Body.String())
	}

	var card map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &card); err != nil {
		t.Fatalf("decoding agent card: %v", err)
	}

	if got := card["protocolVersion"]; got != "0.3" {
		t.Errorf("protocolVersion = %v, want %q", got, "0.3")
	}
	if got := card["preferredTransport"]; got != "JSONRPC" {
		t.Errorf("preferredTransport = %v, want %q", got, "JSONRPC")
	}
	caps, _ := card["capabilities"].(map[string]any)
	if streaming, _ := caps["streaming"].(bool); !streaming {
		t.Errorf("capabilities.streaming = %v, want true", caps["streaming"])
	}
	modes, _ := card["defaultInputModes"].([]any)
	if len(modes) != 1 || modes[0] != "text" {
		t.Errorf("defaultInputModes = %v, want [text]", modes)
	}
	skills, _ := card["skills"].([]any)
	if len(skills) != 1 {
		t.Errorf("skills len = %d, want 1", len(skills))
	}

	// Nothing else answers this path: a GET to "/" falls through to the RPC handler,
	// which only accepts POST — confirming the card is served at exactly agentCardPath.
	rpcReq := httptest.NewRequest(http.MethodGet, "/", nil)
	rpcRec := httptest.NewRecorder()
	mux.ServeHTTP(rpcRec, rpcReq)
	if rpcRec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET / status = %d, want 405 (RPC handler, not the card)", rpcRec.Code)
	}
}

// --- message/stream event sequence ---------------------------------------------------

func TestStreamEventSequenceSucceeded(t *testing.T) {
	run := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", UID: types.UID("uid-1")},
		Status: ottoflowv1alpha1.WorkflowRunStatus{
			Phase:   ottoflowv1alpha1.WorkflowRunPhaseSucceeded,
			Outputs: map[string]apiextensionsv1.JSON{"greeting": j(`"hello x"`)},
		},
	}
	taskID, contextID := run.Name, string(run.UID)

	submitted := buildTaskSubmittedEvent(taskID, contextID)
	working := buildWorkingEvent(taskID, contextID)
	artifactEvt, statusEvt := buildTerminalEvents(taskID, contextID, run)

	// Order and kind discriminators, in the exact sequence handleStream sends them.
	events := []any{submitted, working, artifactEvt, statusEvt}
	wantKinds := []string{kindTask, kindStatusUpdate, kindArtifactUpdate, kindStatusUpdate}
	for i, want := range wantKinds {
		if got := eventKind(events[i]); got != want {
			t.Errorf("event[%d] kind = %q, want %q", i, got, want)
		}
	}

	if submitted.Status.State != stateSubmitted {
		t.Errorf("submitted.Status.State = %q, want %q", submitted.Status.State, stateSubmitted)
	}
	if working.Status.State != stateWorking {
		t.Errorf("working.Status.State = %q, want %q", working.Status.State, stateWorking)
	}
	if working.Final {
		t.Errorf("working.Final = true, want false")
	}
	if !artifactEvt.LastChunk {
		t.Errorf("artifactEvt.LastChunk = false, want true")
	}
	if len(artifactEvt.Artifact.Parts) != 1 {
		t.Fatalf("artifactEvt.Artifact.Parts = %+v, want exactly one part", artifactEvt.Artifact.Parts)
	}
	if got := artifactEvt.Artifact.Parts[0]; got.Kind != "text" || got.Text != "hello x" {
		t.Errorf("artifactEvt.Artifact.Parts[0] = %+v, want {text hello x}", got)
	}
	if statusEvt.Status.State != stateCompleted {
		t.Errorf("statusEvt.Status.State = %q, want %q", statusEvt.Status.State, stateCompleted)
	}
	if !statusEvt.Final {
		t.Errorf("statusEvt.Final = false, want true")
	}
}

func TestStreamEventSequenceFailed(t *testing.T) {
	run := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-2", UID: types.UID("uid-2")},
		Status: ottoflowv1alpha1.WorkflowRunStatus{
			Phase:   ottoflowv1alpha1.WorkflowRunPhaseFailed,
			Message: "boom",
			StepStatuses: map[string]ottoflowv1alpha1.StepStatus{
				"fetchWidget": {Phase: ottoflowv1alpha1.StepPhaseFailed, Error: "widget not found"},
			},
		},
	}
	taskID, contextID := run.Name, string(run.UID)

	artifactEvt, statusEvt := buildTerminalEvents(taskID, contextID, run)

	if got := eventKind(statusEvt); got != kindStatusUpdate {
		t.Errorf("statusEvt kind = %q, want %q", got, kindStatusUpdate)
	}
	if statusEvt.Status.State != stateFailed {
		t.Errorf("statusEvt.Status.State = %q, want %q", statusEvt.Status.State, stateFailed)
	}
	if !statusEvt.Final {
		t.Errorf("statusEvt.Final = false, want true")
	}

	if len(artifactEvt.Artifact.Parts) != 1 {
		t.Fatalf("artifactEvt.Artifact.Parts = %+v, want exactly one part", artifactEvt.Artifact.Parts)
	}
	text := artifactEvt.Artifact.Parts[0].Text
	if !strings.Contains(text, "boom") {
		t.Errorf("artifact text = %q, want it to surface the run message %q", text, "boom")
	}
	if !strings.Contains(text, "widget not found") {
		t.Errorf("artifact text = %q, want it to surface the step error %q", text, "widget not found")
	}
}

// --- input mapping --------------------------------------------------------------------

func TestCreateRunInputMapping(t *testing.T) {
	newWF := func() *ottoflowv1alpha1.Workflow {
		return &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "greeter", UID: types.UID("wf-uid")},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Inputs: []ottoflowv1alpha1.Input{{Name: "prompt"}},
				Steps:  []ottoflowv1alpha1.Step{{Name: "noop"}},
				Expose: &ottoflowv1alpha1.ExposeSpec{Kagent: &ottoflowv1alpha1.KagentExposeSpec{}},
			},
		}
	}

	t.Run("non-empty text maps to first input", func(t *testing.T) {
		s := NewServer(newFakeClient(t, newWF()), "greeter", "default", "", 0)
		run, err := s.createRun(context.Background(), "hello there")
		if err != nil {
			t.Fatalf("createRun: %v", err)
		}
		if got := run.Spec.InputValues["prompt"]; got != "hello there" {
			t.Errorf("InputValues[prompt] = %q, want %q", got, "hello there")
		}
	})

	t.Run("empty text leaves InputValues unset so the default applies", func(t *testing.T) {
		s := NewServer(newFakeClient(t, newWF()), "greeter", "default", "", 0)
		run, err := s.createRun(context.Background(), "")
		if err != nil {
			t.Fatalf("createRun: %v", err)
		}
		if v, ok := run.Spec.InputValues["prompt"]; ok {
			t.Errorf("InputValues[prompt] = %q, want unset", v)
		}
	})

	t.Run("run is owned by the workflow so it is garbage-collected", func(t *testing.T) {
		s := NewServer(newFakeClient(t, newWF()), "greeter", "default", "", 0)
		run, err := s.createRun(context.Background(), "hi")
		if err != nil {
			t.Fatalf("createRun: %v", err)
		}
		owners := run.GetOwnerReferences()
		if len(owners) != 1 || owners[0].Kind != "Workflow" || owners[0].UID != types.UID("wf-uid") {
			t.Fatalf("owner references = %+v, want a controller ref to Workflow wf-uid", owners)
		}
		if owners[0].Controller == nil || !*owners[0].Controller {
			t.Errorf("owner ref Controller = %v, want true", owners[0].Controller)
		}
	})

	t.Run("run is refused once the workflow opts out", func(t *testing.T) {
		wf := newWF()
		wf.Spec.Expose = nil // opted out between reconcile and this call
		s := NewServer(newFakeClient(t, wf), "greeter", "default", "", 0)
		if _, err := s.createRun(context.Background(), "hi"); err == nil {
			t.Fatal("createRun succeeded for an opted-out workflow, want an error")
		}
	})

	t.Run("run is refused at the concurrency limit", func(t *testing.T) {
		wf := newWF()
		one := int32(1)
		wf.Spec.Run = &ottoflowv1alpha1.RunPolicy{MaxConcurrentRuns: &one}
		active := &ottoflowv1alpha1.WorkflowRun{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default", Name: "greeter-running",
				Labels: map[string]string{"ottoflow.nirmata.io/workflow": "greeter"},
			},
			Status: ottoflowv1alpha1.WorkflowRunStatus{Phase: ottoflowv1alpha1.WorkflowRunPhaseRunning},
		}
		s := NewServer(newFakeClient(t, wf, active), "greeter", "default", "", 0)
		if _, err := s.createRun(context.Background(), "hi"); err == nil {
			t.Fatal("createRun succeeded at the concurrency limit, want an error")
		}
	})
}

// --- agent card metadata --------------------------------------------------------------

func TestBuildCardUsesExposeMetadata(t *testing.T) {
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "greeter"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Expose: &ottoflowv1alpha1.ExposeSpec{Kagent: &ottoflowv1alpha1.KagentExposeSpec{
				DisplayName: "Greeter",
				Description: "Says hello",
				Tags:        []string{"demo"},
				Examples:    []string{"say hi"},
			}},
		},
	}
	card := BuildCard(wf, "http://example.test/")
	if card.Name != "Greeter" {
		t.Errorf("card.Name = %q, want the displayName %q", card.Name, "Greeter")
	}
	if card.Description != "Says hello" {
		t.Errorf("card.Description = %q, want the expose description", card.Description)
	}
	if len(card.Skills) != 1 || card.Skills[0].ID != "greeter" {
		t.Fatalf("skills = %+v, want one skill keyed by the workflow name", card.Skills)
	}
	if got := card.Skills[0].Tags; len(got) != 1 || got[0] != "demo" {
		t.Errorf("skill.Tags = %v, want [demo]", got)
	}
	if got := card.Skills[0].Examples; len(got) != 1 || got[0] != "say hi" {
		t.Errorf("skill.Examples = %v, want [say hi]", got)
	}
}

// --- deadline behavior ----------------------------------------------------------------

func TestStillRunningEventsAreNotFailed(t *testing.T) {
	artifactEvt, statusEvt := buildStillRunningEvents("run-9", "ctx-9", "default", defaultRunTimeout)
	if statusEvt.Status.State == stateFailed {
		t.Errorf("deadline status state = %q, want anything but %q", statusEvt.Status.State, stateFailed)
	}
	if !statusEvt.Final {
		t.Error("deadline status Final = false, want true to close the stream")
	}
	text := artifactEvt.Artifact.Parts[0].Text
	if !strings.Contains(text, "run-9") || !strings.Contains(text, "still executing") {
		t.Errorf("deadline artifact text = %q, want it to name the run and say it is still executing", text)
	}
}

// --- SSE envelope -----------------------------------------------------------------------

func TestWriteSSEEnvelope(t *testing.T) {
	s := &Server{}
	rec := httptest.NewRecorder()
	evt := buildTaskSubmittedEvent("task-1", "ctx-1")

	s.writeSSE(rec, rec, json.RawMessage("7"), evt)

	body := rec.Body.String()
	const prefix, suffix = "data: ", "\n\n"
	if !strings.HasPrefix(body, prefix) || !strings.HasSuffix(body, suffix) {
		t.Fatalf("SSE line = %q, want form %q<json>%q", body, prefix, suffix)
	}
	payload := strings.TrimSuffix(strings.TrimPrefix(body, prefix), suffix)

	var env struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		t.Fatalf("decoding envelope: %v; payload=%s", err, payload)
	}
	if env.JSONRPC != "2.0" {
		t.Errorf("jsonrpc = %q, want %q", env.JSONRPC, "2.0")
	}
	if string(env.ID) != "7" {
		t.Errorf("id = %s, want echoed id 7", env.ID)
	}

	var gotResult taskEvent
	if err := json.Unmarshal(env.Result, &gotResult); err != nil {
		t.Fatalf("decoding result: %v", err)
	}
	if gotResult != evt {
		t.Errorf("result = %+v, want %+v", gotResult, evt)
	}
}
