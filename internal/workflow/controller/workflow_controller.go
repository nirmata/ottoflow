/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/logging"
	executor "github.com/nirmata/ottoflow/internal/workflow/executor"
)

// +kubebuilder:rbac:groups="",resources=secrets,verbs=get

// WorkflowReconciler reconciles a Workflow object
type WorkflowReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	TriggerManager *TriggerManager
	CELCache       *executor.CELCompilationCache
	// WebhookServer is optional. When set, its per-workflow in-memory state
	// (rate limiter + dedup entry) is cleaned up on Workflow deletion.
	// Nil when the reconciler is created without webhook support (SetupWithManager default).
	WebhookServer *WebhookServer
}

func (r *WorkflowReconciler) forgetWebhookState(workflowKey string) {
	r.TriggerManager.CleanupWebhookDedup(workflowKey)
	if r.WebhookServer != nil {
		r.WebhookServer.RemoveLimiter(workflowKey)
	}
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// It manages workflow triggers (cron and event) by registering/unregistering them.
func (r *WorkflowReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the Workflow instance
	workflow := &ottoflowv1alpha1.Workflow{}
	if err := r.Get(ctx, req.NamespacedName, workflow); err != nil {
		if apierrors.IsNotFound(err) && r.TriggerManager != nil {
			r.TriggerManager.pruneTriggers(req.String(), nil)
			r.forgetWebhookState(req.String())
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Check if workflow is being deleted
	if !workflow.DeletionTimestamp.IsZero() {
		if r.TriggerManager != nil {
			if err := r.TriggerManager.UnregisterWorkflow(ctx, workflow); err != nil {
				logger.Error(err, "failed to unregister triggers", logging.KeyWorkflow, req.Name, logging.KeyNamespace, req.Namespace)
				return ctrl.Result{}, err
			}
			// Clean up webhook dedup state and rate limiter for this workflow.
			r.forgetWebhookState(req.String())
		}
		return ctrl.Result{}, nil
	}

	// Compile all CEL expressions to surface errors early; programs are discarded
	if r.CELCache != nil {
		if errs := r.CELCache.CompileWorkflow(workflow); len(errs) > 0 {
			for _, e := range errs {
				logger.Error(e, "CEL compilation error", logging.KeyWorkflow, req.Name, logging.KeyNamespace, req.Namespace)
			}
		}
	}

	// Register triggers for the workflow
	if r.TriggerManager != nil {
		if err := r.TriggerManager.RegisterWorkflow(ctx, workflow); err != nil {
			logger.Error(err, "failed to register triggers", logging.KeyWorkflow, req.Name, logging.KeyNamespace, req.Namespace)
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *WorkflowReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.TriggerManager == nil {
		r.TriggerManager = NewTriggerManager(mgr.GetClient(), mgr.GetScheme(), nil)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&ottoflowv1alpha1.Workflow{}).
		Complete(r)
}
