/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

// nirmataModelProvider mirrors the provider name the agent-executor gates its Nirmata-token
// requirement on. SOURCE OF TRUTH: internal/workflow/executor/exec_handler.go
// (const nirmataProvider = "nirmata"), which also coerces an empty provider to this value
// when validating LLM credentials.
//
// Duplicated here rather than exported from that package: the constant there is unexported and
// this is its only consumer outside it. Duplicating the literal is safe because changing it is
// a breaking CRD enum change that would require coordinated edits everywhere regardless.
const nirmataModelProvider = "nirmata"

// workflowNeedsAgentExecutor reports whether workflow — including everything it reaches via
// ForEach (inline or templated child steps), StepTemplateRef expansion, and WorkflowRef
// sub-workflows — contains at least one AgentRef step.
//
// AgentRef is the only step type that calls the agent-executor service (exec_client.go
// executeAgentViaExecHTTP): MCPToolCall connects straight to its target MCPServer from the
// runner pod and never goes through agent-executor, and ExternalAgentRef calls an external
// A2A endpoint, not our own agent-executor. So AgentRef presence is exactly the condition
// under which a runner Job needs the agent-executor CA mounted. Today buildWorkflowRunnerJob
// mounts that CA whenever RunnerConfig.AgentExecutorCASecret is set, without consulting this
// predicate, so its only callers are this package's tests.
//
// This intentionally does NOT reuse secretRefWalker/collectSecretRefs: that walk also fetches
// every reachable MCPServer (and the Agent CRD behind each AgentRef, for its MCPTools) and is
// fail-closed on those fetches, which has nothing to do with whether agent-executor's CA is
// needed — an AgentRef step needs the CA regardless of whether its Agent or MCP tools resolve
// cleanly, and a workflow with no AgentRef step must never fail this check just because some
// unrelated MCPServer reference is broken. This walker only inspects the Step tree already in
// memory plus StepTemplate/Workflow CRDs (the same two kinds collectSecretRefs fetches for the
// same reason: expanding StepTemplateRef/WorkflowRef requires reading them), with the same
// cycle (visitedKey) and depth (maxWorkflowRefDepth) guards collectSecretRefs uses.
//
//nolint:unparam // only tests call this today and each passes the same runNamespace; see above.
func workflowNeedsAgentExecutor(ctx context.Context, c client.Client, workflow *ottoflowv1alpha1.Workflow, runNamespace string) (bool, error) {
	// agentReader==c: requireNirmataProvider is false, so agentRefNeeds returns before ever
	// touching agentReader (see its doc comment) — this walk never fetches an Agent.
	found, err := walkAgentSteps(ctx, c, c, workflow, runNamespace, false)
	if err != nil {
		return false, fmt.Errorf("checking agent-executor need: %w", err)
	}
	return found, nil
}

// walkAgentSteps runs one agentStepWalker pass over workflow's steps, seeded and looped
// identically for both callers below — they differ only in requireNirmataProvider (see
// agentStepWalker's doc comment for what that toggle changes). agentReader is the reader
// agentRefNeeds uses for its Agent Get; see agentStepWalker.agentReader's doc comment for why
// it must be uncached for the requireNirmataProvider walk.
//
// Errors returned from here (and from every helper this walk calls: agentRefNeeds,
// fromWorkflowRef, fetchStepTemplateStep) are unprefixed — each of workflowNeedsAgentExecutor
// and workflowNeedsNirmataLLMCredentials wraps its result once, at the boundary, naming its
// own check, so an error surfaced through the credentials preflight names that check and not
// the unrelated agent-executor-CA check.
func walkAgentSteps(
	ctx context.Context, c client.Client, agentReader client.Reader,
	workflow *ottoflowv1alpha1.Workflow, runNamespace string, requireNirmataProvider bool,
) (bool, error) {
	w := &agentStepWalker{
		client:      c,
		agentReader: agentReader,
		visited: map[visitedKey]struct{}{
			{kind: "Workflow", namespace: workflow.Namespace, name: workflow.Name}: {},
		},
		requireNirmataProvider: requireNirmataProvider,
	}
	for _, step := range workflow.Spec.Steps {
		found, err := w.fromStep(ctx, step, runNamespace, 0)
		if err != nil {
			return false, err
		}
		if found {
			return true, nil
		}
	}
	return false, nil
}

