/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package cmd

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/workflow/executor"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// recordingPrometheusClient records every query it receives and returns empty results.
// Used to verify that CEL expressions build the correct Prometheus query strings.
type recordingPrometheusClient struct {
	queries []string
}

func (c *recordingPrometheusClient) Query(
	_ context.Context, query string, _ time.Time,
) (executor.PrometheusResult, error) {
	c.queries = append(c.queries, query)
	return &emptyPrometheusResult{}, nil
}

// emptyPrometheusResult satisfies executor.PrometheusResult with no samples.
type emptyPrometheusResult struct{}

func (r *emptyPrometheusResult) Type() string                           { return "vector" }
func (r *emptyPrometheusResult) GetVector() []executor.PrometheusSample { return nil }
func (r *emptyPrometheusResult) GetScalar() float64                     { return 0 }

// newTestCELEvaluator creates a CELEvaluator backed by a fake k8s client.
func newTestCELEvaluator(t *testing.T, promClient executor.PrometheusClient) *executor.CELEvaluator {
	t.Helper()
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(ottoflowv1alpha1.AddToScheme(scheme))
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	wfRun := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "test-run", Namespace: "default"},
	}
	eval, err := executor.NewCELEvaluatorWithMetrics(k8sClient, nil, nil, promClient, nil, wfRun, 0, nil)
	if err != nil {
		t.Fatalf("NewCELEvaluatorWithMetrics: %v", err)
	}
	return eval
}

// findStep returns the named step from a workflow, or fails the test.
func findStep(t *testing.T, wf *ottoflowv1alpha1.Workflow, name string) ottoflowv1alpha1.Step {
	t.Helper()
	for _, s := range wf.Spec.Steps {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("step %q not found in workflow", name)
	return ottoflowv1alpha1.Step{}
}

// buildVars constructs the standard vars map for CEL evaluation with provided inputs and variables.
func buildVars(inputs, variables map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"inputs":      inputs,
		"variables":   variables,
		"expressions": map[string]interface{}{},
		"steps":       map[string]interface{}{},
		"outputs":     map[string]interface{}{},
	}
}

// podComparison builds a mock pod comparison entry as it would come from collectMetrics.
func podComparison(
	podID string,
	actualCPU, cpuP95, requestedCPU, actualMem, memP95, memPeak, requestedMem float64,
) map[string]interface{} {
	return map[string]interface{}{
		"podId":                podID,
		"name":                 podID,
		"namespace":            "default",
		"actualCPUMilli":       actualCPU,
		"cpuP95Milli":          cpuP95,
		"requestedCPUMilli":    requestedCPU,
		"actualMemoryBytes":    actualMem,
		"memP95Bytes":          memP95,
		"memPeakBytes":         memPeak,
		"requestedMemoryBytes": requestedMem,
		"matched":              true,
	}
}

const overProvisionThreshold = "0.5"

// evalAnalyzeRightSizing runs the analyzeRightSizing step with the given podComparisons.
func evalAnalyzeRightSizing(t *testing.T, pods []interface{}) map[string]interface{} {
	t.Helper()
	wf, err := loadWorkflowFromFile("../../samples/workflows/production/cost-analyzer.yaml")
	if err != nil {
		t.Fatalf("load cost-analyzer.yaml: %v", err)
	}
	step := findStep(t, wf, "analyzeRightSizing")
	eval := newTestCELEvaluator(t, nil)

	vars := buildVars(
		map[string]interface{}{
			"overProvisionThreshold": overProvisionThreshold,
			"cpuCostPerCore":         "0.048",
			"memoryCostPerGB":        "0.006",
			"metricsWindow":          "24h",
		},
		map[string]interface{}{
			"podComparisons":          pods,
			"prometheusDataAvailable": true,
			"prometheusAvailable":     true,
			"p95DataAvailable":        true,
			"metricsFailed":           0,
			"metricsFailedNamespaces": []interface{}{},
			"activeNamespaces":        []interface{}{"default"},
		},
	)

	results, err := eval.EvaluateStepExpressions(context.Background(), step, vars)
	if err != nil {
		t.Fatalf("EvaluateStepExpressions(analyzeRightSizing): %v", err)
	}
	return results
}

