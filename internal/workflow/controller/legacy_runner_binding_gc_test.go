/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/go-logr/logr/funcr"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/log"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

const gcRole = "ottoflow-role"

func gcBinding(name, saName string, labelled bool) *rbacv1.ClusterRoleBinding {
	b := buildRunnerClusterRoleBinding(name, gcRole, saName, "ns")
	if !labelled {
		b.Labels = nil
	}
	return b
}

// runGC runs the GC over objs and returns the names of the bindings that remain.
func runGC(t *testing.T, flagRole, runnerSA string, objs ...client.Object) map[string]bool {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(objs...).Build()
	if err := NewLegacyRunnerBindingGC(c, flagRole, "controller-manager", runnerSA).Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	var list rbacv1.ClusterRoleBindingList
	if err := c.List(context.Background(), &list); err != nil {
		t.Fatal(err)
	}
	left := map[string]bool{}
	for _, b := range list.Items {
		left[b.Name] = true
	}
	return left
}

func TestLegacyRunnerBindingGC_DeletesControllerManagerBinding(t *testing.T) {
	legacy := workflowRunnerRoleBindingName("ns", "controller-manager")
	left := runGC(t, gcRole, "", gcBinding(legacy, "controller-manager", true))
	if left[legacy] {
		t.Errorf("legacy binding %q not deleted", legacy)
	}
}

func TestLegacyRunnerBindingGC_KeepsCurrentAndChartBindings(t *testing.T) {
	current := workflowRunnerRoleBindingName("ns", "wf-runner")
	lookalike := workflowRunnerRoleBindingName("ns", "controller-manager")
	left := runGC(t, gcRole, "",
		gcBinding(current, "wf-runner", true),
		gcBinding("ottoflow-rolebinding", "controller-manager", true),
		gcBinding(lookalike, "controller-manager", false),
	)
	for _, n := range []string{current, "ottoflow-rolebinding", lookalike} {
		if !left[n] {
			t.Errorf("binding %q was deleted", n)
		}
	}
}

func TestLegacyRunnerBindingGC_KeepsOtherReleaseBinding(t *testing.T) {
	other := workflowRunnerRoleBindingName("ns", "controller-manager")
	left := runGC(t, "ottoflow-staging-role", "", gcBinding(other, "controller-manager", true))
	if !left[other] {
		t.Errorf("other release's binding %q was deleted", other)
	}
}

func TestLegacyRunnerBindingGC_SharedRunnerInstallCleansLegacy(t *testing.T) {
	legacy := workflowRunnerRoleBindingName("ns", "controller-manager")
	shared := workflowRunnerRoleBindingName("ns", "shared-runner")
	left := runGC(t, gcRole, "shared-runner",
		gcBinding(legacy, "controller-manager", true),
		gcBinding(shared, "shared-runner", true),
	)
	if left[legacy] {
		t.Errorf("legacy binding %q not deleted", legacy)
	}
	if !left[shared] {
		t.Errorf("configured shared runner binding %q was deleted", shared)
	}
}

func TestLegacyRunnerBindingGC_KeepsEscapeHatchBinding(t *testing.T) {
	hatch := workflowRunnerRoleBindingName("ns", "controller-manager")
	left := runGC(t, gcRole, "controller-manager", gcBinding(hatch, "controller-manager", true))
	if !left[hatch] {
		t.Errorf("escape-hatch binding %q was deleted", hatch)
	}
}

func TestLegacyRunnerBindingGC_KeepsOtherReleaseRunnerOnSharedRole(t *testing.T) {
	live := workflowRunnerRoleBindingName("ns", "other-sa")
	left := runGC(t, gcRole, "", gcBinding(live, "other-sa", true))
	if !left[live] {
		t.Errorf("other release's live runner binding %q on the shared role was deleted", live)
	}
}

func TestLegacyRunnerBindingGC_DisabledWhenFlagEmpty(t *testing.T) {
	legacy := workflowRunnerRoleBindingName("ns", "controller-manager")
	left := runGC(t, "", "", gcBinding(legacy, "controller-manager", true))
	if !left[legacy] {
		t.Errorf("binding %q deleted with GC disabled", legacy)
	}
}

