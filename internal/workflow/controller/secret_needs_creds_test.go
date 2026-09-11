/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

// --- workflowNeedsAgentExecutor ---

func TestWorkflowNeedsAgentExecutor_NoSteps(t *testing.T) {
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "wf1"},
		Spec:       ottoflowv1alpha1.WorkflowSpec{Steps: []ottoflowv1alpha1.Step{{Name: "s1", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `"ok"`}}}}},
	}
	c := newSecretRefsTestClient(t)
	got, err := workflowNeedsAgentExecutor(context.Background(), c, wf, "ns1")
	if err != nil {
		t.Fatalf("workflowNeedsAgentExecutor: %v", err)
	}
	if got {
		t.Error("an expressions-only workflow must not need the agent-executor")
	}
}

func TestWorkflowNeedsAgentExecutor_TopLevelAgentRef(t *testing.T) {
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "wf1"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{{Name: "callAgent", AgentRef: &ottoflowv1alpha1.StepAgentRef{Name: "agent1"}}},
		},
	}
	c := newSecretRefsTestClient(t)
	got, err := workflowNeedsAgentExecutor(context.Background(), c, wf, "ns1")
	if err != nil {
		t.Fatalf("workflowNeedsAgentExecutor: %v", err)
	}
	if !got {
		t.Error("a top-level agentRef step must need the agent-executor")
	}
}

// TestWorkflowNeedsAgentExecutor_MCPToolCallOnly_False proves the distinction the predicate
// rests on: mcpToolCall connects straight to its MCPServer from the runner pod
// (executeMCPToolCall/mcp_executor.go) and never goes through agent-executor, so a workflow
// with only mcpToolCall steps must not wait on, or mount, the agent-executor CA.
func TestWorkflowNeedsAgentExecutor_MCPToolCallOnly_False(t *testing.T) {
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "wf1"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{{
				Name:        "callTool",
				MCPToolCall: &ottoflowv1alpha1.StepMCPToolCall{Server: "srv1", Tool: "doThing"},
			}},
		},
	}
	c := newSecretRefsTestClient(t)
	got, err := workflowNeedsAgentExecutor(context.Background(), c, wf, "ns1")
	if err != nil {
		t.Fatalf("workflowNeedsAgentExecutor: %v", err)
	}
	if got {
		t.Error("an mcpToolCall-only workflow must not need the agent-executor")
	}
}

func TestWorkflowNeedsAgentExecutor_ForEachInlineAgentRef(t *testing.T) {
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "wf1"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{
				{
					Name: "fanOut",
					ForEach: &ottoflowv1alpha1.StepForEach{
						Items: "[1,2]",
						Step:  &ottoflowv1alpha1.StepForEachStep{AgentRef: &ottoflowv1alpha1.StepAgentRef{Name: "agent1"}},
					},
				},
			},
		},
	}
	c := newSecretRefsTestClient(t)
	got, err := workflowNeedsAgentExecutor(context.Background(), c, wf, "ns1")
	if err != nil {
		t.Fatalf("workflowNeedsAgentExecutor: %v", err)
	}
	if !got {
		t.Error("an agentRef inside a forEach inline step must need the agent-executor")
	}
}

func TestWorkflowNeedsAgentExecutor_StepTemplateRefAgentRef(t *testing.T) {
	tpl := &ottoflowv1alpha1.StepTemplate{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "tpl1"},
		Spec: ottoflowv1alpha1.StepTemplateSpec{
			Step: ottoflowv1alpha1.StepTemplateStep{AgentRef: &ottoflowv1alpha1.StepAgentRef{Name: "agent1"}},
		},
	}
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "wf1"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{{Name: "useTpl", StepTemplateRef: &ottoflowv1alpha1.StepTemplateRef{Name: "tpl1"}}},
		},
	}
	c := newSecretRefsTestClient(t, tpl)
	got, err := workflowNeedsAgentExecutor(context.Background(), c, wf, "ns1")
	if err != nil {
		t.Fatalf("workflowNeedsAgentExecutor: %v", err)
	}
	if !got {
		t.Error("an agentRef reached via stepTemplateRef must need the agent-executor")
	}
}

