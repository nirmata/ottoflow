/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"context"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/metrics"
)

// activeResyncInterval is how often the active-runs gauge is recomputed.
const activeResyncInterval = 30 * time.Second

// RunMetrics records the ottoflow_workflow_* metrics from WorkflowRun status.
//
// The executor increments them today, but it runs in the runner Job, which
// serves no HTTP and then exits, so nothing has ever been able to scrape them.
// Everything they need is already in status, which the runner writes and the
// controller watches.
type RunMetrics struct {
	cache  ctrlcache.Cache
	client client.Client
	// seen is every label set the gauge has reported, so one whose runs have
	// all been deleted can be dropped instead of reporting its last value
	// forever. Only syncActive touches it, from one goroutine.
	seen map[[2]string]struct{}
}

func NewRunMetrics(c ctrlcache.Cache, k8s client.Client) *RunMetrics {
	return &RunMetrics{cache: c, client: k8s, seen: map[[2]string]struct{}{}}
}

// NeedLeaderElection keeps one recorder per cluster. Counters are per-process,
// so every replica recording every run would multiply them by replica count.
func (m *RunMetrics) NeedLeaderElection() bool { return true }

func (m *RunMetrics) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("run-metrics")

	informer, err := m.cache.GetInformer(ctx, &ottoflowv1alpha1.WorkflowRun{})
	if err != nil {
		return err
	}
	// Only updates are handled. A restart replays every cached run as an add,
	// and counting those would re-count runs already counted before it.
	if _, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj any) {
			old, oldOK := oldObj.(*ottoflowv1alpha1.WorkflowRun)
			current, newOK := newObj.(*ottoflowv1alpha1.WorkflowRun)
			if oldOK && newOK {
				recordRunTransition(old, current)
			}
		},
	}); err != nil {
		return err
	}

	ticker := time.NewTicker(activeResyncInterval)
	defer ticker.Stop()
	for {
		if err := m.syncActive(ctx); err != nil {
			logger.Error(err, "recomputing active runs")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// recordRunTransition records a run that has just reached a terminal phase,
// and the steps it finished with.
func recordRunTransition(old, current *ottoflowv1alpha1.WorkflowRun) {
	// Only the crossing into a terminal phase counts. Requiring the old phase
	// to be non-terminal keeps a terminal-to-terminal update from counting the
	// run twice — a cron run marked Failed for Replace whose runner then
	// persists Succeeded is one.
	if old.Status.Phase.IsTerminal() || !current.Status.Phase.IsTerminal() {
		return
	}

	workflow, namespace := current.Spec.WorkflowRef.Name, current.Namespace
	metrics.WorkflowRunsTotal.WithLabelValues(workflow, namespace, runResult(current)).Inc()
	if d, ok := elapsed(current.Status.StartTime, runCompletion(current)); ok {
		metrics.WorkflowRunDurationSeconds.WithLabelValues(workflow, namespace).Observe(d)
	}

	for name, step := range current.Status.StepStatuses {
		result, ran := stepResult(step)
		if !ran {
			continue
		}
		metrics.WorkflowStepsTotal.WithLabelValues(workflow, namespace, name, result).Inc()
		if d, ok := elapsed(step.StartTime, step.CompletionTime); ok {
			metrics.WorkflowStepDurationSeconds.WithLabelValues(workflow, namespace, name).Observe(d)
		}
	}
}

// runCompletion is when the run finished. A failing run is persisted through
// persistExecutionFailure, which sets only the execution completion time, so
// reading the top-level one alone drops the duration of every failure.
func runCompletion(run *ottoflowv1alpha1.WorkflowRun) *metav1.Time {
	if run.Status.CompletionTime != nil {
		return run.Status.CompletionTime
	}
	if run.Status.Execution != nil {
		return run.Status.Execution.CompletionTime
	}
	return nil
}

// syncActive recomputes the active gauge from the cache. A gauge tracked by
// increments drifts past any event the controller did not see; recomputing it
// cannot.
func (m *RunMetrics) syncActive(ctx context.Context) error {
	var runs ottoflowv1alpha1.WorkflowRunList
	if err := m.client.List(ctx, &runs); err != nil {
		return err
	}

	active := map[[2]string]float64{}
	for i := range runs.Items {
		run := &runs.Items[i]
		key := [2]string{run.Spec.WorkflowRef.Name, run.Namespace}
		if _, seen := active[key]; !seen {
			active[key] = 0
		}
		if run.Status.Phase == ottoflowv1alpha1.WorkflowRunPhaseRunning {
			active[key]++
		}
	}
	for key, count := range active {
		metrics.WorkflowRunsActive.WithLabelValues(key[0], key[1]).Set(count)
		m.seen[key] = struct{}{}
	}
	for key := range m.seen {
		if _, present := active[key]; !present {
			metrics.WorkflowRunsActive.DeleteLabelValues(key[0], key[1])
			delete(m.seen, key)
		}
	}
	return nil
}

func runResult(run *ottoflowv1alpha1.WorkflowRun) string {
	if run.Status.Phase == ottoflowv1alpha1.WorkflowRunPhaseSucceeded {
		return "succeeded"
	}
	return "failed"
}

// stepResult reports how a step ended, and whether it ended at all. Every
// declared step is initialized to Pending before execution, so a run that fails
// early leaves its downstream steps sitting there: counting those as failures
// would put steps that never ran into the failure rate.
func stepResult(step ottoflowv1alpha1.StepStatus) (result string, ran bool) {
	switch step.Phase {
	case ottoflowv1alpha1.StepPhaseSucceeded:
		return "succeeded", true
	case ottoflowv1alpha1.StepPhaseSkipped:
		return "skipped", true
	case ottoflowv1alpha1.StepPhaseFailed:
		return "failed", true
	default:
		return "", false
	}
}

func elapsed(start, end *metav1.Time) (float64, bool) {
	if start == nil || end == nil || start.IsZero() || end.IsZero() {
		return 0, false
	}
	d := end.Sub(start.Time)
	if d < 0 {
		return 0, false
	}
	return d.Seconds(), true
}
