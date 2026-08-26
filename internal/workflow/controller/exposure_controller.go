/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/logging"
)

const (
	// a2aExposureFinalizer guards teardown of the kagent Agent when the Workflow is
	// deleted or spec.expose.kagent is cleared.
	a2aExposureFinalizer = "ottoflow.nirmata.io/a2a-exposure"

	// serveA2ARoleName is the shared per-namespace RoleBinding name that binds a serve-a2a
	// BYO pod's ServiceAccount to the Helm-provisioned shared serve-a2a ClusterRole.
	serveA2ARoleName = "ottoflow-serve-a2a"

	// exposureRequeue is the periodic resync for exposed Workflows. There is no Watch on
	// the kagent Agent type, so we requeue to reconcile drift.
	exposureRequeue = 10 * time.Minute
	// crdMissingRequeue is used when the kagent CRD is not installed yet.
	crdMissingRequeue = 2 * time.Minute
)

// kagent Agent identifiers. We use unstructured to avoid importing kagent's Go types.
const (
	kagentAPIVersion = "kagent.dev/v1alpha2"
	kagentKind       = "Agent"

	labelWorkflowNamespace = "ottoflow.nirmata.io/workflow-namespace"
	labelWorkflowName      = "ottoflow.nirmata.io/workflow-name"
	labelWorkflowUID       = "ottoflow.nirmata.io/workflow-uid"
)

func kagentAgentGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: "kagent.dev", Version: "v1alpha2", Kind: kagentKind}
}

// +kubebuilder:rbac:groups=kagent.dev,resources=agents,verbs=get;list;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch;create;update;delete

// WorkflowExposureReconciler publishes Workflows that opt in via spec.expose.kagent as kagent
// BYO Agents (pointing at the serve-a2a image), plus the ServiceAccount and RoleBinding the
// BYO pod needs. The RoleBinding references a Helm-provisioned shared ClusterRole
// (ServeA2AClusterRole); the controller never mints those permissions itself. Clearing the
// field or deleting the Workflow removes the Agent.
type WorkflowExposureReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// ServeA2AImage is the serve-a2a container image the BYO Agent runs. When empty, a2a
	// exposure is skipped (logged) rather than creating a broken Agent.
	ServeA2AImage string
	// ServeA2AServiceAccount is the ServiceAccount name the BYO pod runs as (created per namespace).
	ServeA2AServiceAccount string
	// ServeA2AClusterRole is the shared, Helm-provisioned ClusterRole the per-namespace
	// RoleBinding references. The controller only BINDS it (never creates a Role), so it does
	// not need to hold+grant those verbs itself and passes Kubernetes RBAC escalation checks.
	ServeA2AClusterRole string
}

