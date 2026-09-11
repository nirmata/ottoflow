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

	authorizationv1 "k8s.io/api/authorization/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

// ssrrClient wraps base to intercept SelfSubjectRulesReview Create calls, returning status (or
// createErr, when non-nil, instead of ever touching status). When forbiddenGetNamespace is
// non-empty it additionally makes every Secret Get scoped to that namespace return Forbidden, via
// secretGetErrorReader (controller_unit_test.go) namespace-scoped — the two seams
// secretAccessAdvice's tests need, combined so one fixture can drive both a real denial path and
// the advice it produces. calls, if non-nil, counts SelfSubjectRulesReview Create attempts.
func ssrrClient(base client.WithWatch, forbiddenGetNamespace string, status authorizationv1.SubjectRulesReviewStatus, createErr error, calls *int) client.WithWatch {
	wrapped := base
	if forbiddenGetNamespace != "" {
		wrapped = secretGetErrorReader(wrapped,
			func(key client.ObjectKey) bool { return key.Namespace == forbiddenGetNamespace },
			func(key client.ObjectKey) error {
				return apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, key.Name, errors.New("no Role"))
			},
		)
	}
	return interceptor.NewClient(wrapped, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			ssrr, ok := obj.(*authorizationv1.SelfSubjectRulesReview)
			if !ok {
				return c.Create(ctx, obj, opts...)
			}
			if calls != nil {
				*calls++
			}
			if createErr != nil {
				return createErr
			}
			ssrr.Status = status
			return nil
		},
	})
}

func newSSRRTestClient(status authorizationv1.SubjectRulesReviewStatus) client.Client {
	base := fake.NewClientBuilder().WithScheme(unitTestScheme).Build()
	return ssrrClient(base, "", status, nil, nil)
}

// classifyAdvice maps a secretAccessAdvice return value to the arm that produced it, using the
// distinctive phrase each arm's message carries. Empty input classifies as "" (no advice).
func classifyAdvice(t *testing.T, advice string) string {
	t.Helper()
	switch {
	case advice == "":
		return ""
	case !strings.HasPrefix(advice, " ") || strings.HasPrefix(advice, "  "):
		t.Fatalf("advice must be \"\" or begin with exactly one leading space, got: %q", advice)
		return "?"
	case strings.Contains(advice, "DOES currently hold"):
		return "A"
	case strings.Contains(advice, "grant verbs ["):
		return "B"
	case strings.Contains(advice, "no rule grants any access"):
		return "C"
	default:
		t.Fatalf("advice matched no known arm: %q", advice)
		return "?"
	}
}

// TestSecretAccessAdvice_Arms tables the six measured RBAC states secretAccessAdvice must tell
// apart, pinning which of the three arms (A: fully granted, B: partial match, C: no grant) each
// one produces.
func TestSecretAccessAdvice_Arms(t *testing.T) {
	key := types.NamespacedName{Namespace: "tenant-a", Name: "ottoflow-llm-credentials"}

	tests := []struct {
		name  string
		rules []authorizationv1.ResourceRule
		want  string
	}{
		{
			name: "typo'd resourceNames",
			rules: []authorizationv1.ResourceRule{{
				Verbs: []string{"get"}, APIGroups: []string{""}, Resources: []string{"secrets"},
				ResourceNames: []string{"ottoflow-llm-credentials-TYPO"},
			}},
			want: "B",
		},
		{
			name:  "no grant",
			rules: nil,
			want:  "C",
		},
		{
			name: "correct name",
			rules: []authorizationv1.ResourceRule{{
				Verbs: []string{"get"}, APIGroups: []string{""}, Resources: []string{"secrets"},
				ResourceNames: []string{"ottoflow-llm-credentials"},
			}},
			want: "A",
		},
		{
			name: "blanket secrets get, no resourceNames",
			rules: []authorizationv1.ResourceRule{{
				Verbs: []string{"get"}, APIGroups: []string{""}, Resources: []string{"secrets"},
			}},
			want: "A",
		},
		{
			name: "right name, wrong verb",
			rules: []authorizationv1.ResourceRule{{
				Verbs: []string{"list"}, APIGroups: []string{""}, Resources: []string{"secrets"},
				ResourceNames: []string{"ottoflow-llm-credentials"},
			}},
			want: "B",
		},
		{
			name: "wildcard verbs/apiGroups/resources",
			rules: []authorizationv1.ResourceRule{{
				Verbs: []string{"*"}, APIGroups: []string{"*"}, Resources: []string{"*"},
			}},
			want: "A",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newSSRRTestClient(authorizationv1.SubjectRulesReviewStatus{ResourceRules: tt.rules})
			advice := secretAccessAdvice(context.Background(), c, key, key.Namespace)
			if got := classifyAdvice(t, advice); got != tt.want {
				t.Errorf("arm = %q, want %q (advice: %q)", got, tt.want, advice)
			}
		})
	}
}

