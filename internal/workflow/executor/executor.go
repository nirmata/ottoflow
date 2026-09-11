/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	metricsclientset "k8s.io/metrics/pkg/client/clientset/versioned"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/agent"
	"github.com/nirmata/ottoflow/internal/metrics"
)

// WorkflowEventRecorder records Kubernetes events for workflow and step transitions.
// Pass nil to disable event emission (e.g. CLI). Use corev1.EventTypeNormal and corev1.EventTypeWarning for eventtype.
type WorkflowEventRecorder interface {
	Eventf(regarding runtime.Object, related runtime.Object, eventtype, reason, action, note string, args ...interface{})
}

// MCPManager manages MCP client connections
type MCPManager interface {
	GetClient(ctx context.Context, serverName string, namespace string) (agent.MCPClient, error)
	// Close stops background eviction and closes all cached client connections.
	Close() error
}

// mcpManagerImpl implements MCPManager using agent.MCPClientManager
type mcpManagerImpl struct {
	manager *agent.MCPClientManager
}

// NewMCPManager creates a new MCP manager with idle-client eviction running in the background.
// Callers must call Close() when the manager is no longer needed to stop the eviction goroutine.
// localExecutionMode selects how MCP env/auth Secrets are resolved: true (CLI local mode) does
// a live client.Client.Get; false (the in-cluster workflow-runner Job) reads from the files the
// controller mounted into the pod (internal/secretmount) and never calls the Secret API.
func NewMCPManager(k8sClient client.Client, localExecutionMode bool) (MCPManager, error) {
	factory := agent.NewDefaultMCPClientFactory(k8sClient, localExecutionMode)
	manager := agent.NewMCPClientManager(k8sClient, factory)
	// Start background eviction of idle MCP clients (stdio-backed servers spawn child processes).
	// The goroutine is stopped when Close() is called on the returned manager.
	manager.StartEviction(5*time.Minute, agent.DefaultMCPClientIdleTimeout)
	return &mcpManagerImpl{manager: manager}, nil
}

// GetClient gets or creates an MCP client for the given server
func (m *mcpManagerImpl) GetClient(ctx context.Context, serverName string, namespace string) (agent.MCPClient, error) {
	return m.manager.GetClient(ctx, serverName, namespace)
}

// Close stops the background eviction goroutine and closes all cached MCP client connections.
func (m *mcpManagerImpl) Close() error {
	return m.manager.Close()
}

// ProgressCallback is invoked when workflow execution state changes (e.g., step completes).
// Used by CLI for streaming progress display. workflow and workflowRun are passed for context.
type ProgressCallback func(workflowRun *ottoflowv1alpha1.WorkflowRun, workflow *ottoflowv1alpha1.Workflow)

// WorkflowExecutor executes workflows
type WorkflowExecutor struct {
	client             client.Client
	controlClient      client.Client
	contextManager     *ContextManager
	celEvaluator       *CELEvaluator
	agentExecutor      agent.AgentExecutor
	mcpManager         MCPManager
	prometheusClient   PrometheusClient
	localExecutionMode bool // when true, agent steps run in-process (CLI local mode)
	maxWorkers         int  // Maximum number of concurrent workers for forEach steps
	progressCallback   ProgressCallback
	eventRecorder      WorkflowEventRecorder
	// currentWorkflow is set during ExecuteWorkflow so forEach (and other step types) can invoke progress callback with workflow
	currentWorkflow *ottoflowv1alpha1.Workflow
	// outboundRateLimiter is set during ExecuteWorkflow when spec.executionLimits.outboundRequestsPerMinute is set; used before MCP/agent calls
	outboundRateLimiter *rate.Limiter
	// execHTTPClient/Once/Err: lazily-initialized shared HTTP client for exec endpoint calls (thread-safe via sync.Once).
	execHTTPClientOnce sync.Once
	execHTTPClient     *http.Client
	execHTTPClientErr  error
	// openReportsCRD/Once/Avail/Err: cached per-run CRD availability check for openReport steps (thread-safe via sync.Once).
	openReportsCRDOnce  sync.Once
	openReportsCRDAvail bool
	openReportsCRDErr   error
	checkpointManager   *CheckpointManager
	providerOverride    string
	modelOverride       string
	// kubeClient is the typed Kubernetes clientset propagated to the CELEvaluator for resource.GetLogs.
	// nil when the executor was created without a rest.Config (convenience wrappers, CLI local mode).
	kubeClient kubernetes.Interface
}

// Close releases resources held by the executor, including stopping the MCP client eviction
// goroutine and closing cached MCP connections. Callers should call Close when the executor
// is no longer needed.
func (e *WorkflowExecutor) Close() error {
	if e.mcpManager != nil {
		return e.mcpManager.Close()
	}
	return nil
}

// SetProgressCallback sets an optional callback invoked when step status changes.
func (e *WorkflowExecutor) SetProgressCallback(cb ProgressCallback) {
	e.progressCallback = cb
}

func (e *WorkflowExecutor) SetCheckpointManager(cm *CheckpointManager) {
	e.checkpointManager = cm
}

// SetAgentOverrides sets provider and model overrides for agent steps (local mode only).
// When set, agent CRD values are overridden before execution.
func (e *WorkflowExecutor) SetAgentOverrides(provider, model string) {
	e.providerOverride = provider
	e.modelOverride = model
}

// SetImageDataFetcher sets the image data fetcher used by the CEL evaluator (for tests). When nil, default is used.
func (e *WorkflowExecutor) SetImageDataFetcher(f ImageDataFetcher) {
	e.celEvaluator.SetImageDataFetcher(f)
}

// waitOutboundRateLimit blocks until the outbound rate limiter allows a request, or ctx is done.
// No-op if no rate limiter is configured.
func (e *WorkflowExecutor) waitOutboundRateLimit(ctx context.Context) error {
	if e.outboundRateLimiter == nil {
		return nil
	}
	return e.outboundRateLimiter.Wait(ctx)
}

// SetCELCache pre-populates this executor's CEL evaluator with programs from
// the shared compilation cache and sets the workflow's CEL cost limit for any
// on-the-fly compilation. Pass the workflow so the cost limit can be applied.
func (e *WorkflowExecutor) SetCELCache(cache *CELCompilationCache, workflowKey string, workflow *ottoflowv1alpha1.Workflow) {
	if workflow != nil {
		e.celEvaluator.SetCELCostLimit(ResolveCELCostLimit(&workflow.Spec))
	}
	if cache == nil {
		return
	}
	e.celEvaluator.PreloadFromCache(cache, workflowKey)
}

