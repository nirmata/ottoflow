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
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

// llmCredentialsTestClient wraps base with interceptors for both calls
// injectWellKnownLLMCredentials issues against a well-known-LLM-credentials Secret read: the
// SelfSubjectAccessReview gate (canGetSecret) and the Secret Get itself. Tests set both
// WorkflowRunReconciler.Client and .APIReader to the SAME value returned here, so one
// call-order log captures ordering between the two calls.
//
//   - ssarStatus/ssarErr: the SelfSubjectAccessReview Create's returned Status, or (ssarErr
//     non-nil) the error the Create call itself fails with — status is never touched then.
//   - getErr: if non-nil, every Secret Get returns this error instead of touching the tracker.
//   - ssarCalls/getCalls: if non-nil, count Create/Get attempts for that object type.
//   - attrs: if non-nil, receives a copy of each SelfSubjectAccessReview's ResourceAttributes,
//     in call order.
//   - order: if non-nil, appends "ssar" or "get" (in call order) so a test can assert relative
//     ordering between the two calls.
func llmCredentialsTestClient(
	base client.WithWatch,
	ssarStatus authorizationv1.SubjectAccessReviewStatus, ssarErr error,
	getErr error,
	ssarCalls, getCalls *int,
	attrs *[]authorizationv1.ResourceAttributes,
	order *[]string,
) client.WithWatch {
	return interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			ssar, ok := obj.(*authorizationv1.SelfSubjectAccessReview)
			if !ok {
				return c.Create(ctx, obj, opts...)
			}
			if ssarCalls != nil {
				*ssarCalls++
			}
			if attrs != nil && ssar.Spec.ResourceAttributes != nil {
				*attrs = append(*attrs, *ssar.Spec.ResourceAttributes)
			}
			if order != nil {
				*order = append(*order, "ssar")
			}
			if ssarErr != nil {
				return ssarErr
			}
			ssar.Status = ssarStatus
			return nil
		},
		// Deliberately its own type-switch rather than composing secretGetErrorReader
		// (controller_unit_test.go): that shared helper always fails once its match matches —
		// it has no "count/record this call, then still delegate" path. This closure needs
		// exactly that when getErr is nil (every Secret Get still counted and order-logged, but
		// passed straight through to a real Get) — forcing it through the shared shape would mean
		// adding that path back onto the shared helper just to support this one caller, so it
		// stays separate instead.
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.Secret); !ok {
				return c.Get(ctx, key, obj, opts...)
			}
			if getCalls != nil {
				*getCalls++
			}
			if order != nil {
				*order = append(*order, "get")
			}
			if getErr != nil {
				return getErr
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
}

// recordingNeedsCreds wraps staticNeedsCreds(v) with a call counter, for tests that must prove
// the closure was (or was not) invoked — not merely what it would have returned.
func recordingNeedsCreds(v bool, calls *int) func() (bool, error) {
	return func() (bool, error) {
		*calls++
		return v, nil
	}
}

// llmCredsFixture builds a WorkflowRun (namespace "team-a", well-known Secret name
// "ottoflow-llm-credentials") and a WorkflowRunReconciler wired with client for both Client and
// APIReader, plus a FakeRecorder — the shared setup every T1-T10 test starts from.
func llmCredsFixture(t *testing.T, client client.Client) (*WorkflowRunReconciler, *ottoflowv1alpha1.WorkflowRun, *events.FakeRecorder) {
	t.Helper()
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "test-run", Namespace: "team-a"},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf"}},
	}
	rec := events.NewFakeRecorder(10)
	r := &WorkflowRunReconciler{
		Client:        client,
		APIReader:     client,
		Scheme:        unitTestScheme,
		EventRecorder: rec,
		RunnerConfig:  RunnerConfig{LLMCredentialsSecret: "ottoflow-llm-credentials"},
	}
	return r, wr, rec
}

func newLLMCredsSecret(ns, name string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Data:       map[string][]byte{testLLMTokenKey: []byte("tok")},
	}
}

