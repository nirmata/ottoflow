/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	// Import client auth plugins for kubeconfig exec auth.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	metricsclientset "k8s.io/metrics/pkg/client/clientset/versioned"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/logging"
	"github.com/nirmata/ottoflow/internal/tracing"
	"github.com/nirmata/ottoflow/internal/workflow/cluster"
	workflowexecutor "github.com/nirmata/ottoflow/internal/workflow/executor"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(ottoflowv1alpha1.AddToScheme(scheme))
}

// runnerConfig holds the runner's identity and optional config (set by the controller or flags).
type runnerConfig struct {
	runName       string
	runNamespace  string
	runKey        client.ObjectKey
	jobName       string
	podName       string
	prometheusURL string
}

func main() {
	cfg := parseRunnerFlags()
	ctx := context.Background()

	// Initialize OTel TracerProvider first — it registers the W3C propagator globally,
	// which must be set before we call Extract below.
	// InitTracerProvider sets the global provider itself; we only need the flush handle.
	_, flush, err := tracing.InitTracerProvider(ctx, "workflow-runner")
	if err != nil {
		klog.ErrorS(err, "failed to init tracer provider, continuing without traces")
		otel.SetTracerProvider(noop.NewTracerProvider())
		flush = func(context.Context) error { return nil }
	}
	// Flush before process exit so in-flight spans drain to the collector.
	// defer order (LIFO): rootSpan.End registered after → runs first, then flush.
	defer func() {
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = flush(flushCtx)
	}()

	// Extract the controller's trace context from W3C TraceContext env vars
	// (TRACEPARENT / TRACESTATE injected by buildWorkflowRunnerJob). When unset,
	// Extract returns a context with no remote span context and Start creates a new root.
	carrier := propagation.MapCarrier{
		"traceparent": os.Getenv("TRACEPARENT"),
		"tracestate":  os.Getenv("TRACESTATE"),
	}
	ctx = otel.GetTextMapPropagator().Extract(ctx, carrier)

	// Root span for the entire WorkflowRun execution.
	// Chained to the controller's workflow_run.reconcile span via the extracted trace context.
	ctx, rootSpan := otel.Tracer("ottoflow").Start(ctx, "invoke_workflow",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			attribute.String("gen_ai.system", "ottoflow"),
			attribute.String("workflow.run.name", cfg.runName),
			attribute.String("workflow.run.namespace", cfg.runNamespace),
		))
	defer rootSpan.End()

	controlClient := mustGetControlPlane(ctx)
	workflowRun := mustLoadWorkflowRun(ctx, controlClient, cfg.runKey)
	workflow := mustLoadWorkflow(ctx, controlClient, workflowRun, cfg.runKey)
	// Workflow spec name is available only after CRD load; add it late — spans accept attributes before End().
	rootSpan.SetAttributes(attribute.String("gen_ai.workflow.name", workflow.Name))

	targetClient, metricsClient, kubeClient, prometheusClient := mustGetTargetClients(
		ctx, controlClient, workflowRun, cfg.prometheusURL, cfg.jobName, cfg.podName)
	mustPersistRunningStatus(ctx, controlClient, cfg.runKey, workflowRun, cfg.jobName, cfg.podName)

	exec := mustNewWorkflowExecutor(
		ctx, controlClient, targetClient, metricsClient, kubeClient, prometheusClient, workflowRun)
	exec.SetCheckpointManager(workflowexecutor.NewCheckpointManager(controlClient, workflowRun))
	runWorkflowToCompletion(ctx, exec, workflow, workflowRun, controlClient, cfg)
}

func parseRunnerFlags() runnerConfig {
	var runName, runNamespace, prometheusURL, jobName, podName string
	flag.StringVar(&runName, "workflow-run-name", os.Getenv("WORKFLOW_RUN_NAME"), "WorkflowRun name")
	flag.StringVar(&runNamespace, "workflow-run-namespace", os.Getenv("WORKFLOW_RUN_NAMESPACE"), "WorkflowRun namespace")
	flag.StringVar(&prometheusURL, "prometheus-url", "", "Prometheus server URL for CEL prometheusMetrics() (optional)")
	flag.StringVar(&jobName, "job-name", os.Getenv("JOB_NAME"),
		"Job name (runner pod identity; set by controller when running in-cluster)")
	flag.StringVar(&podName, "pod-name", os.Getenv("POD_NAME"),
		"Pod name (runner pod identity; set by controller when running in-cluster)")
	flag.Parse()
	if runName == "" || runNamespace == "" {
		klog.Fatalf("workflow-run-name and workflow-run-namespace are required")
	}
	if jobName == "" || podName == "" {
		klog.Fatalf("job-name and pod-name are required (runner must run inside the controller-created Job pod)")
	}
	// Nirmata credentials: in-cluster, use a Secret and reference it in WorkflowRun spec.execution.job.env
	// with valueFrom.secretKeyRef; the runner reads NIRMATA_* from the process environment.
	return runnerConfig{
		runName:       runName,
		runNamespace:  runNamespace,
		runKey:        client.ObjectKey{Name: runName, Namespace: runNamespace},
		prometheusURL: prometheusURL,
		jobName:       jobName,
		podName:       podName,
	}
}

