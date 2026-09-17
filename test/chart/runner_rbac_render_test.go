/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

// Package chart contains assertions rendered from the OttoFlow Helm chart, verifying
// that the runner-aggregated RBAC ruleset stays least-privilege and per-release scoped.
package chart

import (
	"bytes"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

// chartDir resolves the OttoFlow chart directory relative to this test file
// (test/chart/runner_rbac_render_test.go -> <repo>/charts/ottoflow).
func chartDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to resolve caller for chart path")
	}
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")
	return filepath.Join(repoRoot, "charts", "ottoflow")
}

// helmBin locates a helm binary: PATH first, then a repo-local bin/helm-* (see Makefile HELM var).
func helmBin(t *testing.T) string {
	t.Helper()
	if p, err := exec.LookPath("helm"); err == nil {
		return p
	}
	_, thisFile, _, ok := runtime.Caller(0)
	if ok {
		repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")
		if matches, _ := filepath.Glob(filepath.Join(repoRoot, "bin", "helm-*")); len(matches) > 0 {
			return matches[0]
		}
	}
	t.Skip("helm not found on PATH")
	return ""
}

// splitYAMLDocs splits a multi-document YAML stream (as emitted by `helm template`) into
// individual documents.
func splitYAMLDocs(output string) []string {
	output = strings.TrimSpace(output)
	output = strings.TrimPrefix(output, "---\n")
	return strings.Split(output, "\n---\n")
}

// helmTemplate runs `helm template <release>` against the OttoFlow chart, optionally layering
// an extra values file on top of the chart defaults, and returns the raw multi-document YAML
// output plus any error from the helm invocation (including a template `fail` — the caller
// decides whether an error is expected). The single low-level render used by every chart test
// in this package.
func helmTemplate(t *testing.T, release, valuesFile string) (string, error) {
	t.Helper()
	helm := helmBin(t)
	chart := chartDir(t)

	args := []string{"template", release, chart, "--namespace", "ottoflow"}
	if valuesFile != "" {
		args = append(args, "-f", valuesFile)
	}
	cmd := exec.Command(helm, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w: %s", err, stderr.String())
	}
	return stdout.String(), nil
}

// render runs `helm template` for the given release against the OttoFlow chart with default
// values and returns every rendered ClusterRole. Non-ClusterRole documents (ServiceAccounts,
// ClusterRoleBindings, Deployments, etc.) are decoded too but discarded once their Kind is known.
func render(t *testing.T, release string) []rbacv1.ClusterRole {
	t.Helper()
	output, err := helmTemplate(t, release, "")
	if err != nil {
		t.Fatalf("helm template %s failed: %v", release, err)
	}

	var roles []rbacv1.ClusterRole
	for _, doc := range splitYAMLDocs(output) {
		doc = strings.TrimSpace(doc)
		if doc == "" {
			continue
		}
		var cr rbacv1.ClusterRole
		if err := yaml.Unmarshal([]byte(doc), &cr); err != nil {
			// Not a ClusterRole-shaped document (or not YAML at all, e.g. NOTES.txt) - skip.
			continue
		}
		if cr.Kind != "ClusterRole" {
			continue
		}
		roles = append(roles, cr)
	}
	return roles
}

const aggregateToRunnerLabel = "rbac.ottoflow.io/aggregate-to-runner"
const aggregateInstanceLabel = "rbac.ottoflow.io/aggregate-instance"

// forbiddenGrant describes a (apiGroup, resource, verb) combination the runner-aggregated
// ClusterRoles must never grant, because it would let a compromised workflow-runner Job
// escalate to controller-level privileges or read every Secret in the cluster.
type forbiddenGrant struct {
	apiGroup string
	resource string
	verbs    []string
}

var forbiddenGrants = []forbiddenGrant{
	{apiGroup: "batch", resource: "jobs", verbs: []string{"create"}},
	{apiGroup: "", resource: "serviceaccounts", verbs: []string{"create"}},
	{apiGroup: "rbac.authorization.k8s.io", resource: "clusterrolebindings", verbs: []string{"create"}},
	// Every verb on secrets: the runner reads the Secrets the controller mounts into its Job
	// from files and holds no Secret grant of its own. get/list/watch is the rule this chart
	// removed from the runner role; it is listed here so that re-adding it fails this test.
	{apiGroup: "", resource: "secrets", verbs: []string{
		"get", "list", "watch", "create", "update", "patch", "delete", "deletecollection",
	}},
}

// containsOrWildcard reports whether list contains want, or contains the RBAC wildcard "*".
func containsOrWildcard(list []string, want string) bool {
	for _, v := range list {
		if v == "*" || v == want {
			return true
		}
	}
	return false
}

