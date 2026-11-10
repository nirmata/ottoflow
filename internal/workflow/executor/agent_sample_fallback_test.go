/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/agent"
)

func TestSampleAgentStepRecommendations(t *testing.T) {
	const prose = "Scale to 3 replicas and pin the image tag."
	for _, f := range []struct{ file, step string }{
		{"agent-step.yaml", "analyzeWithAgent"},
		{"agent-mcp-combined.yaml", "analyzeWithContext"},
	} {
		for _, r := range []struct {
			name, reply string
			want        interface{}
		}{
			{"prose", prose, prose},
			{"json", `{"recommendations": ["scale to 3"]}`, []interface{}{"scale to 3"}},
		} {
			t.Run(f.file+"/"+r.name, func(t *testing.T) {
				raw, err := os.ReadFile("../../../samples/workflows/features/" + f.file)
				if err != nil {
					t.Fatal(err)
				}
				var wf ottoflowv1alpha1.Workflow
				if err := yaml.Unmarshal(raw, &wf); err != nil {
					t.Fatal(err)
				}
				step := *findStepByName(wf.Spec.Steps, f.step)
				step.DependsOn = nil
				step.AgentRef = &ottoflowv1alpha1.StepAgentRef{Name: "analysis-agent"}
				wf.Spec.Steps = []ottoflowv1alpha1.Step{step}

				ag := &ottoflowv1alpha1.Agent{
					ObjectMeta: metav1.ObjectMeta{Name: "analysis-agent", Namespace: "default"},
					Spec: ottoflowv1alpha1.AgentSpec{
						Prompt: "p", ModelProvider: "openai", ModelName: "gpt-4",
						OutputExtraction: &ottoflowv1alpha1.OutputExtraction{Type: "json", Pattern: "$.recommendations"},
					},
				}
				c := fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(ag).Build()
				run := newTestWorkflowRun("r")
				ctx := context.Background()
				stub := agent.NewMockAgentExecutor()
				stub.SetDefaultResponse(r.reply)
				we, err := NewWorkflowExecutorWithAgentExecutor(c, nil, nil, nil, run, stub, true, 0, 5, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := we.ExecuteWorkflow(ctx, &wf, run); err != nil {
					t.Fatalf("workflow failed: %v", err)
				}
				cd, err := we.GetContextManager().ReadContext(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if got := cd["variables"].(map[string]interface{})["recommendations"]; !reflect.DeepEqual(got, r.want) {
					t.Fatalf("recommendations = %v, want %v", got, r.want)
				}
			})
		}
	}
}

// sampleDeployment returns a Deployment in ns whose single container runs image.
func sampleDeployment(ns, name, image string) *appsv1.Deployment {
	replicas := int32(2)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name: "app", Image: image, Ports: []corev1.ContainerPort{{ContainerPort: 8080}},
				}}},
			},
		},
	}
}

