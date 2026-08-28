/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"net/netip"

	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// unstructuredAdapter closes two gaps cel-go 0.31 left in native->ref.Val
// conversion, then delegates to the base adapter:
//
//   - unboxes *unstructured.Unstructured to its plain map (0.31 dropped the
//     struct-wrapping fallback, so the base adapter errors on it);
//   - stringifies the Go natives cidr()/ip()/quantity() leave behind
//     (netip.Prefix, netip.Addr, *resource.Quantity), which the base adapter
//     can't convert back when a step re-reads them as an output.
//
// Only these three are stringified: for each, String() equals its JSON
// encoding, so an in-memory read matches the value reloaded after the step
// context is checkpointed. Do NOT add a type whose String() differs from its
// JSON form (e.g. url() -> *url.URL, a JSON struct) -- it would read as a
// string before a checkpoint and a map after.
//
// Known limit: a typed Unstructured nested in a wholesale-converted container
// (e.g. []any{u1,u2}) is not unboxed; no code path binds that shape today.
type unstructuredAdapter struct {
	base types.Adapter
}

func (a unstructuredAdapter) NativeToValue(value any) ref.Val {
	switch v := value.(type) {
	case unstructured.Unstructured:
		return a.base.NativeToValue(v.Object)
	case *unstructured.Unstructured:
		if v == nil {
			return types.NullValue // explicit: a typed nil ptr is NOT a nil interface
		}
		return a.base.NativeToValue(v.Object)
	case netip.Prefix:
		return types.String(v.String())
	case netip.Addr:
		return types.String(v.String())
	case *resource.Quantity:
		if v == nil {
			return types.NullValue
		}
		return types.String(v.String())
	case resource.Quantity:
		return types.String(v.String())
	default:
		return a.base.NativeToValue(value)
	}
}
