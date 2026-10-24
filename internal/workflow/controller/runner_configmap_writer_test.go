/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"context"
	"errors"
	"slices"
	"strings"
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
	if err := cmWriterReconciler(c, cmWriterRole).ensureRunnerConfigMapWriter(context.Background(), cmWriterNS, cmWriterSA, cmWriterUID); err != nil {
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
	if err := cmWriterReconciler(c, "").ensureRunnerConfigMapWriter(context.Background(), cmWriterNS, cmWriterSA, cmWriterUID); err != nil {
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
	if err := cmWriterReconciler(c, cmWriterRole).ensureRunnerConfigMapWriter(context.Background(), cmWriterNS, cmWriterSA, cmWriterUID); err == nil {
		t.Fatal("expected error for unlabeled binding")
	}
	rb, _ := getWriterBinding(t, c)
	if rb.RoleRef.Name != "foreign-role" {
		t.Errorf("unlabeled binding was modified: %+v", rb.RoleRef)
	}
}

func TestEnsureRunnerConfigMapWriter_RoleRefDriftRecreated(t *testing.T) {
	stale := managedWriterBinding("ClusterRole", "old-role", cmWriterUID)
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(cmWriterSAObj(), stale).Build()
	if err := cmWriterReconciler(c, cmWriterRole).ensureRunnerConfigMapWriter(context.Background(), cmWriterNS, cmWriterSA, cmWriterUID); err != nil {
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
	drifted := managedWriterBinding("ClusterRole", cmWriterRole, cmWriterUID)
	drifted.Subjects = nil
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(cmWriterSAObj(), drifted).Build()
	if err := cmWriterReconciler(c, cmWriterRole).ensureRunnerConfigMapWriter(context.Background(), cmWriterNS, cmWriterSA, cmWriterUID); err != nil {
		t.Fatal(err)
	}
	rb, _ := getWriterBinding(t, c)
	if len(rb.Subjects) != 1 || rb.Subjects[0].Name != cmWriterSA {
		t.Errorf("subjects = %+v", rb.Subjects)
	}
}

func managedWriterBinding(kind, name string, owner types.UID) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: runnerConfigMapWriterBindingName(cmWriterSA), Namespace: cmWriterNS,
			Labels:          map[string]string{"app.kubernetes.io/part-of": ottoflowPartOf},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "ServiceAccount", Name: cmWriterSA, UID: owner}},
		},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: kind, Name: name},
		Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: cmWriterSA, Namespace: cmWriterNS}},
	}
}

func TestEnsureRunnerConfigMapWriter_RoleKindMismatchRecreated(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(cmWriterSAObj(), managedWriterBinding("Role", cmWriterRole, cmWriterUID)).Build()
	if err := cmWriterReconciler(c, cmWriterRole).ensureRunnerConfigMapWriter(context.Background(), cmWriterNS, cmWriterSA, cmWriterUID); err != nil {
		t.Fatal(err)
	}
	rb, _ := getWriterBinding(t, c)
	if rb.RoleRef.Kind != "ClusterRole" {
		t.Errorf("roleRef = %+v", rb.RoleRef)
	}
}

func TestEnsureRunnerConfigMapWriter_StaleSAOwnerRecreated(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(cmWriterSAObj(), managedWriterBinding("ClusterRole", cmWriterRole, "old-uid")).Build()
	if err := cmWriterReconciler(c, cmWriterRole).ensureRunnerConfigMapWriter(context.Background(), cmWriterNS, cmWriterSA, cmWriterUID); err != nil {
		t.Fatal(err)
	}
	rb, _ := getWriterBinding(t, c)
	if len(rb.OwnerReferences) != 1 || rb.OwnerReferences[0].UID != cmWriterUID {
		t.Errorf("ownerRefs = %+v", rb.OwnerReferences)
	}
}

// failGetOf fails Gets of objects of type T with err and passes every other Get through.
func failGetOf[T client.Object](err error) interceptor.Funcs {
	return interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if _, ok := obj.(T); ok {
			return err
		}
		return c.Get(ctx, key, obj, opts...)
	}}
}