// TestSecretAccessAdvice_WildcardVerbsResourcesGroupsCountAsGranted pins state 6 from the table
// above on its own: verbs/apiGroups/resources all "*" must classify as a full match (arm A), not
// a partial one.
func TestSecretAccessAdvice_WildcardVerbsResourcesGroupsCountAsGranted(t *testing.T) {
	key := types.NamespacedName{Namespace: "tenant-a", Name: "any-secret"}
	c := newSSRRTestClient(authorizationv1.SubjectRulesReviewStatus{
		ResourceRules: []authorizationv1.ResourceRule{{
			Verbs: []string{"*"}, APIGroups: []string{"*"}, Resources: []string{"*"},
		}},
	})
	advice := secretAccessAdvice(context.Background(), c, key, key.Namespace)
	if got := classifyAdvice(t, advice); got != "A" {
		t.Fatalf("arm = %q, want A (advice: %q)", got, advice)
	}
}

// TestSecretAccessAdvice_AsteriskInResourceNamesIsLiteral proves the load-bearing distinction
// this file exists to make: a Role with resourceNames:["*"] does NOT grant a Secret whose name
// differs — "*" there is an ordinary object name, not a wildcard — so this must classify as B
// (partial match), never A.
func TestSecretAccessAdvice_AsteriskInResourceNamesIsLiteral(t *testing.T) {
	key := types.NamespacedName{Namespace: "tenant-a", Name: "ottoflow-llm-credentials"}
	c := newSSRRTestClient(authorizationv1.SubjectRulesReviewStatus{
		ResourceRules: []authorizationv1.ResourceRule{{
			Verbs: []string{"get"}, APIGroups: []string{""}, Resources: []string{"secrets"},
			ResourceNames: []string{"*"},
		}},
	})
	advice := secretAccessAdvice(context.Background(), c, key, key.Namespace)
	if got := classifyAdvice(t, advice); got != "B" {
		t.Fatalf("resourceNames:[\"*\"] must classify as B (literal name, not wildcard), got %q (advice: %q)", got, advice)
	}
	if !strings.Contains(advice, `"*"`) {
		t.Errorf("expected the literal asterisk to be named in the advice so the operator can find it, got: %q", advice)
	}
}

// TestSecretAccessAdvice_EmptyResourceNamesGrantsAllNames proves the other half of that
// distinction: an ABSENT resourceNames list (as opposed to a literal "*" entry) really does grant
// every name, per RBAC semantics — this must classify as A. A naive slices.Contains check against
// an empty slice would wrongly return false and demote this to B.
func TestSecretAccessAdvice_EmptyResourceNamesGrantsAllNames(t *testing.T) {
	key := types.NamespacedName{Namespace: "tenant-a", Name: "ottoflow-llm-credentials"}
	c := newSSRRTestClient(authorizationv1.SubjectRulesReviewStatus{
		ResourceRules: []authorizationv1.ResourceRule{{
			Verbs: []string{"get"}, APIGroups: []string{""}, Resources: []string{"secrets"},
			// ResourceNames deliberately omitted.
		}},
	})
	advice := secretAccessAdvice(context.Background(), c, key, key.Namespace)
	if got := classifyAdvice(t, advice); got != "A" {
		t.Fatalf("an empty resourceNames slice grants every name and must classify as A, got %q (advice: %q)", got, advice)
	}
}

