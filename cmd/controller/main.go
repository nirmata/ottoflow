/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package main

import (
	"crypto/tls"
	"errors"
	"flag"
	"os"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2/textlogger"
	metricsclientset "k8s.io/metrics/pkg/client/clientset/versioned"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	ctrlwebhook "sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/nirmata/ottoflow/internal/auth"
	"github.com/nirmata/ottoflow/internal/certmanager"
	_ "github.com/nirmata/ottoflow/internal/metrics" // Register Prometheus metrics
	"github.com/nirmata/ottoflow/internal/tracing"
	workflowexecutor "github.com/nirmata/ottoflow/internal/workflow/executor"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	ottowebhook "github.com/nirmata/ottoflow/internal/webhook"
	workflowcontroller "github.com/nirmata/ottoflow/internal/workflow/controller"
	//+kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(ottoflowv1alpha1.AddToScheme(scheme))
	//+kubebuilder:scaffold:scheme
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var namespace string
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var celCacheSize int
	var prometheusURL string
	var agentExecutorServiceName string
	var agentExecutorNamespace string
	var workflowRunnerImage string
	var workflowRunnerServiceAccount string
	var workflowRunnerClusterRole string
	var agentExecutorCallerClusterRole string
	var workflowRunnerAgentExecutorCASecret string
	var secretSourceNamespace string
	var workflowRunnerImagePullSecrets string
	var workflowRunnerImagePullPolicy string
	var workflowRunnerPodLabelsPartOf string
	var workflowRunnerTTLSecondsAfterFinished int
	var workflowRunnerLLMCredentialsSecret string
	var webhookTriggerAddr string
	var mcpAddr string
	var mcpCallerNamespace string
	var serveA2AImage string
	var serveA2AServiceAccount string
	var serveA2AClusterRole string

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", true,
		"Enable leader election for controller manager. "+
			"When true (default), one active leader is elected; set false only for local dev without HA.")
	flag.StringVar(&namespace, "namespace", "ottoflow",
		"Namespace for leader election (where the lease is created). Defaults to ottoflow.")
	flag.BoolVar(&secureMetrics, "metrics-secure", false,
		"If set the metrics endpoint is served securely")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	flag.IntVar(&celCacheSize, "cel-cache-size", 1000,
		"Maximum number of compiled CEL expressions to cache (default: 1000)")
	flag.StringVar(&prometheusURL, "prometheus-url", "",
		"Prometheus server URL for CEL prometheusMetrics() (optional).")
	flag.StringVar(&agentExecutorServiceName, "agent-executor-service-name", os.Getenv("AGENT_EXECUTOR_SERVICE_NAME"),
		"Agent executor Service name for internal TLS cert controller (optional).")
	flag.StringVar(&agentExecutorNamespace, "agent-executor-namespace",
		envOrDefault("AGENT_EXECUTOR_NAMESPACE", "ottoflow"),
		"Namespace for agent-executor TLS cert controller (default: ottoflow).")
	flag.StringVar(&workflowRunnerImage, "workflow-runner-image", os.Getenv("WORKFLOW_RUNNER_IMAGE"),
		"Image for the workflow runner Job (default: ghcr.io/nirmata/ottoflow/workflow-runner:latest).")
	flag.StringVar(&workflowRunnerServiceAccount, "workflow-runner-service-account",
		os.Getenv("WORKFLOW_RUNNER_SERVICE_ACCOUNT"),
		"Service account name for the workflow runner Job. If empty, derived from Workflow name as {workflow-name}-runner.")
	flag.StringVar(&workflowRunnerClusterRole, "workflow-runner-cluster-role", os.Getenv("WORKFLOW_RUNNER_CLUSTER_ROLE"),
		"ClusterRole name for runner Job RBAC (required; the Helm chart sets this to <release>-runner-role).")
	flag.StringVar(&agentExecutorCallerClusterRole, "agent-executor-caller-cluster-role",
		os.Getenv("AGENT_EXECUTOR_CALLER_CLUSTER_ROLE"),
		"ClusterRole name for agent-executor caller RBAC; empty disables (optional).")
	flag.StringVar(&workflowRunnerAgentExecutorCASecret, "workflow-runner-agent-executor-ca-secret", "",
		"Secret name in run namespace for agent-executor CA (internal TLS); empty disables CA mount (optional).")
	flag.StringVar(&secretSourceNamespace, "secret-source-namespace", "",
		"Namespace to copy runner Secret-backed volumes from when missing (optional; default: workflow namespace).")
	flag.StringVar(&workflowRunnerImagePullSecrets, "workflow-runner-image-pull-secrets",
		os.Getenv("WORKFLOW_RUNNER_IMAGE_PULL_SECRETS"),
		"Comma-separated Secret names for runner pod imagePullSecrets (optional).")
	flag.StringVar(&workflowRunnerImagePullPolicy, "workflow-runner-image-pull-policy",
		envOrDefault("WORKFLOW_RUNNER_IMAGE_PULL_POLICY", "IfNotPresent"),
		"ImagePullPolicy for the runner Job container (Always|Never|IfNotPresent; default: IfNotPresent).")
	flag.StringVar(&workflowRunnerPodLabelsPartOf, "workflow-runner-pod-labels-part-of",
		os.Getenv("WORKFLOW_RUNNER_POD_LABELS_PART_OF"),
		"Value for runner pod label app.kubernetes.io/part-of (default: ottoflow).")
	flag.IntVar(&workflowRunnerTTLSecondsAfterFinished,
		"workflow-runner-ttl-seconds-after-finished", 0,
		"Seconds after runner Job completion before deletion (0 = 3600).")
	llmCredSecret := os.Getenv("WORKFLOW_RUNNER_LLM_CREDENTIALS_SECRET")
	flag.StringVar(&workflowRunnerLLMCredentialsSecret, "workflow-runner-llm-credentials-secret",
		llmCredSecret,
		"Secret name in the WorkflowRun namespace for LLM credential injection. "+
			"Empty (default) disables injection. Override per run via spec.execution.llmCredentialsSecret.")

	flag.StringVar(&webhookTriggerAddr, "webhook-trigger-addr", "",
		"Address for the webhook trigger HTTP server (empty disables). "+
			"Receives signed POST requests and creates WorkflowRuns when enabled. "+
			"TLS should be terminated at ingress; see docs/user/tasks/triggers.md.")
	flag.StringVar(&serveA2AImage, "serve-a2a-image", os.Getenv("SERVE_A2A_IMAGE"),
		"Image for the serve-a2a BYO kagent Agent that exposes Workflows with spec.expose.kagent set. "+
			"Empty disables a2a exposure (Agents are not created).")
	flag.StringVar(&serveA2AServiceAccount, "serve-a2a-service-account",
		envOrDefault("SERVE_A2A_SERVICE_ACCOUNT", "serve-a2a"),
		"ServiceAccount name the serve-a2a BYO pod runs as (created per namespace; default: serve-a2a).")
	flag.StringVar(&serveA2AClusterRole, "serve-a2a-cluster-role",
		envOrDefault("SERVE_A2A_CLUSTER_ROLE", "ottoflow-serve-a2a"),
		"Shared ClusterRole the serve-a2a BYO pod's ServiceAccount is bound to (Helm-provisioned; "+
			"the controller only creates a RoleBinding to it, never the ClusterRole itself). "+
			"Default: ottoflow-serve-a2a.")

	webhookServiceName := envOrDefault("WEBHOOK_SERVICE_NAME", "ottoflow-webhook")
	webhookConfigName := envOrDefault("WEBHOOK_CONFIG_NAME", "ottoflow-validating")

	logConfig := textlogger.NewConfig()
	flag.StringVar(&mcpAddr, "mcp-addr", "",
		"The address the MCP server binds to, serving Workflows as MCP tools. Empty disables it.")
	flag.StringVar(&mcpCallerNamespace, "mcp-caller-namespace", "",
		"Namespace holding the mcp-caller ConfigMap that SubjectAccessReview checks for MCP callers. "+
			"Defaults to --namespace.")
	logConfig.AddFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(textlogger.NewLogger(logConfig))

	if workflowRunnerClusterRole == "" {
		setupLog.Error(errors.New("workflow-runner-cluster-role not set"),
			"--workflow-runner-cluster-role (or WORKFLOW_RUNNER_CLUSTER_ROLE) is required: it must name a "+
				"dedicated least-privilege ClusterRole for runner Jobs. Refusing to fall back to the controller's own role.")
		os.Exit(1)
	}

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	tlsOpts := []func(*tls.Config){}
	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Bootstrap webhook TLS from internal cert manager (always enabled).
	config := ctrl.GetConfigOrDie()
	ctx := ctrl.SetupSignalHandler()
	webhookCertDir, caBundle, err := certmanager.BootstrapWebhookCerts(
		ctx, setupLog.WithName("webhook-tls"), config, namespace, webhookServiceName)
	if err != nil {
		setupLog.Error(err, "failed to bootstrap webhook TLS certs")
		os.Exit(1)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		setupLog.Error(err, "failed to create kubernetes clientset")
		os.Exit(1)
	}
	if err := certmanager.PatchValidatingWebhookConfigCA(ctx, clientset, webhookConfigName, caBundle); err != nil {
		setupLog.Info("could not patch ValidatingWebhookConfiguration (VWC may not exist yet)",
			"name", webhookConfigName, "error", err)
	}
	webhookServer := ctrlwebhook.NewServer(ctrlwebhook.Options{
		CertDir: webhookCertDir,
		TLSOpts: tlsOpts,
	})

	// Leader election namespace is required when not running in-cluster.
	// make run creates the namespace and passes --namespace.
	mgrOpts := ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress:   metricsAddr,
			SecureServing: secureMetrics,
			TLSOpts:       tlsOpts,
		},
		WebhookServer:           webhookServer,
		HealthProbeBindAddress:  probeAddr,
		LeaderElection:          enableLeaderElection,
		LeaderElectionID:        "5a5e221a.nirmata.io",
		LeaderElectionNamespace: namespace,
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	}
	mgr, err := ctrl.NewManager(config, mgrOpts)
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Create the cron scheduler and add it to the manager so it participates
	// in leader election (only fires on the leader pod).
	scheduler := workflowcontroller.NewScheduler(mgr.GetClient(), ctrl.Log)
	if err := mgr.Add(scheduler); err != nil {
		setupLog.Error(err, "unable to add scheduler to manager")
		os.Exit(1)
	}

	// Shared event recorder — used by both the WorkflowRun reconciler and the callback server
	// so K8s events (CallbackReceived, CallbackTimeout, etc.) are emitted from the same source.
	eventRecorder := mgr.GetEventRecorder("workflowrun-controller")

	// Create and register the callback server (handles POST /api/v1/workflow-runs/.../callback/...)
	// The callback server is leader-elected — only the active leader opens the port.
	callbackServer := workflowcontroller.NewCallbackServer(mgr.GetClient(), eventRecorder, ":8084")
	if err := mgr.Add(callbackServer); err != nil {
		setupLog.Error(err, "unable to add callback server to manager")
		os.Exit(1)
	}

	// Create shared TriggerManager with REST config for dynamic client
	triggerManager, err := workflowcontroller.NewTriggerManagerWithConfig(
		mgr.GetClient(), mgr.GetScheme(), mgr.GetConfig(), scheduler)
	if err != nil {
		setupLog.Error(err, "unable to create trigger manager")
		os.Exit(1)
	}

	// Create webhook trigger server and add to manager (leader-elected; only fires on leader).
	var webhookTriggerServer *workflowcontroller.WebhookServer
	if webhookTriggerAddr != "" {
		webhookTriggerServer = workflowcontroller.NewWebhookServer(
			webhookTriggerAddr,
			ctrl.Log.WithName("webhook-trigger-server"),
			mgr.GetClient(),
			triggerManager,
		)
		if err := mgr.Add(webhookTriggerServer); err != nil {
			setupLog.Error(err, "unable to add webhook trigger server to manager")
			os.Exit(1)
		}
	}

	// Serve Workflows as MCP tools. Callers authenticate with a ServiceAccount
	// token and are authorized by RBAC, the same model the agent executor uses.
	if mcpAddr != "" {
		callerNamespace := mcpCallerNamespace
		if callerNamespace == "" {
			callerNamespace = namespace
		}
		mcpToolServer, err := workflowcontroller.NewMCPToolServer(
			mgr.GetClient(),
			auth.NewTokenReviewAndSARAuthenticator(clientset, callerNamespace, auth.MCPCallerResourceName),
			mcpAddr,
		)
		if err != nil {
			setupLog.Error(err, "unable to create MCP server")
			os.Exit(1)
		}
		if err := mgr.Add(mcpToolServer); err != nil {
			setupLog.Error(err, "unable to add MCP server to manager")
			os.Exit(1)
		}
	}

	runMetrics := workflowcontroller.NewRunMetrics(mgr.GetCache(), mgr.GetClient())
	if err := mgr.Add(runMetrics); err != nil {
		setupLog.Error(err, "unable to add run metrics recorder to manager")
		os.Exit(1)
	}

	// Create metrics client (optional - gracefully handles if metrics server not available)
	var metricsClient metricsclientset.Interface
	metricsClient, err = metricsclientset.NewForConfig(mgr.GetConfig())
	if err != nil {
		setupLog.Info("metrics client not available - resourceMetrics() standard metrics will return errors", "error", err)
		metricsClient = nil // Will gracefully handle in resourceMetrics function
	}

	// Create custom metrics client (optional - gracefully handles if custom metrics adapter not available)
	// For now, use no-op implementation - can be extended to use actual Custom Metrics API client
	var customMetricsClient workflowexecutor.CustomMetricsClient = &workflowexecutor.NoOpCustomMetricsClient{}
	// TODO: Initialize Custom Metrics API client when available
	// Example:
	// customMetricsClient, err = executor.NewCustomMetricsClient(mgr.GetConfig())
	// if err != nil {
	//     setupLog.Info("metrics client not available - resourceMetrics() custom metrics
	//     will return errors", "error", err)
	//     customMetricsClient = &executor.NoOpCustomMetricsClient{}
	// }

	// Create Prometheus client (optional - gracefully handles if Prometheus not configured)
	var prometheusClient workflowexecutor.PrometheusClient = &workflowexecutor.NoOpPrometheusClient{}
	if prometheusURL != "" {
		pc, err := workflowexecutor.NewHTTPPrometheusClient(prometheusURL)
		if err != nil {
			setupLog.Info("prometheus client not available - prometheusMetrics() will return errors", "error", err)
		} else {
			prometheusClient = pc
			setupLog.Info("Prometheus client configured", "url", prometheusURL)
		}
	}

	runnerConfig := workflowcontroller.RunnerConfig{
		RunnerImage:             workflowRunnerImage,
		RunnerServiceAccount:    workflowRunnerServiceAccount,
		RunnerClusterRole:       workflowRunnerClusterRole,
		AgentExecutorCallerRole: agentExecutorCallerClusterRole,
		AgentExecutorCASecret:   workflowRunnerAgentExecutorCASecret,
		AgentExecutorNamespace:  agentExecutorNamespace,
		SecretSourceNamespace:   secretSourceNamespace,
		PrometheusURL:           prometheusURL,
		ImagePullSecrets:        workflowRunnerImagePullSecrets,
		ImagePullPolicy:         workflowRunnerImagePullPolicy,
		PodLabelsPartOf:         workflowRunnerPodLabelsPartOf,
		TTLSecondsAfterFinished: int32(workflowRunnerTTLSecondsAfterFinished),
		LLMCredentialsSecret:    workflowRunnerLLMCredentialsSecret,
	}

	// Create shared CEL compilation cache; expressions are compiled when
	// Workflows are loaded and reused across WorkflowRun reconciliations.
	celCache, err := workflowexecutor.NewCELCompilationCache(
		mgr.GetClient(), metricsClient, customMetricsClient, prometheusClient,
		ctrl.Log.WithName("cel-cache"))
	if err != nil {
		setupLog.Error(err, "unable to create CEL compilation cache")
		os.Exit(1)
	}

	if err = (&workflowcontroller.WorkflowReconciler{
		Client:         mgr.GetClient(),
		Scheme:         mgr.GetScheme(),
		TriggerManager: triggerManager,
		CELCache:       celCache,
		WebhookServer:  webhookTriggerServer,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Workflow")
		os.Exit(1)
	}
	if err = (&workflowcontroller.WorkflowRunReconciler{
		Client:              mgr.GetClient(),
		Scheme:              mgr.GetScheme(),
		MetricsClient:       metricsClient,
		CustomMetricsClient: customMetricsClient,
		PrometheusClient:    prometheusClient,
		CELCacheSize:        celCacheSize,
		CELCache:            celCache,
		EventRecorder:       eventRecorder,
		RunnerConfig:        runnerConfig,
		ControllerNamespace: namespace,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "WorkflowRun")
		os.Exit(1)
	}
	if serveA2AImage == "" {
		setupLog.Info("--serve-a2a-image (or SERVE_A2A_IMAGE) is empty; a2a exposure is disabled " +
			"(Workflows with spec.expose.kagent will not get a kagent Agent)")
	}
	if err = (&workflowcontroller.WorkflowExposureReconciler{
		Client:                 mgr.GetClient(),
		Scheme:                 mgr.GetScheme(),
		ServeA2AImage:          serveA2AImage,
		ServeA2AServiceAccount: serveA2AServiceAccount,
		ServeA2AClusterRole:    serveA2AClusterRole,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "WorkflowExposure")
		os.Exit(1)
	}

	// Validating webhooks (paths must match ValidatingWebhookConfiguration clientConfig.service.path)
	//
	// Register through mgr.GetWebhookServer() rather than the raw webhookServer variable:
	// controller-runtime only adds the webhook server as a manager Runnable inside
	// GetWebhookServer()'s sync.Once (controller-runtime@v0.24.1/pkg/manager/internal.go:278-288).
	// Passing WebhookServer via ctrl.Options above does NOT register it as a Runnable by
	// itself, so calling Register directly on webhookServer would silently build a server
	// that never starts and never serves admission requests.
	hookPathWorkflow := "/validate-ottoflow-nirmata-io-v1alpha1-workflow"
	hookPathWorkflowRun := "/validate-ottoflow-nirmata-io-v1alpha1-workflowrun"
	hookPathAgent := "/validate-ottoflow-nirmata-io-v1alpha1-agent"
	hookPathMCPServer := "/validate-ottoflow-nirmata-io-v1alpha1-mcpserver"
	mgr.GetWebhookServer().Register(hookPathWorkflow, &ctrlwebhook.Admission{
		Handler: admission.WithValidator(scheme, &ottowebhook.WorkflowValidator{
			Client:     mgr.GetClient(),
			Authorizer: clientset.AuthorizationV1().SubjectAccessReviews(),
		}),
	})
	mgr.GetWebhookServer().Register(hookPathWorkflowRun, &ctrlwebhook.Admission{
		Handler: admission.WithValidator(scheme, &ottowebhook.WorkflowRunValidator{
			Client:     mgr.GetClient(),
			Authorizer: clientset.AuthorizationV1().SubjectAccessReviews(),
		}),
	})
	mgr.GetWebhookServer().Register(hookPathAgent, &ctrlwebhook.Admission{
		Handler: admission.WithValidator(scheme, &ottowebhook.AgentValidator{}),
	})
	mgr.GetWebhookServer().Register(hookPathMCPServer, &ctrlwebhook.Admission{
		Handler: admission.WithValidator(scheme, &ottowebhook.MCPServerValidator{}),
	})
	setupLog.Info("registered validating webhooks",
		"workflow", hookPathWorkflow, "workflowRun", hookPathWorkflowRun,
		"agent", hookPathAgent, "mcpServer", hookPathMCPServer)

	//+kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	// Start agent-executor TLS certificate controller (internal cert generation, no cert-manager)
	if agentExecutorServiceName != "" {
		logger := ctrl.Log.WithName("agent-executor-tls")
		err := certmanager.Setup(ctx, logger, mgr.GetConfig(), agentExecutorNamespace, agentExecutorServiceName)
		if err != nil {
			setupLog.Error(err, "unable to start agent-executor TLS certificate controller")
			os.Exit(1)
		}
	}

	_, flushTracing, err := tracing.InitTracerProvider(ctx, "ottoflow-controller")
	if err != nil {
		setupLog.Error(err, "failed to init tracer provider, continuing without traces")
	} else {
		defer func() { _ = flushTracing(ctx) }()
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
