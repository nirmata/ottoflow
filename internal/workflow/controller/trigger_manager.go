/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	celgo "github.com/google/cel-go/cel"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/logging"
)

// getObjectKind extracts the Kind from an object, handling cases where TypeMeta might not be set
func getObjectKind(obj client.Object) string {
	gvk := obj.GetObjectKind().GroupVersionKind()
	if gvk.Kind != "" {
		return gvk.Kind
	}
	// Fallback: try to infer from object type
	switch obj.(type) {
	case *corev1.Pod:
		return "Pod"
	case *corev1.Service:
		return "Service"
	case *corev1.ConfigMap:
		return "ConfigMap"
	case *corev1.Secret:
		return "Secret"
	default:
		// Last resort: use type name
		return fmt.Sprintf("%T", obj)
	}
}

// ResourceWatcherFactory creates a watch.Interface for a resource. Used in tests to inject
// a fake watcher without a real dynamic client.
type ResourceWatcherFactory interface {
	Watch(ctx context.Context, gvr schema.GroupVersionResource, namespace string, opts metav1.ListOptions) (watch.Interface, error)
}

// dedupEntry tracks the last WorkflowRun created for a (trigger, object) pair.
type dedupEntry struct {
	key       string    // last revision/dedupKey value that created a run (empty if time-window only)
	createdAt time.Time // when the last WorkflowRun was created
}

// defaultDedupWindow is the deduplication window applied by default to both
// webhook triggers (when DedupKey is set but DedupWindow is omitted — see
// WebhookTrigger.DedupWindow) and event triggers (when no revision field is
// auto-detected and DedupKey is not set — see CreateWorkflowRunFromEvent).
//
// For event triggers this window is keyed per-object (by UID), so it only ever
// suppresses a repeat event for an object already seen — e.g. a flapping Pod
// re-firing the same event over and over. It does nothing, and structurally
// cannot do anything, for a stream of distinct new objects: each one is a
// first-sight miss against dedup state, so it is never suppressed. That is
// exactly what a self-amplifying loop produces (e.g. a Pod-scoped trigger
// fed on its own runner pods, each a distinct new Pod — thousands of runs
// in minutes). That class of loop is prevented by the runner-label exclusion and
// the WorkflowRun ownership guard below, not by dedup. Event triggers may
// override this default via an explicit DedupWindow.
const defaultDedupWindow = 10 * time.Minute

const (
	watchReopenDelay    = time.Second
	watchReopenMaxDelay = 30 * time.Second
)

// runnerManagedLabel marks Jobs/Pods created by OttoFlow's WorkflowRun controller.
const runnerManagedLabel = "ottoflow.nirmata.io/workflowrun"

// workflowRunGroupKind identifies WorkflowRun events for the self-amplification
// guard in CreateWorkflowRunFromEvent. Comparing the full GroupKind (not just the
// Kind string) matters: a resource named "WorkflowRun" in a different (or core,
// empty-string) API group must not be mistaken for OttoFlow's own type.
var workflowRunGroupKind = ottoflowv1alpha1.GroupVersion.WithKind("WorkflowRun").GroupKind()

// isOwnWorkflowRun reports whether eventObject is a WorkflowRun that workflow itself
// created (i.e. a run in workflow's own ownership chain). A trigger watching
// kind: WorkflowRun with no filter would otherwise fire on every run it spawns,
// amplifying without bound — the same self-amplification bug class, but for triggers
// that watch WorkflowRun directly instead of the Pods/Jobs a run creates.
func isOwnWorkflowRun(eventObject *unstructured.Unstructured, workflow *ottoflowv1alpha1.Workflow) bool {
	gvk := eventObject.GetObjectKind().GroupVersionKind()
	if gvk.GroupKind() != workflowRunGroupKind {
		return false
	}
	for _, ref := range eventObject.GetOwnerReferences() {
		if ref.Kind == "Workflow" && ref.UID == workflow.UID {
			return true
		}
	}
	return false
}

// knownRevisionPaths are probed in order to auto-detect the dedup key for common GitOps controllers.
// First non-empty field value wins. Covers ArgoCD, FluxCD Kustomization/HelmRelease, and FluxCD source controllers.
var knownRevisionPaths = [][]string{
	{"status", "sync", "revision"},      // ArgoCD Application
	{"status", "lastAppliedRevision"},   // FluxCD Kustomization
	{"status", "lastAttemptedRevision"}, // FluxCD HelmRelease
	{"status", "artifact", "revision"},  // FluxCD OCIRepository / GitRepository
}

