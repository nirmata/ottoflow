/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package webhook

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/workflow/executor"
)

// WorkflowValidator validates Workflow resources at admission time.
type WorkflowValidator struct {
	Client client.Client
	// Authorizer decides whether the author may run workflows as the
	// ServiceAccount this Workflow declares. A nil Authorizer rejects every
	// Workflow that declares one.
	Authorizer SubjectAccessReviewer
}

// ValidateCreate implements admission.Validator.
func (v *WorkflowValidator) ValidateCreate(ctx context.Context, w *ottoflowv1alpha1.Workflow) (admission.Warnings, error) {
	return v.validate(ctx, w)
}

// ValidateUpdate implements admission.Validator.
func (v *WorkflowValidator) ValidateUpdate(ctx context.Context, oldW, w *ottoflowv1alpha1.Workflow) (admission.Warnings, error) {
	return v.validate(ctx, w)
}

// ValidateDelete implements admission.Validator.
func (v *WorkflowValidator) ValidateDelete(ctx context.Context, w *ottoflowv1alpha1.Workflow) (admission.Warnings, error) {
	return nil, nil
}

func (v *WorkflowValidator) validate(ctx context.Context, w *ottoflowv1alpha1.Workflow) (admission.Warnings, error) {
	if w == nil {
		return nil, nil
	}
	if err := v.authorizeWorkflowServiceAccount(ctx, w); err != nil {
		return nil, err
	}
	if len(w.Spec.Steps) == 0 {
		return nil, nil
	}
	if _, err := executor.BuildDAG(w.Spec.Steps); err != nil {
		return nil, fmt.Errorf("workflow %q invalid: %w", w.Name, err)
	}
	if err := ValidateStepDependencies(&w.Spec); err != nil {
		return nil, fmt.Errorf("workflow %q invalid: %w", w.Name, err)
	}
	if err := ValidateInputRefs(&w.Spec); err != nil {
		return nil, fmt.Errorf("workflow %q invalid: %w", w.Name, err)
	}
	if err := validateWebhookTriggers(w); err != nil {
		return nil, err
	}
	// Structural validation — runs regardless of whether a k8s client is available
	for _, step := range w.Spec.Steps {
		if err := validateStepExclusiveAction(step.Name, step); err != nil {
			return nil, err
		}
		if err := validateExternalAgentRef(step.Name, step.ExternalAgentRef); err != nil {
			return nil, err
		}
		if err := validateOpenReport(step.Name, step.OpenReport); err != nil {
			return nil, err
		}
		if err := validateWaitForCallback(step.Name, step.WaitForCallback, step.ForEach); err != nil {
			return nil, err
		}
		// Also validate forEach inline step's externalAgentRef and openReport
		if step.ForEach != nil && step.ForEach.Step != nil {
			if err := validateForEachStepExclusiveAction(step.Name+" (forEach)", step.ForEach.Step); err != nil {
				return nil, err
			}
			if err := validateExternalAgentRef(step.Name+" (forEach)", step.ForEach.Step.ExternalAgentRef); err != nil {
				return nil, err
			}
			if err := validateOpenReport(step.Name+" (forEach)", step.ForEach.Step.OpenReport); err != nil {
				return nil, err
			}
		}
	}

	var warnings admission.Warnings
	if v.Client != nil {
		ns := w.Namespace
		if ns == "" {
			ns = "default"
		}
		// Optional: ensure referenced Workflow templates exist (same namespace)
		seen := make(map[string]struct{})
		for _, step := range w.Spec.Steps {
			if step.WorkflowRef != nil && step.WorkflowRef.Name != "" {
				key := step.WorkflowRef.Name
				if _, ok := seen[key]; ok {
					continue
				}
				seen[key] = struct{}{}
				var ref ottoflowv1alpha1.Workflow
				refKey := client.ObjectKey{Namespace: namespaceOr(step.WorkflowRef.Namespace, ns), Name: step.WorkflowRef.Name}
				if err := v.Client.Get(ctx, refKey, &ref); err != nil {
					if apierrors.IsNotFound(err) {
						return nil, fmt.Errorf("workflow step %q references workflow %q not found in namespace %q", step.Name, step.WorkflowRef.Name, refKey.Namespace)
					}
					// transient errors: warn and allow
					warnings = append(warnings, fmt.Sprintf("could not verify WorkflowRef %q: %v", step.WorkflowRef.Name, err))
				}
			}
			if step.AgentRef != nil && step.AgentRef.Name != "" {
				key := "agent/" + step.AgentRef.Name
				if _, ok := seen[key]; ok {
					continue
				}
				seen[key] = struct{}{}
				var ref ottoflowv1alpha1.Agent
				refKey := client.ObjectKey{Namespace: namespaceOr(step.AgentRef.Namespace, ns), Name: step.AgentRef.Name}
				if err := v.Client.Get(ctx, refKey, &ref); err != nil {
					if apierrors.IsNotFound(err) {
						return nil, fmt.Errorf("workflow step %q references agent %q not found in namespace %q", step.Name, step.AgentRef.Name, ns)
					}
					warnings = append(warnings, fmt.Sprintf("could not verify AgentRef %q: %v", step.AgentRef.Name, err))
				}
			}
			// MCPServer refs appear in agent or mcp tool steps — validate Agent/MCP at agent CRD or step level if needed later
		}
	}
	return warnings, nil
}

func namespaceOr(ns, fallback string) string {
	if ns != "" {
		return ns
	}
	return fallback
}