func TestLegacyRunnerBindingGC_KeepsChartBindingWithCollidingName(t *testing.T) {
	name := workflowRunnerRoleBindingName("ns", "controller-manager")
	b := gcBinding(name, "controller-manager", true)
	b.Labels["app.kubernetes.io/component"] = "rbac"
	if left := runGC(t, gcRole, "", b); !left[name] {
		t.Errorf("chart binding %q was deleted", name)
	}
}

func TestLegacyRunnerBindingGC_DeleteIsPreconditioned(t *testing.T) {
	name := workflowRunnerRoleBindingName("ns", "controller-manager")
	b := gcBinding(name, "controller-manager", true)
	b.UID = "uid-1"
	var got client.DeleteOptions
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(b).WithInterceptorFuncs(interceptor.Funcs{
		Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			got.ApplyOptions(opts)
			return nil
		},
	}).Build()
	if err := NewLegacyRunnerBindingGC(c, gcRole, "controller-manager", "").Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got.Preconditions == nil || got.Preconditions.UID == nil || *got.Preconditions.UID != "uid-1" || got.Preconditions.ResourceVersion == nil {
		t.Errorf("delete lacked UID/resourceVersion preconditions: %+v", got.Preconditions)
	}
}

// inUse is a pod, its Job and its WorkflowRun, wired the way a real runner's are.
type inUse struct {
	pod *corev1.Pod
	job *batchv1.Job
	wr  *ottoflowv1alpha1.WorkflowRun
}

func newInUse() inUse {
	wr := &ottoflowv1alpha1.WorkflowRun{}
	wr.Namespace, wr.Name, wr.UID = "ns", "wr", "wr-uid"
	wr.Status.Phase = ottoflowv1alpha1.WorkflowRunPhaseRunning
	job := &batchv1.Job{}
	job.Namespace, job.Name, job.UID = "ns", "job", "job-uid"
	job.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(wr, ottoflowv1alpha1.GroupVersion.WithKind("WorkflowRun"))}
	pod := &corev1.Pod{}
	pod.Namespace, pod.Name = "ns", "runner-pod"
	pod.Spec.ServiceAccountName = "controller-manager"
	pod.Status.Phase = corev1.PodRunning
	pod.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(job, batchv1.SchemeGroupVersion.WithKind("Job"))}
	return inUse{pod, job, wr}
}

func (u inUse) objs() []client.Object { return []client.Object{u.pod, u.job, u.wr} }

// runGCIn runs the GC over a candidate binding plus extra objects, with a log sink and optional
// interceptors, checks whether the candidate remains, and returns what was logged.
func runGCIn(t *testing.T, wantKept bool, f interceptor.Funcs, extra ...client.Object) (logs string) {
	t.Helper()
	name := workflowRunnerRoleBindingName("ns", "controller-manager")
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithObjects(gcBinding(name, "controller-manager", true)).WithObjects(extra...).WithInterceptorFuncs(f).Build()
	var sb strings.Builder
	ctx := log.IntoContext(context.Background(), funcr.New(func(prefix, args string) { sb.WriteString(prefix + args + "\n") }, funcr.Options{}))
	if err := NewLegacyRunnerBindingGC(c, gcRole, "controller-manager", "").Start(ctx); err != nil {
		t.Fatal(err)
	}
	var list rbacv1.ClusterRoleBindingList
	if err := c.List(context.Background(), &list); err != nil {
		t.Fatal(err)
	}
	if kept := len(list.Items) == 1; kept != wantKept {
		t.Errorf("binding kept=%v, want %v", kept, wantKept)
	}
	return sb.String()
}

func TestLegacyRunnerBindingGC_KeptWhileLiveRunPodUsesSubject(t *testing.T) {
	runGCIn(t, true, interceptor.Funcs{}, newInUse().objs()...)
}

func TestLegacyRunnerBindingGC_DeletedWhenPodSucceeded(t *testing.T) {
	u := newInUse()
	u.pod.Status.Phase = corev1.PodSucceeded
	runGCIn(t, false, interceptor.Funcs{}, u.objs()...)
}

