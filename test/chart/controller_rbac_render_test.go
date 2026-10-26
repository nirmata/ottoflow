/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package chart

import (
	"strings"
	"testing"
)

const aggregateToControllerLabel = "rbac.ottoflow.io/aggregate-to-controller"

// TestControllerCoreRoleGrantsConfigMapWrites pins the grant the controller's agent-executor CA
// publication depends on. For a workflow with an agent step, the controller publishes the CA
// certificate as a ConfigMap in the WorkflowRun's namespace (ensureAgentExecutorCA in
// internal/workflow/controller) using create, get and update on configmaps from the core
// ClusterRole aggregated into the controller role. Narrowing that rule would fail every such run
// outside the install namespace, so it is asserted here for the default and a non-default release.
func TestControllerCoreRoleGrantsConfigMapWrites(t *testing.T) {
	for _, release := range []string{"ottoflow", "myrel"} {
		t.Run(release, func(t *testing.T) {
			var found int
			for _, role := range render(t, release) {
				if !strings.HasSuffix(role.Name, "-role:core") || role.Labels[aggregateToControllerLabel] != "true" {
					continue
				}
				found++
				want := []string{"create", "get", "update"}
				granted := make([]string, 0, len(want))
				for _, rule := range role.Rules {
					granted = append(granted, grantedVerbs(rule, "", "configmaps", want)...)
				}
				for _, verb := range want {
					if !containsOrWildcard(granted, verb) {
						t.Errorf("ClusterRole %s does not grant %s on configmaps (granted: %v); "+
							"the controller needs it to publish the agent-executor CA into run namespaces", role.Name, verb, granted)
					}
				}
			}
			if found != 1 {
				t.Fatalf("expected exactly one controller-aggregated *-role:core ClusterRole, found %d", found)
			}
		})
	}
}

// TestControllerCoreRoleGrantsRoleBindings: the controller binds each runner ServiceAccount to the
// ConfigMap-writer ClusterRole with a RoleBinding, so default installs (serveA2A off) need these verbs.
func TestControllerCoreRoleGrantsRoleBindings(t *testing.T) {
	want := []string{"create", "get", "list", "watch", "update", "delete"}
	for _, role := range render(t, "ottoflow") {
		if !strings.HasSuffix(role.Name, "-role:core") || role.Labels[aggregateToControllerLabel] == "" {
			continue
		}
		granted := make([]string, 0, len(want))
		for _, rule := range role.Rules {
			granted = append(granted, grantedVerbs(rule, "rbac.authorization.k8s.io", "rolebindings", want)...)
		}
		for _, verb := range want {
			if !containsOrWildcard(granted, verb) {
				t.Errorf("ClusterRole %s does not grant %s on rolebindings (granted: %v)", role.Name, verb, granted)
			}
		}
		return
	}
	t.Fatal("controller-aggregated *-role:core ClusterRole not rendered")
}
