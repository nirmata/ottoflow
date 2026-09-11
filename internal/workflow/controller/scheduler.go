/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/robfig/cron/v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/logging"
)

// Scheduler manages cron-based workflow triggers using an in-process cron
// scheduler. It implements manager.Runnable and manager.LeaderElectionRunnable
// so it is started by the controller manager only on the elected leader.
type Scheduler struct {
	client    client.Client
	logger    logr.Logger
	cron      *cron.Cron
	mu        sync.Mutex
	ctx       context.Context
	entries   map[string]cron.EntryID
	workflows map[string]*ottoflowv1alpha1.Workflow
	cronSpecs map[string]*ottoflowv1alpha1.CronTrigger
}

// NewScheduler creates a Scheduler. The cron engine is created immediately
// (so AddSchedule may be called before Start) but entries only fire once
// Start is called.
func NewScheduler(c client.Client, logger logr.Logger) *Scheduler {
	return &Scheduler{
		client:    c,
		logger:    logger.WithName("scheduler"),
		cron:      cron.New(),
		entries:   make(map[string]cron.EntryID),
		workflows: make(map[string]*ottoflowv1alpha1.Workflow),
		cronSpecs: make(map[string]*ottoflowv1alpha1.CronTrigger),
	}
}

// NeedLeaderElection implements manager.LeaderElectionRunnable.
// The scheduler must only run on the leader to avoid duplicate fires.
func (s *Scheduler) NeedLeaderElection() bool {
	return true
}

// Start implements manager.Runnable. It starts the cron engine and blocks
// until ctx is cancelled (leadership lost or shutdown).
func (s *Scheduler) Start(ctx context.Context) error {
	s.mu.Lock()
	s.ctx = ctx
	s.mu.Unlock()

	s.logger.Info("Starting cron scheduler")
	s.cron.Start()

	<-ctx.Done()

	s.logger.Info("Stopping cron scheduler")
	stopCtx := s.cron.Stop()
	<-stopCtx.Done()
	return nil
}

// AddSchedule registers (or replaces) a cron schedule for the given key.
// When the schedule fires, a WorkflowRun is created for the workflow.
func (s *Scheduler) AddSchedule(key string, workflow *ottoflowv1alpha1.Workflow, cronSpec *ottoflowv1alpha1.CronTrigger) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if entryID, exists := s.entries[key]; exists {
		s.cron.Remove(entryID)
		delete(s.entries, key)
	}

	schedule := cronSpec.Schedule
	if cronSpec.Timezone != "" {
		schedule = "CRON_TZ=" + cronSpec.Timezone + " " + schedule
	}

	entryID, err := s.cron.AddFunc(schedule, func() {
		s.handleCronFire(key)
	})
	if err != nil {
		return fmt.Errorf("invalid cron schedule %q: %w", schedule, err)
	}

	s.entries[key] = entryID
	s.workflows[key] = workflow
	s.cronSpecs[key] = cronSpec

	s.logger.Info("Added cron schedule", "key", key, "schedule", schedule, logging.KeyWorkflow, workflow.Name, logging.KeyNamespace, workflow.Namespace)
	return nil
}

// RemoveSchedule removes a previously registered cron schedule.
func (s *Scheduler) RemoveSchedule(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if entryID, exists := s.entries[key]; exists {
		s.cron.Remove(entryID)
		delete(s.entries, key)
		delete(s.workflows, key)
		delete(s.cronSpecs, key)
		s.logger.Info("Removed cron schedule", "key", key)
	}
}

// HasSchedule returns true if a schedule is registered for the given key.
func (s *Scheduler) HasSchedule(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, exists := s.entries[key]
	return exists
}

