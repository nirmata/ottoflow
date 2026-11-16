/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package chart

import (
	"slices"
	"strings"
	"testing"

	"github.com/nirmata/ottoflow/internal/certmanager"
	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

const agentExecutorInternalTLSValues = `
agentExecutor:
  enabled: true
  tls:
    internal:
      enabled: true
`

func decodeCertmanagerDocs(t *testing.T, output string) (appsv1.Deployment, rbacv1.Role) {
	t.Helper()
	docs := splitYAMLDocs(output)
	var deploy appsv1.Deployment
	if err := yaml.Unmarshal([]byte(controllerDeployment(t, docs)), &deploy); err != nil {
		t.Fatal(err)
	}
	var role rbacv1.Role
	for _, doc := range docs {
		var rl rbacv1.Role
		if yaml.Unmarshal([]byte(doc), &rl) == nil && rl.Kind == "Role" && strings.HasSuffix(rl.Name, "-certmanager-role") {
			role = rl
		}
	}
	if role.Name == "" {
		t.Fatal("certmanager Role missing from the render")
	}
	return deploy, role
}

func flagValue(t *testing.T, deploy appsv1.Deployment, name string) string {
	t.Helper()
	for _, arg := range deploy.Spec.Template.Spec.Containers[0].Args {
		if v, ok := strings.CutPrefix(arg, "--"+name+"="); ok {
			return v
		}
	}
	t.Fatalf("controller arg --%s not rendered", name)
	return ""
}

func TestCertmanagerRole_NamesMatchWhatTheControllerComputes(t *testing.T) {
	values := writeSecretAccessValues(t, agentExecutorInternalTLSValues)
	for _, release := range []string{"ottoflow", "prod"} {
		t.Run(release, func(t *testing.T) {
			output, err := helmTemplate(t, release, values)
			if err != nil {
				t.Fatalf("helm template failed: %v", err)
			}
			deploy, role := decodeCertmanagerDocs(t, output)

			var webhookSvc string
			for _, env := range deploy.Spec.Template.Spec.Containers[0].Env {
				if env.Name == "WEBHOOK_SERVICE_NAME" {
					webhookSvc = env.Value
				}
			}
			agentSvc := flagValue(t, deploy, "agent-executor-service-name")
			want := []string{
				certmanager.GetRootCASecretName(webhookSvc, "ottoflow"), certmanager.GetTLSPairSecretName(webhookSvc, "ottoflow"),
				certmanager.GetRootCASecretName(agentSvc, "ottoflow"), certmanager.GetTLSPairSecretName(agentSvc, "ottoflow"),
			}

			var got []string
			for _, rule := range role.Rules {
				got = append(got, rule.ResourceNames...)
			}
			slices.Sort(got)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("certmanager Role resourceNames = %v, want %v", got, want)
			}
			if ca := flagValue(t, deploy, "workflow-runner-agent-executor-ca-secret"); !slices.Contains(got, ca) {
				t.Errorf("--workflow-runner-agent-executor-ca-secret=%q is not a name the certmanager Role grants (%v)", ca, got)
			}
		})
	}
}

func TestAgentExecutorCASecretValue_RefusedOnlyWhereTheFlagIsRendered(t *testing.T) {
	t.Run("internal TLS", func(t *testing.T) {
		values := writeSecretAccessValues(t,
			agentExecutorInternalTLSValues+"workflowRunner:\n  agentExecutorCASecret: custom-ca\n")
		_, err := helmTemplate(t, "ottoflow", values)
		if err == nil || !strings.Contains(err.Error(), "agentExecutorCASecret is no longer supported") {
			t.Fatalf("want a render failure naming the retired value, got %v", err)
		}
	})
	t.Run("external TLS", func(t *testing.T) {
		values := writeSecretAccessValues(t, `
agentExecutor:
  enabled: true
  tls:
    secretName: my-own-tls
workflowRunner:
  agentExecutorCASecret: custom-ca
`)
		output, err := helmTemplate(t, "ottoflow", values)
		if err != nil {
			t.Fatalf("an inert value on an external-TLS install must not block the render: %v", err)
		}
		if strings.Contains(output, "--workflow-runner-agent-executor-ca-secret") {
			t.Error("the CA flag must not be rendered when agent-executor TLS is external")
		}
	})
}