// TriggerManager manages workflow triggers (cron and event).
// Cron triggers are delegated to the Scheduler; event triggers use dynamic
// watches managed directly here.
type TriggerManager struct {
	client           client.Client
	dynamicClient    dynamic.Interface
	watcherFactory   ResourceWatcherFactory // optional; when set, used instead of dynamicClient for Watch
	scheme           *runtime.Scheme
	scheduler        *Scheduler
	eventWatchers    map[string]watch.Interface
	stopCancels      map[string]context.CancelFunc
	eventWorkflowMap map[string]*ottoflowv1alpha1.Workflow
	eventSpecMap     map[string]*ottoflowv1alpha1.EventTrigger
	// triggerCELEnv is a minimal CEL environment exposing only `object` for trigger-level
	// expressions (celFilter, inputMapping, dedupKey). Independent of the executor's full
	// Kyverno stack — trigger expressions only need field access on the event object.
	triggerCELEnv      *celgo.Env
	triggerCELPrograms sync.Map // map[string]celgo.Program — compiled expression cache
	// dedupMu guards dedupState against concurrent writes from parallel watch goroutines.
	// Outer key is triggerKey; inner key is objectNS+"/"+objectName.
	// The nested structure lets unregisterEventTrigger prune all entries for a trigger in O(1).
	dedupMu    sync.Mutex
	dedupState map[string]map[string]dedupEntry
}

// NewTriggerManager creates a new trigger manager without dynamic client
// support. Event triggers will not work; use NewTriggerManagerWithConfig
// for full functionality.
func NewTriggerManager(client client.Client, scheme *runtime.Scheme, scheduler *Scheduler) *TriggerManager {
	celEnv, err := newTriggerCELEnv()
	if err != nil {
		// CEL env creation only fails on invalid env options — this is a programming error.
		panic(fmt.Sprintf("failed to create trigger CEL environment: %v", err))
	}
	return &TriggerManager{
		client:           client,
		dynamicClient:    nil,
		watcherFactory:   nil,
		scheme:           scheme,
		scheduler:        scheduler,
		eventWatchers:    make(map[string]watch.Interface),
		stopCancels:      make(map[string]context.CancelFunc),
		eventWorkflowMap: make(map[string]*ottoflowv1alpha1.Workflow),
		eventSpecMap:     make(map[string]*ottoflowv1alpha1.EventTrigger),
		triggerCELEnv:    celEnv,
		dedupState:       make(map[string]map[string]dedupEntry),
	}
}

// NewTriggerManagerWithWatcherFactory creates a trigger manager that uses the
// given ResourceWatcherFactory for event watches (e.g. in tests). When factory
// is non-nil, event triggers use it instead of the dynamic client.
func NewTriggerManagerWithWatcherFactory(client client.Client, scheme *runtime.Scheme, scheduler *Scheduler, factory ResourceWatcherFactory) *TriggerManager {
	tm := NewTriggerManager(client, scheme, scheduler)
	tm.watcherFactory = factory
	return tm
}

// NewTriggerManagerWithConfig creates a new trigger manager with explicit REST
// config for the dynamic client (required for event triggers).
func NewTriggerManagerWithConfig(client client.Client, scheme *runtime.Scheme, config *rest.Config, scheduler *Scheduler) (*TriggerManager, error) {
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create dynamic client: %w", err)
	}

	celEnv, err := newTriggerCELEnv()
	if err != nil {
		return nil, fmt.Errorf("failed to create trigger CEL environment: %w", err)
	}

	return &TriggerManager{
		client:           client,
		dynamicClient:    dynamicClient,
		watcherFactory:   nil,
		scheme:           scheme,
		scheduler:        scheduler,
		eventWatchers:    make(map[string]watch.Interface),
		stopCancels:      make(map[string]context.CancelFunc),
		eventWorkflowMap: make(map[string]*ottoflowv1alpha1.Workflow),
		eventSpecMap:     make(map[string]*ottoflowv1alpha1.EventTrigger),
		triggerCELEnv:    celEnv,
		dedupState:       make(map[string]map[string]dedupEntry),
	}, nil
}

// RegisterWorkflow registers triggers for a workflow
func (tm *TriggerManager) RegisterWorkflow(ctx context.Context, workflow *ottoflowv1alpha1.Workflow) error {
	logger := log.FromContext(ctx)
	workflowKey := client.ObjectKeyFromObject(workflow).String()

	tm.pruneTriggers(workflowKey, workflow.Spec.Triggers)

	for i, trigger := range workflow.Spec.Triggers {
		if trigger.Cron != nil && tm.scheduler != nil {
			cronKey := fmt.Sprintf("%s-cron-%d", workflowKey, i)
			if err := tm.scheduler.AddSchedule(cronKey, workflow, trigger.Cron); err != nil {
				logger.Error(err, "failed to register cron trigger", logging.KeyWorkflow, workflow.Name, logging.KeyNamespace, workflow.Namespace, "trigger", i)
				return err
			}
		}

		if trigger.Event != nil {
			eventKey := fmt.Sprintf("%s-event-%d", workflowKey, i)
			if err := tm.registerEventTrigger(ctx, eventKey, workflow, trigger.Event); err != nil {
				logger.Error(err, "failed to register event trigger", logging.KeyWorkflow, workflow.Name, logging.KeyNamespace, workflow.Namespace, "trigger", i)
				return err
			}
		}
	}

	return nil
}

// UnregisterWorkflow removes triggers for a workflow
func (tm *TriggerManager) UnregisterWorkflow(ctx context.Context, workflow *ottoflowv1alpha1.Workflow) error {
	workflowKey := client.ObjectKeyFromObject(workflow).String()

	tm.pruneTriggers(workflowKey, nil)

	return nil
}