func getListField(t *testing.T, results map[string]interface{}, key string) []interface{} {
	t.Helper()
	v, ok := results[key]
	if !ok {
		t.Fatalf("result key %q not found", key)
	}
	list, ok := v.([]interface{})
	if !ok {
		t.Fatalf("result[%q] is %T, want []interface{}", key, v)
	}
	return list
}

func getMapField(t *testing.T, item interface{}, field string) float64 {
	t.Helper()
	m, ok := item.(map[string]interface{})
	if !ok {
		t.Fatalf("item is %T, want map[string]interface{}", item)
	}
	v, ok := m[field]
	if !ok {
		t.Fatalf("field %q not found in item", field)
	}
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("field %q is %T, want float64", field, v)
	}
	return f
}

func approxEqual(a, b, epsilon float64) bool {
	return math.Abs(a-b) < epsilon
}

// getIntCountField reads a size()-derived field (e.g. podCount), which CEL surfaces as
// int64 rather than float64.
func getIntCountField(t *testing.T, item interface{}, field string) int64 {
	t.Helper()
	m, ok := item.(map[string]interface{})
	if !ok {
		t.Fatalf("item is %T, want map[string]interface{}", item)
	}
	v, ok := m[field]
	if !ok {
		t.Fatalf("field %q not found in item", field)
	}
	i, ok := v.(int64)
	if !ok {
		t.Fatalf("field %q is %T, want int64", field, v)
	}
	return i
}

// TestCostAnalyzer_P95CPU_UsedWhenAvailable verifies that when cpuP95Milli > 0,
// the recommendation uses P95 * 1.1 rather than actual * 1.3.
func TestCostAnalyzer_P95CPU_UsedWhenAvailable(t *testing.T) {
	pods := []interface{}{
		podComparison("default/pod-a",
			/*actualCPU=*/ 80.0 /*cpuP95=*/, 120.0 /*requestedCPU=*/, 500.0,
			/*actualMem=*/ 50e6 /*memP95=*/, 0.0 /*memPeak=*/, 0.0 /*requestedMem=*/, 100e6),
	}
	results := evalAnalyzeRightSizing(t, pods)

	cpuPods := getListField(t, results, "cpuOverProvisionedPods")
	if len(cpuPods) != 1 {
		t.Fatalf("expected 1 CPU over-provisioned pod, got %d", len(cpuPods))
	}
	got := getMapField(t, cpuPods[0], "recommendedCPUMilli")
	want := 120.0 * 1.1
	if !approxEqual(got, want, 0.01) {
		t.Errorf("recommendedCPUMilli = %.4f, want P95*1.1 = %.4f (not actual*1.3 = %.4f)",
			got, want, 80.0*1.3)
	}
}

// TestCostAnalyzer_P95CPU_FallbackToActual verifies that when cpuP95Milli == 0,
// the recommendation falls back to actual * 1.3.
func TestCostAnalyzer_P95CPU_FallbackToActual(t *testing.T) {
	pods := []interface{}{
		podComparison("default/pod-b",
			/*actualCPU=*/ 100.0 /*cpuP95=*/, 0.0 /*requestedCPU=*/, 500.0,
			/*actualMem=*/ 50e6 /*memP95=*/, 0.0 /*memPeak=*/, 0.0 /*requestedMem=*/, 100e6),
	}
	results := evalAnalyzeRightSizing(t, pods)

	cpuPods := getListField(t, results, "cpuOverProvisionedPods")
	if len(cpuPods) != 1 {
		t.Fatalf("expected 1 CPU over-provisioned pod, got %d", len(cpuPods))
	}
	got := getMapField(t, cpuPods[0], "recommendedCPUMilli")
	want := 100.0 * 1.3
	if !approxEqual(got, want, 0.01) {
		t.Errorf("recommendedCPUMilli = %.4f, want actual*1.3 = %.4f", got, want)
	}
}