// Reconcile ensures a kagent Agent exists for each Workflow with spec.expose.kagent set, and
// removes it when the field is cleared or the Workflow is deleted.
func (r *WorkflowExposureReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	wf := &ottoflowv1alpha1.Workflow{}
	if err := r.Get(ctx, req.NamespacedName, wf); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	exposeKagent := wf.Spec.Expose.IsKagentEnabled()
	beingDeleted := !wf.DeletionTimestamp.IsZero()

	// A2A maps a single free-text prompt to the workflow's FIRST input (see serve-a2a). A
	// required input the prompt can't fill (any required, default-less input after the first)
	// makes the workflow unexposable: it would accept the A2A call and then fail in the executor.
	// Refuse to expose it — and tear down any Agent published before the schema became incompatible.
	exposable := exposeKagent && inputsExposable(wf)
	if exposeKagent && !exposable {
		logger.Info("workflow opts into a2a exposure but has a required input the A2A prompt cannot fill; not exposing",
			logging.KeyWorkflow, req.Name, logging.KeyNamespace, req.Namespace)
	}

	// Teardown: not opted in, incompatible input schema, or Workflow being deleted. Delete the
	// Agent, keep the shared SA/RoleBinding (namespace fixtures shared by other exposed Workflows).
	if !exposable || beingDeleted {
		if controllerutil.ContainsFinalizer(wf, a2aExposureFinalizer) {
			if err := r.deleteAgent(ctx, wf); err != nil {
				logger.Error(err, "failed to delete kagent Agent", logging.KeyWorkflow, req.Name, logging.KeyNamespace, req.Namespace)
				return ctrl.Result{}, err
			}
			controllerutil.RemoveFinalizer(wf, a2aExposureFinalizer)
			if err := r.Update(ctx, wf); err != nil {
				return ctrl.Result{}, client.IgnoreNotFound(err)
			}
		}
		return ctrl.Result{}, nil
	}

	// Setup path. Without an image we cannot build a working Agent; log and wait for the
	// next spec change / resync rather than creating a broken Agent.
	if r.ServeA2AImage == "" {
		logger.Info("serve-a2a image not configured; skipping a2a exposure",
			logging.KeyWorkflow, req.Name, logging.KeyNamespace, req.Namespace)
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(wf, a2aExposureFinalizer) {
		controllerutil.AddFinalizer(wf, a2aExposureFinalizer)
		if err := r.Update(ctx, wf); err != nil {
			return ctrl.Result{}, err
		}
	}

	if err := r.ensureRBAC(ctx, wf.Namespace); err != nil {
		logger.Error(err, "failed to ensure serve-a2a RBAC", logging.KeyNamespace, req.Namespace)
		return ctrl.Result{}, err
	}

	if err := r.upsertAgent(ctx, wf); err != nil {
		// kagent CRD not installed: don't error-loop, requeue and re-check later.
		if apimeta.IsNoMatchError(err) {
			logger.Info("kagent Agent CRD not installed; requeueing",
				logging.KeyWorkflow, req.Name, logging.KeyNamespace, req.Namespace)
			return ctrl.Result{RequeueAfter: crdMissingRequeue}, nil
		}
		logger.Error(err, "failed to upsert kagent Agent", logging.KeyWorkflow, req.Name, logging.KeyNamespace, req.Namespace)
		return ctrl.Result{}, err
	}

	// No Watch on the Agent type, so requeue periodically to reconcile drift.
	return ctrl.Result{RequeueAfter: exposureRequeue}, nil
}

// ensureRBAC creates (never modifies/deletes) the ServiceAccount and RoleBinding the serve-a2a
// BYO pod needs, in the Workflow's namespace.
//
// The RoleBinding references the shared, Helm-provisioned ClusterRole (r.ServeA2AClusterRole)
// rather than a Role the controller mints itself. Binding an existing ClusterRole means the
// controller does not need to hold+grant workflowruns:create etc.; Kubernetes' RBAC
// escalation-prevention check is satisfied by the controller's "bind" grant on that
// ClusterRole (see the Helm chart's -role:core), so this works under locked-down permissions,
// not just an admin dev cluster.
func (r *WorkflowExposureReconciler) ensureRBAC(ctx context.Context, ns string) error {
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: r.ServeA2AServiceAccount, Namespace: ns},
	}
	if err := r.createIfAbsent(ctx, sa); err != nil {
		return err
	}

	desired := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: serveA2ARoleName, Namespace: ns},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     r.ServeA2AClusterRole,
		},
		Subjects: []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      r.ServeA2AServiceAccount,
			Namespace: ns,
		}},
	}
	return r.reconcileRoleBinding(ctx, desired)
}

// reconcileRoleBinding makes the serve-a2a RoleBinding match desired. Unlike the ServiceAccount,
// the binding is reconciled rather than created-if-absent: if an operator changes
// serviceAccountName or clusterRole, a create-if-absent binding would keep pointing at the old
// ServiceAccount/ClusterRole while newly reconciled BYO Agents run as the new ServiceAccount and
// silently lose Workflow access. roleRef is immutable, so a drifted roleRef is fixed by
// delete+recreate; a drifted subject list is fixed with an in-place update.
func (r *WorkflowExposureReconciler) reconcileRoleBinding(ctx context.Context, desired *rbacv1.RoleBinding) error {
	var existing rbacv1.RoleBinding
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), &existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	if existing.RoleRef != desired.RoleRef {
		// roleRef is immutable; the only way to repoint it is to recreate the binding.
		if err := r.Delete(ctx, &existing); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		return r.Create(ctx, desired)
	}
	if !equality.Semantic.DeepEqual(existing.Subjects, desired.Subjects) {
		existing.Subjects = desired.Subjects
		return r.Update(ctx, &existing)
	}
	return nil
}

