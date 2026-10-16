/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

const (
	openReportsCRDName    = "reports.openreports.io"
	openReportsInstallURL = "https://openreports.io/docs/install/"
)

var openReportsCRDGVK = schema.GroupVersionKind{
	Group:   "apiextensions.k8s.io",
	Version: "v1",
	Kind:    "CustomResourceDefinition",
}

var openReportsGVK = schema.GroupVersionKind{
	Group:   "openreports.io",
	Version: "v1alpha1",
	Kind:    "Report",
}

// executeOpenReportStep emits workflow results as an OpenReports.io Report CRD.
// If the OpenReports CRD is not installed, it falls back to storing data in context
// and emitting a Warning Kubernetes Event on the WorkflowRun.
func (e *WorkflowExecutor) executeOpenReportStep(
	ctx context.Context,
	workflowRun *ottoflowv1alpha1.WorkflowRun,
	step ottoflowv1alpha1.Step,
) (map[string]interface{}, error) {
	ref := step.OpenReport

	contextData, err := e.contextManager.ReadContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("openReport: failed to read context: %w", err)
	}
	vars := e.celEvaluator.BuildVariableMap(contextData)

	// Evaluate required results expression
	resultsVal, err := e.celEvaluator.EvaluateExpression(ctx, ref.ResultsExpression, vars)
	if err != nil {
		return nil, fmt.Errorf("openReport: failed to evaluate resultsExpression: %w", err)
	}
	results, ok := resultsVal.([]interface{})
	if !ok {
		return nil, fmt.Errorf("openReport: resultsExpression must evaluate to a list, got %T", resultsVal)
	}

	// Evaluate optional scope expression
	var scopeMap map[string]interface{}
	if ref.ScopeExpression != "" {
		scopeVal, err := e.celEvaluator.EvaluateExpression(ctx, ref.ScopeExpression, vars)
		if err != nil {
			return nil, fmt.Errorf("openReport: failed to evaluate scopeExpression: %w", err)
		}
		if sm, ok := scopeVal.(map[string]interface{}); ok {
			scopeMap = sm
		} else {
			return nil, fmt.Errorf("openReport: scopeExpression must evaluate to a map, got %T", scopeVal)
		}
	}

	summaryMap, err := e.computeOpenReportSummary(ctx, ref, vars, results)
	if err != nil {
		return nil, err
	}

	source := ref.Source
	if source == "" {
		source = "ottoflow"
	}
	namespace := ref.Namespace
	if namespace == "" {
		namespace = workflowRun.Namespace
	}

	crdAvailable, unavailableReason, err := e.isOpenReportsCRDAvailable(ctx)
	if err != nil {
		return nil, fmt.Errorf("openReport: failed to check CRD availability: %w", err)
	}

	var mode, reportName, reportNamespace string
	if crdAvailable {
		if err := e.createOpenReportCRD(ctx, ref, namespace, source, scopeMap, summaryMap, results); err != nil {
			return nil, err
		}
		mode, reportName, reportNamespace = "crd", ref.ReportName, namespace
	} else {
		msg := fmt.Sprintf(
			"%s "+
				"Report data is available in reportResult.data and can be captured via workflow outputs. "+
				"To create CRD-backed reports, install OpenReports: %s",
			unavailableReason, openReportsInstallURL,
		)
		klog.Warningf("openReport step %q: %s", step.Name, msg)
		if e.eventRecorder != nil {
			e.eventRecorder.Eventf(workflowRun, nil, corev1.EventTypeWarning, "OpenReportsFallback", step.Name, "%s", msg)
		}
		mode = "data"
	}

	reportResult := map[string]interface{}{
		"mode":      mode,
		"name":      reportName,
		"namespace": reportNamespace,
		"summary":   summaryMap,
		"data":      results,
	}
	return e.writeOpenReportOutputs(ctx, step, reportResult)
}

// isOpenReportsCRDAvailable checks whether the openreports.io/v1alpha1 Report CRD is
// installed in the TARGET cluster (e.client), which is where createOpenReportCRD writes.
// The result is cached for the lifetime of this executor (one WorkflowRun, one target).
// A nil target client (CLI -f with no kubeconfig) or a Forbidden probe cannot determine
// availability and falls back to data mode with a reason; NotFound means not installed.
// Any other error fails the step.
func (e *WorkflowExecutor) isOpenReportsCRDAvailable(ctx context.Context) (bool, string, error) {
	e.openReportsCRDOnce.Do(func() {
		if e.client == nil {
			e.openReportsCRDReason = fmt.Sprintf("Cannot determine whether the OpenReports CRD (%s) is installed: no Kubernetes client is available in this execution mode (no kubeconfig).", openReportsCRDName)
			return
		}
		crd := &unstructured.Unstructured{}
		crd.SetGroupVersionKind(openReportsCRDGVK)
		err := e.client.Get(ctx, types.NamespacedName{Name: openReportsCRDName}, crd)
		switch {
		case err == nil:
			e.openReportsCRDAvail = true
		case apierrors.IsNotFound(err):
			e.openReportsCRDReason = fmt.Sprintf("OpenReports CRD (%s) is not installed in the target cluster.", openReportsCRDName)
		case apierrors.IsForbidden(err):
			e.openReportsCRDReason = fmt.Sprintf("Cannot determine whether the OpenReports CRD (%[1]s) is installed: the target identity is not permitted to get customresourcedefinitions (resourceNames: %[1]s), so the Report write was not attempted. Underlying error: %[2]v.", openReportsCRDName, err)
		default:
			e.openReportsCRDErr = err
		}
	})
	return e.openReportsCRDAvail, e.openReportsCRDReason, e.openReportsCRDErr
}

