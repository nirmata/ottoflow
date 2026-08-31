/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package chart

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"sigs.k8s.io/yaml"
)

// readValues loads values.yaml as a generic map.
func readValues(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(chartDir(t), "values.yaml"))
	if err != nil {
		t.Fatalf("reading values.yaml: %v", err)
	}
	var values map[string]any
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatalf("parsing values.yaml: %v", err)
	}
	return values
}

// readSchema loads values.schema.json.
func readSchema(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(chartDir(t), "values.schema.json"))
	if err != nil {
		t.Fatalf("reading values.schema.json: %v", err)
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("parsing values.schema.json: %v", err)
	}
	return schema
}

// TestSchemaDescribesEveryValue fails when a key is added to values.yaml and
// not to the schema. With additionalProperties false, that combination makes
// the chart refuse its own defaults, so it has to be caught here rather than at
// someone's install.
func TestSchemaDescribesEveryValue(t *testing.T) {
	missing := diffKeys(readValues(t), properties(readSchema(t)), "")
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("values.yaml keys absent from values.schema.json: %v", missing)
	}
}

// TestSchemaDescribesNothingExtra fails when the schema keeps describing a
// value that values.yaml no longer declares. A key documented in the schema
// and set by nobody is the shape dead values take once they are removed.
func TestSchemaDescribesNothingExtra(t *testing.T) {
	extra := diffKeys(properties(readSchema(t)), readValues(t), "")
	if len(extra) > 0 {
		sort.Strings(extra)
		t.Errorf("values.schema.json keys absent from values.yaml: %v", extra)
	}
}

// TestSchemaIsClosed asserts the top level rejects unknown keys. Without it a
// value set at a path no template reads is accepted and silently ignored,
// which is how metrics ended up nested under webhook.
func TestSchemaIsClosed(t *testing.T) {
	schema := readSchema(t)
	if closed, _ := schema["additionalProperties"].(bool); closed {
		t.Error("values.schema.json allows additional top-level properties")
	}
	if _, ok := schema["additionalProperties"]; !ok {
		t.Error("values.schema.json does not set additionalProperties at the top level")
	}
}

// properties reduces a schema node to the value shape its properties describe,
// so the two trees can be walked together. Nodes that deliberately accept
// arbitrary keys (Kubernetes passthrough like resources or nodeSelector)
// declare no properties and compare as leaves.
func properties(node map[string]any) map[string]any {
	props, ok := node["properties"].(map[string]any)
	if !ok {
		return map[string]any{}
	}
	out := make(map[string]any, len(props))
	for name, raw := range props {
		child, ok := raw.(map[string]any)
		if !ok {
			out[name] = nil
			continue
		}
		if nested := properties(child); len(nested) > 0 {
			out[name] = nested
			continue
		}
		out[name] = nil
	}
	return out
}

// diffKeys returns the paths present in want and absent from got.
func diffKeys(want, got map[string]any, prefix string) []string {
	var missing []string
	for name, value := range want {
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		other, present := got[name]
		if !present {
			missing = append(missing, path)
			continue
		}
		wantChild, wantNested := value.(map[string]any)
		gotChild, gotNested := other.(map[string]any)
		if wantNested && gotNested {
			missing = append(missing, diffKeys(wantChild, gotChild, path)...)
		}
	}
	return missing
}
