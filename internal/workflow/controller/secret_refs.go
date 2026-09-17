/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/secretmount"
)

// maxWorkflowRefDepth bounds workflowRef recursion. Combined with the visited-set cycle
// check below, this is a belt-and-suspenders guard: the visited set alone already
// terminates any cycle (A->B->A), but a long non-cyclic chain (A->B->C->D->...) is bounded
// here so a misconfigured workflow cannot make secret-ref resolution run away.
const maxWorkflowRefDepth = 10

// resolvedSecretRef is one Secret reference discovered while walking a Workflow/WorkflowRun,
// with enough information to mount it into the runner Job (or reject it) and to build an
// actionable error message.
type resolvedSecretRef struct {
	// Namespace is the fully-resolved (already-defaulted) namespace the Secret lives in.
	Namespace string
	// Name is the Secret name.
	Name string
	// Key is the Secret data key needed. Ignored when IsKubeconfig is true.
	Key string
	// Origin is a human-readable description of where this reference came from, for error
	// messages (e.g. `step "callAgent" externalAgentRef.auth.secretRef`).
	Origin string
	// IsKubeconfig is true only for WorkflowRun.Spec.ClusterRef.KubeConfigSecretRef: the
	// whole Secret (every key) must be mounted, not just one key, because Key is optional
	// there and resolve.go probes "config"/"kubeconfig"/"value" at read time.
	IsKubeconfig bool
	// EnvVarName is non-empty only for an MCPServer env credential (Spec.Env entries with a
	// SecretKeyRef); it records the env var name for the origin/error message. Like every other
	// cred, the value is delivered to the runner as a mounted file (never a pod env var): the
	// runner rebuilds the MCP subprocess env and resolves each value from the mount map
	// (internal/agent/mcp_client_impl.go resolveEnvValue), so a pod SecretKeyRef env var would be
	// clobbered by the runner-built (empty) value. Only meaningful when NeedsRunnerMount is true.
	EnvVarName string
	// Optional mirrors corev1.SecretKeySelector.Optional on an MCPServer env credential.
	// Kubernetes' contract for an optional SecretKeyRef is that a missing Secret or missing key
	// leaves the variable UNSET rather than failing the pod, so an optional ref must not be
	// projected as a mandatory volume item (that wedges the runner pod in FailedMount, which
	// handleStuckRunnerPod turns into a terminally Failed run) and must not make
	// resolveEnvValue error at execution time. buildSecretMounts groups optional refs into
	// their own Secret volume with SecretVolumeSource.Optional=true — which the kubelet honours
	// for a missing ITEM KEY as well as a missing Secret (k8s.io/kubernetes secret volume
	// MakePayload skips a mapped key that is absent when optional is set) — and
	// internal/agent/mcp_client_impl.go omits the env entry when the file is not there.
	// Only meaningful when EnvVarName != "".
	Optional bool
	// NeedsRunnerMount is false for refs reachable only via a Step.AgentRef's Agent.Spec.MCPTools:
	// those MCP servers are called from the agent-executor pod (a separate process with its
	// own, unaffected, already-scoped secrets RBAC — charts/ottoflow/templates/
	// agent-executor-clusterrole.yaml), never from the workflow-runner. They are still
	// collected and policy-checked here (fail-fast on a missing/ungranted ref, and the
	// cross-namespace default-deny applies uniformly) but the controller does not mount
	// them into the runner Job.
	NeedsRunnerMount bool
}

// visitedKey identifies one CR the walk has already started processing, keyed by kind so a
// Workflow and a StepTemplate with the same namespaced name never collide.
type visitedKey struct {
	kind      string
	namespace string
	name      string
}

// secretRefWalker carries the read-only state shared across one collectSecretRefs call.
type secretRefWalker struct {
	client  client.Client
	visited map[visitedKey]struct{}
}

// collectSecretRefs walks a Workflow (and everything it can reach: ForEach inline steps,
// StepTemplateRef expansions, WorkflowRef sub-workflows, and every MCPServer reachable from
// a mcpToolCall or an agentRef's Agent.Spec.MCPTools) and returns every Secret reference it
// finds, each annotated with its origin (for error messages) and its fully-resolved
// (already-defaulted) namespace.
//
// FAIL-CLOSED: a missing reference or a CR that cannot be fetched (NotFound or any other API
// error) aborts the whole walk with a terminal error — this function never silently skips a
// reference it could not resolve.
//
// TOCTOU note: this runs at Job-build time in the controller. The runner re-fetches every CR
// (Workflow, StepTemplate, MCPServer, Agent) again at execution time, so a reference that
// changes between build time and execution time (a StepTemplate edited, a Secret renamed)
// is re-validated then too; if that later resolution fails, re-running the WorkflowRun
// re-resolves against the current state of those CRs.
func collectSecretRefs(
	ctx context.Context,
	c client.Client,
	workflow *ottoflowv1alpha1.Workflow,
	workflowRun *ottoflowv1alpha1.WorkflowRun,
) ([]resolvedSecretRef, error) {
	runNamespace := workflowRun.Namespace
	var refs []resolvedSecretRef

	if workflowRun.Spec.ClusterRef != nil && workflowRun.Spec.ClusterRef.KubeConfigSecretRef != nil {
		ref := workflowRun.Spec.ClusterRef.KubeConfigSecretRef
		ns := ref.Namespace
		if ns == "" {
			ns = runNamespace
		}
		refs = append(refs, resolvedSecretRef{
			Namespace:        ns,
			Name:             ref.Name,
			Origin:           "workflowRun.spec.clusterRef.kubeConfigSecretRef",
			IsKubeconfig:     true,
			NeedsRunnerMount: true,
		})
	}

	w := &secretRefWalker{
		client: c,
		visited: map[visitedKey]struct{}{
			{kind: "Workflow", namespace: workflow.Namespace, name: workflow.Name}: {},
		},
	}
	for _, step := range workflow.Spec.Steps {
		stepRefs, err := w.fromStep(ctx, step, runNamespace, fmt.Sprintf("step %q", step.Name), 0)
		if err != nil {
			return nil, err
		}
		refs = append(refs, stepRefs...)
	}
	return refs, nil
}

