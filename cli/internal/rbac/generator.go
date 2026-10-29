/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package rbac

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/auth"
)

// Options configures the RBAC generator.
type Options struct {
	// Namespace is the default target namespace for ServiceAccount/Role/RoleBinding output.
	Namespace string
	// AgentExecutorNamespace is the namespace where the agent-executor service runs and
	// evaluates its SAR caller check. When set, agentRef RBAC rules target this namespace
	// instead of Namespace.
	AgentExecutorNamespace string
}

// Generator performs static analysis on a Workflow and emits RBAC manifests.
type Generator struct {
	opts Options
}

// policyRule represents a single RBAC rule contribution extracted from a workflow step.
type policyRule struct {
	group     string
	resource  string
	resName   string // non-empty → restrict via resourceNames
	verbs     []string
	clustered bool   // true → contributes to ClusterRole; false → contributes to Role
	targetNS  string // namespace for namespace-scoped rules (ignored when clustered=true)
}

// ruleKey uniquely identifies a group of policyRules that can be merged.
type ruleKey struct {
	group     string
	resource  string
	resName   string
	clustered bool
	targetNS  string
}

// New creates a Generator.
func New(opts Options) (*Generator, error) {
	return &Generator{opts: opts}, nil
}

// GenerateForWorkflow analyzes wf and returns deterministic YAML RBAC manifests.
// Warnings lists steps where a dynamic namespace caused a conservative ClusterRole fallback.
func (g *Generator) GenerateForWorkflow(wf *ottoflowv1alpha1.Workflow) ([]byte, []string, error) {
	var rules []policyRule
	var warnings []string
	for _, step := range wf.Spec.Steps {
		r, ws := g.analyzeStep(step)
		rules = append(rules, r...)
		warnings = append(warnings, ws...)
	}
	coalesced := coalesceRules(rules)
	objects := g.buildObjects(wf.Name+"-runner", coalesced)
	b, err := serialize(objects)
	return b, warnings, err
}

// analyzeStep extracts policyRules from a top-level Step.
func (g *Generator) analyzeStep(step ottoflowv1alpha1.Step) ([]policyRule, []string) {
	var rules []policyRule
	var warnings []string
	if step.ResourceQuery != nil {
		r, w := g.resourceQueryRule(step.ResourceQuery)
		rules = append(rules, r)
		if w != "" {
			warnings = append(warnings, fmt.Sprintf("step %q: %s", step.Name, w))
		}
	}
	if step.Mutate != nil {
		r, w := g.mutateRule(step.Mutate)
		rules = append(rules, r)
		if w != "" {
			warnings = append(warnings, fmt.Sprintf("step %q: %s", step.Name, w))
		}
	}
	if step.AgentRef != nil {
		rules = append(rules, g.agentRefRule())
	}
	if step.ForEach != nil && step.ForEach.Step != nil {
		inner, ws := g.analyzeForEachStep(*step.ForEach.Step)
		for _, w := range ws {
			warnings = append(warnings, fmt.Sprintf("step %q forEach: %s", step.Name, w))
		}
		rules = append(rules, inner...)
	}
	return rules, warnings
}

// analyzeForEachStep handles the StepForEachStep inline type.
func (g *Generator) analyzeForEachStep(step ottoflowv1alpha1.StepForEachStep) ([]policyRule, []string) {
	var rules []policyRule
	var warnings []string
	if step.ResourceQuery != nil {
		r, w := g.resourceQueryRule(step.ResourceQuery)
		rules = append(rules, r)
		if w != "" {
			warnings = append(warnings, w)
		}
	}
	if step.Mutate != nil {
		r, w := g.mutateRule(step.Mutate)
		rules = append(rules, r)
		if w != "" {
			warnings = append(warnings, w)
		}
	}
	if step.AgentRef != nil {
		rules = append(rules, g.agentRefRule())
	}
	return rules, warnings
}