// TestCostAnalyzer_P95Memory_UsedWhenAvailable verifies that when memP95Bytes > 0,
// the memory recommendation uses P95 * 1.1 rather than actual * 1.3.
func TestCostAnalyzer_P95Memory_UsedWhenAvailable(t *testing.T) {
	pods := []interface{}{
		podComparison("default/pod-c",
			/*actualCPU=*/ 80.0 /*cpuP95=*/, 0.0 /*requestedCPU=*/, 100.0,
			/*actualMem=*/ 100e6 /*memP95=*/, 150e6 /*memPeak=*/, 0.0 /*requestedMem=*/, 500e6),
	}
	results := evalAnalyzeRightSizing(t, pods)

	memPods := getListField(t, results, "memoryOverProvisionedPods")
	if len(memPods) != 1 {
		t.Fatalf("expected 1 memory over-provisioned pod, got %d", len(memPods))
	}
	got := getMapField(t, memPods[0], "recommendedMemoryBytes")
	want := 150e6 * 1.1
	if !approxEqual(got, want, 1.0) {
		t.Errorf("recommendedMemoryBytes = %.0f, want P95*1.1 = %.0f (not actual*1.3 = %.0f)",
			got, want, 100e6*1.3)
	}
}

// TestCostAnalyzer_P95Memory_FallbackToActual verifies fallback when memP95Bytes == 0.
func TestCostAnalyzer_P95Memory_FallbackToActual(t *testing.T) {
	pods := []interface{}{
		podComparison("default/pod-d",
			/*actualCPU=*/ 80.0 /*cpuP95=*/, 0.0 /*requestedCPU=*/, 100.0,
			/*actualMem=*/ 100e6 /*memP95=*/, 0.0 /*memPeak=*/, 0.0 /*requestedMem=*/, 500e6),
	}
	results := evalAnalyzeRightSizing(t, pods)

	memPods := getListField(t, results, "memoryOverProvisionedPods")
	if len(memPods) != 1 {
		t.Fatalf("expected 1 memory over-provisioned pod, got %d", len(memPods))
	}
	got := getMapField(t, memPods[0], "recommendedMemoryBytes")
	want := 100e6 * 1.3
	if !approxEqual(got, want, 1.0) {
		t.Errorf("recommendedMemoryBytes = %.0f, want actual*1.3 = %.0f", got, want)
	}
}

// TestCostAnalyzer_PeakMemory_FromPeak verifies peakMemoryBytes uses memPeakBytes when > 0.
func TestCostAnalyzer_PeakMemory_FromPeak(t *testing.T) {
	pods := []interface{}{
		podComparison("default/pod-e",
			/*actualCPU=*/ 80.0 /*cpuP95=*/, 0.0 /*requestedCPU=*/, 100.0,
			/*actualMem=*/ 100e6 /*memP95=*/, 150e6 /*memPeak=*/, 200e6 /*requestedMem=*/, 500e6),
	}
	results := evalAnalyzeRightSizing(t, pods)

	memPods := getListField(t, results, "memoryOverProvisionedPods")
	if len(memPods) != 1 {
		t.Fatalf("expected 1 memory over-provisioned pod, got %d", len(memPods))
	}
	got := getMapField(t, memPods[0], "peakMemoryBytes")
	want := 200e6
	if !approxEqual(got, want, 1.0) {
		t.Errorf("peakMemoryBytes = %.0f, want memPeakBytes = %.0f", got, want)
	}
}

// TestCostAnalyzer_PeakMemory_FallbackToP95 verifies peakMemoryBytes falls back to memP95Bytes.
func TestCostAnalyzer_PeakMemory_FallbackToP95(t *testing.T) {
	pods := []interface{}{
		podComparison("default/pod-f",
			/*actualCPU=*/ 80.0 /*cpuP95=*/, 0.0 /*requestedCPU=*/, 100.0,
			/*actualMem=*/ 100e6 /*memP95=*/, 150e6 /*memPeak=*/, 0.0 /*requestedMem=*/, 500e6),
	}
	results := evalAnalyzeRightSizing(t, pods)

	memPods := getListField(t, results, "memoryOverProvisionedPods")
	if len(memPods) != 1 {
		t.Fatalf("expected 1 memory over-provisioned pod, got %d", len(memPods))
	}
	got := getMapField(t, memPods[0], "peakMemoryBytes")
	want := 150e6
	if !approxEqual(got, want, 1.0) {
		t.Errorf("peakMemoryBytes = %.0f, want memP95Bytes = %.0f", got, want)
	}
}