// fromStep collects secret refs from one top-level (or workflowRef-nested) Step.
// effectiveNamespace is the namespace CASecretRef/Auth.SecretRef/StepTemplateRef/WorkflowRef
// default to when they don't specify their own — the current WorkflowRun's namespace at the
// top level, or (after a WorkflowRef hop) the sub-workflow's own resolved namespace, exactly
// mirroring the defaulting the runtime executor applies (executor.go executeWorkflowReference).
func (w *secretRefWalker) fromStep(ctx context.Context, step ottoflowv1alpha1.Step, effectiveNamespace, origin string, depth int) ([]resolvedSecretRef, error) {
	var refs []resolvedSecretRef

	refs = append(refs, externalAgentRefSecretRefs(step.ExternalAgentRef, effectiveNamespace, origin)...)

	if step.MCPToolCall != nil {
		mcpRefs, err := w.mcpServerSecretRefs(ctx, step.MCPToolCall.Server, effectiveNamespace, origin+".mcpToolCall", true)
		if err != nil {
			return nil, err
		}
		refs = append(refs, mcpRefs...)
	}

	if step.AgentRef != nil {
		agentRefs, err := w.agentRefSecretRefs(ctx, step.AgentRef, effectiveNamespace, origin+".agentRef")
		if err != nil {
			return nil, err
		}
		refs = append(refs, agentRefs...)
	}

	if step.StepTemplateRef != nil {
		tplRefs, err := w.stepTemplateRefSecretRefs(ctx, step.StepTemplateRef, effectiveNamespace, origin+".stepTemplateRef", depth)
		if err != nil {
			return nil, err
		}
		refs = append(refs, tplRefs...)
	}

	if step.WorkflowRef != nil {
		wfRefs, err := w.workflowRefSecretRefs(ctx, step.WorkflowRef, effectiveNamespace, origin+".workflowRef", depth)
		if err != nil {
			return nil, err
		}
		refs = append(refs, wfRefs...)
	}

	if step.ForEach != nil {
		feRefs, err := w.fromForEach(ctx, step.ForEach, effectiveNamespace, origin+".forEach", depth)
		if err != nil {
			return nil, err
		}
		refs = append(refs, feRefs...)
	}

	return refs, nil
}

// fromForEach collects secret refs from a ForEach step's inline child step or its
// StepTemplateRef.
func (w *secretRefWalker) fromForEach(ctx context.Context, fe *ottoflowv1alpha1.StepForEach, effectiveNamespace, origin string, depth int) ([]resolvedSecretRef, error) {
	var refs []resolvedSecretRef

	if fe.Step != nil {
		inlineRefs, err := w.fromForEachStep(ctx, fe.Step, effectiveNamespace, origin+".step", depth)
		if err != nil {
			return nil, err
		}
		refs = append(refs, inlineRefs...)
	}

	if fe.StepTemplateRef != nil {
		ns := fe.StepTemplateRef.Namespace
		if ns == "" {
			ns = effectiveNamespace
		}
		tplStep, err := w.fetchStepTemplateStep(ctx, ns, fe.StepTemplateRef.Name, origin+".stepTemplateRef")
		if err != nil {
			return nil, err
		}
		tplRefs, err := w.fromStepTemplateStep(ctx, tplStep, effectiveNamespace, origin+".stepTemplateRef", depth)
		if err != nil {
			return nil, err
		}
		refs = append(refs, tplRefs...)
	}

	return refs, nil
}

// fromForEachStep collects secret refs from an inline ForEach child step definition.
func (w *secretRefWalker) fromForEachStep(ctx context.Context, s *ottoflowv1alpha1.StepForEachStep, effectiveNamespace, origin string, depth int) ([]resolvedSecretRef, error) {
	return w.fromRefFields(ctx, s.ExternalAgentRef, s.MCPToolCall, s.AgentRef, s.WorkflowRef, effectiveNamespace, origin, depth)
}