// NewWorkflowExecutor creates a new workflow executor
func NewWorkflowExecutor(k8sClient client.Client, workflowRun *ottoflowv1alpha1.WorkflowRun) (*WorkflowExecutor, error) {
	return NewWorkflowExecutorWithMetrics(k8sClient, nil, nil, nil, workflowRun, 0, 0, nil)
}

// NewWorkflowExecutorWithMetrics creates a new workflow executor with optional metrics clients
// celCacheSize is the maximum number of compiled CEL expressions to cache (0 uses default)
// maxWorkers is the maximum number of concurrent workers for forEach steps (0 uses default of 5)
// eventRecorder is optional; pass nil to disable Kubernetes event emission (e.g. CLI)
func NewWorkflowExecutorWithMetrics(
	k8sClient client.Client,
	metricsClient metricsclientset.Interface,
	customMetricsClient CustomMetricsClient,
	prometheusClient PrometheusClient,
	workflowRun *ottoflowv1alpha1.WorkflowRun,
	celCacheSize int,
	maxWorkers int,
	eventRecorder WorkflowEventRecorder,
) (*WorkflowExecutor, error) {
	return NewWorkflowExecutorWithAgentExecutor(k8sClient, metricsClient, customMetricsClient, prometheusClient, workflowRun, nil, false, celCacheSize, maxWorkers, eventRecorder)
}

// NewWorkflowExecutorWithAgentExecutor creates a new workflow executor with optional metrics clients and custom agent executor.
// localExecutionMode enables in-process agent execution (used by CLI when --workflow-dir is set).
// If agentExecutor is nil, a default RoutingAgentExecutor is created
// celCacheSize is the maximum number of compiled CEL expressions to cache (0 uses default)
// maxWorkers is the maximum number of concurrent workers for forEach steps (0 uses default of 5)
// eventRecorder is optional; pass nil to disable Kubernetes event emission
func NewWorkflowExecutorWithAgentExecutor(
	k8sClient client.Client,
	metricsClient metricsclientset.Interface,
	customMetricsClient CustomMetricsClient,
	prometheusClient PrometheusClient,
	workflowRun *ottoflowv1alpha1.WorkflowRun,
	agentExecutor agent.AgentExecutor,
	localExecutionMode bool,
	celCacheSize int,
	maxWorkers int,
	eventRecorder WorkflowEventRecorder,
) (*WorkflowExecutor, error) {
	return NewWorkflowExecutorWithClientsAndAgentExecutor(k8sClient, k8sClient, metricsClient, customMetricsClient, prometheusClient, workflowRun, agentExecutor, nil, localExecutionMode, celCacheSize, maxWorkers, eventRecorder, nil)
}

// NewWorkflowExecutorWithAgentExecutorAndMCPManager creates an executor with a custom agent executor and MCP manager.
// Used by tests to inject mock MCPManager for MCP tool call steps.
func NewWorkflowExecutorWithAgentExecutorAndMCPManager(
	k8sClient client.Client,
	metricsClient metricsclientset.Interface,
	customMetricsClient CustomMetricsClient,
	prometheusClient PrometheusClient,
	workflowRun *ottoflowv1alpha1.WorkflowRun,
	agentExecutor agent.AgentExecutor,
	mcpManager MCPManager,
	localExecutionMode bool,
	celCacheSize int,
	maxWorkers int,
	eventRecorder WorkflowEventRecorder,
) (*WorkflowExecutor, error) {
	return NewWorkflowExecutorWithClientsAndAgentExecutor(k8sClient, k8sClient, metricsClient, customMetricsClient, prometheusClient, workflowRun, agentExecutor, mcpManager, localExecutionMode, celCacheSize, maxWorkers, eventRecorder, nil)
}

// NewWorkflowExecutorWithClientsAndAgentExecutor creates an executor with separate control-plane and target-cluster clients.
// controlClient is used for OttoFlow control-plane objects; targetClient is used for resource operations and CEL resource macros.
// kubeClient is optional; when non-nil, resource.GetLogs CEL calls use it. Pass nil for convenience callers that lack a rest.Config.
func NewWorkflowExecutorWithClientsAndAgentExecutor(
	controlClient client.Client,
	targetClient client.Client,
	metricsClient metricsclientset.Interface,
	customMetricsClient CustomMetricsClient,
	prometheusClient PrometheusClient,
	workflowRun *ottoflowv1alpha1.WorkflowRun,
	agentExecutor agent.AgentExecutor,
	mcpManager MCPManager,
	localExecutionMode bool,
	celCacheSize int,
	maxWorkers int,
	eventRecorder WorkflowEventRecorder,
	kubeClient kubernetes.Interface,
) (*WorkflowExecutor, error) {
	contextManager := NewContextManager(workflowRun)

	celEvaluator, err := NewCELEvaluatorWithMetrics(targetClient, metricsClient, customMetricsClient, prometheusClient, kubeClient, workflowRun, celCacheSize, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create CEL evaluator: %w", err)
	}

	if mcpManager == nil {
		var err error
		mcpManager, err = NewMCPManager(controlClient, localExecutionMode)
		if err != nil {
			return nil, fmt.Errorf("failed to create MCP manager: %w", err)
		}
	}

	if agentExecutor == nil {
		// Pass mcpManager as MCPClientProvider so agents with Spec.MCPTools get tools during LLM execution
		agentExecutor = agent.NewRoutingAgentExecutor(mcpManager)
	}

	// Default maxWorkers to 5 if not specified
	if maxWorkers <= 0 {
		maxWorkers = 5
	}

	return &WorkflowExecutor{
		client:             targetClient,
		controlClient:      controlClient,
		contextManager:     contextManager,
		celEvaluator:       celEvaluator,
		agentExecutor:      agentExecutor,
		mcpManager:         mcpManager,
		prometheusClient:   prometheusClient,
		localExecutionMode: localExecutionMode,
		maxWorkers:         maxWorkers,
		eventRecorder:      eventRecorder,
		kubeClient:         kubeClient,
	}, nil
}