// computeOpenReportSummary returns pass/fail/warn/error/skip counts either by evaluating
// SummaryExpression or by counting result values in the results list.
func (e *WorkflowExecutor) computeOpenReportSummary(
	ctx context.Context,
	ref *ottoflowv1alpha1.StepOpenReport,
	vars map[string]interface{},
	results []interface{},
) (map[string]interface{}, error) {
	if ref.SummaryExpression != "" {
		summaryVal, err := e.celEvaluator.EvaluateExpression(ctx, ref.SummaryExpression, vars)
		if err != nil {
			return nil, fmt.Errorf("openReport: failed to evaluate summaryExpression: %w", err)
		}
		sm, ok := summaryVal.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("openReport: summaryExpression must evaluate to a map, got %T", summaryVal)
		}
		return sm, nil
	}

	summary := map[string]interface{}{
		"pass": int64(0), "fail": int64(0), "warn": int64(0), "error": int64(0), "skip": int64(0),
	}
	for _, r := range results {
		item, ok := r.(map[string]interface{})
		if !ok {
			continue
		}
		if resultVal, ok := item["result"].(string); ok {
			if _, known := summary[resultVal]; known {
				summary[resultVal] = summary[resultVal].(int64) + 1
			}
		}
	}
	return summary, nil
}

// createOpenReportCRD creates (or updates) the OpenReports.io Report CRD object.
func (e *WorkflowExecutor) createOpenReportCRD(
	ctx context.Context,
	ref *ottoflowv1alpha1.StepOpenReport,
	namespace, source string,
	scopeMap, summaryMap map[string]interface{},
	results []interface{},
) error {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "openreports.io/v1alpha1",
			"kind":       "Report",
			"metadata": map[string]interface{}{
				"name":      ref.ReportName,
				"namespace": namespace,
			},
			"source":  source,
			"summary": summaryMap,
			"results": results,
		},
	}
	if scopeMap != nil {
		obj.Object["scope"] = scopeMap
	}

	if err := e.client.Create(ctx, obj); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("openReport: failed to create Report CRD %s/%s: %w", namespace, ref.ReportName, err)
		}
		// Idempotent update: mutate the fetched object in-place to preserve metadata
		// (labels, annotations, ownerRefs, finalizers) added by controllers or users.
		existing := &unstructured.Unstructured{}
		existing.SetGroupVersionKind(openReportsGVK)
		if getErr := e.client.Get(ctx, types.NamespacedName{Name: ref.ReportName, Namespace: namespace}, existing); getErr != nil {
			return fmt.Errorf("openReport: failed to get existing Report CRD for update: %w", getErr)
		}
		existing.Object["source"] = source
		existing.Object["summary"] = summaryMap
		existing.Object["results"] = results
		if scopeMap != nil {
			existing.Object["scope"] = scopeMap
		} else {
			delete(existing.Object, "scope")
		}
		if updateErr := e.client.Update(ctx, existing); updateErr != nil {
			return fmt.Errorf("openReport: failed to update existing Report CRD %s/%s: %w", namespace, ref.ReportName, updateErr)
		}
	}

	klog.V(3).InfoS("OpenReport step created Report CRD",
		"report", ref.ReportName,
		"namespace", namespace,
	)
	return nil
}

// writeOpenReportOutputs writes reportResult to the workflow context and evaluates any
// user-defined step outputs. Mirrors the output pattern used by other step executors.
func (e *WorkflowExecutor) writeOpenReportOutputs(
	ctx context.Context,
	step ottoflowv1alpha1.Step,
	reportResult map[string]interface{},
) (map[string]interface{}, error) {
	var outputs map[string]interface{}

	if len(step.Outputs) > 0 {
		contextData, err := e.contextManager.ReadContext(ctx)
		if err != nil {
			return nil, fmt.Errorf("openReport: failed to read context for output evaluation: %w", err)
		}
		outputVars := e.celEvaluator.BuildVariableMap(contextData)
		outputVars["reportResult"] = reportResult

		outputs, err = e.celEvaluator.EvaluateStepOutputs(ctx, step, outputVars)
		if err != nil {
			return nil, fmt.Errorf("openReport: failed to evaluate step outputs: %w", err)
		}
		outputs["reportResult"] = reportResult
	} else {
		outputs = map[string]interface{}{
			"reportResult": reportResult,
		}
	}

	if err := e.contextManager.WriteStepOutputs(ctx, step.Name, outputs); err != nil {
		return nil, fmt.Errorf("openReport: failed to write step outputs: %w", err)
	}
	return outputs, nil
}
