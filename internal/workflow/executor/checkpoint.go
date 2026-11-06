/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

const checkpointWriteTimeout = 30 * time.Second
const auditSnapshotWriteTimeout = 30 * time.Second

// auditSnapshotSizeThreshold caps the marshaled snapshot size with headroom under the
// ~1MiB ConfigMap limit (etcd overhead, other keys, metadata all eat into that ceiling).
const auditSnapshotSizeThreshold = 900 * 1024
const auditSnapshotMaxArrayEntries = 50

// CheckpointSnapshot is the per-step state persisted to a ConfigMap for crash recovery.
type CheckpointSnapshot struct {
	Version           int                                    `json:"version"`
	WorkflowRunUID    string                                 `json:"workflowRunUID"`
	LastCompletedStep string                                 `json:"lastCompletedStep"`
	StepStatuses      map[string]ottoflowv1alpha1.StepStatus `json:"stepStatuses"`
	Context           map[string]interface{}                 `json:"context"`
	// CompletionOrder is the exact step-completion order recorded during live execution
	// (ContextManager.CompletionOrder), persisted so ContextBudgetMode=lastN restores the same
	// lastN window after a restart. CompletionTime has only second granularity, so
	// reconstructing order from StepStatuses alone can't disambiguate same-second completions —
	// this field avoids that reconstruction entirely. Omitted (empty) in checkpoints written
	// before this field existed; loaders must fall back to StepStatuses-based reconstruction
	// (RestoreCompletionOrder) when it's empty.
	CompletionOrder []string `json:"completionOrder,omitempty"`
}

// terminalStepCount returns the number of step statuses that have reached a terminal phase
// (Succeeded, Failed, or Skipped — matching isStepDone's notion of "done", but counting Failed
// regardless of FailurePolicy since a Failed status never reverts either way). Pending, Running,
// and Waiting are excluded: Waiting is a callback-pending state that resolves to a terminal
// phase later, not a terminal phase itself.
//
// Unlike len(StepStatuses) — which ExecuteWorkflow populates for every step up front, before any
// step completes — this count only grows as steps actually finish, so it can detect that one
// snapshot represents more progress than another even when both have the same total StepStatuses
// length (see the comment in SaveAsync).
func terminalStepCount(statuses map[string]ottoflowv1alpha1.StepStatus) int {
	count := 0
	for _, s := range statuses {
		switch s.Phase {
		case ottoflowv1alpha1.StepPhaseSucceeded, ottoflowv1alpha1.StepPhaseFailed, ottoflowv1alpha1.StepPhaseSkipped:
			count++
		}
	}
	return count
}

// CheckpointManager persists workflow checkpoints to a ConfigMap via async writes.
type CheckpointManager struct {
	controlClient client.Client
	cmKey         client.ObjectKey
	auditCmKey    client.ObjectKey
	runUID        string
	ownerRefs     []metav1.OwnerReference
	pending       sync.WaitGroup
	enabled       bool
}

func NewCheckpointManager(controlClient client.Client, workflowRun *ottoflowv1alpha1.WorkflowRun) *CheckpointManager {
	enabled := workflowRun.CheckpointingEnabled()

	isController := true
	blockOwner := true
	ownerRefs := []metav1.OwnerReference{
		{
			APIVersion:         ottoflowv1alpha1.GroupVersion.String(),
			Kind:               "WorkflowRun",
			Name:               workflowRun.Name,
			UID:                workflowRun.UID,
			Controller:         &isController,
			BlockOwnerDeletion: &blockOwner,
		},
	}

	return &CheckpointManager{
		controlClient: controlClient,
		cmKey: client.ObjectKey{
			Namespace: workflowRun.Namespace,
			Name:      checkpointConfigMapName(workflowRun.Name),
		},
		auditCmKey: client.ObjectKey{
			Namespace: workflowRun.Namespace,
			Name:      auditConfigMapName(workflowRun.Name),
		},
		runUID:    string(workflowRun.UID),
		ownerRefs: ownerRefs,
		enabled:   enabled,
	}
}

// checkpointConfigMapName returns the ConfigMap name, truncated to 63 chars with a hash suffix.
func checkpointConfigMapName(runName string) string {
	return truncatedConfigMapName("ottoflow-cp-", runName)
}