// GetContextManager returns the context manager (for controller access)
func (e *WorkflowExecutor) GetContextManager() *ContextManager {
	return e.contextManager
}

// newChildExecutor creates an executor for running a sub-workflow inline (same process/Job).
// Used by executeWorkflowReference to collapse WorkflowRef at runtime for code reuse.
func (e *WorkflowExecutor) newChildExecutor(childWorkflowRun *ottoflowv1alpha1.WorkflowRun) (*WorkflowExecutor, error) {
	child, err := NewWorkflowExecutorWithClientsAndAgentExecutor(
		e.controlClient,
		e.client,
		nil, // metricsClient - not needed for inline child
		&NoOpCustomMetricsClient{},
		e.prometheusClient,
		childWorkflowRun,
		e.agentExecutor,
		e.mcpManager,
		e.localExecutionMode,
		0,
		e.maxWorkers,
		e.eventRecorder,
		e.kubeClient,
	)
	if err != nil {
		return nil, err
	}
	// Propagate setter-based fields so sub-workflows behave identically to the parent.
	child.progressCallback = e.progressCallback
	// Share the parent's already-initialized HTTP client directly so child executors (forEach)
	// reuse the same connection pool without re-initializing.
	if c := e.execHTTPClient; c != nil {
		child.execHTTPClient = c
		child.execHTTPClientOnce.Do(func() {}) // mark Once as done so child won't re-init
	}
	child.SetAgentOverrides(e.providerOverride, e.modelOverride)
	return child, nil
}

// eventConfig returns the effective event config (workflowRun overrides workflow).
// enabled: when false, no events. level: Workflow (workflow-level only) or WorkflowAndSteps (workflow + step events).
func eventConfig(workflow *ottoflowv1alpha1.Workflow, workflowRun *ottoflowv1alpha1.WorkflowRun) (enabled bool, level string) {
	enabled = true
	level = "WorkflowAndSteps"
	if workflow.Spec.Events != nil {
		if workflow.Spec.Events.Enabled != nil {
			enabled = *workflow.Spec.Events.Enabled
		}
		if workflow.Spec.Events.Level != "" {
			level = workflow.Spec.Events.Level
		}
	}
	if workflowRun.Spec.Events != nil {
		if workflowRun.Spec.Events.Enabled != nil {
			enabled = *workflowRun.Spec.Events.Enabled
		}
		if workflowRun.Spec.Events.Level != "" {
			level = workflowRun.Spec.Events.Level
		}
	}
	return enabled, level
}

// emitWorkflowLevelEvent emits workflow-level events (WorkflowRunning, WorkflowSucceeded, WorkflowFailed).
// Only emits when events are enabled.
func (e *WorkflowExecutor) emitWorkflowLevelEvent(workflow *ottoflowv1alpha1.Workflow, workflowRun *ottoflowv1alpha1.WorkflowRun, reason, eventType, message string) {
	if e.eventRecorder == nil {
		return
	}
	enabled, _ := eventConfig(workflow, workflowRun)
	if !enabled {
		return
	}
	e.eventRecorder.Eventf(workflowRun, nil, eventType, reason, reason, "%s", message)
}

func (e *WorkflowExecutor) emitStepEvent(workflow *ottoflowv1alpha1.Workflow, workflowRun *ottoflowv1alpha1.WorkflowRun, message string) {
	if e.eventRecorder == nil {
		return
	}
	enabled, level := eventConfig(workflow, workflowRun)
	if !enabled || level != "WorkflowAndSteps" {
		return
	}
	e.eventRecorder.Eventf(workflowRun, nil, corev1.EventTypeNormal, "WorkflowExecution", "WorkflowExecution", "%s", message)
}

func (e *WorkflowExecutor) emitStepEventWarning(workflow *ottoflowv1alpha1.Workflow, workflowRun *ottoflowv1alpha1.WorkflowRun, message string) {
	if e.eventRecorder == nil {
		return
	}
	enabled, level := eventConfig(workflow, workflowRun)
	if !enabled || level != "WorkflowAndSteps" {
		return
	}
	e.eventRecorder.Eventf(workflowRun, nil, corev1.EventTypeWarning, "WorkflowExecution", "WorkflowExecution", "%s", message)
}

