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
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/workflow/token"
)

// ErrAwaitingCallback is the sentinel error returned when a waitForCallback step
// has set up its callback token and the runner must exit with code 0 (clean pause).
// The caller (runner main loop) checks for this error with errors.Is and exits cleanly.
var ErrAwaitingCallback = errors.New("awaiting callback")

// executeWaitForCallback handles the waitForCallback step type.
//
// There are three paths through this function:
//  1. Recovery path: PendingCallback exists for this step AND outputs are populated
//     → unmarshal outputs, clear PendingCallback via K8s patch, return outputs.
//  2. Recovery path: PendingCallback exists for this step AND no outputs (still waiting)
//     → return ErrAwaitingCallback so the runner exits with code 0 again.
//  3. New path: no PendingCallback for this step
//     → generate token, patch WorkflowRun status, set step Waiting, return ErrAwaitingCallback.
func (e *WorkflowExecutor) executeWaitForCallback(ctx context.Context, workflowRun *ottoflowv1alpha1.WorkflowRun, step ottoflowv1alpha1.Step) (map[string]interface{}, error) {
	logger := klog.LoggerWithValues(klog.FromContext(ctx), "step", step.Name, "stepType", "waitForCallback")
	ctx = klog.NewContext(ctx, logger)

	if step.WaitForCallback == nil {
		return nil, fmt.Errorf("waitForCallback step has nil configuration")
	}
	wfc := step.WaitForCallback

	// Parse timeout duration (for new-token generation; also used for logging on existing token)
	timeoutDur, err := time.ParseDuration(wfc.Timeout)
	if err != nil {
		return nil, fmt.Errorf("invalid timeout duration %q: %w", wfc.Timeout, err)
	}

	// --- Recovery path: callback already pending for this step ---
	if workflowRun.Status.PendingCallback != nil && workflowRun.Status.PendingCallback.StepName == step.Name {
		cb := workflowRun.Status.PendingCallback

		// Check if callback received (Outputs will be non-empty)
		if len(cb.Outputs.Raw) > 0 {
			outputs := make(map[string]interface{})
			if err := json.Unmarshal(cb.Outputs.Raw, &outputs); err != nil {
				return nil, fmt.Errorf("failed to parse callback outputs: %w", err)
			}
			logger.Info("Callback received, resuming step", "stepName", step.Name)

			// Clear PendingCallback via K8s status patch so the controller knows we've consumed it.
			// This MUST succeed before we proceed — a crash here leaves K8s showing an active
			// PendingCallback while the runner has already consumed the outputs. On the next
			// Job restart the recovery path retries this clear.
			if e.controlClient != nil {
				if patchErr := clearPendingCallbackPatch(ctx, e.controlClient, workflowRun); patchErr != nil {
					return nil, fmt.Errorf("failed to clear PendingCallback after callback consumed: %w", patchErr)
				}
			}
			workflowRun.Status.PendingCallback = nil
			return outputs, nil
		}

		// Check timeout expiry
		if time.Now().Unix() > cb.ExpiresAt {
			logger.Info("Callback timeout expired on recovery", "stepName", step.Name)
			if e.controlClient != nil {
				if patchErr := clearPendingCallbackPatch(ctx, e.controlClient, workflowRun); patchErr != nil {
					logger.Error(patchErr, "failed to clear expired PendingCallback")
				}
			}
			workflowRun.Status.PendingCallback = nil
			return handleCallbackTimeout(wfc, timeoutDur)
		}

		// Still waiting — re-pause
		logger.Info("Still awaiting callback", "stepName", step.Name,
			"expiresAt", time.Unix(cb.ExpiresAt, 0).Format(time.RFC3339))
		return nil, fmt.Errorf("%w: step %q waiting for callback (expires %s)",
			ErrAwaitingCallback, step.Name, time.Unix(cb.ExpiresAt, 0).Format(time.RFC3339))
	}

	// --- New path: generate token and pause ---
	gen := token.NewGenerator()
	callbackToken, tokenHash, err := gen.Generate()
	if err != nil {
		return nil, fmt.Errorf("failed to generate callback token: %w", err)
	}

	now := time.Now()
	expiresAt := now.Add(timeoutDur).Unix()

	callbackState := &ottoflowv1alpha1.CallbackState{
		TokenHash: tokenHash, // only the hash is stored in K8s status; plaintext never persisted
		StepName:  step.Name,
		ExpiresAt: expiresAt,
		CreatedAt: now.Unix(),
	}

	// Patch WorkflowRun.status.pendingCallback via K8s status subresource
	if e.controlClient != nil {
		if patchErr := setPendingCallbackPatch(ctx, e.controlClient, workflowRun, callbackState); patchErr != nil {
			return nil, fmt.Errorf("failed to patch WorkflowRun status with PendingCallback: %w", patchErr)
		}
	}
	// Update in-memory so caller sees current state
	workflowRun.Status.PendingCallback = callbackState

	callbackURL := fmt.Sprintf("/api/v1/workflow-runs/%s/%s/callback/%s",
		workflowRun.Namespace, workflowRun.Name, callbackToken)
	logger.Info("Callback token generated — step paused",
		"stepName", step.Name,
		"callbackURL", callbackURL,
		"expiresAt", time.Unix(expiresAt, 0).Format(time.RFC3339),
		"timeout", wfc.Timeout,
	)
	if wfc.Message != "" {
		logger.Info("Step message", "message", wfc.Message)
	}

	// Best-effort write of the plaintext token to variables.callbackToken in the
	// in-process context. Nothing reads it — the ErrAwaitingCallback below propagates
	// out of executor.go's sequential step loop, and a resume starts a fresh process.
	if wErr := e.contextManager.WriteStepOutputs(ctx, step.Name, map[string]interface{}{"callbackToken": callbackToken}); wErr != nil {
		logger.Error(wErr, "failed to write callbackToken to step context")
	}

	return nil, fmt.Errorf("%w: step %q waiting for callback at POST %s (expires %s)",
		ErrAwaitingCallback, step.Name, callbackURL,
		time.Unix(expiresAt, 0).Format(time.RFC3339))
}

