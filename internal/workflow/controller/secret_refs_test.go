/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

func newSecretRefsTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(ottoflowv1alpha1.AddToScheme(s))
	return s
}

func newSecretRefsTestClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(newSecretRefsTestScheme(t)).WithObjects(objs...).Build()
}

// testWorkflowRun builds a WorkflowRun named "run1" in "ns1" — every test in this file uses
// those, so only the referenced Workflow's name varies.
func testWorkflowRun(workflowName string) *ottoflowv1alpha1.WorkflowRun {
	const namespace, name = "ns1", "run1"
	return &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: ottoflowv1alpha1.WorkflowRunSpec{
			WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: workflowName, Namespace: namespace},
		},
	}
}

// refKeys renders refs as sorted "namespace/name/key" strings for easy comparison.
func refKeys(refs []resolvedSecretRef) []string {
	out := make([]string, len(refs))
	for i, r := range refs {
		out[i] = r.Namespace + "/" + r.Name + "/" + r.Key
	}
	sort.Strings(out)
	return out
}

func containsKey(refs []resolvedSecretRef, key string) bool {
	for _, k := range refKeys(refs) {
		if k == key {
			return true
		}
	}
	return false
}

func TestCollectSecretRefs_TopLevelExternalAgentRef(t *testing.T) {
	run := testWorkflowRun("wf1")
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "wf1"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{
				{
					Name: "callAgent",
					ExternalAgentRef: &ottoflowv1alpha1.StepExternalAgentRef{
						URL:    "https://agent.example.com",
						Prompt: "'hi'",
						CASecretRef: &ottoflowv1alpha1.NamespacedSecretRef{
							Name: "agent-ca",
						},
						Auth: &ottoflowv1alpha1.ExternalAgentAuth{
							SecretRef: &ottoflowv1alpha1.SecretReference{Name: "agent-token", Key: "token"},
						},
					},
				},
			},
		},
	}
	c := newSecretRefsTestClient(t)
	refs, err := collectSecretRefs(context.Background(), c, wf, run)
	if err != nil {
		t.Fatalf("collectSecretRefs: %v", err)
	}
	if !containsKey(refs, "ns1/agent-ca/ca.crt") {
		t.Errorf("expected CA ref, got %v", refKeys(refs))
	}
	if !containsKey(refs, "ns1/agent-token/token") {
		t.Errorf("expected auth token ref, got %v", refKeys(refs))
	}
}

func TestCollectSecretRefs_ForEachInlineStep(t *testing.T) {
	run := testWorkflowRun("wf1")
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "wf1"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{
				{
					Name: "fanOut",
					ForEach: &ottoflowv1alpha1.StepForEach{
						Items: "[1,2]",
						Step: &ottoflowv1alpha1.StepForEachStep{
							ExternalAgentRef: &ottoflowv1alpha1.StepExternalAgentRef{
								URL:    "https://agent.example.com",
								Prompt: "'hi'",
								CASecretRef: &ottoflowv1alpha1.NamespacedSecretRef{
									Name: "inline-ca",
								},
							},
						},
					},
				},
			},
		},
	}
	c := newSecretRefsTestClient(t)
	refs, err := collectSecretRefs(context.Background(), c, wf, run)
	if err != nil {
		t.Fatalf("collectSecretRefs: %v", err)
	}
	if !containsKey(refs, "ns1/inline-ca/ca.crt") {
		t.Errorf("expected inline forEach CA ref, got %v", refKeys(refs))
	}
}

