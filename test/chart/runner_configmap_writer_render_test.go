/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package chart

import (
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

const (
	writerFlag = "--runner-configmap-writer-cluster-role"
	writerRole = "ottoflow-runner-configmap-writer"
)

func renderedClusterRoles(t *testing.T, docs []string) map[string]rbacv1.ClusterRole {
	t.Helper()
	out := map[string]rbacv1.ClusterRole{}
	for _, d := range docs {
		if docKind(d).Kind != "ClusterRole" {
			continue
		}
		var cr rbacv1.ClusterRole
		if err := yaml.Unmarshal([]byte(d), &cr); err != nil {
			t.Fatal(err)
		}
		out[cr.Name] = cr
	}
	return out
}

func controllerDeployment(t *testing.T, docs []string) string {
	t.Helper()
	for _, d := range docs {
		if docKind(d).Kind == "Deployment" && strings.Contains(d, "--workflow-runner-cluster-role=") {
			return d
		}
	}
	t.Fatal("controller Deployment not rendered")
	return ""
}

func TestRunnerConfigMapWriter_RoleAndFlag(t *testing.T) {
	docs := renderDocs(t)
	writer, ok := renderedClusterRoles(t, docs)[writerRole]
	if !ok {
		t.Fatalf("ClusterRole %s not rendered", writerRole)
	}
	if writer.Labels[aggregateToRunnerLabel] != "" {
		t.Errorf("writer role must not aggregate to the runner role: %v", writer.Labels)
	}
	if len(writer.Rules) != 1 {
		t.Fatalf("writer rules = %+v, want one", writer.Rules)
	}
	if !ruleIs(writer.Rules[0], "", "configmaps", "", "create,delete,patch,update") {
		t.Errorf("writer rule = %+v, want configmaps create,delete,patch,update", writer.Rules[0])
	}
	if !strings.Contains(controllerDeployment(t, docs), writerFlag+"="+writerRole) {
		t.Errorf("controller Deployment lacks %s=%s", writerFlag, writerRole)
	}
}

func TestRunnerConfigMapWriter_NotRenderedWithoutRBAC(t *testing.T) {
	docs := renderDocs(t, "--set", "rbac.create=false", "--set", "workflowRunner.clusterRole=ext-runner")
	if _, ok := renderedClusterRoles(t, docs)[writerRole]; ok {
		t.Error("writer ClusterRole rendered with rbac.create=false")
	}
	if strings.Contains(controllerDeployment(t, docs), writerFlag) {
		t.Errorf("controller Deployment passes %s with rbac.create=false", writerFlag)
	}
}
