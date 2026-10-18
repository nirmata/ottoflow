/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package auth

import (
	"context"
	"fmt"

	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// MissingReviewerAccess asks the apiserver (SelfSubjectAccessReview, which any authenticated
// identity may create) whether the current identity may create the TokenReviews and
// SubjectAccessReviews AuthenticateRequest needs, and returns one message per verb that is denied
// or could not be checked. An empty result means both are allowed. Without them every
// authentication fails closed, so callers log the result at startup to name the missing binding.
func MissingReviewerAccess(ctx context.Context, k kubernetes.Interface) []string {
	checks := []struct{ group, resource string }{
		{"authentication.k8s.io", "tokenreviews"},
		{"authorization.k8s.io", "subjectaccessreviews"},
	}
	var missing []string
	for _, c := range checks {
		res, err := k.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authorizationv1.SelfSubjectAccessReview{
			Spec: authorizationv1.SelfSubjectAccessReviewSpec{
				ResourceAttributes: &authorizationv1.ResourceAttributes{Verb: "create", Group: c.group, Resource: c.resource},
			},
		}, metav1.CreateOptions{})
		switch {
		case err != nil:
			missing = append(missing, fmt.Sprintf("create %s: could not check: %v", c.resource, err))
		case !res.Status.Allowed:
			missing = append(missing, fmt.Sprintf("create %s.%s: denied", c.resource, c.group))
		}
	}
	return missing
}
