/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package chart

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/nirmata/ottoflow/internal/auth"
)

// The caller permission is virtual: nothing may serve auth.CallerAPIGroup, or roles written
// for it (e.g. verbs "*" on the group) would satisfy the SAR.
func TestNoCRDServesCallerAPIGroup(t *testing.T) {
	var checked int
	chart := chartDir(t)
	dirs := []string{filepath.Join(chart, "..", "..", "config", "crd", "bases"), filepath.Join(chart, "crds")}
	for _, dir := range dirs {
		files, _ := filepath.Glob(filepath.Join(dir, "*.yaml"))
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			var crd apiextv1.CustomResourceDefinition
			if err := yaml.Unmarshal(b, &crd); err != nil {
				t.Fatalf("%s: %v", f, err)
			}
			checked++
			if crd.Spec.Group == auth.CallerAPIGroup {
				t.Errorf("%s declares spec.group %q, reserved for the virtual caller permission", f, auth.CallerAPIGroup)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no CRD files found")
	}
}

var callerRoles = map[string]bool{
	"ottoflow-agent-executor-caller": true,
	"ottoflow-mcp-caller":            true,
	"ottoflow-a2a-caller":            true,
}

// Only the caller roles may grant the gate permission, wildcards included.
func TestOnlyCallerRolesGrantCallerPermission(t *testing.T) {
	renders := map[string][]string{
		"default": nil,
		"enabled": {
			"--set", "agentExecutor.enabled=true",
			"--set", "controller.mcp.enabled=true",
			"--set", "serveA2A.enabled=true",
		},
	}
	verb := []string{auth.CallerVerb}
	for name, args := range renders {
		t.Run(name, func(t *testing.T) {
			rules := map[string][]rbacv1.PolicyRule{}
			aggregated := map[string]bool{}
			for _, d := range renderDocs(t, args...) {
				var o struct {
					Kind     string              `json:"kind"`
					Metadata metav1.ObjectMeta   `json:"metadata"`
					Rules    []rbacv1.PolicyRule `json:"rules"`
				}
				if err := yaml.Unmarshal([]byte(d), &o); err != nil {
					t.Fatal(err)
				}
				if o.Kind == "ClusterRole" || o.Kind == "Role" {
					key := o.Kind + "/" + o.Metadata.Name
					rules[key] = append(rules[key], o.Rules...)
					for l := range o.Metadata.Labels {
						aggregated[key] = aggregated[key] || strings.Contains(l, "aggregate-to")
					}
				}
			}
			if name == "enabled" {
				for role := range callerRoles {
					if _, ok := rules["ClusterRole/"+role]; !ok {
						t.Fatalf("enabled values did not render %s; the --set paths are stale", role)
					}
				}
			}
			for role, rs := range rules {
				granted := false
				for _, r := range rs {
					granted = granted || len(grantedVerbs(r, auth.CallerAPIGroup, auth.CallerResource, verb)) > 0
				}
				if granted != callerRoles[strings.TrimPrefix(role, "ClusterRole/")] {
					t.Errorf("%s grants the caller permission = %v, want the opposite", role, granted)
				}
				if granted && aggregated[role] {
					t.Errorf("%s grants the caller permission and aggregates into other roles", role)
				}
			}
		})
	}
}
