/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/workflow/token"
)

func newTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(ottoflowv1alpha1.AddToScheme(s))
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	return s
}

func newTestWorkflowRun(name string) *ottoflowv1alpha1.WorkflowRun {
	return &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: ottoflowv1alpha1.WorkflowRunSpec{
			WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf"},
		},
	}
}

// TestExecuteWaitForCallback_NewToken verifies the new-token path:
// - no PendingCallback on the WorkflowRun
// - should return ErrAwaitingCallback
// - should set PendingCallback on the in-memory WorkflowRun
func TestExecuteWaitForCallback_NewToken(t *testing.T) {
	scheme := newTestScheme()
	wr := newTestWorkflowRun("run-1")
	k8s := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(wr).
		WithObjects(wr).Build()

	exec, err := NewWorkflowExecutor(k8s, wr)
	if err != nil {
		t.Fatalf("NewWorkflowExecutor: %v", err)
	}

	step := ottoflowv1alpha1.Step{
		Name: "approveStep",
		WaitForCallback: &ottoflowv1alpha1.WaitForCallbackStep{
			Timeout:       "1h",
			CallbackRef:   "approve",
			FailurePolicy: "Fail",
		},
	}

	ctx := context.Background()
	_, execErr := exec.executeWaitForCallback(ctx, wr, step)

	if !errors.Is(execErr, ErrAwaitingCallback) {
		t.Fatalf("expected ErrAwaitingCallback, got: %v", execErr)
	}

	if wr.Status.PendingCallback == nil {
		t.Fatal("expected PendingCallback to be set in-memory")
	}
	cb := wr.Status.PendingCallback
	if cb.StepName != "approveStep" {
		t.Errorf("StepName = %q, want %q", cb.StepName, "approveStep")
	}
	if cb.TokenHash == "" {
		t.Error("TokenHash is empty")
	}
	if cb.ExpiresAt <= time.Now().Unix() {
		t.Error("ExpiresAt should be in the future")
	}
	if cb.CreatedAt <= 0 {
		t.Error("CreatedAt should be positive")
	}
}

// TestExecuteWaitForCallback_RecoveryWithOutputs verifies the recovery path when
// outputs are already set (callback was received).
func TestExecuteWaitForCallback_RecoveryWithOutputs(t *testing.T) {
	scheme := newTestScheme()
	wr := newTestWorkflowRun("run-2")
	wr.Status.PendingCallback = &ottoflowv1alpha1.CallbackState{
		TokenHash: token.HashToken("cb_" + "a1b2c3d4" + "a1b2c3d4" + "a1b2c3d4" + "a1b2c3d4"),
		StepName:  "approveStep",
		ExpiresAt: time.Now().Add(1 * time.Hour).Unix(),
		Outputs: apiextensionsv1.JSON{
			Raw: []byte(`{"approved":true,"comment":"LGTM"}`),
		},
	}

	k8s := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(wr).
		WithObjects(wr).Build()

	exec, err := NewWorkflowExecutor(k8s, wr)
	if err != nil {
		t.Fatalf("NewWorkflowExecutor: %v", err)
	}

	step := ottoflowv1alpha1.Step{
		Name: "approveStep",
		WaitForCallback: &ottoflowv1alpha1.WaitForCallbackStep{
			Timeout: "1h",
		},
	}

	ctx := context.Background()
	if err := exec.GetContextManager().InitializeContext(ctx, &ottoflowv1alpha1.Workflow{}, nil); err != nil {
		t.Fatalf("InitializeContext: %v", err)
	}
	outputs, err := exec.executeWaitForCallback(ctx, wr, step)
	if err != nil {
		t.Fatalf("expected nil error (outputs received), got: %v", err)
	}
	if outputs == nil {
		t.Fatal("expected outputs map to be non-nil")
	}
	approved, ok := outputs["approved"].(bool)
	if !ok || !approved {
		t.Errorf("expected outputs[approved]=true, got %v", outputs["approved"])
	}
	cd, err := exec.GetContextManager().ReadContext(ctx)
	if err != nil {
		t.Fatalf("ReadContext: %v", err)
	}
	vars := cd["variables"].(map[string]interface{})
	if vars["approved"] != true || vars["comment"] != "LGTM" {
		t.Errorf("variables = %v, want approved=true comment=LGTM", vars)
	}
	// PendingCallback should be cleared
	if wr.Status.PendingCallback != nil {
		t.Error("expected PendingCallback to be cleared after consuming outputs")
	}
}

// TestExecuteWaitForCallback_Timeout_Fail verifies that an expired callback with FailurePolicyFail
// returns a non-ErrAwaitingCallback error.
func TestExecuteWaitForCallback_Timeout_Fail(t *testing.T) {
	scheme := newTestScheme()
	wr := newTestWorkflowRun("run-3")
	wr.Status.PendingCallback = &ottoflowv1alpha1.CallbackState{
		TokenHash: token.HashToken("cb_" + "a1b2c3d4" + "a1b2c3d4" + "a1b2c3d4" + "a1b2c3d4"),
		StepName:  "approveStep",
		ExpiresAt: time.Now().Add(-1 * time.Hour).Unix(), // expired
	}

	k8s := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(wr).
		WithObjects(wr).Build()

	exec, err := NewWorkflowExecutor(k8s, wr)
	if err != nil {
		t.Fatalf("NewWorkflowExecutor: %v", err)
	}

	step := ottoflowv1alpha1.Step{
		Name: "approveStep",
		WaitForCallback: &ottoflowv1alpha1.WaitForCallbackStep{
			Timeout:       "1h",
			FailurePolicy: ottoflowv1alpha1.FailurePolicyFail,
		},
	}

	ctx := context.Background()
	_, err = exec.executeWaitForCallback(ctx, wr, step)
	if err == nil {
		t.Fatal("expected error for timeout with FailurePolicyFail")
	}
	if errors.Is(err, ErrAwaitingCallback) {
		t.Error("ErrAwaitingCallback should not be returned on timeout")
	}
	if wr.Status.PendingCallback != nil {
		t.Error("PendingCallback should be cleared after timeout")
	}
}

