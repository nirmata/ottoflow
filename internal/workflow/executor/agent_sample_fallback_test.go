/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"context"
	"os"
	"reflect"
	"testing"

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
