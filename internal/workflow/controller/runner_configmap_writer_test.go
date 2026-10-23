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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

const (
	cmWriterRole = "ottoflow-runner-configmap-writer"
	cmWriterSA   = "wf-runner"
	cmWriterUID  = types.UID("sa-uid-1")
	cmWriterNS   = "run-a"
)

func cmWriterSAObj() *corev1.ServiceAccount {
	return &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: cmWriterSA, Namespace: cmWriterNS, UID: cmWriterUID}}
}

func cmWriterReconciler(c client.Client, role string) *WorkflowRunReconciler {
	return &WorkflowRunReconciler{Client: c, Scheme: unitTestScheme, RunnerConfig: RunnerConfig{
		RunnerClusterRole: "ottoflow-role", RunnerConfigMapWriterRole: role,
	}}
}

func getWriterBinding(t *testing.T, c client.Client) (*rbacv1.RoleBinding, error) {
	t.Helper()
	rb := &rbacv1.RoleBinding{}
	err := c.Get(context.Background(), types.NamespacedName{Namespace: cmWriterNS, Name: runnerConfigMapWriterBindingName(cmWriterSA)}, rb)
	return rb, err
}

func TestEnsureRunnerConfigMapWriter_CreatesBindingWithOwnerRef(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(cmWriterSAObj()).Build()
	if err := cmWriterReconciler(c, cmWriterRole).ensureRunnerConfigMapWriter(context.Background(), cmWriterNS, cmWriterSA); err != nil {
		t.Fatal(err)
	}
	rb, err := getWriterBinding(t, c)
	if err != nil {
		t.Fatal(err)
	}
	if rb.RoleRef.Kind != "ClusterRole" || rb.RoleRef.Name != cmWriterRole {
		t.Errorf("roleRef = %+v", rb.RoleRef)
	}
	if len(rb.Subjects) != 1 || rb.Subjects[0].Name != cmWriterSA || rb.Subjects[0].Namespace != cmWriterNS {
		t.Errorf("subjects = %+v", rb.Subjects)
	}
	if len(rb.OwnerReferences) != 1 || rb.OwnerReferences[0].UID != cmWriterUID || rb.OwnerReferences[0].Kind != "ServiceAccount" {
		t.Errorf("ownerRefs = %+v", rb.OwnerReferences)
	}
	if rb.Labels["app.kubernetes.io/part-of"] != "ottoflow" {
		t.Errorf("labels = %v", rb.Labels)
	}
}

func TestEnsureRunnerConfigMapWriter_EmptyRoleSkips(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(cmWriterSAObj()).Build()
	if err := cmWriterReconciler(c, "").ensureRunnerConfigMapWriter(context.Background(), cmWriterNS, cmWriterSA); err != nil {
		t.Fatal(err)
	}
	if _, err := getWriterBinding(t, c); err == nil {
		t.Fatal("binding created with empty writer role")
	}
}

func TestEnsureRunnerConfigMapWriter_UnlabeledBindingRefused(t *testing.T) {
	foreign := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: runnerConfigMapWriterBindingName(cmWriterSA), Namespace: cmWriterNS},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "foreign-role"},
	}
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(cmWriterSAObj(), foreign).Build()
	if err := cmWriterReconciler(c, cmWriterRole).ensureRunnerConfigMapWriter(context.Background(), cmWriterNS, cmWriterSA); err == nil {
		t.Fatal("expected error for unlabeled binding")
	}
	rb, _ := getWriterBinding(t, c)
	if rb.RoleRef.Name != "foreign-role" {
		t.Errorf("unlabeled binding was modified: %+v", rb.RoleRef)
	}
}

func TestEnsureRunnerConfigMapWriter_RoleRefDriftRecreated(t *testing.T) {
	stale := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: runnerConfigMapWriterBindingName(cmWriterSA), Namespace: cmWriterNS, Labels: map[string]string{"app.kubernetes.io/part-of": ottoflowPartOf}},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "old-role"},
	}
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(cmWriterSAObj(), stale).Build()
	if err := cmWriterReconciler(c, cmWriterRole).ensureRunnerConfigMapWriter(context.Background(), cmWriterNS, cmWriterSA); err != nil {
		t.Fatal(err)
	}
	rb, err := getWriterBinding(t, c)
	if err != nil {
		t.Fatal(err)
	}
	if rb.RoleRef.Name != cmWriterRole {
		t.Errorf("roleRef = %+v", rb.RoleRef)
	}
}