// handleCronFire is invoked by the cron engine when a schedule fires.
func (s *Scheduler) handleCronFire(key string) {
	s.mu.Lock()
	workflow := s.workflows[key]
	cronSpec := s.cronSpecs[key]
	ctx := s.ctx
	s.mu.Unlock()

	if workflow == nil || cronSpec == nil || ctx == nil {
		return
	}

	logger := s.logger.WithValues("key", key, logging.KeyWorkflow, workflow.Name, logging.KeyNamespace, workflow.Namespace)

	switch cronSpec.ConcurrencyPolicy {
	case "Forbid":
		hasActive, err := s.hasActiveWorkflowRun(ctx, workflow)
		if err != nil {
			logger.Error(err, "Failed to check for active WorkflowRuns")
			return
		}
		if hasActive {
			logger.V(1).Info("Skipping cron fire: active WorkflowRun exists (Forbid)")
			return
		}
	case "Replace":
		if err := s.cancelActiveWorkflowRuns(ctx, workflow, logger); err != nil {
			logger.Error(err, "Failed to cancel active WorkflowRuns for Replace policy")
			return
		}
	}

	// Enforce MaxConcurrentRuns: skip creating if active runs already at limit
	if workflow.Spec.Run != nil && workflow.Spec.Run.MaxConcurrentRuns != nil && *workflow.Spec.Run.MaxConcurrentRuns > 0 {
		active, err := countActiveWorkflowRuns(ctx, s.client, workflow)
		if err != nil {
			logger.Error(err, "Failed to count active WorkflowRuns")
			return
		}
		if active >= int(*workflow.Spec.Run.MaxConcurrentRuns) {
			logger.V(1).Info("Skipping cron fire: max concurrent runs reached", "active", active, "max", *workflow.Spec.Run.MaxConcurrentRuns)
			return
		}
	}

	if err := s.createWorkflowRun(ctx, workflow, cronSpec); err != nil {
		logger.Error(err, "Failed to create WorkflowRun from cron trigger")
		return
	}
	logger.Info("Created WorkflowRun from cron trigger")
}

// countActiveWorkflowRuns returns the number of WorkflowRuns (Pending or Running) for the workflow.
// Used to enforce spec.run.maxConcurrentRuns in scheduler and trigger manager.
func countActiveWorkflowRuns(ctx context.Context, c client.Client, workflow *ottoflowv1alpha1.Workflow) (int, error) {
	var list ottoflowv1alpha1.WorkflowRunList
	if err := c.List(ctx, &list,
		client.InNamespace(workflow.Namespace),
		client.MatchingLabels{"ottoflow.nirmata.io/workflow": workflow.Name},
	); err != nil {
		return 0, err
	}
	count := 0
	for _, wr := range list.Items {
		if wr.Status.Phase == ottoflowv1alpha1.WorkflowRunPhasePending ||
			wr.Status.Phase == ottoflowv1alpha1.WorkflowRunPhaseRunning {
			count++
		}
	}
	return count, nil
}

func (s *Scheduler) hasActiveWorkflowRun(ctx context.Context, workflow *ottoflowv1alpha1.Workflow) (bool, error) {
	var list ottoflowv1alpha1.WorkflowRunList
	if err := s.client.List(ctx, &list,
		client.InNamespace(workflow.Namespace),
		client.MatchingLabels{
			"ottoflow.nirmata.io/workflow": workflow.Name,
			"ottoflow.nirmata.io/trigger":  "cron",
		},
	); err != nil {
		return false, err
	}
	for _, wr := range list.Items {
		if wr.Status.Phase == ottoflowv1alpha1.WorkflowRunPhasePending ||
			wr.Status.Phase == ottoflowv1alpha1.WorkflowRunPhaseRunning {
			return true, nil
		}
	}
	return false, nil
}

func (s *Scheduler) cancelActiveWorkflowRuns(ctx context.Context, workflow *ottoflowv1alpha1.Workflow, logger logr.Logger) error {
	var list ottoflowv1alpha1.WorkflowRunList
	if err := s.client.List(ctx, &list,
		client.InNamespace(workflow.Namespace),
		client.MatchingLabels{
			"ottoflow.nirmata.io/workflow": workflow.Name,
			"ottoflow.nirmata.io/trigger":  "cron",
		},
	); err != nil {
		return err
	}
	for i := range list.Items {
		wr := &list.Items[i]
		if wr.Status.Phase == ottoflowv1alpha1.WorkflowRunPhasePending ||
			wr.Status.Phase == ottoflowv1alpha1.WorkflowRunPhaseRunning {
			setRunFailed(wr, "Replaced by newer cron trigger execution", "")
			if err := s.client.Status().Update(ctx, wr); err != nil {
				return fmt.Errorf("failed to cancel WorkflowRun %s: %w", wr.Name, err)
			}
			logger.Info("Cancelled active WorkflowRun for Replace policy", logging.KeyWorkflowRun, wr.Name, logging.KeyNamespace, wr.Namespace)
		}
	}
	return nil
}