func TestWorkflowNeedsAgentExecutor_WorkflowRefTwoLevelsAgentRef(t *testing.T) {
	subsub := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "subsub1"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{{Name: "deepCall", AgentRef: &ottoflowv1alpha1.StepAgentRef{Name: "agent1"}}},
		},
	}
	sub := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "sub1"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{{Name: "callSubSub", WorkflowRef: &ottoflowv1alpha1.StepWorkflowRef{Name: "subsub1"}}},
		},
	}
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "wf1"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{{Name: "callSub", WorkflowRef: &ottoflowv1alpha1.StepWorkflowRef{Name: "sub1"}}},
		},
	}
	c := newSecretRefsTestClient(t, sub, subsub)
	got, err := workflowNeedsAgentExecutor(context.Background(), c, wf, "ns1")
	if err != nil {
		t.Fatalf("workflowNeedsAgentExecutor: %v", err)
	}
	if !got {
		t.Error("an agentRef reached two workflowRef hops down must need the agent-executor")
	}
}

func TestWorkflowNeedsAgentExecutor_WorkflowRefCycleTerminates(t *testing.T) {
	wfA := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "wfA"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{{Name: "callB", WorkflowRef: &ottoflowv1alpha1.StepWorkflowRef{Name: "wfB"}}},
		},
	}
	wfB := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "wfB"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{{Name: "callBackToA", WorkflowRef: &ottoflowv1alpha1.StepWorkflowRef{Name: "wfA"}}},
		},
	}
	c := newSecretRefsTestClient(t, wfB)

	done := make(chan struct{})
	var got bool
	var err error
	go func() {
		got, err = workflowNeedsAgentExecutor(context.Background(), c, wfA, "ns1")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("workflowNeedsAgentExecutor did not terminate on an A->B->A workflowRef cycle")
	}
	if err != nil {
		t.Fatalf("workflowNeedsAgentExecutor: %v", err)
	}
	if got {
		t.Error("neither wfA nor wfB has an agentRef step; expected false")
	}
}

// --- workflowNeedsNirmataLLMCredentials: Secret RBAC preflight ---
//
// This matrix is what stops a "narrowed only fromStep" regression appearing silently:
// cases 2, 3 and 4 each cover one of the three fromLeafRefs routes, and case 10 pins that
// workflowNeedsAgentExecutor (a different predicate, which ignores the provider) is
// unaffected by the narrowing.

// Case 1: top-level AgentRef, openai -> false.
func TestWorkflowNeedsNirmataLLMCredentials_TopLevelOpenAI_False(t *testing.T) {
	agentCRD := &ottoflowv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "agent1"},
		Spec:       ottoflowv1alpha1.AgentSpec{Prompt: "x", ModelProvider: "openai"},
	}
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "wf1"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{{Name: "callAgent", AgentRef: &ottoflowv1alpha1.StepAgentRef{Name: "agent1"}}},
		},
	}
	c := newSecretRefsTestClient(t, agentCRD)
	got, err := workflowNeedsNirmataLLMCredentials(context.Background(), c, c, wf, "ns1")
	if err != nil {
		t.Fatalf("workflowNeedsNirmataLLMCredentials: %v", err)
	}
	if got {
		t.Error("a top-level agentRef to an openai Agent must not need a Nirmata token")
	}
}

// Case 2: ForEach-wrapped AgentRef, openai -> false (fromForEach -> fromLeafRefs).
func TestWorkflowNeedsNirmataLLMCredentials_ForEachInlineOpenAI_False(t *testing.T) {
	agentCRD := &ottoflowv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "agent1"},
		Spec:       ottoflowv1alpha1.AgentSpec{Prompt: "x", ModelProvider: "openai"},
	}
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "wf1"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{
				{
					Name: "fanOut",
					ForEach: &ottoflowv1alpha1.StepForEach{
						Items: "[1,2]",
						Step:  &ottoflowv1alpha1.StepForEachStep{AgentRef: &ottoflowv1alpha1.StepAgentRef{Name: "agent1"}},
					},
				},
			},
		},
	}
	c := newSecretRefsTestClient(t, agentCRD)
	got, err := workflowNeedsNirmataLLMCredentials(context.Background(), c, c, wf, "ns1")
	if err != nil {
		t.Fatalf("workflowNeedsNirmataLLMCredentials: %v", err)
	}
	if got {
		t.Error("a forEach-inline agentRef to an openai Agent must not need a Nirmata token")
	}
}