func TestEnsureRunnerConfigMapWriter_SubjectDriftRepaired(t *testing.T) {
	drifted := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: runnerConfigMapWriterBindingName(cmWriterSA), Namespace: cmWriterNS, Labels: map[string]string{"app.kubernetes.io/part-of": ottoflowPartOf}},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: cmWriterRole},
	}
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(cmWriterSAObj(), drifted).Build()
	if err := cmWriterReconciler(c, cmWriterRole).ensureRunnerConfigMapWriter(context.Background(), cmWriterNS, cmWriterSA); err != nil {
		t.Fatal(err)
	}
	rb, _ := getWriterBinding(t, c)
	if len(rb.Subjects) != 1 || rb.Subjects[0].Name != cmWriterSA {
		t.Errorf("subjects = %+v", rb.Subjects)
	}
}

// ensureRunnerAccess hits the SA AlreadyExists branch (empty local UID); the binding must still carry the live UID.
func TestEnsureRunnerAccess_AlreadyExistsSA_BindingOwnerRefUID(t *testing.T) {
	ns := cmWriterNS
	sa := cmWriterSAObj()
	wr := &ottoflowv1alpha1.WorkflowRun{ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: ns}}
	// First SA Get misses so Create returns AlreadyExists, leaving the local SA without a UID.
	gets := 0
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(sa).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.ServiceAccount); ok && gets == 0 {
				gets++
				return apierrors.NewNotFound(schema.GroupResource{Resource: "serviceaccounts"}, key.Name)
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	}).Build()
	if err := cmWriterReconciler(c, cmWriterRole).ensureRunnerAccess(context.Background(), wr, nil, cmWriterSA, false); err != nil {
		t.Fatal(err)
	}
	rb, err := getWriterBinding(t, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(rb.OwnerReferences) != 1 || rb.OwnerReferences[0].UID != cmWriterUID {
		t.Errorf("ownerRefs = %+v", rb.OwnerReferences)
	}
}

func TestEnsureRunnerAccess_ExplicitSA_NoWriterBinding(t *testing.T) {
	ns := cmWriterNS
	wr := &ottoflowv1alpha1.WorkflowRun{ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: ns}}
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(cmWriterSAObj()).Build()
	if err := cmWriterReconciler(c, cmWriterRole).ensureRunnerAccess(context.Background(), wr, nil, cmWriterSA, true); err != nil {
		t.Fatal(err)
	}
	if _, err := getWriterBinding(t, c); err == nil {
		t.Fatal("binding created for explicit SA")
	}
}

func existingJobFixture(jobStatus batchv1.JobStatus) (*ottoflowv1alpha1.WorkflowRun, []client.Object) {
	ns := cmWriterNS
	jobName := "run-1-runner"
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowSpec{Steps: []ottoflowv1alpha1.Step{{Name: "s1", Expressions: []ottoflowv1alpha1.Expression{{Name: "x", Expression: `"ok"`}}}}},
	}
	wr := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: ns},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf", Namespace: ns}},
		Status: ottoflowv1alpha1.WorkflowRunStatus{
			Phase:     ottoflowv1alpha1.WorkflowRunPhasePending,
			Execution: &ottoflowv1alpha1.WorkflowRunExecutionStatus{JobName: jobName, Phase: "Pending"},
		},
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: ns},
		Spec:       batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{ServiceAccountName: cmWriterSA}}},
		Status:     jobStatus,
	}
	return wr, []client.Object{wf, wr, job, cmWriterSAObj()}
}

func TestReconcile_ExistingJob_EnsuresWriterBinding(t *testing.T) {
	wr, objs := existingJobFixture(batchv1.JobStatus{Active: 1})
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(wr).WithObjects(objs...).Build()
	r := cmWriterReconciler(c, cmWriterRole)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(wr)}); err != nil {
		t.Fatal(err)
	}
	if _, err := getWriterBinding(t, c); err != nil {
		t.Fatalf("binding not created for run with existing Job: %v", err)
	}
	got := &ottoflowv1alpha1.WorkflowRun{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(wr), got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase == ottoflowv1alpha1.WorkflowRunPhaseFailed {
		t.Error("run failed")
	}
}

// An ensure failure on an existing-Job reconcile must fall through to Job handling (a Failed Job still fails the run).
func TestReconcile_ExistingJob_EnsureFails_StillHandlesFailedJob(t *testing.T) {
	wr, objs := existingJobFixture(batchv1.JobStatus{Failed: 1})
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(wr).WithObjects(objs...).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*rbacv1.RoleBinding); ok {
				return errors.New("boom")
			}
			return cl.Create(ctx, obj, opts...)
		},
	}).Build()
	r := cmWriterReconciler(c, cmWriterRole)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(wr)}); err != nil {
		t.Fatalf("ensure error leaked out of Reconcile: %v", err)
	}
	got := &ottoflowv1alpha1.WorkflowRun{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(wr), got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != ottoflowv1alpha1.WorkflowRunPhaseFailed || got.Status.Execution == nil ||
		got.Status.Execution.Message != "Runner Job run-1-runner failed" {
		t.Errorf("failed Job not handled: phase=%s exec=%+v msg=%q", got.Status.Phase, got.Status.Execution, got.Status.Message)
	}
}