// TestSecretAccessAdvice_ApiGroupMismatchIsNotACandidate proves a rule must match apiGroup AND
// resource to be considered at all: a rule granting "secrets" in a foreign API group is reported
// by the SSRR and denied by the real authorizer, so it must not count as a partial match (B) — it
// must fall all the way through to C, same as no rule at all.
func TestSecretAccessAdvice_ApiGroupMismatchIsNotACandidate(t *testing.T) {
	key := types.NamespacedName{Namespace: "tenant-a", Name: "ottoflow-llm-credentials"}
	c := newSSRRTestClient(authorizationv1.SubjectRulesReviewStatus{
		ResourceRules: []authorizationv1.ResourceRule{{
			Verbs: []string{"get"}, APIGroups: []string{"ottoflow.nirmata.io"}, Resources: []string{"secrets"},
			ResourceNames: []string{"ottoflow-llm-credentials"},
		}},
	})
	advice := secretAccessAdvice(context.Background(), c, key, key.Namespace)
	if got := classifyAdvice(t, advice); got != "C" {
		t.Fatalf("a foreign-apiGroup secrets rule must classify as C, not B, got %q (advice: %q)", got, advice)
	}
}

// TestSecretAccessAdvice_IncompletePreemptsEveryArm proves Incomplete short-circuits before any
// rule is examined, in both directions: a rule set that would otherwise be a full match (A) and
// one that would otherwise be no grant at all (C) must both come back "" once Incomplete is set,
// because an incomplete rule list cannot support asserting either "granted" or "not granted".
func TestSecretAccessAdvice_IncompletePreemptsEveryArm(t *testing.T) {
	key := types.NamespacedName{Namespace: "tenant-a", Name: "ottoflow-llm-credentials"}

	wouldBeA := authorizationv1.SubjectRulesReviewStatus{
		Incomplete: true,
		ResourceRules: []authorizationv1.ResourceRule{{
			Verbs: []string{"*"}, APIGroups: []string{"*"}, Resources: []string{"*"},
		}},
	}
	if advice := secretAccessAdvice(context.Background(), newSSRRTestClient(wouldBeA), key, key.Namespace); advice != "" {
		t.Errorf("Incomplete must preempt an otherwise-full match, got: %q", advice)
	}

	wouldBeC := authorizationv1.SubjectRulesReviewStatus{Incomplete: true}
	if advice := secretAccessAdvice(context.Background(), newSSRRTestClient(wouldBeC), key, key.Namespace); advice != "" {
		t.Errorf("Incomplete must preempt an otherwise-empty rule set, got: %q", advice)
	}
}

// TestSecretAccessAdvice_ForeignNamespaceProducesNoAdviceAndNoSSRR proves the namespace
// precondition is checked before the SSRR is ever issued, not merely before its result is used:
// when key.Namespace != runNamespace, secretAccessAdvice must return "" AND must not have called
// Create at all — an install-namespace Secret read must never leak that namespace's own RBAC
// rules into a tenant's WorkflowRun.Status.
func TestSecretAccessAdvice_ForeignNamespaceProducesNoAdviceAndNoSSRR(t *testing.T) {
	key := types.NamespacedName{Namespace: "ottoflow", Name: "ottoflow-webhook.ottoflow.svc.tls-ca"}
	var calls int
	base := fake.NewClientBuilder().WithScheme(unitTestScheme).Build()
	c := ssrrClient(base, "", authorizationv1.SubjectRulesReviewStatus{
		ResourceRules: []authorizationv1.ResourceRule{{Verbs: []string{"*"}, APIGroups: []string{"*"}, Resources: []string{"*"}}},
	}, nil, &calls)

	advice := secretAccessAdvice(context.Background(), c, key, "tenant-a" /* runNamespace != key.Namespace */)
	if advice != "" {
		t.Errorf("expected no advice across namespaces, got: %q", advice)
	}
	if calls != 0 {
		t.Errorf("expected zero SelfSubjectRulesReview Create calls, got %d", calls)
	}
}

