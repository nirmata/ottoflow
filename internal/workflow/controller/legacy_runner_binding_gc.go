/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"context"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

var _ manager.LeaderElectionRunnable = (*LegacyRunnerBindingGC)(nil)

// LegacyRunnerBindingGC deletes, once at startup, this release's pre-upgrade runner
// ClusterRoleBindings: ottoflow-runner-<ns>-<sa> bindings to the controller's own role
// (legacyRole) whose subject is exactly this release's controller SA (legacySA) and not the
// configured runner SA, so another release's binding to a shared role survives.
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
