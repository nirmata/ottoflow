/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package auth

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

// AgentExecutorCallerResourceName is the name of the virtual ConfigMap used for RBAC.
// A ClusterRole that grants "get" on configmaps with this name is bound to runner SAs;
// the agent executor uses SubjectAccessReview against this permission to allow callers.
const AgentExecutorCallerResourceName = "agent-executor-caller"

// MCPCallerResourceName is the virtual ConfigMap name behind "who may invoke a
// workflow as an MCP tool". It is deliberately not AgentExecutorCallerResourceName:
// calling the agent executor and running any workflow in the cluster are
// different grants, and one ClusterRole for both would make the narrower one
// impossible to give out.
const MCPCallerResourceName = "mcp-caller"

type contextKey string

const serviceAccountContextKey contextKey = "serviceAccount"

// TokenReviewAndSARAuthenticator authenticates by validating the bearer token (TokenReview)
// and then checking that the identity has a specific RBAC permission (SubjectAccessReview).
// This allows operators to manage "who can call the agent executor" via Kubernetes RBAC
// (ClusterRole + ClusterRoleBindings) instead of an explicit allowlist in the binary.
type TokenReviewAndSARAuthenticator struct {
	k8sClient      kubernetes.Interface
	checkNamespace string
	checkResource  string // ConfigMap name to check (e.g. AgentExecutorCallerResourceName)
	// audiences is sent as TokenReview.spec.audiences. Nil/empty omits the field, so the
	// apiserver validates against its own audience (the behaviour every pre-existing caller has).
	audiences []string
}

// NewTokenReviewAndSARAuthenticator creates an authenticator that allows any identity
// which has "get" on configmaps named checkResource in checkNamespace (typically the
// ottoflow release namespace). Bind a ClusterRole with that rule to runner and
// controller ServiceAccounts so they can call the agent executor.
func NewTokenReviewAndSARAuthenticator(k8sClient kubernetes.Interface, checkNamespace, checkResource string) *TokenReviewAndSARAuthenticator {
	if checkResource == "" {
		checkResource = AgentExecutorCallerResourceName
	}
	return &TokenReviewAndSARAuthenticator{
		k8sClient:      k8sClient,
		checkNamespace: checkNamespace,
		checkResource:  checkResource,
	}
}

// WithAudiences returns a copy of the authenticator whose TokenReview requires the token to be
// issued for at least one of audiences. The receiver is unchanged, so callers that never call
// this keep today's apiserver-audience behaviour.
func (a *TokenReviewAndSARAuthenticator) WithAudiences(audiences ...string) *TokenReviewAndSARAuthenticator {
	c := *a
	c.audiences = append([]string(nil), audiences...)
	return &c
}

// AuthenticateRequest validates the bearer token and then checks RBAC via SubjectAccessReview.
func (a *TokenReviewAndSARAuthenticator) AuthenticateRequest(r *http.Request) (string, error) {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return "", fmt.Errorf("missing Authorization header")
	}
	if !strings.HasPrefix(authHeader, "Bearer ") {
		return "", fmt.Errorf("invalid Authorization header format")
	}
	token := strings.TrimPrefix(authHeader, "Bearer ")

	ctx := r.Context()

	// 1) TokenReview to validate token and get identity
	tokenReview := &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{Token: token, Audiences: a.audiences},
	}
	result, err := a.k8sClient.AuthenticationV1().TokenReviews().Create(ctx, tokenReview, metav1.CreateOptions{})
	if err != nil {
		klog.V(4).ErrorS(err, "TokenReview API call failed")
		return "", fmt.Errorf("token validation failed: %w", err)
	}
	if !result.Status.Authenticated {
		klog.V(4).InfoS("TokenReview returned not authenticated", "error", result.Status.Error)
		return "", fmt.Errorf("token not authenticated: %s", result.Status.Error)
	}
	// An audience-unaware reviewer can answer authenticated=true without having checked the
	// requested audiences, so when audiences were asked for, the answer must name at least one.
	if len(a.audiences) > 0 && !slices.ContainsFunc(result.Status.Audiences, func(got string) bool { return slices.Contains(a.audiences, got) }) {
		return "", fmt.Errorf("token not validated for the required audience")
	}
	saName := result.Status.User.Username
	if !strings.HasPrefix(saName, "system:serviceaccount:") {
		return "", fmt.Errorf("invalid service account format: %s", saName)
	}

	// 2) SubjectAccessReview: can this identity get the caller ConfigMap in the check namespace?
	sar := &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{
			User:   saName,
			Groups: result.Status.User.Groups,
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace: a.checkNamespace,
				Verb:      "get",
				Group:     "",
				Resource:  "configmaps",
				Name:      a.checkResource,
			},
		},
	}
	sarResult, err := a.k8sClient.AuthorizationV1().SubjectAccessReviews().Create(ctx, sar, metav1.CreateOptions{})
	if err != nil {
		klog.V(4).ErrorS(err, "SubjectAccessReview API call failed")
		return "", fmt.Errorf("permission check failed: %w", err)
	}
	if !sarResult.Status.Allowed {
		klog.Warningf("ServiceAccount %s not allowed to call agent executor (SAR denied)", saName)
		return "", fmt.Errorf("service account %s not allowed", saName)
	}

	klog.V(4).InfoS("Successfully authenticated request via RBAC", "serviceAccount", saName)
	return saName, nil
}

// Middleware creates an HTTP middleware that authenticates using TokenReview + SAR.
func (a *TokenReviewAndSARAuthenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			next.ServeHTTP(w, r)
			return
		}
		saName, err := a.AuthenticateRequest(r)
		if err != nil {
			klog.V(4).InfoS("Authentication failed", "error", err, "path", r.URL.Path)
			http.Error(w, fmt.Sprintf("Unauthorized: %v", err), http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), serviceAccountContextKey, saName)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
