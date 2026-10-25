/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"strings"
	"testing"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

func TestValidateExternalAgentTransport_HTTPRejectedHint(t *testing.T) {
	_, err := ValidateExternalAgentTransport(&ottoflowv1alpha1.StepExternalAgentRef{
		URL: "http://kagent-controller.kagent.svc:8083",
	})
	if err == nil {
		t.Fatal("expected an error for http:// without allowInsecureHTTP")
	}
	msg := err.Error()
	for _, want := range []string{"HTTPS", "Workflow or StepTemplate CRD", "re-apply the Workflow or StepTemplate"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not contain %q", msg, want)
		}
	}
	if strings.Contains(msg, "set allowInsecureHTTP to permit") {
		t.Errorf("error %q still tells the user to set allowInsecureHTTP", msg)
	}
}

func TestIsClusterLocalHTTPHost(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		// cluster-local: plaintext http is acceptable
		{"localhost", true},
		{"127.0.0.1", true},
		{"::1", true},
		{"foo.svc", true},
		{"a.b.svc.cluster.local", true},
		{"FOO.SVC", true},                // case-insensitive
		{"foo.svc.cluster.local.", true}, // trailing FQDN dot
		// not cluster-local: must fail closed
		{"example.com", false},
		{"10.0.0.1", false},
		{"evilsvc.com", false},      // ".svc" is not a suffix here
		{"foo.svc.evil.com", false}, // ".svc" mid-host, not a suffix
	}
	for _, tc := range cases {
		if got := isClusterLocalHTTPHost(tc.host); got != tc.want {
			t.Errorf("isClusterLocalHTTPHost(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}