// TestExecuteWaitForCallback_Timeout_Continue verifies that an expired callback with
// FailurePolicyContinue returns empty outputs and no error.
func TestExecuteWaitForCallback_Timeout_Continue(t *testing.T) {
	scheme := newTestScheme()
	wr := newTestWorkflowRun("run-4")
	wr.Status.PendingCallback = &ottoflowv1alpha1.CallbackState{
		TokenHash: token.HashToken("cb_" + "a1b2c3d4" + "a1b2c3d4" + "a1b2c3d4" + "a1b2c3d4"),
		StepName:  "approveStep",
		ExpiresAt: time.Now().Add(-5 * time.Minute).Unix(), // expired
	}

	k8s := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(wr).
		WithObjects(wr).Build()

	exec, err := NewWorkflowExecutor(k8s, wr)
	if err != nil {
		t.Fatalf("NewWorkflowExecutor: %v", err)
	}

	step := ottoflowv1alpha1.Step{
		Name: "approveStep",
		WaitForCallback: &ottoflowv1alpha1.WaitForCallbackStep{
			Timeout:       "1h",
			FailurePolicy: ottoflowv1alpha1.FailurePolicyContinue,
		},
	}

	ctx := context.Background()
	outputs, err := exec.executeWaitForCallback(ctx, wr, step)
	if err != nil {
		t.Fatalf("FailurePolicyContinue: expected nil error, got: %v", err)
	}
	if outputs == nil {
		t.Error("expected empty outputs map, not nil")
	}
}

// TestErrAwaitingCallback_IsCheck verifies error wrapping works correctly.
func TestErrAwaitingCallback_IsCheck(t *testing.T) {
	wrapped := errors.Join(ErrAwaitingCallback, errors.New("details"))
	if !errors.Is(wrapped, ErrAwaitingCallback) {
		t.Error("errors.Is should find ErrAwaitingCallback in wrapped error")
	}
	// A plain string error should NOT match ErrAwaitingCallback (documents expected behavior)
	fmtErr := errors.New("waiting")
	if errors.Is(fmtErr, ErrAwaitingCallback) {
		t.Error("plain string error should not match ErrAwaitingCallback")
	}
}

// TestExecuteWorkflow_WaitForCallbackOutputsReachDownstream verifies that declared
// callback fields reach downstream CEL as variables.<key> and undeclared ones do not.
func TestExecuteWorkflow_WaitForCallbackOutputsReachDownstream(t *testing.T) {
	wr := newTestWorkflowRun("run-cb")
	wr.Status.PendingCallback = &ottoflowv1alpha1.CallbackState{
		StepName:  "awaitApproval",
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
		Outputs:   apiextensionsv1.JSON{Raw: []byte(`{"approved":true,"reviewer":"alice","injected":"x"}`)},
	}
	k8s := fake.NewClientBuilder().WithScheme(newTestScheme()).
		WithStatusSubresource(wr).
		WithObjects(wr).Build()
	exec, err := NewWorkflowExecutor(k8s, wr)
	if err != nil {
		t.Fatalf("NewWorkflowExecutor: %v", err)
	}
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec: ottoflowv1alpha1.WorkflowSpec{Steps: []ottoflowv1alpha1.Step{
			{Name: "awaitApproval", WaitForCallback: &ottoflowv1alpha1.WaitForCallbackStep{
				Timeout:      "1h",
				OutputSchema: &apiextensionsv1.JSON{Raw: []byte(`{"type":"object","properties":{"approved":{"type":"boolean"},"reviewer":{"type":"string"}}}`)},
			}},
			{Name: "postApproval", DependsOn: []string{"awaitApproval"}, Outputs: []ottoflowv1alpha1.Output{
				{Name: "decision", Expression: `variables.approved ? "go" : "stop"`},
				{Name: "who", Expression: `variables.reviewer`},
			}},
		}},
	}

	ctx := context.Background()
	if err := exec.ExecuteWorkflow(ctx, wf, wr); err != nil {
		t.Fatalf("ExecuteWorkflow: %v", err)
	}
	if wr.Status.Phase != ottoflowv1alpha1.WorkflowRunPhaseSucceeded {
		t.Fatalf("phase = %s, want Succeeded", wr.Status.Phase)
	}
	cd, err := exec.GetContextManager().ReadContext(ctx)
	if err != nil {
		t.Fatalf("ReadContext: %v", err)
	}
	vars := cd["variables"].(map[string]interface{})
	if vars["decision"] != "go" || vars["who"] != "alice" {
		t.Errorf("variables = %v, want decision=go who=alice", vars)
	}
	if _, ok := vars["injected"]; ok {
		t.Errorf("undeclared key injected reached variables: %v", vars)
	}
	if wr.Status.PendingCallback != nil {
		t.Error("expected PendingCallback to be cleared")
	}
}
