/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"context"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/agent"
)

// cancelOnGet models SIGTERM landing during a CEL resource.Get: it cancels the run context and
// fails the read the way a real client does once its context is done.
type cancelOnGet struct {
	client.Client
	cancel context.CancelFunc
}

func (c cancelOnGet) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	c.cancel()
	return ctx.Err()
}

// A SIGTERM that cancels workflow-output evaluation must not record the run as Succeeded:
// the output would be missing from a run the controller then never retries.
func TestOutputEvalUnderCancelIsNotSuccess(t *testing.T) {
	const getExpr = `resource.Get("v1", "configmaps", "default", "cm").metadata.name`
	for name, out := range map[string]ottoflowv1alpha1.Output{
		"expression": {Name: "o", Expression: getExpr},
		// EvaluateOutputValue falls back to the literal string on an evaluation error.
		"value": {Name: "o", Value: &apiextensionsv1.JSON{Raw: []byte(`"` + strings.ReplaceAll(getExpr, `"`, `\"`) + `"`)}},
	} {
		t.Run(name, func(t *testing.T) { outputEvalUnderCancel(t, out) })
	}
}

func outputEvalUnderCancel(t *testing.T, out ottoflowv1alpha1.Output) {
	t.Helper()
	scheme := runtime.NewScheme()
	utilruntime.Must(ottoflowv1alpha1.AddToScheme(scheme))
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := cancelOnGet{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), cancel: cancel}
	run := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "default"},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf"}},
	}
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps:   []ottoflowv1alpha1.Step{{Name: "s", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: "1"}}}},
			Outputs: []ottoflowv1alpha1.Output{out},
		},
	}
	exec, err := NewWorkflowExecutorWithAgentExecutor(c, nil, nil, nil, run, agent.NewMockAgentExecutor(), false, 0, 5, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := exec.contextManager.InitializeContext(ctx, wf, run.Spec.InputValues); err != nil {
		t.Fatal(err)
	}
	err = exec.ExecuteWorkflow(ctx, wf, run)
	t.Logf("err=%v ctxErr=%v phase=%s outputs=%d", err, ctx.Err(), run.Status.Phase, len(run.Status.Outputs))
	if ctx.Err() == nil {
		t.Fatal("probe never cancelled the context: the output expression did not reach resource.Get")
	}
	if err == nil || run.Status.Phase == ottoflowv1alpha1.WorkflowRunPhaseSucceeded {
		t.Errorf("cancel during output evaluation reported success: err=%v phase=%s", err, run.Status.Phase)
	}
}

// A cancelled callback patch must report the failure: swallowing it lets the runner exit 0
// with PendingCallback only in memory, so the controller has nothing to resume from.
func TestPendingCallbackPatchUnderCancelReportsError(t *testing.T) {
	scheme := runtime.NewScheme()
	utilruntime.Must(ottoflowv1alpha1.AddToScheme(scheme))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := cancelOnGet{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), cancel: cancel}
	run := &ottoflowv1alpha1.WorkflowRun{ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "default"}}
	cb := &ottoflowv1alpha1.CallbackState{TokenHash: "h", StepName: "s"}
	if err := setPendingCallbackPatch(ctx, c, run, cb); err == nil {
		t.Error("setPendingCallbackPatch returned nil though its write never reached the server")
	}
	if err := clearPendingCallbackPatch(ctx, c, run); err == nil {
		t.Error("clearPendingCallbackPatch returned nil though its write never reached the server")
	}
}
