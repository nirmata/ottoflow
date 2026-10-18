/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package a2a

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/auth"
)

const rpcBody = `{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"message":{"parts":[{"kind":"text","text":"caller text"}]}}}`

func exposedWorkflow() *ottoflowv1alpha1.Workflow {
	return &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "greeter", UID: "u1"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Inputs: []ottoflowv1alpha1.Input{{Name: "prompt"}},
			Expose: &ottoflowv1alpha1.ExposeSpec{Kagent: &ottoflowv1alpha1.KagentExposeSpec{}},
		},
	}
}

// reviewClient is a fake clientset whose TokenReview and SubjectAccessReview answers the test
// controls. It records the last TokenReview so tests can check what was asked.
type reviewClient struct {
	*fake.Clientset
	tokenReview *authenticationv1.TokenReview
}

func newReviewClient(authenticated bool, user string, allowed bool, tokenErr, sarErr error) *reviewClient {
	rc := &reviewClient{Clientset: fake.NewSimpleClientset()}
	rc.PrependReactor("create", "tokenreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		tr := a.(k8stesting.CreateAction).GetObject().(*authenticationv1.TokenReview)
		rc.tokenReview = tr.DeepCopy()
		if tokenErr != nil {
			return true, nil, tokenErr
		}
		tr.Status = authenticationv1.TokenReviewStatus{Authenticated: authenticated, Audiences: tr.Spec.Audiences, User: authenticationv1.UserInfo{Username: user}}
		return true, tr, nil
	})
	rc.PrependReactor("create", "subjectaccessreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if sarErr != nil {
			return true, nil, sarErr
		}
		sar := a.(k8stesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview)
		sar.Status.Allowed = allowed
		return true, sar, nil
	})
	return rc
}

// serve builds the production handler chain (mux behind RequireAuth) over a fake client and sends
// one request; it returns the status, body, and the number of WorkflowRuns created.
func serve(t *testing.T, authn Authenticator, method, path, authz, body string) (int, string, int) {
	t.Helper()
	c := newFakeClient(t, exposedWorkflow())
	mux := http.NewServeMux()
	NewServer(c, "greeter", "default", "http://x/", 0).Register(mux)
	h := RequireAuth(authn, mux)

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(method, "http://x/", strings.NewReader(body)).WithContext(ctx)
	// httptest.NewRequest would clean "//" in the target; parse the URL directly so Path and
	// RawPath (set for encoded slashes) are exactly what a server would see.
	u, err := url.Parse("http://x" + path)
	if err != nil {
		t.Fatal(err)
	}
	req.URL = u
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var runs ottoflowv1alpha1.WorkflowRunList
	if err := c.List(context.Background(), &runs); err != nil {
		t.Fatal(err)
	}
	return rec.Code, rec.Body.String(), len(runs.Items)
}

func realAuthn(rc *reviewClient) Authenticator {
	return auth.NewTokenReviewAndSARAuthenticator(rc, "default", CallerResourceName).WithAudiences(TokenAudience)
}

// Regression: an unauthenticated message/send used to create a WorkflowRun. Behind RequireAuth it
// must not, on any path. The /healthz and /readyz rows are the bypass auth.Middleware would have
// left open.
func TestUnauthenticatedPathsCreateNoRun(t *testing.T) {
	for _, path := range []string{"/", "/healthz", "/readyz", "/anything", "//.well-known/agent-card.json", "/.well-known/agent-card.json/", "/.well-known%2Fagent-card.json"} {
		t.Run(path, func(t *testing.T) {
			code, body, runs := serve(t, realAuthn(newReviewClient(true, "system:serviceaccount:default:caller", true, nil, nil)), http.MethodPost, path, "", rpcBody)
			if code != http.StatusUnauthorized || runs != 0 {
				t.Fatalf("POST %s without a token: status=%d runs=%d, want 401 and 0 runs", path, code, runs)
			}
			if strings.TrimSpace(body) != "Unauthorized" {
				t.Errorf("body = %q, want exactly %q", body, "Unauthorized")
			}
		})
	}
}

func TestAgentCardIsPublicAndCreatesNoRun(t *testing.T) {
	code, _, runs := serve(t, realAuthn(newReviewClient(false, "", false, nil, nil)), http.MethodGet, agentCardPath, "", "")
	if code != http.StatusOK || runs != 0 {
		t.Fatalf("GET card without a token: status=%d runs=%d, want 200 and 0 runs", code, runs)
	}
}

