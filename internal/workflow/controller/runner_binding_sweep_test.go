/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

const sweepNS = "sweep-ns"

func sweepBindings(sa string) []client.Object {
	return []client.Object{
		buildRunnerClusterRoleBinding(workflowRunnerRoleBindingName(sweepNS, sa), "runner-role", sa, sweepNS),
		buildRunnerClusterRoleBinding(agentExecutorCallerRoleBindingName(sweepNS, sa), "caller-role", sa, sweepNS),
	}
}

func sweepUnlabelledAccount(name string) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: sweepNS}}
}

func sweepRun(phase ottoflowv1alpha1.WorkflowRunPhase) *ottoflowv1alpha1.WorkflowRun {
	return &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run", Namespace: sweepNS, UID: "run-uid"},
		Status:     ottoflowv1alpha1.WorkflowRunStatus{Phase: phase},
	}
}

func sweepRunAs(phase ottoflowv1alpha1.WorkflowRunPhase, sa string) *ottoflowv1alpha1.WorkflowRun {
	run := sweepRun(phase)
	run.Spec.Execution = &ottoflowv1alpha1.WorkflowRunExecutionSpec{Job: &ottoflowv1alpha1.WorkflowRunJobSpec{ServiceAccountName: sa}}
	return run
}

func sweepJob(finished bool) *batchv1.Job {
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "run-runner", Namespace: sweepNS, UID: "job-uid", OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(sweepRun(""), ottoflowv1alpha1.GroupVersion.WithKind("WorkflowRun"))}}}
	job.Spec.Template.Spec.ServiceAccountName = "idle"
	if finished {
		job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	}
	return job
}

func sweepPod(sa string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "run-runner-pod", Namespace: sweepNS, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(sweepJob(false), batchv1.SchemeGroupVersion.WithKind("Job"))}},
		Spec:       corev1.PodSpec{ServiceAccountName: sa},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func nonBlockingOwner(obj client.Object) client.Object {
	obj.GetOwnerReferences()[0].BlockOwnerDeletion = nil
	return obj
}

func newSweepReconciler(readerFuncs interceptor.Funcs, objs ...client.Object) (*WorkflowRunReconciler, client.Client) {
	base := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(objs...).
		WithIndex(&corev1.Pod{}, "spec.serviceAccountName", func(o client.Object) []string {
			return []string{o.(*corev1.Pod).Spec.ServiceAccountName}
		}).Build()
	return &WorkflowRunReconciler{Client: base, Scheme: unitTestScheme, APIReader: interceptor.NewClient(base, readerFuncs), RunnerConfig: RunnerConfig{RunnerServiceAccount: "idle"}}, base
}

func bindingNames(sa string) []string {
	return []string{workflowRunnerRoleBindingName(sweepNS, sa), agentExecutorCallerRoleBindingName(sweepNS, sa)}
}

func accountReadCounter(reads map[string]int) interceptor.Funcs {
	return interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if _, ok := obj.(*corev1.ServiceAccount); ok {
			reads[key.Name]++
		}
		return c.Get(ctx, key, obj, opts...)
	}}
}

func TestRunnerBindingSweep_RemovesBindingsOfMissingServiceAccount(t *testing.T) {
	r, c := newSweepReconciler(interceptor.Funcs{}, sweepBindings("gone")...)
	r.sweepRunnerBindings(context.Background())
	requireBindings(t, c, false, bindingNames("gone")...)
}