// T1 — the SelfSubjectAccessReview is issued before the Secret Get, with the resolved
// {namespace, name}, and Spec.ResourceAttributes matches the Get's own implicit authorization
// attributes exactly.
//
// Revert -> red: remove the gate entirely (call the Get directly) -> ssarCalls stays 0, the order
// log never contains "ssar", and the attrs slice is empty. Drop Version from the SSAR ->
// attribute comparison fails.
func TestInjectWellKnownLLMCredentials_ReviewIssuedBeforeGet(t *testing.T) {
	secret := newLLMCredsSecret("team-a", "ottoflow-llm-credentials")
	base := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(secret).Build()
	var ssarCalls, getCalls int
	var attrs []authorizationv1.ResourceAttributes
	var order []string
	c := llmCredentialsTestClient(base, authorizationv1.SubjectAccessReviewStatus{Allowed: true}, nil, nil, &ssarCalls, &getCalls, &attrs, &order)
	r, wr, _ := llmCredsFixture(t, c)

	_, err := r.injectWellKnownLLMCredentials(context.Background(), wr, nil, staticNeedsCreds(false))
	if err != nil {
		t.Fatalf("injectWellKnownLLMCredentials: %v", err)
	}
	if ssarCalls != 1 {
		t.Fatalf("expected exactly 1 SelfSubjectAccessReview Create, got %d", ssarCalls)
	}
	if getCalls != 1 {
		t.Fatalf("expected exactly 1 Secret Get, got %d", getCalls)
	}
	if len(order) != 2 || order[0] != "ssar" || order[1] != "get" {
		t.Fatalf("expected call order [ssar get], got %v", order)
	}
	if len(attrs) != 1 {
		t.Fatalf("expected exactly 1 recorded ResourceAttributes, got %d", len(attrs))
	}
	want := authorizationv1.ResourceAttributes{
		Group: "", Version: "v1", Resource: "secrets", Verb: "get",
		Namespace: "team-a", Name: "ottoflow-llm-credentials",
	}
	if attrs[0] != want {
		t.Errorf("ResourceAttributes = %+v, want %+v", attrs[0], want)
	}
}

// T2 — ANTI-BYPASS: a per-run spec.execution.llmCredentialsSecret naming a DIFFERENT namespace
// than the WorkflowRun's own must have the review issued for that resolved namespace/name, not
// the run's own namespace — identical to the key the Get itself uses. Building the review's
// attributes from workflowRun.Namespace instead (rather than the shared key variable) would
// authorize one object while the Get reads a different one.
//
// Revert -> red: build the review from workflowRun.Namespace/workflowRun.Name instead of the
// resolved key -> attrs[0].Namespace/Name stop matching team-b/other below.
func TestInjectWellKnownLLMCredentials_ReviewUsesResolvedNamespaceAndName(t *testing.T) {
	secret := newLLMCredsSecret("team-b", "other")
	base := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(secret).Build()
	var attrs []authorizationv1.ResourceAttributes
	c := llmCredentialsTestClient(base, authorizationv1.SubjectAccessReviewStatus{Allowed: true}, nil, nil, nil, nil, &attrs, nil)
	r, wr, _ := llmCredsFixture(t, c)
	wr.Spec.Execution = &ottoflowv1alpha1.WorkflowRunExecutionSpec{
		LLMCredentialsSecret: &ottoflowv1alpha1.LLMCredentialsSecretRef{Name: "other", Namespace: "team-b"},
	}

	if _, err := r.injectWellKnownLLMCredentials(context.Background(), wr, nil, staticNeedsCreds(false)); err != nil {
		t.Fatalf("injectWellKnownLLMCredentials: %v", err)
	}
	if len(attrs) != 1 {
		t.Fatalf("expected exactly 1 recorded ResourceAttributes, got %d", len(attrs))
	}
	if attrs[0].Namespace != "team-b" || attrs[0].Name != "other" {
		t.Errorf("review named %s/%s, want team-b/other (the RESOLVED per-run override, not workflowRun.Namespace)",
			attrs[0].Namespace, attrs[0].Name)
	}
}

