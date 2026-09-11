/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"

	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// selfSecretRules asks the API server which rules the controller's own identity holds in
// namespace, via a SelfSubjectRulesReview — the same "what can I do" check `kubectl auth can-i
// --list` uses. This is read-only and diagnostic: SelfSubjectRulesReview grants nothing and
// gates nothing (its own doc comment says as much: it "should NOT be used ... to drive
// authorization decisions"), and every OttoFlow install already has permission to issue it —
// `create selfsubjectrulesreviews.authorization.k8s.io` is granted cluster-wide to
// system:authenticated by the built-in system:basic-user ClusterRole, so no chart change is
// needed to call this. The gate on the well-known-LLM-credentials read is a SEPARATE check —
// canGetSecret (secret_access_review.go), a SelfSubjectAccessReview — and that one does decide
// whether the read is attempted; this SelfSubjectRulesReview remains diagnosis only, run after
// a denial (from either check) to explain it, never before one to prevent it.
func selfSecretRules(ctx context.Context, c client.Client, namespace string) (*authorizationv1.SubjectRulesReviewStatus, error) {
	ssrr := &authorizationv1.SelfSubjectRulesReview{
		Spec: authorizationv1.SelfSubjectRulesReviewSpec{Namespace: namespace},
	}
	if err := c.Create(ctx, ssrr); err != nil {
		return nil, err
	}
	return &ssrr.Status, nil
}

// matchesWildcard is for verbs, apiGroups and resources, where RBAC honours "*".
// Returns false on an empty slice: an empty list is never match-all, because SSRR
// rules are synthesized and RBAC itself cannot store verbs: [].
func matchesWildcard(list []string, want string) bool {
	for _, v := range list {
		if v == "*" || v == want {
			return true
		}
	}
	return false
}

// matchesResourceName is for resourceNames ONLY, where "*" is an ORDINARY OBJECT NAME,
// not a wildcard. Measured: a Role with resourceNames:["*"] denies get on
// secrets/ottoflow-llm-credentials and allows get on a Secret literally named "*".
// Reusing matchesWildcard here would report that Role as a full match and tell the
// operator to go hunt a webhook that does not exist — on an install whose actual fault
// is the literal asterisk, which is exactly what this advice exists to name.
// An EMPTY resourceNames slice grants all names, so it must return true.
func matchesResourceName(names []string, want string) bool {
	if len(names) == 0 {
		return true
	}
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// sanitizeForAdvice makes a single verb or resourceName safe to echo back into an error message
// that ends up in WorkflowRun.Status: it drops every byte < 0x20 and 0x7f (Kubernetes applies no
// DNS validation to resourceNames, so a Role in the cluster can carry a newline plus a forged
// "Confirm with: kubectl ..." line), truncates to 64 bytes with a "…" marker, and quotes the
// result via %q so it always renders as one self-contained Go-syntax string literal no matter
// what the rule actually contained.
func sanitizeForAdvice(s string) string {
	stripped := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 0x20 && c != 0x7f {
			stripped = append(stripped, c)
		}
	}
	if len(stripped) > 64 {
		truncated := make([]byte, 64, 64+len("…"))
		copy(truncated, stripped[:64])
		stripped = append(truncated, "…"...)
	}
	return fmt.Sprintf("%q", string(stripped))
}

// formatAdviceList sanitizes, sorts, dedupes, and renders a set of server-influenced strings
// (granted verbs or resourceNames) for embedding in an advice message via a single %s. Capped at
// 8: exactly 8 entries render all eight with no ellipsis; 9 or more render the first eight plus
// " (+N more)". The result is always returned as one opaque value to be substituted through %s —
// never treat it as (or concatenate it into) a format string, since a sanitized-but-still-quoted
// entry can legitimately contain a literal '%' byte.
func formatAdviceList(items map[string]struct{}) string {
	quoted := make([]string, 0, len(items))
	for raw := range items {
		quoted = append(quoted, sanitizeForAdvice(raw))
	}
	slices.Sort(quoted)
	// Compact de-duplicates adjacent equal entries: two distinct raw values can collapse to
	// the same quoted, truncated form (e.g. differing only past the 64-byte cutoff).
	quoted = slices.Compact(quoted)
	if len(quoted) <= 8 {
		return strings.Join(quoted, ", ")
	}
	return fmt.Sprintf("%s (+%d more)", strings.Join(quoted[:8], ", "), len(quoted)-8)
}

