/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"regexp"
	"strconv"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

const (
	defaultRetryInitialInterval = time.Second
	defaultRetryMaxInterval     = 5 * time.Minute
	defaultRetryMultiplier      = 2.0
)

// ErrStepTimeout is wrapped into the error returned when a step exceeds spec.steps[].timeout.
var ErrStepTimeout = errors.New("step timed out")

// executeStep runs a step honoring its timeout (per attempt) and retry policy.
//
// Steps that delegate to an inner executeStep call apply these policies at the inner level
// to avoid compounding them: stepTemplateRef merges retry/timeout into the instantiated step,
// and forEach items carry their own retry/timeout. A forEach parent still honors its own
// timeout (bounding the whole loop) but is not retried as a unit. waitForCallback steps
// manage their own expiry and are never retried.
func (e *WorkflowExecutor) executeStep(ctx context.Context, workflowRun *ottoflowv1alpha1.WorkflowRun, step ottoflowv1alpha1.Step) (map[string]interface{}, error) {
	if step.StepTemplateRef != nil || step.WaitForCallback != nil {
		return e.executeStepOnce(ctx, workflowRun, step)
	}

	timeout, err := parseStepTimeout(step.Timeout)
	if err != nil {
		return nil, err
	}

	attempts := 1
	if step.Retry != nil && step.Retry.Attempts > 1 && step.ForEach == nil {
		attempts = step.Retry.Attempts
	}

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		outputs, err := e.executeStepAttempt(ctx, workflowRun, step, timeout)
		if err == nil {
			return outputs, nil
		}
		lastErr = err

		if attempt == attempts || ctx.Err() != nil || errors.Is(err, ErrAwaitingCallback) || !shouldRetry(step.Retry, err) {
			break
		}

		delay, err := retryDelay(step.Retry.Backoff, attempt)
		if err != nil {
			return nil, err
		}
		e.recordStepRetry(workflowRun, step.Name, attempt, delay)
		klog.V(2).InfoS("retrying step", "step", step.Name, "attempt", attempt+1, "maxAttempts", attempts, "delay", delay, "error", lastErr)

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, lastErr
		case <-timer.C:
		}
	}

	if attempts > 1 {
		return nil, fmt.Errorf("step failed after %d attempt(s): %w", attempts, lastErr)
	}
	return nil, lastErr
}

// executeStepAttempt runs one attempt of a step, bounded by timeout when non-zero.
func (e *WorkflowExecutor) executeStepAttempt(ctx context.Context, workflowRun *ottoflowv1alpha1.WorkflowRun, step ottoflowv1alpha1.Step, timeout time.Duration) (map[string]interface{}, error) {
	if timeout <= 0 {
		return e.executeStepOnce(ctx, workflowRun, step)
	}
	attemptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	outputs, err := e.executeStepOnce(attemptCtx, workflowRun, step)
	// Report a timeout only when this step's own deadline fired, not when the parent ctx was cancelled.
	if errors.Is(attemptCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
		if err == nil {
			err = context.DeadlineExceeded
		}
		return nil, fmt.Errorf("%w after %s: %w", ErrStepTimeout, timeout, err)
	}
	return outputs, err
}

// recordStepRetry updates the step's retry bookkeeping in the run status.
func (e *WorkflowExecutor) recordStepRetry(workflowRun *ottoflowv1alpha1.WorkflowRun, stepName string, attempt int, delay time.Duration) {
	status, ok := workflowRun.Status.StepStatuses[stepName]
	if !ok {
		return // forEach items have no per-item status entry
	}
	now := metav1.Now()
	next := metav1.NewTime(now.Add(delay))
	status.RetryCount = attempt
	status.LastRetryTime = &now
	status.NextRetryTime = &next
	workflowRun.Status.StepStatuses[stepName] = status
	if e.currentWorkflow != nil {
		e.invokeProgressCallback(workflowRun, e.currentWorkflow)
	}
}