// deepCopyExecution returns a deep copy of the execution spec, or nil if in is nil.
func deepCopyExecution(in *ottoflowv1alpha1.WorkflowRunExecutionSpec) *ottoflowv1alpha1.WorkflowRunExecutionSpec {
	if in == nil {
		return nil
	}
	return in.DeepCopy()
}

// cronInputValuesFromSecrets reads the given InputValuesFrom entries and returns a map of input name to value.
// Secrets are read in workflowNamespace when ref.Namespace is empty.
func (s *Scheduler) cronInputValuesFromSecrets(ctx context.Context, workflowNamespace string, from []ottoflowv1alpha1.CronInputFromSecret) (map[string]string, error) {
	out := make(map[string]string)
	for _, entry := range from {
		ns := entry.SecretRef.Namespace
		if ns == "" {
			ns = workflowNamespace
		}
		secret := &corev1.Secret{}
		key := client.ObjectKey{Namespace: ns, Name: entry.SecretRef.Name}
		if err := s.client.Get(ctx, key, secret); err != nil {
			return nil, fmt.Errorf("secret %s/%s: %w", ns, entry.SecretRef.Name, err)
		}
		data, ok := secret.Data[entry.SecretRef.Key]
		if !ok {
			return nil, fmt.Errorf("secret %s/%s: key %q not found", ns, entry.SecretRef.Name, entry.SecretRef.Key)
		}
		out[entry.InputName] = string(data)
	}
	return out, nil
}

func (s *Scheduler) createWorkflowRun(ctx context.Context, workflow *ottoflowv1alpha1.Workflow, cronSpec *ottoflowv1alpha1.CronTrigger) error {
	workflowRunName := fmt.Sprintf("%s-%d", workflow.Name, time.Now().Unix())

	inputValues := make(map[string]string)
	if len(cronSpec.InputValuesFrom) > 0 {
		fromSecret, err := s.cronInputValuesFromSecrets(ctx, workflow.Namespace, cronSpec.InputValuesFrom)
		if err != nil {
			return fmt.Errorf("cron inputValuesFrom: %w", err)
		}
		for k, v := range fromSecret {
			inputValues[k] = v
		}
	}

	workflowRun := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      workflowRunName,
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
				"ottoflow.nirmata.io/workflow":   workflow.Name,
				"ottoflow.nirmata.io/trigger":    "cron",
				"ottoflow.nirmata.io/managed-by": "ottoflow-scheduler",
			},
		},
		Spec: ottoflowv1alpha1.WorkflowRunSpec{
			WorkflowRef: ottoflowv1alpha1.WorkflowRef{
				Name:      workflow.Name,
				Namespace: workflow.Namespace,
			},
			InputValues: inputValues,
			Execution:   deepCopyExecution(workflow.Spec.Execution),
		},
		Status: ottoflowv1alpha1.WorkflowRunStatus{
			Phase: ottoflowv1alpha1.WorkflowRunPhasePending,
			Trigger: &ottoflowv1alpha1.TriggerInfo{
				Type:         "Cron",
				CronSchedule: cronSpec.Schedule,
				TriggeredAt:  metav1.Now(),
			},
		},
	}

	statusToSet := workflowRun.Status.DeepCopy()

	if err := s.client.Create(ctx, workflowRun); err != nil {
		return err
	}

	// The cached client may not see the object immediately after Create.
	// Retry the Get+Status.Update with a short backoff.
	key := client.ObjectKeyFromObject(workflowRun)
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			time.Sleep(200 * time.Millisecond)
		}
		if err := s.client.Get(ctx, key, workflowRun); err != nil {
			lastErr = err
			continue
		}
		workflowRun.Status = *statusToSet
		if err := s.client.Status().Update(ctx, workflowRun); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	return fmt.Errorf("failed to set WorkflowRun status after create: %w", lastErr)
}