// TestCostAnalyzer_PeakMemory_FallbackToActual verifies peakMemoryBytes falls back to actual
// when both peak and P95 are zero.
func TestCostAnalyzer_PeakMemory_FallbackToActual(t *testing.T) {
	pods := []interface{}{
		podComparison("default/pod-g",
			/*actualCPU=*/ 80.0 /*cpuP95=*/, 0.0 /*requestedCPU=*/, 100.0,
			/*actualMem=*/ 100e6 /*memP95=*/, 0.0 /*memPeak=*/, 0.0 /*requestedMem=*/, 500e6),
	}
	results := evalAnalyzeRightSizing(t, pods)

	memPods := getListField(t, results, "memoryOverProvisionedPods")
	if len(memPods) != 1 {
		t.Fatalf("expected 1 memory over-provisioned pod, got %d", len(memPods))
	}
	got := getMapField(t, memPods[0], "peakMemoryBytes")
	want := 100e6
	if !approxEqual(got, want, 1.0) {
		t.Errorf("peakMemoryBytes = %.0f, want actualMemoryBytes = %.0f", got, want)
	}
}

// TestCostAnalyzer_NotOverProvisionedWhenUtilizationHigh verifies that pods with
// high utilization (above threshold) are not flagged as over-provisioned.
func TestCostAnalyzer_NotOverProvisionedWhenUtilizationHigh(t *testing.T) {
	pods := []interface{}{
		podComparison("default/pod-h",
			/*actualCPU=*/ 400.0 /*cpuP95=*/, 0.0 /*requestedCPU=*/, 500.0,
			/*actualMem=*/ 450e6 /*memP95=*/, 0.0 /*memPeak=*/, 0.0 /*requestedMem=*/, 500e6),
	}
	results := evalAnalyzeRightSizing(t, pods)

	cpuPods := getListField(t, results, "cpuOverProvisionedPods")
	memPods := getListField(t, results, "memoryOverProvisionedPods")
	if len(cpuPods) != 0 {
		t.Errorf("expected 0 CPU over-provisioned pods for high utilization, got %d", len(cpuPods))
	}
	if len(memPods) != 0 {
		t.Errorf("expected 0 memory over-provisioned pods for high utilization, got %d", len(memPods))
	}
}

// TestCostAnalyzer_WorkflowStructure verifies that the workflow YAML parses correctly and
// that key steps (including the new metricsWindow-based ones) are present and valid.
func TestCostAnalyzer_WorkflowStructure(t *testing.T) {
	wf, err := loadWorkflowFromFile("../../samples/workflows/production/cost-analyzer.yaml")
	if err != nil {
		t.Fatalf("load cost-analyzer.yaml: %v", err)
	}
	for _, stepName := range []string{
		"collectResources",
		"collectNamespaceResources",
		"buildPodList",
		"probePrometheus",
		"collectPrometheusMetrics",
		"joinPrometheusMetrics",
		"analyzeRightSizing",
	} {
		_ = findStep(t, wf, stepName)
	}
	// Verify metricsWindow input is defined with the expected default.
	found := false
	for _, inp := range wf.Spec.Inputs {
		if inp.Name == "metricsWindow" {
			found = true
			if inp.Default != "24h" {
				t.Errorf("metricsWindow default = %q, want 24h", inp.Default)
			}
		}
	}
	if !found {
		t.Error("metricsWindow input not found in workflow spec")
	}
}

// TestCostAnalyzer_MetricsWindow_QueryContainsWindow verifies that the CEL expressions in
// collectPrometheusMetrics embed inputs.metricsWindow into the Prometheus query string.
// With metricsWindow="48h", all three P95/peak queries must contain "[48h:5m]".
func TestCostAnalyzer_MetricsWindow_QueryContainsWindow(t *testing.T) {
	wf, err := loadWorkflowFromFile("../../samples/workflows/production/cost-analyzer.yaml")
	if err != nil {
		t.Fatalf("load cost-analyzer.yaml: %v", err)
	}
	step := findStep(t, wf, "collectPrometheusMetrics")

	rec := &recordingPrometheusClient{}
	eval := newTestCELEvaluator(t, rec)

	vars := buildVars(
		map[string]interface{}{
			"metricsWindow": "48h",
		},
		map[string]interface{}{
			"podIdentifiers":      []interface{}{},
			"prometheusAvailable": true,
		},
	)

	_, err = eval.EvaluateStepExpressions(context.Background(), step, vars)
	if err != nil {
		t.Fatalf("EvaluateStepExpressions(collectPrometheusMetrics): %v", err)
	}

	// The 3 P95/peak queries must embed the configured window: [48h:5m]
	wantSubstr := "[48h:5m]"
	p95Queries := 0
	for _, q := range rec.queries {
		if strings.Contains(q, wantSubstr) {
			p95Queries++
		}
	}
	if p95Queries < 3 {
		t.Errorf("expected at least 3 Prometheus queries containing %q, got %d\nall queries: %v",
			wantSubstr, p95Queries, rec.queries)
	}
}

