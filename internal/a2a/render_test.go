/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package a2a

import (
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

func j(raw string) apiextensionsv1.JSON { return apiextensionsv1.JSON{Raw: []byte(raw)} }

func TestRenderOutputs(t *testing.T) {
	tests := []struct {
		name    string
		outputs map[string]apiextensionsv1.JSON
		want    string
	}{
		{
			name:    "empty",
			outputs: nil,
			want:    "",
		},
		{
			name:    "single scalar string is unwrapped",
			outputs: map[string]apiextensionsv1.JSON{"greeting": j(`"hello world"`)},
			want:    "hello world",
		},
		{
			name:    "single object stays compact JSON",
			outputs: map[string]apiextensionsv1.JSON{"data": j(`{"a":1,"b":"x"}`)},
			want:    `{"a":1,"b":"x"}`,
		},
		{
			name: "multiple outputs render sorted key: value lines",
			outputs: map[string]apiextensionsv1.JSON{
				"b": j(`{"n":2}`),
				"a": j(`"x"`),
			},
			want: "a: x\nb: {\"n\":2}",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RenderOutputs(tt.outputs); got != tt.want {
				t.Fatalf("RenderOutputs()\n got: %q\nwant: %q", got, tt.want)
			}
		})
	}
}
