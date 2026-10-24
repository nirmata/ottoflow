/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

var _ manager.LeaderElectionRunnable = (*LegacyRunnerBindingGC)(nil)

// LegacyRunnerBindingGC deletes, once at startup, this release's pre-upgrade runner
// ClusterRoleBindings: ottoflow-runner-<ns>-<sa> bindings to the controller's own role
// (legacyRole) whose subject is exactly this release's controller SA (legacySA) and not the
// configured runner SA, so another release's binding with a different subject survives.
// One with the same name and role cannot be told apart and is kept only while a live run uses it.
// Empty legacyRole or legacySA disables it.
type LegacyRunnerBindingGC struct {
	client      client.Client
	legacyRole  string
	legacySA    string
	runnerSAcfg string
}

func NewLegacyRunnerBindingGC(c client.Client, legacyRole, legacySA, runnerServiceAccount string) *LegacyRunnerBindingGC {
	return &LegacyRunnerBindingGC{client: c, legacyRole: legacyRole, legacySA: legacySA, runnerSAcfg: runnerServiceAccount}
}

// NeedLeaderElection makes only the leader delete.
func (g *LegacyRunnerBindingGC) NeedLeaderElection() bool { return true }

// Start never returns an error: a failed cleanup must not stop the manager.
func (g *LegacyRunnerBindingGC) Start(ctx context.Context) error {
	if g.legacyRole == "" || g.legacySA == "" {
		return nil
	}
	logger := log.FromContext(ctx).WithName("legacy-runner-binding-gc")
	var list rbacv1.ClusterRoleBindingList
	if err := g.client.List(ctx, &list, client.MatchingLabels{"app.kubernetes.io/part-of": ottoflowPartOf}); err != nil {
		logger.Error(err, "listing ClusterRoleBindings")
		return nil
	}
	for i := range list.Items {
		crb := &list.Items[i]
		if !g.isLegacy(crb) {
			continue
		}
		pins, err := g.pinningPods(ctx, crb)
		if err != nil {
			logger.Error(err, "checking for runs still using legacy runner ClusterRoleBinding; keeping it", "name", crb.Name)
			continue
		}
		if len(pins) > 0 {
			logger.Info("keeping legacy runner ClusterRoleBinding: live runs still use its ServiceAccount", "name", crb.Name, "pods", pins)
			continue
		}
		pre := client.Preconditions{UID: &crb.UID, ResourceVersion: &crb.ResourceVersion}
		if err := g.client.Delete(ctx, crb, pre); err != nil {
			if !apierrors.IsNotFound(err) {
				logger.Error(err, "deleting legacy runner ClusterRoleBinding", "name", crb.Name)
			}
			continue
		}
		logger.Info("deleted legacy runner ClusterRoleBinding", "name", crb.Name, "subject", crb.Subjects[0].Name)
	}
	return nil
}

func (g *LegacyRunnerBindingGC) isLegacy(crb *rbacv1.ClusterRoleBinding) bool {
	if crb.Labels["app.kubernetes.io/component"] == "rbac" || len(crb.Subjects) != 1 || crb.RoleRef.Name != g.legacyRole {
		return false
	}
	s := crb.Subjects[0]
	return s.Kind == rbacv1.ServiceAccountKind &&
		s.Name == g.legacySA &&
		s.Name != g.runnerSAcfg &&
		crb.Name == workflowRunnerRoleBindingName(s.Namespace, s.Name)
}

// pinningPods returns "pod(workflowrun)" for each non-terminal pod in the subject's namespace that
// runs as the subject and is controlled by a Job controlled by a live, non-terminal WorkflowRun.
// Any lookup error other than NotFound is returned so the caller keeps the binding.
func (g *LegacyRunnerBindingGC) pinningPods(ctx context.Context, crb *rbacv1.ClusterRoleBinding) ([]string, error) {
	s := crb.Subjects[0]
	var pods corev1.PodList
	if err := g.client.List(ctx, &pods, client.InNamespace(s.Namespace)); err != nil {
		return nil, err
	}
	var pins []string
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Spec.ServiceAccountName != s.Name || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		var job batchv1.Job
		found, err := g.controllerOwner(ctx, pod, "Job", &job)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		var wr ottoflowv1alpha1.WorkflowRun
		found, err = g.controllerOwner(ctx, &job, "WorkflowRun", &wr)
		if err != nil {
			return nil, err
		}
		if found && !runIsTerminal(&wr) {
			pins = append(pins, fmt.Sprintf("%s(%s)", pod.Name, wr.Name))
		}
	}
	return pins, nil
}

// controllerOwner fetches controllee's controlling owner of kind into owner; found is false when the
// ref is absent, of another kind or lacks blockOwnerDeletion, or the owner is gone or has another UID.
func (g *LegacyRunnerBindingGC) controllerOwner(ctx context.Context, controllee metav1.Object, kind string, owner client.Object) (bool, error) {
	ref := metav1.GetControllerOfNoCopy(controllee)
	if ref == nil || ref.Kind != kind || !ptr.Deref(ref.BlockOwnerDeletion, false) {
		return false, nil
	}
	if err := g.client.Get(ctx, client.ObjectKey{Namespace: controllee.GetNamespace(), Name: ref.Name}, owner); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	return owner.GetUID() == ref.UID, nil
}