func mustGetControlPlane(_ context.Context) client.Client {
	restConfig, err := config.GetConfig()
	if err != nil {
		klog.Fatalf("failed to get in-cluster config: %v", err)
	}
	controlClient, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		klog.Fatalf("failed to create control client: %v", err)
	}
	return controlClient
}

func mustLoadWorkflowRun(
	ctx context.Context, controlClient client.Client, runKey client.ObjectKey,
) *ottoflowv1alpha1.WorkflowRun {
	workflowRun := &ottoflowv1alpha1.WorkflowRun{}
	if err := controlClient.Get(ctx, runKey, workflowRun); err != nil {
		klog.Fatalf("failed to get WorkflowRun %s/%s: %v", runKey.Namespace, runKey.Name, err)
	}
	return workflowRun
}

func mustLoadWorkflow(
	ctx context.Context,
	controlClient client.Client,
	workflowRun *ottoflowv1alpha1.WorkflowRun,
	runKey client.ObjectKey,
) *ottoflowv1alpha1.Workflow {
	workflowNamespace := workflowRun.Spec.WorkflowRef.Namespace
	if workflowNamespace == "" {
		workflowNamespace = workflowRun.Namespace
	}
	workflow := &ottoflowv1alpha1.Workflow{}
	workflowKey := client.ObjectKey{Name: workflowRun.Spec.WorkflowRef.Name, Namespace: workflowNamespace}
	if err := controlClient.Get(ctx, workflowKey, workflow); err != nil {
		updateExecutionFailure(ctx, controlClient, runKey, workflowRun, "", "",
			fmt.Sprintf("Failed to get Workflow %s/%s: %v", workflowNamespace, workflowRun.Spec.WorkflowRef.Name, err))
		klog.Fatalf("failed to get Workflow %s/%s: %v", workflowNamespace, workflowRun.Spec.WorkflowRef.Name, err)
	}
	return workflow
}

func mustGetTargetClients(
	ctx context.Context,
	controlClient client.Client,
	workflowRun *ottoflowv1alpha1.WorkflowRun,
	prometheusURL, jobName, podName string,
) (client.Client, metricsclientset.Interface, kubernetes.Interface, workflowexecutor.PrometheusClient) {
	// localExecutionMode=false: the in-cluster runner reads a kubeConfigSecretRef from the
	// file the controller mounted, never through the Secret API.
	targetRestConfig, err := cluster.RestConfigForClusterRef(ctx, controlClient, workflowRun, false)
	if err != nil {
		updateExecutionFailure(ctx, controlClient, client.ObjectKeyFromObject(workflowRun), workflowRun, jobName, podName,
			fmt.Sprintf("Failed to resolve target cluster: %v", err))
		klog.Fatalf("failed to resolve target cluster: %v", err)
	}
	targetClient, err := cluster.ClientFromRESTConfig(targetRestConfig, scheme)
	if err != nil {
		updateExecutionFailure(ctx, controlClient, client.ObjectKeyFromObject(workflowRun), workflowRun, jobName, podName,
			fmt.Sprintf("Failed to create target client: %v", err))
		klog.Fatalf("failed to create target client: %v", err)
	}
	metricsClient, err := metricsclientset.NewForConfig(targetRestConfig)
	if err != nil {
		keys := logging.KeysForRun(workflowRun.Spec.WorkflowRef.Name, workflowRun.Namespace, workflowRun.Name)
		klog.InfoS("metrics client not available", append(keys, "error", err)...)
		metricsClient = nil
	}
	kubeClient, err := kubernetes.NewForConfig(targetRestConfig)
	if err != nil {
		keys := logging.KeysForRun(workflowRun.Spec.WorkflowRef.Name, workflowRun.Namespace, workflowRun.Name)
		klog.InfoS("typed kube client not available, resource.GetLogs will be disabled", append(keys, "error", err)...)
		kubeClient = nil
	}
	resolvedURL, source := resolvePrometheusURL(ctx, targetClient, prometheusURL)
	prometheusClient := workflowexecutor.PrometheusClient(&workflowexecutor.NoOpPrometheusClient{})
	if resolvedURL != "" {
		if pc, err := workflowexecutor.NewHTTPPrometheusClient(resolvedURL); err == nil {
			prometheusClient = pc
			keys := logging.KeysForRun(workflowRun.Spec.WorkflowRef.Name, workflowRun.Namespace, workflowRun.Name)
			klog.InfoS("prometheus client configured", append(keys, "url", resolvedURL, "source", source)...)
		} else {
			keys := logging.KeysForRun(workflowRun.Spec.WorkflowRef.Name, workflowRun.Namespace, workflowRun.Name)
			klog.InfoS("prometheus client not available", append(keys, "error", err)...)
		}
	}
	return targetClient, metricsClient, kubeClient, prometheusClient
}

