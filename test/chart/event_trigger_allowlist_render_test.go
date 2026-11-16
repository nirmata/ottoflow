/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package chart

import (
	"strings"
	"testing"
)

func TestEventTriggerAllowedSourceNamespacesArg(t *testing.T) {
	const flag = "--event-trigger-allowed-source-namespaces"
	if strings.Contains(controllerDeployment(t, renderDocs(t)), flag) {
		t.Errorf("%s rendered with an empty list", flag)
	}
	on := controllerDeployment(t, renderDocs(t, "--set", "controller.eventTriggerAllowedSourceNamespaces={a,b}"))
	if !strings.Contains(on, flag+"=a,b") {
		t.Errorf("expected %s=a,b in the rendered controller args", flag)
	}
}
