/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"context"
	"fmt"
	"io"
	"time"

	celapi "github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	corev1 "k8s.io/api/core/v1"
	k8sresource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/pager"
	"k8s.io/klog/v2"
	metricsclientset "k8s.io/metrics/pkg/client/clientset/versioned"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// unwrapObjectMap extracts the underlying map[string]interface{} representation
// of a Kubernetes object passed in via CEL. cel-go's type adapter can wrap an
// unstructured.Unstructured as either:
//   - map[string]interface{} (when the caller passed the .Object map directly), or
//   - unstructured.Unstructured / *unstructured.Unstructured (when the typed
//     value was passed through; some CEL adapter paths preserve the original type).
//
// The previous implementation only handled the first form, so any pod object
// reaching the CEL layer through the typed path silently returned 0 from the
// caller — observed as cluster-level "savings > baseline" reports. Returns
// (objMap, true) on success, or (nil, false) when the value is not a recognized
// Kubernetes object representation. Logs a warning on miss so silent zeros
// become diagnosable.
func unwrapObjectMap(val ref.Val, fnName string) (map[string]interface{}, bool) {
	switch v := val.Value().(type) {
	case map[string]interface{}:
		return v, true
	case unstructured.Unstructured:
		return v.Object, true
	case *unstructured.Unstructured:
		if v == nil {
			return nil, false
		}
		return v.Object, true
	default:
		klog.Warningf("%s: unsupported pod/node value type %T; returning 0. The CEL value did not match any of map[string]interface{}, unstructured.Unstructured, or *unstructured.Unstructured.", fnName, val.Value())
		return nil, false
	}
}

// metricsResponseIsIncomplete returns true when a metrics-server list response covers
// fewer than 80% of running pods, indicating the scrape window hasn't caught up or the
// server timed out mid-list. The runningPodsCount > 10 guard avoids false positives in
// small namespaces where newly-started pods can lag ~60 s before appearing in metrics.
func metricsResponseIsIncomplete(metricCount, runningPodsCount int) bool {
	return runningPodsCount > 10 && metricCount*10 < runningPodsCount*8
}

const (
	// podLogsMaxTailLines is the maximum number of log lines fetched by resourceLogs/resource.GetLogs.
	podLogsMaxTailLines = 10_000
	// podLogsByteCap is the maximum response size: responses larger than 4 MB are truncated.
	podLogsByteCap = 4 * 1024 * 1024
	// podLogsTimeout caps the blocking window of fetchPodLogs inside prg.Eval().
	podLogsTimeout = 30 * time.Second
)

// fetchPodLogs retrieves pod container logs via the typed Kubernetes clientset.
// Returns ("", nil) for an empty log body (pod not yet logging, e.g. ImagePullBackOff).
// Returns ("", err) when the pod is not found or the log endpoint fails.
// Applies a 4 MB byte cap enforced during the read via io.LimitReader so large responses
// are never fully buffered; responses exceeding podLogsByteCap are truncated with "\n[truncated]".
// tailLines=0 fetches the full log (no TailLines in PodLogOptions); values >10000 are clamped.
//
// This function is called inside prg.Eval() while macroEvalMu is held. That is safe because
// CELEvaluator is constructed per-WorkflowRun and macroEvalMu serialises macro-calling CEL evaluation
// within a single run (forEach goroutines each acquire it independently). The 30s timeout
// caps the worst-case blocking window.
func fetchPodLogs(ctx context.Context, kubeClient kubernetes.Interface, ns, podName, containerName string, tailLines int64) (string, error) {
	logCtx, cancel := context.WithTimeout(ctx, podLogsTimeout)
	defer cancel()

	opts := &corev1.PodLogOptions{
		Container: containerName,
	}
	if tailLines < 0 {
		tailLines = 0
	}
	if tailLines > podLogsMaxTailLines {
		tailLines = podLogsMaxTailLines
	}
	if tailLines > 0 {
		tl := tailLines // local copy — never take address of a literal or loop variable
		opts.TailLines = &tl
	}
	// tailLines==0: TailLines=nil → apiserver returns full log

	stream, err := kubeClient.CoreV1().Pods(ns).GetLogs(podName, opts).Stream(logCtx)
	if err != nil {
		return "", fmt.Errorf("pod %s/%s not found or log unavailable: %w", ns, podName, err)
	}
	defer func() { _ = stream.Close() }()

	// Read through a LimitReader so the cap is enforced during the read, not after full
	// buffering. cap+1 lets us detect truncation: reading exactly cap+1 bytes means there
	// was more data in the stream.
	rawBytes, err := io.ReadAll(io.LimitReader(stream, podLogsByteCap+1))
	if err != nil {
		return "", fmt.Errorf("pod %s/%s log read failed: %w", ns, podName, err)
	}
	if len(rawBytes) == 0 {
		return "", nil // pod has no logs yet
	}
	if len(rawBytes) > podLogsByteCap {
		return string(rawBytes[:podLogsByteCap]) + "\n[truncated]", nil
	}
	return string(rawBytes), nil
}