func isDigits(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}

// pruneTriggers unregisters every cron and event trigger of workflowKey that is
// not a trigger of the same kind in triggers (nil prunes all).
func (tm *TriggerManager) pruneTriggers(workflowKey string, triggers []ottoflowv1alpha1.Trigger) {
	for key := range tm.eventSpecMap {
		if staleTriggerKey(key, workflowKey+"-event-", triggers, func(t ottoflowv1alpha1.Trigger) bool { return t.Event != nil }) {
			tm.unregisterEventTrigger(key)
		}
	}
	if tm.scheduler != nil {
		for _, key := range tm.scheduler.scheduleKeys() {
			if staleTriggerKey(key, workflowKey+"-cron-", triggers, func(t ottoflowv1alpha1.Trigger) bool { return t.Cron != nil }) {
				tm.scheduler.RemoveSchedule(key)
			}
		}
	}
}

// staleTriggerKey reports whether key is prefix+N and triggers[N] is missing or fails want.
func staleTriggerKey(key, prefix string, triggers []ottoflowv1alpha1.Trigger, want func(ottoflowv1alpha1.Trigger) bool) bool {
	idx, ok := strings.CutPrefix(key, prefix)
	if !ok || !isDigits(idx) {
		return false
	}
	i, err := strconv.Atoi(idx)
	return err != nil || i >= len(triggers) || !want(triggers[i])
}

// stopWatchersForKey stops and removes all watchers whose keys are key + "-resource-" + digits. Used by both registerEventTrigger
// (to clean up before re-registering) and unregisterEventTrigger.
func (tm *TriggerManager) stopWatchersForKey(key string) {
	keysToRemove := []string{}
	prefix := key + "-resource-"
	for k := range tm.eventWatchers {
		if idx, ok := strings.CutPrefix(k, prefix); ok && isDigits(idx) {
			keysToRemove = append(keysToRemove, k)
		}
	}
	for _, k := range keysToRemove {
		if cancel, exists := tm.stopCancels[k]; exists {
			cancel()
			delete(tm.stopCancels, k)
		}
		if watcher, exists := tm.eventWatchers[k]; exists {
			watcher.Stop()
			delete(tm.eventWatchers, k)
		}
	}
}

// registerEventTrigger sets up a watch for Kubernetes events
func (tm *TriggerManager) registerEventTrigger(ctx context.Context, key string, workflow *ottoflowv1alpha1.Workflow, eventSpec *ottoflowv1alpha1.EventTrigger) error {
	logger := log.FromContext(ctx)

	// Stop any existing watchers for this key before re-registering.
	// Use prefix matching — watchers are stored under resource-specific keys
	// (key + "-resource-N"), not the bare event key.
	tm.stopWatchersForKey(key)

	// Store workflow and event spec for this trigger
	tm.eventWorkflowMap[key] = workflow
	tm.eventSpecMap[key] = eventSpec

	// Set up watchers for each resource type.
	// Pass key (the event key) separately so watchEvents can use it as the dedup scope —
	// dedup state is keyed by event key, not the resource-specific key.
	for i, resource := range eventSpec.Resources {
		resourceKey := fmt.Sprintf("%s-resource-%d", key, i)
		if err := tm.watchResource(ctx, resourceKey, key, workflow, resource, eventSpec); err != nil {
			logger.Error(err, "failed to watch resource", logging.KeyWorkflow, workflow.Name, logging.KeyNamespace, workflow.Namespace, "resource", resource)
			return err
		}
	}

	return nil
}