func TestCollectSecretRefs_ForEachStepTemplateRef(t *testing.T) {
	run := testWorkflowRun("wf1")
	tpl := &ottoflowv1alpha1.StepTemplate{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "tpl1"},
		Spec: ottoflowv1alpha1.StepTemplateSpec{
			Step: ottoflowv1alpha1.StepTemplateStep{
				ExternalAgentRef: &ottoflowv1alpha1.StepExternalAgentRef{
					URL:    "https://agent.example.com",
					Prompt: "'hi'",
					CASecretRef: &ottoflowv1alpha1.NamespacedSecretRef{
						Name: "tpl-ca",
					},
				},
			},
		},
	}
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "wf1"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{
				{
					Name: "fanOut",
					ForEach: &ottoflowv1alpha1.StepForEach{
						Items: "[1,2]",
						StepTemplateRef: &ottoflowv1alpha1.StepForEachTemplateRef{
							Name: "tpl1",
						},
					},
				},
			},
		},
	}
	c := newSecretRefsTestClient(t, tpl)
	refs, err := collectSecretRefs(context.Background(), c, wf, run)
	if err != nil {
		t.Fatalf("collectSecretRefs: %v", err)
	}
	if !containsKey(refs, "ns1/tpl-ca/ca.crt") {
		t.Errorf("expected forEach stepTemplateRef CA ref, got %v", refKeys(refs))
	}
}

func TestCollectSecretRefs_TopLevelStepTemplateRef(t *testing.T) {
	run := testWorkflowRun("wf1")
	tpl := &ottoflowv1alpha1.StepTemplate{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "tpl1"},
		Spec: ottoflowv1alpha1.StepTemplateSpec{
			Step: ottoflowv1alpha1.StepTemplateStep{
				ExternalAgentRef: &ottoflowv1alpha1.StepExternalAgentRef{
					URL:    "https://agent.example.com",
					Prompt: "'hi'",
					CASecretRef: &ottoflowv1alpha1.NamespacedSecretRef{
						Name: "top-tpl-ca",
					},
				},
			},
		},
	}
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "wf1"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{
				{
					Name:            "useTpl",
					StepTemplateRef: &ottoflowv1alpha1.StepTemplateRef{Name: "tpl1"},
				},
			},
		},
	}
	c := newSecretRefsTestClient(t, tpl)
	refs, err := collectSecretRefs(context.Background(), c, wf, run)
	if err != nil {
		t.Fatalf("collectSecretRefs: %v", err)
	}
	if !containsKey(refs, "ns1/top-tpl-ca/ca.crt") {
		t.Errorf("expected top-level stepTemplateRef CA ref, got %v", refKeys(refs))
	}
}

func TestCollectSecretRefs_WorkflowRefOneLevel(t *testing.T) {
	run := testWorkflowRun("wf1")
	sub := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "sub1"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{
				{
					Name: "innerCall",
					ExternalAgentRef: &ottoflowv1alpha1.StepExternalAgentRef{
						URL:    "https://agent.example.com",
						Prompt: "'hi'",
						CASecretRef: &ottoflowv1alpha1.NamespacedSecretRef{
							Name: "sub-ca",
						},
					},
				},
			},
		},
	}
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "wf1"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{
				{
					Name:        "callSub",
					WorkflowRef: &ottoflowv1alpha1.StepWorkflowRef{Name: "sub1"},
				},
			},
		},
	}
	c := newSecretRefsTestClient(t, sub)
	refs, err := collectSecretRefs(context.Background(), c, wf, run)
	if err != nil {
		t.Fatalf("collectSecretRefs: %v", err)
	}
	if !containsKey(refs, "ns1/sub-ca/ca.crt") {
		t.Errorf("expected 1-level workflowRef CA ref, got %v", refKeys(refs))
	}
}

func TestCollectSecretRefs_WorkflowRefTwoLevels(t *testing.T) {
	run := testWorkflowRun("wf1")
	subsub := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "subsub1"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{
				{
					Name: "deepCall",
					ExternalAgentRef: &ottoflowv1alpha1.StepExternalAgentRef{
						URL:    "https://agent.example.com",
						Prompt: "'hi'",
						CASecretRef: &ottoflowv1alpha1.NamespacedSecretRef{
							Name: "deep-ca",
						},
					},
				},
			},
		},
	}
	sub := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "sub1"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{
				{
					Name:        "callSubSub",
					WorkflowRef: &ottoflowv1alpha1.StepWorkflowRef{Name: "subsub1"},
				},
			},
		},
	}
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "wf1"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{
				{
					Name:        "callSub",
					WorkflowRef: &ottoflowv1alpha1.StepWorkflowRef{Name: "sub1"},
				},
			},
		},
	}
	c := newSecretRefsTestClient(t, sub, subsub)
	refs, err := collectSecretRefs(context.Background(), c, wf, run)
	if err != nil {
		t.Fatalf("collectSecretRefs: %v", err)
	}
	if !containsKey(refs, "ns1/deep-ca/ca.crt") {
		t.Errorf("expected 2-level workflowRef CA ref, got %v", refKeys(refs))
	}
}