// Case 3: StepTemplate-wrapped AgentRef, openai -> false (fromStep -> fromLeafRefs).
func TestWorkflowNeedsNirmataLLMCredentials_StepTemplateOpenAI_False(t *testing.T) {
	agentCRD := &ottoflowv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "agent1"},
		Spec:       ottoflowv1alpha1.AgentSpec{Prompt: "x", ModelProvider: "openai"},
	}
	tpl := &ottoflowv1alpha1.StepTemplate{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "tpl1"},
		Spec: ottoflowv1alpha1.StepTemplateSpec{
			Step: ottoflowv1alpha1.StepTemplateStep{AgentRef: &ottoflowv1alpha1.StepAgentRef{Name: "agent1"}},
		},
	}
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "wf1"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{{Name: "useTpl", StepTemplateRef: &ottoflowv1alpha1.StepTemplateRef{Name: "tpl1"}}},
		},
	}
	c := newSecretRefsTestClient(t, tpl, agentCRD)
	got, err := workflowNeedsNirmataLLMCredentials(context.Background(), c, c, wf, "ns1")
	if err != nil {
		t.Fatalf("workflowNeedsNirmataLLMCredentials: %v", err)
	}
	if got {
		t.Error("a stepTemplateRef-reached agentRef to an openai Agent must not need a Nirmata token")
	}
}

// Case 4: ForEach + StepTemplateRef, openai -> false (fromForEach -> fromLeafRefs).
func TestWorkflowNeedsNirmataLLMCredentials_ForEachStepTemplateOpenAI_False(t *testing.T) {
	agentCRD := &ottoflowv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "agent1"},
		Spec:       ottoflowv1alpha1.AgentSpec{Prompt: "x", ModelProvider: "openai"},
	}
	tpl := &ottoflowv1alpha1.StepTemplate{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "tpl1"},
		Spec: ottoflowv1alpha1.StepTemplateSpec{
			Step: ottoflowv1alpha1.StepTemplateStep{AgentRef: &ottoflowv1alpha1.StepAgentRef{Name: "agent1"}},
		},
	}
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "wf1"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{
				{
					Name: "fanOut",
					ForEach: &ottoflowv1alpha1.StepForEach{
						Items:           "[1,2]",
						StepTemplateRef: &ottoflowv1alpha1.StepForEachTemplateRef{Name: "tpl1"},
					},
				},
			},
		},
	}
	c := newSecretRefsTestClient(t, tpl, agentCRD)
	got, err := workflowNeedsNirmataLLMCredentials(context.Background(), c, c, wf, "ns1")
	if err != nil {
		t.Fatalf("workflowNeedsNirmataLLMCredentials: %v", err)
	}
	if got {
		t.Error("a forEach stepTemplateRef-reached agentRef to an openai Agent must not need a Nirmata token")
	}
}

// Case 5: any shape, modelProvider "nirmata" -> true.
func TestWorkflowNeedsNirmataLLMCredentials_NirmataProvider_True(t *testing.T) {
	agentCRD := &ottoflowv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "agent1"},
		Spec:       ottoflowv1alpha1.AgentSpec{Prompt: "x", ModelProvider: "nirmata"},
	}
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "wf1"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{{Name: "callAgent", AgentRef: &ottoflowv1alpha1.StepAgentRef{Name: "agent1"}}},
		},
	}
	c := newSecretRefsTestClient(t, agentCRD)
	got, err := workflowNeedsNirmataLLMCredentials(context.Background(), c, c, wf, "ns1")
	if err != nil {
		t.Fatalf("workflowNeedsNirmataLLMCredentials: %v", err)
	}
	if !got {
		t.Error("a nirmata-provider agentRef must need a Nirmata token")
	}
}

