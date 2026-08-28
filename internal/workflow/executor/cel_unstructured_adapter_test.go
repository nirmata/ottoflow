/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"net/netip"
	"testing"

	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/common/types/traits"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestUnstructuredAdapter(t *testing.T) {
	adapter := unstructuredAdapter{base: types.DefaultTypeAdapter}

	tests := []struct {
		name  string
		input any
		check func(t *testing.T, result ref.Val)
	}{
		{
			name:  "value unstructured.Unstructured unboxes to map",
			input: unstructured.Unstructured{Object: map[string]any{"a": "b"}},
			check: func(t *testing.T, result ref.Val) {
				mapper, ok := result.(traits.Mapper)
				if !ok {
					t.Fatalf("expected traits.Mapper, got %T", result)
				}
				got := mapper.Get(types.String("a"))
				if got.Equal(types.String("b")) != types.True {
					t.Fatalf("expected value %q, got %v", "b", got)
				}
			},
		},
		{
			name:  "pointer *unstructured.Unstructured unboxes to map",
			input: &unstructured.Unstructured{Object: map[string]any{"a": "b"}},
			check: func(t *testing.T, result ref.Val) {
				mapper, ok := result.(traits.Mapper)
				if !ok {
					t.Fatalf("expected traits.Mapper, got %T", result)
				}
				got := mapper.Get(types.String("a"))
				if got.Equal(types.String("b")) != types.True {
					t.Fatalf("expected value %q, got %v", "b", got)
				}
			},
		},
		{
			name:  "typed nil *unstructured.Unstructured returns NullValue",
			input: (*unstructured.Unstructured)(nil),
			check: func(t *testing.T, result ref.Val) {
				if result != types.NullValue {
					t.Fatalf("expected types.NullValue, got %v (%T)", result, result)
				}
			},
		},
		{
			name:  "passthrough string",
			input: "x",
			check: func(t *testing.T, result ref.Val) {
				if result.Equal(types.String("x")) != types.True {
					t.Fatalf("expected string %q, got %v", "x", result)
				}
			},
		},
		{
			name:  "passthrough map",
			input: map[string]any{"k": "v"},
			check: func(t *testing.T, result ref.Val) {
				if _, ok := result.(traits.Mapper); !ok {
					t.Fatalf("expected traits.Mapper, got %T", result)
				}
			},
		},
		// cidr()/ip()/quantity() outputs are stored as these natives; the adapter
		// must render each as its canonical string (== its JSON form), not error.
		{
			name:  "netip.Prefix (cidr masked output) stringifies",
			input: netip.MustParsePrefix("192.168.0.0/16"),
			check: func(t *testing.T, result ref.Val) {
				if result.Equal(types.String("192.168.0.0/16")) != types.True {
					t.Fatalf("expected %q, got %v (%T)", "192.168.0.0/16", result, result)
				}
			},
		},
		{
			name:  "netip.Addr (ip output) stringifies",
			input: netip.MustParseAddr("10.0.0.5"),
			check: func(t *testing.T, result ref.Val) {
				if result.Equal(types.String("10.0.0.5")) != types.True {
					t.Fatalf("expected %q, got %v (%T)", "10.0.0.5", result, result)
				}
			},
		},
		{
			name:  "*resource.Quantity stringifies",
			input: resource.NewQuantity(2*1024*1024*1024, resource.BinarySI),
			check: func(t *testing.T, result ref.Val) {
				if result.Equal(types.String("2Gi")) != types.True {
					t.Fatalf("expected %q, got %v (%T)", "2Gi", result, result)
				}
			},
		},
		{
			name:  "typed nil *resource.Quantity returns NullValue, does not panic on String()",
			input: (*resource.Quantity)(nil),
			check: func(t *testing.T, result ref.Val) {
				if result != types.NullValue {
					t.Fatalf("expected types.NullValue, got %v (%T)", result, result)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := adapter.NativeToValue(tt.input)
			tt.check(t, result)
		})
	}
}