// T3 — a denied review prevents the Get entirely (required arm): 0 Gets, a plain (non-
// transientBuildError) error classifying errSecretAccessDenied / SecretAccessDenied, carrying
// the full operator remediation plus the gate's own identifying fragment, and the Warning event.
//
// Revert -> red: let the denied path fall through to the Get anyway -> getCalls becomes 1.
func TestInjectWellKnownLLMCredentials_ReviewDeniedPreventsRead_Required(t *testing.T) {
	base := fake.NewClientBuilder().WithScheme(unitTestScheme).Build()
	var getCalls int
	c := llmCredentialsTestClient(base, authorizationv1.SubjectAccessReviewStatus{}, nil, nil, nil, &getCalls, nil, nil)
	r, wr, rec := llmCredsFixture(t, c)

	extras, err := r.injectWellKnownLLMCredentials(context.Background(), wr, nil, staticNeedsCreds(true))
	if err == nil {
		t.Fatal("expected an error when a denied review is credential-gated")
	}
	if len(extras) != 0 {
		t.Errorf("expected no env vars alongside the error, got %d", len(extras))
	}
	if getCalls != 0 {
		t.Fatalf("expected 0 Secret Gets (the denied review must prevent the read), got %d", getCalls)
	}
	var te *transientBuildError
	if errors.As(err, &te) {
		t.Errorf("must NOT be a transientBuildError, got: %v", err)
	}
	if !errors.Is(err, errSecretAccessDenied) {
		t.Error("expected errors.Is(err, errSecretAccessDenied) to hold")
	}
	if secretAccessDeniedReason(err) != ottoflowv1alpha1.WorkflowRunFailureReasonSecretAccessDenied {
		t.Errorf("expected reason SecretAccessDenied, got %q", secretAccessDeniedReason(err))
	}
	for _, want := range []string{
		"Forbidden", "Role", `"ottoflow-llm-credentials"`, "team-a",
		"rbac.secretAccess", "kubectl auth can-i", SecretReadNotAuthorizedFragment,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("expected error to contain %q, got: %v", want, err)
		}
	}
	select {
	case ev := <-rec.Events:
		if !strings.Contains(ev, "Forbidden") {
			t.Errorf("expected a Warning event mentioning Forbidden, got %q", ev)
		}
	default:
		t.Error("expected a Warning event to be recorded")
	}
}

// T4 — a denied review is benign when nothing needs the credentials (optional arm): 0 Gets,
// (nil, nil), and the Warning event still fires.
func TestInjectWellKnownLLMCredentials_ReviewDeniedPreventsRead_Optional(t *testing.T) {
	base := fake.NewClientBuilder().WithScheme(unitTestScheme).Build()
	var getCalls int
	c := llmCredentialsTestClient(base, authorizationv1.SubjectAccessReviewStatus{}, nil, nil, nil, &getCalls, nil, nil)
	r, wr, rec := llmCredsFixture(t, c)

	extras, err := r.injectWellKnownLLMCredentials(context.Background(), wr, nil, staticNeedsCreds(false))
	if err != nil {
		t.Fatalf("a denied review with needsCreds=false must be benign, got: %v", err)
	}
	if len(extras) != 0 {
		t.Errorf("expected no env vars, got %d", len(extras))
	}
	if getCalls != 0 {
		t.Fatalf("expected 0 Secret Gets, got %d", getCalls)
	}
	select {
	case ev := <-rec.Events:
		if !strings.Contains(ev, "Forbidden") {
			t.Errorf("expected a Warning event mentioning Forbidden, got %q", ev)
		}
	default:
		t.Error("expected a Warning event to be recorded even on the benign skip")
	}
}

// T5 — an ALLOWED review followed by the Get itself returning Forbidden (a grant revoked, or
// simply a plain RBAC denial the review somehow missed) must still classify exactly like a
// Forbidden Get with no gate in front of it: required -> terminal SecretAccessDenied carrying
// the API server's own 403 text (NOT the gate's fragment, since the gate did not catch this
// one); optional -> benign skip plus the event.
func TestInjectWellKnownLLMCredentials_ReviewAllowedThenForbidden(t *testing.T) {
	getErr := apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "ottoflow-llm-credentials", errors.New("grant revoked"))

	t.Run("required", func(t *testing.T) {
		base := fake.NewClientBuilder().WithScheme(unitTestScheme).Build()
		c := llmCredentialsTestClient(base, authorizationv1.SubjectAccessReviewStatus{Allowed: true}, nil, getErr, nil, nil, nil, nil)
		r, wr, rec := llmCredsFixture(t, c)

		err := mustInjectErr(t, r, wr, staticNeedsCreds(true))
		if !errors.Is(err, errSecretAccessDenied) {
			t.Error("expected errors.Is(err, errSecretAccessDenied) to hold")
		}
		if !strings.Contains(err.Error(), "grant revoked") {
			t.Errorf("expected the API server's own 403 text in the message, got: %v", err)
		}
		if strings.Contains(err.Error(), SecretReadNotAuthorizedFragment) {
			t.Errorf("the gate's fragment must NOT appear — the Get, not the gate, caught this denial: %v", err)
		}
		select {
		case <-rec.Events:
		default:
			t.Error("expected a Warning event")
		}
	})

	t.Run("optional", func(t *testing.T) {
		base := fake.NewClientBuilder().WithScheme(unitTestScheme).Build()
		c := llmCredentialsTestClient(base, authorizationv1.SubjectAccessReviewStatus{Allowed: true}, nil, getErr, nil, nil, nil, nil)
		r, wr, rec := llmCredsFixture(t, c)

		extras, err := r.injectWellKnownLLMCredentials(context.Background(), wr, nil, staticNeedsCreds(false))
		if err != nil {
			t.Fatalf("expected a benign skip, got: %v", err)
		}
		if len(extras) != 0 {
			t.Errorf("expected no env vars, got %d", len(extras))
		}
		select {
		case <-rec.Events:
		default:
			t.Error("expected a Warning event even on the benign skip")
		}
	})
}