// fromStepTemplateStep collects secret refs from an instantiated StepTemplate's step body.
// effectiveNamespace is unchanged from the caller: instantiating a StepTemplate expands its
// step definition inline into the current step and executes it in the current WorkflowRun's
// namespace (steptemplate_executor.go only uses the template's own namespace to fetch the
// StepTemplate CR itself), so CASecretRef/Auth.SecretRef/WorkflowRef defaulting inside it must
// use the same effectiveNamespace as the step that referenced the template.
func (w *secretRefWalker) fromStepTemplateStep(ctx context.Context, s *ottoflowv1alpha1.StepTemplateStep, effectiveNamespace, origin string, depth int) ([]resolvedSecretRef, error) {
	return w.fromRefFields(ctx, s.ExternalAgentRef, s.MCPToolCall, s.AgentRef, s.WorkflowRef, effectiveNamespace, origin, depth)
}

// fromRefFields collects secret refs from the four ref-bearing fields shared by
// StepForEachStep and StepTemplateStep (fromForEachStep and fromStepTemplateStep are one-line
// wrappers around this). fromStep is NOT folded in here: it handles additional Step-only
// fields (e.g. ResourceQuery, StepTemplateRef) that these two step-body types don't have.
func (w *secretRefWalker) fromRefFields(
	ctx context.Context,
	externalAgentRef *ottoflowv1alpha1.StepExternalAgentRef,
	mcpToolCall *ottoflowv1alpha1.StepMCPToolCall,
	agentRef *ottoflowv1alpha1.StepAgentRef,
	workflowRef *ottoflowv1alpha1.StepWorkflowRef,
	effectiveNamespace, origin string,
	depth int,
) ([]resolvedSecretRef, error) {
	var refs []resolvedSecretRef

	refs = append(refs, externalAgentRefSecretRefs(externalAgentRef, effectiveNamespace, origin)...)

	if mcpToolCall != nil {
		mcpRefs, err := w.mcpServerSecretRefs(ctx, mcpToolCall.Server, effectiveNamespace, origin+".mcpToolCall", true)
		if err != nil {
			return nil, err
		}
		refs = append(refs, mcpRefs...)
	}

	if agentRef != nil {
		agentRefs, err := w.agentRefSecretRefs(ctx, agentRef, effectiveNamespace, origin+".agentRef")
		if err != nil {
			return nil, err
		}
		refs = append(refs, agentRefs...)
	}

	if workflowRef != nil {
		wfRefs, err := w.workflowRefSecretRefs(ctx, workflowRef, effectiveNamespace, origin+".workflowRef", depth)
		if err != nil {
			return nil, err
		}
		refs = append(refs, wfRefs...)
	}

	return refs, nil
}

// stepTemplateRefSecretRefs fetches a top-level Step's StepTemplate and walks its step body.
func (w *secretRefWalker) stepTemplateRefSecretRefs(ctx context.Context, ref *ottoflowv1alpha1.StepTemplateRef, effectiveNamespace, origin string, depth int) ([]resolvedSecretRef, error) {
	ns := ref.Namespace
	if ns == "" {
		ns = effectiveNamespace
	}
	tplStep, err := w.fetchStepTemplateStep(ctx, ns, ref.Name, origin)
	if err != nil {
		return nil, err
	}
	return w.fromStepTemplateStep(ctx, tplStep, effectiveNamespace, origin, depth)
}

// fetchStepTemplateStep fetches a StepTemplate CR and returns its step body.
func (w *secretRefWalker) fetchStepTemplateStep(ctx context.Context, namespace, name, origin string) (*ottoflowv1alpha1.StepTemplateStep, error) {
	var tpl ottoflowv1alpha1.StepTemplate
	if err := w.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &tpl); err != nil {
		return nil, fmt.Errorf("resolving secret refs: fetching StepTemplate %s/%s (referenced by %s): %w; "+
			"re-run the WorkflowRun to re-resolve if this was a transient error", namespace, name, origin, err)
	}
	return &tpl.Spec.Step, nil
}

// workflowRefSecretRefs fetches a referenced sub-Workflow and walks its steps.
func (w *secretRefWalker) workflowRefSecretRefs(ctx context.Context, ref *ottoflowv1alpha1.StepWorkflowRef, effectiveNamespace, origin string, depth int) ([]resolvedSecretRef, error) {
	if depth >= maxWorkflowRefDepth {
		return nil, fmt.Errorf("resolving secret refs: workflowRef chain at %s exceeds max depth %d (possible misconfiguration)", origin, maxWorkflowRefDepth)
	}

	// Mirrors executor.go executeWorkflowReference: the sub-workflow's own namespace becomes
	// the effective namespace for everything inside it, cascading to further nested refs.
	subNamespace := ref.Namespace
	if subNamespace == "" {
		subNamespace = effectiveNamespace
	}

	vk := visitedKey{kind: "Workflow", namespace: subNamespace, name: ref.Name}
	if _, seen := w.visited[vk]; seen {
		// Cycle (A->B->A) or a diamond reference already processed: terminate this branch.
		return nil, nil
	}
	w.visited[vk] = struct{}{}

	var sub ottoflowv1alpha1.Workflow
	if err := w.client.Get(ctx, client.ObjectKey{Namespace: subNamespace, Name: ref.Name}, &sub); err != nil {
		return nil, fmt.Errorf("resolving secret refs: fetching sub-Workflow %s/%s (referenced by %s): %w; "+
			"re-run the WorkflowRun to re-resolve if this was a transient error", subNamespace, ref.Name, origin, err)
	}

	var refs []resolvedSecretRef
	for _, step := range sub.Spec.Steps {
		stepOrigin := fmt.Sprintf("%s -> workflow %q step %q", origin, sub.Name, step.Name)
		stepRefs, err := w.fromStep(ctx, step, subNamespace, stepOrigin, depth+1)
		if err != nil {
			return nil, err
		}
		refs = append(refs, stepRefs...)
	}
	return refs, nil
}

