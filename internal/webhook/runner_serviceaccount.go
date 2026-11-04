/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package webhook

import (
	"context"
	"fmt"

	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

// UseVerb is the verb a subject needs on a ServiceAccount to run a workflow as
// it. Grant it with a Role naming the ServiceAccounts a subject may borrow:
//
//	rules:
//	- apiGroups: [""]
//	  resources: ["serviceaccounts"]
//	  resourceNames: ["build-runner"]
//	  verbs: ["use"]
const UseVerb = "use"

// SubjectAccessReviewer creates SubjectAccessReviews.
// kubernetes.Interface's AuthorizationV1().SubjectAccessReviews() satisfies it.
type SubjectAccessReviewer interface {
	Create(ctx context.Context, sar *authorizationv1.SubjectAccessReview, opts metav1.CreateOptions) (*authorizationv1.SubjectAccessReview, error)
}

// authorizeRunnerServiceAccount checks that whoever submitted this WorkflowRun
// may run workloads as the ServiceAccount it names.
//
// Without it, spec.execution.job.serviceAccountName is an escalation: the
// runner Job is launched with that ServiceAccount's token, so anyone who can
// create a WorkflowRun can name a privileged ServiceAccount and get its
// credentials, which is what scoping the runner to its own least-privilege
// role set out to prevent.
func (v *WorkflowRunValidator) authorizeRunnerServiceAccount(ctx context.Context, run *ottoflowv1alpha1.WorkflowRun) error {
	serviceAccount := runnerServiceAccountName(run.Spec.Execution)
	if serviceAccount == "" {
		return nil
	}
	if v.inheritedFromWorkflow(ctx, run, serviceAccount) {
		return nil
	}
	return authorizeUse(ctx, v.Authorizer, run.Namespace, serviceAccount, fmt.Sprintf("WorkflowRun %q", run.Name))
}

// inheritedFromWorkflow reports whether this run is using the ServiceAccount
// its Workflow declares rather than choosing one.
//
// The scheduler copies spec.execution from the Workflow into each run it
// creates, and creates it as the controller, so reviewing the submitter would
// ask whether the controller may use the account and reject every cron run.
// The declaration was already authorized when the Workflow was admitted, and
// every run of that Workflow uses that account anyway, so there is nothing
// left to decide here.
//
// Same namespace only: the account is used in the run's namespace, and a
// Workflow elsewhere was authorized against its own.
func (v *WorkflowRunValidator) inheritedFromWorkflow(ctx context.Context, run *ottoflowv1alpha1.WorkflowRun, serviceAccount string) bool {
	if v.Client == nil {
		return false
	}
	if ns := run.Spec.WorkflowRef.Namespace; ns != "" && ns != run.Namespace {
		return false
	}

	var workflow ottoflowv1alpha1.Workflow
	key := client.ObjectKey{Namespace: run.Namespace, Name: run.Spec.WorkflowRef.Name}
	if err := v.Client.Get(ctx, key, &workflow); err != nil {
		return false
	}
	return runnerServiceAccountName(workflow.Spec.Execution) == serviceAccount
}

// authorizeWorkflowServiceAccount checks the Workflow author may run as the
// ServiceAccount the Workflow declares. This is where the account is chosen;
// runs created from the Workflow inherit it.
func (v *WorkflowValidator) authorizeWorkflowServiceAccount(ctx context.Context, workflow *ottoflowv1alpha1.Workflow) error {
	serviceAccount := runnerServiceAccountName(workflow.Spec.Execution)
	if serviceAccount == "" {
		return nil
	}
	return authorizeUse(ctx, v.Authorizer, workflow.Namespace, serviceAccount, fmt.Sprintf("Workflow %q", workflow.Name))
}

// authorizeUse asks whether the identity behind this admission request may run
// workloads as serviceAccount in namespace.
func authorizeUse(ctx context.Context, authorizer SubjectAccessReviewer, namespace, serviceAccount, subject string) error {
	if authorizer == nil {
		return fmt.Errorf("%s sets spec.execution.job.serviceAccountName but the validator cannot authorize it", subject)
	}

	request, err := admission.RequestFromContext(ctx)
	if err != nil {
		return fmt.Errorf("%s sets spec.execution.job.serviceAccountName but the request carries no user to authorize: %w", subject, err)
	}

	extra := map[string]authorizationv1.ExtraValue{}
	for key, values := range request.UserInfo.Extra {
		extra[key] = authorizationv1.ExtraValue(values)
	}

	review, err := authorizer.Create(ctx, &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{
			User:   request.UserInfo.Username,
			UID:    request.UserInfo.UID,
			Groups: request.UserInfo.Groups,
			Extra:  extra,
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace: namespace,
				Verb:      UseVerb,
				Resource:  "serviceaccounts",
				Name:      serviceAccount,
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("%s: checking whether %q may use serviceaccount %q: %w",
			subject, request.UserInfo.Username, serviceAccount, err)
	}
	if !review.Status.Allowed {
		return fmt.Errorf("%s: %q may not use serviceaccount %q in namespace %q%s",
			subject, request.UserInfo.Username, serviceAccount, namespace, reviewReason(review))
	}
	return nil
}

func runnerServiceAccountName(execution *ottoflowv1alpha1.WorkflowRunExecutionSpec) string {
	if execution == nil || execution.Job == nil {
		return ""
	}
	return execution.Job.ServiceAccountName
}

func reviewReason(review *authorizationv1.SubjectAccessReview) string {
	if review.Status.Reason == "" {
		return ""
	}
	return ": " + review.Status.Reason
}