// createIfAbsent creates obj only when it does not already exist. It never updates or
// deletes, so shared namespace fixtures are safe against per-Workflow reconciles.
func (r *WorkflowExposureReconciler) createIfAbsent(ctx context.Context, obj client.Object) error {
	existing, ok := obj.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("object %T is not a client.Object", obj)
	}
	err := r.Get(ctx, client.ObjectKeyFromObject(obj), existing)
	if err == nil {
		return nil // already exists; leave it untouched
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	return r.Create(ctx, obj)
}

// upsertAgent creates or updates the kagent Agent for the Workflow, then prunes any
// stale-named Agents this controller previously created for the same Workflow (e.g. a
// leftover flo-<hash> after the Workflow name became eligible to be used directly).
func (r *WorkflowExposureReconciler) upsertAgent(ctx context.Context, wf *ottoflowv1alpha1.Workflow) error {
	desired := buildAgentObject(wf, r.ServeA2AImage, r.ServeA2AServiceAccount)

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(kagentAgentGVK())
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), existing)
	switch {
	case apierrors.IsNotFound(err):
		if err := r.Create(ctx, desired); err != nil {
			return err
		}
	case err != nil:
		return err // includes NoMatch (CRD absent), handled by the caller
	default:
		// Do not hijack an Agent we do not manage: an unrelated kagent Agent (or one belonging to
		// a different Workflow) may already hold this computed name. Overwriting its spec+labels
		// would take it over. Only reconcile when the live object carries our management labels for
		// THIS Workflow; otherwise surface the conflict rather than silently owning it.
		if !managedBy(existing, wf) {
			return fmt.Errorf(
				"kagent Agent %s/%s already exists and is not managed by OttoFlow for workflow %q (uid %s); refusing to overwrite it",
				existing.GetNamespace(), existing.GetName(), wf.Name, wf.UID)
		}
		// Preserve the live object's metadata (resourceVersion etc.); only reconcile spec + labels.
		existing.Object["spec"] = desired.Object["spec"]
		existing.SetLabels(desired.GetLabels())
		if err := r.Update(ctx, existing); err != nil {
			return err
		}
	}

	return r.pruneStaleAgents(ctx, wf, desired.GetName())
}

// pruneStaleAgents deletes any kagent Agents this controller manages for wf whose name differs
// from keepName (the currently-computed name). It only touches Agents that carry all three
// managed labels matching this Workflow, so it never deletes Agents belonging to other
// Workflows or unmanaged Agents that merely share the workflow-name label.
func (r *WorkflowExposureReconciler) pruneStaleAgents(ctx context.Context, wf *ottoflowv1alpha1.Workflow, keepName string) error {
	logger := log.FromContext(ctx)

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{Group: "kagent.dev", Version: "v1alpha2", Kind: kagentKind + "List"})
	if err := r.List(ctx, list,
		client.InNamespace(wf.Namespace),
		client.MatchingLabels{labelWorkflowName: wf.Name},
	); err != nil {
		return err
	}

	for i := range list.Items {
		item := &list.Items[i]
		if item.GetName() == keepName {
			continue
		}
		labels := item.GetLabels()
		// Guard: only delete Agents we own for THIS Workflow (namespace + uid must match too).
		if labels[labelWorkflowNamespace] != wf.Namespace || labels[labelWorkflowUID] != string(wf.UID) {
			continue
		}
		if err := r.Delete(ctx, item); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		logger.Info("deleted stale kagent Agent after rename",
			logging.KeyWorkflow, wf.Name, logging.KeyNamespace, wf.Namespace, "staleAgent", item.GetName())
	}
	return nil
}

// deleteAgent deletes the Workflow's kagent Agent, tolerating both a missing object and a
// missing CRD (nothing to clean up in either case).
func (r *WorkflowExposureReconciler) deleteAgent(ctx context.Context, wf *ottoflowv1alpha1.Workflow) error {
	ag := &unstructured.Unstructured{}
	ag.SetGroupVersionKind(kagentAgentGVK())
	ag.SetNamespace(wf.Namespace)
	ag.SetName(agentName(wf))
	err := r.Delete(ctx, ag)
	if apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err) {
		return nil
	}
	return err
}