func TestCollectSecretRefs_WorkflowRefCycleTerminates(t *testing.T) {
	run := testWorkflowRun("wfA")
	wfA := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "wfA"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{
				{
					Name: "callB",
					ExternalAgentRef: &ottoflowv1alpha1.StepExternalAgentRef{
						URL:    "https://agent.example.com",
						Prompt: "'hi'",
						CASecretRef: &ottoflowv1alpha1.NamespacedSecretRef{
							Name: "a-ca",
						},
					},
					WorkflowRef: &ottoflowv1alpha1.StepWorkflowRef{Name: "wfB"},
				},
			},
		},
	}
	wfB := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "wfB"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{
				{
					Name:        "callBackToA",
					WorkflowRef: &ottoflowv1alpha1.StepWorkflowRef{Name: "wfA"},
				},
			},
		},
	}
	c := newSecretRefsTestClient(t, wfB)

	done := make(chan struct{})
	var refs []resolvedSecretRef
	var err error
	go func() {
		refs, err = collectSecretRefs(context.Background(), c, wfA, run)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("collectSecretRefs did not terminate on an A->B->A workflowRef cycle")
	}
	if err != nil {
		t.Fatalf("collectSecretRefs: %v", err)
	}
	if !containsKey(refs, "ns1/a-ca/ca.crt") {
		t.Errorf("expected wfA's own CA ref to still be collected, got %v", refKeys(refs))
	}
}