func TestLegacyRunnerBindingGC_DeletedWhenWorkflowRunTerminal(t *testing.T) {
	for _, ph := range []ottoflowv1alpha1.WorkflowRunPhase{ottoflowv1alpha1.WorkflowRunPhaseSucceeded, ottoflowv1alpha1.WorkflowRunPhaseFailed} {
		t.Run(string(ph), func(t *testing.T) {
			u := newInUse()
			u.wr.Status.Phase = ph
			runGCIn(t, false, interceptor.Funcs{}, u.objs()...)
		})
	}
}

func TestLegacyRunnerBindingGC_DeletedWhenJobHasOnlyCopiedLabel(t *testing.T) {
	u := newInUse()
	u.job.OwnerReferences = nil
	u.job.Labels = map[string]string{runnerManagedLabel: "wr"}
	runGCIn(t, false, interceptor.Funcs{}, u.objs()...)
}

func TestLegacyRunnerBindingGC_DeletedWhenOnlyControllerPodUsesSubject(t *testing.T) {
	u := newInUse()
	u.pod.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "job", UID: "job-uid", Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true)}}
	runGCIn(t, false, interceptor.Funcs{}, u.objs()...)
}

func TestLegacyRunnerBindingGC_DeletedWhenPinningPodInOtherNamespace(t *testing.T) {
	u := newInUse()
	u.pod.Namespace = "elsewhere"
	runGCIn(t, false, interceptor.Funcs{}, u.objs()...)
}

func TestLegacyRunnerBindingGC_SkipLogsBindingAndPod(t *testing.T) {
	logs := runGCIn(t, true, interceptor.Funcs{}, newInUse().objs()...)
	for _, want := range []string{workflowRunnerRoleBindingName("ns", "controller-manager"), "runner-pod(wr)"} {
		if !strings.Contains(logs, want) {
			t.Errorf("skip log lacks %q: %s", want, logs)
		}
	}
}

func TestLegacyRunnerBindingGC_KeptWhenPodListFails(t *testing.T) {
	runGCIn(t, true, interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, l client.ObjectList, o ...client.ListOption) error {
		if _, ok := l.(*corev1.PodList); ok {
			return errors.New("boom")
		}
		return c.List(ctx, l, o...)
	}}, newInUse().objs()...)
}

func TestLegacyRunnerBindingGC_DeletedWhenOwnerUIDDiffers(t *testing.T) {
	u := newInUse()
	u.job.OwnerReferences[0].UID = "stale-uid"
	runGCIn(t, false, interceptor.Funcs{}, u.objs()...)
}

func TestLegacyRunnerBindingGC_DeletedWhenOwnerWorkflowRunMissing(t *testing.T) {
	u := newInUse()
	runGCIn(t, false, interceptor.Funcs{}, u.pod, u.job)
}

func TestLegacyRunnerBindingGC_KeptWhenOwnerGetFails(t *testing.T) {
	for _, name := range []string{"job", "wr"} {
		t.Run(name, func(t *testing.T) {
			logs := runGCIn(t, true, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, k client.ObjectKey, o client.Object, opts ...client.GetOption) error {
				if k.Name == name {
					return errors.New("boom")
				}
				return c.Get(ctx, k, o, opts...)
			}}, newInUse().objs()...)
			if !strings.Contains(logs, "boom") {
				t.Errorf("error not logged: %s", logs)
			}
		})
	}
}

func TestLegacyRunnerBindingGC_DeletedWhenOwnerRefLacksFlag(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(u inUse)
	}{
		{"pod/controller", func(u inUse) { u.pod.OwnerReferences[0].Controller = ptr.To(false) }},
		{"pod/blockOwnerDeletion", func(u inUse) { u.pod.OwnerReferences[0].BlockOwnerDeletion = ptr.To(false) }},
		{"job/controller", func(u inUse) { u.job.OwnerReferences[0].Controller = ptr.To(false) }},
		{"job/blockOwnerDeletion", func(u inUse) { u.job.OwnerReferences[0].BlockOwnerDeletion = ptr.To(false) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := newInUse()
			tc.mutate(u)
			runGCIn(t, false, interceptor.Funcs{}, u.objs()...)
		})
	}
}
