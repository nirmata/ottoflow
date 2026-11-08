/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	metricPrefix = "ottoflow_workflow_"
)

var (
	// WorkflowRunsTotal counts WorkflowRuns by phase (succeeded, failed)
	WorkflowRunsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: metricPrefix + "runs_total",
			Help: "Total number of WorkflowRuns by phase",
		},
		[]string{"workflow", "namespace", "phase"},
	)

	// WorkflowTriggerEventsDroppedTotal counts event-trigger events dropped because the WorkflowRun
	// create path returned an error (after retrying transient Create errors). A run may still
	// exist if a Create landed but its response was lost. reason: api_error.
	WorkflowTriggerEventsDroppedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: metricPrefix + "trigger_events_dropped_total",
			Help: "Total number of event-trigger events dropped because the WorkflowRun create path returned an error, by reason (api_error)",
		},
		[]string{"workflow", "namespace", "reason"},
	)

	// WorkflowRunDurationSeconds is a histogram of workflow execution duration
	WorkflowRunDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    metricPrefix + "run_duration_seconds",
			Help:    "Duration of workflow execution in seconds",
			Buckets: []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300},
		},
		[]string{"workflow", "namespace"},
	)

	// WorkflowStepsTotal counts step executions by phase
	WorkflowStepsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: metricPrefix + "steps_total",
			Help: "Total number of step executions by phase",
		},
		[]string{"workflow", "namespace", "step", "phase"},
	)

	// WorkflowStepDurationSeconds is a histogram of step execution duration
	WorkflowStepDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    metricPrefix + "step_duration_seconds",
			Help:    "Duration of step execution in seconds",
			Buckets: []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300},
		},
		[]string{"workflow", "namespace", "step"},
	)

	// WorkflowRunsActive is a gauge of currently running WorkflowRuns
	WorkflowRunsActive = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: metricPrefix + "runs_active",
			Help: "Number of WorkflowRuns currently in Running phase",
		},
		[]string{"workflow", "namespace"},
	)
)

// Registered on controller-runtime's registry, which is what /metrics serves.
func init() {
	ctrlmetrics.Registry.MustRegister(
		WorkflowRunsTotal,
		WorkflowRunDurationSeconds,
		WorkflowStepsTotal,
		WorkflowStepDurationSeconds,
		WorkflowRunsActive,
		WorkflowTriggerEventsDroppedTotal,
	)
}
