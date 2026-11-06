/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

func TestInstantiateStepTemplateKeepsPrometheusMCPAndWorkflowRefFields(t *testing.T) {
	src := ottoflowv1alpha1.StepTemplateStep{
		PrometheusQuery: &ottoflowv1alpha1.StepPrometheusQuery{
			Query:     "up{job=\"{{.job}}\"}",
			TimeRange: "{{.tr}}",
			Step:      "{{.step}}",
			Variables: map[string]string{"v": "{{.v}}"},
			Outputs:   map[string]string{"o": "{{.o}}"},
		},
		MCPToolCall: &ottoflowv1alpha1.StepMCPToolCall{
			Server:    "srv",
			Tool:      "tool",
			Arguments: map[string]string{"a": "{{.a}}"},
		},
		WorkflowRef: &ottoflowv1alpha1.StepWorkflowRef{
			Name:      "wf",
			Namespace: "ns",
			Inputs:    map[string]string{"i": "{{.i}}"},
		},
	}
	orig := src.DeepCopy()
	args := map[string]interface{}{"job": "j", "tr": "5m", "step": "15s", "v": "1", "o": "2", "a": "3", "i": "4"}

	want := src.DeepCopy()
	want.PrometheusQuery.Query = `up{job="j"}`
	want.PrometheusQuery.TimeRange = "5m"
	want.PrometheusQuery.Step = "15s"
	want.PrometheusQuery.Variables = map[string]string{"v": "1"}
	want.PrometheusQuery.Outputs = map[string]string{"o": "2"}
	want.MCPToolCall.Arguments = map[string]string{"a": "3"}
	want.WorkflowRef.Inputs = map[string]string{"i": "4"}

	got, err := (&WorkflowExecutor{}).instantiateStepTemplate(src, "s1", args)
	if err != nil {
		t.Fatal(err)
	}

	gotRefs := &ottoflowv1alpha1.StepTemplateStep{PrometheusQuery: got.PrometheusQuery, MCPToolCall: got.MCPToolCall, WorkflowRef: got.WorkflowRef}
	if d := cmp.Diff(want, gotRefs); d != "" {
		t.Errorf("copy differs from source plus substitution (-want +got):\n%s", d)
	}

	got.PrometheusQuery.Variables["x"] = "x"
	got.PrometheusQuery.Outputs["x"] = "x"
	got.MCPToolCall.Arguments["x"] = "x"
	got.WorkflowRef.Inputs["x"] = "x"
	if d := cmp.Diff(orig, &src); d != "" {
		t.Errorf("source mutated through the copy (-want +got):\n%s", d)
	}
}
