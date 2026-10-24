/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// captureTokenReview returns a clientset that approves every token and records the TokenReview.
func captureTokenReview() (*fake.Clientset, *authenticationv1.TokenReview) {
	var got authenticationv1.TokenReview
	c := fake.NewSimpleClientset()
	c.PrependReactor("create", "tokenreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		tr := a.(k8stesting.CreateAction).GetObject().(*authenticationv1.TokenReview)
		got = *tr.DeepCopy()
		tr.Status = authenticationv1.TokenReviewStatus{Authenticated: true, Audiences: tr.Spec.Audiences, User: authenticationv1.UserInfo{Username: "system:serviceaccount:ns:sa"}}
		return true, tr, nil
	})
	c.PrependReactor("create", "subjectaccessreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		sar := a.(k8stesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview)
		sar.Status.Allowed = true
		return true, sar, nil
	})
	return c, &got
}

func bearerRequest() *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(""))
	r.Header.Set("Authorization", "Bearer tok")
	return r
}

// Existing callers (MCP, agent-executor) build the authenticator without audiences. Their
// TokenReview must stay byte-identical on the wire: no audiences field.
func TestTokenReviewAudiencesOmittedByDefault(t *testing.T) {
	c, got := captureTokenReview()
	a := NewTokenReviewAndSARAuthenticator(c, "ns", "")
	if _, err := a.AuthenticateRequest(bearerRequest()); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Audiences != nil {
		t.Fatalf("default authenticator sent audiences %v, want none", got.Spec.Audiences)
	}
}

func TestWithAudiencesSendsThemAndLeavesReceiverAlone(t *testing.T) {
	c, got := captureTokenReview()
	base := NewTokenReviewAndSARAuthenticator(c, "ns", "")
	scoped := base.WithAudiences("ottoflow-a2a")
	if _, err := scoped.AuthenticateRequest(bearerRequest()); err != nil {
		t.Fatal(err)
	}
	if len(got.Spec.Audiences) != 1 || got.Spec.Audiences[0] != "ottoflow-a2a" {
		t.Fatalf("audiences = %v, want [ottoflow-a2a]", got.Spec.Audiences)
	}
	if _, err := base.AuthenticateRequest(bearerRequest()); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Audiences != nil {
		t.Fatalf("WithAudiences mutated the receiver: audiences = %v", got.Spec.Audiences)
	}
}

func TestMissingReviewerAccess(t *testing.T) {
	allowOnly := func(allowed map[string]bool) *fake.Clientset {
		c := fake.NewSimpleClientset()
		c.PrependReactor("create", "selfsubjectaccessreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
			r := a.(k8stesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
			r.Status.Allowed = allowed[r.Spec.ResourceAttributes.Resource]
			return true, r, nil
		})
		return c
	}
	if m := MissingReviewerAccess(context.Background(), allowOnly(map[string]bool{"tokenreviews": true, "subjectaccessreviews": true})); len(m) != 0 {
		t.Errorf("both allowed: missing = %v, want none", m)
	}
	m := MissingReviewerAccess(context.Background(), allowOnly(map[string]bool{"tokenreviews": true}))
	if len(m) != 1 || !strings.Contains(m[0], "subjectaccessreviews") {
		t.Errorf("only tokenreviews allowed: missing = %v, want one entry naming subjectaccessreviews", m)
	}
	if m := MissingReviewerAccess(context.Background(), allowOnly(nil)); len(m) != 2 {
		t.Errorf("nothing allowed: missing = %v, want 2 entries", m)
	}
}

// An audience-unaware reviewer answers authenticated=true with no (or other) audiences. When
// audiences were requested that must be refused before any SubjectAccessReview.
func TestWithAudiencesRejectsResponsesNotNamingARequestedAudience(t *testing.T) {
	for name, got := range map[string][]string{"empty": nil, "disjoint": {"https://kubernetes.default.svc"}} {
		t.Run(name, func(t *testing.T) {
			c := fake.NewSimpleClientset()
			sarCalls := 0
			c.PrependReactor("create", "tokenreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
				tr := a.(k8stesting.CreateAction).GetObject().(*authenticationv1.TokenReview)
				tr.Status = authenticationv1.TokenReviewStatus{Authenticated: true, Audiences: got, User: authenticationv1.UserInfo{Username: "system:serviceaccount:ns:sa"}}
				return true, tr, nil
			})
			c.PrependReactor("create", "subjectaccessreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
				sarCalls++
				sar := a.(k8stesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview)
				sar.Status.Allowed = true
				return true, sar, nil
			})
			if _, err := NewTokenReviewAndSARAuthenticator(c, "ns", "").WithAudiences("ottoflow-a2a").AuthenticateRequest(bearerRequest()); err == nil || sarCalls != 0 {
				t.Fatalf("err=%v sarCalls=%d, want an error and no SubjectAccessReview", err, sarCalls)
			}
		})
	}
}

func TestSARChecksVirtualCallerResource(t *testing.T) {
	var got authorizationv1.SubjectAccessReviewSpec
	c, _ := captureTokenReview()
	c.PrependReactor("create", "subjectaccessreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		sar := a.(k8stesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview)
		got = *sar.Spec.DeepCopy()
		sar.Status.Allowed = true
		return true, sar, nil
	})
	if _, err := NewTokenReviewAndSARAuthenticator(c, "check-ns", MCPCallerResourceName).AuthenticateRequest(bearerRequest()); err != nil {
		t.Fatal(err)
	}
	want := authorizationv1.ResourceAttributes{Namespace: "check-ns", Verb: "invoke", Group: "gate.ottoflow.nirmata.io", Resource: "callers", Name: "mcp-caller"}
	if ra := got.ResourceAttributes; ra == nil || *ra != want {
		t.Fatalf("SAR resource attributes = %+v, want %+v", ra, want)
	}
	if got.User != "system:serviceaccount:ns:sa" {
		t.Fatalf("SAR user = %q", got.User)
	}
}
