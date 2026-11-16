/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package webhook

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

func eventScopeWorkflow(resNS string) *ottoflowv1alpha1.Workflow {
	return &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "team-a"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Triggers: []ottoflowv1alpha1.Trigger{{Event: &ottoflowv1alpha1.EventTrigger{
				Resources: []ottoflowv1alpha1.EventResource{{APIVersion: "v1", Kind: "ConfigMap", Namespace: resNS}},
			}}},
		},
	}
}

// admission applies the event-trigger watch scope on create, and on update only when triggers change.
func TestWorkflowValidator_EventTriggerScope(t *testing.T) {
	ctx := context.Background()
	v := &WorkflowValidator{}
	if _, err := v.ValidateCreate(ctx, eventScopeWorkflow("team-b")); err == nil {
		t.Error("create with another namespace should be rejected")
	}
	if _, err := v.ValidateCreate(ctx, eventScopeWorkflow("")); err == nil {
		t.Error("create with an empty namespace should be rejected")
	}
	if _, err := v.ValidateCreate(ctx, eventScopeWorkflow("team-a")); err != nil {
		t.Errorf("same-namespace create rejected: %v", err)
	}
	allow := &WorkflowValidator{EventTriggerAllowedNamespaces: []string{"team-a"}}
	if _, err := allow.ValidateCreate(ctx, eventScopeWorkflow("team-b")); err != nil {
		t.Errorf("allowlisted create rejected: %v", err)
	}

	stored := eventScopeWorkflow("team-b")
	changed := stored.DeepCopy()
	changed.Labels = map[string]string{"a": "b"}
	if _, err := v.ValidateUpdate(ctx, stored, changed); err != nil {
		t.Errorf("update leaving triggers unchanged should pass: %v", err)
	}
	changed.Spec.Triggers[0].Event.Resources[0].Namespace = "team-c"
	if _, err := v.ValidateUpdate(ctx, stored, changed); err == nil {
		t.Error("update changing triggers to an out-of-scope namespace should be rejected")
	}
}

// admission rejects a cluster-scoped kind even in the Workflow's namespace, unless allowlisted.
func TestWorkflowValidator_ClusterScopedEventKind(t *testing.T) {
	ctx := context.Background()
	m := meta.NewDefaultRESTMapper(nil)
	m.Add(corev1.SchemeGroupVersion.WithKind("Node"), meta.RESTScopeRoot)
	m.Add(corev1.SchemeGroupVersion.WithKind("ConfigMap"), meta.RESTScopeNamespace)
	c := fake.NewClientBuilder().WithRESTMapper(m).Build()
	if _, err := (&WorkflowValidator{Client: c}).ValidateCreate(ctx, eventScopeWorkflow("team-a")); err != nil {
		t.Errorf("same-namespace namespaced kind rejected: %v", err)
	}
	node := eventScopeWorkflow("team-a")
	node.Spec.Triggers[0].Event.Resources[0].Kind = "Node"
	if _, err := (&WorkflowValidator{Client: c}).ValidateCreate(ctx, node); err == nil {
		t.Error("cluster-scoped kind in the Workflow namespace should be rejected")
	}
	allow := &WorkflowValidator{Client: c, EventTriggerAllowedNamespaces: []string{"team-a"}}
	if _, err := allow.ValidateCreate(ctx, node); err != nil {
		t.Errorf("allowlisted cluster-scoped kind rejected: %v", err)
	}
}

// admission rejects unresolvable and malformed kinds, even in the Workflow's namespace, unless allowlisted.
func TestWorkflowValidator_UnresolvableOrMalformedEventKind(t *testing.T) {
	ctx := context.Background()
	m := meta.NewDefaultRESTMapper(nil)
	m.Add(corev1.SchemeGroupVersion.WithKind("ConfigMap"), meta.RESTScopeNamespace)
	c := fake.NewClientBuilder().WithRESTMapper(m).Build()
	for _, kind := range []string{"Widget", "Config/Map"} {
		w := eventScopeWorkflow("team-a")
		w.Spec.Triggers[0].Event.Resources[0].Kind = kind
		for _, v := range []*WorkflowValidator{{Client: c}, {}} {
			if _, err := v.ValidateCreate(ctx, w); err == nil && (kind != "Widget" || v.Client != nil) {
				t.Errorf("kind %q should be rejected", kind)
			}
		}
		allow := &WorkflowValidator{Client: c, EventTriggerAllowedNamespaces: []string{"team-a"}}
		if _, err := allow.ValidateCreate(ctx, w); err != nil {
			t.Errorf("allowlisted kind %q rejected: %v", kind, err)
		}
	}
}