// agentStepWalker carries the read-only state shared across one workflowNeedsAgentExecutor
// call. Deliberately separate from secretRefWalker — see workflowNeedsAgentExecutor's doc
// comment for why.
type agentStepWalker struct {
	// client fetches the Workflow/StepTemplate CRs this walk expands (fromWorkflowRef,
	// fetchStepTemplateStep) — the shared, cached controller client, same as every other
	// non-Secret CR read in this package.
	client  client.Client
	visited map[visitedKey]struct{}
	// requireNirmataProvider narrows an AgentRef match to Agents whose modelProvider is
	// "" or "nirmata" — the only providers for which the agent-executor demands a
	// runner-supplied token (exec_handler.go). Zero value false, so
	// workflowNeedsAgentExecutor counts every AgentRef and never fetches an Agent.
	requireNirmataProvider bool
	// agentReader is the reader agentRefNeeds uses for its Agent Get, kept separate from
	// client (unlike Secrets, Agent caching is not disabled manager-wide, so this is not
	// "always the direct reader" — it is whatever the caller of walkAgentSteps supplies).
	// workflowNeedsNirmataLLMCredentials's caller passes the controller's direct
	// (non-cached) reader specifically to avoid a stale-informer race: an Agent applied in
	// the same batch as the Workflow/WorkflowRun it's referenced from may not have reached
	// this controller's informer cache yet, and a cache-miss NotFound here becomes
	// agentRefNeeds' benign (false, nil)
	// — silently defeating the whole preflight this walk exists to run. Every other input
	// to this same decision (injectWellKnownLLMCredentials' Secret Get) already goes
	// through the direct reader; this field keeps the Agent Get consistent with that.
	// workflowNeedsAgentExecutor (requireNirmataProvider==false) never uses this field, so
	// its caller passes the ordinary cached client here and pays no extra cost.
	agentReader client.Reader
}

// stepMayNotRun reports whether step's own matchConditions/failurePolicy mean the runtime
// might never execute it (a matchConditions entry can evaluate false and skip it) or execute
// it and still let the workflow succeed on failure (failurePolicy: Continue) — either way, an
// AgentRef reached only through this step cannot be credited as "needs Nirmata credentials"
// by a build-time walk. Used only by the requireNirmataProvider (credentials) walk: see
// workflowNeedsNirmataLLMCredentials' doc comment for why a build-time verdict is least
// trustworthy exactly where execution is conditional.
//
// Deliberately checks only the STEP's own fields, never a StepTemplate body's copied
// matchConditions/failurePolicy: executeStepTemplate (steptemplate_executor.go) merges those
// template fields into the instantiated step only when the outer step doesn't set its own, but
// checkMatchConditions and the FailurePolicy branch in the workflow loop (executor.go) run
// once, before executeStep is ever called, against the ORIGINAL referencing Step — never
// against the instantiated/merged one executeStepTemplate builds. A StepTemplate's own
// matchConditions/failurePolicy are therefore inert at runtime today; crediting them here would
// exclude a step that actually executes unconditionally and hard-fails on error, widening this
// walk's already-accepted coverage gap for no reason.
func stepMayNotRun(matchConditions []ottoflowv1alpha1.MatchCondition, failurePolicy string) bool {
	return len(matchConditions) > 0 || failurePolicy == ottoflowv1alpha1.FailurePolicyContinue
}

// agentRefNeeds reports whether an AgentRef counts as a match for this walk. It is the
// ONLY place requireNirmataProvider is applied: both bare `agentRef != nil` sites
// (fromStep and fromLeafRefs) must route through it. fromLeafRefs is the path taken for
// ForEach.Step.AgentRef and StepTemplateStep.AgentRef, so narrowing only fromStep would
// leave a ForEach- or StepTemplate-wrapped non-Nirmata Agent counted as needing a token.
//
// A missing Agent is (false, nil): this does NOT mean the run proceeds and later fails with
// the runner's own "agent not found" message — collectSecretRefs (agentRefSecretRefs) fetches
// the same Agent moments later during the same buildWorkflowRunnerJob call and has no
// NotFound branch of its own, so a genuinely missing Agent still aborts the build terminally,
// just with THAT function's "resolving secret refs: fetching Agent ... not found" error
// instead of RBAC advice from this one. No runner pod is ever created in that case, so the
// runner's own "agent not found" message can never actually surface. Returning (false, nil)
// here only means this walk itself does not misdiagnose the eventual failure as an RBAC
// problem; it does not mean the run keeps going. Any other fetch error propagates, matching
// this walker's existing convention (fetchStepTemplateStep, below), so setRunFailed names the
// real API failure instead of wrapping it in RBAC advice.
func (w *agentStepWalker) agentRefNeeds(
	ctx context.Context, ref *ottoflowv1alpha1.StepAgentRef, effectiveNamespace string,
) (bool, error) {
	if !w.requireNirmataProvider {
		return true, nil // MUST stay first: preserves workflowNeedsAgentExecutor exactly.
	}
	// Defaulting mirrors agentRefSecretRefs and the runtime executor (agent_executor.go).
	ns := ref.Namespace
	if ns == "" {
		ns = effectiveNamespace
	}
	var agentCRD ottoflowv1alpha1.Agent
	if err := w.agentReader.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, &agentCRD); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("fetching Agent %s/%s: %w", ns, ref.Name, err)
	}
	// exec_handler.go coerces an empty provider to nirmata.
	p := agentCRD.Spec.ModelProvider
	return p == "" || p == nirmataModelProvider, nil
}