// agentRefSecretRefs fetches the referenced Agent CR and collects secret refs from every
// MCPServer it names in Spec.MCPTools ("server:tool" entries). These are NOT flagged
// NeedsRunnerMount: an agentRef step's MCP tools are called from the agent-executor pod, not
// the workflow-runner (see resolvedSecretRef.NeedsRunnerMount doc).
//
// Each MCPServer is resolved in the AGENT's namespace (ns), not the step's effectiveNamespace:
// the runner posts to /api/exec/{agentNamespace}/{agentName} (exec_client.go buildExecURL) and
// the agent-executor resolves that Agent's MCP tools in the namespace from the request path
// (exec_handler.go -> ExecuteAgent -> buildSessionToolsFromMCP -> MCPClientManager.GetClient).
// Using effectiveNamespace for an explicitly cross-namespace agentRef looked up a DIFFERENT
// MCPServer than the one that will actually run: either none (a fail-closed abort of a valid
// workflow with a misleading "MCPServer <runNamespace>/<name> not found") or, when a same-named
// MCPServer exists in both namespaces, the wrong one — policy-checking credentials the
// agent-executor never reads while never checking the ones it does.
func (w *secretRefWalker) agentRefSecretRefs(ctx context.Context, ref *ottoflowv1alpha1.StepAgentRef, effectiveNamespace, origin string) ([]resolvedSecretRef, error) {
	ns := ref.Namespace
	if ns == "" {
		ns = effectiveNamespace
	}
	var agentCRD ottoflowv1alpha1.Agent
	if err := w.client.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, &agentCRD); err != nil {
		return nil, fmt.Errorf("resolving secret refs: fetching Agent %s/%s (referenced by %s): %w; "+
			"re-run the WorkflowRun to re-resolve if this was a transient error", ns, ref.Name, origin, err)
	}

	var refs []resolvedSecretRef
	for _, mcpToolRef := range agentCRD.Spec.MCPTools {
		mcpToolRef = strings.TrimSpace(mcpToolRef)
		if mcpToolRef == "" {
			continue
		}
		serverName, _, _ := strings.Cut(mcpToolRef, ":")
		if serverName == "" {
			continue
		}
		serverRefs, err := w.mcpServerSecretRefs(ctx, serverName, ns, origin, false)
		if err != nil {
			return nil, err
		}
		refs = append(refs, serverRefs...)
	}
	return refs, nil
}