// podToWorkloadEntry builds a mock podToWorkload entry as it would come from resolveWorkloads.
func podToWorkloadEntry(
	podID, workloadKind, namespace, ownerName string,
	cpuRequestedMilli, cpuRecommendedMilli, cpuSavingsMilli float64,
	memRequestedBytes, memRecommendedBytes, memSavingsBytes, memPeakBytes float64,
) map[string]interface{} {
	return map[string]interface{}{
		"podId":        podID,
		"workloadKind": workloadKind,
		"namespace":    namespace,
		"ownerName":    ownerName,
		"cpu": []interface{}{
			map[string]interface{}{
				"requestedMilli":   cpuRequestedMilli,
				"recommendedMilli": cpuRecommendedMilli,
				"savingsMilli":     cpuSavingsMilli,
			},
		},
		"memory": []interface{}{
			map[string]interface{}{
				"requestedBytes":   memRequestedBytes,
				"recommendedBytes": memRecommendedBytes,
				"savingsBytes":     memSavingsBytes,
				"peakBytes":        memPeakBytes,
			},
		},
	}
}

// evalComputeWorkloadRemediations runs the computeWorkloadRemediations step with the given
// podToWorkload entries, using the workflow's actual configured CEL cost budget
// (celCostLimit: 20000000, see below) so a regression to the per-field-refilter expression
// shape below would fail this test even at moderate scale.
func evalComputeWorkloadRemediations(t *testing.T, podToWorkload []interface{}) map[string]interface{} {
	t.Helper()
	wf, err := loadWorkflowFromFile("../../samples/workflows/production/cost-analyzer.yaml")
	if err != nil {
		t.Fatalf("load cost-analyzer.yaml: %v", err)
	}
	step := findStep(t, wf, "computeWorkloadRemediations")
	eval := newTestCELEvaluator(t, nil)
	// Use the workflow's actual configured budget (celCostLimit: 20000000), not the
	// evaluator's 2M default — the default is already known to be tight for a single
	// O(pods) pass at realistic cluster scale (see the celCostLimit comment at the top
	// of cost-analyzer.yaml), so it isn't the right lever for testing this step's
	// per-key refilter cost specifically.
	eval.SetCELCostLimit(executor.ResolveCELCostLimit(&wf.Spec))

	vars := buildVars(
		map[string]interface{}{
			"cpuCostPerCore":  "0.048",
			"memoryCostPerGB": "0.006",
		},
		map[string]interface{}{
			"podToWorkload": podToWorkload,
		},
	)

	results, err := eval.EvaluateStepExpressions(context.Background(), step, vars)
	if err != nil {
		t.Fatalf("EvaluateStepExpressions(computeWorkloadRemediations): %v", err)
	}
	return results
}

