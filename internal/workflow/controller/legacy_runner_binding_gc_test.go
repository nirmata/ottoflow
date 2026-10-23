/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"context"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
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
	if err := NewLegacyRunnerBindingGC(c, flagRole, runnerSA).Start(context.Background()); err != nil {
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
	if err := NewLegacyRunnerBindingGC(c, gcRole, "").Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got.Preconditions == nil || got.Preconditions.UID == nil || *got.Preconditions.UID != "uid-1" || got.Preconditions.ResourceVersion == nil {
		t.Errorf("delete lacked UID/resourceVersion preconditions: %+v", got.Preconditions)
	}
}