// TestSampleCVEVexAssessment runs production/cve-vex-assessment.yaml with the repo's real CEL
// environment, a fake cluster and a stubbed agent reply. `ottoflow validate` neither type-checks
// step-output CEL nor compiles spec.outputs, so this is the check that the OpenVEX document
// expression compiles and evaluates.
func TestSampleCVEVexAssessment(t *testing.T) {
	raw, err := os.ReadFile("../../../samples/workflows/production/cve-vex-assessment.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// The file holds the Agent, then the Workflow. Strict decoding fails on a misspelled field.
	docs := strings.Split(string(raw), "\n---\n")
	if len(docs) != 2 {
		t.Fatalf("want 2 YAML documents (Agent, Workflow), got %d", len(docs))
	}
	var ag ottoflowv1alpha1.Agent
	if err := yaml.UnmarshalStrict([]byte(docs[0]), &ag); err != nil {
		t.Fatal(err)
	}
	var wf ottoflowv1alpha1.Workflow
	if err := yaml.UnmarshalStrict([]byte(docs[1]), &wf); err != nil {
		t.Fatal(err)
	}

	const image, cve, noStatus = "ghcr.io/acme/web:1.2.3", "CVE-2025-0001", "no such key: status"
	// What collectDeployments keeps for prod/web: the set fields and the defaults for unset ones.
	const wantCollected = `[{"automountServiceAccountToken":true,"containers":[{"allowPrivilegeEscalation":"unset",` +
		`"capabilitiesAdd":["NET_ADMIN"],"name":"app","ports":[8080],"privileged":"unset",` +
		`"readOnlyRootFilesystem":"unset","runAsNonRoot":true}],"deployment":"prod/web","hostNetwork":false,` +
		`"hostPID":false,"replicas":2,"serviceAccount":"default"}]`
	runAsNonRoot := true
	for _, tc := range []struct {
		name, cveID, reply string
		want               map[string]any // the statement's judgment fields; nil when the run must fail
		wantErr            string         // a substring of the run's error when want is nil
	}{
		{"not_affected", cve, `{"status":"not_affected","justification":"vulnerable_code_not_in_execute_path"}`,
			map[string]any{"status": "not_affected", "justification": "vulnerable_code_not_in_execute_path"}, ""},
		{"unknown justification", cve, `{"status":"not_affected","justification":"made_up"}`,
			map[string]any{"status": "under_investigation"}, ""},
		{"affected without action", cve, `{"status":"affected"}`, map[string]any{"status": "under_investigation"}, ""},
		{"affected with null action", cve, `{"status":"affected","action_statement":null}`,
			map[string]any{"status": "under_investigation"}, ""},
		{"affected", cve, `{"status":"affected","action_statement":"Upgrade to 1.2.4."}`,
			map[string]any{"status": "affected", "action_statement": "Upgrade to 1.2.4."}, ""},
		{"fixed", cve, `{"status":"fixed"}`, map[string]any{"status": "fixed"}, ""},
		{"prose reply", cve, "It is probably fine.", nil, noStatus},
		{"wrong key", cve, `{"Status":"fixed"}`, nil, noStatus},
		{"cveId with a space", "CVE-2025 0001", `{"status":"fixed"}`, nil, "no such key: invalid cveId"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			web := sampleDeployment("prod", "web", image)
			web.Spec.Template.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{
				RunAsNonRoot: &runAsNonRoot,
				Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"NET_ADMIN"}},
			}
			c := fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(
				ag.DeepCopy(), web, sampleDeployment("prod", "other", "ghcr.io/acme/other:9"),
			).Build()
			run := newTestWorkflowRun("r")
			run.Namespace = wf.Namespace // the namespace input defaults to "": all namespaces
			run.Spec.InputValues = map[string]string{"image": image, "cveId": tc.cveID}
			stub := agent.NewMockAgentExecutor()
			stub.SetDefaultResponse(tc.reply)
			we, err := NewWorkflowExecutorWithAgentExecutor(c, nil, nil, nil, run, stub, true, 0, 5, nil)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			before := time.Now().Unix()
			err = we.ExecuteWorkflow(ctx, &wf, run)
			calls := stub.GetCallHistory()
			if tc.want == nil {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want an error containing %q, got %v", tc.wantErr, err)
				}
				if tc.cveID != cve && len(calls) != 0 {
					t.Fatalf("the agent was called %d times for an invalid cveId", len(calls))
				}
				return
			}
			if err != nil {
				t.Fatalf("workflow failed: %v", err)
			}

			if len(calls) != 1 {
				t.Fatalf("agent calls = %d, want 1", len(calls))
			}
			if !strings.Contains(calls[0].Prompt, "prod/web") || strings.Contains(calls[0].Prompt, "prod/other") {
				t.Fatalf("prompt must list prod/web only:\n%s", calls[0].Prompt)
			}
			if got := string(run.Status.Outputs["affectedDeployments"].Raw); got != `["prod/web"]` {
				t.Fatalf("affectedDeployments = %s", got)
			}
			cd, err := we.GetContextManager().ReadContext(ctx)
			if err != nil {
				t.Fatal(err)
			}
			collected, err := json.Marshal(cd["variables"].(map[string]any)["affectedDeployments"])
			if err != nil || string(collected) != wantCollected {
				t.Fatalf("variables.affectedDeployments = %s (err %v), want %s", collected, err, wantCollected)
			}

			rawVex, ok := run.Status.Outputs["vex"]
			if !ok {
				t.Fatalf("no vex output: %v", run.Status.Outputs)
			}
			var doc struct {
				Context    string           `json:"@context"`
				ID         string           `json:"@id"`
				Author     string           `json:"author"`
				Timestamp  string           `json:"timestamp"`
				Version    int              `json:"version"`
				Statements []map[string]any `json:"statements"`
			}
			if err := json.Unmarshal(rawVex.Raw, &doc); err != nil {
				t.Fatalf("vex %s: %v", rawVex.Raw, err)
			}
			if doc.Context != "https://openvex.dev/ns/v0.2.0" || doc.Version != 1 || doc.Author == "" {
				t.Fatalf("@context/version/author: %s", rawVex.Raw)
			}
			const idPrefix = "https://openvex.dev/docs/public/ottoflow-" + cve + "-"
			secs, err := strconv.ParseInt(strings.TrimPrefix(doc.ID, idPrefix), 10, 64)
			if !strings.HasPrefix(doc.ID, idPrefix) || err != nil || secs < before || secs > time.Now().Unix() {
				t.Fatalf("@id %q is not %s<unix seconds of this run>", doc.ID, idPrefix)
			}
			// RFC 3339 with the process's UTC offset, not necessarily Z.
			if _, err := time.Parse(time.RFC3339Nano, doc.Timestamp); err != nil {
				t.Fatalf("timestamp %q: %v", doc.Timestamp, err)
			}

			// vulnerability and products come from the run inputs, never from the reply.
			want := map[string]any{"vulnerability": map[string]any{"name": cve}, "products": []any{map[string]any{"@id": image}}}
			maps.Copy(want, tc.want)
			if len(doc.Statements) != 1 || !reflect.DeepEqual(doc.Statements[0], want) {
				t.Fatalf("statements = %v, want [%v]", doc.Statements, want)
			}
		})
	}
}