func TestCollectSecretRefs_MCPToolCallAllAuthTypes(t *testing.T) {
	envServer := &ottoflowv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "env-server"},
		Spec: ottoflowv1alpha1.MCPServerSpec{
			Transport: ottoflowv1alpha1.TransportConfig{Type: "stdio", Command: []string{"foo"}},
			Env: []corev1.EnvVar{
				{
					Name: "API_TOKEN",
					ValueFrom: &corev1.EnvVarSource{
						SecretKeyRef: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: "env-secret"},
							Key:                  "token",
						},
					},
				},
			},
		},
	}
	bearerServer := &ottoflowv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "bearer-server"},
		Spec: ottoflowv1alpha1.MCPServerSpec{
			Transport: ottoflowv1alpha1.TransportConfig{Type: "http", Address: "http://x"},
			Auth: &ottoflowv1alpha1.AuthConfig{
				Type:      "bearer",
				SecretRef: &ottoflowv1alpha1.SecretReference{Name: "bearer-secret", Key: "token"},
			},
		},
	}
	apiKeyServer := &ottoflowv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "apikey-server"},
		Spec: ottoflowv1alpha1.MCPServerSpec{
			Transport: ottoflowv1alpha1.TransportConfig{Type: "http", Address: "http://x"},
			Auth: &ottoflowv1alpha1.AuthConfig{
				Type:      "apiKey",
				SecretRef: &ottoflowv1alpha1.SecretReference{Name: "apikey-secret", Key: "key"},
			},
		},
	}
	basicServer := &ottoflowv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "basic-server"},
		Spec: ottoflowv1alpha1.MCPServerSpec{
			Transport: ottoflowv1alpha1.TransportConfig{Type: "http", Address: "http://x"},
			Auth: &ottoflowv1alpha1.AuthConfig{
				Type: "basic",
				// Key is deliberately something the runner never reads (resolveAuthConfigs
				// in internal/agent/mcp_client_impl.go hardcodes "username"/"password" for
				// basic auth, per AuthConfig's own doc comment) — proves the walk mounts
				// the two fixed keys regardless of what this field says, not this one.
				SecretRef: &ottoflowv1alpha1.SecretReference{Name: "basic-secret", Key: "ignored-by-runner"},
			},
		},
	}
	oauthServer := &ottoflowv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "oauth-server"},
		Spec: ottoflowv1alpha1.MCPServerSpec{
			Transport: ottoflowv1alpha1.TransportConfig{Type: "http", Address: "http://x"},
			Auth: &ottoflowv1alpha1.AuthConfig{
				Type: "oauth2",
				OAuth2: &ottoflowv1alpha1.OAuth2Config{
					TokenURL:             "https://token.example.com",
					ClientSecretRef:      &ottoflowv1alpha1.SecretReference{Name: "oauth-client-secret", Key: "client_secret"},
					ClientCredentialsRef: &ottoflowv1alpha1.NamespacedSecretRef{Name: "oauth-creds"},
				},
			},
		},
	}

	servers := []string{"env-server", "bearer-server", "apikey-server", "basic-server", "oauth-server"}
	steps := make([]ottoflowv1alpha1.Step, 0, len(servers))
	for i, s := range servers {
		steps = append(steps, ottoflowv1alpha1.Step{
			Name: "call" + s[:1] + string(rune('A'+i)),
			MCPToolCall: &ottoflowv1alpha1.StepMCPToolCall{
				Server: s,
				Tool:   "doThing",
			},
		})
	}
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "wf1"},
		Spec:       ottoflowv1alpha1.WorkflowSpec{Steps: steps},
	}
	run := testWorkflowRun("wf1")
	c := newSecretRefsTestClient(t, envServer, bearerServer, apiKeyServer, basicServer, oauthServer)
	refs, err := collectSecretRefs(context.Background(), c, wf, run)
	if err != nil {
		t.Fatalf("collectSecretRefs: %v", err)
	}
	want := []string{
		"ns1/env-secret/token",
		"ns1/bearer-secret/token",
		"ns1/apikey-secret/key",
		// basic auth mounts BOTH fixed keys the runner reads (resolveAuthConfigs
		// hardcodes "username"/"password"), not the single key SecretRef.Key names.
		"ns1/basic-secret/username",
		"ns1/basic-secret/password",
		"ns1/oauth-client-secret/client_secret",
		"ns1/oauth-creds/client_id",
		"ns1/oauth-creds/client_secret",
	}
	for _, k := range want {
		if !containsKey(refs, k) {
			t.Errorf("expected ref %s, got %v", k, refKeys(refs))
		}
	}
	// The basic-server SecretRef's OWN Key ("ignored-by-runner") must never be
	// mounted — only the two fixed keys the runner actually reads.
	if containsKey(refs, "ns1/basic-secret/ignored-by-runner") {
		t.Errorf("basic auth must not mount SecretRef.Key verbatim, got %v", refKeys(refs))
	}
	for _, r := range refs {
		if r.EnvVarName == "API_TOKEN" && !r.NeedsRunnerMount {
			t.Errorf("expected mcpToolCall-reached env ref to have NeedsRunnerMount=true")
		}
	}
}

func TestCollectSecretRefs_AgentRefMCPTools(t *testing.T) {
	server := &ottoflowv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "agent-mcp-server"},
		Spec: ottoflowv1alpha1.MCPServerSpec{
			Transport: ottoflowv1alpha1.TransportConfig{Type: "http", Address: "http://x"},
			Auth: &ottoflowv1alpha1.AuthConfig{
				Type:      "bearer",
				SecretRef: &ottoflowv1alpha1.SecretReference{Name: "agent-mcp-secret", Key: "token"},
			},
		},
	}
	agentCRD := &ottoflowv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "agent1"},
		Spec: ottoflowv1alpha1.AgentSpec{
			Prompt:        "do stuff",
			ModelProvider: "openai",
			MCPTools:      []string{"agent-mcp-server:doThing"},
		},
	}
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "wf1"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{
				{
					Name:     "runAgent",
					AgentRef: &ottoflowv1alpha1.StepAgentRef{Name: "agent1"},
				},
			},
		},
	}
	run := testWorkflowRun("wf1")
	c := newSecretRefsTestClient(t, server, agentCRD)
	refs, err := collectSecretRefs(context.Background(), c, wf, run)
	if err != nil {
		t.Fatalf("collectSecretRefs: %v", err)
	}
	if !containsKey(refs, "ns1/agent-mcp-secret/token") {
		t.Errorf("expected agentRef mcpTools ref, got %v", refKeys(refs))
	}
	for _, r := range refs {
		if r.Name == "agent-mcp-secret" && r.NeedsRunnerMount {
			t.Errorf("agentRef-reached MCP secret must NOT be mounted into the runner (handled by agent-executor)")
		}
	}
}

