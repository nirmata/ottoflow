/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package chart

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

// writeSecretAccessValues writes a temporary Helm values file under t.TempDir() (auto-cleaned)
// and returns its path.
func writeSecretAccessValues(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secret-access-values.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("failed to write temp values file: %v", err)
	}
	return path
}

// renderWithValuesFile renders the "ottoflow" release (the expected resource names below are
// written for it), optionally layering an extra values file on top of the chart defaults.
// Thin wrapper over the shared helmTemplate helper (runner_rbac_render_test.go).
func renderWithValuesFile(t *testing.T, valuesFile string) (string, error) {
	t.Helper()
	return helmTemplate(t, "ottoflow", valuesFile)
}

// secretAccessDocs holds the secret-access Roles and RoleBindings found in a rendered chart
// (rbac-secret-access-role.yaml — every document it emits has "-secret-access" in its name).
type secretAccessDocs struct {
	roles        []rbacv1.Role
	roleBindings []rbacv1.RoleBinding
}

// decodeSecretAccessDocs splits output into YAML documents and keeps only the Role/RoleBinding
// documents belonging to rbac-secret-access-role.yaml, identified by name containing
// "secret-access" (distinguishing them from unrelated Roles like the cert-manager or leader-
// election Roles the chart also renders).
func decodeSecretAccessDocs(t *testing.T, output string) secretAccessDocs {
	t.Helper()
	var docs secretAccessDocs
	for _, doc := range splitYAMLDocs(output) {
		doc = strings.TrimSpace(doc)
		if doc == "" {
			continue
		}
		var meta struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(doc), &meta); err != nil {
			continue
		}
		if !strings.Contains(meta.Metadata.Name, "secret-access") {
			continue
		}
		switch meta.Kind {
		case "Role":
			var r rbacv1.Role
			if err := yaml.Unmarshal([]byte(doc), &r); err == nil {
				docs.roles = append(docs.roles, r)
			}
		case "RoleBinding":
			var rb rbacv1.RoleBinding
			if err := yaml.Unmarshal([]byte(doc), &rb); err == nil {
				docs.roleBindings = append(docs.roleBindings, rb)
			}
		}
	}
	return docs
}

// TestSecretAccessRole_DefaultValues_RendersNothing verifies OttoFlow's "zero Secret access by
// default" claim (docs/user/rbac-secret-access.md): with no rbac.secretAccess.* opt-in, the chart
// must render no secret-access Role or RoleBinding at all.
func TestSecretAccessRole_DefaultValues_RendersNothing(t *testing.T) {
	output, err := renderWithValuesFile(t, "")
	if err != nil {
		t.Fatalf("helm template failed: %v", err)
	}
	docs := decodeSecretAccessDocs(t, output)
	if len(docs.roles) != 0 {
		t.Errorf("expected no secret-access Role by default, got %d: %+v", len(docs.roles), docs.roles)
	}
	if len(docs.roleBindings) != 0 {
		t.Errorf("expected no secret-access RoleBinding by default, got %d: %+v", len(docs.roleBindings), docs.roleBindings)
	}
}

// TestSecretAccessRole_ControllerEnabled_RendersRoleAndBinding verifies that opting in
// rbac.secretAccess.controller renders exactly one Role and one RoleBinding for the controller
// component, and nothing for agentExecutor (which was left unset).
func TestSecretAccessRole_ControllerEnabled_RendersRoleAndBinding(t *testing.T) {
	values := writeSecretAccessValues(t, `
rbac:
  secretAccess:
    controller:
      - namespace: team-a
        secretNames:
          - cred-a
          - cred-b
`)
	output, err := renderWithValuesFile(t, values)
	if err != nil {
		t.Fatalf("helm template failed: %v", err)
	}
	docs := decodeSecretAccessDocs(t, output)

	if len(docs.roles) != 1 {
		t.Fatalf("expected exactly 1 secret-access Role (controller only), got %d: %+v", len(docs.roles), docs.roles)
	}
	if len(docs.roleBindings) != 1 {
		t.Fatalf("expected exactly 1 secret-access RoleBinding (controller only), got %d: %+v",
			len(docs.roleBindings), docs.roleBindings)
	}

	role := docs.roles[0]
	if role.Name != "ottoflow-controller-secret-access" {
		t.Errorf("Role.Name = %q, want %q", role.Name, "ottoflow-controller-secret-access")
	}
	if role.Namespace != "team-a" {
		t.Errorf("Role.Namespace = %q, want %q", role.Namespace, "team-a")
	}
}

