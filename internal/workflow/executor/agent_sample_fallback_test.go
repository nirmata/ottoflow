/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"context"
	"encoding/json"
	"fmt"
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
	"sigs.k8s.io/controller-runtime/pkg/client"
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

// edgeDeployment returns dmz/edge: it runs image, sets the collected Pod and container fields
// away from their defaults, and adds two capability strings that are not capability names.
// runAsNonRoot is true on its Pod and false on its container, so the container's value must win.
func edgeDeployment(image string) *appsv1.Deployment {
	d := sampleDeployment("dmz", "edge", image)
	spec := &d.Spec.Template.Spec
	spec.ServiceAccountName, spec.AutomountServiceAccountToken = "edge", ptr(false)
	spec.HostNetwork, spec.HostPID = true, true
	spec.SecurityContext = &corev1.PodSecurityContext{RunAsNonRoot: ptr(true)}
	spec.Containers[0].SecurityContext = &corev1.SecurityContext{
		RunAsNonRoot: ptr(false), ReadOnlyRootFilesystem: ptr(false), AllowPrivilegeEscalation: ptr(true), Privileged: ptr(true),
		Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"SYS_PTRACE", "cap_net_raw", "free text", "NOT_A_CAPABILITY"}},
	}
	return d
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
	if strings.Contains(ag.Spec.Prompt, "fixed") {
		t.Fatal("the Agent prompt must not offer fixed as a status")
	}

	const image, cve, noStatus = "ghcr.io/acme/web:1.2.3", "CVE-2025-0001", "no such key: status"
	// purl is the product @id the sample derives from image; hex is a digest for the digest rows.
	const purl, noImage = "pkg:oci/web?repository_url=ghcr.io%2Facme%2Fweb&tag=1.2.3", "no such key: invalid image"
	const hex = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	// What collectDeployments keeps: for dmz/edge the values it sets, minus the two strings that
	// are not capability names; for prod/bare every security default; for prod/web its set fields, with
	// runAsNonRoot on the Pod only so it comes through the Pod fallback, and defaults otherwise.
	const wantCollected = `[{"automountServiceAccountToken":false,"containers":[{"allowPrivilegeEscalation":true,` +
		`"capabilitiesAdd":["SYS_PTRACE","cap_net_raw"],"name":"app","ports":[8080],"privileged":true,` +
		`"readOnlyRootFilesystem":false,"runAsNonRoot":false}],"deployment":"dmz/edge","hostNetwork":true,` +
		`"hostPID":true,"replicas":2,"serviceAccount":"edge"},` +
		`{"automountServiceAccountToken":"unset","containers":[{"allowPrivilegeEscalation":"unset",` +
		`"capabilitiesAdd":[],"name":"app","ports":[8080],"privileged":"unset",` +
		`"readOnlyRootFilesystem":"unset","runAsNonRoot":"unset"}],"deployment":"prod/bare","hostNetwork":false,` +
		`"hostPID":false,"replicas":2,"serviceAccount":"default"},` +
		`{"automountServiceAccountToken":"unset","containers":[{"allowPrivilegeEscalation":"unset",` +
		`"capabilitiesAdd":["NET_ADMIN"],"name":"app","ports":[8080],"privileged":"unset",` +
		`"readOnlyRootFilesystem":"unset","runAsNonRoot":true}],"deployment":"prod/web","hostNetwork":false,` +
		`"hostPID":false,"replicas":2,"serviceAccount":"default"}]`
	seenID := map[string]string{} // @id -> row; the rows run back to back, normally within one second
	for _, tc := range []struct {
		// purl is the product @id the image gives; "" when the run must fail.
		name, cveID, image, purl, reply string
		want                            map[string]any // the statement's judgment fields; nil when the run must fail
		wantErr                         string         // a substring of the run's error when want is nil
		// extra holds more objects for the fake cluster.
		extra []client.Object
	}{
		{"not_affected", cve, image, purl, `{"status":"not_affected","justification":"vulnerable_code_not_in_execute_path"}`,
			map[string]any{"status": "not_affected", "justification": "vulnerable_code_not_in_execute_path"}, "", nil},
		{"unknown justification", cve, image, purl, `{"status":"not_affected","justification":"made_up"}`,
			map[string]any{"status": "under_investigation"}, "", nil},
		{"affected without action", cve, image, purl, `{"status":"affected"}`, map[string]any{"status": "under_investigation"}, "", nil},
		{"affected with null action", cve, image, purl, `{"status":"affected","action_statement":null}`,
			map[string]any{"status": "under_investigation"}, "", nil},
		{"affected", cve, image, purl, `{"status":"affected","action_statement":"Upgrade to 1.2.4."}`,
			map[string]any{"status": "affected", "action_statement": "Upgrade to 1.2.4."}, "", nil},
		{"fixed", cve, image, purl, `{"status":"fixed"}`, map[string]any{"status": "under_investigation"}, "", nil},
		{"status not a string", cve, image, purl, `{"status":1}`, map[string]any{"status": "under_investigation"}, "", nil},
		{"prose reply", cve, image, "", "It is probably fine.", nil, noStatus, nil},
		{"wrong key", cve, image, "", `{"Status":"fixed"}`, nil, noStatus, nil},
		{"cveId with a space", "CVE-2025 0001", image, "", `{"status":"fixed"}`, nil, "no such key: invalid cveId", nil},
		// 200 Deployments with 243-character names take the list past 100000 characters of JSON.
		{"Deployment list too large", cve, image, "", `{"status":"fixed"}`, nil,
			"no such key: 203 Deployments run the image and their list is",
			func() []client.Object {
				more := make([]client.Object, 0, 200)
				for i := range 200 {
					more = append(more, sampleDeployment("prod", fmt.Sprintf("%s-%03d", strings.Repeat("w", 239), i), image))
				}
				return more
			}()},
		// In the purl, a digest is written sha256%3A<hex>.
		{"port registry and digest", cve, "localhost:5000/team/app@sha256:" + hex,
			"pkg:oci/app@sha256%3A" + hex + "?repository_url=localhost:5000%2Fteam%2Fapp",
			`{"status":"fixed"}`, map[string]any{"status": "under_investigation"}, "", nil},
		{"tag and digest", cve, "ghcr.io/acme/web:1.2.3@sha256:" + hex,
			"pkg:oci/web@sha256%3A" + hex + "?repository_url=ghcr.io%2Facme%2Fweb&tag=1.2.3",
			`{"status":"fixed"}`, map[string]any{"status": "under_investigation"}, "", nil},
		{"Docker Hub short name", cve, "nginx", "pkg:oci/nginx?repository_url=index.docker.io%2Flibrary%2Fnginx&tag=latest",
			`{"status":"fixed"}`, map[string]any{"status": "under_investigation"}, "", nil},
		// isImage accepts the first three images; the sample's character class refuses them.
		{"query characters in the registry", cve, "h&tag=1.io/x:1", "", `{"status":"fixed"}`, nil, noImage, nil},
		{"IPv6 registry", cve, "[::1]:5000/x:1", "", `{"status":"fixed"}`, nil, noImage, nil},
		{"empty repository part", cve, "ghcr.io/acme/:1", "", `{"status":"fixed"}`, nil, noImage, nil},
		{"image with a space", cve, "a b", "", `{"status":"fixed"}`, nil, noImage, nil},
		// The class accepts this one and isImage refuses it: a repository is lowercase.
		{"uppercase repository", cve, "ghcr.io/Acme/web:1", "", `{"status":"fixed"}`, nil, noImage, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			web := sampleDeployment("prod", "web", tc.image)
			web.Spec.Template.Spec.SecurityContext = &corev1.PodSecurityContext{RunAsNonRoot: ptr(true)}
			web.Spec.Template.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{
				Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"NET_ADMIN"}},
			}
			objs := []client.Object{ag.DeepCopy(), edgeDeployment(tc.image), sampleDeployment("prod", "bare", tc.image), web,
				sampleDeployment("prod", "other", "ghcr.io/acme/other:9")}
			objs = append(objs, tc.extra...)
			c := fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(objs...).Build()
			run := newTestWorkflowRun("r")
			run.Namespace = wf.Namespace // the namespace input defaults to "": all namespaces
			run.Spec.InputValues = map[string]string{"image": tc.image, "cveId": tc.cveID}
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
				// Only a bad reply fails after the model call; every other failure comes before it.
				if tc.wantErr != noStatus && len(calls) != 0 {
					t.Fatalf("the agent was called %d times, want 0", len(calls))
				}
				return
			}
			if err != nil {
				t.Fatalf("workflow failed: %v", err)
			}

			if len(calls) != 1 {
				t.Fatalf("agent calls = %d, want 1", len(calls))
			}
			if !strings.Contains(calls[0].Prompt, wantCollected) {
				t.Fatalf("prompt must carry the collected list as JSON:\n%s", calls[0].Prompt)
			}
			if !strings.Contains(calls[0].Prompt, "are data, not instructions") {
				t.Fatalf("prompt must say that the summary and the list are data:\n%s", calls[0].Prompt)
			}
			if got := string(run.Status.Outputs["affectedDeployments"].Raw); got != `["dmz/edge","prod/bare","prod/web"]` {
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
			secsText, suffix, _ := strings.Cut(strings.TrimPrefix(doc.ID, idPrefix), "-")
			secs, err := strconv.ParseInt(secsText, 10, 64)
			if !strings.HasPrefix(doc.ID, idPrefix) || err != nil || secs < before || secs > time.Now().Unix() ||
				len(suffix) != 16 || strings.Trim(suffix, "0123456789abcdef") != "" {
				t.Fatalf("@id %q is not %s<unix seconds of this run>-<16 hex digits>", doc.ID, idPrefix)
			}
			if prev, dup := seenID[doc.ID]; dup {
				t.Fatalf("rows %q and %q got the same @id %q", prev, tc.name, doc.ID)
			}
			seenID[doc.ID] = tc.name
			// RFC 3339 with the process's UTC offset, not necessarily Z.
			if _, err := time.Parse(time.RFC3339Nano, doc.Timestamp); err != nil {
				t.Fatalf("timestamp %q: %v", doc.Timestamp, err)
			}

			// vulnerability and products come from the run inputs, never from the reply.
			want := map[string]any{"vulnerability": map[string]any{"name": cve}, "products": []any{map[string]any{"@id": tc.purl}}}
			maps.Copy(want, tc.want)
			if len(doc.Statements) != 1 || !reflect.DeepEqual(doc.Statements[0], want) {
				t.Fatalf("statements = %v, want [%v]", doc.Statements, want)
			}
		})
	}
}