// ExecuteWorkflow executes a workflow
func (e *WorkflowExecutor) ExecuteWorkflow(ctx context.Context, workflow *ottoflowv1alpha1.Workflow, workflowRun *ottoflowv1alpha1.WorkflowRun) error {
	// Apply workflow's CEL cost limit for this run (CLI and any path that didn't set it via SetCELCache)
	e.celEvaluator.SetCELCostLimit(ResolveCELCostLimit(&workflow.Spec))

	restoredFromCheckpoint, err := e.loadCheckpointIfNeeded(ctx, workflowRun)
	if err != nil {
		return err
	}

	err = e.contextManager.InitializeContext(ctx, workflow, workflowRun.Spec.InputValues)
	if err != nil {
		return fmt.Errorf("failed to initialize context: %w", err)
	}

	if !restoredFromCheckpoint {
		if err := e.evaluateWorkflowVariables(ctx, workflow); err != nil {
			return err
		}
	}

	defer e.checkpointCleanup(ctx, workflow, workflowRun)()

	// Build DAG
	dag, err := BuildDAG(workflow.Spec.Steps)
	if err != nil {
		return fmt.Errorf("failed to build DAG: %w", err)
	}

	// Initialize step statuses
	if workflowRun.Status.StepStatuses == nil {
		workflowRun.Status.StepStatuses = make(map[string]ottoflowv1alpha1.StepStatus)
	}
	for _, step := range workflow.Spec.Steps {
		if _, exists := workflowRun.Status.StepStatuses[step.Name]; !exists {
			workflowRun.Status.StepStatuses[step.Name] = ottoflowv1alpha1.StepStatus{
				Phase: ottoflowv1alpha1.StepPhasePending,
			}
		}
	}

	// Set current workflow so forEach (and other steps) can invoke progress callback with workflow for CLI display
	e.currentWorkflow = workflow
	defer func() { e.currentWorkflow = nil }()

	// Set rate limiter for outbound calls (MCP, agent) when spec limits are set
	if workflow.Spec.ExecutionLimits != nil && workflow.Spec.ExecutionLimits.OutboundRequestsPerMinute != nil && *workflow.Spec.ExecutionLimits.OutboundRequestsPerMinute > 0 {
		rpm := *workflow.Spec.ExecutionLimits.OutboundRequestsPerMinute
		e.outboundRateLimiter = rate.NewLimiter(rate.Limit(rpm)/60, int(rpm)/6+1) // burst = rpm/6 + 1
		defer func() { e.outboundRateLimiter = nil }()
	}

	// Set workflow to Running if not already (Phase may be "" or Pending, e.g. from CLI)
	workflowName := workflowRun.Spec.WorkflowRef.Name
	namespace := workflowRun.Namespace
	if workflowRun.Status.Phase == "" || workflowRun.Status.Phase == ottoflowv1alpha1.WorkflowRunPhasePending {
		workflowRun.Status.Phase = ottoflowv1alpha1.WorkflowRunPhaseRunning
		startNow := metav1.Now()
		workflowRun.Status.StartTime = &startNow
		e.emitWorkflowLevelEvent(workflow, workflowRun, "WorkflowRunning", corev1.EventTypeNormal, "Workflow run started")
	}

	// Resolve maxConcurrentSteps: at most this many steps executed per iteration (batch limit)
	maxConcurrentSteps := int32(0) // 0 means no limit (process all ready)
	if workflow.Spec.ExecutionLimits != nil && workflow.Spec.ExecutionLimits.MaxConcurrentSteps != nil && *workflow.Spec.ExecutionLimits.MaxConcurrentSteps > 0 {
		maxConcurrentSteps = *workflow.Spec.ExecutionLimits.MaxConcurrentSteps
	}

	// Loop until all steps are complete
	maxIterations := len(workflow.Spec.Steps) * 2 // Safety limit
	for iteration := 0; iteration < maxIterations; iteration++ {
		completedSteps := make(map[string]bool)
		for _, step := range workflow.Spec.Steps {
			completedSteps[step.Name] = isStepDone(step, workflowRun.Status.StepStatuses[step.Name])
		}
		readySteps := dag.GetReadySteps(completedSteps)
		if maxConcurrentSteps > 0 && int32(len(readySteps)) > maxConcurrentSteps {
			readySteps = readySteps[:maxConcurrentSteps]
		}

		// If no ready steps, check if we're done
		if len(readySteps) == 0 {
			allDone := true
			for _, step := range workflow.Spec.Steps {
				if !isStepDone(step, workflowRun.Status.StepStatuses[step.Name]) {
					allDone = false
					break
				}
			}
			if allDone {
				e.recordWorkflowSuccess(ctx, workflow, workflowRun, workflowName, namespace)
				return nil
			}

			for _, step := range workflow.Spec.Steps {
				status := workflowRun.Status.StepStatuses[step.Name]
				if status.Phase == ottoflowv1alpha1.StepPhaseFailed {
					msg := fmt.Sprintf("Step %s failed: %s", step.Name, status.Error)
					e.recordWorkflowFailure(workflow, workflowRun, msg)
					return fmt.Errorf("workflow failed: step %s failed", step.Name)
				}
			}

			return nil
		}

		// Execute ready steps
		executedAny := false
		for _, stepName := range readySteps {
			step := findStepByName(workflow.Spec.Steps, stepName)
			if step == nil {
				return fmt.Errorf("step %s not found", stepName)
			}

			stepStatus := workflowRun.Status.StepStatuses[stepName]
			if stepStatus.Phase != ottoflowv1alpha1.StepPhasePending {
				continue // Already processed
			}

			executedAny = true

			// Check matchConditions
			shouldExecute, err := e.checkMatchConditions(ctx, *step)
			if err != nil {
				stepStatus.Phase = ottoflowv1alpha1.StepPhaseFailed
				stepStatus.Error = fmt.Sprintf("matchCondition evaluation failed: %v", err)
				workflowRun.Status.StepStatuses[stepName] = stepStatus
				e.emitStepEventWarning(workflow, workflowRun, fmt.Sprintf("Step %q failed: %v", stepName, err))
				e.invokeProgressCallback(workflowRun, workflow)
				if step.FailurePolicy == ottoflowv1alpha1.FailurePolicyContinue {
					continue
				}
				e.recordWorkflowFailure(workflow, workflowRun, fmt.Sprintf("Step %s failed: %v", stepName, err))
				return fmt.Errorf("matchCondition evaluation failed for step %s: %w", stepName, err)
			}

			if !shouldExecute {
				stepStatus.Phase = ottoflowv1alpha1.StepPhaseSkipped
				workflowRun.Status.StepStatuses[stepName] = stepStatus
				e.emitStepEvent(workflow, workflowRun, fmt.Sprintf("Step %q skipped (conditions not met)", stepName))
				e.invokeProgressCallback(workflowRun, workflow)
				e.saveCheckpoint(ctx, workflowRun, stepName)
				continue
			}

			// Execute step
			stepStatus.Phase = ottoflowv1alpha1.StepPhaseRunning
			startTime := time.Now()
			now := metav1.NewTime(startTime)
			stepStatus.StartTime = &now
			workflowRun.Status.StepStatuses[stepName] = stepStatus
			e.emitStepEvent(workflow, workflowRun, fmt.Sprintf("Step %q started", stepName))
			e.invokeProgressCallback(workflowRun, workflow)

			// Always start a step.* span for every step type — GenAI executors
			// (agent_executor, mcp_executor, external_agent_executor) add their own child spans.
			// IIFE so defer runs per-step, not at ExecuteWorkflow exit (defer-in-loop antipattern).
			err = func() (retErr error) {
				stepCtx, stepSpan := otel.Tracer("ottoflow").Start(ctx, "step."+stepName,
					trace.WithSpanKind(trace.SpanKindInternal),
					trace.WithAttributes(
						attribute.String("workflow.step.name", stepName),
						attribute.String("workflow.step.type", stepType(*step)),
						attribute.String("workflow.name", workflowName),
						attribute.String("workflow.run.namespace", namespace),
					))
				defer func() {
					if retErr != nil {
						stepSpan.SetStatus(codes.Error, retErr.Error())
					} else {
						stepSpan.SetStatus(codes.Ok, "")
					}
					stepSpan.End()
				}()
				_, retErr = e.executeStep(stepCtx, workflowRun, *step)
				return
			}()
			if err != nil {
				// Handle waitForCallback sentinel: set step Waiting and propagate up
				if errors.Is(err, ErrAwaitingCallback) {
					stepStatus = workflowRun.Status.StepStatuses[stepName]
					stepStatus.Phase = ottoflowv1alpha1.StepPhaseWaiting
					if wfc := step.WaitForCallback; wfc != nil && wfc.Message != "" {
						stepStatus.Message = wfc.Message
					} else if workflowRun.Status.PendingCallback != nil {
						stepStatus.Message = fmt.Sprintf("Awaiting callback, expires at %s",
							time.Unix(workflowRun.Status.PendingCallback.ExpiresAt, 0).Format(time.RFC3339))
					}
					workflowRun.Status.StepStatuses[stepName] = stepStatus
					e.invokeProgressCallback(workflowRun, workflow)
					// Propagate sentinel — runner main loop will exit with code 0
					return err
				}
				stepStatus = workflowRun.Status.StepStatuses[stepName]
				stepStatus.Phase = ottoflowv1alpha1.StepPhaseFailed
				stepStatus.Error = err.Error()
				stepStatus.Message = err.Error() // Also set Message for compatibility
				completionTime := time.Now()
				completionTimeMeta := metav1.NewTime(completionTime)
				stepStatus.CompletionTime = &completionTimeMeta
				workflowRun.Status.StepStatuses[stepName] = stepStatus
				e.emitStepEventWarning(workflow, workflowRun, fmt.Sprintf("Step %q failed: %s", stepName, err.Error()))
				e.invokeProgressCallback(workflowRun, workflow)

				if step.FailurePolicy == ottoflowv1alpha1.FailurePolicyContinue {
					e.saveCheckpoint(ctx, workflowRun, stepName)
					continue
				}

				e.recordWorkflowFailure(workflow, workflowRun, fmt.Sprintf("Step %s failed: %v", stepName, err))
				return err
			}
			// Mark step as succeeded
			// Re-read status to get the latest, but preserve StartTime
			stepStatus = workflowRun.Status.StepStatuses[stepName]
			startTimePreserved := stepStatus.StartTime
			stepStatus.Phase = ottoflowv1alpha1.StepPhaseSucceeded
			completionTime := time.Now()
			completionTimeMeta := metav1.NewTime(completionTime)
			stepStatus.CompletionTime = &completionTimeMeta
			// Ensure StartTime is preserved (in case re-read cleared it)
			if stepStatus.StartTime == nil && startTimePreserved != nil {
				stepStatus.StartTime = startTimePreserved
			}
			workflowRun.Status.StepStatuses[stepName] = stepStatus
			e.emitStepEvent(workflow, workflowRun, fmt.Sprintf("Step %q succeeded", stepName))
			e.invokeProgressCallback(workflowRun, workflow)
			e.contextManager.RecordStepCompletion(stepName)
			e.saveCheckpoint(ctx, workflowRun, stepName)
		}

		// If we didn't execute any steps in this iteration, break to avoid infinite loop
		if !executedAny {
			break
		}
	}

	allComplete := true
	for _, step := range workflow.Spec.Steps {
		if !isStepDone(step, workflowRun.Status.StepStatuses[step.Name]) {
			allComplete = false
			break
		}
	}
	if allComplete {
		e.recordWorkflowSuccess(ctx, workflow, workflowRun, workflowName, namespace)
	}

	return nil
}