// resourceQueryRule emits get/list on the queried GVR.
// If the namespace is a dynamic CEL expression, falls back to ClusterRole and returns a warning.
func (g *Generator) resourceQueryRule(rq *ottoflowv1alpha1.StepResourceQuery) (policyRule, string) {
	group, resource := parseGVR(rq.APIVersion, rq.Resource)
	ns, clustered := classifyNamespace(rq.Namespace, g.opts.Namespace)
	var warning string
	if !clustered && rq.Namespace != "" && !isStaticallyKnownNamespace(rq.Namespace) {
		ns, clustered = "", true
		warning = fmt.Sprintf("namespace %q is dynamic — generated ClusterRole as conservative fallback", rq.Namespace)
	}
	return policyRule{
		group:     group,
		resource:  resource,
		verbs:     []string{"get", "list"},
		clustered: clustered,
		targetNS:  ns,
	}, warning
}

// mutateRule emits get/patch/update on the target GVR.
// If the namespace is a dynamic CEL expression, falls back to ClusterRole and returns a warning.
func (g *Generator) mutateRule(m *ottoflowv1alpha1.StepMutate) (policyRule, string) {
	group, resource := parseGVR(m.Target.APIVersion, m.Target.Resource)
	ns, clustered := classifyNamespace(m.Target.Namespace, g.opts.Namespace)
	var warning string
	if !clustered && m.Target.Namespace != "" && !isStaticallyKnownNamespace(m.Target.Namespace) {
		ns, clustered = "", true
		warning = fmt.Sprintf("namespace %q is dynamic — generated ClusterRole as conservative fallback", m.Target.Namespace)
	}
	return policyRule{
		group:     group,
		resource:  resource,
		verbs:     []string{"get", "patch", "update"},
		clustered: clustered,
		targetNS:  ns,
	}, warning
}

// agentRefRule emits invoke on callers.gate.ottoflow.nirmata.io/agent-executor-caller (SAR authorization check).
// The rule targets AgentExecutorNamespace when set, falling back to Namespace, because the
// agent-executor evaluates the SAR check in its own namespace, not the runner's namespace.
func (g *Generator) agentRefRule() policyRule {
	ns := g.opts.AgentExecutorNamespace
	if ns == "" {
		ns = g.opts.Namespace
	}
	return policyRule{
		group:     auth.CallerAPIGroup,
		resource:  auth.CallerResource,
		resName:   auth.AgentExecutorCallerResourceName,
		verbs:     []string{auth.CallerVerb},
		clustered: false,
		targetNS:  ns,
	}
}

// classifyNamespace categorizes a step's namespace field.
// Returns (resolvedNamespace, isClusterScoped).
//
//   - empty string → cluster-scoped (ClusterRole)
//   - matches literalNSPattern → literal namespace → namespace-scoped Role in that namespace
//   - double-quoted CEL string literal (e.g. "default") → unquoted and treated as literal
//   - otherwise → falls back to defaultNS
//
// Note: callers guard with isStaticallyKnownNamespace before calling this function,
// so the final branch (non-literal, non-quoted CEL expression) is currently unreachable.
// It remains as a safe fallback if called directly without the guard.
//
// Kubernetes namespace names must be RFC 1123 DNS labels (lowercase alphanumeric and hyphens only).
// Values containing dots or other non-matching characters are treated as CEL expressions rather than literal names.
func classifyNamespace(ns, defaultNS string) (string, bool) {
	if ns == "" {
		return "", true
	}
	if isLiteralNamespace(ns) {
		return ns, false
	}
	// CEL string literal: "staging" → treat as literal namespace staging.
	if len(ns) >= 2 && ns[0] == '"' && ns[len(ns)-1] == '"' {
		if inner := ns[1 : len(ns)-1]; isLiteralNamespace(inner) {
			return inner, false
		}
	}
	return defaultNS, false
}

var literalNSPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

func isLiteralNamespace(ns string) bool {
	return literalNSPattern.MatchString(ns)
}

// isStaticallyKnownNamespace reports whether ns resolves to a concrete namespace
// without cluster access: either a bare RFC 1123 label or a quoted CEL string literal.
func isStaticallyKnownNamespace(ns string) bool {
	if isLiteralNamespace(ns) {
		return true
	}
	if len(ns) >= 2 && ns[0] == '"' && ns[len(ns)-1] == '"' {
		return isLiteralNamespace(ns[1 : len(ns)-1])
	}
	return false
}