// auditConfigMapName returns the ConfigMap name for the always-on completion audit
// snapshot (see SaveAuditSnapshot), truncated to 63 chars with a hash suffix. Uses a
// distinct prefix from checkpointConfigMapName so the two ConfigMaps never collide and
// so DeleteCheckpointForRun (which only ever targets the checkpoint name) cannot
// accidentally delete the audit snapshot.
func auditConfigMapName(runName string) string {
	return truncatedConfigMapName("ottoflow-audit-", runName)
}

func truncatedConfigMapName(prefix, runName string) string {
	base := prefix + strings.ToLower(runName)
	base = strings.ReplaceAll(base, "_", "-")
	if len(base) <= 63 {
		return strings.TrimSuffix(base, "-")
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(runName))
	suffix := fmt.Sprintf("%08x", h.Sum64())
	prefix = strings.TrimSuffix(base[:63-len(suffix)-1], "-")
	return prefix + "-" + suffix
}

// DeleteCheckpointForRun performs a best-effort delete of the checkpoint ConfigMap for the
// given WorkflowRun. Safe to call when no ConfigMap exists (NotFound is ignored). Called by
// the controller when a run reaches a terminal Failed state to ensure no leak after a pod crash
// prevented the executor's defer-based cleanup from running.
func DeleteCheckpointForRun(ctx context.Context, c client.Client, workflowRun *ottoflowv1alpha1.WorkflowRun) {
	if !workflowRun.CheckpointingEnabled() {
		return
	}
	cm := &corev1.ConfigMap{}
	cm.Name = checkpointConfigMapName(workflowRun.Name)
	cm.Namespace = workflowRun.Namespace
	if err := c.Delete(ctx, cm); err != nil && !apierrors.IsNotFound(err) {
		klog.ErrorS(err, "checkpoint: failed to delete ConfigMap on terminal failure", "name", cm.Name)
	}
}

func (cm *CheckpointManager) SaveAsync(ctx context.Context, snapshot CheckpointSnapshot) {
	if !cm.enabled {
		return
	}
	klog.V(4).InfoS("checkpoint: queuing async save", "lastCompletedStep", snapshot.LastCompletedStep, "steps", len(snapshot.StepStatuses))

	raw, err := json.Marshal(snapshot)
	if err != nil {
		klog.ErrorS(err, "checkpoint: failed to marshal snapshot", "workflowRunUID", cm.runUID)
		return
	}

	cm.pending.Add(1)
	go func() {
		defer cm.pending.Done()

		writeCtx, cancel := context.WithTimeout(context.Background(), checkpointWriteTimeout)
		defer cancel()

		err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			existing := &corev1.ConfigMap{}
			getErr := cm.controlClient.Get(writeCtx, cm.cmKey, existing)
			if apierrors.IsNotFound(getErr) {
				newCM := &corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Name:      cm.cmKey.Name,
						Namespace: cm.cmKey.Namespace,
						Labels: map[string]string{
							"app.kubernetes.io/part-of":    "ottoflow",
							"app.kubernetes.io/managed-by": "ottoflow-runner",
						},
						OwnerReferences: cm.ownerRefs,
					},
					Data: map[string]string{"checkpoint": string(raw)},
				}
				createErr := cm.controlClient.Create(writeCtx, newCM)
				if apierrors.IsAlreadyExists(createErr) {
					// Synthesize a conflict so RetryOnConflict re-enters the loop as a Get+Update.
					return apierrors.NewConflict(corev1.Resource("configmaps"), cm.cmKey.Name, createErr)
				}
				return createErr
			}
			if getErr != nil {
				return getErr
			}
			// Only overwrite if our snapshot is strictly more advanced for the same run.
			// Prevents older goroutines from downgrading a newer checkpoint.
			// The UID check guards against a stale ConfigMap left by a prior run with the same name.
			//
			// Progress is measured by terminal step count, not len(StepStatuses): ExecuteWorkflow
			// initializes an entry for every workflow step up front (all Pending) before the first
			// save, so len(StepStatuses) is constant for the life of the run and would make this
			// guard trip on the very first comparison, permanently freezing the checkpoint at
			// whichever snapshot happened to create the ConfigMap. Terminal count strictly
			// increases with each completed step, so it correctly orders snapshots instead.
			if existing.Data != nil {
				var existingSnap CheckpointSnapshot
				if jsonErr := json.Unmarshal([]byte(existing.Data["checkpoint"]), &existingSnap); jsonErr == nil {
					if existingSnap.WorkflowRunUID == cm.runUID && terminalStepCount(existingSnap.StepStatuses) >= terminalStepCount(snapshot.StepStatuses) {
						return nil
					}
				}
			}
			if existing.Data == nil {
				existing.Data = make(map[string]string)
			}
			existing.Data["checkpoint"] = string(raw)
			return cm.controlClient.Update(writeCtx, existing)
		})
		if err != nil {
			klog.ErrorS(err, "checkpoint: failed to save ConfigMap", "configMap", cm.cmKey)
		}
	}()
}