// resolvePrometheusURL returns the Prometheus URL the runner should use, and a
// short source label for logging ("flag" / "discovered" / "").
//
// The flag/env value passed by the controller takes precedence so operators
// can pin a specific endpoint (cross-cluster Prometheus, Thanos query, etc.).
// When no URL is provided, the runner attempts in-cluster auto-discovery via
// the target cluster's Service list. The probe is best-effort: if discovery
// fails or the candidate doesn't answer within the deadline, the runner falls
// through to the no-op client and the cost-analyzer's metrics-server path
// continues to work as before.
func resolvePrometheusURL(ctx context.Context, c client.Client, explicit string) (string, string) {
	if explicit != "" {
		return explicit, "flag"
	}
	url := discoverPrometheusURL(ctx, c)
	if url != "" {
		return url, "discovered"
	}
	return "", ""
}

// prometheusServicePort returns the port to use when contacting a Prometheus
// Service. Order of preference: a port literally named "web" (kube-prometheus-stack
// convention), a port named "http-web" or "http", then any port whose number is
// 9090 (the upstream default). Returns 0 when nothing matches.
func prometheusServicePort(svc *corev1.Service) int32 {
	var named, http, byNumber int32
	for _, p := range svc.Spec.Ports {
		switch p.Name {
		case "web":
			named = p.Port
		case "http-web", "http":
			if http == 0 {
				http = p.Port
			}
		}
		if p.Port == 9090 && byNumber == 0 {
			byNumber = p.Port
		}
	}
	switch {
	case named != 0:
		return named
	case http != 0:
		return http
	default:
		return byNumber
	}
}

// prometheusCandidate pairs a candidate URL with the Service object that
// produced it, so we can verify the Service actually backs a Prometheus pod
// before issuing a network probe.
type prometheusCandidate struct {
	url string
	svc corev1.Service
}

// discoverPrometheusURL searches the target cluster for a Prometheus Service.
// It tries selectors in order of specificity (kube-prometheus-stack first,
// then plain "app=prometheus", then a name-based fallback), filters out
// candidates whose backing pods are clearly not Prometheus (no pods, or no
// container image containing "prometheus"), then probes each survivor with
// a 3-second deadline before returning the first that answers.
// Returns the empty string when no Prometheus is reachable.
func discoverPrometheusURL(ctx context.Context, c client.Client) string {
	candidates := collectPrometheusCandidates(ctx, c)
	for _, cand := range candidates {
		if !serviceBacksPrometheusPod(ctx, c, cand.svc) {
			klog.V(2).InfoS("prometheus discovery: skipped — service does not back a Prometheus pod",
				"url", cand.url, "service", cand.svc.Namespace+"/"+cand.svc.Name)
			continue
		}
		if probePrometheusURL(ctx, cand.url) {
			return cand.url
		}
	}
	return ""
}

