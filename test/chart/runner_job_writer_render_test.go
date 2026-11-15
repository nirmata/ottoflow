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

const (
	jobWriterFlag = "--runner-job-writer-cluster-role"
	jobWriterRole = "ottoflow-runner-job-writer"
)

func TestRunnerJobWriter_OffByDefault(t *testing.T) {
	docs := renderDocs(t)
	if _, ok := renderedClusterRoles(t, docs)[jobWriterRole]; ok {
		t.Errorf("ClusterRole %s rendered without containerStep.allowedImages", jobWriterRole)
	}
	dep := controllerDeployment(t, docs)
	for _, flag := range []string{jobWriterFlag, "--container-step-allowed-images", "--container-step-max-output-bytes"} {
		if strings.Contains(dep, flag) {
			t.Errorf("controller Deployment passes %s without containerStep.allowedImages", flag)
		}
	}
}

func TestRunnerJobWriter_RoleAndFlags(t *testing.T) {
	docs := renderDocs(t,
		"--set", "containerStep.allowedImages={ghcr.io/kyverno/kyverno-cli,registry.example.com/tools/}",
		"--set", "containerStep.maxOutputBytes=2097152")
	role, ok := renderedClusterRoles(t, docs)[jobWriterRole]
	if !ok {
		t.Fatalf("ClusterRole %s not rendered", jobWriterRole)
	}
	if role.Labels[aggregateToRunnerLabel] != "" {
		t.Errorf("job writer role must not aggregate to the runner role: %v", role.Labels)
	}
	if len(role.Rules) != 1 || !ruleIs(role.Rules[0], "batch", "jobs", "", "create,delete") {
		t.Errorf("job writer rules = %+v, want batch jobs create,delete", role.Rules)
	}
	dep := controllerDeployment(t, docs)
	for _, want := range []string{
		jobWriterFlag + "=" + jobWriterRole,
		"--container-step-allowed-images=ghcr.io/kyverno/kyverno-cli,registry.example.com/tools/",
		"--container-step-max-output-bytes=2097152",
	} {
		if !strings.Contains(dep, want) {
			t.Errorf("controller Deployment lacks %s", want)
		}
	}
}