// SaveAuditSnapshot writes a one-time, always-on snapshot of a run's final execution
// context to a ConfigMap owned by the WorkflowRun. Unlike SaveAsync, this is never gated
// by cm.enabled: it exists so "what did this run actually see/compute" can be inspected
// after completion even when per-step checkpointing was never turned on. Callers are
// expected to invoke this exactly once, at terminal completion (Succeeded or Failed).
// It blocks until the write completes (or ctx is done) and returns the ConfigMap name on
// success, so callers can record it on WorkflowRun.Status.AuditSnapshotConfigMap.
//
// The ConfigMap carries the same ownerRefs as the checkpoint ConfigMap, so it is
// garbage-collected together with its WorkflowRun (governed by the Workflow's retention
// settings) rather than tied to the executor pod's own lifetime.
func (cm *CheckpointManager) SaveAuditSnapshot(ctx context.Context, snapshot CheckpointSnapshot) (string, error) {
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return "", fmt.Errorf("audit snapshot: failed to marshal snapshot: %w", err)
	}
	if len(raw) > auditSnapshotSizeThreshold {
		raw, err = json.Marshal(truncateAuditSnapshot(snapshot))
		if err != nil {
			return "", fmt.Errorf("audit snapshot: failed to marshal truncated snapshot: %w", err)
		}
		if len(raw) > auditSnapshotSizeThreshold {
			raw, err = json.Marshal(minimalAuditSnapshot(snapshot))
			if err != nil {
				return "", fmt.Errorf("audit snapshot: failed to marshal minimal snapshot: %w", err)
			}
			if len(raw) > auditSnapshotSizeThreshold {
				return "", fmt.Errorf("audit snapshot: minimal snapshot still exceeds %d byte threshold (%d bytes)", auditSnapshotSizeThreshold, len(raw))
			}
			klog.InfoS("audit snapshot: context still too large after array truncation, dropped entirely", "workflowRunUID", cm.runUID)
		}
	}

	writeCtx, cancel := context.WithTimeout(ctx, auditSnapshotWriteTimeout)
	defer cancel()

	err = retryOnConflictKeepErr(retry.DefaultBackoff, func() error {
		existing := &corev1.ConfigMap{}
		getErr := cm.controlClient.Get(writeCtx, cm.auditCmKey, existing)
		if apierrors.IsNotFound(getErr) {
			newCM := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      cm.auditCmKey.Name,
					Namespace: cm.auditCmKey.Namespace,
					Labels: map[string]string{
						"app.kubernetes.io/part-of":    "ottoflow",
						"app.kubernetes.io/managed-by": "ottoflow-runner",
					},
					OwnerReferences: cm.ownerRefs,
				},
				Data: map[string]string{"snapshot": string(raw)},
			}
			createErr := cm.controlClient.Create(writeCtx, newCM)
			if apierrors.IsAlreadyExists(createErr) {
				// Synthesize a conflict so RetryOnConflict re-enters the loop as a Get+Update.
				return apierrors.NewConflict(corev1.Resource("configmaps"), cm.auditCmKey.Name, createErr)
			}
			return createErr
		}
		if getErr != nil {
			return getErr
		}
		// Re-assert ownerRefs/labels on the update path too: a stale ConfigMap left by a
		// prior run with the same name (auditConfigMapName is deterministic per run name,
		// not per UID) could otherwise remain owned by that old WorkflowRun and be
		// garbage-collected out from under this run's Status.AuditSnapshotConfigMap.
		existing.OwnerReferences = cm.ownerRefs
		if existing.Labels == nil {
			existing.Labels = make(map[string]string, 2)
		}
		existing.Labels["app.kubernetes.io/part-of"] = "ottoflow"
		existing.Labels["app.kubernetes.io/managed-by"] = "ottoflow-runner"
		if existing.Data == nil {
			existing.Data = make(map[string]string)
		}
		existing.Data["snapshot"] = string(raw)
		return cm.controlClient.Update(writeCtx, existing)
	})
	if err != nil {
		return "", fmt.Errorf("audit snapshot: failed to save ConfigMap: %w", err)
	}
	return cm.auditCmKey.Name, nil
}