// watchResource sets up a watch for a specific resource type.
// key is the resource-specific key (used for watcher storage); eventKey is the
// bare event key used as the dedup scope in CreateWorkflowRunFromEvent.
func (tm *TriggerManager) watchResource(ctx context.Context, key string, eventKey string, workflow *ottoflowv1alpha1.Workflow, resource ottoflowv1alpha1.EventResource, eventSpec *ottoflowv1alpha1.EventTrigger) error {
	logger := log.FromContext(ctx)

	// Parse API version and kind
	gv, err := schema.ParseGroupVersion(resource.APIVersion)
	if err != nil {
		return fmt.Errorf("invalid API version %s: %w", resource.APIVersion, err)
	}

	gvk := schema.GroupVersionKind{
		Group:   gv.Group,
		Version: gv.Version,
		Kind:    resource.Kind,
	}

	// Create unstructured object for watching
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)

	// Build list options
	listOptions := &client.ListOptions{}
	if resource.Namespace != "" {
		listOptions.Namespace = resource.Namespace
	}

	// Add label selector if specified
	if eventSpec.LabelSelector != nil {
		selector, err := metav1.LabelSelectorAsSelector(eventSpec.LabelSelector)
		if err != nil {
			return fmt.Errorf("invalid label selector: %w", err)
		}
		listOptions.LabelSelector = selector
	}

	// Add field selector if specified
	if eventSpec.FieldSelector != "" {
		// Parse field selector
		fieldSelector, err := fields.ParseSelector(eventSpec.FieldSelector)
		if err != nil {
			return fmt.Errorf("invalid field selector: %w", err)
		}
		listOptions.FieldSelector = fieldSelector
	}

	// Resolve the plural resource name via the REST mapper (authoritative, handles CRDs correctly).
	// Fall back to lowercase Kind + "s" only if the mapper is unavailable or returns an error.
	var gvr schema.GroupVersionResource
	if mapping, err := tm.client.RESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version); err == nil {
		gvr = mapping.Resource
	} else {
		logger.V(1).Info("REST mapper lookup failed — falling back to naive pluralization",
			"gvk", gvk, "err", err)
		gvr = schema.GroupVersionResource{
			Group:    gvk.Group,
			Version:  gvk.Version,
			Resource: strings.ToLower(gvk.Kind) + "s",
		}
	}

	// Build options for watch
	opts := metav1.ListOptions{}
	if eventSpec.LabelSelector != nil {
		selector, err := metav1.LabelSelectorAsSelector(eventSpec.LabelSelector)
		if err != nil {
			return fmt.Errorf("invalid label selector: %w", err)
		}
		opts.LabelSelector = selector.String()
	}
	if eventSpec.FieldSelector != "" {
		opts.FieldSelector = eventSpec.FieldSelector
	}

	// Server-side exclusion of OttoFlow-managed runner Jobs/Pods so the watch never
	// receives them; CreateWorkflowRunFromEvent keeps a matching guard as defense in depth.
	// (Two layers, one concept: "exclude OttoFlow's own managed objects" — a future
	// self-amplification vector, e.g. checkpoint ConfigMaps, plugs in here.)
	if opts.LabelSelector == "" {
		opts.LabelSelector = "!" + runnerManagedLabel
	} else {
		opts.LabelSelector += ",!" + runnerManagedLabel
	}

	// A trigger watching kind: WorkflowRun directly isn't covered by the exclusion
	// above — WorkflowRuns carry "ottoflow.nirmata.io/workflow", not runnerManagedLabel
	// (see buildWorkflowRun) — so add a coarse server-side exclusion of this
	// Workflow's own runs here too. isOwnWorkflowRun in CreateWorkflowRunFromEvent
	// remains the precise arbiter (defense in depth): a name-keyed label wouldn't
	// survive the Workflow being deleted and recreated under the same name.
	if gvk.GroupKind() == workflowRunGroupKind {
		opts.LabelSelector += ",ottoflow.nirmata.io/workflow!=" + workflow.Name
	}

	wctx, cancel := context.WithCancel(ctx)
	openWatch := func() (watch.Interface, error) {
		if tm.watcherFactory != nil {
			return tm.watcherFactory.Watch(wctx, gvr, resource.Namespace, opts)
		}
		if tm.dynamicClient == nil {
			return nil, fmt.Errorf("dynamic client not initialized")
		}
		if resource.Namespace != "" {
			return tm.dynamicClient.Resource(gvr).Namespace(resource.Namespace).Watch(wctx, opts)
		}
		return tm.dynamicClient.Resource(gvr).Watch(wctx, opts)
	}
	watcher, err := openWatch()
	if err != nil {
		cancel()
		return fmt.Errorf("failed to create watcher: %w", err)
	}

	// Store watcher
	tm.eventWatchers[key] = watcher

	tm.stopCancels[key] = cancel

	// Start watching in goroutine
	go tm.watchEvents(ctx, eventKey, workflow, eventSpec, watcher, wctx.Done(), openWatch)

	logger.Info("Registered event trigger", "resource", gvk, logging.KeyWorkflow, workflow.Name, logging.KeyNamespace, workflow.Namespace, "key", key)

	return nil
}

// watchEvents watches for events and creates WorkflowRuns.
// eventKey is the bare event key used as the dedup scope — scoped to the trigger,
// not to a single resource watcher, so unregisterEventTrigger can clean it up.
func (tm *TriggerManager) watchEvents(ctx context.Context, eventKey string, workflow *ottoflowv1alpha1.Workflow, eventSpec *ottoflowv1alpha1.EventTrigger, watcher watch.Interface, stopChan <-chan struct{}, openWatch func() (watch.Interface, error)) {
	logger := log.FromContext(ctx)
	defer func() { watcher.Stop() }()

	for {
		select {
		case <-ctx.Done():
			return
		case <-stopChan:
			return
		case event, ok := <-watcher.ResultChan():
			if !ok {
				w, reopened := tm.reopenWatch(ctx, stopChan, openWatch, workflow)
				if !reopened {
					return
				}
				watcher.Stop()
				watcher = w
				continue
			}

			// Dynamic watches always return *unstructured.Unstructured.
			unstruct, ok := event.Object.(*unstructured.Unstructured)
			if !ok {
				logger.Info("watch event object is not Unstructured — skipping", "type", fmt.Sprintf("%T", event.Object))
				continue
			}

			// Create WorkflowRun from event — use eventKey (not the resource-specific key)
			// so dedup state is scoped to the trigger and cleaned up by unregisterEventTrigger.
			if err := tm.CreateWorkflowRunFromEvent(ctx, eventKey, workflow, eventSpec, unstruct, event.Type); err != nil {
				logger.Error(err, "failed to create WorkflowRun from event", logging.KeyWorkflow, workflow.Name, logging.KeyNamespace, workflow.Namespace, "event", event.Type, "object", client.ObjectKeyFromObject(unstruct))
			}
		}
	}
}