// listResourceEventsForCEL implements resourceEvents(): it lists events filtered by
// involvedObject and converts them to a CEL list. Extracted out of the CEL closure so its
// own branching is counted against this function, not against the much larger
// GetResourceMacroOptionsWithMetrics that builds all the resource macros.
func listResourceEventsForCEL(macroCtx *macroContextHolder, k8sClient client.Client, apiVersion, kind, ns, name string) ref.Val {
	if k8sClient == nil {
		return types.NewErr("kubernetes client not available (no kubeconfig); this workflow requires a cluster")
	}

	// List events filtered by involvedObject. Events have no retention bound
	// on the cluster side, so a noisy controller can produce thousands per
	// involvedObject. Page through with client-go's ListPager so a single
	// large response can't blow up memory or time out the apiserver.
	events := &corev1.EventList{}
	baseOpts := []client.ListOption{
		client.InNamespace(ns),
		client.MatchingFieldsSelector{
			Selector: fields.AndSelectors(
				fields.OneTermEqualSelector("involvedObject.name", name),
				fields.OneTermEqualSelector("involvedObject.kind", kind),
			),
		},
	}
	p := pager.New(func(ctx context.Context, lopts metav1.ListOptions) (runtime.Object, error) {
		page := &corev1.EventList{}
		pageOpts := make([]client.ListOption, len(baseOpts), len(baseOpts)+2)
		copy(pageOpts, baseOpts)
		if lopts.Limit > 0 {
			pageOpts = append(pageOpts, client.Limit(lopts.Limit))
		}
		if lopts.Continue != "" {
			pageOpts = append(pageOpts, client.Continue(lopts.Continue))
		}
		pageCtx, cancel := context.WithTimeout(ctx, listPageTimeout)
		defer cancel()
		return page, k8sClient.List(pageCtx, page, pageOpts...)
	})
	p.PageSize = listPageSize
	if err := p.EachListItemWithAlloc(macroCtx.get(), metav1.ListOptions{}, func(obj runtime.Object) error {
		evt, ok := obj.(*corev1.Event)
		if !ok {
			return fmt.Errorf("unexpected object type %T from events list", obj)
		}
		events.Items = append(events.Items, *evt)
		return macroCtx.get().Err()
	}); err != nil {
		return types.NewErr("failed to list events for '%s/%s/%s/%s': %v", apiVersion, kind, ns, name, err)
	}

	// Convert to CEL list
	items := make([]ref.Val, 0, len(events.Items))
	for i := range events.Items {
		// Convert Event to map
		eventMap := map[string]interface{}{
			"type":           events.Items[i].Type,
			"reason":         events.Items[i].Reason,
			"message":        events.Items[i].Message,
			"firstTimestamp": events.Items[i].FirstTimestamp.Format(time.RFC3339),
			"lastTimestamp":  events.Items[i].LastTimestamp.Format(time.RFC3339),
			"count":          int64(events.Items[i].Count),
		}
		items = append(items, types.NewDynamicMap(types.DefaultTypeAdapter, eventMap))
	}

	return types.NewDynamicList(types.DefaultTypeAdapter, items)
}