// serviceBacksPrometheusPod returns true when the Service's pod selector
// matches at least one pod whose container image identifies as Prometheus
// (image name or repo path contains "prometheus", case-insensitive). This
// excludes Services that share Prometheus-like labels or names but route
// to a different backend (e.g., a Service named "prometheus-adapter" that
// proxies the Custom Metrics API), without making us issue a network probe
// against every wrong candidate.
//
// A Service with empty selector (headless or ExternalName) is treated as
// "unknown" and allowed through — the metric probe is the final arbiter.
func serviceBacksPrometheusPod(ctx context.Context, c client.Client, svc corev1.Service) bool {
	if len(svc.Spec.Selector) == 0 {
		return true
	}
	pods := &corev1.PodList{}
	listCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := c.List(listCtx, pods,
		client.InNamespace(svc.Namespace),
		client.MatchingLabels(svc.Spec.Selector),
		client.Limit(5),
	)
	if err != nil {
		klog.V(2).InfoS("prometheus discovery: pod list for service failed; allowing through",
			"service", svc.Namespace+"/"+svc.Name, "error", err)
		return true
	}
	if len(pods.Items) == 0 {
		return false
	}
	for i := range pods.Items {
		for _, container := range pods.Items[i].Spec.Containers {
			if strings.Contains(strings.ToLower(container.Image), "prometheus") {
				return true
			}
		}
	}
	return false
}

