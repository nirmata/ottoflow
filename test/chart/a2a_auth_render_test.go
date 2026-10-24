/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package chart

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

const (
	a2aReviewerRole = "ottoflow-a2a-token-reviewer"
	a2aCallerRole   = "ottoflow-a2a-caller"
)

// renderDocs runs `helm template ottoflow <chart> --namespace ottoflow [extraArgs...]` and returns
// the individual YAML documents. Reuses chartDir/helmBin/splitYAMLDocs from the chart test package.
func renderDocs(t *testing.T, extraArgs ...string) []string {
	t.Helper()
	helm := helmBin(t)
	chart := chartDir(t)

	args := append([]string{"template", "ottoflow", chart, "--namespace", "ottoflow"}, extraArgs...)
	cmd := exec.Command(helm, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("helm template %v failed: %v\nstderr:\n%s", args, err, stderr.String())
	}
	return splitYAMLDocs(stdout.String())
}

// docKind returns the apiVersion/kind of a single rendered document.
func docKind(doc string) metav1.TypeMeta {
	var tm metav1.TypeMeta
	_ = yaml.Unmarshal([]byte(doc), &tm)
	return tm
}

func a2aClusterRoles(t *testing.T, docs []string) map[string]rbacv1.ClusterRole {
	t.Helper()
	out := map[string]rbacv1.ClusterRole{}
	for _, d := range docs {
		if docKind(d).Kind != "ClusterRole" {
			continue
		}
		var cr rbacv1.ClusterRole
		if err := yaml.Unmarshal([]byte(d), &cr); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(cr.Name, "a2a-token-reviewer") || strings.Contains(cr.Name, "a2a-caller") {
			out[cr.Name] = cr
		}
	}
	return out
}

func controllerArgsContain(docs []string, flag string) bool {
	for _, d := range docs {
		if docKind(d).Kind == "Deployment" && strings.Contains(d, "--serve-a2a-image") {
			return strings.Contains(d, flag)
		}
	}
	return false
}

func TestA2AAuthRoles_OffByDefault(t *testing.T) {
	if got := a2aClusterRoles(t, renderDocs(t)); len(got) != 0 {
		t.Fatalf("serveA2A disabled rendered %d a2a auth roles, want 0", len(got))
	}
}

func TestA2AAuthRoles_NotRenderedWithoutRBAC(t *testing.T) {
	docs := renderDocs(t, "--set", "serveA2A.enabled=true", "--set", "rbac.create=false")
	if got := a2aClusterRoles(t, docs); len(got) != 0 {
		t.Fatalf("rbac.create=false rendered %d a2a auth roles, want 0", len(got))
	}
}

func TestA2AAuthRoles_ExactRules(t *testing.T) {
	roles := a2aClusterRoles(t, renderDocs(t, "--set", "serveA2A.enabled=true"))
	if len(roles) != 2 {
		t.Fatalf("got %d a2a auth roles %v, want 2", len(roles), roles)
	}

	rev, ok := roles[a2aReviewerRole]
	if !ok {
		t.Fatalf("missing ClusterRole %s", a2aReviewerRole)
	}
	if len(rev.Rules) != 2 ||
		!ruleIs(rev.Rules[0], "authentication.k8s.io", "tokenreviews", "", "create") ||
		!ruleIs(rev.Rules[1], "authorization.k8s.io", "subjectaccessreviews", "", "create") {
		t.Errorf("reviewer rules = %+v, want create tokenreviews + create subjectaccessreviews only", rev.Rules)
	}

	caller, ok := roles[a2aCallerRole]
	if !ok {
		t.Fatalf("missing ClusterRole %s", a2aCallerRole)
	}
	if len(caller.Rules) != 1 {
		t.Fatalf("caller rules = %+v, want exactly one", caller.Rules)
	}
	if !ruleIs(caller.Rules[0], "gate.ottoflow.nirmata.io", "callers", "a2a-caller", "invoke") {
		t.Errorf("caller rule = %+v, want invoke callers.gate.ottoflow.nirmata.io/a2a-caller", caller.Rules[0])
	}
}

// ruleIs reports whether r is exactly one apiGroup, one resource, one verb and (when name is
// non-empty) one resourceName.
func ruleIs(r rbacv1.PolicyRule, group, resource, name, verb string) bool {
	var names []string
	if name != "" {
		names = []string{name}
	}
	return strings.Join(r.APIGroups, ",") == group && strings.Join(r.Resources, ",") == resource &&
		strings.Join(r.ResourceNames, ",") == strings.Join(names, ",") && strings.Join(r.Verbs, ",") == verb
}

func TestA2AAllowUnauthenticatedFlag(t *testing.T) {
	const flag = "--serve-a2a-allow-unauthenticated"
	if controllerArgsContain(renderDocs(t, "--set", "serveA2A.enabled=true"), flag) {
		t.Error("default render passes " + flag + "; auth must be on by default")
	}
	optOut := renderDocs(t, "--set", "serveA2A.enabled=true", "--set", "serveA2A.allowUnauthenticated=true")
	if !controllerArgsContain(optOut, flag) {
		t.Error("allowUnauthenticated=true did not pass " + flag)
	}
}

func TestCallerRolesUseVirtualResource(t *testing.T) {
	docs := renderDocs(t, "--set", "controller.mcp.enabled=true")
	roles := renderedClusterRoles(t, docs)
	for role, name := range map[string]string{
		"ottoflow-agent-executor-caller": "agent-executor-caller",
		"ottoflow-mcp-caller":            "mcp-caller",
	} {
		cr, ok := roles[role]
		if !ok {
			t.Errorf("ClusterRole %s not rendered", role)
			continue
		}
		if len(cr.Rules) != 1 || !ruleIs(cr.Rules[0], "gate.ottoflow.nirmata.io", "callers", name, "invoke") {
			t.Errorf("%s rules = %+v, want one invoke callers.gate.ottoflow.nirmata.io/%s", role, cr.Rules, name)
		}
	}
	for _, d := range docs {
		if docKind(d).Kind != "ConfigMap" {
			continue
		}
		var cm metav1.PartialObjectMetadata
		if err := yaml.Unmarshal([]byte(d), &cm); err != nil {
			t.Fatal(err)
		}
		if cm.Name == "agent-executor-caller" || cm.Name == "mcp-caller" {
			t.Errorf("placeholder ConfigMap %s still rendered", cm.Name)
		}
	}
}