func isStepDone(step ottoflowv1alpha1.Step, status ottoflowv1alpha1.StepStatus) bool {
	if status.Phase == ottoflowv1alpha1.StepPhaseSucceeded || status.Phase == ottoflowv1alpha1.StepPhaseSkipped {
		return true
	}
	return status.Phase == ottoflowv1alpha1.StepPhaseFailed && step.FailurePolicy == ottoflowv1alpha1.FailurePolicyContinue
}

func (e *WorkflowExecutor) evaluateWorkflowVariables(ctx context.Context, workflow *ottoflowv1alpha1.Workflow) error {
	if len(workflow.Spec.Variables) == 0 {
		return nil
	}
	contextData, err := e.contextManager.ReadContext(ctx)
	if err != nil {
		return fmt.Errorf("failed to read context: %w", err)
	}
	vars := e.celEvaluator.BuildVariableMap(contextData)
	inMemoryContext := e.contextManager.GetContext()
	variablesMap := inMemoryContext["variables"].(map[string]interface{})
	for _, variable := range workflow.Spec.Variables {
		result, err := e.celEvaluator.EvaluateExpression(ctx, variable.Expression, vars)
		if err != nil {
			return fmt.Errorf("failed to evaluate variable '%s': %w", variable.Name, err)
		}
		variablesMap[variable.Name] = result
		contextData, err = e.contextManager.ReadContext(ctx)
		if err != nil {
			return fmt.Errorf("failed to re-read context after variable %q: %w", variable.Name, err)
		}
		vars = e.celEvaluator.BuildVariableMap(contextData)
	}
	return nil
}

func (e *WorkflowExecutor) recordWorkflowFailure(
	workflow *ottoflowv1alpha1.Workflow,
	workflowRun *ottoflowv1alpha1.WorkflowRun,
	message string,
) {
	workflowRun.Status.Phase = ottoflowv1alpha1.WorkflowRunPhaseFailed
	workflowRun.Status.Message = message
	e.emitWorkflowLevelEvent(workflow, workflowRun, "WorkflowFailed", corev1.EventTypeWarning, message)
}

func (e *WorkflowExecutor) recordWorkflowSuccess(
	ctx context.Context,
	workflow *ottoflowv1alpha1.Workflow,
	workflowRun *ottoflowv1alpha1.WorkflowRun,
	workflowName, namespace string,
) {
	workflowRun.Status.Phase = ottoflowv1alpha1.WorkflowRunPhaseSucceeded
	now := metav1.Now()
	workflowRun.Status.CompletionTime = &now
	e.emitWorkflowLevelEvent(workflow, workflowRun, "WorkflowSucceeded", corev1.EventTypeNormal, "Workflow run completed successfully")

	if err := e.evaluateWorkflowOutputs(ctx, workflow, workflowRun, workflowName, namespace); err != nil {
		fmt.Printf("Warning: failed to evaluate workflow outputs: %v\n", err)
	}
	e.invokeProgressCallback(workflowRun, workflow)
}