// secretAccessAdvice builds a diagnostic addendum for a denied Secret read, naming which Secret
// rules the controller's identity actually holds in key.Namespace so an operator can tell "no
// Role at all" apart from "a Role that names the wrong Secret" — both currently surface as the
// identical 403. It NEVER changes the outcome of the read: by the time this is called the
// caller has already decided the run fails; this only decides what the failure message says.
// Returns "" whenever nothing useful or safe can be said, or a string beginning with a single
// leading space, ready to splice directly into an existing message.
func secretAccessAdvice(ctx context.Context, c client.Client, key types.NamespacedName, runNamespace string) string {
	if c == nil {
		return ""
	}
	// The SSRR is never issued against any namespace other than the WorkflowRun's own, so a
	// Secret in the install namespace (which the runner Secret-copy path reaches by default via
	// getReferencedWorkflow's install-namespace fallback) never has its RBAC described in a tenant's
	// status. The comparison is on key.Namespace, NOT on any other incoming parameter: it is
	// key.Namespace whose rules the SSRR result below is actually about, so the
	// sourceNamespace == "" default reassignment in ensureRunnerSecrets is correctly
	// treated as eligible here — a later refactor must not move this comparison to key off
	// anything other than key.Namespace.
	if key.Namespace != runNamespace {
		return "" // never issue the SSRR at all
	}
	st, err := selfSecretRules(ctx, c, key.Namespace)
	if err != nil || st == nil || st.Incomplete || st.EvaluationError != "" {
		// Incomplete means the rule list is not exhaustive, so examining it could assert "no
		// grant exists" when an authorization webhook holds one the SSRR could not enumerate.
		return ""
	}

	// A candidate rule must match apiGroup AND resource — not resource alone (measured:
	// apiGroups:["ottoflow.nirmata.io"], resources:["secrets"] is reported by SSRR and denied by
	// the authorizer, because it names a different API group's "secrets" resource, not core
	// v1 Secrets).
	var (
		candidate         bool
		full              bool
		verbs             = map[string]struct{}{}
		names             = map[string]struct{}{}
		namesUnrestricted bool
	)
	for _, rule := range st.ResourceRules {
		if !matchesWildcard(rule.APIGroups, "") || !matchesWildcard(rule.Resources, "secrets") {
			continue
		}
		candidate = true
		for _, v := range rule.Verbs {
			verbs[v] = struct{}{}
		}
		if len(rule.ResourceNames) == 0 {
			namesUnrestricted = true
		} else {
			for _, n := range rule.ResourceNames {
				names[n] = struct{}{}
			}
		}
		if matchesWildcard(rule.Verbs, "get") && matchesResourceName(rule.ResourceNames, key.Name) {
			full = true
		}
	}

	switch {
	case full:
		// Arm A: full match on all four axes. The API server reports the controller does hold
		// this access, so the denial did not come from a missing Role — look for an
		// authorization webhook, or a grant changed between the check and the read.
		return fmt.Sprintf(
			" The API server reports the controller identity DOES currently hold get on "+
				"secrets/%s in namespace %s, so this denial did not come from a missing Role — "+
				"look for an authorization webhook, or a grant that changed between the check "+
				"and the read.",
			key.Name, key.Namespace)
	case candidate:
		// Arm B: apiGroup+resource match, verb and/or name mismatch. This is the arm that
		// carries information the 403 alone cannot: exactly what is granted, versus exactly
		// what this read needs.
		verbList := formatAdviceList(verbs)
		nameList := "all names (no resourceNames restriction on at least one matching rule)"
		if !namesUnrestricted {
			nameList = formatAdviceList(names)
		}
		return fmt.Sprintf(
			" The API server reports the controller's Secret rules in namespace %s grant verbs "+
				"[%s] on resourceNames [%s] — this read needs get on %q.",
			key.Namespace, verbList, nameList, key.Name)
	default:
		// Arm C: no rule mentions secrets in that apiGroup. Confirms no grant exists, so the
		// Role remediation above is correct.
		return fmt.Sprintf(
			" The API server reports no rule grants any access to secrets in namespace %s, "+
				"confirming no Role exists yet for this Secret.",
			key.Namespace)
	}
}
