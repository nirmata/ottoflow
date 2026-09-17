/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"context"
	"errors"

	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SecretReadNotAuthorizedFragment is the stable substring identifying a denial reported by the
// canGetSecret gate below, as opposed to a denial the Get itself later surfaces as Forbidden.
// It is exported so end-to-end tests outside this package can reference it directly instead of
// hand-typing the literal, which is what makes the "can never disagree" guarantee real —
// errSecretReadNotAuthorized is composed from it below, rather than duplicating the text.
const SecretReadNotAuthorizedFragment = "SelfSubjectAccessReview reported that the controller identity is not authorized"

// errSecretReadNotAuthorized is passed as forbiddenSecretRoleError's cause when canGetSecret
// classifies the review as denied. No server-supplied string ever enters it — in particular never
// SelfSubjectAccessReview.Status.Reason, which on an allow names the RoleBinding/Role/ServiceAccount
// that grant access and on a denial carries whatever text the authorizer returned, and must never
// be echoed into a tenant-visible message — because the
// read was never attempted, so there is nothing from the API server to report beyond the bare
// fact of the denial.
var errSecretReadNotAuthorized = errors.New(SecretReadNotAuthorizedFragment + " to get this Secret; the read was not attempted")

// canGetSecret asks the API server, via a SelfSubjectAccessReview, whether the controller's own
// identity is currently allowed to `get` the Secret named by key — BEFORE the controller actually
// issues that Get. It classifies the answer into exactly three outcomes:
//
//   - (true, nil): the review says the read is allowed. The caller proceeds to the actual Get,
//     which remains authoritative — this review is a posture measure, not the security control.
//   - (false, nil): the review affirmatively denies the read. The caller must not issue the Get.
//   - (false, err): the review is INCONCLUSIVE — the Create call itself failed (a fake test
//     client with no SelfSubjectAccessReview status.name defaulting errors here; so can a
//     transient API server issue), or the review's own evaluation could not decide. The caller
//     must treat this as neither an allow nor a deny and fall through to the actual Get, which
//     supplies its own authoritative answer — including, on a genuine authorizer outage, its own
//     500 -> transientBuildError -> requeue retry. err is for logging only: it is never
//     tenant-visible and is deliberately never composed from any API-server-supplied text.
//
// The classification ladder mirrors k8s.io/apiserver's own request-authorization order
// (pkg/endpoints/filters/authorization.go withAuthorization): an Allow decision wins outright;
// otherwise an evaluation error is a 500 (treated here as inconclusive, matching the API server's
// own "I don't know" case); otherwise the request is Forbidden (403, i.e. denied).
// Status.Denied is deliberately NOT consulted as an independent signal: at that filter, both an
// explicit Deny and a plain NoOpinion resolve to the identical 403, so Denied adds no decision
// weight the final arm below lacks — while a fail-closed authorization-webhook outage sets
// Denied=true ALONGSIDE EvaluationError (measured against a real envtest apiserver with
// --authorization-config wired to a webhook at a dead endpoint), which must read as inconclusive,
// never as a confident denial. A plain RBAC denial with no error is the zero value of
// SubjectAccessReviewStatus (Allowed=false, Denied=false, EvaluationError=""), which the final
// arm below covers correctly, since no non-denial "no opinion" case exists once Allowed and
// EvaluationError have both been ruled out.
//
// Spec.ResourceAttributes.Version is set to "v1", matching the Secret Get's own implicit
// authorization attributes: plain RBAC ignores this field entirely, so setting it costs nothing
// there, but it closes the door on an authorization webhook whose matchConditions key on the
// request's apiVersion authorizing a version this review never asked about — which would let the
// review and the read diverge on outcome.
//
// `create selfsubjectaccessreviews.authorization.k8s.io` is granted cluster-wide to
// system:authenticated by the built-in system:basic-user ClusterRole — the same grant the
// SelfSubjectRulesReview diagnostic in secret_access_advice.go already relies on — so every
// OttoFlow install can already issue this review; no chart change is needed.
//
// c == nil (a reconciler built without a Client, as some unit tests do) returns inconclusive
// rather than panicking.
func canGetSecret(ctx context.Context, c client.Client, key types.NamespacedName) (bool, error) {
	if c == nil {
		return false, errors.New("canGetSecret: nil Client")
	}
	ssar := &authorizationv1.SelfSubjectAccessReview{
		Spec: authorizationv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Group:     "",
				Version:   "v1",
				Resource:  "secrets",
				Verb:      "get",
				Namespace: key.Namespace,
				Name:      key.Name,
			},
		},
	}
	if err := c.Create(ctx, ssar); err != nil {
		return false, err
	}
	switch {
	case ssar.Status.Allowed:
		return true, nil
	case ssar.Status.EvaluationError != "":
		// EvaluationError is not echoed anywhere: the caller only needs to know the review
		// could not decide, never the server's own text.
		return false, errors.New("SelfSubjectAccessReview evaluation error")
	default:
		return false, nil
	}
}
