/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"fmt"
	"net/url"
	"strings"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

// ValidateExternalAgentTransport enforces the transport-security policy for an
// externalAgentRef URL. It is the single source of truth shared by the admission
// webhook and the runtime A2A client.
//
// Policy:
//   - https:// is always allowed; AllowInsecureHTTP is ignored.
//   - http:// is rejected unless AllowInsecureHTTP is set AND the host is
//     cluster-local AND no bearer token (auth.secretRef) or CA bundle
//     (caSecretRef) is configured.
//   - any other scheme, an unparseable URL, an empty host, or embedded
//     userinfo credentials are rejected.
func ValidateExternalAgentTransport(step *ottoflowv1alpha1.StepExternalAgentRef) (*url.URL, error) {
	u, err := url.Parse(step.URL)
	if err != nil || u.Host == "" || u.User != nil {
		return nil, fmt.Errorf("externalAgentRef URL must be a valid URL with no embedded credentials, got %q", step.URL)
	}

	switch u.Scheme {
	case "https":
		return u, nil
	case "http":
		if !step.AllowInsecureHTTP {
			return nil, fmt.Errorf("externalAgentRef URL must use HTTPS, got %q (allowInsecureHTTP is false on this step as evaluated; if your manifest sets allowInsecureHTTP, the installed Workflow or StepTemplate CRD may predate the field and the API server dropped it: update the CRDs, then re-apply the Workflow or StepTemplate)", step.URL)
		}
		if step.Auth != nil && step.Auth.SecretRef != nil {
			return nil, fmt.Errorf("allowInsecureHTTP cannot be combined with auth.secretRef (a bearer token must not be sent over cleartext http)")
		}
		if step.CASecretRef != nil {
			return nil, fmt.Errorf("externalAgentRef: allowInsecureHTTP cannot be combined with caSecretRef (a CA bundle has no effect over plaintext http)")
		}
		if !isClusterLocalHTTPHost(u.Hostname()) {
			return nil, fmt.Errorf("externalAgentRef URL with allowInsecureHTTP must target a cluster-local host (ending in .svc or .svc.cluster.local, or localhost/127.0.0.1/::1), got %q", step.URL)
		}
		return u, nil
	default:
		return nil, fmt.Errorf("externalAgentRef URL scheme %q is not supported", u.Scheme)
	}
}

// isClusterLocalHTTPHost reports whether host is a cluster-local hostname for which
// plaintext http:// is acceptable. It fails closed: any host that is not explicitly
// recognized as cluster-local returns false.
func isClusterLocalHTTPHost(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, ".")) // lowercase + strip FQDN trailing dot
	return h == "localhost" || h == "127.0.0.1" || h == "::1" ||
		strings.HasSuffix(h, ".svc") || strings.HasSuffix(h, ".svc.cluster.local")
}
