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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

func wf(namespace, name string) *ottoflowv1alpha1.Workflow {
	return &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, UID: types.UID("uid-" + name)},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Expose: &ottoflowv1alpha1.ExposeSpec{Kagent: &ottoflowv1alpha1.KagentExposeSpec{}},
		},
	}
}

func TestAgentName(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		wfName    string
		want      string // "" means expect the hashed flo-<hash> fallback
	}{
		// A valid DNS-1123 label is used verbatim so kagent's UI shows a real name.
		{"simple", "default", "my-workflow", "my-workflow"},
		{"single char", "ns", "w", "w"},
		{"max length label", "ns", "a234567890123456789012345678901234567890123456789012345678901234"[:63], "a234567890123456789012345678901234567890123456789012345678901234"[:63]},
		// Invalid or over-long names fall back to the hashed scheme.
		{"over long", "ns", "an-equally-long-workflow-name-that-clearly-exceeds-the-sixty-three-character-dns-label-limit", ""},
		{"invalid chars", "ns", "My_Workflow", ""},
		{"leading dash", "ns", "-bad", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := wf(tt.namespace, tt.wfName)
			got := agentName(w)

			// Always DNS-1123 label compliant and <=63 chars.
			if errs := validation.IsDNS1123Label(got); len(errs) > 0 {
				t.Errorf("agentName(%q,%q)=%q not a DNS-1123 label: %v", tt.namespace, tt.wfName, got, errs)
			}
			if len(got) > 63 {
				t.Errorf("agentName length %d > 63: %q", len(got), got)
			}

			if tt.want != "" {
				if got != tt.want {
					t.Errorf("agentName(%q,%q)=%q want %q", tt.namespace, tt.wfName, got, tt.want)
				}
			} else {
				// Fallback: hashed flo-<hash> form.
				if got != hashedAgentName(w) {
					t.Errorf("agentName(%q,%q)=%q want hashed fallback %q", tt.namespace, tt.wfName, got, hashedAgentName(w))
				}
			}

			// Deterministic: same input -> same output.
			if again := agentName(wf(tt.namespace, tt.wfName)); again != got {
				t.Errorf("agentName not deterministic: %q != %q", got, again)
			}
		})
	}

	// Different namespace or name in the hashed fallback -> different agent name.
	a := hashedAgentName(wf("ns1", "wf"))
	b := hashedAgentName(wf("ns2", "wf"))
	c := hashedAgentName(wf("ns1", "wf2"))
	if a == b || a == c || b == c {
		t.Errorf("hashed agent names collided: ns1/wf=%q ns2/wf=%q ns1/wf2=%q", a, b, c)
	}
}

