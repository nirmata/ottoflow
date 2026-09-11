/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

// errSecretAccessDenied is wrapped into every error forbiddenSecretRoleError and
// forbiddenSecretCopyError build, so a caller can classify a Forbidden Secret read with
// errors.Is regardless of how many times the error was further wrapped (e.g. via
// fmt.Errorf("...: %w", err)) before it reached setRunFailed. See
// ottoflowv1alpha1.WorkflowRunFailureReasonSecretAccessDenied.
var errSecretAccessDenied = errors.New("secret access denied")

// secretAccessDeniedReason reports WorkflowRunFailureReasonSecretAccessDenied when err (or
// anything it wraps) is errSecretAccessDenied, and the empty reason for every other error
// (including nil) — the value setRunFailed's reason parameter expects.
func secretAccessDeniedReason(err error) ottoflowv1alpha1.WorkflowRunFailureReason {
	if errors.Is(err, errSecretAccessDenied) {
		return ottoflowv1alpha1.WorkflowRunFailureReasonSecretAccessDenied
	}
	return ""
}

// forbiddenSecretRoleError wraps a Forbidden Secret read in an actionable message. The
// controller is designed to hold no Secret access beyond what an operator grants per Secret, so
// a denial is configuration to fix rather than a condition to wait out: when returned from a
// build/reconcile step it drives the run to a terminal Failed status (setRunFailed) rather than
// an infinite requeue. See docs/user/rbac-secret-access.md.
//
// r.ControllerNamespace is the controller's own install namespace (wired from the --namespace
// flag — the same mechanism main.go already uses for certmanager.BootstrapWebhookCerts). It
// decides which of two DIFFERENT fixes the message gives:
//
//   - key.Namespace != r.ControllerNamespace (the common case): the rbac.secretAccess.controller
//     Helm values path is the right fix, and the message names it (never
//     rbac.*ClusterRole.extraResources, which docs/user/rbac-secret-access.md says must never
//     carry a secrets grant).
//   - key.Namespace == r.ControllerNamespace: that values path is a dead end. The Helm shortcut
//     refuses to render a Role in the install namespace (docs/user/rbac-secret-access.md, "Keep
//     workflow Secrets out of the install namespace") — blast-radius hygiene: workflow Secrets
//     belong in a tenant namespace, not the control-plane namespace. Pointing operators at
//     rbac.secretAccess.controller here would send them straight into that refusal, so the
//     message instead says to create the Role+RoleBinding directly in that namespace.
//
// r.ControllerNamespace == "" (unknown — e.g. ControllerNamespace unset, or a direct call from
// a unit test) falls back to the first case.
//
// The message also gives the exact `kubectl auth can-i` command an operator can run to confirm
// the denial before and after granting access. <install-ns> and <controller-sa> are literal
// placeholders in that command — the controller does not know its own ServiceAccount name at
// this call site, so it cannot substitute it.
//
// The command is backtick-delimited and the wrapped error follows in its own parenthetical,
// rather than a bare ": %w" straight after "<controller-sa>": without a clear delimiter, copying
// "the command" up to the trailing wrapped error would carry along the ":" separator, producing
// an invalid --as value ending in a stray colon.
//
// runNamespace is the WorkflowRun's own namespace. It is handed to secretAccessAdvice
// (secret_access_advice.go) purely to build a diagnostic SelfSubjectRulesReview addendum, which
// is spliced in right after the backtick-delimited command and before " (underlying error:". The
// addendum never changes whether this error is returned — only what it says — and is "" whenever
// secretAccessAdvice's own preconditions don't hold (nil Client, key.Namespace != runNamespace,
// or an inconclusive/failed SSRR), in which case the message carries no addendum at all.
func (r *WorkflowRunReconciler) forbiddenSecretRoleError(ctx context.Context, key types.NamespacedName, runNamespace string, err error) error {
	fix := fmt.Sprintf(
		"Grant the controller ServiceAccount get on this Secret via a namespaced Role+RoleBinding "+
			"in namespace %q (rules: secrets get, resourceNames: [%q]) — see rbac.secretAccess.controller "+
			"in values.yaml, or docs/user/rbac-secret-access.md.",
		key.Namespace, key.Name)
	if r.ControllerNamespace != "" && key.Namespace == r.ControllerNamespace {
		fix = fmt.Sprintf(
			"This Secret is in OttoFlow's own install namespace %q, where the chart's Secret-RBAC "+
				"shortcut deliberately refuses to render a Role (blast-radius hygiene) — create a Role "+
				"and RoleBinding directly in that namespace granting the controller ServiceAccount get "+
				"on this Secret (resourceNames: [%q]); see docs/user/rbac-secret-access.md.",
			key.Namespace, key.Name)
	}
	advice := secretAccessAdvice(ctx, r.Client, key, runNamespace)
	// Trailing ": %w" wraps errSecretAccessDenied (Go 1.20+ supports multiple %w verbs in one
	// fmt.Errorf), so this error classifies via errors.Is(err, errSecretAccessDenied) in a single
	// call rather than by wrapping a second time.
	return fmt.Errorf(
		"reading Secret %s is Forbidden: OttoFlow ships with no Secret access. %s Confirm with: "+
			"`kubectl auth can-i get secrets/%s -n %s --as=system:serviceaccount:<install-ns>:<controller-sa>`%s "+
			"(underlying error: %w): %w",
		key, fix, key.Name, key.Namespace, advice, err, errSecretAccessDenied)
}