// mcpServerSecretRefs fetches an MCPServer CR from serverNamespace — neither mcpToolCall.Server
// nor Agent.Spec.MCPTools carries a namespace override of its own, so the caller supplies the
// namespace whose consumer will actually resolve it: the step's effectiveNamespace for an
// mcpToolCall (resolved by the runner, mcp_executor.go), or the Agent's own namespace for an
// agentRef (resolved by the agent-executor, see agentRefSecretRefs). Returns every Secret
// reference in its auth and env configuration: env SecretKeyRef, auth.secretRef
// (bearer/apiKey/basic all share this one field), oauth2.clientSecretRef, and
// oauth2.clientCredentialsRef (which implies two keys, client_id and client_secret — see
// internal/agent/mcp_client_impl.go resolveAuthConfigs).
func (w *secretRefWalker) mcpServerSecretRefs(ctx context.Context, serverName, serverNamespace, origin string, needsRunnerMount bool) ([]resolvedSecretRef, error) {
	if serverName == "" {
		return nil, nil
	}
	var mcpServer ottoflowv1alpha1.MCPServer
	if err := w.client.Get(ctx, client.ObjectKey{Namespace: serverNamespace, Name: serverName}, &mcpServer); err != nil {
		return nil, fmt.Errorf("resolving secret refs: fetching MCPServer %s/%s (referenced by %s): %w; "+
			"re-run the WorkflowRun to re-resolve if this was a transient error", serverNamespace, serverName, origin, err)
	}

	base := fmt.Sprintf("%s (MCPServer %s/%s)", origin, mcpServer.Namespace, mcpServer.Name)
	var refs []resolvedSecretRef

	for _, ev := range mcpServer.Spec.Env {
		if ev.ValueFrom == nil || ev.ValueFrom.SecretKeyRef == nil {
			continue
		}
		sel := ev.ValueFrom.SecretKeyRef
		refs = append(refs, resolvedSecretRef{
			// corev1.SecretKeySelector has no namespace field: resolveEnvValue resolves it in
			// the MCPServer's own namespace (internal/agent/mcp_client_impl.go).
			Namespace:        mcpServer.Namespace,
			Name:             sel.Name,
			Key:              sel.Key,
			Origin:           fmt.Sprintf("%s env %q", base, ev.Name),
			EnvVarName:       ev.Name,
			Optional:         sel.Optional != nil && *sel.Optional,
			NeedsRunnerMount: needsRunnerMount,
		})
	}

	if auth := mcpServer.Spec.Auth; auth != nil {
		if auth.SecretRef != nil {
			ns := auth.SecretRef.Namespace
			if ns == "" {
				ns = mcpServer.Namespace
			}
			if auth.Type == "basic" {
				// resolveAuthConfigs (internal/agent/mcp_client_impl.go) reads the two
				// FIXED keys "username" and "password" for basic auth (per AuthConfig's
				// own doc comment), ignoring auth.SecretRef.Key entirely — mount both,
				// not the single key this ref would otherwise carry.
				credOrigin := base + " auth.secretRef (basic)"
				refs = append(refs,
					resolvedSecretRef{Namespace: ns, Name: auth.SecretRef.Name, Key: "username", Origin: credOrigin, NeedsRunnerMount: needsRunnerMount},
					resolvedSecretRef{Namespace: ns, Name: auth.SecretRef.Name, Key: "password", Origin: credOrigin, NeedsRunnerMount: needsRunnerMount},
				)
			} else {
				// bearer/apiKey: resolveAuthConfigs reads auth.SecretRef.Key directly, so
				// this ref's own Key is correct as-is.
				refs = append(refs, resolvedSecretRef{
					Namespace:        ns,
					Name:             auth.SecretRef.Name,
					Key:              auth.SecretRef.Key,
					Origin:           base + " auth.secretRef",
					NeedsRunnerMount: needsRunnerMount,
				})
			}
		}
		if auth.OAuth2 != nil {
			if auth.OAuth2.ClientSecretRef != nil {
				ns := auth.OAuth2.ClientSecretRef.Namespace
				if ns == "" {
					ns = mcpServer.Namespace
				}
				refs = append(refs, resolvedSecretRef{
					Namespace:        ns,
					Name:             auth.OAuth2.ClientSecretRef.Name,
					Key:              auth.OAuth2.ClientSecretRef.Key,
					Origin:           base + " auth.oauth2.clientSecretRef",
					NeedsRunnerMount: needsRunnerMount,
				})
			}
			if auth.OAuth2.ClientCredentialsRef != nil {
				ns := auth.OAuth2.ClientCredentialsRef.Namespace
				if ns == "" {
					ns = mcpServer.Namespace
				}
				credOrigin := base + " auth.oauth2.clientCredentialsRef"
				refs = append(refs,
					resolvedSecretRef{Namespace: ns, Name: auth.OAuth2.ClientCredentialsRef.Name, Key: "client_id", Origin: credOrigin, NeedsRunnerMount: needsRunnerMount},
					resolvedSecretRef{Namespace: ns, Name: auth.OAuth2.ClientCredentialsRef.Name, Key: "client_secret", Origin: credOrigin, NeedsRunnerMount: needsRunnerMount},
				)
			}
		}
	}

	return refs, nil
}

// externalAgentRefSecretRefs returns the CA and bearer-token secret refs (if any) on an
// ExternalAgentRef step. Both default their namespace to effectiveNamespace, mirroring
// newA2AClient / getSecretValue in internal/workflow/executor/a2a_client.go.
func externalAgentRefSecretRefs(ref *ottoflowv1alpha1.StepExternalAgentRef, effectiveNamespace, origin string) []resolvedSecretRef {
	if ref == nil {
		return nil
	}
	var refs []resolvedSecretRef
	if ref.CASecretRef != nil {
		ns := ref.CASecretRef.Namespace
		if ns == "" {
			ns = effectiveNamespace
		}
		refs = append(refs, resolvedSecretRef{
			Namespace:        ns,
			Name:             ref.CASecretRef.Name,
			Key:              "ca.crt", // NamespacedSecretRef has no Key field; a2a_client.go always reads "ca.crt".
			Origin:           origin + ".externalAgentRef.caSecretRef",
			NeedsRunnerMount: true,
		})
	}
	if ref.Auth != nil && ref.Auth.SecretRef != nil {
		sr := ref.Auth.SecretRef
		ns := sr.Namespace
		if ns == "" {
			ns = effectiveNamespace
		}
		refs = append(refs, resolvedSecretRef{
			Namespace:        ns,
			Name:             sr.Name,
			Key:              sr.Key,
			Origin:           origin + ".externalAgentRef.auth.secretRef",
			NeedsRunnerMount: true,
		})
	}
	return refs
}

