/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"

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