func mustInjectErr(t *testing.T, r *WorkflowRunReconciler, wr *ottoflowv1alpha1.WorkflowRun, needsCreds func() (bool, error)) error {
	t.Helper()
	extras, err := r.injectWellKnownLLMCredentials(context.Background(), wr, nil, needsCreds)
	if err == nil {
		t.Fatal("expected a non-nil error")
	}
	if len(extras) != 0 {
		t.Errorf("expected no env vars alongside the error, got %d", len(extras))
	}
	return err
}

// T6 — an ALLOWED review followed by a genuinely missing Secret (NotFound) is benign, with no
// event: NotFound is not a denial of any kind.
func TestInjectWellKnownLLMCredentials_ReviewAllowedThenNotFound(t *testing.T) {
	base := fake.NewClientBuilder().WithScheme(unitTestScheme).Build() // no Secret object
	c := llmCredentialsTestClient(base, authorizationv1.SubjectAccessReviewStatus{Allowed: true}, nil, nil, nil, nil, nil, nil)
	r, wr, rec := llmCredsFixture(t, c)

	extras, err := r.injectWellKnownLLMCredentials(context.Background(), wr, nil, staticNeedsCreds(true))
	if err != nil {
		t.Fatalf("a missing Secret must stay benign, got: %v", err)
	}
	if len(extras) != 0 {
		t.Errorf("expected no env vars, got %d", len(extras))
	}
	select {
	case ev := <-rec.Events:
		t.Errorf("expected no event for a plain NotFound, got %q", ev)
	default:
	}
}

// T7 — an INCONCLUSIVE review (the SelfSubjectAccessReview Create itself errors) falls through
// to the Get UNCONDITIONALLY: the Get runs and injection succeeds exactly as if there were no
// gate at all, and needsCreds is never called on this path (only an actual denial calls it).
//
// Revert -> red: treat an inconclusive review as a denial instead of falling through -> extras
// becomes empty and needsCredsCalls becomes nonzero below.
func TestInjectWellKnownLLMCredentials_ReviewInconclusiveFallsThroughToGet(t *testing.T) {
	secret := newLLMCredsSecret("team-a", "ottoflow-llm-credentials")
	base := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(secret).Build()
	c := llmCredentialsTestClient(base, authorizationv1.SubjectAccessReviewStatus{}, errors.New("SSAR Create failed"), nil, nil, nil, nil, nil)
	r, wr, _ := llmCredsFixture(t, c)
	var needsCredsCalls int

	extras, err := r.injectWellKnownLLMCredentials(context.Background(), wr, nil, recordingNeedsCreds(true, &needsCredsCalls))
	if err != nil {
		t.Fatalf("an inconclusive review must fall through to the Get, got: %v", err)
	}
	if len(extras) != 1 || extras[0].Name != testLLMTokenKey {
		t.Fatalf("expected injection to succeed via the fallthrough Get, got %+v", extras)
	}
	if needsCredsCalls != 0 {
		t.Fatalf("needsCreds must NEVER be called on the inconclusive path, got %d calls", needsCredsCalls)
	}
}

// T8 — an INCONCLUSIVE review followed by the Get returning a genuine server error (the measured
// fail-closed-authorization-webhook-outage shape: an Internal Server Error, not Forbidden) must
// retry: a *transientBuildError, classifying to the empty (non-SecretAccessDenied) reason. This
// is the executable proof that an authorizer outage is retried rather than reported as a denial
// — the Get's own 500 -> transientBuildError path supplies it, with no mechanism of its own.
func TestInjectWellKnownLLMCredentials_ReviewInconclusiveThenServerError_Retries(t *testing.T) {
	base := fake.NewClientBuilder().WithScheme(unitTestScheme).Build()
	getErr := apierrors.NewInternalError(errors.New("authorization webhook did not respond"))
	c := llmCredentialsTestClient(base, authorizationv1.SubjectAccessReviewStatus{}, errors.New("SSAR Create failed"), getErr, nil, nil, nil, nil)
	r, wr, _ := llmCredsFixture(t, c)

	_, err := r.injectWellKnownLLMCredentials(context.Background(), wr, nil, staticNeedsCreds(true))
	if err == nil {
		t.Fatal("expected a non-nil error")
	}
	var te *transientBuildError
	if !errors.As(err, &te) {
		t.Fatalf("expected a *transientBuildError (requeue-and-retry), got: %v (%T)", err, err)
	}
	if secretAccessDeniedReason(err) != "" {
		t.Errorf("a transient/inconclusive outcome must never classify SecretAccessDenied, got %q", secretAccessDeniedReason(err))
	}
}

