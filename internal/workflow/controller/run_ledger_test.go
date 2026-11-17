/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

// newClockLedger returns a ledger on a fake clock the test advances through the returned pointer.
func newClockLedger() (*runLedger, *time.Time) {
	now := time.Unix(1_000_000, 0)
	l := newRunLedger()
	l.now = func() time.Time { return now }
	return l, &now
}

func mustAdmit(t *testing.T, l *runLedger, c client.Client, wf *ottoflowv1alpha1.Workflow, want bool) *runToken {
	t.Helper()
	tok, admitted, _, err := l.admit(context.Background(), c, wf)
	if err != nil {
		t.Fatal(err)
	}
	if admitted != want {
		t.Fatalf("admitted = %v, want %v", admitted, want)
	}
	return tok
}

// A reservation still in flight never expires.
func TestRunCapLedgerInFlightNeverExpires(t *testing.T) {
	l, now := newClockLedger()
	c, wf := makeFakeClient(), capWorkflow(1)
	mustAdmit(t, l, c, wf, true)
	*now = now.Add(10 * runHoldTTL)
	mustAdmit(t, l, c, wf, false)
}

// A hold expires once the TTL has passed, and not before.
func TestRunCapLedgerHoldExpires(t *testing.T) {
	l, now := newClockLedger()
	c, wf := makeFakeClient(), capWorkflow(1)
	mustAdmit(t, l, c, wf, true).hold("")
	start := *now
	*now = start.Add(runHoldTTL - time.Nanosecond)
	mustAdmit(t, l, c, wf, false)
	*now = start.Add(runHoldTTL + time.Nanosecond)
	mustAdmit(t, l, c, wf, true)
}

// A named hold is dropped as soon as the cache lists that run.
func TestRunCapLedgerDropsHoldByName(t *testing.T) {
	l, _ := newClockLedger()
	wf := capWorkflow(1)
	c := makeFakeClient(wf)
	mustAdmit(t, l, c, wf, true).hold("r1")
	mustAdmit(t, l, c, wf, false)

	run := &ottoflowv1alpha1.WorkflowRun{ObjectMeta: metav1.ObjectMeta{
		Name: "r1", Namespace: defaultNamespace,
		Labels: map[string]string{ottoflowv1alpha1.WorkflowLabelKey: wf.Name},
	}}
	if err := c.Create(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	markRunSucceeded(t, c, "r1")
	mustAdmit(t, l, c, wf, true)
}

// A token that only gets the deferred safety call does not stay in flight: its slot frees once the TTL has passed.
func TestRunCapLedgerSafetyCallSettles(t *testing.T) {
	l, now := newClockLedger()
	c, wf := makeFakeClient(), capWorkflow(1)
	func() {
		tok := mustAdmit(t, l, c, wf, true)
		defer tok.hold("")
	}()
	*now = now.Add(runHoldTTL + time.Second)
	mustAdmit(t, l, c, wf, true)
}

// An admit for one workflow sweeps another workflow's expired hold.
func TestRunCapLedgerSweepsOtherWorkflows(t *testing.T) {
	l, now := newClockLedger()
	c := makeFakeClient()
	sibling := capWorkflow(1)
	sibling.Name = "sibling"
	mustAdmit(t, l, c, sibling, true).hold("")
	*now = now.Add(runHoldTTL + time.Second)
	mustAdmit(t, l, c, capWorkflow(1), true)

	l.mu.Lock()
	defer l.mu.Unlock()
	if rs := l.res[client.ObjectKeyFromObject(sibling)]; len(rs) != 0 {
		t.Fatalf("reservations for the other workflow = %d, want 0", len(rs))
	}
}

// Only a Create that was never sent, or one the API server rejected before writing, frees the slot.
func TestRunCapNoRunCreatedClassifier(t *testing.T) {
	forbidden := apierrors.NewForbidden(runsGR, "r", errors.New("denied"))
	secretsGR := schema.GroupResource{Resource: "secrets"}
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"cron input read forbidden", fmt.Errorf("%w: %w", errCronInputValuesFrom, apierrors.NewForbidden(secretsGR, "s", errors.New("denied"))), true},
		{"already exists", apierrors.NewAlreadyExists(runsGR, "r"), false},
		{"invalid", capInvalid, true},
		{"forbidden", forbidden, true},
		{"status failure wrapping forbidden", fmt.Errorf("%w: k: %w", errStatusUpdateAfterCreate, forbidden), false},
		{"status failure wrapping not found", fmt.Errorf("%w: k: %w", errStatusUpdateAfterCreate, apierrors.NewNotFound(runsGR, "r")), false},
		{"gateway timeout", apierrors.NewTimeoutError("timed out", 0), false},
		{"server timeout with retry", apierrors.NewServerTimeout(runsGR, "create", 2), false},
		{"server timeout without retry", apierrors.NewServerTimeout(runsGR, "create", 0), false},
		{"service unavailable", apierrors.NewServiceUnavailable("apiserver is shutting down"), false},
		{"internal error", apierrors.NewInternalError(errors.New("boom")), false},
		{"too many requests", apierrors.NewTooManyRequests("slow down", 1), false},
		{"plain error", errors.New("connection refused"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := noRunCreated(tc.err); got != tc.want {
				t.Errorf("noRunCreated(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
