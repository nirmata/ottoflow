/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"slices"
	"testing"

	"github.com/nirmata/ottoflow/internal/workflow/executor"
)

// TestNirmataTokenEnvNames_SubsetOfLLMEnvAllowlist pins the invariant hasExplicitNirmataCreds
// depends on: every name in nirmataTokenEnvNames is one the runner forwards to the agent-executor
// (executor.LLMEnvAllowlist). A token supplied under a name outside that allowlist never reaches
// the agent-executor, so counting it as "the run carries its own credentials" would skip the
// credentials-Secret injection for a run that still has no working token. A rename on either side
// fails here instead of surfacing as that silent skip.
func TestNirmataTokenEnvNames_SubsetOfLLMEnvAllowlist(t *testing.T) {
	if len(nirmataTokenEnvNames) == 0 {
		t.Fatal("nirmataTokenEnvNames is empty; hasExplicitNirmataCreds could never recognise a token")
	}
	for _, name := range nirmataTokenEnvNames {
		if !slices.Contains(executor.LLMEnvAllowlist, name) {
			t.Errorf("nirmataTokenEnvNames entry %q is not in executor.LLMEnvAllowlist %v: the runner would "+
				"never forward it, so it cannot count as explicit credentials", name, executor.LLMEnvAllowlist)
		}
	}
}
