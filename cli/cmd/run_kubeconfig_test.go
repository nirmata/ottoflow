/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nirmata/ottoflow/test/stdiomcp"
)

func TestMain(m *testing.M) {
	stdiomcp.ServeIfChild()
	os.Exit(m.Run())
}

// noClusterKubeconfig parses, but its server refuses connections, so nothing reaches a cluster.
const noClusterKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: c
  cluster:
    server: https://127.0.0.1:1
contexts:
- name: c
  context:
    cluster: c
    user: u
current-context: c
users:
- name: u
  user:
    token: fake
`

const stdioKubeconfigWorkflow = `apiVersion: ottoflow.nirmata.io/v1alpha1
kind: MCPServer
metadata:
  name: env-probe
  namespace: default
spec:
  transport:
    type: stdio
    command: [%q]
  env:
    - name: %s
      value: "1"
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: stdio-kubeconfig
  namespace: default
spec:
  steps:
    - name: probe
      mcpToolCall:
        server: env-probe
        tool: %s
      outputs:
        - name: seen
          expression: 'toolResult'
  outputs:
    - name: seen
      expression: 'variables.seen'
`

// `ottoflow run wf.yaml --kubeconfig X` (the bare-file form) must start stdio MCP servers with
// KUBECONFIG=X, not the shell's KUBECONFIG, so they act on the same cluster as the run.
func TestRunBareFileKubeconfigFlagReachesStdioMCPServer(t *testing.T) {
	dir := t.TempDir()
	flagPath := filepath.Join(dir, "target.kubeconfig")
	ambientPath := filepath.Join(dir, "ambient.kubeconfig")
	for _, p := range []string{flagPath, ambientPath} {
		if err := os.WriteFile(p, []byte(noClusterKubeconfig), 0600); err != nil {
			t.Fatalf("write kubeconfig: %v", err)
		}
	}
	t.Setenv("HOME", dir)
	t.Setenv("KUBECONFIG", ambientPath)

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	wfPath := filepath.Join(dir, "wf.yaml")
	manifest := fmt.Sprintf(stdioKubeconfigWorkflow, exe, stdiomcp.ServeEnv, stdiomcp.Tool)
	if err := os.WriteFile(wfPath, []byte(manifest), 0600); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	outDir := filepath.Join(dir, "out")

	oldKubeconfig, oldOutputDir, oldRunFile := kubeconfig, outputDir, runFile
	t.Cleanup(func() {
		kubeconfig, outputDir, runFile = oldKubeconfig, oldOutputDir, oldRunFile
		rootCmd.SetArgs(nil)
	})
	rootCmd.SetArgs([]string{"run", wfPath, "--kubeconfig", flagPath, "--output-dir", outDir})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("ottoflow run: %v", err)
	}

	saved, err := filepath.Glob(filepath.Join(outDir, "*.json"))
	if err != nil || len(saved) != 1 {
		t.Fatalf("expected one saved JSON run output in %s, got %v (err %v)", outDir, saved, err)
	}
	data, err := os.ReadFile(saved[0])
	if err != nil {
		t.Fatalf("read run output: %v", err)
	}
	if want := "KUBECONFIG=" + flagPath; !strings.Contains(string(data), want) {
		t.Errorf("stdio MCP server output does not contain %q; run output:\n%s", want, data)
	}
}