func TestBuildAgentObject(t *testing.T) {
	w := wf("prod", "nightly-report")
	obj := buildAgentObject(w, "ghcr.io/nirmata/serve-a2a:v1", "serve-a2a")

	if got := obj.GetAPIVersion(); got != kagentAPIVersion {
		t.Errorf("apiVersion=%q want %q", got, kagentAPIVersion)
	}
	if got := obj.GetKind(); got != kagentKind {
		t.Errorf("kind=%q want %q", got, kagentKind)
	}
	if got, want := obj.GetName(), agentName(w); got != want {
		t.Errorf("name=%q want %q", got, want)
	}
	if got := obj.GetNamespace(); got != "prod" {
		t.Errorf("namespace=%q want %q", got, "prod")
	}

	labels := obj.GetLabels()
	for k, want := range map[string]string{
		labelWorkflowNamespace: "prod",
		labelWorkflowName:      "nightly-report",
		labelWorkflowUID:       "uid-nightly-report",
	} {
		if labels[k] != want {
			t.Errorf("label %q=%q want %q", k, labels[k], want)
		}
	}

	if got, _, _ := unstructured.NestedString(obj.Object, "spec", "type"); got != "BYO" {
		t.Errorf("spec.type=%q want BYO", got)
	}
	if got, _, _ := unstructured.NestedString(obj.Object, "spec", "description"); got != "Runs the nightly-report OttoFlow workflow" {
		t.Errorf("spec.description=%q want default", got)
	}
	if got, _, _ := unstructured.NestedString(obj.Object, "spec", "byo", "deployment", "image"); got != "ghcr.io/nirmata/serve-a2a:v1" {
		t.Errorf("image=%q", got)
	}
	if got, _, _ := unstructured.NestedString(obj.Object, "spec", "byo", "deployment", "serviceAccountName"); got != "serve-a2a" {
		t.Errorf("serviceAccountName=%q", got)
	}

	env, _, err := unstructured.NestedSlice(obj.Object, "spec", "byo", "deployment", "env")
	if err != nil || len(env) != 3 {
		t.Fatalf("env slice: len=%d err=%v", len(env), err)
	}
	gotEnv := map[string]string{}
	for _, e := range env {
		m, _ := e.(map[string]interface{})
		gotEnv[m["name"].(string)] = m["value"].(string)
	}
	wantEnv := map[string]string{
		"WORKFLOW_NAME":      "nightly-report",
		"WORKFLOW_NAMESPACE": "prod",
		"A2A_PUBLIC_URL":     "http://" + agentName(w) + ".prod.svc.cluster.local:8080/",
	}
	for k, want := range wantEnv {
		if gotEnv[k] != want {
			t.Errorf("env %q=%q want %q", k, gotEnv[k], want)
		}
	}
}

func TestBuildAgentObjectCustomDescription(t *testing.T) {
	w := wf("prod", "nightly-report")
	w.Spec.Expose.Kagent.Description = "Custom desc"
	obj := buildAgentObject(w, "img", "sa")
	if got, _, _ := unstructured.NestedString(obj.Object, "spec", "description"); got != "Custom desc" {
		t.Errorf("spec.description=%q want %q", got, "Custom desc")
	}
}