// TestSecretAccessAdvice_ControlCharsAndLengthAreStripped proves the sanitizer runs before any
// rule-supplied text is echoed: a resourceName carrying a literal newline plus a forged
// "Confirm with:" line must never surface that newline (Kubernetes applies no DNS validation to
// resourceNames, so an attacker with Role-create rights in the namespace could otherwise inject a
// fake log line), and an oversized resourceName must be truncated rather than reproduced in full.
func TestSecretAccessAdvice_ControlCharsAndLengthAreStripped(t *testing.T) {
	key := types.NamespacedName{Namespace: "tenant-a", Name: "ottoflow-llm-credentials"}

	t.Run("control chars stripped", func(t *testing.T) {
		forged := "evil-name\nConfirm with: `kubectl delete ns tenant-a --as=cluster-admin`"
		c := newSSRRTestClient(authorizationv1.SubjectRulesReviewStatus{
			ResourceRules: []authorizationv1.ResourceRule{{
				Verbs: []string{"list"}, APIGroups: []string{""}, Resources: []string{"secrets"},
				ResourceNames: []string{forged},
			}},
		})
		advice := secretAccessAdvice(context.Background(), c, key, key.Namespace)
		if strings.Contains(advice, "\n") {
			t.Errorf("advice must never contain a raw newline byte, got: %q", advice)
		}
	})

	t.Run("length capped", func(t *testing.T) {
		long := strings.Repeat("b", 300)
		c := newSSRRTestClient(authorizationv1.SubjectRulesReviewStatus{
			ResourceRules: []authorizationv1.ResourceRule{{
				Verbs: []string{"list"}, APIGroups: []string{""}, Resources: []string{"secrets"},
				ResourceNames: []string{long},
			}},
		})
		advice := secretAccessAdvice(context.Background(), c, key, key.Namespace)
		if strings.Contains(advice, strings.Repeat("b", 65)) {
			t.Errorf("expected the 300-byte resourceName to be truncated well before 65 bytes, got: %q", advice)
		}
		if !strings.Contains(advice, "…") {
			t.Errorf("expected a truncation marker in the advice, got: %q", advice)
		}
	})
}

// TestSecretAccessAdvice_CapBoundary pins the exact cap edge: 8 distinct resourceNames render all
// eight with no "+N more" suffix, and 9 render only the first eight plus " (+1 more)".
func TestSecretAccessAdvice_CapBoundary(t *testing.T) {
	key := types.NamespacedName{Namespace: "tenant-a", Name: "ottoflow-llm-credentials"}
	names := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = "name-" + string(rune('0'+i))
		}
		return out
	}

	t.Run("exactly 8, no ellipsis", func(t *testing.T) {
		c := newSSRRTestClient(authorizationv1.SubjectRulesReviewStatus{
			ResourceRules: []authorizationv1.ResourceRule{{
				Verbs: []string{"list"}, APIGroups: []string{""}, Resources: []string{"secrets"},
				ResourceNames: names(8),
			}},
		})
		advice := secretAccessAdvice(context.Background(), c, key, key.Namespace)
		for _, n := range names(8) {
			if !strings.Contains(advice, n) {
				t.Errorf("expected %q to be listed, advice: %q", n, advice)
			}
		}
		if strings.Contains(advice, "more)") {
			t.Errorf("exactly 8 entries must render with no ellipsis, got: %q", advice)
		}
	})

	t.Run("9 entries, +1 more", func(t *testing.T) {
		c := newSSRRTestClient(authorizationv1.SubjectRulesReviewStatus{
			ResourceRules: []authorizationv1.ResourceRule{{
				Verbs: []string{"list"}, APIGroups: []string{""}, Resources: []string{"secrets"},
				ResourceNames: names(9),
			}},
		})
		advice := secretAccessAdvice(context.Background(), c, key, key.Namespace)
		if !strings.Contains(advice, "(+1 more)") {
			t.Errorf("expected a \"(+1 more)\" suffix for 9 entries, advice: %q", advice)
		}
		if strings.Contains(advice, "name-8") {
			t.Errorf("expected the 9th entry to be folded into the +1 more count, not listed, advice: %q", advice)
		}
		if !strings.Contains(advice, "name-7") {
			t.Errorf("expected the first 8 entries to still be listed, advice: %q", advice)
		}
	})
}

