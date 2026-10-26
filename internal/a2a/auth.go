/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package a2a

import (
	"context"
	"net/http"
	"time"

	"k8s.io/klog/v2"
)

const (
	// CallerResourceName is the virtual ConfigMap whose "get" a caller must hold, in the
	// Workflow's namespace, to run that Workflow over A2A. It is distinct from the MCP and
	// agent-executor caller names so each grant can be given out on its own.
	CallerResourceName = "a2a-caller"

	// TokenAudience is the TokenReview audience serve-a2a requires. A token minted for it
	// (kubectl create token --audience=ottoflow-a2a) is useless against the apiserver, so a
	// leaked copy in a kagent Secret grants nothing but this one endpoint.
	TokenAudience = "ottoflow-a2a"
)

// authTimeout bounds the TokenReview + SubjectAccessReview round trips so a hung apiserver
// cannot pin handler goroutines before authentication has succeeded. A var only so tests can
// shorten it.
var authTimeout = 10 * time.Second

// Authenticator authenticates one request and returns the caller identity.
// *auth.TokenReviewAndSARAuthenticator satisfies it.
type Authenticator interface {
	AuthenticateRequest(r *http.Request) (string, error)
}

// RequireAuth wraps next so every request except one whose path is exactly the agent card path
// must pass authn. It deliberately does not reuse auth.Middleware: that one lets /healthz and
// /readyz through, and serve-a2a routes every unmatched path to the JSON-RPC handler, so a bypass
// there would be an unauthenticated run-creation path. The card stays public because kagent's
// readiness probe fetches it without credentials.
//
// The match is on r.URL.EscapedPath() as received, before the mux can clean or redirect it, so
// "//.well-known/agent-card.json", ".../agent-card.json/" and "/.well-known%2Fagent-card.json" (whose
// decoded Path equals the card path but which the mux routes to the JSON-RPC catch-all) are
// authenticated like any other path.
// Every failure is a bare 401; the reason is logged at V(4) only.
func RequireAuth(authn Authenticator, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() == agentCardPath {
			next.ServeHTTP(w, r)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), authTimeout)
		defer cancel()
		who, err := authn.AuthenticateRequest(r.WithContext(ctx))
		if err != nil {
			klog.V(4).InfoS("A2A request rejected", "error", err, "path", r.URL.Path)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		klog.V(4).InfoS("A2A request authenticated", "serviceAccount", who)
		next.ServeHTTP(w, r)
	})
}
