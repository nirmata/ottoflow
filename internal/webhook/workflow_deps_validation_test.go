/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package webhook

import (
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

func TestValidateStepDependencies_LiteralsAndStructuredValues(t *testing.T) {
	cases := []struct {
		name    string
		expr    string
		value   string
		wantErr bool
	}{
		{name: "single-quoted literal is text", expr: `'stepStatus.first.failed'`},
		{name: "mixed quotes literal is text", expr: `"it's " + 'steps.first.x'`},
		{name: "json string value is an expression", value: `{"failed":"stepStatus.first.failed"}`, wantErr: true},
		{name: "json steps value is an expression", value: `{"a":["steps.first.results"]}`, wantErr: true},
		{name: "json value with only a literal is text", value: `{"a":"'steps.first.x'"}`},
		{name: "json literal text is not an expression", value: `{"a":"please read steps.first.x as plain text"}`},
		{name: "json key is not an expression", value: `{"steps.first.x":1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := ottoflowv1alpha1.Output{Name: "o", Expression: tc.expr}
			if tc.value != "" {
				out.Value = &apiextensionsv1.JSON{Raw: []byte(tc.value)}
			}
			first := ottoflowv1alpha1.Step{Name: "first", Outputs: []ottoflowv1alpha1.Output{{Name: "x", Expression: `"ok"`}}}
			plain := ottoflowv1alpha1.Step{Name: "second", Outputs: []ottoflowv1alpha1.Output{out}}
			fe := ottoflowv1alpha1.Step{Name: "fe", ForEach: &ottoflowv1alpha1.StepForEach{
				Items: "[1]", Step: &ottoflowv1alpha1.StepForEachStep{Outputs: []ottoflowv1alpha1.Output{out}},
			}}
			for name, step := range map[string]ottoflowv1alpha1.Step{"step": plain, "forEach": fe} {
				err := ValidateStepDependencies(&ottoflowv1alpha1.WorkflowSpec{Steps: []ottoflowv1alpha1.Step{first, step}})
				if (err != nil) != tc.wantErr {
					t.Errorf("%s: err = %v, wantErr %v", name, err, tc.wantErr)
				}
			}
		})
	}
}
