/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/nirmata/ottoflow/internal/agent"
)

// TestNoNonTestCodeMutatesProcessEnv guards the SECURITY note on LLMEnvAllowlist: LLMEnvAllowlist
// values are tenant-controlled, and the agent endpoint allowlist and the skipVerifySSL refusal
// hold only while none of them reaches the agent-executor's process environment, where LLM
// clients read their endpoint and TLS settings. No non-test file calls os.Setenv today, so any
// new call must be reviewed against that rule. This is a text search: a mention inside a
// comment or string trips it too.
func TestNoNonTestCodeMutatesProcessEnv(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to resolve caller for repo root")
	}
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")

	var scanned int
	var matches []string
	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); path != repoRoot && (strings.HasPrefix(name, ".") || name == "bin" || name == "vendor") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		scanned++
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(b), "os.Setenv(") || strings.Contains(string(b), "syscall.Setenv(") {
			rel, _ := filepath.Rel(repoRoot, path)
			matches = append(matches, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking repo: %v", err)
	}
	// A broken walk (wrong root, everything skipped) must not pass vacuously.
	if scanned < 50 {
		t.Fatalf("scanned only %d non-test .go files (want at least 50); the walk is broken", scanned)
	}
	if len(matches) > 0 {
		t.Fatalf("os.Setenv/syscall.Setenv found in %v. LLMEnvAllowlist values are tenant-controlled "+
			"(exec_client.go); a process-environment write fed from them would let a workflow run choose "+
			"where the agent-executor's LLM credentials are sent. If the new call is unrelated, make "+
			"sure it never takes a value from agent.GetLLMEnv, then narrow this test.", matches)
	}
}

// TestEndpointAndTLSVariablesAreNotTenantForwardable: the endpoint allowlist must be read only
// from the agent-executor's own environment, and LLM_SKIP_VERIFY_SSL is gollm's process-wide TLS
// switch. Forwarding either through X-LLM-Env would let a run undo the corresponding refusal.
func TestEndpointAndTLSVariablesAreNotTenantForwardable(t *testing.T) {
	for _, name := range []string{agent.AgentLLMEndpointAllowlistEnv, "LLM_SKIP_VERIFY_SSL"} {
		if slices.Contains(LLMEnvAllowlist, name) {
			t.Errorf("%s must not be in LLMEnvAllowlist: the X-LLM-Env header would make it tenant-controlled", name)
		}
	}
}