// reopenWatch re-creates a watch whose result channel closed, retrying with
// exponential backoff until ctx or stopChan is done (reopened=false).
func (tm *TriggerManager) reopenWatch(ctx context.Context, stopChan <-chan struct{}, openWatch func() (watch.Interface, error), workflow *ottoflowv1alpha1.Workflow) (watch.Interface, bool) {
	delay := watchReopenDelay
	for {
		select {
		case <-ctx.Done():
			return nil, false
		case <-stopChan:
			return nil, false
		case <-time.After(delay):
		}
		w, err := openWatch()
		if err == nil {
			return w, true
		}
		log.FromContext(ctx).Error(err, "failed to re-open closed watch", logging.KeyWorkflow, workflow.Name, logging.KeyNamespace, workflow.Namespace)
		delay = min(delay*2, watchReopenMaxDelay)
	}
}

// unregisterEventTrigger removes an event trigger
func (tm *TriggerManager) unregisterEventTrigger(key string) {
	// Stop all watchers and channels for this trigger key (including resource-specific keys)
	tm.stopWatchersForKey(key)

	delete(tm.eventWorkflowMap, key)
	delete(tm.eventSpecMap, key)

	tm.dedupMu.Lock()
	delete(tm.dedupState, key)
	tm.dedupMu.Unlock()
}

// buildWorkflowRun constructs a *WorkflowRun with the given name, inputs, and trigger info.
// It does NOT call Create — callers are responsible for Create + status retry loop.
// Labels contain only the workflow-name label; callers add trigger-type and managed-by labels.
func buildWorkflowRun(
	workflow *ottoflowv1alpha1.Workflow,
	name string,
	inputs map[string]string,
	triggerInfo ottoflowv1alpha1.TriggerInfo,
) *ottoflowv1alpha1.WorkflowRun {
	return &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: workflow.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: ottoflowv1alpha1.GroupVersion.String(),
					Kind:       "Workflow",
					Name:       workflow.Name,
					UID:        workflow.UID,
					Controller: &[]bool{true}[0],
				},
			},
			Labels: map[string]string{
				"ottoflow.nirmata.io/workflow": workflow.Name,
			},
		},
		Spec: ottoflowv1alpha1.WorkflowRunSpec{
			WorkflowRef: ottoflowv1alpha1.WorkflowRef{
				Name:      workflow.Name,
				Namespace: workflow.Namespace,
			},
			InputValues: inputs,
			Execution:   workflow.Spec.Execution.DeepCopy(),
		},
		Status: ottoflowv1alpha1.WorkflowRunStatus{
			Phase:   ottoflowv1alpha1.WorkflowRunPhasePending,
			Trigger: &triggerInfo,
		},
	}
}

