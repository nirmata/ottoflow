/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// resetRootCmd snapshots the mutable global state these tests touch — the
// version vars, the persistent --output flag, and rootCmd's args/writers — and
// restores it when the test ends. versionOutputFormat is a persistent cobra flag
// whose value survives across rootCmd.Execute() calls, so without this the tests
// would be order-dependent (e.g. a prior --output json run leaking into a test
// that omits --output) and would also pollute other cmd-package tests that drive
// rootCmd.
func resetRootCmd(t *testing.T) {
	t.Helper()
	oldV, oldC, oldB, oldFormat := version, gitCommit, buildTime, versionOutputFormat
	t.Cleanup(func() {
		version, gitCommit, buildTime, versionOutputFormat = oldV, oldC, oldB, oldFormat
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
	})
}

func TestVersionCommandTable(t *testing.T) {
	resetRootCmd(t)
	version, gitCommit, buildTime = "v1.2.3", "abc1234", "2026-08-24_00:00:00"

	buf := &bytes.Buffer{}
	rootCmd.SetOut(buf)
	rootCmd.SetErr(buf)
	// Pass --output table explicitly rather than relying on the flag default:
	// versionOutputFormat is a persistent flag, so being explicit keeps this test
	// self-sufficient regardless of what ran before it.
	rootCmd.SetArgs([]string{"version", "--output", "table"})

	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	wants := []string{
		"Version:    v1.2.3",
		"Git Commit: abc1234",
		"Build Time: 2026-08-24_00:00:00",
		"Go Version:",
		"Platform:",
	}
	for _, want := range wants {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q, got:\n%s", want, out)
		}
	}
}

func TestVersionCommandJSON(t *testing.T) {
	resetRootCmd(t)
	version, gitCommit, buildTime = "v1.2.3", "abc1234", "2026-08-24_00:00:00"

	buf := &bytes.Buffer{}
	rootCmd.SetOut(buf)
	rootCmd.SetErr(buf)
	rootCmd.SetArgs([]string{"version", "--output", "json"})

	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var info buildInfo
	if err := json.Unmarshal(buf.Bytes(), &info); err != nil {
		t.Fatalf("failed to unmarshal JSON output: %v\noutput: %s", err, buf.String())
	}
	if info.Version != "v1.2.3" || info.GitCommit != "abc1234" || info.BuildTime != "2026-08-24_00:00:00" {
		t.Errorf("unexpected build info: %+v", info)
	}
	if info.GoVersion == "" || info.Platform == "" {
		t.Errorf("expected GoVersion and Platform to be populated: %+v", info)
	}
}

func TestVersionCommandInvalidOutputFormat(t *testing.T) {
	resetRootCmd(t)

	buf := &bytes.Buffer{}
	rootCmd.SetOut(buf)
	rootCmd.SetErr(buf)
	rootCmd.SetArgs([]string{"version", "--output", "xml"})

	if err := rootCmd.Execute(); err == nil {
		t.Fatal("expected error for unsupported output format, got nil")
	}
}