// TestSecretAccessRole_ExactRulesAndSubjects renders BOTH controller and agentExecutor
// secretAccess and asserts the exact resourceNames on each Role and the exact subjects on each
// RoleBinding — full slice comparison, not Contains/suffix checks, since a least-privilege grant
// silently widening (an extra resourceName, an extra subject) is exactly the kind of regression
// substring assertions would miss.
func TestSecretAccessRole_ExactRulesAndSubjects(t *testing.T) {
	values := writeSecretAccessValues(t, `
rbac:
  secretAccess:
    controller:
      - namespace: team-a
        secretNames:
          - cred-a
          - cred-b
    agentExecutor:
      - namespace: team-b
        secretNames:
          - openai-key
`)
	output, err := renderWithValuesFile(t, values)
	if err != nil {
		t.Fatalf("helm template failed: %v", err)
	}
	docs := decodeSecretAccessDocs(t, output)

	if len(docs.roles) != 2 {
		t.Fatalf("expected exactly 2 secret-access Roles, got %d: %+v", len(docs.roles), docs.roles)
	}
	if len(docs.roleBindings) != 2 {
		t.Fatalf("expected exactly 2 secret-access RoleBindings, got %d: %+v", len(docs.roleBindings), docs.roleBindings)
	}

	rolesByName := make(map[string]rbacv1.Role, len(docs.roles))
	for _, r := range docs.roles {
		rolesByName[r.Name] = r
	}
	bindingsByName := make(map[string]rbacv1.RoleBinding, len(docs.roleBindings))
	for _, rb := range docs.roleBindings {
		bindingsByName[rb.Name] = rb
	}

	// --- controller Role: exact rule shape and exact resourceNames ---
	ctrlRole, ok := rolesByName["ottoflow-controller-secret-access"]
	if !ok {
		t.Fatalf("missing Role %q, got roles: %v", "ottoflow-controller-secret-access", rolesByName)
	}
	if ctrlRole.Namespace != "team-a" {
		t.Errorf("controller Role.Namespace = %q, want %q", ctrlRole.Namespace, "team-a")
	}
	if len(ctrlRole.Rules) != 1 {
		t.Fatalf("controller Role: expected exactly 1 rule, got %d: %+v", len(ctrlRole.Rules), ctrlRole.Rules)
	}
	ctrlRule := ctrlRole.Rules[0]
	if want := []string{""}; !stringSlicesEqual(ctrlRule.APIGroups, want) {
		t.Errorf("controller Role rule APIGroups = %v, want %v", ctrlRule.APIGroups, want)
	}
	if want := []string{"secrets"}; !stringSlicesEqual(ctrlRule.Resources, want) {
		t.Errorf("controller Role rule Resources = %v, want %v", ctrlRule.Resources, want)
	}
	if want := []string{"get"}; !stringSlicesEqual(ctrlRule.Verbs, want) {
		t.Errorf("controller Role rule Verbs = %v, want %v", ctrlRule.Verbs, want)
	}
	if want := []string{"cred-a", "cred-b"}; !stringSlicesEqual(ctrlRule.ResourceNames, want) {
		t.Errorf("controller Role rule ResourceNames = %v, want %v (exact match, not a subset check)",
			ctrlRule.ResourceNames, want)
	}

	// --- controller RoleBinding: exact roleRef and exact subjects ---
	ctrlBinding, ok := bindingsByName["ottoflow-controller-secret-access"]
	if !ok {
		t.Fatalf("missing RoleBinding %q, got bindings: %v", "ottoflow-controller-secret-access", bindingsByName)
	}
	wantCtrlRoleRef := rbacv1.RoleRef{
		APIGroup: "rbac.authorization.k8s.io",
		Kind:     "Role",
		Name:     "ottoflow-controller-secret-access",
	}
	if ctrlBinding.RoleRef != wantCtrlRoleRef {
		t.Errorf("controller RoleBinding.RoleRef = %+v, want %+v", ctrlBinding.RoleRef, wantCtrlRoleRef)
	}
	wantCtrlSubjects := []rbacv1.Subject{{Kind: "ServiceAccount", Name: "controller-manager", Namespace: "ottoflow"}}
	if !subjectSlicesEqual(ctrlBinding.Subjects, wantCtrlSubjects) {
		t.Errorf("controller RoleBinding.Subjects = %+v, want %+v (exact match)", ctrlBinding.Subjects, wantCtrlSubjects)
	}

	// --- agentExecutor Role: exact resourceNames ---
	aeRole, ok := rolesByName["ottoflow-agent-executor-secret-access"]
	if !ok {
		t.Fatalf("missing Role %q, got roles: %v", "ottoflow-agent-executor-secret-access", rolesByName)
	}
	if aeRole.Namespace != "team-b" {
		t.Errorf("agentExecutor Role.Namespace = %q, want %q", aeRole.Namespace, "team-b")
	}
	if len(aeRole.Rules) != 1 {
		t.Fatalf("agentExecutor Role: expected exactly 1 rule, got %d: %+v", len(aeRole.Rules), aeRole.Rules)
	}
	if want := []string{"openai-key"}; !stringSlicesEqual(aeRole.Rules[0].ResourceNames, want) {
		t.Errorf("agentExecutor Role rule ResourceNames = %v, want %v (exact match, not a subset check)",
			aeRole.Rules[0].ResourceNames, want)
	}

	// --- agentExecutor RoleBinding: exact subjects ---
	aeBinding, ok := bindingsByName["ottoflow-agent-executor-secret-access"]
	if !ok {
		t.Fatalf("missing RoleBinding %q, got bindings: %v", "ottoflow-agent-executor-secret-access", bindingsByName)
	}
	wantAESubjects := []rbacv1.Subject{{Kind: "ServiceAccount", Name: "ottoflow-agent-executor", Namespace: "ottoflow"}}
	if !subjectSlicesEqual(aeBinding.Subjects, wantAESubjects) {
		t.Errorf("agentExecutor RoleBinding.Subjects = %+v, want %+v (exact match)", aeBinding.Subjects, wantAESubjects)
	}
}