// Every API call in ensureRunnerConfigMapWriter keeps Forbidden terminal and requeues other failures.
func TestEnsureRunnerConfigMapWriter_CallErrorClassification(t *testing.T) {
	gr := schema.GroupResource{Resource: "rolebindings"}
	drifted := managedWriterBinding("ClusterRole", cmWriterRole, cmWriterUID)
	drifted.Subjects = nil
	stale := managedWriterBinding("ClusterRole", "old-role", cmWriterUID)
	sites := map[string]struct {
		objs  []client.Object
		saUID types.UID
		fail  func(error) interceptor.Funcs
	}{
		"binding get": {nil, cmWriterUID, failGetOf[*rbacv1.RoleBinding]},
		"sa get":      {nil, "", failGetOf[*corev1.ServiceAccount]},
		"update": {[]client.Object{drifted}, cmWriterUID, func(e error) interceptor.Funcs {
			return interceptor.Funcs{Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error { return e }}
		}},
		"delete": {[]client.Object{stale}, cmWriterUID, func(e error) interceptor.Funcs {
			return interceptor.Funcs{Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error { return e }}
		}},
		"create": {nil, cmWriterUID, func(e error) interceptor.Funcs {
			return interceptor.Funcs{Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error { return e }}
		}},
	}
	errs := map[string]struct {
		err       error
		transient bool
	}{
		"timeout":   {apierrors.NewTimeoutError("slow", 1), true},
		"5xx":       {apierrors.NewInternalError(errors.New("boom")), true},
		"conflict":  {apierrors.NewConflict(gr, "x", errors.New("stale")), true},
		"exists":    {apierrors.NewAlreadyExists(gr, "x"), true},
		"forbidden": {apierrors.NewForbidden(gr, "x", errors.New("no")), false},
		"invalid":   {apierrors.NewInvalid(schema.GroupKind{Kind: "RoleBinding"}, "x", nil), false},
	}
	for site, s := range sites {
		for name, e := range errs {
			t.Run(site+"/"+name, func(t *testing.T) {
				c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(append([]client.Object{cmWriterSAObj()}, s.objs...)...).WithInterceptorFuncs(s.fail(e.err)).Build()
				err := cmWriterReconciler(c, cmWriterRole).ensureRunnerConfigMapWriter(context.Background(), cmWriterNS, cmWriterSA, s.saUID)
				if err == nil || errors.Is(err, errRequeueRunnerAccess) != e.transient {
					t.Errorf("err = %v, want transient=%v", err, e.transient)
				}
			})
		}
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

func TestEnsureRunnerConfigMapWriter_SteadyStateSkipsSARead(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(managedWriterBinding("ClusterRole", cmWriterRole, cmWriterUID)).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.ServiceAccount); ok {
				t.Error("unexpected ServiceAccount read")
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	}).Build()
	if err := cmWriterReconciler(c, cmWriterRole).ensureRunnerConfigMapWriter(context.Background(), cmWriterNS, cmWriterSA, ""); err != nil {
		t.Fatal(err)
	}
}

// failOn fails one verb on objects accepted by match(obj, name); every other call passes through.
func failOn(verb string, match func(client.Object, string) bool, err error) interceptor.Funcs {
	var f interceptor.Funcs
	switch verb {
	case "get":
		f.Get = func(ctx context.Context, c client.WithWatch, k client.ObjectKey, o client.Object, opts ...client.GetOption) error {
			if match(o, k.Name) {
				return err
			}
			return c.Get(ctx, k, o, opts...)
		}
	case "create":
		f.Create = func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
			if match(o, o.GetName()) {
				return err
			}
			return c.Create(ctx, o, opts...)
		}
	case "update":
		f.Update = func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.UpdateOption) error {
			if match(o, o.GetName()) {
				return err
			}
			return c.Update(ctx, o, opts...)
		}
	case "delete":
		f.Delete = func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.DeleteOption) error {
			if match(o, o.GetName()) {
				return err
			}
			return c.Delete(ctx, o, opts...)
		}
	}
	return f
}