// TestCostAnalyzer_ComputeWorkloadRemediations_Aggregation verifies per-workload aggregation
// is correct after the single-pass rewrite: podCount counts pods per
// workload key, requested/recommended/kind/namespace/name reflect a representative pod (first
// match — matching pre-refactor semantics, since all pods of one workload share the same
// request/recommendation), savings are summed across all pods in the workload, and
// memPeakBytes is the max across pods.
func TestCostAnalyzer_ComputeWorkloadRemediations_Aggregation(t *testing.T) {
	podToWorkload := []interface{}{
		podToWorkloadEntry("default/web-1", "Deployment", "default", "web",
			500.0, 200.0, 300.0, 100e6, 50e6, 50e6, 60e6),
		podToWorkloadEntry("default/web-2", "Deployment", "default", "web",
			500.0, 200.0, 300.0, 100e6, 50e6, 50e6, 70e6),
		podToWorkloadEntry("kube-system/ds-1", "DaemonSet", "kube-system", "node-exporter",
			100.0, 50.0, 50.0, 20e6, 10e6, 10e6, 15e6),
	}

	results := evalComputeWorkloadRemediations(t, podToWorkload)
	remediations := getListField(t, results, "workloadRemediations")
	if len(remediations) != 2 {
		t.Fatalf("expected 2 distinct workloads, got %d", len(remediations))
	}

	var web, ds map[string]interface{}
	for _, r := range remediations {
		m, ok := r.(map[string]interface{})
		if !ok {
			t.Fatalf("remediation entry is %T, want map[string]interface{}", r)
		}
		switch m["name"] {
		case "web":
			web = m
		case "node-exporter":
			ds = m
		}
	}
	if web == nil || ds == nil {
		t.Fatalf("expected both 'web' and 'node-exporter' workloads in results, got %+v", remediations)
	}

	if got := getIntCountField(t, web, "podCount"); got != 2 {
		t.Errorf("web podCount = %d, want 2", got)
	}
	if got := getMapField(t, web, "cpuRequestedMilli"); got != 500.0 {
		t.Errorf("web cpuRequestedMilli = %.2f, want 500.0 (representative pod value, not summed)", got)
	}
	wantCPUSavings := 2 * (300.0 / 1000.0 * 0.048)
	if got := getMapField(t, web, "cpuSavingsUSD"); !approxEqual(got, wantCPUSavings, 0.0001) {
		t.Errorf("web cpuSavingsUSD = %.4f, want %.4f (summed across 2 pods)", got, wantCPUSavings)
	}
	if got := getMapField(t, web, "memPeakBytes"); got != 70e6 {
		t.Errorf("web memPeakBytes = %.0f, want 70e6 (max across pods)", got)
	}
	if got := web["namespace"]; got != "default" {
		t.Errorf("web namespace = %v, want default", got)
	}
	if got := web["kind"]; got != "Deployment" {
		t.Errorf("web kind = %v, want Deployment", got)
	}

	if got := getIntCountField(t, ds, "podCount"); got != 1 {
		t.Errorf("node-exporter podCount = %d, want 1", got)
	}
	if got := ds["kind"]; got != "DaemonSet" {
		t.Errorf("node-exporter kind = %v, want DaemonSet", got)
	}
}

// TestCostAnalyzer_ComputeWorkloadRemediations_CostBudget is a regression test for a
// per-key refilter that re-scanned the full pod list once per output field:
// computeWorkloadRemediations.workloadRemediations previously re-filtered the full
// podToWorkload array 11x per distinct workload key (once per output field) instead of once,
// i.e. O(11 x keys x pods). That was enough to exceed the CEL cost budget on customer clusters
// with many over-provisioned workloads, independent of the earlier
// dead-ReplicaSet fix in resolveWorkloads. This builds a synthetic dataset at a scale that
// reliably exceeds the workflow's configured CEL cost budget (celCostLimit: 20000000, not the
// evaluator's tighter 2M default) under the old 11x-refilter shape, and asserts the single-pass
// rewrite evaluates within that budget.
func TestCostAnalyzer_ComputeWorkloadRemediations_CostBudget(t *testing.T) {
	const numWorkloads = 150
	const podsPerWorkload = 20
	podToWorkload := make([]interface{}, 0, numWorkloads*podsPerWorkload)
	for w := 0; w < numWorkloads; w++ {
		ownerName := fmt.Sprintf("app-%d", w)
		namespace := fmt.Sprintf("ns%d", w)
		for p := 0; p < podsPerWorkload; p++ {
			podID := fmt.Sprintf("%s/%s-%d", namespace, ownerName, p)
			podToWorkload = append(podToWorkload, podToWorkloadEntry(
				podID, "Deployment", namespace, ownerName,
				500.0, 200.0, 300.0, 100e6, 50e6, 50e6, 60e6))
		}
	}

	results := evalComputeWorkloadRemediations(t, podToWorkload)
	remediations := getListField(t, results, "workloadRemediations")
	if len(remediations) != numWorkloads {
		t.Fatalf("expected %d distinct workloads, got %d", numWorkloads, len(remediations))
	}
}