// --- forbiddenSecretRoleError: splice point, byte-identical baseline, and failure modes -------

// baselineForbiddenSecretRoleError is the exact message forbiddenSecretRoleError produces when
// secretAccessAdvice contributes nothing, written out by hand rather than derived from the
// implementation. Tests compare against this literal, not against a call into the current
// implementation with advice suppressed, so a regression that introduces a stray space or
// reorders the splice is caught even if it happens to leave advice-less output "close enough".
func baselineForbiddenSecretRoleError(key types.NamespacedName, fix string, underlying error) string {
	return "reading Secret " + key.String() + " is Forbidden: OttoFlow ships with no Secret access. " + fix +
		" Confirm with: `kubectl auth can-i get secrets/" + key.Name + " -n " + key.Namespace +
		" --as=system:serviceaccount:<install-ns>:<controller-sa>` (underlying error: " + underlying.Error() +
		"): secret access denied"
}

func valuesPathFix(key types.NamespacedName) string {
	return `Grant the controller ServiceAccount get on this Secret via a namespaced Role+RoleBinding ` +
		`in namespace "` + key.Namespace + `" (rules: secrets get, resourceNames: ["` + key.Name + `"]) ` +
		`— see rbac.secretAccess.controller in values.yaml, or docs/user/rbac-secret-access.md.`
}

// TestForbiddenSecretRoleError_EmptyAdviceIsByteIdentical proves that when secretAccessAdvice's
// own precondition fails for a reason OTHER than a nil Client (here: key.Namespace !=
// runNamespace), forbiddenSecretRoleError's output is byte-identical to the advice-less baseline.
func TestForbiddenSecretRoleError_EmptyAdviceIsByteIdentical(t *testing.T) {
	key := types.NamespacedName{Namespace: "tenant-a", Name: "db-creds"}
	underlying := errors.New("no Role")
	base := fake.NewClientBuilder().WithScheme(unitTestScheme).Build()
	// A working, non-nil Client that WOULD answer with a full grant if asked — proving the
	// byte-identical result here comes from the namespace-mismatch precondition, not from an
	// unreachable or broken client.
	c := ssrrClient(base, "", authorizationv1.SubjectRulesReviewStatus{
		ResourceRules: []authorizationv1.ResourceRule{{Verbs: []string{"*"}, APIGroups: []string{"*"}, Resources: []string{"*"}}},
	}, nil, nil)
	r := &WorkflowRunReconciler{Client: c}

	got := r.forbiddenSecretRoleError(context.Background(), key, "some-other-namespace", underlying).Error()
	want := baselineForbiddenSecretRoleError(key, valuesPathFix(key), underlying)
	if got != want {
		t.Errorf("output not byte-identical to the advice-less baseline:\n got:  %q\n want: %q", got, want)
	}
}

// TestForbiddenSecretRoleError_NilClientIsByteIdentical proves the Client==nil precondition on
// its own: a reconciler with no Client configured must still produce the exact baseline message,
// with no panic.
func TestForbiddenSecretRoleError_NilClientIsByteIdentical(t *testing.T) {
	key := types.NamespacedName{Namespace: "tenant-a", Name: "db-creds"}
	underlying := errors.New("no Role")
	r := &WorkflowRunReconciler{}

	got := r.forbiddenSecretRoleError(context.Background(), key, key.Namespace, underlying).Error()
	want := baselineForbiddenSecretRoleError(key, valuesPathFix(key), underlying)
	if got != want {
		t.Errorf("output not byte-identical to the advice-less baseline:\n got:  %q\n want: %q", got, want)
	}
}