func parseStepTimeout(raw string) (time.Duration, error) {
	if raw == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid step timeout %q: %w", raw, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("invalid step timeout %q: must be positive", raw)
	}
	return d, nil
}

// retryDelay returns the wait before the retry that follows the given (1-based) failed attempt.
func retryDelay(backoff *ottoflowv1alpha1.BackoffConfig, attempt int) (time.Duration, error) {
	strategy := "exponential"
	initial := defaultRetryInitialInterval
	maxInterval := defaultRetryMaxInterval
	multiplier := defaultRetryMultiplier

	if backoff != nil {
		if backoff.Strategy != "" {
			strategy = backoff.Strategy
		}
		if backoff.InitialInterval != "" {
			d, err := time.ParseDuration(backoff.InitialInterval)
			if err != nil {
				return 0, fmt.Errorf("invalid retry backoff initialInterval %q: %w", backoff.InitialInterval, err)
			}
			initial = d
		}
		if backoff.MaxInterval != "" {
			d, err := time.ParseDuration(backoff.MaxInterval)
			if err != nil {
				return 0, fmt.Errorf("invalid retry backoff maxInterval %q: %w", backoff.MaxInterval, err)
			}
			maxInterval = d
		}
		if backoff.Multiplier > 0 {
			multiplier = backoff.Multiplier
		}
	}

	var delay time.Duration
	switch strategy {
	case "none":
		return 0, nil
	case "linear":
		delay = initial
	default: // exponential
		f := float64(initial) * math.Pow(multiplier, float64(attempt-1))
		if f > float64(maxInterval) {
			return maxInterval, nil
		}
		delay = time.Duration(f)
	}
	if delay > maxInterval {
		delay = maxInterval
	}
	return delay, nil
}

// shouldRetry reports whether err matches the policy's retryOn conditions. An empty
// retryOn list retries on any error. Each condition matches when any of its set fields match.
func shouldRetry(policy *ottoflowv1alpha1.RetryPolicy, err error) bool {
	if policy == nil || len(policy.RetryOn) == 0 {
		return true
	}
	for _, cond := range policy.RetryOn {
		if cond.ErrorType != "" && matchesErrorType(cond.ErrorType, err) {
			return true
		}
		if len(cond.HTTPStatus) > 0 && matchesHTTPStatus(cond.HTTPStatus, err) {
			return true
		}
		if cond.ErrorMessage != "" {
			re, reErr := regexp.Compile(cond.ErrorMessage)
			if reErr != nil {
				klog.InfoS("ignoring invalid retryOn.errorMessage regex", "pattern", cond.ErrorMessage, "error", reErr)
				continue
			}
			if re.MatchString(err.Error()) {
				return true
			}
		}
	}
	return false
}

func matchesErrorType(errorType string, err error) bool {
	isTimeout := errors.Is(err, ErrStepTimeout) || errors.Is(err, context.DeadlineExceeded) || apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err)
	var netErr net.Error
	isNetwork := errors.As(err, &netErr)
	switch errorType {
	case "TimeoutError":
		return isTimeout
	case "NetworkError":
		return isNetwork
	case "TransientError":
		return isTimeout || isNetwork || apierrors.IsTooManyRequests(err) || apierrors.IsServiceUnavailable(err) ||
			apierrors.IsInternalError(err) || apierrors.IsConflict(err)
	default:
		return false
	}
}

// matchesHTTPStatus matches Kubernetes API status codes, and otherwise looks for the code as a
// standalone number in the error text (MCP, A2A, Prometheus and LLM clients surface codes that way).
func matchesHTTPStatus(codes []int, err error) bool {
	var statusErr apierrors.APIStatus
	if errors.As(err, &statusErr) {
		code := int(statusErr.Status().Code)
		for _, c := range codes {
			if c == code {
				return true
			}
		}
		return false
	}
	msg := err.Error()
	for _, c := range codes {
		if regexp.MustCompile(`\b` + strconv.Itoa(c) + `\b`).MatchString(msg) {
			return true
		}
	}
	return false
}