// requestPod builds a pod with one container requesting cpu and memory; a nil status omits it.
func requestPod(name string, status map[string]interface{}, cpu, mem string) map[string]interface{} {
	p := map[string]interface{}{
		"metadata": map[string]interface{}{"name": name, "namespace": "default"},
		"spec": map[string]interface{}{"containers": []interface{}{
			map[string]interface{}{"name": "c", "resources": map[string]interface{}{
				"requests": map[string]interface{}{"cpu": cpu, "memory": mem},
			}},
		}},
	}
	if status != nil {
		p["status"] = status
	}
	return p
}

// TestCostAnalyzer_RequestTotals_PhaseFilterFailOpen pins that the request totals only drop pods
// explicitly in Succeeded/Failed, and that the requests guardrail treats any non-zero total as
// "filtered-most-pods". A tightened phase predicate fails the exact-sum asserts.
func TestCostAnalyzer_RequestTotals_PhaseFilterFailOpen(t *testing.T) {
	wf, err := loadWorkflowFromFile("../../samples/workflows/production/cost-analyzer.yaml")
	if err != nil {
		t.Fatalf("load cost-analyzer.yaml: %v", err)
	}
	eval := newTestCELEvaluator(t, nil)
	phase := func(p string) map[string]interface{} { return map[string]interface{}{"phase": p} }

	pods := []interface{}{
		requestPod("no-status", nil, "500m", "1Mi"),
		requestPod("no-phase", map[string]interface{}{}, "100m", "2Mi"),
		requestPod("unknown", phase("Unknown"), "250m", "4Mi"),
		requestPod("running", phase("Running"), "1", "8Mi"),
		requestPod("pending", phase("Pending"), "2", "16Mi"),
		requestPod("succeeded", phase("Succeeded"), "4", "32Mi"),
		requestPod("failed", phase("Failed"), "8", "64Mi"),
	}
	step := findStep(t, wf, "analyzeClusterUtilization")
	vars := buildVars(map[string]interface{}{}, map[string]interface{}{"pods": pods, "nodes": []interface{}{}})
	results, err := eval.EvaluateStepExpressions(context.Background(), step, vars)
	if err != nil {
		t.Fatalf("EvaluateStepExpressions(analyzeClusterUtilization): %v", err)
	}
	outputs, err := eval.EvaluateStepOutputs(context.Background(), step, vars)
	if err != nil {
		t.Fatalf("EvaluateStepOutputs(analyzeClusterUtilization): %v", err)
	}
	outCPU, _ := outputs["totalCPURequestsCores"].(float64)
	outGB, _ := outputs["totalMemoryRequestsGB"].(float64)
	cpu, _ := results["totalCPURequests"].(float64)
	mem, _ := results["totalMemoryRequests"].(float64)
	if !approxEqual(cpu, 3.85, 1e-9) {
		t.Errorf("totalCPURequests = %v, want 3.85 (all but Succeeded/Failed)", cpu)
	}
	if !approxEqual(mem, 31*1048576, 1) {
		t.Errorf("totalMemoryRequests = %v, want %d (all but Succeeded/Failed)", mem, 31*1048576)
	}

	var guard string
	for _, e := range findStep(t, wf, "computeSavings").Expressions {
		if e.Name == "metricsSourceResolved" {
			guard = e.Expression
		}
	}
	if guard == "" {
		t.Fatal("metricsSourceResolved expression not found in computeSavings")
	}
	for _, tc := range []struct {
		name   string
		cpu, g float64
		want   string
	}{
		{"cpu and memory (step outputs)", outCPU, outGB, "filtered-most-pods"},
		{"memory only", 0, 1, "filtered-most-pods"},
		{"cpu only", 1, 0, "filtered-most-pods"},
		{"no requests", 0, 0, "no-requests"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := eval.EvaluateExpression(context.Background(), guard, buildVars(
				map[string]interface{}{},
				map[string]interface{}{
					"metricsSource": "no-requests", "totalCPURequestsCores": tc.cpu, "totalMemoryRequestsGB": tc.g,
				}))
			if err != nil {
				t.Fatalf("EvaluateExpression(metricsSourceResolved): %v", err)
			}
			if got != tc.want {
				t.Errorf("metricsSourceResolved = %v, want %s", got, tc.want)
			}
		})
	}
}