// CreateWorkflowRunFromEvent creates a WorkflowRun from an event trigger.
// triggerKey uniquely identifies the (workflow, trigger index) pair and scopes dedup state.
func (tm *TriggerManager) CreateWorkflowRunFromEvent(ctx context.Context, triggerKey string, workflow *ottoflowv1alpha1.Workflow, eventSpec *ottoflowv1alpha1.EventTrigger, eventObject *unstructured.Unstructured, eventType watch.EventType) error {
	logger := log.FromContext(ctx)
	objectData := eventObject.Object

	// Never trigger on OttoFlow's own runner Jobs/Pods: a trigger watching the same
	// resource kinds its runs create would otherwise feed on itself (run → Job → Pod
	// → pod events → more runs), unbounded.
	if _, managed := eventObject.GetLabels()[runnerManagedLabel]; managed {
		return nil
	}

	// Never trigger on a WorkflowRun this same Workflow created: a trigger watching
	// kind: WorkflowRun directly isn't covered by the runner-label guard above (the
	// label only marks Pods/Jobs), and would otherwise feed on its own runs the same
	// unbounded way.
	if isOwnWorkflowRun(eventObject, workflow) {
		return nil
	}

	// Check if operation matches
	if len(eventSpec.Operations) > 0 {
		operationMatches := false
		for _, op := range eventSpec.Operations {
			if (op == "CREATE" && eventType == watch.Added) ||
				(op == "UPDATE" && eventType == watch.Modified) ||
				(op == "DELETE" && eventType == watch.Deleted) {
				operationMatches = true
				break
			}
		}
		if !operationMatches {
			return nil
		}
	}

	// Apply celFilter: drop the event if the expression returns false or errors.
	if eventSpec.CELFilter != "" {
		result, err := tm.evaluateTriggerCEL(eventSpec.CELFilter, objectData)
		if err != nil {
			logger.V(1).Info("celFilter eval error — dropping event",
				"filter", eventSpec.CELFilter, "err", err,
				logging.KeyWorkflow, workflow.Name)
			return nil
		}
		if matched, ok := result.(bool); !ok || !matched {
			return nil
		}
	}

	// Evaluate inputMapping: replace the stub with real CEL evaluation.
	inputValues := make(map[string]string)
	for inputName, celExpr := range eventSpec.InputMapping {
		result, err := tm.evaluateTriggerCEL(celExpr, objectData)
		if err != nil {
			logger.V(1).Info("inputMapping CEL eval failed — using empty string",
				"input", inputName, "expr", celExpr, "err", err,
				logging.KeyWorkflow, workflow.Name)
			inputValues[inputName] = ""
			continue
		}
		inputValues[inputName] = fmt.Sprintf("%v", result)
	}

	// Deduplication: prevent duplicate WorkflowRuns from rapid-fire watch events.
	// Priority: explicit dedupKey CEL → auto-detected revision field → time window fallback.
	var currentDedupKey string
	if eventSpec.DedupKey != "" {
		if r, err := tm.evaluateTriggerCEL(eventSpec.DedupKey, objectData); err == nil {
			currentDedupKey = fmt.Sprintf("%v", r)
		}
	}
	if currentDedupKey == "" {
		currentDedupKey = autoDetectDedupKey(objectData)
	}

	// Key dedup state by UID when available: a resource deleted and recreated with
	// the same namespace/name gets a new UID, so the new instance isn't mistaken
	// for the old one still inside the dedup window. Fall back to namespace/name
	// for objects with no UID (shouldn't happen for real watch events, but keeps
	// this defensive).
	objKey := string(eventObject.GetUID())
	if objKey == "" {
		objKey = eventObject.GetNamespace() + "/" + eventObject.GetName()
	}

	tm.dedupMu.Lock()
	entry := tm.dedupState[triggerKey][objKey]
	if currentDedupKey != "" {
		if entry.key == currentDedupKey {
			tm.dedupMu.Unlock()
			return nil // same revision already triggered a run — burst noise
		}
	} else {
		// No dedup key resolved (e.g. Pods have no revision field): fall back to a
		// per-object time window, defaulting to defaultDedupWindow unless the trigger
		// overrides it. This suppresses rapid-fire duplicate events for an object
		// already seen (e.g. a flapping Pod) — see defaultDedupWindow's comment for
		// why this same mechanism does nothing to gate a self-amplifying loop, where
		// every triggering object is new by construction and so never matches here.
		window := defaultDedupWindow
		if eventSpec.DedupWindow != nil {
			window = eventSpec.DedupWindow.Duration
		}
		if time.Since(entry.createdAt) < window {
			tm.dedupMu.Unlock()
			return nil // within dedup window
		}
	}
	tm.dedupMu.Unlock()

	// Enforce MaxConcurrentRuns: skip creating if active runs already at limit
	if workflow.Spec.Run != nil && workflow.Spec.Run.MaxConcurrentRuns != nil && *workflow.Spec.Run.MaxConcurrentRuns > 0 {
		active, err := countActiveWorkflowRuns(ctx, tm.client, workflow)
		if err != nil {
			return fmt.Errorf("count active WorkflowRuns: %w", err)
		}
		if active >= int(*workflow.Spec.Run.MaxConcurrentRuns) {
			return nil // Skip creating; event is dropped
		}
	}

	// Generate workflow run name: uid4 identifies the source object; time8 ensures
	// each trigger fire gets a unique name even if prior runs haven't been cleaned up.
	var uid4 string
	if uid := eventObject.GetUID(); uid != "" {
		uidStr := string(uid)
		if len(uidStr) >= 4 {
			uid4 = uidStr[:4]
		} else {
			uid4 = uidStr
		}
	} else {
		hash := sha256.Sum256([]byte(eventObject.GetName()))
		uid4 = fmt.Sprintf("%x", hash[:2])
	}
	time8 := fmt.Sprintf("%08x", time.Now().UnixNano()&0xFFFFFFFF)
	workflowRunName := fmt.Sprintf("%s-%s-%s", workflow.Name, uid4, time8)

	workflowRun := buildWorkflowRun(workflow, workflowRunName, inputValues, ottoflowv1alpha1.TriggerInfo{
		Type:        "Event",
		TriggeredAt: metav1.Now(),
		EventResource: &ottoflowv1alpha1.EventResourceInfo{
			APIVersion: eventObject.GetObjectKind().GroupVersionKind().GroupVersion().String(),
			Kind:       getObjectKind(eventObject),
			Name:       eventObject.GetName(),
			Namespace:  eventObject.GetNamespace(),
		},
	})
	workflowRun.Labels["ottoflow.nirmata.io/trigger"] = "event"

	statusToSet := workflowRun.Status.DeepCopy()

	if err := tm.client.Create(ctx, workflowRun); err != nil {
		return err
	}

	// Record dedup state only after a successful create so a failed create doesn't block the next event.
	tm.dedupMu.Lock()
	if tm.dedupState[triggerKey] == nil {
		tm.dedupState[triggerKey] = make(map[string]dedupEntry)
	}
	tm.dedupState[triggerKey][objKey] = dedupEntry{key: currentDedupKey, createdAt: time.Now()}
	tm.dedupMu.Unlock()

	// The cached client may not see the object immediately after Create.
	// Retry the Get+Status.Update with a short backoff.
	wrKey := client.ObjectKeyFromObject(workflowRun)
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			time.Sleep(200 * time.Millisecond)
		}
		if err := tm.client.Get(ctx, wrKey, workflowRun); err != nil {
			lastErr = err
			continue
		}
		workflowRun.Status = *statusToSet
		if err := tm.client.Status().Update(ctx, workflowRun); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	return fmt.Errorf("failed to set WorkflowRun status after create: %w", lastErr)
}

