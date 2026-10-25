/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package chart

import (
	"os/exec"
	"strings"
	"testing"
)

func TestClusterRoleFlagsRequireRBACOrExplicitName(t *testing.T) {
	const noRBAC = "rbac.create=false"
	tests := []struct {
		name    string
		sets    []string
		want    string
		wantErr bool
	}{
		{"runner_default", nil, "--workflow-runner-cluster-role=ottoflow-runner-role", false},
		{"serveA2A_default", []string{"serveA2A.enabled=true"}, "--serve-a2a-cluster-role=ottoflow-serve-a2a", false},
		{"runner_unset", []string{noRBAC}, "workflowRunner.clusterRole", true},
		{"runner_set", []string{noRBAC, "workflowRunner.clusterRole=ext-runner"},
			"--workflow-runner-cluster-role=ext-runner", false},
		{"serveA2A_unset", []string{noRBAC, "workflowRunner.clusterRole=ext-runner", "serveA2A.enabled=true"},
			"serveA2A.clusterRole", true},
		{"serveA2A_set", []string{noRBAC, "workflowRunner.clusterRole=ext-runner", "serveA2A.enabled=true",
			"serveA2A.clusterRole=ext-a2a"}, "--serve-a2a-cluster-role=ext-a2a", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"template", "ottoflow", chartDir(t), "--namespace", "ottoflow"}
			for _, s := range tc.sets {
				args = append(args, "--set", s)
			}
			out, err := exec.Command(helmBin(t), args...).CombinedOutput()
			if (err != nil) != tc.wantErr {
				t.Fatalf("helm template err = %v, wantErr %v\n%s", err, tc.wantErr, out)
			}
			if !strings.Contains(string(out), tc.want) {
				t.Errorf("output lacks %q\n%s", tc.want, out)
			}
		})
	}
}
