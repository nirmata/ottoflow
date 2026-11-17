/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"context"
	"testing"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

// A failed inputMapping must still set the key; omitting it would fail every triggered run of a workflow that marks that input required.
func TestEventTriggerInputMapping_FailedExpressionStillSetsKey(t *testing.T) {
	env := newCreateRetryEnv(nil)
	env.spec.InputMapping = map[string]string{"appName": "object.status.missing"}
	if err := env.create(context.Background()); err != nil {
		t.Fatal(err)
	}

	var runs ottoflowv1alpha1.WorkflowRunList
	if err := env.c.List(context.Background(), &runs); err != nil || len(runs.Items) != 1 {
		t.Fatalf("list runs: %v, %d items", err, len(runs.Items))
	}
	got, ok := runs.Items[0].Spec.InputValues["appName"]
	if !ok || got != "" {
		t.Errorf("expected appName set to empty string, got %q (present=%v)", got, ok)
	}
}