// GetResourceMacroOptions returns CEL environment options for resource helper macros
// These macros provide simplified access to common Kubernetes resource operations
func GetResourceMacroOptions(k8sClient client.Client, namespace string, macroCtx *macroContextHolder) ([]celapi.EnvOption, error) {
	return GetResourceMacroOptionsWithMetrics(k8sClient, nil, nil, nil, nil, namespace, macroCtx)
}

// GetResourceMacroOptionsWithMetrics returns CEL environment options for resource helper macros
// with optional metrics clients for resourceMetrics and prometheusMetrics functions.
// kubeClient is optional; when nil, resource.GetLogs and resourceLogs return a CEL error.
// macroCtx is updated by EvaluateExpression before each evaluation that calls one of them so that macro closures
// use the caller's context rather than context.Background().
func GetResourceMacroOptionsWithMetrics(
	k8sClient client.Client,
	kubeClient kubernetes.Interface,
	metricsClient metricsclientset.Interface,
	customMetricsClient CustomMetricsClient,
	prometheusClient PrometheusClient,
	namespace string,
	macroCtx *macroContextHolder,
) ([]celapi.EnvOption, error) {
	opts := make([]celapi.EnvOption, 0, 4)

	// resourceLogs(apiVersion, kind, namespace, name, container) -> logs string
	// Deprecated: use resource.GetLogs(namespace, podName, containerName, tailLines) instead.
	// apiVersion and kind are accepted for backwards compatibility but are ignored; only Pod logs are supported.
	// Defaults to tailLines=100.
	resourceLogsFunc := celapi.Function("resourceLogs",
		celapi.Overload("resourceLogs_string_string_string_string_string",
			[]*celapi.Type{celapi.StringType, celapi.StringType, celapi.StringType, celapi.StringType, celapi.StringType},
			celapi.StringType,
			celapi.FunctionBinding(func(args ...ref.Val) ref.Val {
				if len(args) != 5 {
					return types.NewErr("resourceLogs requires 5 arguments: apiVersion, kind, namespace, name, container")
				}
				ns, _ := args[2].Value().(string)
				name, _ := args[3].Value().(string)
				container, _ := args[4].Value().(string)

				if ns == "" {
					ns = namespace
				}
				if kubeClient == nil {
					return types.NewErr("kubeClient not available: resourceLogs requires a typed Kubernetes clientset")
				}
				logs, err := fetchPodLogs(macroCtx.get(), kubeClient, ns, name, container, 100)
				if err != nil {
					return types.NewErr("%v", err)
				}
				return types.String(logs)
			}),
		),
	)

	// resource.GetLogs(namespace, podName, containerName, tailLines) -> string
	// Returns the last tailLines lines of a container's log. tailLines=0 returns the full log.
	// Returns "" when the pod has no logs yet (e.g. ImagePullBackOff). Returns a CEL error for
	// NotFound or missing kubeClient. namespace="" falls back to the WorkflowRun namespace.
	resourceGetLogsFunc := celapi.Function("resource.GetLogs",
		celapi.Overload("resource_GetLogs_string_string_string_int",
			[]*celapi.Type{celapi.StringType, celapi.StringType, celapi.StringType, celapi.IntType},
			celapi.StringType,
			celapi.FunctionBinding(func(args ...ref.Val) ref.Val {
				if len(args) != 4 {
					return types.NewErr("resource.GetLogs requires 4 arguments: namespace, podName, containerName, tailLines")
				}
				ns, _ := args[0].Value().(string)
				podName, _ := args[1].Value().(string)
				containerName, _ := args[2].Value().(string)
				tailLinesVal, ok := args[3].Value().(int64)
				if !ok {
					return types.NewErr("resource.GetLogs: tailLines must be an integer")
				}
				if ns == "" {
					ns = namespace
				}
				if kubeClient == nil {
					return types.NewErr("kubeClient not available: resource.GetLogs requires a typed Kubernetes clientset")
				}
				logs, err := fetchPodLogs(macroCtx.get(), kubeClient, ns, podName, containerName, tailLinesVal)
				if err != nil {
					return types.NewErr("%v", err)
				}
				return types.String(logs)
			}),
		),
	)

	// resourceEvents(apiVersion, kind, namespace, name) -> events list
	// Returns events filtered by involvedObject
	resourceEventsFunc := celapi.Function("resourceEvents",
		celapi.Overload("resourceEvents_string_string_string_string",
			[]*celapi.Type{celapi.StringType, celapi.StringType, celapi.StringType, celapi.StringType},
			celapi.ListType(celapi.DynType),
			celapi.FunctionBinding(func(args ...ref.Val) ref.Val {
				if len(args) != 4 {
					return types.NewErr("resourceEvents requires 4 arguments: apiVersion, kind, namespace, name")
				}
				apiVersion, _ := args[0].Value().(string)
				kind, _ := args[1].Value().(string)
				ns, _ := args[2].Value().(string)
				name, _ := args[3].Value().(string)

				// Use default namespace if empty
				if ns == "" {
					ns = namespace
				}

				return listResourceEventsForCEL(macroCtx, k8sClient, apiVersion, kind, ns, name)
			}),
		),
	)

	// resourceMetrics(apiVersion, kind, namespace, name, metricName) -> metrics map
	// Returns CPU, memory, and other resource usage metrics from metrics API
	// If metricName is provided and not empty, queries Custom Metrics API for that specific metric (e.g., GPU metrics)
	// If metricName is empty, returns standard CPU/memory metrics from standard metrics API
	resourceMetricsFunc := celapi.Function("resourceMetrics",
		celapi.Overload("resourceMetrics_string_string_string_string_string",
			[]*celapi.Type{celapi.StringType, celapi.StringType, celapi.StringType, celapi.StringType, celapi.StringType},
			celapi.MapType(celapi.StringType, celapi.DynType),
			celapi.FunctionBinding(func(args ...ref.Val) ref.Val {
				if len(args) != 5 {
					return types.NewErr("resourceMetrics requires 5 arguments: apiVersion, kind, namespace, name, metricName")
				}
				apiVersion, _ := args[0].Value().(string)
				kind, _ := args[1].Value().(string)
				ns, _ := args[2].Value().(string)
				name, _ := args[3].Value().(string)
				metricName, _ := args[4].Value().(string)

				// Use default namespace if empty
				if ns == "" {
					ns = namespace
				}

				// If metricName is provided, use Custom Metrics API
				if metricName != "" {
					if customMetricsClient == nil {
						return types.NewErr("custom metrics client not available - custom metrics adapter may not be installed")
					}

					// Query custom metrics API
					metricValue, err := customMetricsClient.GetMetric(macroCtx.get(), apiVersion, kind, ns, name, metricName)
					if err != nil {
						return types.NewErr("failed to get custom metric '%s' for '%s/%s/%s/%s': %v",
							metricName, apiVersion, kind, ns, name, err)
					}

					// Return custom metric result
					return types.NewDynamicMap(types.DefaultTypeAdapter, map[string]interface{}{
						"metricName": metricValue.MetricName(),
						"value":      metricValue.Value(),
						"timestamp":  metricValue.Timestamp().Format(time.RFC3339),
						"window":     metricValue.WindowSeconds(),
					})
				}

				// Otherwise, use standard metrics API (existing behavior)
				// Currently only supports Pod metrics
				if apiVersion != "v1" || kind != "Pod" {
					return types.NewErr("resourceMetrics standard metrics currently only supports v1/Pod resources")
				}

				// Check if metrics client is available
				if metricsClient == nil {
					return types.NewErr("metrics client not available - metrics server may not be installed")
				}

				// Get pod metrics from metrics API
				podMetrics, err := metricsClient.MetricsV1beta1().PodMetricses(ns).Get(macroCtx.get(), name, metav1.GetOptions{})
				if err != nil {
					return types.NewErr("failed to get pod metrics '%s/%s': %v", ns, name, err)
				}

				// Build metrics map with container-level metrics
				metricsMap := map[string]interface{}{
					"timestamp": podMetrics.Timestamp.Format(time.RFC3339),
					"window":    podMetrics.Window.Duration.String(),
				}

				// Aggregate container metrics
				var totalCPU, totalMemory int64
				containers := make([]map[string]interface{}, len(podMetrics.Containers))
				for i, container := range podMetrics.Containers {
					cpuMilli := container.Usage.Cpu().MilliValue()
					memoryBytes := container.Usage.Memory().Value()
					totalCPU += cpuMilli
					totalMemory += memoryBytes

					containers[i] = map[string]interface{}{
						"name":   container.Name,
						"cpu":    fmt.Sprintf("%dm", cpuMilli),
						"memory": fmt.Sprintf("%d", memoryBytes),
					}
				}

				metricsMap["containers"] = containers
				metricsMap["totalCPU"] = fmt.Sprintf("%dm", totalCPU)
				metricsMap["totalMemory"] = fmt.Sprintf("%d", totalMemory)

				return types.NewDynamicMap(types.DefaultTypeAdapter, metricsMap)
			}),
		),
	)

	// resourceMetricsList(namespace) -> list of metrics maps
	// Returns CPU and memory usage metrics for all pods in the given namespace from metrics API.
	// Each item has: namespace, name, totalCPU (millicores int64), totalMemory (bytes int64),
	// and containers (list of {name, cpu, memory}).
	resourceMetricsListFunc := celapi.Function("resourceMetricsList",
		celapi.Overload("resourceMetricsList_string",
			[]*celapi.Type{celapi.StringType},
			celapi.ListType(celapi.MapType(celapi.StringType, celapi.DynType)),
			celapi.UnaryBinding(func(arg ref.Val) ref.Val {
				ns, _ := arg.Value().(string)
				if ns == "" {
					ns = namespace
				}

				if k8sClient == nil {
					return types.NewErr("kubernetes client not available (no kubeconfig); this workflow requires a cluster")
				}
				if metricsClient == nil {
					return types.NewErr("metrics client not available - metrics server may not be installed")
				}

				// Count running pods from the controller-runtime cache in pages so we can
				// detect incomplete metrics responses on large namespaces without loading
				// all pod specs into memory at once.
				runningPodsCount := 0
				{
					continueToken := ""
					for {
						page := &corev1.PodList{}
						opts := []client.ListOption{client.InNamespace(ns), client.Limit(500)}
						if continueToken != "" {
							opts = append(opts, client.Continue(continueToken))
						}
						if listErr := k8sClient.List(macroCtx.get(), page, opts...); listErr != nil {
							klog.Warningf("resourceMetricsList: failed to list pods in namespace %q for completeness check: %v; skipping partial-metrics detection", ns, listErr)
							break
						}
						for i := range page.Items {
							if page.Items[i].Status.Phase == corev1.PodRunning {
								runningPodsCount++
							}
						}
						continueToken = page.GetContinue()
						if continueToken == "" {
							break
						}
					}
				}

				isIncomplete := func(metricCount int) bool {
					return metricsResponseIsIncomplete(metricCount, runningPodsCount)
				}

				// First attempt: 30-second server-side timeout.
				timeout := int64(30)
				podMetricsList, err := metricsClient.MetricsV1beta1().PodMetricses(ns).List(
					macroCtx.get(), metav1.ListOptions{TimeoutSeconds: &timeout})

				// Retry up to 2 more times with escalating timeouts when the first call
				// fails or returns partial data for a large namespace.
				for attempt := 2; attempt <= 3; attempt++ {
					if err == nil && !isIncomplete(len(podMetricsList.Items)) {
						break
					}
					if err != nil {
						klog.Warningf("resourceMetricsList: attempt %d/3 for namespace %q: %v", attempt-1, ns, err)
					} else {
						klog.Warningf("resourceMetricsList: partial data for namespace %q (%d/%d running pods), retrying with longer timeout",
							ns, len(podMetricsList.Items), runningPodsCount)
					}
					retryTimeout := int64(30) * int64(attempt) // 60 s, then 90 s
					select {
					case <-macroCtx.get().Done():
						return types.NewErr("context cancelled retrying metrics for namespace '%s': %v", ns, macroCtx.get().Err())
					case <-time.After(time.Duration(attempt-1) * 2 * time.Second):
					}
					podMetricsList, err = metricsClient.MetricsV1beta1().PodMetricses(ns).List(
						macroCtx.get(), metav1.ListOptions{TimeoutSeconds: &retryTimeout})
				}
				if err != nil {
					return types.NewErr("failed to list pod metrics in namespace '%s': %v", ns, err)
				}
				if isIncomplete(len(podMetricsList.Items)) {
					klog.Warningf("resourceMetricsList: returning partial metrics for namespace %q (%d/%d running pods) after 3 attempts",
						ns, len(podMetricsList.Items), runningPodsCount)
				}

				items := make([]ref.Val, 0, len(podMetricsList.Items))
				for _, pm := range podMetricsList.Items {
					var totalCPU, totalMemory int64
					containers := make([]map[string]interface{}, len(pm.Containers))
					for i, container := range pm.Containers {
						cpuMilli := container.Usage.Cpu().MilliValue()
						memoryBytes := container.Usage.Memory().Value()
						totalCPU += cpuMilli
						totalMemory += memoryBytes
						containers[i] = map[string]interface{}{
							"name":   container.Name,
							"cpu":    fmt.Sprintf("%dm", cpuMilli),
							"memory": fmt.Sprintf("%d", memoryBytes),
						}
					}
					podMap := map[string]interface{}{
						"namespace":   pm.Namespace,
						"name":        pm.Name,
						"totalCPU":    totalCPU,
						"totalMemory": totalMemory,
						"timestamp":   pm.Timestamp.Format(time.RFC3339),
						"window":      pm.Window.Duration.String(),
						"containers":  containers,
					}
					items = append(items, types.NewDynamicMap(types.DefaultTypeAdapter, podMap))
				}

				return types.NewDynamicList(types.DefaultTypeAdapter, items)
			}),
		),
	)

	// prometheusMetrics(query, timeRange) -> metrics map
	// Queries Prometheus for metrics using PromQL. Returns query results as a structured map
	// (type, samples, value). On query failure returns the same shape with empty data and
	// an "error" field set so workflows can check has(result.error) and fail or continue.
	prometheusMetricsFunc := celapi.Function("prometheusMetrics",
		celapi.Overload("prometheusMetrics_string_string",
			[]*celapi.Type{celapi.StringType, celapi.StringType},
			celapi.MapType(celapi.StringType, celapi.DynType),
			celapi.FunctionBinding(func(args ...ref.Val) ref.Val {
				if len(args) != 2 {
					return types.NewErr("prometheusMetrics requires 2 arguments: query, timeRange")
				}
				query, _ := args[0].Value().(string)
				timeRange, _ := args[1].Value().(string)

				if prometheusClient == nil {
					return types.NewErr("prometheus client not available - prometheus may not be configured")
				}

				// Parse time range (e.g., "5m", "1h", "30s")
				duration, err := time.ParseDuration(timeRange)
				if err != nil {
					return types.NewErr("invalid timeRange '%s': %v", timeRange, err)
				}

				// Query Prometheus; on failure return empty vector with error field so workflows can fail or continue
				result, err := prometheusClient.Query(macroCtx.get(), query, time.Now().Add(-duration))
				if err != nil {
					if _, isNoOp := prometheusClient.(*NoOpPrometheusClient); !isNoOp {
						klog.Warningf("prometheusMetrics query failed: query=%q timeRange=%q: %v", query, timeRange, err)
					}
					return convertPrometheusErrorToCEL(err.Error())
				}

				// Convert Prometheus result to CEL map
				return convertPrometheusResultToCEL(result)
			}),
		),
	)

	// podTotalCPURequest(pod) -> double
	// Sums CPU requests across all containers in a pod, safely handling missing fields.
	// Replaces the verbose has(c.resources) && has(c.resources.requests) && ... pattern.
	podTotalCPURequestFunc := celapi.Function("podTotalCPURequest",
		celapi.Overload("podTotalCPURequest_dyn",
			[]*celapi.Type{celapi.DynType},
			celapi.DoubleType,
			celapi.UnaryBinding(func(arg ref.Val) ref.Val {
				return sumContainerResource(arg, "cpu")
			}),
		),
	)

	// podTotalMemRequest(pod) -> double
	// Sums memory requests across all containers in a pod, safely handling missing fields.
	podTotalMemRequestFunc := celapi.Function("podTotalMemRequest",
		celapi.Overload("podTotalMemRequest_dyn",
			[]*celapi.Type{celapi.DynType},
			celapi.DoubleType,
			celapi.UnaryBinding(func(arg ref.Val) ref.Val {
				return sumContainerResource(arg, "memory")
			}),
		),
	)

	// nodeCapacityCPU(node) -> double
	// Extracts CPU capacity from a node as a float (cores).
	nodeCapacityCPUFunc := celapi.Function("nodeCapacityCPU",
		celapi.Overload("nodeCapacityCPU_dyn",
			[]*celapi.Type{celapi.DynType},
			celapi.DoubleType,
			celapi.UnaryBinding(func(arg ref.Val) ref.Val {
				return extractNodeQuantity(arg, "capacity", "cpu")
			}),
		),
	)

	// nodeCapacityMemory(node) -> double
	// Extracts memory capacity from a node as a float (bytes).
	nodeCapacityMemoryFunc := celapi.Function("nodeCapacityMemory",
		celapi.Overload("nodeCapacityMemory_dyn",
			[]*celapi.Type{celapi.DynType},
			celapi.DoubleType,
			celapi.UnaryBinding(func(arg ref.Val) ref.Val {
				return extractNodeQuantity(arg, "capacity", "memory")
			}),
		),
	)

	// nodeAllocatableCPU(node) -> double
	// Extracts allocatable CPU from a node as a float (cores).
	nodeAllocatableCPUFunc := celapi.Function("nodeAllocatableCPU",
		celapi.Overload("nodeAllocatableCPU_dyn",
			[]*celapi.Type{celapi.DynType},
			celapi.DoubleType,
			celapi.UnaryBinding(func(arg ref.Val) ref.Val {
				return extractNodeQuantity(arg, "allocatable", "cpu")
			}),
		),
	)

	// nodeAllocatableMemory(node) -> double
	// Extracts allocatable memory from a node as a float (bytes).
	nodeAllocatableMemoryFunc := celapi.Function("nodeAllocatableMemory",
		celapi.Overload("nodeAllocatableMemory_dyn",
			[]*celapi.Type{celapi.DynType},
			celapi.DoubleType,
			celapi.UnaryBinding(func(arg ref.Val) ref.Val {
				return extractNodeQuantity(arg, "allocatable", "memory")
			}),
		),
	)

	if macroCtx != nil {
		macroCtx.readers = []string{"resourceLogs", "resource.GetLogs", "resourceEvents", "resourceMetrics", "resourceMetricsList", "prometheusMetrics"}
	}
	opts = append(opts,
		resourceLogsFunc, resourceGetLogsFunc, resourceEventsFunc, resourceMetricsFunc, resourceMetricsListFunc, prometheusMetricsFunc,
		podTotalCPURequestFunc, podTotalMemRequestFunc,
		nodeCapacityCPUFunc, nodeCapacityMemoryFunc,
		nodeAllocatableCPUFunc, nodeAllocatableMemoryFunc,
	)
	return opts, nil
}