// validateSecretRefPolicy default-denies any secret reference whose resolved namespace
// differs from the WorkflowRun's own namespace, unless that namespace is in the operator's
// allowlist (RunnerConfig.SecretRefAllowedNamespaces, set via
// --secret-ref-allowed-namespaces). Native Kubernetes Secret mounts (volumes
// and SecretKeyRef env) can only ever reach a Secret in the pod's own namespace, so a
// same-namespace ref always passes; a cross-namespace ref — allowlisted or not — still cannot
// be mounted into the runner Job by buildWorkflowRunnerJob in this phase (there is no
// cross-namespace secret copy — see ensureRunnerSecrets/buildWorkflowRunnerJob doc comments).
// The allowlist's effect here is purely a policy gate for the refs agent-executor resolves
// under its own RBAC (NeedsRunnerMount==false): it distinguishes a namespace the operator
// has vetted as a legitimate secret source from one that is not (the WorkflowRun author is
// told exactly what to ask the operator for).
func validateSecretRefPolicy(refs []resolvedSecretRef, runNamespace string, cfg RunnerConfig) error {
	for _, ref := range refs {
		if ref.Namespace == runNamespace {
			continue
		}
		// A ref that needs to be mounted into the runner Job can never be satisfied
		// cross-namespace, allowlisted or not: buildSecretMounts below has no
		// cross-namespace mounting mechanism — a native Kubernetes Secret volume can only
		// ever reach a Secret in the pod's own namespace (see its doc comment). Fail here
		// with the accurate diagnosis and fix, instead of letting an allowlisted
		// namespace pass this check only to fail later in buildSecretMounts with a less
		// specific error that doesn't say the allowlist could never have helped.
		if ref.NeedsRunnerMount {
			return fmt.Errorf(
				"secret ref %s points to namespace %q, which differs from the WorkflowRun's namespace %q; this "+
					"reference is mounted into the runner Job as a native Kubernetes Secret volume, which can only "+
					"reach a Secret in the runner pod's own namespace — --secret-ref-allowed-namespaces "+
					"cannot make this work for this reference kind. Move the secret into namespace %q, "+
					"or (if this reference came from a workflowRef step) move the sub-workflow into namespace %q "+
					"so its Secret references resolve there instead — a workflowRef sub-workflow executes inline "+
					"in the same runner Job/pod as its parent, not a separate one in its own namespace",
				ref.Origin, ref.Namespace, runNamespace, runNamespace, runNamespace,
			)
		}
		if _, allowed := cfg.SecretRefAllowedNamespaces[ref.Namespace]; allowed {
			continue
		}
		// Remediation names the identity that actually reads this class of ref: a
		// NeedsRunnerMount==false ref (an agentRef step's MCP tool credentials) is resolved
		// by the agent-executor service under agent-executor's own RBAC — the runner
		// ServiceAccount never touches it. What gates it HERE is only the operator
		// allowlist; pointing the author at runner RBAC would send them to fix the wrong
		// identity and leave the run failing anyway.
		return fmt.Errorf(
			"secret ref %s points to namespace %q, which differs from the WorkflowRun's namespace %q and is not in "+
				"the operator-configured allowlist. This reference is resolved by the agent-executor service (under "+
				"agent-executor's own RBAC), not by the workflow runner; the allowlist is what gates it. Either move "+
				"the secret into namespace %q, or have an operator add namespace %q to "+
				"--secret-ref-allowed-namespaces and confirm agent-executor's RBAC can read Secrets there",
			ref.Origin, ref.Namespace, runNamespace, runNamespace, ref.Namespace,
		)
	}
	return nil
}

// secretMountBaseDir is the parent directory under which each distinct referenced Secret gets
// its own per-index subdirectory (one file per requested key). Kept short and fixed so both
// sides (this function and the runner-side readers in internal/workflow/cluster,
// internal/workflow/executor, and internal/agent) never need to derive it dynamically.
const secretMountBaseDir = "/etc/ottoflow/secrets"

// generatedSecretVolumePrefix prefixes every Secret volume name buildSecretMounts mints
// ("ottoflow-secret-0", "ottoflow-secret-kubeconfig", ...). It is a RESERVED prefix, not just
// a collision-avoidance convention, for two reasons:
//
//   - ensureRunnerSecrets must be able to tell a controller-minted ref volume from an
//     operator-supplied spec.execution.job.volumes entry, because its legacy cross-namespace
//     copy path must never apply to a minted one (see ensureRunnerSecrets).
//   - a minted name colliding with an operator-supplied one would otherwise fail at Job
//     creation with an opaque duplicate-volume-name error from the API server.
//
// buildSecretMounts therefore rejects ANY pre-existing volume whose name starts with this
// prefix, not merely one that happens to equal a name it is about to mint.
const generatedSecretVolumePrefix = "ottoflow-secret-"

// generatedSecretVolumeOriginsAnnotation names the runner Job annotation buildWorkflowRunnerJob
// writes the marshaled origins map to — minted volume name (e.g. "ottoflow-secret-0") to the
// resolvedSecretRef.Origin that produced it, as returned by buildSecretMounts below.
// handleStuckRunnerPod (stuck_runner_pod.go) reads it back to name the originating workflow
// field in a stuck-pod diagnosis (see secretVolumeAttribution).
const generatedSecretVolumeOriginsAnnotation = "ottoflow.nirmata.io/secret-volume-origins"

// maxSecretVolumeOriginsAnnotationBytes caps the secret-volume-origins annotation.
const maxSecretVolumeOriginsAnnotationBytes = 8192