func (e *WorkflowExecutor) invokeProgressCallback(workflowRun *ottoflowv1alpha1.WorkflowRun, workflow *ottoflowv1alpha1.Workflow) {
	if e.progressCallback != nil {
		e.progressCallback(workflowRun, workflow)
	}
}

// invokeForEachProgressCallback invokes the progress callback with the current workflow when set.
// Used by forEach to report per-item progress (e.g. "3/5 items") so the CLI can update the display.
func (e *WorkflowExecutor) invokeForEachProgressCallback(workflowRun *ottoflowv1alpha1.WorkflowRun) {
	if e.progressCallback != nil && e.currentWorkflow != nil {
		e.progressCallback(workflowRun, e.currentWorkflow)
	}
}

// checkMatchConditions evaluates match conditions for a step
func (e *WorkflowExecutor) checkMatchConditions(ctx context.Context, step ottoflowv1alpha1.Step) (bool, error) {
	if len(step.MatchConditions) == 0 {
		return true, nil // No conditions means always execute
	}

	// Read context
	contextData, err := e.contextManager.ReadContext(ctx)
	if err != nil {
		return false, err
	}

	// Build variable map
	vars := e.celEvaluator.BuildVariableMap(contextData)

	// Evaluate all conditions - ALL must be true
	for _, condition := range step.MatchConditions {
		result, err := e.celEvaluator.EvaluateExpression(ctx, condition.Expression, vars)
		if err != nil {
			return false, fmt.Errorf("failed to evaluate matchCondition '%s': %w", condition.Name, err)
		}

		// Convert to boolean
		boolResult, ok := result.(bool)
		if !ok {
			return false, fmt.Errorf("matchCondition '%s' did not evaluate to boolean", condition.Name)
		}

		if !boolResult {
			return false, nil // Any false condition means skip step
		}
	}

	return true, nil // All conditions true
}

// executeStep executes a single step. Returns (outputs, nil) on success so forEach can collect per-item outputs; returns (nil, err) on failure.
func (e *WorkflowExecutor) executeStep(ctx context.Context, workflowRun *ottoflowv1alpha1.WorkflowRun, step ottoflowv1alpha1.Step) (map[string]interface{}, error) {
	// Check step type and delegate to appropriate handler
	if step.ForEach != nil {
		err := e.executeForEach(ctx, workflowRun, step)
		return nil, err
	}
	if step.StepTemplateRef != nil {
		return e.executeStepTemplate(ctx, workflowRun, step)
	}
	if step.WorkflowRef != nil {
		outputs, err := e.executeWorkflowReference(ctx, workflowRun, step)
		return outputs, err
	}
	if step.AgentRef != nil {
		return e.executeAgentStep(ctx, workflowRun, step)
	}
	if step.MCPToolCall != nil {
		return e.executeMCPToolCall(ctx, workflowRun, step)
	}
	if step.ExternalAgentRef != nil {
		return e.executeExternalAgentStep(ctx, workflowRun, step)
	}
	if step.ResourceQuery != nil {
		return e.executeResourceQuery(ctx, workflowRun, step)
	}
	if step.PrometheusQuery != nil {
		return e.executePrometheusQuery(ctx, workflowRun, step)
	}
	if step.Mutate != nil {
		return e.executeMutate(ctx, workflowRun, step)
	}
	if step.OpenReport != nil {
		return e.executeOpenReportStep(ctx, workflowRun, step)
	}
	if step.WaitForCallback != nil {
		return e.executeWaitForCallback(ctx, workflowRun, step)
	}
	// Default: expression-based step
	contextData, err := e.contextManager.ReadContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to read context: %w", err)
	}

	// Build variable map for CEL evaluation
	vars := e.celEvaluator.BuildVariableMap(contextData)

	// Evaluate expressions sequentially and update context
	if len(step.Expressions) > 0 {
		exprResults, err := e.celEvaluator.EvaluateStepExpressions(ctx, step, vars)
		if err != nil {
			return nil, fmt.Errorf("failed to evaluate expressions: %w", err)
		}

		// Update context with expression results
		if contextData["expressions"] == nil {
			contextData["expressions"] = make(map[string]interface{})
		}
		for k, v := range exprResults {
			contextData["expressions"].(map[string]interface{})[k] = v
		}

		// Update vars for output evaluation
		vars = e.celEvaluator.BuildVariableMap(contextData)
	}

	// Evaluate outputs
	outputs, err := e.celEvaluator.EvaluateStepOutputs(ctx, step, vars)
	if err != nil {
		return nil, fmt.Errorf("failed to evaluate outputs: %w", err)
	}

	// Write outputs to context (directly to variables map)
	if err := e.contextManager.WriteStepOutputs(ctx, step.Name, outputs); err != nil {
		return nil, fmt.Errorf("failed to write step outputs: %w", err)
	}

	return outputs, nil
}

// stepType returns the OTel workflow.step.type attribute value for a step.
func stepType(step ottoflowv1alpha1.Step) string {
	switch {
	case step.ForEach != nil:
		return "forEach"
	case step.StepTemplateRef != nil:
		return "stepTemplateRef"
	case step.WorkflowRef != nil:
		return "workflowRef"
	case step.AgentRef != nil:
		return "agentRef"
	case step.MCPToolCall != nil:
		return "mcpToolCall"
	case step.ExternalAgentRef != nil:
		return "externalAgentRef"
	case step.ResourceQuery != nil:
		return "resourceQuery"
	case step.PrometheusQuery != nil:
		return "prometheusQuery"
	case step.Mutate != nil:
		return "mutate"
	case step.OpenReport != nil:
		return "openReport"
	default:
		return "expression"
	}
}