func collectPrometheusCandidates(ctx context.Context, c client.Client) []prometheusCandidate {
	seen := map[string]struct{}{}
	var ordered []prometheusCandidate
	addCandidate := func(svc corev1.Service) {
		port := prometheusServicePort(&svc)
		if port == 0 {
			return
		}
		url := fmt.Sprintf("http://%s.%s.svc:%d", svc.Name, svc.Namespace, port)
		if _, dup := seen[url]; dup {
			return
		}
		seen[url] = struct{}{}
		ordered = append(ordered, prometheusCandidate{url: url, svc: svc})
	}
	listSelectors := []labels.Selector{
		labels.SelectorFromSet(labels.Set{"app.kubernetes.io/name": "prometheus"}),
		labels.SelectorFromSet(labels.Set{"app": "prometheus"}),
	}
	for _, sel := range listSelectors {
		svcList := &corev1.ServiceList{}
		listCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := c.List(listCtx, svcList, &client.ListOptions{LabelSelector: sel})
		cancel()
		if err != nil {
			klog.V(2).InfoS("prometheus discovery: service list failed", "selector", sel.String(), "error", err)
			continue
		}
		for i := range svcList.Items {
			addCandidate(svcList.Items[i])
		}
	}
	// Name-based fallback for charts that don't set the standard label.
	allList := &corev1.ServiceList{}
	listCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := c.List(listCtx, allList); err == nil {
		for i := range allList.Items {
			name := allList.Items[i].Name
			// Match common Prometheus Service names but exclude unrelated subcomponents
			// (alertmanager, exporters, kube-state-metrics, operator, pushgateway, etc.).
			if !containsAny(name, "prometheus") {
				continue
			}
			excludes := []string{"alertmanager", "exporter", "kube-state", "operator", "pushgateway", "grafana", "node-exporter"}
			if containsAny(name, excludes...) {
				continue
			}
			addCandidate(allList.Items[i])
		}
	}
	return ordered
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// probePrometheusURL issues a query that is specific to the cost-analyzer
// use case rather than a generic "up" query. Many clusters run multiple
// Prometheus instances (one for cluster metrics, one for application metrics,
// one for federation, etc.), and "up" succeeds against any of them — but
// cost-analyzer specifically needs cAdvisor's container_cpu_usage_seconds_total.
// The probe is success only if the candidate Prometheus has *at least one
// active series* for that metric, which means it's actually scraping kubelet
// cAdvisor and is suitable as a metrics source.
//
// A return of false here means "this candidate works as a Prometheus, but
// doesn't have the data we need" — discovery moves on to the next candidate.
func probePrometheusURL(ctx context.Context, url string) bool {
	pc, err := workflowexecutor.NewHTTPPrometheusClient(url)
	if err != nil {
		klog.V(2).InfoS("prometheus discovery: client init failed", "url", url, "error", err)
		return false
	}
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	result, err := pc.Query(probeCtx, "count(container_cpu_usage_seconds_total)", time.Now())
	if err != nil {
		klog.V(2).InfoS("prometheus discovery: probe failed", "url", url, "error", err)
		return false
	}
	// count() returns an empty vector when the metric has no active series
	// (i.e., cAdvisor is not being scraped or the data is stale beyond
	// Prometheus's lookback window). A scalar response shouldn't happen for
	// count() but is treated as "not what we need" for safety.
	if result == nil {
		klog.V(2).InfoS("prometheus discovery: probe returned nil result", "url", url)
		return false
	}
	if result.Type() != "vector" {
		klog.V(2).InfoS("prometheus discovery: probe returned unexpected result type", "url", url, "type", result.Type())
		return false
	}
	samples := result.GetVector()
	if len(samples) == 0 {
		klog.V(2).InfoS("prometheus discovery: probe returned no samples (cAdvisor not scraped here)", "url", url)
		return false
	}
	// At least one sample present; require the count to be > 0.
	count := samples[0].Value()
	if count <= 0 {
		klog.V(2).InfoS("prometheus discovery: cAdvisor metric is stale", "url", url, "count", count)
		return false
	}
	klog.InfoS("prometheus discovery: probe succeeded", "url", url, "cadvisorSeriesCount", samples[0].Value())
	return true
}

func mustPersistRunningStatus(
	ctx context.Context,
	controlClient client.Client,
	runKey client.ObjectKey,
	workflowRun *ottoflowv1alpha1.WorkflowRun,
	jobName, podName string,
) {
	now := metav1.Now()
	workflowRun.Status.Execution = &ottoflowv1alpha1.WorkflowRunExecutionStatus{
		Phase:     string(ottoflowv1alpha1.WorkflowRunPhaseRunning),
		JobName:   jobName,
		PodName:   podName,
		Message:   "Runner pod started",
		StartTime: &now,
	}
	if err := persistWorkflowRunStatus(ctx, controlClient, runKey, workflowRun.Status); err != nil {
		keys := logging.KeysForRun(workflowRun.Spec.WorkflowRef.Name, workflowRun.Namespace, workflowRun.Name)
		klog.ErrorS(err, "failed to persist initial WorkflowRun status", keys...)
		klog.FlushAndExit(klog.ExitFlushTimeout, 1)
	}
}

func mustNewWorkflowExecutor(
	ctx context.Context,
	controlClient, targetClient client.Client,
	metricsClient metricsclientset.Interface,
	kubeClient kubernetes.Interface,
	prometheusClient workflowexecutor.PrometheusClient,
	workflowRun *ottoflowv1alpha1.WorkflowRun,
) *workflowexecutor.WorkflowExecutor {
	exec, err := workflowexecutor.NewWorkflowExecutorWithClientsAndAgentExecutor(
		controlClient,
		targetClient,
		metricsClient,
		&workflowexecutor.NoOpCustomMetricsClient{},
		prometheusClient,
		workflowRun,
		nil,
		nil,
		false, // localExecutionMode: cluster runner always uses exec HTTP
		0,
		5,
		nil,
		kubeClient,
	)
	if err != nil {
		updateExecutionFailure(ctx, controlClient, client.ObjectKeyFromObject(workflowRun), workflowRun, "", "",
			fmt.Sprintf("Failed to create workflow executor: %v", err))
		keys := logging.KeysForRun(workflowRun.Spec.WorkflowRef.Name, workflowRun.Namespace, workflowRun.Name)
		klog.ErrorS(err, "failed to create workflow executor", keys...)
		klog.FlushAndExit(klog.ExitFlushTimeout, 1)
	}
	return exec
}

func runWorkflowToCompletion(
	ctx context.Context,
	exec *workflowexecutor.WorkflowExecutor,
	workflow *ottoflowv1alpha1.Workflow,
	workflowRun *ottoflowv1alpha1.WorkflowRun,
	controlClient client.Client,
	cfg runnerConfig,
) {
	defer exec.Close() //nolint:errcheck
	if err := exec.ExecuteWorkflow(ctx, workflow, workflowRun); err != nil {
		// waitForCallback pause: exit with code 0 so the Job is not marked failed.
		// The controller will recreate the Job once the callback arrives.
		if errors.Is(err, workflowexecutor.ErrAwaitingCallback) {
			keys := append(logging.KeysForRun(workflow.Name, cfg.runNamespace, cfg.runName),
				logging.KeyPhase, string(workflowRun.Status.Phase))
			klog.InfoS("WorkflowRun paused awaiting callback; runner exiting cleanly", keys...)
			klog.FlushAndExit(klog.ExitFlushTimeout, 0)
		}
		persistExecutionFailure(ctx, controlClient, cfg.runKey, workflowRun, cfg.jobName, cfg.podName, err.Error())
		keys := append(logging.KeysForRun(workflow.Name, cfg.runNamespace, cfg.runName),
			logging.KeyPhase, string(workflowRun.Status.Phase))
		klog.ErrorS(err, "workflow execution failed", keys...)
		klog.FlushAndExit(klog.ExitFlushTimeout, 1)
	}
	persistExecutionSuccess(ctx, controlClient, cfg.runKey, workflowRun, cfg.jobName, cfg.podName)
	keys := append(logging.KeysForRun(workflow.Name, cfg.runNamespace, cfg.runName),
		logging.KeyPhase, string(workflowRun.Status.Phase))
	klog.InfoS("WorkflowRun completed", keys...)
}

func persistExecutionFailure(
	ctx context.Context,
	controlClient client.Client,
	runKey client.ObjectKey,
	workflowRun *ottoflowv1alpha1.WorkflowRun,
	jobName, podName, message string,
) {
	end := metav1.Now()
	if workflowRun.Status.Execution == nil {
		workflowRun.Status.Execution = &ottoflowv1alpha1.WorkflowRunExecutionStatus{}
	}
	workflowRun.Status.Execution.Phase = string(ottoflowv1alpha1.WorkflowRunPhaseFailed)
	workflowRun.Status.Execution.JobName = jobName
	workflowRun.Status.Execution.PodName = podName
	workflowRun.Status.Execution.Message = message
	workflowRun.Status.Execution.CompletionTime = &end
	if err := persistWorkflowRunStatus(ctx, controlClient, runKey, workflowRun.Status); err != nil {
		keys := append(logging.KeysForRun(workflowRun.Spec.WorkflowRef.Name, workflowRun.Namespace, workflowRun.Name),
			logging.KeyPhase, "failed")
		klog.ErrorS(err, "workflow failed and failed to persist status", keys...)
	}
}

func persistExecutionSuccess(
	ctx context.Context,
	controlClient client.Client,
	runKey client.ObjectKey,
	workflowRun *ottoflowv1alpha1.WorkflowRun,
	jobName, podName string,
) {
	end := metav1.Now()
	if workflowRun.Status.Execution == nil {
		workflowRun.Status.Execution = &ottoflowv1alpha1.WorkflowRunExecutionStatus{}
	}
	workflowRun.Status.Execution.Phase = string(workflowRun.Status.Phase)
	workflowRun.Status.Execution.JobName = jobName
	workflowRun.Status.Execution.PodName = podName
	workflowRun.Status.Execution.Message = workflowRun.Status.Message
	workflowRun.Status.Execution.CompletionTime = &end
	if err := persistWorkflowRunStatus(ctx, controlClient, runKey, workflowRun.Status); err != nil {
		keys := append(logging.KeysForRun(workflowRun.Spec.WorkflowRef.Name, workflowRun.Namespace, workflowRun.Name),
			logging.KeyPhase, string(workflowRun.Status.Phase))
		klog.ErrorS(err, "workflow succeeded but failed to persist final status", keys...)
		klog.FlushAndExit(klog.ExitFlushTimeout, 1)
	}
}

func updateExecutionFailure(
	ctx context.Context,
	controlClient client.Client,
	runKey client.ObjectKey,
	workflowRun *ottoflowv1alpha1.WorkflowRun,
	jobName, podName, message string,
) {
	now := metav1.Now()
	if jobName == "" {
		jobName = os.Getenv("JOB_NAME")
	}
	if podName == "" {
		podName = os.Getenv("POD_NAME")
	}
	workflowRun.Status.Phase = ottoflowv1alpha1.WorkflowRunPhaseFailed
	workflowRun.Status.Message = message
	workflowRun.Status.CompletionTime = &now
	workflowRun.Status.Execution = &ottoflowv1alpha1.WorkflowRunExecutionStatus{
		Phase:          string(ottoflowv1alpha1.WorkflowRunPhaseFailed),
		JobName:        jobName,
		PodName:        podName,
		Message:        message,
		CompletionTime: &now,
	}
	if err := persistWorkflowRunStatus(ctx, controlClient, runKey, workflowRun.Status); err != nil {
		keys := append(
			logging.KeysForRun(workflowRun.Spec.WorkflowRef.Name, workflowRun.Namespace, workflowRun.Name),
			logging.KeyPhase, "failed")
		klog.ErrorS(err, "failed to persist WorkflowRun failure status", keys...)
	}
}

func persistWorkflowRunStatus(
	ctx context.Context,
	controlClient client.Client,
	runKey client.ObjectKey,
	status ottoflowv1alpha1.WorkflowRunStatus,
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &ottoflowv1alpha1.WorkflowRun{}
		if err := controlClient.Get(ctx, runKey, latest); err != nil {
			return err
		}
		latest.Status = status
		return controlClient.Status().Update(ctx, latest)
	})
}
