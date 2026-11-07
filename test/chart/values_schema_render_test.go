/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package chart

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestValuesSchemaRejectsUnknownKeys asserts that values.schema.json closes the top level, the
// webhook, namespace and controller.image objects, so a typo, a misnested key (webhook.metrics belongs
// under controller) or a removed value (crds.install) fails the render instead of being silently
// ignored.
func TestValuesSchemaRejectsUnknownKeys(t *testing.T) {
	cases := map[string]string{
		"unknown top-level key":      "bogusTopLevel: 1\n",
		"misnested webhook key":      "webhook:\n  metrics:\n    enabled: true\n",
		"removed crds.install":       "crds:\n  install: false\n",
		"removed namespace.create":   "namespace:\n  create: true\n",
		"removed image.nameOverride": "controller:\n  image:\n    nameOverride: x\n",
	}
	for name, values := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := renderWithValuesFile(t, writeSecretAccessValues(t, values))
			if err == nil || !strings.Contains(err.Error(), "don't meet the specifications of the schema") {
				t.Errorf("want a values-schema failure for %q, got %v", values, err)
			}
		})
	}
}

// TestE2EValuesFileMatchesSchema keeps the values file CI installs with valid under the closed schema.
func TestE2EValuesFileMatchesSchema(t *testing.T) {
	f := filepath.Join(chartDir(t), "..", "..", "test", "e2e", "helm-values-e2e.yaml")
	if _, err := renderWithValuesFile(t, f); err != nil {
		t.Fatalf("helm template with e2e values failed: %v", err)
	}
}