// executeWorkflowReference executes a sub-workflow step by running the referenced workflow
// inline in the same process/Job (collapsed at runtime for code reuse). This allows WorkflowRef
// to work in both cluster and local execution; no separate WorkflowRun or Job is created.
func (e *WorkflowExecutor) executeWorkflowReference(ctx context.Context, workflowRun *ottoflowv1alpha1.WorkflowRun, step ottoflowv1alpha1.Step) (map[string]interface{}, error) {
	if step.WorkflowRef == nil {
		return nil, fmt.Errorf("WorkflowRef is nil")
	}

	// Get current context for CEL evaluation
	contextData, err := e.contextManager.ReadContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to read context: %w", err)
	}

	// Build variable map for CEL evaluation
	vars := e.celEvaluator.BuildVariableMap(contextData)

	// Evaluate input mappings using CEL
	inputValues := make(map[string]string)
	for inputName, expr := range step.WorkflowRef.Inputs {
		result, err := e.celEvaluator.EvaluateExpression(ctx, expr, vars)
		if err != nil {
			return nil, fmt.Errorf("failed to evaluate input '%s': %w", inputName, err)
		}
		// Convert result to string
		var strValue string
		if str, ok := result.(string); ok {
			strValue = str
		} else {
			// Convert to JSON string for non-string types
			jsonBytes, err := json.Marshal(result)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal input '%s': %w", inputName, err)
			}
			strValue = string(jsonBytes)
		}
		inputValues[inputName] = strValue
	}

	// Determine namespace for sub-workflow
	subWorkflowNamespace := step.WorkflowRef.Namespace
	if subWorkflowNamespace == "" {
		subWorkflowNamespace = workflowRun.Namespace
	}

	// Fetch the referenced workflow
	subWorkflow := &ottoflowv1alpha1.Workflow{}
	subWorkflowKey := client.ObjectKey{
		Name:      step.WorkflowRef.Name,
		Namespace: subWorkflowNamespace,
	}
	if err := e.controlClient.Get(ctx, subWorkflowKey, subWorkflow); err != nil {
		return nil, fmt.Errorf("failed to get sub-workflow %s: %w", subWorkflowKey, err)
	}

	// In-memory child WorkflowRun (not created in API server); run inline in same process/Job.
	childRun := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s-inline", workflowRun.Name, step.Name),
			Namespace: subWorkflowNamespace,
		},
		Spec: ottoflowv1alpha1.WorkflowRunSpec{
			WorkflowRef: ottoflowv1alpha1.WorkflowRef{
				Name:      step.WorkflowRef.Name,
				Namespace: subWorkflowNamespace,
			},
			InputValues: inputValues,
			ClusterRef:  workflowRun.Spec.ClusterRef,
			Execution:   workflowRun.Spec.Execution,
		},
		Status: ottoflowv1alpha1.WorkflowRunStatus{
			Phase: ottoflowv1alpha1.WorkflowRunPhasePending,
		},
	}

	childExec, err := e.newChildExecutor(childRun)
	if err != nil {
		return nil, fmt.Errorf("failed to create executor for sub-workflow: %w", err)
	}

	if err := childExec.ExecuteWorkflow(ctx, subWorkflow, childRun); err != nil {
		if workflowRun.Status.StepStatuses == nil {
			workflowRun.Status.StepStatuses = make(map[string]ottoflowv1alpha1.StepStatus)
		}
		stepStatus := workflowRun.Status.StepStatuses[step.Name]
		stepStatus.Phase = ottoflowv1alpha1.StepPhaseFailed
		stepStatus.Error = err.Error()
		workflowRun.Status.StepStatuses[step.Name] = stepStatus
		return nil, fmt.Errorf("sub-workflow failed: %w", err)
	}

	// Sub-workflow succeeded; copy outputs to parent step and update step status
	if workflowRun.Status.StepStatuses == nil {
		workflowRun.Status.StepStatuses = make(map[string]ottoflowv1alpha1.StepStatus)
	}
	stepStatus := workflowRun.Status.StepStatuses[step.Name]
	stepStatus.Phase = ottoflowv1alpha1.StepPhaseSucceeded
	workflowRun.Status.StepStatuses[step.Name] = stepStatus

	var out map[string]interface{}
	if childRun.Status.Outputs != nil {
		out = make(map[string]interface{})
		for k, v := range childRun.Status.Outputs {
			var val interface{}
			if err := json.Unmarshal(v.Raw, &val); err == nil {
				out[k] = val
			}
		}
		if len(out) > 0 {
			if err := e.contextManager.WriteStepOutputs(ctx, step.Name, out); err != nil {
				return nil, fmt.Errorf("failed to write step outputs: %w", err)
			}
		}
	}
	return out, nil
}

// evaluateWorkflowOutputs evaluates workflow-level outputs and emits custom metrics
func (e *WorkflowExecutor) evaluateWorkflowOutputs(ctx context.Context, workflow *ottoflowv1alpha1.Workflow, workflowRun *ottoflowv1alpha1.WorkflowRun, workflowName, namespace string) error {
	if len(workflow.Spec.Outputs) == 0 {
		return nil // No outputs to evaluate
	}

	// Get current context
	contextData := e.contextManager.GetContext()
	if contextData == nil {
		return fmt.Errorf("context not available")
	}

	// Add outputs map to context for outputs that reference earlier outputs
	outputsMap := make(map[string]interface{})
	contextData["outputs"] = outputsMap

	// Evaluate outputs
	outputs := make(map[string]apiextensionsv1.JSON)
	for _, outputDef := range workflow.Spec.Outputs {
		// Build variable map (includes outputs from previous iterations)
		vars := e.celEvaluator.BuildVariableMap(contextData)

		var result interface{}
		var err error

		// Value field takes precedence over Expression field
		if outputDef.Value != nil {
			// Unmarshal JSON value to Go interface{}
			var valueData interface{}
			if err := json.Unmarshal(outputDef.Value.Raw, &valueData); err != nil {
				fmt.Printf("Warning: failed to unmarshal workflow output '%s' value: %v\n", outputDef.Name, err)
				continue
			}
			result, err = e.celEvaluator.EvaluateOutputValue(ctx, valueData, vars)
			if err != nil {
				// Log error but continue with other outputs
				fmt.Printf("Warning: failed to evaluate workflow output '%s' value: %v\n", outputDef.Name, err)
				continue
			}
		} else if outputDef.Expression != "" {
			result, err = e.celEvaluator.EvaluateExpression(ctx, outputDef.Expression, vars)
			if err != nil {
				// Log error but continue with other outputs
				fmt.Printf("Warning: failed to evaluate workflow output '%s' expression: %v\n", outputDef.Name, err)
				continue
			}
		} else {
			fmt.Printf("Warning: workflow output '%s' must specify either 'expression' or 'value', skipping\n", outputDef.Name)
			continue
		}

		// Add to outputs map for subsequent output and metric label evaluation
		outputsMap[outputDef.Name] = result

		// Marshal result to JSON for status (sensitive outputs are not written to status)
		var resultJSON []byte
		if outputDef.Sensitive {
			resultJSON = []byte(`{"_ottoflow_redacted":true,"reason":"sensitive"}`)
		} else {
			var err error
			resultJSON, err = json.Marshal(result)
			if err != nil {
				fmt.Printf("Warning: failed to marshal workflow output '%s': %v\n", outputDef.Name, err)
				continue
			}
		}
		outputs[outputDef.Name] = apiextensionsv1.JSON{Raw: resultJSON}

		// Emit custom metric if output has metric config (vars now includes this output)
		if outputDef.Metric != nil {
			metricVars := e.celEvaluator.BuildVariableMap(contextData)
			metrics.EmitOutputMetric(ctx, result, outputDef, workflowName, namespace, metricVars, e.celEvaluator)
		}
	}

	// Add outputs to status
	if len(outputs) > 0 {
		workflowRun.Status.Outputs = outputs
	}

	return nil
}