// Case 6: any shape, modelProvider "" -> true (exec_handler.go coerces empty to nirmata).
func TestWorkflowNeedsNirmataLLMCredentials_EmptyProviderDefaultsToNirmata_True(t *testing.T) {
	agentCRD := &ottoflowv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "agent1"},
		Spec:       ottoflowv1alpha1.AgentSpec{Prompt: "x"}, // ModelProvider left empty, as a
		// pre-required-field legacy Agent object would be.
	}
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "wf1"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{{Name: "callAgent", AgentRef: &ottoflowv1alpha1.StepAgentRef{Name: "agent1"}}},
		},
	}
	c := newSecretRefsTestClient(t, agentCRD)
	got, err := workflowNeedsNirmataLLMCredentials(context.Background(), c, c, wf, "ns1")
	if err != nil {
		t.Fatalf("workflowNeedsNirmataLLMCredentials: %v", err)
	}
	if !got {
		t.Error("an empty modelProvider must be treated as nirmata, per exec_handler.go's own defaulting")
	}
}

// Case 7: mixed openai-then-nirmata steps -> true, proving the walk keeps going past a
// non-Nirmata AgentRef instead of stopping at the first match.
func TestWorkflowNeedsNirmataLLMCredentials_MixedStepsKeepsWalking_True(t *testing.T) {
	openaiAgent := &ottoflowv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "openai-agent"},
		Spec:       ottoflowv1alpha1.AgentSpec{Prompt: "x", ModelProvider: "openai"},
	}
	nirmataAgent := &ottoflowv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "nirmata-agent"},
		Spec:       ottoflowv1alpha1.AgentSpec{Prompt: "x", ModelProvider: "nirmata"},
	}
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "wf1"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{
				{Name: "callOpenAI", AgentRef: &ottoflowv1alpha1.StepAgentRef{Name: "openai-agent"}},
				{Name: "callNirmata", AgentRef: &ottoflowv1alpha1.StepAgentRef{Name: "nirmata-agent"}},
			},
		},
	}
	c := newSecretRefsTestClient(t, openaiAgent, nirmataAgent)
	got, err := workflowNeedsNirmataLLMCredentials(context.Background(), c, c, wf, "ns1")
	if err != nil {
		t.Fatalf("workflowNeedsNirmataLLMCredentials: %v", err)
	}
	if !got {
		t.Error("a later nirmata-provider step must still be found after an earlier non-Nirmata step")
	}
}

// Case 8: Agent NotFound -> (false, nil) — a missing Agent is not treated as needing a token;
// the run still fails, but with collectSecretRefs' own "fetching Agent ... not found" error
// (see agentRefNeeds' doc comment), not RBAC advice.
func TestWorkflowNeedsNirmataLLMCredentials_AgentNotFound_False(t *testing.T) {
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "wf1"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{{Name: "callAgent", AgentRef: &ottoflowv1alpha1.StepAgentRef{Name: "does-not-exist"}}},
		},
	}
	c := newSecretRefsTestClient(t) // no Agent objects at all
	got, err := workflowNeedsNirmataLLMCredentials(context.Background(), c, c, wf, "ns1")
	if err != nil {
		t.Fatalf("expected a missing Agent to be benign (false, nil), got error: %v", err)
	}
	if got {
		t.Error("a missing Agent must not be treated as needing a Nirmata token — collectSecretRefs' " +
			"own \"fetching Agent ... not found\" error is the accurate diagnosis, not RBAC advice")
	}
}