// TestForbiddenSecretRoleError_NonEmptyAdviceExactString asserts the FULL message byte-for-byte
// with advice present, not just a substring — TestForbiddenSecretRoleError_EmptyAdviceIsByteIdentical
// alone cannot catch a double-space or misplaced splice, because both a correct and a subtly
// broken splice collapse to the identical string once advice is "".
func TestForbiddenSecretRoleError_NonEmptyAdviceExactString(t *testing.T) {
	key := types.NamespacedName{Namespace: "tenant-a", Name: "db-creds"}
	underlying := errors.New("no Role")
	base := fake.NewClientBuilder().WithScheme(unitTestScheme).Build()
	c := ssrrClient(base, "", authorizationv1.SubjectRulesReviewStatus{}, nil, nil) // no rules at all -> arm C
	r := &WorkflowRunReconciler{Client: c}

	got := r.forbiddenSecretRoleError(context.Background(), key, key.Namespace, underlying).Error()
	advice := " The API server reports no rule grants any access to secrets in namespace " + key.Namespace +
		", confirming no Role exists yet for this Secret."
	want := "reading Secret " + key.String() + " is Forbidden: OttoFlow ships with no Secret access. " + valuesPathFix(key) +
		" Confirm with: `kubectl auth can-i get secrets/" + key.Name + " -n " + key.Namespace +
		" --as=system:serviceaccount:<install-ns>:<controller-sa>`" + advice +
		" (underlying error: " + underlying.Error() + "): secret access denied"
	if got != want {
		t.Errorf("exact message mismatch:\n got:  %q\n want: %q", got, want)
	}
	if !errors.Is(r.forbiddenSecretRoleError(context.Background(), key, key.Namespace, underlying), errSecretAccessDenied) {
		t.Error("expected errors.Is(..., errSecretAccessDenied) to hold")
	}
}

// TestForbiddenSecretRoleError_CarriesTypoAdvice is the case the advice exists for: a Role that
// names the wrong Secret produces the same bare 403 as no Role at all. The
// message must name the typo so an operator can see it directly, and must still classify as
// SecretAccessDenied via errors.Is.
func TestForbiddenSecretRoleError_CarriesTypoAdvice(t *testing.T) {
	key := types.NamespacedName{Namespace: "tenant-a", Name: "ottoflow-llm-credentials"}
	base := fake.NewClientBuilder().WithScheme(unitTestScheme).Build()
	c := ssrrClient(base, "", authorizationv1.SubjectRulesReviewStatus{
		ResourceRules: []authorizationv1.ResourceRule{{
			Verbs: []string{"get"}, APIGroups: []string{""}, Resources: []string{"secrets"},
			ResourceNames: []string{"ottoflow-llm-credentials-TYPO"},
		}},
	}, nil, nil)
	r := &WorkflowRunReconciler{Client: c}

	err := r.forbiddenSecretRoleError(context.Background(), key, key.Namespace, errors.New("no Role"))
	if !strings.Contains(err.Error(), "ottoflow-llm-credentials-TYPO") {
		t.Errorf("expected the message to name the typo'd Secret name, got: %v", err)
	}
	if !errors.Is(err, errSecretAccessDenied) {
		t.Error("expected errors.Is(err, errSecretAccessDenied) to hold")
	}
}

// TestForbiddenSecretRoleError_SSRRFailureNeverChangesOutcome proves the advice is best-effort:
// when the SelfSubjectRulesReview Create itself fails, forbiddenSecretRoleError must still return
// the same terminal, SecretAccessDenied-classified error (with no advice and no panic) — the
// diagnostic never gates or destabilizes the actual outcome.
func TestForbiddenSecretRoleError_SSRRFailureNeverChangesOutcome(t *testing.T) {
	key := types.NamespacedName{Namespace: "tenant-a", Name: "db-creds"}
	base := fake.NewClientBuilder().WithScheme(unitTestScheme).Build()
	c := ssrrClient(base, "", authorizationv1.SubjectRulesReviewStatus{}, errors.New("SSRR create failed"), nil)
	r := &WorkflowRunReconciler{Client: c}

	err := r.forbiddenSecretRoleError(context.Background(), key, key.Namespace, errors.New("no Role"))
	if err == nil {
		t.Fatal("expected a non-nil terminal error")
	}
	if !errors.Is(err, errSecretAccessDenied) {
		t.Error("expected errors.Is(err, errSecretAccessDenied) to hold even when the SSRR itself failed")
	}
	if secretAccessDeniedReason(err) != ottoflowv1alpha1.WorkflowRunFailureReasonSecretAccessDenied {
		t.Errorf("expected reason SecretAccessDenied, got %q", secretAccessDeniedReason(err))
	}
}