func TestAuthenticatedCallerCreatesRun(t *testing.T) {
	rc := newReviewClient(true, "system:serviceaccount:default:caller", true, nil, nil)
	code, _, runs := serve(t, realAuthn(rc), http.MethodPost, "/", "Bearer tok", rpcBody)
	if code != http.StatusOK || runs != 1 {
		t.Fatalf("authorized call: status=%d runs=%d, want 200 and 1 run", code, runs)
	}
	if got := rc.tokenReview.Spec.Audiences; len(got) != 1 || got[0] != TokenAudience {
		t.Errorf("TokenReview audiences = %v, want [%s]", got, TokenAudience)
	}
}

// Every error class from sar.go must end as a 401 with no run created.
func TestAuthFailsClosed(t *testing.T) {
	sa := "system:serviceaccount:default:caller"
	cases := map[string]struct {
		rc    *reviewClient
		authz string
	}{
		"missing header":      {newReviewClient(true, sa, true, nil, nil), ""},
		"not a bearer header": {newReviewClient(true, sa, true, nil, nil), "Basic abc"},
		"tokenreview error":   {newReviewClient(true, sa, true, errors.New("apiserver down"), nil), "Bearer tok"},
		// A token for another audience comes back unauthenticated from the apiserver.
		"wrong audience (not authenticated)": {newReviewClient(false, "", true, nil, nil), "Bearer tok"},
		"non service account":                {newReviewClient(true, "alice", true, nil, nil), "Bearer tok"},
		"sar error":                          {newReviewClient(true, sa, true, nil, errors.New("apiserver down")), "Bearer tok"},
		"sar denied":                         {newReviewClient(true, sa, false, nil, nil), "Bearer tok"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			code, body, runs := serve(t, realAuthn(tc.rc), http.MethodPost, "/", tc.authz, rpcBody)
			if code != http.StatusUnauthorized || runs != 0 {
				t.Fatalf("status=%d runs=%d, want 401 and 0 runs", code, runs)
			}
			if strings.TrimSpace(body) != "Unauthorized" || strings.Contains(body, "apiserver") {
				t.Errorf("body leaks detail: %q", body)
			}
		})
	}
}

// blockingAuthn waits for the request context, the way a client call to a hung apiserver does.
type blockingAuthn struct{}

func (b blockingAuthn) AuthenticateRequest(r *http.Request) (string, error) {
	<-r.Context().Done()
	return "", r.Context().Err()
}

// The middleware must bound authentication itself: a request with no deadline gets one, and a
// longer parent deadline (a server watchdog) does not override it.
func TestAuthMiddlewareSetsItsOwnDeadline(t *testing.T) {
	old := authTimeout
	authTimeout = 200 * time.Millisecond
	defer func() { authTimeout = old }()
	parent, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	for name, ctx := range map[string]context.Context{"no parent deadline": context.Background(), "longer parent deadline": parent} {
		t.Run(name, func(t *testing.T) {
			var left time.Duration
			probe := authnFunc(func(r *http.Request) (string, error) {
				d, ok := r.Context().Deadline()
				if !ok {
					return "", errors.New("no deadline")
				}
				left = time.Until(d)
				return "", errors.New("stop")
			})
			req := httptest.NewRequest(http.MethodPost, "http://x/", nil).WithContext(ctx)
			RequireAuth(probe, http.NotFoundHandler()).ServeHTTP(httptest.NewRecorder(), req)
			if left <= 0 || left > authTimeout {
				t.Fatalf("remaining deadline = %v, want in (0, %v]", left, authTimeout)
			}
		})
	}
}

func TestAuthTimeoutFailsClosed(t *testing.T) {
	old := authTimeout
	authTimeout = 50 * time.Millisecond
	defer func() { authTimeout = old }()
	nextCalled := false
	h := RequireAuth(blockingAuthn{}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { nextCalled = true }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://x/", nil)) // no parent deadline: only the middleware's timeout can end this
	if rec.Code != http.StatusUnauthorized || nextCalled {
		t.Fatalf("hung auth: status=%d nextCalled=%v, want 401 and not called", rec.Code, nextCalled)
	}
}

type authnFunc func(*http.Request) (string, error)

func (f authnFunc) AuthenticateRequest(r *http.Request) (string, error) { return f(r) }
