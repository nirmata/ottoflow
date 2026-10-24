/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package chart

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
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

func TestNoRenderedClusterRoleGrantsAllGroupsAllResources(t *testing.T) {
	renders := map[string][]string{
		"default": nil,
		"enabled": {
			"--set", "agentExecutor.enabled=true",
			"--set", "controller.mcp.enabled=true",
			"--set", "serveA2A.enabled=true",
		},
	}
	for name, args := range renders {
		t.Run(name, func(t *testing.T) {
			roles := renderedClusterRoles(t, renderDocs(t, args...))
			if name == "enabled" {
				if _, ok := roles["ottoflow-a2a-caller"]; !ok {
					t.Fatal("enabled values did not render ottoflow-a2a-caller; the --set paths are stale")
				}
			}
			for _, role := range roles {
				for _, rule := range role.Rules {
					if slices.Contains(rule.APIGroups, "*") && slices.Contains(rule.Resources, "*") {
						t.Errorf("ClusterRole %s grants apiGroups * with resources * (shipped roles must not): %+v",
							role.Name, rule)
					}
				}
			}
		})
	}
}