func TestInputsExposable(t *testing.T) {
	req := func(name string) ottoflowv1alpha1.Input {
		return ottoflowv1alpha1.Input{Name: name, Required: true}
	}
	reqWithDefault := func(name string) ottoflowv1alpha1.Input {
		return ottoflowv1alpha1.Input{Name: name, Required: true, Default: "x"}
	}
	opt := func(name string) ottoflowv1alpha1.Input {
		return ottoflowv1alpha1.Input{Name: name}
	}

	cases := []struct {
		name   string
		inputs []ottoflowv1alpha1.Input
		want   bool
	}{
		{"no inputs", nil, true},
		{"single required first (prompt fills it)", []ottoflowv1alpha1.Input{req("a")}, true},
		{"optional first, required second (prompt cannot fill it)", []ottoflowv1alpha1.Input{opt("a"), req("b")}, false},
		{"two required (second unfillable)", []ottoflowv1alpha1.Input{req("a"), req("b")}, false},
		{"required-with-default second is fine", []ottoflowv1alpha1.Input{opt("a"), reqWithDefault("b")}, true},
		{"all optional", []ottoflowv1alpha1.Input{opt("a"), opt("b")}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := wf("ns", "wf")
			w.Spec.Inputs = tc.inputs
			if got := inputsExposable(w); got != tc.want {
				t.Errorf("inputsExposable = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestManagedBy(t *testing.T) {
	w := wf("prod", "report") // uid is "uid-report"
	managed := &unstructured.Unstructured{}
	managed.SetLabels(map[string]string{
		labelWorkflowNamespace: "prod",
		labelWorkflowName:      "report",
		labelWorkflowUID:       "uid-report",
	})
	if !managedBy(managed, w) {
		t.Error("managedBy = false for an Agent carrying our labels, want true")
	}

	// An unrelated Agent that merely shares the name (different UID) must not be adopted.
	foreign := &unstructured.Unstructured{}
	foreign.SetLabels(map[string]string{
		labelWorkflowNamespace: "prod",
		labelWorkflowName:      "report",
		labelWorkflowUID:       "someone-elses-uid",
	})
	if managedBy(foreign, w) {
		t.Error("managedBy = true for an Agent with a different workflow UID, want false")
	}

	// An Agent with no management labels at all (a user's hand-made Agent) must not be adopted.
	unlabeled := &unstructured.Unstructured{}
	if managedBy(unlabeled, w) {
		t.Error("managedBy = true for an unlabeled Agent, want false")
	}
}

func newExposureReconciler(t *testing.T, objs ...client.Object) *WorkflowExposureReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("adding client-go scheme: %v", err)
	}
	if err := ottoflowv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding ottoflow scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &WorkflowExposureReconciler{
		Client:                 c,
		Scheme:                 scheme,
		ServeA2AServiceAccount: "serve-a2a",
		ServeA2AClusterRole:    "ottoflow-serve-a2a",
	}
}

func TestReconcileRoleBinding(t *testing.T) {
	desired := func() *rbacv1.RoleBinding {
		return &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: serveA2ARoleName, Namespace: "team-a"},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "ottoflow-serve-a2a"},
			Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: "serve-a2a", Namespace: "team-a"}},
		}
	}

	t.Run("creates when absent", func(t *testing.T) {
		r := newExposureReconciler(t)
		if err := r.reconcileRoleBinding(context.Background(), desired()); err != nil {
			t.Fatalf("reconcileRoleBinding: %v", err)
		}
		var got rbacv1.RoleBinding
		if err := r.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: serveA2ARoleName}, &got); err != nil {
			t.Fatalf("expected the RoleBinding to be created: %v", err)
		}
	})

	t.Run("recreates when roleRef drifts (immutable)", func(t *testing.T) {
		stale := desired()
		stale.RoleRef.Name = "old-clusterrole" // operator changed the configured ClusterRole
		r := newExposureReconciler(t, stale)
		if err := r.reconcileRoleBinding(context.Background(), desired()); err != nil {
			t.Fatalf("reconcileRoleBinding: %v", err)
		}
		var got rbacv1.RoleBinding
		if err := r.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: serveA2ARoleName}, &got); err != nil {
			t.Fatalf("getting RoleBinding: %v", err)
		}
		if got.RoleRef.Name != "ottoflow-serve-a2a" {
			t.Errorf("roleRef.Name = %q, want it repointed to %q", got.RoleRef.Name, "ottoflow-serve-a2a")
		}
	})

	t.Run("updates subjects when only the ServiceAccount drifts", func(t *testing.T) {
		stale := desired()
		stale.Subjects = []rbacv1.Subject{{Kind: "ServiceAccount", Name: "old-sa", Namespace: "team-a"}}
		r := newExposureReconciler(t, stale)
		if err := r.reconcileRoleBinding(context.Background(), desired()); err != nil {
			t.Fatalf("reconcileRoleBinding: %v", err)
		}
		var got rbacv1.RoleBinding
		if err := r.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: serveA2ARoleName}, &got); err != nil {
			t.Fatalf("getting RoleBinding: %v", err)
		}
		if len(got.Subjects) != 1 || got.Subjects[0].Name != "serve-a2a" {
			t.Errorf("subjects = %+v, want the reconciled serve-a2a ServiceAccount", got.Subjects)
		}
	})

	t.Run("no-op when already correct", func(t *testing.T) {
		r := newExposureReconciler(t, desired())
		if err := r.reconcileRoleBinding(context.Background(), desired()); err != nil {
			t.Fatalf("reconcileRoleBinding: %v", err)
		}
		var got rbacv1.RoleBinding
		if err := r.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: serveA2ARoleName}, &got); err != nil {
			t.Fatalf("getting RoleBinding: %v", err)
		}
		if got.RoleRef.Name != "ottoflow-serve-a2a" || len(got.Subjects) != 1 {
			t.Errorf("RoleBinding changed unexpectedly: %+v", got)
		}
	})
}