// sumContainerResource sums a resource (cpu or memory) request across all containers in a pod.
func sumContainerResource(podVal ref.Val, resourceName string) ref.Val {
	pod, ok := unwrapObjectMap(podVal, "sumContainerResource")
	if !ok {
		return types.Double(0.0)
	}
	spec, ok := pod["spec"].(map[string]interface{})
	if !ok {
		return types.Double(0.0)
	}
	containers, ok := spec["containers"].([]interface{})
	if !ok {
		return types.Double(0.0)
	}

	var total float64
	for _, c := range containers {
		container, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		resources, ok := container["resources"].(map[string]interface{})
		if !ok {
			continue
		}
		requests, ok := resources["requests"].(map[string]interface{})
		if !ok {
			continue
		}
		val, ok := requests[resourceName]
		if !ok {
			continue
		}
		qty, err := k8sresource.ParseQuantity(fmt.Sprintf("%v", val))
		if err != nil {
			continue
		}
		total += qty.AsApproximateFloat64()
	}
	return types.Double(total)
}

// extractNodeQuantity extracts a quantity from node.status.<pool>.<resourceName>.
func extractNodeQuantity(nodeVal ref.Val, pool, resourceName string) ref.Val {
	node, ok := unwrapObjectMap(nodeVal, "extractNodeQuantity")
	if !ok {
		return types.Double(0.0)
	}
	status, ok := node["status"].(map[string]interface{})
	if !ok {
		return types.Double(0.0)
	}
	poolMap, ok := status[pool].(map[string]interface{})
	if !ok {
		return types.Double(0.0)
	}
	val, ok := poolMap[resourceName]
	if !ok {
		return types.Double(0.0)
	}
	qty, err := k8sresource.ParseQuantity(fmt.Sprintf("%v", val))
	if err != nil {
		return types.Double(0.0)
	}
	return types.Double(qty.AsApproximateFloat64())
}