// nirmataTokenEnvNames are the three variables the agent-executor accepts as a Nirmata LLM
// token (exec_handler.go). Any one supplied via spec.execution.job.env means the run carries
// its own credentials, so a denied well-known-Secret read cannot affect it. Supplying them is
// a supported, documented path (see the "Explicit spec.execution.job.env entries win" comment
// above the call site in buildWorkflowRunnerJob, and exec_handler.go's own remediation text).
//
// This is a strict SUBSET of executor.LLMEnvAllowlist (only the credential keys, not
// NIRMATA_URL/NIRMATA_LLM_MODEL), so it can't simply be that allowlist filtered by position —
// but it must stay a subset of it: the runner forwards only allowlisted names to the
// agent-executor, so a name listed here but not there would count as explicit credentials
// without ever reaching the agent-executor. TestNirmataTokenEnvNames_SubsetOfLLMEnvAllowlist
// (secret_access_gate_test.go) pins the subset, so a rename on either side fails a test
// instead of silently letting hasExplicitNirmataCreds stop recognizing a still-valid token name.
var nirmataTokenEnvNames = []string{
	"NIRMATA_LLM_TOKEN", "NIRMATA_LLM_SERVICEACCOUNT_TOKEN", "NIRMATA_LLM_APIKEY",
}

// hasExplicitNirmataCreds reports whether the WorkflowRun's own spec.execution.job.env already
// supplies a USABLE Nirmata LLM token, in which case a denied read of the well-known
// credentials Secret cannot cost this run anything.
//
// Takes the actual EnvVar entries, not just a set of names: an entry whose Name matches but
// whose Value is the empty string and whose ValueFrom is nil supplies no usable token at all
// (`{name: NIRMATA_LLM_TOKEN, value: ""}` is a name present with nothing behind it) and must
// not be treated as explicit credentials — that would silently disable the preflight for a run
// that still has no working token. An entry with ValueFrom set DOES count: its value comes from
// a Secret/ConfigMap key the controller does not resolve here, so this function cannot know
// whether it's empty and must not guess — treating it as explicit is the fail-open-only-when-
// genuinely-ambiguous choice, consistent with this whole feature only ever adding a NEW
// terminal failure where the run was already going to fail anyway.
func hasExplicitNirmataCreds(env []corev1.EnvVar) bool {
	names := make(map[string]struct{}, len(nirmataTokenEnvNames))
	for _, n := range nirmataTokenEnvNames {
		names[n] = struct{}{}
	}
	for _, e := range env {
		if _, ok := names[e.Name]; !ok {
			continue
		}
		if e.ValueFrom != nil || e.Value != "" {
			return true
		}
	}
	return false
}