// truncateAuditSnapshot returns a copy of snapshot with large array-valued entries,
// wherever they occur in the Context tree, replaced by a truncated view (first
// auditSnapshotMaxArrayEntries entries plus original/kept counts) — so the marshaled size
// stays under the ConfigMap size limit while still recording, from the snapshot alone, what
// was dropped and how much. The walk is depth-agnostic: forEach results are stored as
// context["steps"][name] = {"results": [...], "succeeded": [...], "failed": [...]}, three
// levels down, which a one-level-deep walk would skip entirely (a large "succeeded" array
// from a single forEach step could then dominate the payload untouched).
func truncateAuditSnapshot(snapshot CheckpointSnapshot) CheckpointSnapshot {
	out := snapshot
	var truncatedKeys []string
	truncated, _ := truncateArraysRecursive(snapshot.Context, "", &truncatedKeys).(map[string]interface{})
	if truncated == nil {
		truncated = map[string]interface{}{}
	}
	out.Context = truncated
	if len(truncatedKeys) > 0 {
		out.Context["_auditSnapshotTruncatedKeys"] = truncatedKeys
	}
	return out
}

func truncateArraysRecursive(v interface{}, path string, truncatedKeys *[]string) interface{} {
	switch val := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(val))
		for k, nv := range val {
			childPath := k
			if path != "" {
				childPath = path + "." + k
			}
			out[k] = truncateArraysRecursive(nv, childPath, truncatedKeys)
		}
		return out
	case []interface{}:
		if len(val) <= auditSnapshotMaxArrayEntries {
			out := make([]interface{}, len(val))
			for i, nv := range val {
				out[i] = truncateArraysRecursive(nv, path, truncatedKeys)
			}
			return out
		}
		*truncatedKeys = append(*truncatedKeys, path)
		kept := make([]interface{}, auditSnapshotMaxArrayEntries)
		for i := 0; i < auditSnapshotMaxArrayEntries; i++ {
			kept[i] = truncateArraysRecursive(val[i], path, truncatedKeys)
		}
		return map[string]interface{}{
			"truncated":     true,
			"originalCount": len(val),
			"keptCount":     auditSnapshotMaxArrayEntries,
			"items":         kept,
		}
	default:
		return v
	}
}

// minimalAuditSnapshot drops the entire Context (the only unbounded field) when even the
// array-truncated snapshot still exceeds the ConfigMap size threshold (e.g. a single huge
// scalar/map value, or so many distinct array keys that truncation alone isn't enough). This
// keeps the run identity, final step, and step statuses recorded rather than failing the
// write outright and leaving no audit trail at all.
func minimalAuditSnapshot(snapshot CheckpointSnapshot) CheckpointSnapshot {
	out := snapshot
	out.Context = map[string]interface{}{
		"_auditSnapshotDropped": true,
		"_auditSnapshotReason":  "context exceeded size threshold even after array truncation",
	}
	return out
}

// Load returns nil when disabled, not found, or the UID doesn't match (stale checkpoint).
func (cm *CheckpointManager) Load(ctx context.Context) (*CheckpointSnapshot, error) {
	if !cm.enabled {
		return nil, nil
	}

	existing := &corev1.ConfigMap{}
	if err := cm.controlClient.Get(ctx, cm.cmKey, existing); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}

	data, ok := existing.Data["checkpoint"]
	if !ok || data == "" {
		return nil, nil
	}

	var snapshot CheckpointSnapshot
	if err := json.Unmarshal([]byte(data), &snapshot); err != nil {
		return nil, fmt.Errorf("checkpoint: failed to unmarshal checkpoint data: %w", err)
	}

	if snapshot.WorkflowRunUID != cm.runUID {
		return nil, nil
	}

	return &snapshot, nil
}

func (cm *CheckpointManager) Delete(ctx context.Context) error {
	if !cm.enabled {
		return nil
	}

	existing := &corev1.ConfigMap{}
	existing.Name = cm.cmKey.Name
	existing.Namespace = cm.cmKey.Namespace
	if err := cm.controlClient.Delete(ctx, existing); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// Flush waits for all pending async saves to complete, or until ctx is cancelled.
func (cm *CheckpointManager) Flush(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		cm.pending.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (cm *CheckpointManager) Enabled() bool { return cm.enabled }