// Case 9: a non-NotFound Agent fetch error must propagate, matching this walker's existing
// fail-closed convention for every other CR fetch.
func TestWorkflowNeedsNirmataLLMCredentials_AgentFetchErrorPropagates(t *testing.T) {
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "wf1"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{{Name: "callAgent", AgentRef: &ottoflowv1alpha1.StepAgentRef{Name: "agent1"}}},
		},
	}
	base := fake.NewClientBuilder().WithScheme(newSecretRefsTestScheme(t)).Build()
	c := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*ottoflowv1alpha1.Agent); ok {
				return apierrors.NewInternalError(errors.New("etcd unavailable"))
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	})
	_, err := workflowNeedsNirmataLLMCredentials(context.Background(), c, c, wf, "ns1")
	if err == nil {
		t.Fatal("expected a non-NotFound Agent fetch error to propagate")
	}
	if !strings.Contains(err.Error(), "etcd unavailable") {
		t.Errorf("expected the underlying error to be wrapped, got: %v", err)
	}
}

// Case 10: workflowNeedsAgentExecutor with an openai AgentRef must be true — the two
// predicates diverge (workflowNeedsNirmataLLMCredentials would say false for this same
// workflow) and the CA-mount predicate must be completely unaffected by the provider narrowing.
func TestWorkflowNeedsAgentExecutor_OpenAIAgentStillTrue(t *testing.T) {
	agentCRD := &ottoflowv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "agent1"},
		Spec:       ottoflowv1alpha1.AgentSpec{Prompt: "x", ModelProvider: "openai"},
	}
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "wf1"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{{Name: "callAgent", AgentRef: &ottoflowv1alpha1.StepAgentRef{Name: "agent1"}}},
		},
	}
	c := newSecretRefsTestClient(t, agentCRD)
	got, err := workflowNeedsAgentExecutor(context.Background(), c, wf, "ns1")
	if err != nil {
		t.Fatalf("workflowNeedsAgentExecutor: %v", err)
	}
	if !got {
		t.Error("workflowNeedsAgentExecutor must be true for any AgentRef, independent of modelProvider — " +
			"the two predicates diverge and the CA mount decision must be unaffected")
	}
}

// --- agentRefNeeds must read the Agent via the caller-supplied
// agentReader, not the shared/cached client. The Agent exists ONLY behind the reader passed as
// agentReader, so if agentRefNeeds read through the cached client instead, this would wrongly
// resolve as Agent-NotFound -> (false, nil).

func TestWorkflowNeedsNirmataLLMCredentials_ReadsAgentViaAPIReader(t *testing.T) {
	agentCRD := &ottoflowv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "agent1"},
		Spec:       ottoflowv1alpha1.AgentSpec{Prompt: "x", ModelProvider: "nirmata"},
	}
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "wf1"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{{Name: "callAgent", AgentRef: &ottoflowv1alpha1.StepAgentRef{Name: "agent1"}}},
		},
	}
	cached := newSecretRefsTestClient(t) // simulates a stale informer that has not seen the Agent yet
	direct := newSecretRefsTestClient(t, agentCRD)
	got, err := workflowNeedsNirmataLLMCredentials(context.Background(), cached, direct, wf, "ns1")
	if err != nil {
		t.Fatalf("workflowNeedsNirmataLLMCredentials: %v", err)
	}
	if !got {
		t.Error("expected the Agent Get to go through agentReader (direct), not the cached client (which has no Agent)")
	}
}

// --- Only an UNCONDITIONALLY reachable AgentRef counts. A build-time
// walk can prove a step is reachable but never that it will actually run, so a step behind
// matchConditions, a step with failurePolicy: Continue, or a step inside a ForEach must not be
// credited — each of those can execute zero times, or fail without failing the run, on a
// workflow that can succeed without the credentials. The control case proves the narrowing does
// not also weaken the
// unconditional path this feature exists for.

