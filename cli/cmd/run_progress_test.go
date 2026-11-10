/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

const forEachWorkflowRefManifest = `apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: child
  namespace: default
spec:
  steps:
    - name: one
      outputs:
        - name: a
          expression: '"x"'
    - name: two
      dependsOn: [one]
      outputs:
        - name: b
          expression: '"y"'
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: parent
  namespace: default
spec:
  steps:
    - name: fan-out
      forEach:
        items: '[1, 2, 3, 4, 5, 6, 7, 8]'
        maxConcurrency: 8
        step:
          workflowRef:
            name: child
`

// Each forEach item runs its workflowRef in a child executor that shares the CLI's progress
// callback, so the callback is called from several goroutines at once. Run with -race.
func TestRunForEachWorkflowRefProgressIsConcurrencySafe(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	kubeconfigPath := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(kubeconfigPath, []byte(noClusterKubeconfig), 0600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	t.Setenv("KUBECONFIG", kubeconfigPath)
	wfPath := filepath.Join(dir, "wf.yaml")
	if err := os.WriteFile(wfPath, []byte(forEachWorkflowRefManifest), 0600); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	oldRunFile, oldWorkflowName, oldOutputFormat, oldMaxWorkers := runFile, workflowName, outputFormat, maxWorkers
	t.Cleanup(func() {
		runFile, workflowName, outputFormat, maxWorkers = oldRunFile, oldWorkflowName, oldOutputFormat, oldMaxWorkers
		rootCmd.SetArgs(nil)
	})
	rootCmd.SetArgs([]string{"run", "-f", wfPath, "parent", "--output", "table", "--max-workers", "8"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("ottoflow run: %v", err)
	}
}