// newTriggerCELEnv creates a minimal CEL environment for trigger-level expressions.
// Only `object` is declared — trigger expressions evaluate field access on the event object.
func newTriggerCELEnv() (*celgo.Env, error) {
	return celgo.NewEnv(celgo.Variable("object", celgo.DynType))
}

// evaluateTriggerCEL evaluates a CEL expression with `object` bound to the event object data.
// Programs are compiled once and cached in triggerCELPrograms for the lifetime of the TriggerManager.
// Safe for concurrent calls from multiple watch goroutines.
func (tm *TriggerManager) evaluateTriggerCEL(expr string, objectData map[string]interface{}) (interface{}, error) {
	if tm.triggerCELEnv == nil {
		return nil, fmt.Errorf("trigger CEL environment not initialized")
	}

	// Load compiled program from cache, or compile and store.
	val, ok := tm.triggerCELPrograms.Load(expr)
	if !ok {
		ast, iss := tm.triggerCELEnv.Compile(expr)
		if iss != nil && iss.Err() != nil {
			return nil, fmt.Errorf("CEL compile error: %w", iss.Err())
		}
		prg, err := tm.triggerCELEnv.Program(ast)
		if err != nil {
			return nil, fmt.Errorf("CEL program error: %w", err)
		}
		// LoadOrStore handles the rare race where two goroutines compile the same expression.
		val, _ = tm.triggerCELPrograms.LoadOrStore(expr, prg)
	}

	out, _, err := val.(celgo.Program).Eval(map[string]interface{}{"object": objectData})
	if err != nil {
		return nil, fmt.Errorf("CEL eval error: %w", err)
	}
	return out.Value(), nil
}

// autoDetectDedupKey probes well-known revision fields on the event object to determine
// a deduplication key without user configuration. Returns empty string if none found.
func autoDetectDedupKey(objectData map[string]interface{}) string {
	for _, path := range knownRevisionPaths {
		if val, found, _ := unstructured.NestedString(objectData, path...); found && val != "" {
			return val
		}
	}
	return ""
}

// WebhookFilterResult distinguishes the outcomes of CreateWorkflowRunFromWebhook so
// the HTTP handler can set the correct response without inspecting (nil, nil).
type WebhookFilterResult int

const (
	WebhookRunCreated         WebhookFilterResult = iota // WorkflowRun was created
	WebhookFiltered                                      // celFilter returned false — 200, no run
	WebhookDeduped                                       // duplicate within dedupWindow — 200, no run
	WebhookConcurrencyLimited                            // MaxConcurrentRuns reached — 429
)

// ErrWorkflowRunCreateFailed is returned when the Kubernetes Create API call fails.
// The HTTP handler maps this to 500 (retriable); other errors become 400 (client fault).
var ErrWorkflowRunCreateFailed = errors.New("WorkflowRun Create failed")