func TestWorkflowNeedsNirmataLLMCredentials_MatchConditions_False(t *testing.T) {
	agentCRD := &ottoflowv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "agent1"},
		Spec:       ottoflowv1alpha1.AgentSpec{Prompt: "x", ModelProvider: "nirmata"},
	}
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "wf1"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{{
				Name:            "callAgent",
				AgentRef:        &ottoflowv1alpha1.StepAgentRef{Name: "agent1"},
				MatchConditions: []ottoflowv1alpha1.MatchCondition{{Name: "cond1", Expression: "true"}},
			}},
		},
	}
	c := newSecretRefsTestClient(t, agentCRD)
	got, err := workflowNeedsNirmataLLMCredentials(context.Background(), c, c, wf, "ns1")
	if err != nil {
		t.Fatalf("workflowNeedsNirmataLLMCredentials: %v", err)
	}
	if got {
		t.Error("a step behind matchConditions may never run and must not be credited as needing a Nirmata token")
	}
}

func TestWorkflowNeedsNirmataLLMCredentials_FailurePolicyContinue_False(t *testing.T) {
	agentCRD := &ottoflowv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "agent1"},
		Spec:       ottoflowv1alpha1.AgentSpec{Prompt: "x", ModelProvider: "nirmata"},
	}
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "wf1"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{{
				Name:          "callAgent",
				AgentRef:      &ottoflowv1alpha1.StepAgentRef{Name: "agent1"},
				FailurePolicy: ottoflowv1alpha1.FailurePolicyContinue,
			}},
		},
	}
	c := newSecretRefsTestClient(t, agentCRD)
	got, err := workflowNeedsNirmataLLMCredentials(context.Background(), c, c, wf, "ns1")
	if err != nil {
		t.Fatalf("workflowNeedsNirmataLLMCredentials: %v", err)
	}
	if got {
		t.Error("a step with failurePolicy: Continue can fail without failing the run and must not be credited")
	}
}

func TestWorkflowNeedsNirmataLLMCredentials_ForEach_False(t *testing.T) {
	agentCRD := &ottoflowv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "agent1"},
		Spec:       ottoflowv1alpha1.AgentSpec{Prompt: "x", ModelProvider: "nirmata"},
	}
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "wf1"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{
				{
					Name: "fanOut",
					ForEach: &ottoflowv1alpha1.StepForEach{
						Items: "items.list", // may evaluate to an empty list; never provable at build time
						Step:  &ottoflowv1alpha1.StepForEachStep{AgentRef: &ottoflowv1alpha1.StepAgentRef{Name: "agent1"}},
					},
				},
			},
		},
	}
	c := newSecretRefsTestClient(t, agentCRD)
	got, err := workflowNeedsNirmataLLMCredentials(context.Background(), c, c, wf, "ns1")
	if err != nil {
		t.Fatalf("workflowNeedsNirmataLLMCredentials: %v", err)
	}
	if got {
		t.Error("a ForEach can iterate zero times; its child AgentRef must not be credited as needing a Nirmata token")
	}
}

// TestWorkflowNeedsNirmataLLMCredentials_StepTemplateOwnMatchConditions_True pins the runtime
// semantics of executeStepTemplate (steptemplate_executor.go): checkMatchConditions and the
// FailurePolicy branch in the workflow loop (executor.go) run once, before executeStep is ever
// called, against the ORIGINAL referencing Step — never against the instantiated/merged step
// executeStepTemplate builds. So a StepTemplate's own matchConditions/failurePolicy are inert at
// runtime; only the outer Step's fields govern whether it may not run. Here the outer Step sets
// neither, so this AgentRef executes unconditionally despite the template declaring a
// matchCondition, and must be credited.
func TestWorkflowNeedsNirmataLLMCredentials_StepTemplateOwnMatchConditions_True(t *testing.T) {
	agentCRD := &ottoflowv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "agent1"},
		Spec:       ottoflowv1alpha1.AgentSpec{Prompt: "x", ModelProvider: "nirmata"},
	}
	tpl := &ottoflowv1alpha1.StepTemplate{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "tpl1"},
		Spec: ottoflowv1alpha1.StepTemplateSpec{
			Step: ottoflowv1alpha1.StepTemplateStep{
				AgentRef:        &ottoflowv1alpha1.StepAgentRef{Name: "agent1"},
				MatchConditions: []ottoflowv1alpha1.MatchCondition{{Name: "cond1", Expression: "true"}},
			},
		},
	}
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "wf1"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{{Name: "useTpl", StepTemplateRef: &ottoflowv1alpha1.StepTemplateRef{Name: "tpl1"}}},
		},
	}
	c := newSecretRefsTestClient(t, tpl, agentCRD)
	got, err := workflowNeedsNirmataLLMCredentials(context.Background(), c, c, wf, "ns1")
	if err != nil {
		t.Fatalf("workflowNeedsNirmataLLMCredentials: %v", err)
	}
	if !got {
		t.Error("a StepTemplate's own matchConditions never take effect at runtime (the outer step's do); this unconditionally-reached AgentRef must be credited")
	}
}