func validateOpenReport(stepName string, ref *ottoflowv1alpha1.StepOpenReport) error {
	if ref == nil {
		return nil
	}
	if ref.ReportName == "" {
		return fmt.Errorf("workflow step %q: openReport reportName must not be empty", stepName)
	}
	if ref.ResultsExpression == "" {
		return fmt.Errorf("workflow step %q: openReport resultsExpression must not be empty", stepName)
	}
	return nil
}

func validateStepExclusiveAction(stepName string, step ottoflowv1alpha1.Step) error {
	count := 0
	if step.WorkflowRef != nil {
		count++
	}
	if step.AgentRef != nil {
		count++
	}
	if step.MCPToolCall != nil {
		count++
	}
	if step.ResourceQuery != nil {
		count++
	}
	if step.PrometheusQuery != nil {
		count++
	}
	if step.Mutate != nil {
		count++
	}
	if step.StepTemplateRef != nil {
		count++
	}
	if step.ForEach != nil {
		count++
	}
	if step.ExternalAgentRef != nil {
		count++
	}
	if step.OpenReport != nil {
		count++
	}
	if step.WaitForCallback != nil {
		count++
	}
	if count > 1 {
		return fmt.Errorf("workflow step %q: exactly one action field must be set, found %d", stepName, count)
	}
	return nil
}

func validateForEachStepExclusiveAction(stepName string, inner *ottoflowv1alpha1.StepForEachStep) error {
	if inner == nil {
		return nil
	}
	count := 0
	if inner.ResourceQuery != nil {
		count++
	}
	if inner.PrometheusQuery != nil {
		count++
	}
	if inner.Mutate != nil {
		count++
	}
	if inner.AgentRef != nil {
		count++
	}
	if inner.MCPToolCall != nil {
		count++
	}
	if inner.WorkflowRef != nil {
		count++
	}
	if inner.ExternalAgentRef != nil {
		count++
	}
	if inner.OpenReport != nil {
		count++
	}
	if count > 1 {
		return fmt.Errorf("workflow step %q: exactly one action field must be set, found %d", stepName, count)
	}
	return nil
}

func validateExternalAgentRef(stepName string, ref *ottoflowv1alpha1.StepExternalAgentRef) error {
	if ref == nil || ref.URL == "" {
		return nil
	}
	if _, err := executor.ValidateExternalAgentTransport(ref); err != nil {
		return fmt.Errorf("workflow step %q: %w", stepName, err)
	}
	if ref.Protocol != "" && ref.Protocol != "a2a" {
		return fmt.Errorf("workflow step %q: externalAgentRef protocol %q is not supported (only \"a2a\" is supported)", stepName, ref.Protocol)
	}
	return nil
}

// validateWaitForCallback validates the waitForCallback step configuration.
// Rejects waitForCallback inside forEach (single PendingCallback slot cannot handle parallel callbacks).
func validateWaitForCallback(stepName string, wfc *ottoflowv1alpha1.WaitForCallbackStep, forEach *ottoflowv1alpha1.StepForEach) error {
	if wfc == nil {
		return nil
	}
	if forEach != nil {
		return fmt.Errorf("workflow step %q: waitForCallback is not supported inside forEach (single PendingCallback slot cannot handle parallel callbacks)", stepName)
	}
	if wfc.Timeout == "" {
		return fmt.Errorf("workflow step %q: waitForCallback.timeout is required", stepName)
	}
	if _, err := time.ParseDuration(wfc.Timeout); err != nil {
		return fmt.Errorf("workflow step %q: waitForCallback.timeout %q is not a valid duration: %v", stepName, wfc.Timeout, err)
	}
	return nil
}

// validateWebhookTriggers enforces webhook-specific constraints that cannot be expressed
// via kubebuilder markers (e.g. maximum DedupWindow, cross-namespace secretRef rejection).
func validateWebhookTriggers(w *ottoflowv1alpha1.Workflow) error {
	var webhookCount int
	for i, t := range w.Spec.Triggers {
		if t.Webhook == nil {
			continue
		}
		webhookCount++
		if webhookCount > 1 {
			return field.Invalid(
				field.NewPath("spec", "triggers").Index(i).Child("webhook"),
				t.Webhook,
				"at most one webhook trigger is allowed per Workflow; the path /webhooks/{namespace}/{name} is unique per workflow",
			)
		}
		wt := t.Webhook
		// Same-namespace secretRef: cross-namespace references are rejected in v1 to
		// limit the RBAC blast radius (secrets/get is a ClusterRole).
		// v2 escape hatch: add explicit multi-tenancy controls and lift this restriction.
		if wt.SecretRef.Namespace != "" && wt.SecretRef.Namespace != w.Namespace {
			return field.Invalid(
				field.NewPath("spec", "triggers").Index(i).Child("webhook", "secretRef", "namespace"),
				wt.SecretRef.Namespace,
				fmt.Sprintf("secretRef.namespace must equal the Workflow namespace %q; cross-namespace secret references are not supported in v1", w.Namespace),
			)
		}
		// DedupWindow max 1h — kubebuilder:validation:Maximum cannot validate Duration (marshals as string).
		if wt.DedupWindow != nil && wt.DedupWindow.Duration > time.Hour {
			return field.Invalid(
				field.NewPath("spec", "triggers").Index(i).Child("webhook", "dedupWindow"),
				wt.DedupWindow.Duration.String(),
				"dedupWindow must not exceed 1 hour; longer windows silently suppress all requests after the first one within the window",
			)
		}
	}
	return nil
}
