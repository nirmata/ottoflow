/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package a2a

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// RenderOutputs converts WorkflowRun status outputs to a human-readable text block.
//
//   - Zero outputs  -> ""
//   - One output    -> just its rendered value
//   - Many outputs  -> one "key: value" line per output, keys sorted for stability
//
// Values are not assumed to be scalars: a JSON string is unwrapped to its raw text,
// anything else (object, array, number, bool) is emitted as its compact JSON.
func RenderOutputs(outputs map[string]apiextensionsv1.JSON) string {
	switch len(outputs) {
	case 0:
		return ""
	case 1:
		for _, v := range outputs {
			return renderValue(v)
		}
	}

	keys := make([]string, 0, len(outputs))
	for k := range outputs {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(k)
		b.WriteString(": ")
		b.WriteString(renderValue(outputs[k]))
	}
	return b.String()
}

// renderValue unwraps a JSON string to its raw text; other JSON kinds are returned
// as their compact JSON form.
func renderValue(v apiextensionsv1.JSON) string {
	raw := bytes.TrimSpace(v.Raw)
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}