// buildSecretMounts turns the refs collectSecretRefs found (already policy-checked by
// validateSecretRefPolicy) into the pieces buildWorkflowRunnerJob adds to the runner Job:
// volumes, volume mounts, the OTTOFLOW_SECRET_MOUNTS JSON the runner parses to find its
// file-mounted values (see internal/secretmount), and the raw origins map (minted volume name ->
// resolvedSecretRef.Origin) that buildWorkflowRunnerJob marshals onto
// generatedSecretVolumeOriginsAnnotation — marshaling and the annotation-size cap are its
// caller's concern, not this function's, since both are properties of the Job annotation, not of
// the mount itself. Every value ends up mounted as a file — never a literal secret value in the
// Job spec.
//
// MCP env credentials (an MCPServer Spec.Env SecretKeyRef, EnvVarName != "") are routed
// through the file-mount map exactly like auth.secretRef creds, NOT emitted as pod env vars —
// see resolvedSecretRef.EnvVarName for why a pod SecretKeyRef env var would be clobbered.
//
// refs reachable only via an agentRef's Agent.Spec.MCPTools (NeedsRunnerMount==false) are
// skipped entirely: those MCP servers are called from the agent-executor pod, not this Job.
//
// A ref that reached here with a namespace other than runNamespace can only be one
// validateSecretRefPolicy allowed through the operator allowlist (an unlisted cross-namespace
// ref is already a terminal error before this function runs). Native Kubernetes Secret
// volumes/SecretKeyRef can only ever reach a Secret in the pod's own namespace, so such a ref
// still cannot be mounted here; it is reported as a distinct, actionable error rather than
// silently dropped.
//
// existingVolumeNames names every volume already on the Job pod spec before this call —
// operator-supplied spec.execution.job.volumes, plus any the controller itself has already
// added (e.g. agent-executor-ca). Every name minted here carries the reserved
// generatedSecretVolumePrefix, and a pre-existing volume using that prefix is rejected up
// front (see that constant) rather than left to fail with an opaque duplicate-volume-name
// error from the Kubernetes API when the Job is created.
//
// An OPTIONAL MCP env credential (resolvedSecretRef.Optional) gets its own Secret volume with
// SecretVolumeSource.Optional=true, separate from the same Secret's mandatory keys: the flag is
// per-volume, not per-item, so mandatory and optional keys of one Secret cannot share a volume
// without silently making one group behave like the other.
func buildSecretMounts(refs []resolvedSecretRef, runNamespace string, existingVolumeNames map[string]struct{}) (
	volumes []corev1.Volume, mounts []corev1.VolumeMount, mountsJSON string, origins map[string]string, err error,
) {
	for name := range existingVolumeNames {
		if strings.HasPrefix(name, generatedSecretVolumePrefix) {
			return nil, nil, "", nil, fmt.Errorf(
				"volume %q on the runner Job pod spec (likely from spec.execution.job.volumes) uses the "+
					"%q prefix, which is reserved for the Secret volumes OttoFlow mounts for the workflow's "+
					"own Secret references; rename that volume to resolve the conflict",
				name, generatedSecretVolumePrefix)
		}
	}

	var kubeconfigRef *resolvedSecretRef
	type secretKeys struct {
		namespace, name string
		keys            map[string]struct{} // mandatory keys
		optionalKeys    map[string]struct{} // keys from an optional SecretKeyRef only
		// mandatoryOrigins / optionalOrigins accumulate every distinct ref.Origin that
		// contributed a key to the corresponding group (entry.keys / entry.optionalKeys),
		// in first-seen order (deterministic: every walk input is an ordered CRD slice) and
		// deduplicated. A single bySecret entry mints up to two volumes below — one per
		// group — and each volume must carry only the origins that actually produced ITS
		// keys: a Secret referenced both optionally (by one step) and mandatorily (by
		// another) mints two volumes with two different origin sets, not one origin shared
		// by both. secretVolumeAttribution then renders the set as one joined clause. A
		// volume minted from a single contributing field (the common case) renders as that
		// field's origin unchanged; two or more join into one comma-separated clause.
		mandatoryOrigins []string
		optionalOrigins  []string
	}
	bySecret := make(map[string]*secretKeys) // keyed by namespace+"/"+name
	origins = make(map[string]string)        // minted volume name -> origin, returned below as-is

	for i := range refs {
		ref := refs[i]
		if !ref.NeedsRunnerMount {
			continue // resolved by the agent-executor pod, not this Job — see doc comment above
		}
		if ref.Namespace != runNamespace {
			return nil, nil, "", nil, fmt.Errorf(
				"secret ref %s is in allowlisted cross-namespace source %q, but cross-namespace secret mounting "+
					"into the runner Job is not yet supported; move the secret into namespace %q",
				ref.Origin, ref.Namespace, runNamespace)
		}

		// The Secret name becomes SecretVolumeSource.SecretName. It comes from a free-form CRD
		// string field the API server never cross-checks at CR admission, so an invalid name
		// would otherwise surface only at Job create as an opaque API validation error instead
		// of naming the reference that is actually wrong.
		if errs := validation.IsDNS1123Subdomain(ref.Name); len(errs) > 0 {
			return nil, nil, "", nil, fmt.Errorf(
				"secret ref %s cannot be mounted: Secret name %q is not a valid DNS-1123 subdomain (%s)",
				ref.Origin, ref.Name, strings.Join(errs, "; "))
		}

		if ref.IsKubeconfig {
			if kubeconfigRef == nil {
				kubeconfigRef = &ref
			}
			continue
		}

		// The key becomes a projected filename (corev1.KeyToPath.Path) and, on the runner side, a
		// path under the mount directory. It comes from a free-form CRD string field the API
		// server never checks against the Secret data-key rules, so validate it here: without
		// this, a key like "../../etc/passwd" is refused by the API server only when the Job is
		// created, as an opaque `secret.items[0].path: must not contain '..'`, instead of naming
		// the reference that is actually wrong.
		if err := secretmount.ValidateDataKey(ref.Key); err != nil {
			return nil, nil, "", nil, fmt.Errorf("secret ref %s cannot be mounted: %w", ref.Origin, err)
		}

		// MCP env creds (EnvVarName != "") fall through here deliberately: they are mounted as
		// files like every other cred, because the runner resolves them from the mount map.

		secretID := ref.Namespace + "/" + ref.Name
		entry, ok := bySecret[secretID]
		if !ok {
			entry = &secretKeys{
				namespace:    ref.Namespace,
				name:         ref.Name,
				keys:         make(map[string]struct{}),
				optionalKeys: make(map[string]struct{}),
			}
			bySecret[secretID] = entry
		}
		if ref.Optional {
			entry.optionalKeys[ref.Key] = struct{}{}
			entry.optionalOrigins = appendOrigin(entry.optionalOrigins, ref.Origin)
		} else {
			entry.keys[ref.Key] = struct{}{}
			entry.mandatoryOrigins = appendOrigin(entry.mandatoryOrigins, ref.Origin)
		}
	}

	if kubeconfigRef != nil {
		const kubeconfigVolName = generatedSecretVolumePrefix + "kubeconfig"
		origins[kubeconfigVolName] = kubeconfigRef.Origin
		volumes = append(volumes, corev1.Volume{
			Name: kubeconfigVolName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: kubeconfigRef.Name},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{
			Name:      kubeconfigVolName,
			MountPath: secretmount.KubeconfigDir,
			ReadOnly:  true,
		})
	}

	// Sort distinct secrets for a deterministic Job spec (map iteration order is not stable).
	secretIDs := make([]string, 0, len(bySecret))
	for id := range bySecret {
		secretIDs = append(secretIDs, id)
	}
	sort.Strings(secretIDs)

	mountsMap := secretmount.Mounts{}
	volIndex := 0
	for _, id := range secretIDs {
		entry := bySecret[id]
		// A key referenced BOTH optionally and mandatorily is mounted once, as mandatory: the
		// stricter requirement wins, and mounting it twice would leave mountsMap pointing at
		// whichever of the two paths was written last.
		for k := range entry.keys {
			delete(entry.optionalKeys, k)
		}
		for _, group := range []struct {
			keys     map[string]struct{}
			origins  []string
			optional bool
		}{
			{entry.keys, entry.mandatoryOrigins, false},
			{entry.optionalKeys, entry.optionalOrigins, true},
		} {
			if len(group.keys) == 0 {
				continue
			}
			keys := make([]string, 0, len(group.keys))
			for k := range group.keys {
				keys = append(keys, k)
			}
			sort.Strings(keys)

			volName := fmt.Sprintf("%s%d", generatedSecretVolumePrefix, volIndex)
			dir := fmt.Sprintf("%s/%d", secretMountBaseDir, volIndex)
			volIndex++
			origins[volName] = strings.Join(group.origins, ", ")
			items := make([]corev1.KeyToPath, len(keys))
			for j, k := range keys {
				items[j] = corev1.KeyToPath{Key: k, Path: k}
				mountsMap[secretmount.Key(entry.namespace, entry.name, k)] = dir + "/" + k
			}
			secretVolume := &corev1.SecretVolumeSource{SecretName: entry.name, Items: items}
			if group.optional {
				secretVolume.Optional = ptr.To(true)
			}
			volumes = append(volumes, corev1.Volume{
				Name:         volName,
				VolumeSource: corev1.VolumeSource{Secret: secretVolume},
			})
			mounts = append(mounts, corev1.VolumeMount{
				Name:      volName,
				MountPath: dir,
				ReadOnly:  true,
			})
		}
	}

	if len(mountsMap) > 0 {
		b, jsonErr := json.Marshal(mountsMap)
		if jsonErr != nil {
			return nil, nil, "", nil, fmt.Errorf("marshaling %s: %w", secretmount.EnvVar, jsonErr)
		}
		mountsJSON = string(b)
	}

	return volumes, mounts, mountsJSON, origins, nil
}

// appendOrigin appends origin to origins unless it is already present, preserving the order
// origins were first seen in. Used to build each emitted volume's origin set directly from ref
// processing order (an ordered CRD slice), so the result is deterministic without ever ranging
// over a map to derive it.
func appendOrigin(origins []string, origin string) []string {
	if slices.Contains(origins, origin) {
		return origins
	}
	return append(origins, origin)
}