func TestRunnerBindingSweep_RunsEveryPeriod(t *testing.T) {
	r, c := newSweepReconciler(interceptor.Funcs{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.runRunnerBindingSweep(ctx, 50*time.Millisecond) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	time.Sleep(200 * time.Millisecond) // let the startup pass run first
	for _, b := range sweepBindings("gone") {
		if err := c.Create(ctx, b); err != nil {
			t.Fatalf("seed binding: %v", err)
		}
	}
	gomega.NewWithT(t).Eventually(func() bool { return bindingsExist(c, sweepNS, "gone") }, 10*time.Second, 25*time.Millisecond).
		Should(gomega.BeFalse(), "bindings seeded after startup were not removed by a later sweep")
}

func TestRunnerBindingSweep_KeepsChartBindingWithCollidingName(t *testing.T) {
	b := buildRunnerClusterRoleBinding(workflowRunnerRoleBindingName(sweepNS, "chart-sa"), "chart-role", "chart-sa", sweepNS)
	b.Labels["app.kubernetes.io/component"] = "controller"
	r, c := newSweepReconciler(interceptor.Funcs{}, b)
	r.sweepRunnerBindings(context.Background())
	requireBindings(t, c, true, b.Name)
}

func TestRunnerBindingSweep_NeverReadsAccountsOfBindingsItDoesNotOwn(t *testing.T) {
	noPartOf := buildRunnerClusterRoleBinding(workflowRunnerRoleBindingName(sweepNS, "nolabel"), "runner-role", "nolabel", sweepNS)
	noPartOf.Labels = nil
	multi := buildRunnerClusterRoleBinding(workflowRunnerRoleBindingName(sweepNS, "multi"), "runner-role", "multi", sweepNS)
	multi.Subjects = append(multi.Subjects, rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Name: "second", Namespace: sweepNS})
	otherSubject := buildRunnerClusterRoleBinding(workflowRunnerRoleBindingName(sweepNS, "named"), "runner-role", "other", sweepNS)
	userSubject := buildRunnerClusterRoleBinding(workflowRunnerRoleBindingName(sweepNS, "user"), "runner-role", "user", sweepNS)
	userSubject.Subjects[0].Kind = rbacv1.UserKind
	owned := sweepBindings("control")

	reads := map[string]int{}
	r, c := newSweepReconciler(accountReadCounter(reads), append(owned, noPartOf, multi, otherSubject, userSubject)...)
	r.sweepRunnerBindings(context.Background())

	requireBindings(t, c, false, bindingNames("control")...)
	requireBindings(t, c, true, noPartOf.Name, multi.Name, otherSubject.Name, userSubject.Name)
	for _, sa := range []string{"nolabel", "multi", "second", "named", "other", "user"} {
		if reads[sa] != 0 {
			t.Errorf("ServiceAccount %s was read %d times, want 0", sa, reads[sa])
		}
	}
}

func TestRunnerBindingSweep_KeepsBindingsWhenAccountReadFails(t *testing.T) {
	for name, readErr := range map[string]error{
		"forbidden": apierrors.NewForbidden(schema.GroupResource{Resource: "serviceaccounts"}, "gone", errors.New("denied")),
		"timeout":   apierrors.NewTimeoutError("request timed out", 1),
	} {
		t.Run(name, func(t *testing.T) {
			r, c := newSweepReconciler(failGetOf[*corev1.ServiceAccount](readErr), sweepBindings("gone")...)
			r.sweepRunnerBindings(context.Background())
			requireBindings(t, c, true, bindingNames("gone")...)
		})
	}
}

func TestRunnerBindingSweep_KeepsLabelledAccountBindingsAndReadsItOnce(t *testing.T) {
	reads := map[string]int{}
	r, c := newSweepReconciler(accountReadCounter(reads), append(sweepBindings("live"), runnerServiceAccount("live", sweepNS))...)
	r.sweepRunnerBindings(context.Background())
	requireBindings(t, c, true, bindingNames("live")...)
	if reads["live"] != 1 {
		t.Errorf("ServiceAccount read %d times for its two bindings, want 1", reads["live"])
	}
}

func TestRunnerBindingSweep_UnlabelledAccountInUseOrIdle(t *testing.T) {
	cases := map[string]struct {
		objs     []client.Object
		wantKept bool
	}{
		"unfinished Job of a running run keeps the bindings": {
			objs: []client.Object{sweepRun(ottoflowv1alpha1.WorkflowRunPhaseRunning), sweepJob(false)}, wantKept: true,
		},
		"running pod of a running run keeps the bindings when its Job has finished": {
			objs: []client.Object{sweepRun(ottoflowv1alpha1.WorkflowRunPhaseRunning), sweepJob(true), sweepPod("idle")}, wantKept: true,
		},
		"unfinished Job with a non-blocking controller reference keeps the bindings": {
			objs: []client.Object{sweepRun(ottoflowv1alpha1.WorkflowRunPhaseRunning), nonBlockingOwner(sweepJob(false))}, wantKept: true,
		},
		"running pod with a non-blocking controller reference keeps the bindings": {
			objs: []client.Object{sweepRun(ottoflowv1alpha1.WorkflowRunPhaseRunning), sweepJob(true), nonBlockingOwner(sweepPod("idle"))}, wantKept: true,
		},
		"unfinished Job of a run using another account removes the bindings": {
			objs: []client.Object{sweepRunAs(ottoflowv1alpha1.WorkflowRunPhaseRunning, "other"), sweepJob(false)},
		},
		"running pod of a run using another account removes the bindings": {
			objs: []client.Object{sweepRunAs(ottoflowv1alpha1.WorkflowRunPhaseRunning, "other"), sweepJob(true), sweepPod("idle")},
		},
		"running pod of another account does not keep the bindings": {
			objs: []client.Object{sweepRun(ottoflowv1alpha1.WorkflowRunPhaseRunning), sweepJob(true), sweepPod("other")},
		},
		"unfinished Job of another account does not keep the bindings": {
			objs: []client.Object{sweepRun(ottoflowv1alpha1.WorkflowRunPhaseRunning), func() client.Object {
				job := sweepJob(false)
				job.Spec.Template.Spec.ServiceAccountName = sweepPod("other").Spec.ServiceAccountName
				return job
			}()},
		},
		"no live run removes the bindings": {},
		"failed run with an active Job removes the bindings": {
			objs: []client.Object{sweepRun(ottoflowv1alpha1.WorkflowRunPhaseFailed), sweepJob(false)},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			objs := append(sweepBindings("idle"), sweepUnlabelledAccount("idle"))
			r, c := newSweepReconciler(interceptor.Funcs{}, append(objs, tc.objs...)...)
			r.sweepRunnerBindings(context.Background())
			requireBindings(t, c, tc.wantKept, bindingNames("idle")...)
		})
	}
}