// workflowNeedsNirmataLLMCredentials reports whether any UNCONDITIONALLY reachable AgentRef
// targets an Agent that will demand a runner-supplied Nirmata token. Strictly narrower than
// workflowNeedsAgentExecutor, which answers the different question of whether the runner
// needs the agent-executor CA mount (see its doc comment).
//
// "Unconditionally reachable" is deliberate and load-bearing, not an implementation detail:
// this predicate drives a NEW terminal build-time failure (the terminal error
// handleDeniedLLMCredentialsRead returns when the credentials read is denied), and a build-time
// walk can only ever prove a step is REACHABLE, never that
// it will actually RUN. A step behind a non-empty matchConditions can evaluate false and be
// skipped entirely; a step with failurePolicy: Continue can fail outright and still let the
// workflow succeed; a step inside a ForEach can iterate zero times, since StepForEach.Items is
// always a CEL expression string whose emptiness can never be proven ahead of execution. Credit
// any of those as "needs Nirmata credentials" and a denied read turns a run that can succeed
// without the credentials into a terminal failure — exactly the over-reach this predicate
// must avoid. So stepMayNotRun and the ForEach check in fromForEach skip (return false,
// not an error) for every conditional or best-effort shape, and this predicate returns true only
// for a step that is guaranteed to run and to hard-fail the workflow if it fails — a build-time
// verdict is least trustworthy exactly where execution is conditional, so this walk simply
// declines to render one there.
func workflowNeedsNirmataLLMCredentials(
	ctx context.Context, c client.Client, agentReader client.Reader,
	workflow *ottoflowv1alpha1.Workflow, runNamespace string,
) (bool, error) {
	found, err := walkAgentSteps(ctx, c, agentReader, workflow, runNamespace, true)
	if err != nil {
		return false, fmt.Errorf("checking Nirmata LLM credential need: %w", err)
	}
	return found, nil
}

// fromStep reports whether step, or anything reachable from it (StepTemplateRef, WorkflowRef,
// ForEach), contains an AgentRef.
func (w *agentStepWalker) fromStep(ctx context.Context, step ottoflowv1alpha1.Step, effectiveNamespace string, depth int) (bool, error) {
	if w.requireNirmataProvider && stepMayNotRun(step.MatchConditions, step.FailurePolicy) {
		// This step's own matchConditions/failurePolicy make its execution (and so
		// everything beneath it — AgentRef, WorkflowRef, StepTemplateRef, ForEach) not
		// statically guaranteed; see stepMayNotRun and workflowNeedsNirmataLLMCredentials'
		// doc comment. workflowNeedsAgentExecutor (requireNirmataProvider==false) is
		// unaffected — it counts reachability only, not guaranteed execution.
		return false, nil
	}
	if step.AgentRef != nil {
		found, err := w.agentRefNeeds(ctx, step.AgentRef, effectiveNamespace)
		if err != nil || found {
			return found, err
		}
		// Non-Nirmata Agent: keep walking — a later step may still need a token.
	}
	if step.WorkflowRef != nil {
		found, err := w.fromWorkflowRef(ctx, step.WorkflowRef, effectiveNamespace, depth)
		if err != nil || found {
			return found, err
		}
	}
	if step.StepTemplateRef != nil {
		ns := step.StepTemplateRef.Namespace
		if ns == "" {
			ns = effectiveNamespace
		}
		tplStep, err := w.fetchStepTemplateStep(ctx, ns, step.StepTemplateRef.Name)
		if err != nil {
			return false, err
		}
		// No separate "may not run" check on tplStep here: the guard at the top of this
		// function already means step.MatchConditions is empty and step.FailurePolicy isn't
		// Continue by the time we get here, and the StepTemplate body's OWN
		// matchConditions/failurePolicy are inert at runtime regardless (see stepMayNotRun's
		// doc comment) — so this branch is only reached when the AgentRef/WorkflowRef inside
		// tplStep is, in fact, unconditionally reachable.
		found, err := w.fromLeafRefs(ctx, tplStep.AgentRef, tplStep.WorkflowRef, effectiveNamespace, depth)
		if err != nil || found {
			return found, err
		}
	}
	if step.ForEach != nil {
		found, err := w.fromForEach(ctx, step.ForEach, effectiveNamespace, depth)
		if err != nil || found {
			return found, err
		}
	}
	return false, nil
}