// prometheusResultToMap converts Prometheus result to a map for CEL context (result.type, result.samples, result.value).
func prometheusResultToMap(result PrometheusResult) map[string]interface{} {
	resultType := result.Type()
	resultMap := map[string]interface{}{
		"type": resultType,
	}

	switch resultType {
	case "vector":
		samples := result.GetVector()
		items := make([]map[string]interface{}, len(samples))
		for i, sample := range samples {
			items[i] = map[string]interface{}{
				"metric":    sample.Metric(),
				"value":     sample.Value(),
				"timestamp": sample.Timestamp().Format(time.RFC3339),
			}
		}
		resultMap["samples"] = items
	case "scalar":
		resultMap["value"] = result.GetScalar()
	default:
		resultMap["value"] = nil
	}

	return resultMap
}

// convertPrometheusResultToCEL converts Prometheus result to CEL map
func convertPrometheusResultToCEL(result PrometheusResult) ref.Val {
	return types.NewDynamicMap(types.DefaultTypeAdapter, prometheusResultToMap(result))
}

// convertPrometheusErrorToCEL returns a CEL map with the same shape as a vector result
// (type, samples) but with empty samples and an "error" field set, so workflows can
// distinguish query failure from "no data" and choose to fail or continue.
func convertPrometheusErrorToCEL(errMsg string) ref.Val {
	m := prometheusResultToMap(&vectorResult{})
	m["error"] = errMsg
	return types.NewDynamicMap(types.DefaultTypeAdapter, m)
}
