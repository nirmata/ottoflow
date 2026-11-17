/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"context"
	"maps"
	"os"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

func TestEventTriggerSamples_RequireInputs(t *testing.T) {
	for _, tc := range []struct {
		file, workflow, output string
		inputs                 map[string]string
	}{
		{
			file:     "../../../samples/workflows/features/argocd-event-trigger.yaml",
			workflow: "argocd-sync-policy-check",
			output:   "syncMessage",
			inputs:   map[string]string{"appName": "guestbook", "appNamespace": "prod-ns"},
		},
		{
			file:     "../../../samples/workflows/features/fluxcd-event-trigger.yaml",
			workflow: "fluxcd-reconcile-compliance",
			output:   "reconcileMessage",
			inputs:   map[string]string{"kustomizationName": "apps", "appliedRevision": "main@sha1:abc123"},
		},
	} {
		t.Run(tc.workflow, func(t *testing.T) {
			manifest, err := os.ReadFile(tc.file)
			if err != nil {
				t.Fatal(err)
			}
			exec := NewLocalWorkflowExecutor(fake.NewClientBuilder().Build(), "", 5, "", "")
			if err := exec.LoadFromReader(strings.NewReader(string(manifest))); err != nil {
				t.Fatalf("LoadFromReader: %v", err)
			}

			run, err := exec.ExecuteWorkflow(context.Background(), tc.workflow, "ottoflow", nil)
			if err == nil || !strings.Contains(err.Error(), "required input") {
				t.Fatalf("expected a 'required input' error with no inputs, got: %v", err)
			}
			if run == nil || run.Status.Phase != ottoflowv1alpha1.WorkflowRunPhaseFailed {
				t.Fatalf("expected Phase Failed with no inputs, got: %+v", run)
			}

			for missing := range tc.inputs {
				partial := maps.Clone(tc.inputs)
				delete(partial, missing)
				_, err := exec.ExecuteWorkflow(context.Background(), tc.workflow, "ottoflow", partial)
				if err == nil || !strings.Contains(err.Error(), "required input "+missing) {
					t.Errorf("expected a 'required input %s' error, got: %v", missing, err)
				}
			}

			run, err = exec.ExecuteWorkflow(context.Background(), tc.workflow, "ottoflow", tc.inputs)
			if err != nil {
				t.Fatalf("ExecuteWorkflow with inputs: %v", err)
			}
			if run.Status.Phase != ottoflowv1alpha1.WorkflowRunPhaseSucceeded {
				t.Fatalf("expected Phase Succeeded with inputs, got: %q", run.Status.Phase)
			}
			raw, ok := run.Status.Outputs[tc.output]
			if !ok {
				t.Fatalf("expected %q workflow output, got: %v", tc.output, run.Status.Outputs)
			}
			for _, v := range tc.inputs {
				if !strings.Contains(string(raw.Raw), v) {
					t.Errorf("expected output to contain %q, got: %s", v, raw.Raw)
				}
			}
		})
	}
}