// buildAgentObject builds the desired kagent BYO Agent for a Workflow. Pure function so it
// can be unit-tested without a cluster.
func buildAgentObject(wf *ottoflowv1alpha1.Workflow, image, serviceAccount string) *unstructured.Unstructured {
	name := agentName(wf)

	description := fmt.Sprintf("Runs the %s OttoFlow workflow", wf.Name)
	if k := wf.Spec.Expose.Kagent; k != nil && k.Description != "" {
		description = k.Description
	}

	// ponytail: only description is propagated to the kagent Agent in slice-1. displayName,
	// examples and tags are captured on the API for a later slice that enriches the A2A card.
	publicURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:8080/", name, wf.Namespace)

	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": kagentAPIVersion,
		"kind":       kagentKind,
		"metadata": map[string]any{
			"name":      name,
			"namespace": wf.Namespace,
			"labels": map[string]any{
				labelWorkflowNamespace: wf.Namespace,
				labelWorkflowName:      wf.Name,
				labelWorkflowUID:       string(wf.UID),
			},
		},
		"spec": map[string]any{
			"type":        "BYO",
			"description": description,
			"byo": map[string]any{
				"deployment": map[string]any{
					"image":              image,
					"serviceAccountName": serviceAccount,
					"env": []any{
						map[string]any{"name": "WORKFLOW_NAME", "value": wf.Name},
						map[string]any{"name": "WORKFLOW_NAMESPACE", "value": wf.Namespace},
						map[string]any{"name": "A2A_PUBLIC_URL", "value": publicURL},
					},
				},
			},
		},
	}}
}

// inputsExposable reports whether the workflow's input schema can be satisfied by A2A's
// single-prompt→first-input mapping. The prompt fills only the FIRST input, so any required
// input without a default at a later index can never be supplied over A2A. The first input may
// be required (the prompt fills it) and any later input is fine as long as it is optional or
// has a default.
func inputsExposable(wf *ottoflowv1alpha1.Workflow) bool {
	for i, in := range wf.Spec.Inputs {
		if i == 0 {
			continue
		}
		if in.Required && in.Default == "" {
			return false
		}
	}
	return true
}

// managedBy reports whether an existing Agent carries this controller's management labels for the
// given Workflow (namespace + name + uid all match). Used before overwriting so a reconcile never
// hijacks an Agent it does not own that merely shares the computed name.
func managedBy(obj *unstructured.Unstructured, wf *ottoflowv1alpha1.Workflow) bool {
	labels := obj.GetLabels()
	return labels[labelWorkflowNamespace] == wf.Namespace &&
		labels[labelWorkflowName] == wf.Name &&
		labels[labelWorkflowUID] == string(wf.UID)
}

// agentName returns a DNS-1123-safe (<=63 char) name for a Workflow's Agent. kagent's UI
// shows the Agent's metadata.name as the card title, so we use the Workflow name directly
// when it is already a valid DNS-1123 label (which is nearly always, since Workflow names are
// themselves Kubernetes object names). Otherwise we fall back to a deterministic flo-<hash>
// derived from namespace+name (stable across reconciles, unique per Workflow).
func agentName(wf *ottoflowv1alpha1.Workflow) string {
	if len(validation.IsDNS1123Label(wf.Name)) == 0 {
		return wf.Name
	}
	return hashedAgentName(wf)
}

// hashedAgentName is the DNS-1123-safe fallback name used when the Workflow name itself is not
// a valid label.
func hashedAgentName(wf *ottoflowv1alpha1.Workflow) string {
	sum := sha256.Sum256([]byte(wf.Namespace + "/" + wf.Name))
	return "flo-" + hex.EncodeToString(sum[:])[:12]
}

// SetupWithManager registers the reconciler. It MUST use a non-default name: the primary
// WorkflowReconciler already owns the default-named For(&Workflow{}) controller, and
// controller-runtime rejects duplicate controller names.
func (r *WorkflowExposureReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("workflowexposure").
		For(&ottoflowv1alpha1.Workflow{}).
		Complete(r)
}
