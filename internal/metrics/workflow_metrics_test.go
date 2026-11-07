/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package metrics

import (
	"testing"

	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Test workflow metrics can be used without panicking (covers init and metric vectors).
func TestWorkflowMetrics_RecordSample(t *testing.T) {
	WorkflowRunsTotal.WithLabelValues("wf", "default", "succeeded").Inc()
	WorkflowRunsTotal.WithLabelValues("wf", "default", "failed").Add(0)

	WorkflowRunDurationSeconds.WithLabelValues("wf", "default").Observe(1.5)
	WorkflowStepsTotal.WithLabelValues("wf", "default", "step1", "succeeded").Inc()
	WorkflowStepDurationSeconds.WithLabelValues("wf", "default", "step1").Observe(0.25)
	WorkflowRunsActive.WithLabelValues("wf", "default").Set(1)
	WorkflowRunsActive.WithLabelValues("wf", "default").Dec()
}

// The six workflow metrics have to sit on the registry /metrics serves.
// Registering them on prometheus.DefaultRegisterer compiles and runs fine and
// exposes nothing.
func TestWorkflowMetricsAreOnTheServedRegistry(t *testing.T) {
	// A *Vec reports no family until a label combination is used.
	WorkflowRunsTotal.WithLabelValues("wf", "default", "succeeded").Inc()
	WorkflowRunDurationSeconds.WithLabelValues("wf", "default").Observe(1)
	WorkflowStepsTotal.WithLabelValues("wf", "default", "s", "succeeded").Inc()
	WorkflowStepDurationSeconds.WithLabelValues("wf", "default", "s").Observe(1)
	WorkflowRunsActive.WithLabelValues("wf", "default").Set(1)
	WorkflowTriggerEventsDroppedTotal.WithLabelValues("wf", "default", "api_error").Inc()

	families, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gathering: %v", err)
	}

	found := map[string]bool{}
	for _, f := range families {
		found[f.GetName()] = true
	}

	for _, name := range []string{
		"ottoflow_workflow_runs_total",
		"ottoflow_workflow_run_duration_seconds",
		"ottoflow_workflow_steps_total",
		"ottoflow_workflow_step_duration_seconds",
		"ottoflow_workflow_runs_active",
		"ottoflow_workflow_trigger_events_dropped_total",
	} {
		if !found[name] {
			t.Errorf("%s is not registered on controller-runtime's registry", name)
		}
	}
}