// TestSecretAccessRole_InstallNamespace_FailsRender verifies the template's install-namespace
// guard: when rbac.secretAccess.controller.secretNames is set but its namespace resolves to the
// install namespace (the default when .namespace is left empty), the template calls `fail` and
// `helm template` must error out, naming the install namespace in the message so an operator
// knows what to change.
func TestSecretAccessRole_InstallNamespace_FailsRender(t *testing.T) {
	values := writeSecretAccessValues(t, `
rbac:
  secretAccess:
    controller:
      - namespace: ""
        secretNames:
          - cred-a
`)
	_, err := renderWithValuesFile(t, values)
	if err == nil {
		t.Fatal("expected helm template to fail when secretAccess resolves to the install namespace, got success")
	}
	if !strings.Contains(err.Error(), "install namespace") {
		t.Errorf("expected error to mention the install namespace, got: %v", err)
	}
	if !strings.Contains(err.Error(), `"ottoflow"`) {
		t.Errorf("expected error to name the resolved install namespace %q, got: %v", "ottoflow", err)
	}
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func subjectSlicesEqual(a, b []rbacv1.Subject) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestSecretAccessRole_MultipleTenantNamespaces verifies the list form: one release grants one
// component access in several tenant namespaces at once (the reason secretAccess.<component> is
// a list, not a single object), rendering one same-named Role+RoleBinding per namespace.
func TestSecretAccessRole_MultipleTenantNamespaces(t *testing.T) {
	values := writeSecretAccessValues(t, `
rbac:
  secretAccess:
    controller:
      - namespace: team-a
        secretNames:
          - cred-a
      - namespace: team-b
        secretNames:
          - cred-b
`)
	output, err := renderWithValuesFile(t, values)
	if err != nil {
		t.Fatalf("helm template failed: %v", err)
	}
	docs := decodeSecretAccessDocs(t, output)

	if len(docs.roles) != 2 || len(docs.roleBindings) != 2 {
		t.Fatalf("expected 2 Roles and 2 RoleBindings (one per tenant namespace), got %d/%d",
			len(docs.roles), len(docs.roleBindings))
	}
	wantNames := map[string][]string{"team-a": {"cred-a"}, "team-b": {"cred-b"}}
	for _, r := range docs.roles {
		want, ok := wantNames[r.Namespace]
		if !ok {
			t.Errorf("unexpected Role namespace %q", r.Namespace)
			continue
		}
		if len(r.Rules) != 1 || !stringSlicesEqual(r.Rules[0].ResourceNames, want) {
			t.Errorf("Role in %q: ResourceNames = %v, want %v", r.Namespace, r.Rules[0].ResourceNames, want)
		}
		delete(wantNames, r.Namespace)
	}
	if len(wantNames) != 0 {
		t.Errorf("missing Roles for namespaces: %v", wantNames)
	}
}

// TestSecretAccessRole_DuplicateNamespace_FailsRender: two entries naming the same namespace for
// one component would render same-named Roles that silently last-write-win at apply time; the
// template must refuse and tell the operator to merge them.
func TestSecretAccessRole_DuplicateNamespace_FailsRender(t *testing.T) {
	values := writeSecretAccessValues(t, `
rbac:
  secretAccess:
    controller:
      - namespace: team-a
        secretNames:
          - cred-a
      - namespace: team-a
        secretNames:
          - cred-b
`)
	_, err := renderWithValuesFile(t, values)
	if err == nil {
		t.Fatal("expected helm template to fail on a duplicate namespace entry, got success")
	}
	if !strings.Contains(err.Error(), "more than once") {
		t.Errorf("expected the duplicate-namespace message, got: %v", err)
	}
}

// TestNoClusterRoleGrantsSecrets pins the chart half of "Secret access starts at zero"
// (docs/user/rbac-secret-access.md): no ClusterRole the chart renders, for any release name, grants
// any verb on secrets - not the controller's, not the runner's, not the agent-executor's, not an
// aggregated fragment. TestRunnerRoleLacksEscalationVerbs only inspects roles labelled
// aggregate-to-runner, so on its own it would not notice a secrets rule re-added to a
// controller-only fragment such as *-role:core, which is exactly where the cluster-wide grant
// this chart removed used to be. The Secret grants the chart does render - the namespaced
// certificate-manager Role and the opt-in rbac.secretAccess Roles - are Roles, not ClusterRoles,
// and so are outside this test.
func TestNoClusterRoleGrantsSecrets(t *testing.T) {
	allVerbs := []string{"get", "list", "watch", "create", "update", "patch", "delete", "deletecollection"}
	for _, release := range []string{"ottoflow", "myrel"} {
		t.Run(release, func(t *testing.T) {
			roles := render(t, release)
			if len(roles) == 0 {
				t.Fatal("no ClusterRoles rendered")
			}
			for _, cr := range roles {
				for _, rule := range cr.Rules {
					if matched := grantedVerbs(rule, "", "secrets", allVerbs); len(matched) > 0 {
						t.Errorf("ClusterRole %q grants %v on secrets via rule %+v; the chart must ship no "+
							"cluster-wide Secret grant, see docs/user/rbac-secret-access.md", cr.Name, matched, rule)
					}
				}
			}
		})
	}
}

// TestExtraResources_SecretsGrantFailsRender pins the guard on every extraResources list, because
// every one of them reaches the controller's own identity. The core, additional and view lists
// render into ClusterRoles labelled aggregate-to-controller, which the controller's aggregated
// ClusterRole selects, so a grant there lands on its ServiceAccount directly. The runner and view
// lists render into runner-aggregated ClusterRoles, which the controller can bind to its own
// ServiceAccount (it keeps ClusterRoleBinding create/update plus bind on the runner role for the
// roleRef-immutability migration). A `secrets` grant — or a resource wildcard, which includes
// secrets — in any of the four would silently re-open the cluster-wide Secret access this chart
// removed, so the render must fail instead.
//
// The case list is per-values-path on purpose: a guard added to three of the four lists leaves the
// fourth as an unguarded way to hand the controller cluster-wide Secret reads, and a test that
// only exercised one path would not notice.
func TestExtraResources_SecretsGrantFailsRender(t *testing.T) {
	for name, values := range map[string]string{
		"runnerClusterRole secrets": `
rbac:
  runnerClusterRole:
    extraResources:
      - apiGroups: [""]
        resources: ["secrets"]
        verbs: ["get"]
`,
		"viewClusterRole secrets": `
rbac:
  viewClusterRole:
    extraResources:
      - apiGroups: [""]
        resources: ["secrets"]
        verbs: ["get"]
`,
		"coreClusterRole secrets": `
rbac:
  coreClusterRole:
    extraResources:
      - apiGroups: [""]
        resources: ["secrets"]
        verbs: ["get", "list", "watch"]
`,
		"clusterRole secrets": `
rbac:
  clusterRole:
    extraResources:
      - apiGroups: [""]
        resources: ["secrets"]
        verbs: ["get", "list", "watch"]
`,
		"runnerClusterRole wildcard": `
rbac:
  runnerClusterRole:
    extraResources:
      - apiGroups: [""]
        resources: ["*"]
        verbs: ["get"]
`,
		"coreClusterRole wildcard": `
rbac:
  coreClusterRole:
    extraResources:
      - apiGroups: [""]
        resources: ["*"]
        verbs: ["get"]
`,
		"clusterRole wildcard": `
rbac:
  clusterRole:
    extraResources:
      - apiGroups: [""]
        resources: ["*"]
        verbs: ["get"]
`,
		"viewClusterRole wildcard": `
rbac:
  viewClusterRole:
    extraResources:
      - apiGroups: [""]
        resources: ["*"]
        verbs: ["get"]
`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := renderWithValuesFile(t, writeSecretAccessValues(t, values))
			if err == nil {
				t.Fatal("expected helm template to fail on a secrets grant in extraResources, got success")
			}
			if !strings.Contains(err.Error(), "rbac.secretAccess") {
				t.Errorf("expected the failure to point at rbac.secretAccess as the sanctioned path, got: %v", err)
			}
		})
	}
}