// loadCheckpointIfNeeded restores state from a checkpoint on pod restart. Returns true if restored.
// Fires when Phase==Running (controller-restart scenario) or Attempts>0 (transient-failure retry).
func (e *WorkflowExecutor) loadCheckpointIfNeeded(ctx context.Context, workflowRun *ottoflowv1alpha1.WorkflowRun) (bool, error) {
	if e.contextManager.IsInitialized() {
		return false, nil
	}
	isRestart := workflowRun.Status.Phase == ottoflowv1alpha1.WorkflowRunPhaseRunning ||
		(workflowRun.Status.Execution != nil && workflowRun.Status.Execution.Attempts > 0)
	if !isRestart {
		return false, nil
	}
	if e.checkpointManager == nil || !e.checkpointManager.Enabled() {
		return false, nil
	}
	snapshot, err := e.checkpointManager.Load(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to load checkpoint: %w", err)
	}
	if snapshot == nil {
		return false, nil
	}
	klog.InfoS("checkpoint: restoring from checkpoint", "lastCompletedStep", snapshot.LastCompletedStep, "steps", len(snapshot.StepStatuses))
	e.contextManager.RestoreContext(snapshot.Context)
	if len(snapshot.CompletionOrder) > 0 {
		// Exact order was persisted (checkpoint written after this field was added).
		e.contextManager.SetCompletionOrder(snapshot.CompletionOrder)
	} else {
		// Older checkpoint without CompletionOrder: fall back to CompletionTime reconstruction.
		e.contextManager.RestoreCompletionOrder(snapshot.StepStatuses)
	}
	workflowRun.Status.StepStatuses = snapshot.StepStatuses
	return true, nil
}

// checkpointCleanup returns a deferred function that flushes writes and deletes the checkpoint on terminal state.
func (e *WorkflowExecutor) saveCheckpoint(ctx context.Context, workflowRun *ottoflowv1alpha1.WorkflowRun, stepName string) {
	if e.checkpointManager == nil {
		return
	}
	e.checkpointManager.SaveAsync(ctx, CheckpointSnapshot{
		Version:           1,
		WorkflowRunUID:    string(workflowRun.UID),
		LastCompletedStep: stepName,
		StepStatuses:      workflowRun.Status.StepStatuses,
		Context:           e.contextManager.GetContext(),
		CompletionOrder:   e.contextManager.CompletionOrder(),
	})
}

// checkpointCleanup returns a deferred function that, on terminal state, flushes and
// deletes the (opt-in) resumability checkpoint if enabled, and always writes a one-time
// audit snapshot of the run's final execution context — regardless of whether per-step
// checkpointing was enabled — so WorkflowRun.Status.AuditSnapshotConfigMap can be set
// before the caller persists status.
func (e *WorkflowExecutor) checkpointCleanup(ctx context.Context, workflow *ottoflowv1alpha1.Workflow, workflowRun *ottoflowv1alpha1.WorkflowRun) func() {
	if e.checkpointManager == nil {
		return func() {}
	}
	return func() {
		_ = e.checkpointManager.Flush(ctx)

		terminal := workflowRun.Status.Phase == ottoflowv1alpha1.WorkflowRunPhaseSucceeded ||
			workflowRun.Status.Phase == ottoflowv1alpha1.WorkflowRunPhaseFailed
		if !terminal {
			return
		}

		if e.checkpointManager.Enabled() {
			_ = e.checkpointManager.Delete(ctx)
		}

		// Redact sensitive:true outputs before persisting: GetContext() returns the live
		// in-memory map by reference, which holds the raw (unredacted) values — the
		// redaction evaluateWorkflowOutputs applies only covers the copy marshaled to
		// Status, not this map. See redactSensitiveContext for why every context-persisting
		// write path (this one included) must redact its own copy.
		redactedContext := redactSensitiveContext(e.contextManager.GetContext(), sensitiveOutputNames(workflow))

		snapshotName, err := e.checkpointManager.SaveAuditSnapshot(ctx, CheckpointSnapshot{
			Version:        1,
			WorkflowRunUID: string(workflowRun.UID),
			StepStatuses:   workflowRun.Status.StepStatuses,
			Context:        redactedContext,
		})
		if err != nil {
			klog.ErrorS(err, "audit snapshot: failed to save", "workflowRunUID", workflowRun.UID)
			workflowRun.Status.AuditSnapshotError = err.Error()
			if e.eventRecorder != nil {
				e.eventRecorder.Eventf(workflowRun, nil, corev1.EventTypeWarning, "AuditSnapshotFailed", "AuditSnapshotFailed",
					"Failed to persist run audit snapshot: %s", err)
			}
			return
		}
		workflowRun.Status.AuditSnapshotConfigMap = snapshotName
		workflowRun.Status.AuditSnapshotError = ""
	}
}

// findStepByName finds a step by name
func findStepByName(steps []ottoflowv1alpha1.Step, name string) *ottoflowv1alpha1.Step {
	for i := range steps {
		if steps[i].Name == name {
			return &steps[i]
		}
	}
	return nil
}
