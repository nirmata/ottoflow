/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package v1alpha1

import "testing"

func TestRunPolicyEffectiveMaxConcurrentRuns(t *testing.T) {
	for name, tc := range map[string]struct {
		run  *RunPolicy
		want int32
	}{
		"nil run":    {nil, 10},
		"nil field":  {&RunPolicy{}, 10},
		"explicit 0": {&RunPolicy{MaxConcurrentRuns: int32Ptr(0)}, 0},
		"explicit 3": {&RunPolicy{MaxConcurrentRuns: int32Ptr(3)}, 3},
	} {
		t.Run(name, func(t *testing.T) {
			if got := tc.run.EffectiveMaxConcurrentRuns(); got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}