func isCRB(name string) func(client.Object, string) bool {
	return func(o client.Object, n string) bool { _, ok := o.(*rbacv1.ClusterRoleBinding); return ok && n == name }
}

func isSA(o client.Object, _ string) bool { _, ok := o.(*corev1.ServiceAccount); return ok }

func callerBinding(role string, subjects []rbacv1.Subject, name string) *rbacv1.ClusterRoleBinding {
	b := buildRunnerClusterRoleBinding(name, role, cmWriterSA, cmWriterNS)
	if subjects != nil {
		b.Subjects = subjects
	}
	return b
}

// Every API call in ensureRunnerAccess keeps Forbidden/Invalid terminal and requeues other failures.
func TestEnsureRunnerAccess_CallSiteClassification(t *testing.T) {
	gr := schema.GroupResource{Resource: "clusterrolebindings"}
	mainName := workflowRunnerRoleBindingName(cmWriterNS, cmWriterSA)
	callerName := agentExecutorCallerRoleBindingName(cmWriterNS, cmWriterSA)
	other := []rbacv1.Subject{{Kind: "ServiceAccount", Name: "other", Namespace: cmWriterNS}}
	exists := apierrors.NewAlreadyExists(gr, mainName)
	sa := cmWriterSAObj()
	type site struct {
		objs       []client.Object
		callerRole string
		fail       func(error) interceptor.Funcs
	}
	on := func(verb string, m func(client.Object, string) bool) func(error) interceptor.Funcs {
		return func(e error) interceptor.Funcs { return failOn(verb, m, e) }
	}
	sites := map[string]site{
		"sa get":                 {nil, "", on("get", isSA)},
		"sa create":              {nil, "", on("create", isSA)},
		"crb get":                {[]client.Object{sa}, "", on("get", isCRB(mainName))},
		"crb create":             {[]client.Object{sa}, "", on("create", isCRB(mainName))},
		"crb update":             {[]client.Object{sa, callerBinding("ottoflow-role", other, mainName)}, "", on("update", isCRB(mainName))},
		"crb delete":             {[]client.Object{sa, callerBinding("old-role", nil, mainName)}, "", on("delete", isCRB(mainName))},
		"crb recreate create":    {[]client.Object{sa, callerBinding("old-role", nil, mainName)}, "", on("create", isCRB(mainName))},
		"caller get":             {[]client.Object{sa}, "caller-role", on("get", isCRB(callerName))},
		"caller create":          {[]client.Object{sa}, "caller-role", on("create", isCRB(callerName))},
		"caller update":          {[]client.Object{sa, callerBinding("caller-role", other, callerName)}, "caller-role", on("update", isCRB(callerName))},
		"caller delete":          {[]client.Object{sa, callerBinding("old-role", nil, callerName)}, "caller-role", on("delete", isCRB(callerName))},
		"caller recreate create": {[]client.Object{sa, callerBinding("old-role", nil, callerName)}, "caller-role", on("create", isCRB(callerName))},
		"crb re-get after exists": {[]client.Object{sa, callerBinding("old-role", nil, mainName)}, "", func(e error) interceptor.Funcs {
			gets := 0
			f := failOn("create", isCRB(mainName), exists)
			f.Get = func(ctx context.Context, c client.WithWatch, k client.ObjectKey, o client.Object, opts ...client.GetOption) error {
				if isCRB(mainName)(o, k.Name) {
					if gets++; gets > 1 {
						return e
					}
				}
				return c.Get(ctx, k, o, opts...)
			}
			return f
		}},
	}
	errs := map[string]struct {
		err       error
		transient bool
	}{
		"timeout":   {apierrors.NewTimeoutError("slow", 1), true},
		"5xx":       {apierrors.NewInternalError(errors.New("boom")), true},
		"conflict":  {apierrors.NewConflict(gr, "x", errors.New("stale")), true},
		"forbidden": {apierrors.NewForbidden(gr, "x", errors.New("no")), false},
		"invalid":   {apierrors.NewInvalid(schema.GroupKind{Kind: "ClusterRoleBinding"}, "x", nil), false},
	}
	wr := &ottoflowv1alpha1.WorkflowRun{ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: cmWriterNS}}
	run := func(s site, e error) error {
		c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(s.objs...).WithInterceptorFuncs(s.fail(e)).Build()
		r := cmWriterReconciler(c, cmWriterRole)
		r.RunnerConfig.AgentExecutorCallerRole = s.callerRole
		return r.ensureRunnerAccess(context.Background(), wr, nil, cmWriterSA, false)
	}
	for name, s := range sites {
		for en, e := range errs {
			t.Run(name+"/"+en, func(t *testing.T) {
				if err := run(s, e.err); err == nil || errors.Is(err, errRequeueRunnerAccess) != e.transient {
					t.Errorf("err = %v, want transient=%v", err, e.transient)
				}
			})
		}
	}
	for en, e := range errs {
		t.Run("hint/"+en, func(t *testing.T) {
			err := run(sites["crb recreate create"], e.err)
			if err == nil || strings.Contains(err.Error(), "holds 'bind'") == e.transient {
				t.Errorf("err = %v, want bind hint only when terminal", err)
			}
		})
	}
}