// coalesceRules merges rules sharing the same (group, resource, resName, clustered, targetNS),
// deduplicates and sorts verbs, and returns a deterministically ordered slice.
func coalesceRules(rules []policyRule) []policyRule {
	verbSets := make(map[ruleKey]map[string]struct{})
	var order []ruleKey
	seen := make(map[ruleKey]bool)

	for _, r := range rules {
		k := ruleKey{r.group, r.resource, r.resName, r.clustered, r.targetNS}
		if !seen[k] {
			order = append(order, k)
			seen[k] = true
			verbSets[k] = make(map[string]struct{})
		}
		for _, v := range r.verbs {
			verbSets[k][v] = struct{}{}
		}
	}

	// Deterministic sort: namespace-scoped first, then by ns, group, resource, resName.
	sort.Slice(order, func(i, j int) bool {
		a, b := order[i], order[j]
		if a.clustered != b.clustered {
			return !a.clustered
		}
		if a.targetNS != b.targetNS {
			return a.targetNS < b.targetNS
		}
		if a.group != b.group {
			return a.group < b.group
		}
		if a.resource != b.resource {
			return a.resource < b.resource
		}
		return a.resName < b.resName
	})

	result := make([]policyRule, 0, len(order))
	for _, k := range order {
		verbSet := verbSets[k]
		verbs := make([]string, 0, len(verbSet))
		for v := range verbSet {
			verbs = append(verbs, v)
		}
		sort.Strings(verbs)
		result = append(result, policyRule{
			group:     k.group,
			resource:  k.resource,
			resName:   k.resName,
			verbs:     verbs,
			clustered: k.clustered,
			targetNS:  k.targetNS,
		})
	}
	return result
}

// buildObjects constructs the ordered list of RBAC Kubernetes objects for a workflow:
// ServiceAccount, Role(s)+RoleBinding(s) per namespace, ClusterRole+ClusterRoleBinding if needed.
func (g *Generator) buildObjects(saName string, rules []policyRule) []any {
	var objects []any
	defaultNS := g.opts.Namespace

	objects = append(objects, newServiceAccount(saName, defaultNS))

	nsByRules := make(map[string][]policyRule)
	var clusterRules []policyRule

	for _, r := range rules {
		if r.clustered {
			clusterRules = append(clusterRules, r)
		} else {
			nsByRules[r.targetNS] = append(nsByRules[r.targetNS], r)
		}
	}

	roleName := saName
	for _, ns := range sortedKeys(nsByRules) {
		objects = append(objects,
			newRole(roleName, ns, nsByRules[ns]),
			newRoleBinding(roleName, ns, saName, defaultNS),
		)
	}

	if len(clusterRules) > 0 {
		objects = append(objects,
			newClusterRole(roleName, clusterRules),
			newClusterRoleBinding(roleName, saName, defaultNS),
		)
	}

	return objects
}

func newServiceAccount(name, namespace string) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "ServiceAccount",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    ottoflowLabels(),
		},
	}
}

func newRole(name, namespace string, rules []policyRule) *rbacv1.Role {
	return &rbacv1.Role{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "rbac.authorization.k8s.io/v1",
			Kind:       "Role",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    ottoflowLabels(),
		},
		Rules: toPolicyRules(rules),
	}
}

func newClusterRole(name string, rules []policyRule) *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "rbac.authorization.k8s.io/v1",
			Kind:       "ClusterRole",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: ottoflowLabels(),
		},
		Rules: toPolicyRules(rules),
	}
}

func newRoleBinding(roleName, namespace, saName, saNS string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "rbac.authorization.k8s.io/v1",
			Kind:       "RoleBinding",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      roleName,
			Namespace: namespace,
			Labels:    ottoflowLabels(),
		},
		Subjects: []rbacv1.Subject{
			{Kind: "ServiceAccount", Name: saName, Namespace: saNS},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     roleName,
		},
	}
}

func newClusterRoleBinding(roleName, saName, saNS string) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "rbac.authorization.k8s.io/v1",
			Kind:       "ClusterRoleBinding",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:   roleName,
			Labels: ottoflowLabels(),
		},
		Subjects: []rbacv1.Subject{
			{Kind: "ServiceAccount", Name: saName, Namespace: saNS},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     roleName,
		},
	}
}

