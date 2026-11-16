/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"context"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/nirmata/ottoflow/internal/workflow/executor"
)

const jobWriterRole = "ottoflow-runner-job-writer"

func TestEnsureRunnerJobWriter(t *testing.T) {
	key := types.NamespacedName{Namespace: cmWriterNS, Name: runnerJobWriterBindingName(cmWriterSA)}

	c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(cmWriterSAObj()).Build()
	r := &WorkflowRunReconciler{Client: c, Scheme: unitTestScheme, RunnerConfig: RunnerConfig{RunnerJobWriterRole: jobWriterRole}}
	if err := r.ensureRunnerJobWriter(context.Background(), cmWriterNS, cmWriterSA, cmWriterUID); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), key, &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
		t.Fatalf("no binding expected while container steps are disabled, got %v", err)
	}

	r.RunnerConfig.ContainerStepAllowedImages = []string{"ghcr.io/kyverno/kyverno-cli"}
	if err := r.ensureRunnerJobWriter(context.Background(), cmWriterNS, cmWriterSA, cmWriterUID); err != nil {
		t.Fatal(err)
	}
	rb := &rbacv1.RoleBinding{}
	if err := c.Get(context.Background(), key, rb); err != nil {
		t.Fatal(err)
	}
	if rb.RoleRef.Kind != "ClusterRole" || rb.RoleRef.Name != jobWriterRole ||
		len(rb.Subjects) != 1 || rb.Subjects[0].Name != cmWriterSA || rb.Subjects[0].Namespace != cmWriterNS ||
		len(rb.OwnerReferences) != 1 || rb.OwnerReferences[0].UID != cmWriterUID {
		t.Errorf("binding = %+v", rb)
	}
}

func TestBuildWorkflowRunnerJob_ContainerStepEnv(t *testing.T) {
	wf, secret, wr := reservedEnvFixture(nil)
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(wf, secret, wr).Build()
	r := &WorkflowRunReconciler{Client: c, Scheme: unitTestScheme, RunnerConfig: RunnerConfig{
		ContainerStepAllowedImages: []string{"a", "b/"}, ContainerStepMaxOutputBytes: 2048,
	}}
	job, err := r.buildWorkflowRunnerJob(context.Background(), wr, wf)
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	if env[executor.ContainerStepAllowedImagesEnv] != "a,b/" || env[executor.ContainerStepMaxOutputBytesEnv] != "2048" {
		t.Errorf("runner env = %v", env)
	}

	r.RunnerConfig = RunnerConfig{}
	job, err = r.buildWorkflowRunnerJob(context.Background(), wr, wf)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		if e.Name == executor.ContainerStepAllowedImagesEnv {
			t.Error("allowlist env must be absent when container steps are disabled")
		}
	}
}
