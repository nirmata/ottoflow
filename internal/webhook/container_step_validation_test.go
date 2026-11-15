/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package webhook

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

func containerStepWorkflow(steps ...ottoflowv1alpha1.Step) *ottoflowv1alpha1.Workflow {
	return &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Inputs: []ottoflowv1alpha1.Input{{Name: "image"}},
			Steps:  steps,
		},
	}
}

func TestWorkflowValidator_ContainerStep(t *testing.T) {
	allowed := []string{"ghcr.io/kyverno/kyverno-cli"}
	step := func(image string) ottoflowv1alpha1.Step {
		return ottoflowv1alpha1.Step{Name: "evaluate", Container: &ottoflowv1alpha1.StepContainer{Image: image}}
	}
	cases := []struct {
		name    string
		allowed []string
		step    ottoflowv1alpha1.Step
		wantErr string
	}{
		{name: "allowed image", allowed: allowed, step: step("ghcr.io/kyverno/kyverno-cli:v1.15.0")},
		{name: "templated image is checked by the runner", allowed: allowed, step: step("{{ inputs.image }}")},
		{name: "image not on allowlist", allowed: allowed, step: step("busybox:latest"), wantErr: "allowlist"},
		{name: "container steps disabled", step: step("ghcr.io/kyverno/kyverno-cli:v1"), wantErr: "disabled"},
		{name: "bad timeout", allowed: allowed, step: ottoflowv1alpha1.Step{Name: "evaluate", Container: &ottoflowv1alpha1.StepContainer{
			Image: "ghcr.io/kyverno/kyverno-cli:v1", Timeout: "soon"}}, wantErr: "timeout"},
		{name: "files over kubeconfig", allowed: allowed, step: ottoflowv1alpha1.Step{Name: "evaluate", Container: &ottoflowv1alpha1.StepContainer{
			Image: "ghcr.io/kyverno/kyverno-cli:v1", Kubeconfig: true,
			Files: &ottoflowv1alpha1.StepContainerFiles{ConfigMapRef: "cm", MountPath: "/kube/"}}}, wantErr: "/kube"},
		{name: "two action fields", allowed: allowed, step: ottoflowv1alpha1.Step{Name: "evaluate",
			Container: &ottoflowv1alpha1.StepContainer{Image: "ghcr.io/kyverno/kyverno-cli:v1"},
			Mutate:    &ottoflowv1alpha1.StepMutate{}}, wantErr: "exactly one action"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := &WorkflowValidator{ContainerStepAllowedImages: tc.allowed}
			_, err := v.ValidateCreate(context.Background(), containerStepWorkflow(tc.step))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidateStepDependencies_ContainerTemplateStepsRef(t *testing.T) {
	spec := &ottoflowv1alpha1.WorkflowSpec{Steps: []ottoflowv1alpha1.Step{
		{Name: "prepare", Outputs: []ottoflowv1alpha1.Output{{Name: "cm", Expression: "'x'"}}},
		{Name: "evaluate", Container: &ottoflowv1alpha1.StepContainer{
			Image: "ghcr.io/kyverno/kyverno-cli:v1",
			Files: &ottoflowv1alpha1.StepContainerFiles{ConfigMapRef: "{{ steps.prepare.cm }}", MountPath: "/in"},
		}},
	}}
	if err := ValidateStepDependencies(spec); err == nil {
		t.Fatal("a steps.* reference in a container template without dependsOn must be rejected")
	}
	spec.Steps[1].DependsOn = []string{"prepare"}
	if err := ValidateStepDependencies(spec); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
