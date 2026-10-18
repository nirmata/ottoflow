/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package chart

import (
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/yaml"
)

// TestDefaultNetworkPolicyAllowsAPIServerPort6443 asserts that the default NetworkPolicy has a
// port-only TCP 6443 egress rule. On clusters whose API server listens on 6443 behind the
// kubernetes Service (kind, kubeadm, k3s, RKE2), kube-proxy rewrites the destination to :6443
// before the CNI enforces egress policy, so a rule for 443 alone drops the controller's API calls.
func TestDefaultNetworkPolicyAllowsAPIServerPort6443(t *testing.T) {
	output, err := helmTemplate(t, "ottoflow", "")
	if err != nil {
		t.Fatalf("helm template failed: %v", err)
	}

	var found bool
	for _, doc := range splitYAMLDocs(output) {
		var np networkingv1.NetworkPolicy
		if err := yaml.Unmarshal([]byte(strings.TrimSpace(doc)), &np); err != nil || np.Kind != "NetworkPolicy" {
			continue
		}
		for _, rule := range np.Spec.Egress {
			if len(rule.To) != 0 {
				continue
			}
			for _, p := range rule.Ports {
				if p.Port != nil && p.Port.IntValue() == 6443 && (p.Protocol == nil || *p.Protocol == "TCP") {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatal("default NetworkPolicy has no port-only TCP 6443 egress rule")
	}
}