// grantedVerbs returns the subset of verbs that rule grants for the given apiGroup/resource,
// treating "*" in APIGroups/Resources/Verbs as matching everything.
func grantedVerbs(rule rbacv1.PolicyRule, apiGroup, resource string, verbs []string) []string {
	if !containsOrWildcard(rule.APIGroups, apiGroup) {
		return nil
	}
	if !containsOrWildcard(rule.Resources, resource) {
		return nil
	}
	var matched []string
	for _, v := range verbs {
		if containsOrWildcard(rule.Verbs, v) {
			matched = append(matched, v)
		}
	}
	return matched
}

// TestRunnerRoleLacksEscalationVerbs verifies that no ClusterRole aggregated to the runner
// role (rbac.ottoflow.io/aggregate-to-runner: "true") grants any of the forbiddenGrants —
// verbs that would let a compromised workflow-runner Job escalate to controller privileges.
func TestRunnerRoleLacksEscalationVerbs(t *testing.T) {
	release := "ottoflow"
	roles := render(t, release)

	var sawRunnerAggregatedRole bool
	for _, cr := range roles {
		if cr.Labels[aggregateToRunnerLabel] != "true" {
			continue
		}
		sawRunnerAggregatedRole = true
		for _, rule := range cr.Rules {
			for _, g := range forbiddenGrants {
				if matched := grantedVerbs(rule, g.apiGroup, g.resource, g.verbs); len(matched) > 0 {
					t.Errorf("ClusterRole %q (aggregate-to-runner) grants forbidden verbs %v on apiGroup=%q resource=%q via rule %+v",
						cr.Name, matched, g.apiGroup, g.resource, rule)
				}
			}
		}
	}
	if !sawRunnerAggregatedRole {
		t.Fatalf("no ClusterRole labeled %s=\"true\" found in rendered chart for release %q", aggregateToRunnerLabel, release)
	}
}

// TestRunnerRoleAggregationIsPerRelease verifies that the runner RBAC aggregation labels are
// scoped per chart instance: every ClusterRole labeled aggregate-to-runner carries the same
// aggregate-instance value as the aggregated runner ClusterRole's own selector, and that value
// differs between two distinct releases. Without this, multiple OttoFlow installs on one
// cluster would aggregate each other's runner ClusterRoles.
//
// The aggregate-instance value is the chart's fullname (see the "ottoflow.fullname" template
// helper), which is not always the literal release name passed to `helm install` (e.g. under
// fullnameOverride) — so this test reads the value back from the rendered output instead of
// reimplementing the fullname logic in Go, and only asserts consistency + cross-release
// distinctness.
func TestRunnerRoleAggregationIsPerRelease(t *testing.T) {
	values := make(map[string]string, 2)
	for _, release := range []string{"rel-a", "rel-b"} {
		roles := render(t, release)

		var aggregationRole *rbacv1.ClusterRole
		for i := range roles {
			cr := &roles[i]
			if cr.AggregationRule != nil && strings.HasSuffix(cr.Name, "-runner-role") {
				aggregationRole = cr
			}
		}
		if aggregationRole == nil {
			t.Fatalf("release %q: no aggregated runner ClusterRole (name ending \"-runner-role\" with an aggregationRule) found",
				release)
		}
		if len(aggregationRole.AggregationRule.ClusterRoleSelectors) == 0 {
			t.Fatalf("release %q: aggregated runner ClusterRole %q has no clusterRoleSelectors", release, aggregationRole.Name)
		}
		want := aggregationRole.AggregationRule.ClusterRoleSelectors[0].MatchLabels[aggregateInstanceLabel]
		if want == "" {
			t.Fatalf("release %q: aggregated runner ClusterRole %q selector has no %s value",
				release, aggregationRole.Name, aggregateInstanceLabel)
		}
		values[release] = want

		// Consistency: every selector on the aggregation role, and every aggregate-to-runner
		// fragment, must carry this same aggregate-instance value.
		for _, sel := range aggregationRole.AggregationRule.ClusterRoleSelectors {
			if got := sel.MatchLabels[aggregateInstanceLabel]; got != want {
				t.Errorf("release %q: ClusterRole %q selector %s=%q, want %q (inconsistent selectors)",
					release, aggregationRole.Name, aggregateInstanceLabel, got, want)
			}
		}
		for _, cr := range roles {
			if cr.Labels[aggregateToRunnerLabel] != "true" {
				continue
			}
			if got := cr.Labels[aggregateInstanceLabel]; got != want {
				t.Errorf("release %q: ClusterRole %q has %s=%q, want %q (must match the runner aggregation role's selector value)",
					release, cr.Name, aggregateInstanceLabel, got, want)
			}
		}
	}

	if values["rel-a"] == values["rel-b"] {
		t.Errorf("aggregate-instance value %q is identical across releases rel-a and rel-b; "+
			"multiple OttoFlow installs on one cluster would aggregate each other's runner ClusterRoles",
			values["rel-a"])
	}
}
