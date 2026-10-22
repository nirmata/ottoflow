/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package webhook

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

// WorkflowRunValidator validates WorkflowRun resources at admission time.
type WorkflowRunValidator struct {
	// Authorizer decides whether the submitter may run as the ServiceAccount a
	// run names. A nil Authorizer rejects every run that names one.
	Authorizer SubjectAccessReviewer
	// Client reads the run's Workflow, to tell a run that inherited its
	// ServiceAccount from one that chose it.
	Client client.Client
}

// ValidateCreate implements admission.Validator.
func (v *WorkflowRunValidator) ValidateCreate(ctx context.Context, run *ottoflowv1alpha1.WorkflowRun) (admission.Warnings, error) {
	return v.validate(ctx, run)
}

// ValidateUpdate implements admission.Validator.
func (v *WorkflowRunValidator) ValidateUpdate(ctx context.Context, oldRun, run *ottoflowv1alpha1.WorkflowRun) (admission.Warnings, error) {
	// Checked on update too: the field is mutable, so authorizing only creates
	// would let a run be submitted with no ServiceAccount and then patched.
	return v.validate(ctx, run)
}

// ValidateDelete implements admission.Validator.
func (v *WorkflowRunValidator) ValidateDelete(ctx context.Context, run *ottoflowv1alpha1.WorkflowRun) (admission.Warnings, error) {
	return nil, nil
}

func (v *WorkflowRunValidator) validate(ctx context.Context, run *ottoflowv1alpha1.WorkflowRun) (admission.Warnings, error) {
	if run == nil {
		return nil, nil
	}
	if run.Spec.WorkflowRef.Name == "" {
		return nil, fmt.Errorf("WorkflowRun %q spec.workflowRef.name is required", run.Name)
	}
	if run.Spec.Execution != nil && run.Spec.Execution.LLMCredentialsSecret != nil {
		ref := run.Spec.Execution.LLMCredentialsSecret
		if ref.Name == "" {
			return nil, fmt.Errorf("WorkflowRun %q spec.execution.llmCredentialsSecret.name is required when llmCredentialsSecret is set", run.Name)
		}
		if ref.Namespace != "" && ref.Namespace != run.Namespace {
			return nil, fmt.Errorf("WorkflowRun %q spec.execution.llmCredentialsSecret.namespace must be empty or match the WorkflowRun namespace %q; cross-namespace Secret references are not supported because SecretKeyRef is namespace-scoped to the runner pod", run.Name, run.Namespace)
		}
	}
	if err := v.authorizeRunnerServiceAccount(ctx, run); err != nil {
		return nil, err
	}
	return nil, nil
}