func TestRunnerBindingSweep_KeepsBindingsWhenInUseCheckFails(t *testing.T) {
	failListOf := func(match func(client.ObjectList) bool) interceptor.Funcs {
		return interceptor.Funcs{List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if match(list) {
				return errors.New("list failed")
			}
			return cl.List(ctx, list, opts...)
		}}
	}
	failJobs := failListOf(func(l client.ObjectList) bool { _, ok := l.(*batchv1.JobList); return ok })
	failPods := failListOf(func(l client.ObjectList) bool { _, ok := l.(*corev1.PodList); return ok })
	failOwnerGet := failGetOf[*ottoflowv1alpha1.WorkflowRun](errors.New("get failed"))
	cases := map[string]struct {
		funcs interceptor.Funcs
		objs  []client.Object
	}{
		"job list":   {failJobs, nil},
		"pod list":   {failPods, nil},
		"run lookup": {failOwnerGet, []client.Object{sweepRun(ottoflowv1alpha1.WorkflowRunPhaseRunning), sweepJob(false)}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			objs := append(sweepBindings("idle"), sweepUnlabelledAccount("idle"))
			r, c := newSweepReconciler(tc.funcs, append(objs, tc.objs...)...)
			r.sweepRunnerBindings(context.Background())
			requireBindings(t, c, true, bindingNames("idle")...)
		})
	}
}

func TestRunnerBindingSweep_KeepsBindingsOfUnlabelledServiceAccountWithLivePod(t *testing.T) {
	_, c := startSweepEnvtest(t)
	ctx := context.Background()
	create := func(obj client.Object) {
		t.Helper()
		if err := c.Create(ctx, obj); err != nil {
			t.Fatalf("create %T %s: %v", obj, obj.GetName(), err)
		}
	}
	create(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: sweepNS}})
	create(sweepUnlabelledAccount("idle"))
	for _, b := range sweepBindings("idle") {
		create(b)
	}
	run := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run", Namespace: sweepNS},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: sweepNS}},
	}
	create(run)
	run.Status.Phase = ottoflowv1alpha1.WorkflowRunPhaseRunning
	if err := c.Status().Update(ctx, run); err != nil {
		t.Fatalf("status update run: %v", err)
	}
	containers := []corev1.Container{{Name: "runner", Image: "img"}}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: "run-runner", Namespace: sweepNS,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(run, ottoflowv1alpha1.GroupVersion.WithKind("WorkflowRun"))},
		},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever, ServiceAccountName: "job-template-sa", Containers: containers,
		}}},
	}
	create(job)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "run-runner-pod", Namespace: sweepNS,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(job, batchv1.SchemeGroupVersion.WithKind("Job"))},
		},
		Spec: corev1.PodSpec{ServiceAccountName: "idle", Containers: containers},
	}
	create(pod)
	pod.Status.Phase = corev1.PodRunning
	if err := c.Status().Update(ctx, pod); err != nil {
		t.Fatalf("status update pod: %v", err)
	}

	r := &WorkflowRunReconciler{Client: c, APIReader: c, Scheme: unitTestScheme, RunnerConfig: RunnerConfig{RunnerServiceAccount: "idle"}}
	r.sweepRunnerBindings(ctx)
	requireBindings(t, c, true, bindingNames("idle")...)
}