// handleCallbackTimeout applies the step's failurePolicy when the callback deadline is exceeded.
func handleCallbackTimeout(wfc *ottoflowv1alpha1.WaitForCallbackStep, timeoutDur time.Duration) (map[string]interface{}, error) {
	policy := wfc.FailurePolicy
	if policy == "" {
		policy = ottoflowv1alpha1.FailurePolicyFail
	}
	if policy == ottoflowv1alpha1.FailurePolicyContinue {
		// Return empty outputs so the step is treated as Skipped by the caller
		return map[string]interface{}{}, nil
	}
	return nil, fmt.Errorf("callback timeout: no callback received within %v", timeoutDur)
}

// setPendingCallbackPatch atomically patches WorkflowRun.status.pendingCallback using
// the status subresource. Retries on conflict, refetching the object each time to get a fresh resourceVersion.
func setPendingCallbackPatch(ctx context.Context, c client.Client, wr *ottoflowv1alpha1.WorkflowRun, cb *ottoflowv1alpha1.CallbackState) error {
	cbJSON, err := json.Marshal(cb)
	if err != nil {
		return fmt.Errorf("marshal CallbackState: %w", err)
	}
	patch := fmt.Sprintf(`{"status":{"pendingCallback":%s}}`, string(cbJSON))
	return retryOnConflictKeepErr(func() error {
		current := &ottoflowv1alpha1.WorkflowRun{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(wr), current); err != nil {
			return err
		}
		return c.Status().Patch(ctx, current, client.RawPatch(types.MergePatchType, []byte(patch)))
	})
}

// clearPendingCallbackPatch atomically clears WorkflowRun.status.pendingCallback via status subresource.
// Refetches the object on each conflict retry to get a fresh resourceVersion.
func clearPendingCallbackPatch(ctx context.Context, c client.Client, wr *ottoflowv1alpha1.WorkflowRun) error {
	patch := `{"status":{"pendingCallback":null}}`
	return retryOnConflictKeepErr(func() error {
		current := &ottoflowv1alpha1.WorkflowRun{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(wr), current); err != nil {
			return err
		}
		return c.Status().Patch(ctx, current, client.RawPatch(types.MergePatchType, []byte(patch)))
	})
}

// retryOnConflictKeepErr is retry.RetryOnConflict that reports the last error when ctx ends mid-retry.
// RetryOnConflict returns nil then, though nothing was written, and SIGTERM would read as a stored patch.
func retryOnConflictKeepErr(fn func() error) error {
	var last error
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		last = fn()
		return last
	})
	if err == nil {
		return last
	}
	return err
}