func TestCollectSecretRefs_MissingCRFailsClosed(t *testing.T) {
	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "wf1"},
		Spec: ottoflowv1alpha1.WorkflowSpec{
			Steps: []ottoflowv1alpha1.Step{
				{
					Name:            "useTpl",
					StepTemplateRef: &ottoflowv1alpha1.StepTemplateRef{Name: "does-not-exist"},
				},
			},
		},
	}
	run := testWorkflowRun("wf1")
	c := newSecretRefsTestClient(t)
	_, err := collectSecretRefs(context.Background(), c, wf, run)
	if err == nil {
		t.Fatal("expected an error for a missing StepTemplate CR, got nil")
	}
	if !strings.Contains(err.Error(), "re-run the WorkflowRun") {
		t.Errorf("expected error to instruct re-running to re-resolve, got: %v", err)
	}
}

func TestValidateSecretRefPolicy(t *testing.T) {
	sameNS := []resolvedSecretRef{{Namespace: "ns1", Name: "s", Key: "k", Origin: "test"}}
	if err := validateSecretRefPolicy(sameNS, "ns1", RunnerConfig{}); err != nil {
		t.Errorf("same-namespace ref should pass, got: %v", err)
	}

	crossNS := []resolvedSecretRef{{Namespace: "ns2", Name: "s", Key: "k", Origin: "test"}}
	if err := validateSecretRefPolicy(crossNS, "ns1", RunnerConfig{}); err == nil {
		t.Error("cross-namespace ref without an allowlist should be denied")
	}

	allowlisted := RunnerConfig{SecretRefAllowedNamespaces: map[string]struct{}{"ns2": {}}}
	if err := validateSecretRefPolicy(crossNS, "ns1", allowlisted); err != nil {
		t.Errorf("cross-namespace ref in the allowlist should pass policy, got: %v", err)
	}
}

// TestValidateSecretRefPolicy_NeedsRunnerMountNeverHelpedByAllowlist proves that a ref which
// needs to be mounted into the runner Job (kubeconfig, externalAgentRef CA/auth, a
// directly-called MCPServer's auth) is rejected even when its namespace IS in the operator
// allowlist, because buildSecretMounts has no cross-namespace mounting mechanism for these
// regardless — the allowlist can only ever help a ref collectSecretRefs marks
// NeedsRunnerMount==false. Without this branch, such a ref would pass this policy check only
// to fail later, deeper in buildSecretMounts, with a less specific error.
func TestValidateSecretRefPolicy_NeedsRunnerMountNeverHelpedByAllowlist(t *testing.T) {
	crossNSMounted := []resolvedSecretRef{{Namespace: "ns2", Name: "s", Key: "k", Origin: "test", NeedsRunnerMount: true}}
	allowlisted := RunnerConfig{SecretRefAllowedNamespaces: map[string]struct{}{"ns2": {}}}

	err := validateSecretRefPolicy(crossNSMounted, "ns1", allowlisted)
	if err == nil {
		t.Fatal("expected a cross-namespace NeedsRunnerMount ref to be rejected even when allowlisted")
	}
	if strings.Contains(err.Error(), "grant the runner ServiceAccount a Role+RoleBinding") {
		t.Errorf("error should not suggest adding to the allowlist as a fix for a NeedsRunnerMount ref, got: %v", err)
	}
	if !strings.Contains(err.Error(), "native Kubernetes Secret volume") {
		t.Errorf("error should explain why the allowlist cannot help, got: %v", err)
	}
}