// isTransientCredentialWalkError reports whether err — returned by the needsCreds() closure
// handleDeniedLLMCredentialsRead calls, ultimately workflowNeedsNirmataLLMCredentials's walk
// (secret_needs_creds.go) — is a genuine, retryable API condition rather than a real
// misconfiguration.
// Only agentRefNeeds' Agent Get (the walk's one uncached, direct-reader read) can plausibly
// produce one of these; every other error the walk can return (maxWorkflowRefDepth exceeded, a
// NotFound sub-Workflow/StepTemplate) is a static misconfiguration a retry cannot fix and must
// stay terminal.
func isTransientCredentialWalkError(err error) bool {
	return apierrors.IsInternalError(err) ||
		apierrors.IsServiceUnavailable(err) ||
		apierrors.IsTimeout(err) ||
		apierrors.IsTooManyRequests(err)
}

func (r *WorkflowRunReconciler) handleDeniedLLMCredentialsRead(
	ctx context.Context,
	workflowRun *ottoflowv1alpha1.WorkflowRun,
	key types.NamespacedName,
	needsCreds func() (bool, error),
	cause error,
) error {
	// Always record the Warning Event first, whether or not the run is about to fail, so the Event
	// contract holds either way — an operator who expected injection needs the signal even when
	// the run itself is about to fail outright for the same underlying reason.
	if r.EventRecorder != nil {
		r.EventRecorder.Eventf(workflowRun, nil, corev1.EventTypeWarning,
			"LLMCredentialsForbidden",
			"SkippedInjection",
			"cannot read LLM credentials Secret %s (Forbidden): grant the controller ServiceAccount "+
				"get on this Secret via a namespaced Role+RoleBinding in namespace %q; see docs/user/rbac-secret-access.md",
			key, key.Namespace)
	}
	needs, nerr := needsCreds()
	if nerr != nil {
		if isTransientCredentialWalkError(nerr) {
			return &transientBuildError{err: nerr}
		}
		return nerr
	}
	if needs {
		// A reachable Nirmata-provider AgentRef needs this credential and the run
		// supplies none of its own: proceeding would only fail later, less clearly, at
		// the agent-executor. Fail now with the actionable RBAC remediation. A plain
		// error (not transientBuildError) so reconcileJobExecution's non-transient
		// branch drives this to a terminal Failed status instead of requeuing forever.
		return r.forbiddenSecretRoleError(ctx, key, workflowRun.Namespace, cause)
	}
	// Skip injection benignly — either nothing reachable needs a Nirmata token, or the
	// run already supplies its own via spec.execution.job.env.
	klog.V(2).InfoS("skipping well-known LLM credential injection: Secret read Forbidden",
		"namespace", key.Namespace, "secret", key.Name)
	return nil
}

func (r *WorkflowRunReconciler) directReader() client.Reader {
	return preferAPIReader(r.APIReader, r.Client)
}

func forbiddenSecretCopyError(sourceKey, runnerKey types.NamespacedName, err error) error {
	// Trailing ": %w" wraps errSecretAccessDenied, same as forbiddenSecretRoleError above — see
	// its comment for why one fmt.Errorf call with two %w verbs is enough.
	return fmt.Errorf(
		"copying Secret %s to runner namespace %q as %q is Forbidden: this cross-namespace "+
			"Secret-volume copy needs a Role granting secrets create in namespace %q, which — "+
			"unlike get — cannot be scoped to a single Secret name (resourceNames only supports "+
			"get/update/delete). Recommended: place Secret %q directly in namespace %q so the "+
			"WorkflowRun's execution.job.volumes reads it in place and no cross-namespace copy is "+
			"needed (only secrets get is then required); see docs/user/rbac-secret-access.md: %w: %w",
		sourceKey, runnerKey.Namespace, runnerKey.Name, runnerKey.Namespace, sourceKey.Name, runnerKey.Namespace, err, errSecretAccessDenied)
}
