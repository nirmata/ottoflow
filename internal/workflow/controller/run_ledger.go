/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

// runHoldTTL is how long a held reservation lasts once its Create has returned.
const runHoldTTL = 30 * time.Second

// reservation is a slot for a run the cache may not show yet; heldAt is zero while in flight.
type reservation struct {
	name    string
	heldAt  time.Time
	settled bool
}

type runLedger struct {
	// ponytail: one mutex over all workflows, so a workflow with thousands of retained runs slows every admit; upgrade path is a lock per workflow.
	mu  sync.Mutex
	now func() time.Time
	res map[types.NamespacedName][]*reservation
}

func newRunLedger() *runLedger {
	return &runLedger{now: time.Now, res: make(map[types.NamespacedName][]*reservation)}
}

// ledgerFor returns the scheduler's ledger so cron and event triggers share slots, or a fresh one without a scheduler.
func ledgerFor(s *Scheduler) *runLedger {
	if s == nil {
		return newRunLedger()
	}
	return s.RunLedger
}

// runToken is an admitted creator's slot; its methods are safe on the nil token a zero cap returns.
type runToken struct {
	l   *runLedger
	key types.NamespacedName
	r   *reservation
}

// admit takes a slot when cached active runs plus reservations are under the cap.
func (l *runLedger) admit(ctx context.Context, c client.Client, wf *ottoflowv1alpha1.Workflow, skip ...string) (tok *runToken, admitted bool, active int, err error) {
	limit := wf.Spec.Run.EffectiveMaxConcurrentRuns()
	if limit <= 0 {
		return nil, true, 0, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	var list ottoflowv1alpha1.WorkflowRunList
	if err := c.List(ctx, &list,
		client.InNamespace(wf.Namespace),
		client.MatchingLabels{ottoflowv1alpha1.WorkflowLabelKey: wf.Name},
	); err != nil {
		return nil, false, 0, err
	}
	cached := make(map[string]bool, len(list.Items))
	for _, wr := range list.Items {
		cached[wr.Name] = true
		if wr.Status.Phase.IsActive() && !slices.Contains(skip, wr.Name) {
			active++
		}
	}

	now := l.now()
	key := client.ObjectKeyFromObject(wf)
	for k, rs := range l.res {
		l.res[k] = slices.DeleteFunc(rs, func(r *reservation) bool {
			return (k == key && cached[r.name]) || (!r.heldAt.IsZero() && now.Sub(r.heldAt) > runHoldTTL)
		})
		if len(l.res[k]) == 0 {
			delete(l.res, k)
		}
	}

	active += len(l.res[key])
	if active >= int(limit) {
		return nil, false, active, nil
	}
	r := &reservation{}
	l.res[key] = append(l.res[key], r)
	return &runToken{l: l, key: key, r: r}, true, active, nil
}

// release frees the slot now.
func (t *runToken) release() {
	if t == nil {
		return
	}
	t.l.mu.Lock()
	defer t.l.mu.Unlock()
	if t.r.settled {
		return
	}
	t.r.settled = true
	t.l.res[t.key] = slices.DeleteFunc(t.l.res[t.key], func(r *reservation) bool { return r == t.r })
}

// hold starts the TTL clock, and a non-empty name lets the cache drop the slot sooner; a no-op once settled.
func (t *runToken) hold(name string) {
	if t == nil {
		return
	}
	t.l.mu.Lock()
	defer t.l.mu.Unlock()
	if t.r.settled {
		return
	}
	t.r.settled = true
	t.r.name = name
	t.r.heldAt = t.l.now()
}

// settle frees the slot when the non-nil err proves no run was stored, and otherwise holds it by name.
func (t *runToken) settle(err error, name string) {
	if noRunCreated(err) {
		t.release()
	} else {
		t.hold(name)
	}
}

// noRunCreated reports whether err proves no run was stored: the Create was never sent, or the API server rejected it before writing.
func noRunCreated(err error) bool {
	if errors.Is(err, errStatusUpdateAfterCreate) {
		return false
	}
	return errors.Is(err, errCronInputValuesFrom) || apierrors.IsInvalid(err) || apierrors.IsForbidden(err)
}
