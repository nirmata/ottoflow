/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"context"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

const (
	runnerBindingSweepPeriod = 10 * time.Minute
	runnerBindingSweepGap    = 100 * time.Millisecond
)

// Never returns an error, so a failed pass cannot stop the manager.
func (r *WorkflowRunReconciler) runRunnerBindingSweep(ctx context.Context, period time.Duration) error {
	ticker := time.NewTicker(period)
	defer ticker.Stop()
	for {
		r.sweepRunnerBindings(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (r *WorkflowRunReconciler) sweepRunnerBindings(ctx context.Context) {
	logger := log.FromContext(ctx).WithName("runner-binding-sweep")
	var list rbacv1.ClusterRoleBindingList
	if err := r.List(ctx, &list, client.MatchingLabels{ownerLabel: ottoflowPartOf}); err != nil {
		logger.Error(err, "listing ClusterRoleBindings")
		return
	}
	seen := map[types.NamespacedName]bool{}
	var subjects []types.NamespacedName
	for i := range list.Items {
		if s, ok := perRunnerBindingSubject(&list.Items[i]); ok && !seen[s] {
			seen[s] = true
			subjects = append(subjects, s)
		}
	}
	for i, s := range subjects {
		if i > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(runnerBindingSweepGap):
			}
		}
		r.sweepRunnerSubject(ctx, s)
	}
}

// The chart's own bindings carry a component label; the controller's never do.
func perRunnerBindingSubject(crb *rbacv1.ClusterRoleBinding) (types.NamespacedName, bool) {
	if _, hasComponent := crb.Labels["app.kubernetes.io/component"]; hasComponent || len(crb.Subjects) != 1 {
		return types.NamespacedName{}, false
	}
	s := crb.Subjects[0]
	if s.Kind != rbacv1.ServiceAccountKind ||
		(crb.Name != workflowRunnerRoleBindingName(s.Namespace, s.Name) &&
			crb.Name != agentExecutorCallerRoleBindingName(s.Namespace, s.Name)) {
		return types.NamespacedName{}, false
	}
	return types.NamespacedName{Namespace: s.Namespace, Name: s.Name}, true
}

func (r *WorkflowRunReconciler) sweepRunnerSubject(ctx context.Context, s types.NamespacedName) {
	logger := log.FromContext(ctx).WithName("runner-binding-sweep").WithValues("namespace", s.Namespace, "serviceAccount", s.Name)
	sa := &corev1.ServiceAccount{}
	err := r.directReader().Get(ctx, s, sa)
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		logger.Error(err, "reading ServiceAccount; keeping its runner ClusterRoleBindings")
		return
	case sa.Labels[ownerLabel] == ottoflowPartOf:
		return
	default:
		inUse, err := r.runnerServiceAccountInUse(ctx, s)
		if err != nil {
			logger.Error(err, "checking for runs using the ServiceAccount; keeping its runner ClusterRoleBindings")
			return
		}
		if inUse {
			return
		}
		// Re-read just before deleting: an operator or a new run's controller may have labelled or
		// created the account during the in-use check. Residual window: a label landing between this
		// read and the delete is not seen.
		if err := r.directReader().Get(ctx, s, sa); err == nil && sa.Labels[ownerLabel] == ottoflowPartOf {
			return
		} else if err != nil && !apierrors.IsNotFound(err) {
			logger.Error(err, "re-reading ServiceAccount; keeping its runner ClusterRoleBindings")
			return
		}
	}
	r.deleteRunnerBindingsFor(ctx, s.Namespace, s.Name)
}

func (r *WorkflowRunReconciler) runnerServiceAccountInUse(ctx context.Context, s types.NamespacedName) (bool, error) {
	// Uncached reads, so informer lag cannot hide a live run.
	reader := r.directReader()
	var jobs batchv1.JobList
	if err := reader.List(ctx, &jobs, client.InNamespace(s.Namespace)); err != nil {
		return false, err
	}
	for i := range jobs.Items {
		job := &jobs.Items[i]
		if jobConditionTrue(job, batchv1.JobComplete) || jobConditionTrue(job, batchv1.JobFailed) {
			continue
		}
		if live, err := liveRunnerJobRunsAs(ctx, reader, s.Name, job); err != nil || live {
			return live, err
		}
	}
	var pods corev1.PodList
	if err := reader.List(ctx, &pods, client.InNamespace(s.Namespace), client.MatchingFields{"spec.serviceAccountName": s.Name}); err != nil {
		return false, err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		var job batchv1.Job
		found, err := controllerOwnerOf(ctx, reader, pod, "Job", &job)
		if err != nil {
			return false, err
		}
		if !found {
			continue
		}
		if live, err := liveRunnerJobRunsAs(ctx, reader, s.Name, &job); err != nil || live {
			return live, err
		}
	}
	return false, nil
}

// Keeps the bindings only for a live run's own runner Job whose pod template runs as sa. The
// template account is fixed at Job creation, so this does not depend on the current runner
// ServiceAccount setting or on the run's editable spec.
func liveRunnerJobRunsAs(ctx context.Context, reader client.Reader, sa string, job *batchv1.Job) (bool, error) {
	if job.Spec.Template.Spec.ServiceAccountName != sa {
		return false, nil
	}
	var wr ottoflowv1alpha1.WorkflowRun
	found, err := controllerOwnerOf(ctx, reader, job, "WorkflowRun", &wr)
	return found && !wr.Status.Phase.IsTerminal() && job.Name == workflowRunnerJobName(wr.Name), err
}

func controllerOwnerOf(ctx context.Context, reader client.Reader, controllee metav1.Object, kind string, owner client.Object) (bool, error) {
	ref := metav1.GetControllerOfNoCopy(controllee)
	if ref == nil || ref.Kind != kind {
		return false, nil
	}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: controllee.GetNamespace(), Name: ref.Name}, owner); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	return owner.GetUID() == ref.UID, nil
}
