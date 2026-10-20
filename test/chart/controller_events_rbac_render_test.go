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

// TestControllerCoreRoleGrantsEventsCreateAndPatch pins the events verbs the controller's
// recorder sends. A repeated Event is a series update, sent as a PATCH; a role with create alone
// gets the PATCH refused, the recorder does not retry it, and the repeated Warning is lost.
func TestControllerCoreRoleGrantsEventsCreateAndPatch(t *testing.T) {
	want := []string{"create", "patch"}
	var found int
	for _, role := range render(t, "ottoflow") {
		if !strings.HasSuffix(role.Name, "-role:core") || role.Labels["rbac.ottoflow.io/aggregate-to-controller"] != "true" {
			continue
		}
		found++
		for _, group := range []string{"", "events.k8s.io"} {
			var granted []string
			for _, rule := range role.Rules {
				granted = append(granted, grantedVerbs(rule, group, "events", want)...)
			}
			for _, verb := range want {
				if !containsOrWildcard(granted, verb) {
					t.Errorf("ClusterRole %s does not grant %s on events in API group %q (granted: %v)",
						role.Name, verb, group, granted)
				}
			}
		}
	}
	if found != 1 {
		t.Fatalf("expected exactly one controller-aggregated *-role:core ClusterRole, found %d", found)
	}
}