// Control case: an unconditional (no matchConditions, no failurePolicy override, not inside a
// ForEach) top-level nirmata AgentRef must still return true — the narrowing above must not
// weaken the unconditional case this feature exists for.
func TestWorkflowNeedsNirmataLLMCredentials_UnconditionalNirmata_True(t *testing.T) {
	agentCRD := &ottoflowv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "agent1"},
		Spec:       ottoflowv1alpha1.AgentSpec{Prompt: "x", ModelProvider: "nirmata"},
	}
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "wf1"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{{Name: "callAgent", AgentRef: &ottoflowv1alpha1.StepAgentRef{Name: "agent1"}}},
		},
	}
	c := newSecretRefsTestClient(t, agentCRD)
	got, err := workflowNeedsNirmataLLMCredentials(context.Background(), c, c, wf, "ns1")
	if err != nil {
		t.Fatalf("workflowNeedsNirmataLLMCredentials: %v", err)
	}
	if !got {
		t.Error("an unconditionally reachable nirmata agentRef must still be credited as needing a Nirmata token")
	}
}

// --- The coverage matrix must include the fromWorkflowRef route (where
// effectiveNamespace flips to the sub-workflow's own namespace) and an agentRef.Namespace
// explicitly set to a namespace different from effectiveNamespace.

func TestWorkflowNeedsNirmataLLMCredentials_WorkflowRefNamespaceFlip_True(t *testing.T) {
	agentCRD := &ottoflowv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns2", Name: "agent1"},
		Spec:       ottoflowv1alpha1.AgentSpec{Prompt: "x", ModelProvider: "nirmata"},
	}
	sub := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns2", Name: "sub1"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{{Name: "callAgent", AgentRef: &ottoflowv1alpha1.StepAgentRef{Name: "agent1"}}},
		},
	}
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "wf1"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{{Name: "callSub", WorkflowRef: &ottoflowv1alpha1.StepWorkflowRef{Name: "sub1", Namespace: "ns2"}}},
		},
	}
	c := newSecretRefsTestClient(t, sub, agentCRD)
	got, err := workflowNeedsNirmataLLMCredentials(context.Background(), c, c, wf, "ns1")
	if err != nil {
		t.Fatalf("workflowNeedsNirmataLLMCredentials: %v", err)
	}
	if !got {
		t.Error("a nirmata agentRef reached through a workflowRef hop (effectiveNamespace flips to the " +
			"sub-workflow's namespace) must be credited")
	}
}

func TestWorkflowNeedsNirmataLLMCredentials_AgentRefExplicitNamespace_True(t *testing.T) {
	agentCRD := &ottoflowv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns2", Name: "agent1"},
		Spec:       ottoflowv1alpha1.AgentSpec{Prompt: "x", ModelProvider: "nirmata"},
	}
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "wf1"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{{
				Name:     "callAgent",
				AgentRef: &ottoflowv1alpha1.StepAgentRef{Name: "agent1", Namespace: "ns2"},
			}},
		},
	}
	c := newSecretRefsTestClient(t, agentCRD)
	got, err := workflowNeedsNirmataLLMCredentials(context.Background(), c, c, wf, "ns1")
	if err != nil {
		t.Fatalf("workflowNeedsNirmataLLMCredentials: %v", err)
	}
	if !got {
		t.Error("an agentRef.Namespace override must resolve the Agent in that namespace, not effectiveNamespace")
	}
}
