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

	"github.com/go-logr/logr/funcr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

const (
	scopeNS = "wf-ns"
	otherNS = "other-ns"
)

func cmRes(ns string) ottoflowv1alpha1.EventResource {
	return ottoflowv1alpha1.EventResource{APIVersion: "v1", Kind: "ConfigMap", Namespace: ns}
}

// The fake client's default REST mapper is empty, so the scope tests supply one.
func newScopeManager(allowed ...string) (*TriggerManager, *fakeWatcherFactory) {
	f := &fakeWatcherFactory{}
	m := meta.NewDefaultRESTMapper(nil)
	m.Add(corev1.SchemeGroupVersion.WithKind("Node"), meta.RESTScopeRoot)
	m.Add(corev1.SchemeGroupVersion.WithKind("ConfigMap"), meta.RESTScopeNamespace)
	tm := NewTriggerManagerWithWatcherFactory(fake.NewClientBuilder().WithScheme(unitTestScheme).WithRESTMapper(m).Build(), unitTestScheme, nil, f)
	tm.SetEventTriggerAllowedNamespaces(allowed)
	return tm, f
}

// Only in-scope resources open a watch; a refused resource never fails the registration or hides its siblings.
func TestEventTriggerScope_Registration(t *testing.T) {
	node := ottoflowv1alpha1.EventResource{APIVersion: "v1", Kind: "Node", Namespace: scopeNS}
	widget := ottoflowv1alpha1.EventResource{APIVersion: "example.com/v1", Kind: "Widget", Namespace: scopeNS}
	cases := []struct {
		name      string
		allowed   []string
		resources []ottoflowv1alpha1.EventResource
		want      []string
	}{
		{"own namespace opens, other is refused", nil, []ottoflowv1alpha1.EventResource{cmRes(scopeNS), cmRes(otherNS)}, []string{scopeNS}},
		{"empty namespace refused", nil, []ottoflowv1alpha1.EventResource{cmRes("")}, nil},
		{"allowlisted opens other and all namespaces", []string{scopeNS}, []ottoflowv1alpha1.EventResource{cmRes(otherNS), cmRes("")}, []string{otherNS, ""}},
		{"cluster-scoped kind refused in own namespace", nil, []ottoflowv1alpha1.EventResource{cmRes(scopeNS), node}, []string{scopeNS}},
		{"unresolvable kind refused in own namespace", nil, []ottoflowv1alpha1.EventResource{cmRes(scopeNS), widget}, []string{scopeNS}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tm, f := newScopeManager(tc.allowed...)
			wf := eventTestWorkflow("wf", ottoflowv1alpha1.Trigger{Event: &ottoflowv1alpha1.EventTrigger{Resources: tc.resources, Operations: []string{"CREATE"}}})
			wf.Namespace = scopeNS
			if err := tm.RegisterWorkflow(context.Background(), wf); err != nil {
				t.Fatalf("RegisterWorkflow must return nil for a refused resource, got %v", err)
			}
			t.Cleanup(func() { _ = tm.UnregisterWorkflow(context.Background(), wf) })
			if !slices.Equal(f.nss, tc.want) {
				t.Fatalf("watched namespaces = %q, want %q", f.nss, tc.want)
			}
		})
	}
}

// A failed watch is still returned, and a later out-of-scope sibling is still logged as refused.
func TestEventTriggerScope_RefusalLoggedAfterWatchFailure(t *testing.T) {
	var lines []string
	ctx := log.IntoContext(context.Background(), funcr.New(func(_, args string) { lines = append(lines, args) }, funcr.Options{}))
	tm, _ := newScopeManager()
	// The invalid field selector fails the in-scope resource's watch before any Watch call.
	wf := eventTestWorkflow("wf", ottoflowv1alpha1.Trigger{Event: &ottoflowv1alpha1.EventTrigger{
		Resources: []ottoflowv1alpha1.EventResource{cmRes(scopeNS), cmRes(otherNS)}, Operations: []string{"CREATE"}, FieldSelector: "bogus",
	}})
	wf.Namespace = scopeNS
	err := tm.RegisterWorkflow(ctx, wf)
	t.Cleanup(func() { _ = tm.UnregisterWorkflow(context.Background(), wf) })
	refusals := 0
	for _, l := range lines {
		if strings.Contains(l, "not watched") && strings.Contains(l, otherNS) {
			refusals++
		}
	}
	if err == nil || refusals != 1 {
		t.Fatalf("err=%v, refusal lines=%d; want a watch error and 1 refusal line", err, refusals)
	}
}

// After the first watch failure, later in-scope resources open no watch.
func TestEventTriggerScope_NoWatchAfterWatchFailure(t *testing.T) {
	tm, f := newScopeManager(scopeNS) // allowlisted, so the malformed apiVersion fails in watchResource
	bad := ottoflowv1alpha1.EventResource{APIVersion: "a/b/c", Kind: "ConfigMap", Namespace: scopeNS}
	wf := eventTestWorkflow("wf", ottoflowv1alpha1.Trigger{Event: &ottoflowv1alpha1.EventTrigger{Resources: []ottoflowv1alpha1.EventResource{bad, cmRes(scopeNS)}, Operations: []string{"CREATE"}}})
	wf.Namespace = scopeNS
	err := tm.RegisterWorkflow(context.Background(), wf)
	t.Cleanup(func() { _ = tm.UnregisterWorkflow(context.Background(), wf) })
	if err == nil || len(f.nss) != 0 {
		t.Fatalf("err=%v, watches=%q; want a watch error and no watches", err, f.nss)
	}
}

// Webhook HMAC Secret refs resolve only in the Workflow's namespace; no Secret read is made for another one.
func TestWebhookFetchSecret_OtherNamespaceRefused(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithInterceptorFuncs(failSecretGet(errors.New("unexpected Secret read"))).Build()
	s := NewWebhookServer(":0", ctrl.Log, c, nil)
	val, err := s.fetchSecret(context.Background(), ottoflowv1alpha1.WebhookSecretRef{Name: "hmac", Namespace: otherNS}, scopeNS)
	if err == nil || val != nil || !strings.Contains(err.Error(), "only in the Workflow's namespace") {
		t.Fatalf("expected refusal before any read; val=%v err=%v", val, err)
	}
}