// fromForEach checks a ForEach step's inline child step or its StepTemplateRef.
func (w *agentStepWalker) fromForEach(ctx context.Context, fe *ottoflowv1alpha1.StepForEach, effectiveNamespace string, depth int) (bool, error) {
	if w.requireNirmataProvider {
		// fe.Items is always a CEL expression string (StepForEach.Items), never a literal
		// list, so whether it evaluates to a non-empty item set can never be proven at
		// build time — a ForEach with zero items never runs its child step at all. Treat
		// every ForEach as conditional for the credentials walk regardless of the child
		// step's own matchConditions/failurePolicy; see workflowNeedsNirmataLLMCredentials'
		// doc comment. workflowNeedsAgentExecutor (requireNirmataProvider==false) is
		// unaffected.
		return false, nil
	}
	if fe.Step != nil {
		found, err := w.fromLeafRefs(ctx, fe.Step.AgentRef, fe.Step.WorkflowRef, effectiveNamespace, depth)
		if err != nil || found {
			return found, err
		}
	}
	if fe.StepTemplateRef != nil {
		ns := fe.StepTemplateRef.Namespace
		if ns == "" {
			ns = effectiveNamespace
		}
		tplStep, err := w.fetchStepTemplateStep(ctx, ns, fe.StepTemplateRef.Name)
		if err != nil {
			return false, err
		}
		return w.fromLeafRefs(ctx, tplStep.AgentRef, tplStep.WorkflowRef, effectiveNamespace, depth)
	}
	return false, nil
}

// fromLeafRefs checks the AgentRef/WorkflowRef pair shared by StepForEachStep and
// StepTemplateStep bodies (mirrors secretRefWalker.fromRefFields, minus the MCPToolCall/
// ExternalAgentRef handling this walker has no use for).
func (w *agentStepWalker) fromLeafRefs(ctx context.Context, agentRef *ottoflowv1alpha1.StepAgentRef, workflowRef *ottoflowv1alpha1.StepWorkflowRef, effectiveNamespace string, depth int) (bool, error) {
	if agentRef != nil {
		found, err := w.agentRefNeeds(ctx, agentRef, effectiveNamespace)
		if err != nil || found {
			return found, err
		}
	}
	if workflowRef != nil {
		return w.fromWorkflowRef(ctx, workflowRef, effectiveNamespace, depth)
	}
	return false, nil
}

// fromWorkflowRef fetches a referenced sub-Workflow and checks its steps. Mirrors
// secretRefWalker.workflowRefSecretRefs' depth/cycle guards and namespace defaulting exactly.
func (w *agentStepWalker) fromWorkflowRef(ctx context.Context, ref *ottoflowv1alpha1.StepWorkflowRef, effectiveNamespace string, depth int) (bool, error) {
	if depth >= maxWorkflowRefDepth {
		return false, fmt.Errorf("workflowRef chain exceeds max depth %d (possible misconfiguration)", maxWorkflowRefDepth)
	}

	subNamespace := ref.Namespace
	if subNamespace == "" {
		subNamespace = effectiveNamespace
	}

	vk := visitedKey{kind: "Workflow", namespace: subNamespace, name: ref.Name}
	if _, seen := w.visited[vk]; seen {
		return false, nil // cycle or diamond reference already processed
	}
	w.visited[vk] = struct{}{}

	var sub ottoflowv1alpha1.Workflow
	if err := w.client.Get(ctx, client.ObjectKey{Namespace: subNamespace, Name: ref.Name}, &sub); err != nil {
		return false, fmt.Errorf("fetching sub-Workflow %s/%s: %w", subNamespace, ref.Name, err)
	}
	for _, s := range sub.Spec.Steps {
		found, err := w.fromStep(ctx, s, subNamespace, depth+1)
		if err != nil || found {
			return found, err
		}
	}
	return false, nil
}

// fetchStepTemplateStep fetches a StepTemplate CR and returns its step body.
func (w *agentStepWalker) fetchStepTemplateStep(ctx context.Context, namespace, name string) (*ottoflowv1alpha1.StepTemplateStep, error) {
	var tpl ottoflowv1alpha1.StepTemplate
	if err := w.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &tpl); err != nil {
		return nil, fmt.Errorf("fetching StepTemplate %s/%s: %w", namespace, name, err)
	}
	return &tpl.Spec.Step, nil
}