// A transient runner-access error must back off via the returned error, not fail the run or reset backoff with RequeueAfter.
func TestReconcile_RunnerAccessTransient_ReturnsErrorWithBackoff(t *testing.T) {
	timeout := apierrors.NewTimeoutError("slow", 1)
	cases := map[string]interceptor.Funcs{
		"crb get timeout": failGetOf[*rbacv1.ClusterRoleBinding](timeout),
		"writer rolebinding create timeout": failOn("create", func(o client.Object, _ string) bool {
			_, ok := o.(*rbacv1.RoleBinding)
			return ok
		}, timeout),
	}
	for name, fns := range cases {
		t.Run(name, func(t *testing.T) {
			wr, objs := existingJobFixture(batchv1.JobStatus{})
			objs = slices.DeleteFunc(objs, func(o client.Object) bool { _, ok := o.(*batchv1.Job); return ok })
			c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(wr).WithObjects(objs...).WithInterceptorFuncs(fns).Build()
			res, err := cmWriterReconciler(c, cmWriterRole).Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(wr)})
			got := &ottoflowv1alpha1.WorkflowRun{}
			if gerr := c.Get(context.Background(), client.ObjectKeyFromObject(wr), got); gerr != nil {
				t.Fatal(gerr)
			}
			if !errors.Is(err, errRequeueRunnerAccess) || res.RequeueAfter != 0 || got.Status.Phase == ottoflowv1alpha1.WorkflowRunPhaseFailed {
				t.Errorf("res=%+v err=%v phase=%s", res, err, got.Status.Phase)
			}
			if jerr := c.Get(context.Background(), types.NamespacedName{Namespace: cmWriterNS, Name: "run-1-runner"}, &batchv1.Job{}); !apierrors.IsNotFound(jerr) {
				t.Errorf("Job created or lookup failed: %v", jerr)
			}
		})
	}
}

func TestEnsureRunnerConfigMapWriter_SAFallbackErrorNamesServiceAccount(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithInterceptorFuncs(failGetOf[*corev1.ServiceAccount](apierrors.NewTimeoutError("slow", 1))).Build()
	err := cmWriterReconciler(c, cmWriterRole).ensureRunnerConfigMapWriter(context.Background(), cmWriterNS, cmWriterSA, "")
	if err == nil || !strings.Contains(err.Error(), "ServiceAccount "+cmWriterNS+"/"+cmWriterSA) {
		t.Errorf("err = %v, want it to name ServiceAccount %s/%s", err, cmWriterNS, cmWriterSA)
	}
}