func toPolicyRules(rules []policyRule) []rbacv1.PolicyRule {
	result := make([]rbacv1.PolicyRule, 0, len(rules))
	for _, r := range rules {
		pr := rbacv1.PolicyRule{
			APIGroups: []string{r.group},
			Resources: []string{r.resource},
			Verbs:     r.verbs,
		}
		if r.resName != "" {
			pr.ResourceNames = []string{r.resName}
		}
		result = append(result, pr)
	}
	return result
}

func ottoflowLabels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by": "ottoflow",
		"app.kubernetes.io/part-of":    "ottoflow",
	}
}

// serialize marshals each object to YAML and joins with "---\n" separators.
func serialize(objects []any) ([]byte, error) {
	var buf bytes.Buffer

	for i, obj := range objects {
		if i > 0 {
			buf.WriteString("---\n")
		}
		data, err := yaml.Marshal(obj)
		if err != nil {
			return nil, fmt.Errorf("marshal RBAC object: %w", err)
		}
		buf.Write(removeCreationTimestamp(data))
	}

	return buf.Bytes(), nil
}

// removeCreationTimestamp strips the "creationTimestamp: null" line that sigs.k8s.io/yaml
// emits for zero-value metav1.Time fields, keeping output clean for GitOps use.
func removeCreationTimestamp(data []byte) []byte {
	var lines [][]byte
	for _, line := range bytes.Split(data, []byte("\n")) {
		if bytes.Equal(bytes.TrimSpace(line), []byte("creationTimestamp: null")) {
			continue
		}
		lines = append(lines, line)
	}
	return bytes.Join(lines, []byte("\n"))
}

// parseGVR splits apiVersion and converts kind to a plural resource name.
// Applies irregular plural handling for common Kubernetes resources before falling back
// to the standard consonant+y→ies, ss/x/ch/sh→es, and default +s rules.
func parseGVR(apiVersion, kind string) (group, resource string) {
	parts := strings.SplitN(apiVersion, "/", 2)
	if len(parts) == 2 {
		group = parts[0]
	}
	resource = pluralizeKind(kind)
	return
}

// irregularPlurals maps lowercase Kind names to their canonical Kubernetes resource (plural) names
// for cases that deviate from the heuristic rules in pluralizeKind.
//
// When to add an entry here: if a workflow step uses a resource Kind whose plural form
// pluralizeKind produces incorrectly (verify with `kubectl api-resources | grep <resource>`),
// add a "lowercase-kind": "correct-plural" entry. Custom Resource Definitions with unusual
// plural forms (e.g. defined with `plural: datacaches` for a Kind `DataCache`) need entries here
// because the CRD author controls the plural, not standard English rules.
var irregularPlurals = map[string]string{
	"endpoints":     "endpoints", // already plural
	"endpointslice": "endpointslices",
}

// pluralizeKind converts a Kubernetes Kind string to its lowercase plural resource name.
//
// Rules applied in order:
//  1. Static override table for known irregulars
//  2. Consonant + y → remove y, append ies  (e.g., NetworkPolicy → networkpolicies)
//  3. Ends in ss, x, ch, or sh → append es  (e.g., Ingress → ingresses)
//  4. Default: append s
func pluralizeKind(kind string) string {
	lower := strings.ToLower(kind)
	// Kind names always start uppercase; all-lowercase input is already a resource name.
	if kind == lower {
		return lower
	}
	if p, ok := irregularPlurals[lower]; ok {
		return p
	}
	if len(lower) >= 2 && lower[len(lower)-1] == 'y' {
		prev := rune(lower[len(lower)-2])
		if !strings.ContainsRune("aeiou", prev) {
			return lower[:len(lower)-1] + "ies"
		}
	}
	if strings.HasSuffix(lower, "ss") || strings.HasSuffix(lower, "x") ||
		strings.HasSuffix(lower, "ch") || strings.HasSuffix(lower, "sh") {
		return lower + "es"
	}
	return lower + "s"
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