// T9 — TestCanGetSecret_Classification pins canGetSecret's classification ladder directly
// against every reachable (Allowed, Denied, EvaluationError) combination, including the two
// fail-closed/fail-open authorizer-outage shapes measured against a real envtest apiserver.
//
// Revert -> red: reinstate a Denied-first ladder (check Status.Denied before EvaluationError) ->
// the "(F,T,\"webhook timeout\") inconclusive (fail-closed outage)" row below flips from
// wantAllowed=false/wantErr=true to wantErr=false, i.e. from inconclusive to denied.
func TestCanGetSecret_Classification(t *testing.T) {
	key := types.NamespacedName{Namespace: "team-a", Name: "ottoflow-llm-credentials"}
	tests := []struct {
		name        string
		status      authorizationv1.SubjectAccessReviewStatus
		createErr   error
		wantAllowed bool
		wantErr     bool
	}{
		{"(T,F,\"\") allow", authorizationv1.SubjectAccessReviewStatus{Allowed: true}, nil, true, false},
		{"(T,*,\"x\") allow regardless of Denied/EvaluationError", authorizationv1.SubjectAccessReviewStatus{Allowed: true, Denied: true, EvaluationError: "irrelevant"}, nil, true, false},
		{"(F,T,\"\") denied", authorizationv1.SubjectAccessReviewStatus{Denied: true}, nil, false, false},
		{"(F,T,\"webhook timeout\") inconclusive (fail-closed outage)", authorizationv1.SubjectAccessReviewStatus{Denied: true, EvaluationError: "webhook timeout"}, nil, false, true},
		{"(F,F,\"\") denied (zero status, plain RBAC denial)", authorizationv1.SubjectAccessReviewStatus{}, nil, false, false},
		{"(F,F,\"webhook timeout\") inconclusive (fail-open outage default)", authorizationv1.SubjectAccessReviewStatus{EvaluationError: "webhook timeout"}, nil, false, true},
		{"Create error -> inconclusive", authorizationv1.SubjectAccessReviewStatus{}, errors.New("connection refused"), false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := fake.NewClientBuilder().WithScheme(unitTestScheme).Build()
			c := llmCredentialsTestClient(base, tt.status, tt.createErr, nil, nil, nil, nil, nil)
			allowed, err := canGetSecret(context.Background(), c, key)
			if allowed != tt.wantAllowed {
				t.Errorf("allowed = %v, want %v", allowed, tt.wantAllowed)
			}
			if (err != nil) != tt.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// T10 — DISCLOSURE GUARD: the SelfSubjectAccessReview's Status.Reason (which on an allow names the
// exact RoleBinding/Role/ServiceAccount that grant access, and on a denial carries whatever the
// authorizer returned — server-influenced content the controller never validated) must never be
// echoed into the tenant-visible failure message.
//
// Revert -> red: compose the failure message from status.Reason -> one of the "leaked" substrings
// below starts matching err.Error().
func TestInjectWellKnownLLMCredentials_ReviewDeniedMessageOmitsServerReason(t *testing.T) {
	base := fake.NewClientBuilder().WithScheme(unitTestScheme).Build()
	status := authorizationv1.SubjectAccessReviewStatus{
		Reason: `RBAC: allowed by RoleBinding "ottoflow/secret-read" of Role "secret-read" to ServiceAccount "controller-manager/ottoflow"`,
	}
	c := llmCredentialsTestClient(base, status, nil, nil, nil, nil, nil, nil)
	r, wr, _ := llmCredsFixture(t, c)

	err := mustInjectErr(t, r, wr, staticNeedsCreds(true))
	// Distinctive to the injected Status.Reason (not to the standard remediation text, which
	// itself legitimately says "Role+RoleBinding" as generic advice).
	for _, leaked := range []string{"RBAC: allowed by", "ottoflow/secret-read", "controller-manager/ottoflow"} {
		if strings.Contains(err.Error(), leaked) {
			t.Errorf("message must never echo the SelfSubjectAccessReview's server-supplied Status.Reason; found %q in: %v", leaked, err)
		}
	}
}