// TestEnsureRunnerSecrets_InstallNamespaceSourceLeaksNothing exercises the REAL ensureRunnerSecrets
// path (not secretAccessAdvice directly) with sourceNamespace == r.ControllerNamespace: the
// install namespace's own Secret-copy source read is Forbidden, and even though the interceptor
// is wired to answer a SelfSubjectRulesReview with a cert-manager-shaped grant (the four TLS
// Secret names the chart's certificate manager uses), none of that may leak into a tenant-visible
// WorkflowRun.Status message — the namespace precondition must block the SSRR before it is ever
// issued for this cross-namespace read. The resulting message must be byte-identical to the
// advice-less baseline.
func TestEnsureRunnerSecrets_InstallNamespaceSourceLeaksNothing(t *testing.T) {
	const runNS, installNS = "tenant-a", "ottoflow"
	certSecretNames := []string{
		"ottoflow-webhook.ottoflow.svc.tls-ca",
		"ottoflow-webhook.ottoflow.svc.tls-pair",
		"ottoflow-agent-executor.ottoflow.svc.tls-ca",
		"ottoflow-agent-executor.ottoflow.svc.tls-pair",
	}
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: runNS, UID: "run-uid"},
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "ottoflow-run-1", Namespace: runNS},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{{
				Name:         "operator-supplied",
				VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "shared-tls"}},
			}},
		}}},
	}
	base := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(wr, job).Build()
	var calls int
	c := ssrrClient(base, installNS, authorizationv1.SubjectRulesReviewStatus{
		ResourceRules: []authorizationv1.ResourceRule{{
			Verbs: []string{"get", "update", "delete"}, APIGroups: []string{""}, Resources: []string{"secrets"},
			ResourceNames: certSecretNames,
		}},
	}, nil, &calls)
	r := &WorkflowRunReconciler{Client: c, Scheme: unitTestScheme, ControllerNamespace: installNS}

	err := r.ensureRunnerSecrets(context.Background(), wr, job, installNS)
	if err == nil {
		t.Fatal("expected ensureRunnerSecrets to fail: the source Secret read is Forbidden")
	}
	if calls != 0 {
		t.Errorf("expected zero SelfSubjectRulesReview Create calls for an install-namespace source, got %d", calls)
	}
	for _, name := range certSecretNames {
		if strings.Contains(err.Error(), name) {
			t.Errorf("install-namespace cert Secret name %q leaked into a tenant-visible message: %v", name, err)
		}
	}

	sourceKey := types.NamespacedName{Namespace: installNS, Name: "shared-tls"}
	// The Forbidden error ensureRunnerSecrets actually sees is what the ssrrClient Get
	// interceptor manufactures — reconstruct that exact error, not a bare errors.New, since
	// apierrors.NewForbidden's Error() text differs from its wrapped reason alone.
	underlying := apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, sourceKey.Name, errors.New("no Role"))
	want := baselineForbiddenSecretRoleError(sourceKey, installNamespaceFix(sourceKey), underlying)
	if err.Error() != want {
		t.Errorf("output not byte-identical to the advice-less baseline:\n got:  %q\n want: %q", err.Error(), want)
	}
}

func installNamespaceFix(key types.NamespacedName) string {
	return `This Secret is in OttoFlow's own install namespace "` + key.Namespace + `", where the chart's Secret-RBAC ` +
		`shortcut deliberately refuses to render a Role (blast-radius hygiene) — create a Role and RoleBinding ` +
		`directly in that namespace granting the controller ServiceAccount get on this Secret (resourceNames: ["` +
		key.Name + `"]); see docs/user/rbac-secret-access.md.`
}