// CreateWorkflowRunFromWebhook runs the four-gate pipeline (filter → inputMapping →
// dedup → MaxConcurrentRuns) and creates a WorkflowRun on success.
func (tm *TriggerManager) CreateWorkflowRunFromWebhook(
	ctx context.Context,
	workflow *ottoflowv1alpha1.Workflow,
	spec *ottoflowv1alpha1.WebhookTrigger,
	body []byte,
	meta WebhookRequestMeta,
) (*ottoflowv1alpha1.WorkflowRun, WebhookFilterResult, error) {
	// Parse body only when a CEL expression needs it.
	// Non-JSON payloads are accepted (e.g. GitHub ping) when no CEL fields are set.
	var objectData map[string]interface{}
	if spec.CELFilter != "" || len(spec.InputMapping) > 0 || spec.DedupKey != "" {
		if err := json.Unmarshal(body, &objectData); err != nil {
			return nil, 0, fmt.Errorf("invalid JSON body: %w", err)
		}
	}

	// Gate 1: CEL filter. Errors → 400 (expression bug, caller gets actionable signal).
	// Filter returning false → 200 OK, WebhookFiltered (not an error).
	if spec.CELFilter != "" {
		raw, err := tm.evaluateTriggerCEL(spec.CELFilter, objectData)
		if err != nil {
			return nil, 0, fmt.Errorf("celFilter expression error: %w", err)
		}
		if matched, ok := raw.(bool); !ok || !matched {
			return nil, WebhookFiltered, nil
		}
	}

	// Gate 2: inputMapping. CEL errors are silent-skipped with a warning — the run is
	// still created with the inputs that evaluated successfully. This is intentional:
	// a single bad expression should not block WorkflowRun creation entirely.
	logger := log.FromContext(ctx)
	inputs := make(map[string]string)
	for inputName, celExpr := range spec.InputMapping {
		val, err := tm.evaluateTriggerCEL(celExpr, objectData)
		if err != nil {
			logger.Info("inputMapping CEL error — input omitted",
				"input", inputName, "expr", celExpr, "err", err)
			continue
		}
		inputs[inputName] = fmt.Sprintf("%v", val)
	}

	// Gate 3: deduplication.
	// triggerKey scopes the dedup map to this webhook trigger.
	// objectKey is what is deduplicated: either the CEL-extracted dedupKey value, or the
	// workflow ns/name (giving "one run per dedupWindow regardless of payload content").
	triggerKey := fmt.Sprintf("%s/%s-webhook", workflow.Namespace, workflow.Name)
	objectKey := fmt.Sprintf("%s/%s", workflow.Namespace, workflow.Name)
	if spec.DedupKey != "" {
		if raw, err := tm.evaluateTriggerCEL(spec.DedupKey, objectData); err == nil {
			objectKey = fmt.Sprintf("webhook-dedup/%v", raw)
		}
	}
	// API contract: DedupWindow defaults to 10 minutes when DedupKey is set.
	var dedupWindow time.Duration
	if spec.DedupWindow != nil {
		dedupWindow = spec.DedupWindow.Duration
	} else if spec.DedupKey != "" {
		dedupWindow = defaultDedupWindow
	}
	if dedupWindow > 0 {
		tm.dedupMu.Lock()
		entries := tm.dedupState[triggerKey]
		if entries != nil {
			if entry, exists := entries[objectKey]; exists && time.Since(entry.createdAt) < dedupWindow {
				tm.dedupMu.Unlock()
				return nil, WebhookDeduped, nil
			}
		}
		tm.dedupMu.Unlock()
	}

	// Gate 4: MaxConcurrentRuns.
	if workflow.Spec.Run != nil && workflow.Spec.Run.MaxConcurrentRuns != nil && *workflow.Spec.Run.MaxConcurrentRuns > 0 {
		active, err := countActiveWorkflowRuns(ctx, tm.client, workflow)
		if err != nil {
			return nil, 0, fmt.Errorf("count active WorkflowRuns: %w", err)
		}
		if active >= int(*workflow.Spec.Run.MaxConcurrentRuns) {
			return nil, WebhookConcurrencyLimited, nil
		}
	}

	// Gate 5: build and create WorkflowRun.
	// Name format: {workflowName}-{rand4}-{time8hex}
	// rand4 uses crypto/rand (no triggering K8s object to derive uid4 from).
	// time8hex is nanosecond-masked — same pattern as CreateWorkflowRunFromEvent.
	rb := make([]byte, 2)
	_, _ = rand.Read(rb)
	rand4 := hex.EncodeToString(rb)
	time8 := fmt.Sprintf("%08x", time.Now().UnixNano()&0xFFFFFFFF)
	runName := fmt.Sprintf("%s-%s-%s", workflow.Name, rand4, time8)

	run := buildWorkflowRun(workflow, runName, inputs, ottoflowv1alpha1.TriggerInfo{
		Type:        "Webhook",
		TriggeredAt: metav1.Now(),
		WebhookRequest: &ottoflowv1alpha1.WebhookRequestInfo{
			RemoteAddr: meta.RemoteAddr,
			RequestID:  meta.RequestID,
		},
	})
	run.Labels["ottoflow.nirmata.io/trigger"] = "webhook"
	run.Labels["ottoflow.nirmata.io/managed-by"] = "ottoflow-webhook-server"

	statusToSet := run.Status.DeepCopy()
	if err := tm.client.Create(ctx, run); err != nil {
		return nil, 0, fmt.Errorf("%w: %v", ErrWorkflowRunCreateFailed, err)
	}

	// Write dedup state AFTER successful Create so a failed Create does not block the next request.
	if dedupWindow > 0 {
		tm.dedupMu.Lock()
		if tm.dedupState[triggerKey] == nil {
			tm.dedupState[triggerKey] = make(map[string]dedupEntry)
		}
		tm.dedupState[triggerKey][objectKey] = dedupEntry{createdAt: time.Now()}
		tm.dedupMu.Unlock()
	}

	// Status update retry loop — mirrors CreateWorkflowRunFromEvent.
	wrKey := types.NamespacedName{Namespace: run.Namespace, Name: run.Name}
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			time.Sleep(200 * time.Millisecond)
		}
		if err := tm.client.Get(ctx, wrKey, run); err != nil {
			if apierrors.IsNotFound(err) {
				break // deleted between Create and Get — stop retrying
			}
			continue
		}
		run.Status = *statusToSet
		if err := tm.client.Status().Update(ctx, run); err == nil {
			break
		}
	}

	return run, WebhookRunCreated, nil
}

// CleanupWebhookDedup removes the dedup state for a deleted Workflow.
// Mirrors unregisterEventTrigger which prunes dedupState for event triggers.
// Called from WorkflowReconciler.Reconcile on Workflow deletion.
func (tm *TriggerManager) CleanupWebhookDedup(workflowKey string) {
	triggerKey := workflowKey + "-webhook"
	tm.dedupMu.Lock()
	defer tm.dedupMu.Unlock()
	delete(tm.dedupState, triggerKey)
}
