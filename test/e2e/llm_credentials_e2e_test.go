//go:build e2e

/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package e2e

import (
	"os"
	"os/exec"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nirmata/ottoflow/test/utils"
)

// applyManifestAndAssertCredentialsForwarded applies a manifest, waits for the
// WorkflowRun to reach a terminal state, then asserts that the failure was an
// API-level error (placeholder token rejected) rather than "credentials required".
// This proves the credential reached the agent-executor regardless of how it was injected.
func applyManifestAndAssertCredentialsForwarded(applyDescription, tempPattern, manifestYAML, runName string) {
	manifestFile, err := os.CreateTemp("", tempPattern)
	Expect(err).NotTo(HaveOccurred())
	_, err = manifestFile.WriteString(manifestYAML)
	Expect(err).NotTo(HaveOccurred())
	Expect(manifestFile.Close()).To(Succeed())

	By(applyDescription)
	cmd := exec.Command("kubectl", "apply", "-f", manifestFile.Name())
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())

	DeferCleanup(func() {
		cmd := exec.Command("kubectl", "delete", "--ignore-not-found=true", "-f", manifestFile.Name())
		_, _ = utils.Run(cmd)
		_ = os.Remove(manifestFile.Name())
	})

	By("waiting for WorkflowRun to reach terminal phase")
	Eventually(func() string {
		cmd := exec.Command("kubectl", "get", "workflowrun", runName,
			"-n", namespace, "-o", "jsonpath={.status.phase}")
		output, err := utils.Run(cmd)
		if err != nil {
			return ""
		}
		return string(output)
	}, 3*time.Minute, time.Second).Should(Or(Equal("Succeeded"), Equal("Failed")))

	cmd = exec.Command("kubectl", "get", "workflowrun", runName,
		"-n", namespace, "-o", "jsonpath={.status.phase}")
	out, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())
	// Placeholder token: Nirmata API rejects; we expect Failed.
	Expect(string(out)).To(Equal("Failed"), "placeholder token typically yields API error")

	cmd = exec.Command("kubectl", "get", "workflowrun", runName,
		"-n", namespace, "-o", "jsonpath={.status.message}")
	msgOut, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())
	Expect(string(msgOut)).NotTo(
		ContainSubstring("Nirmata LLM credentials required"),
		"credentials must have reached the agent-executor",
	)
}

var _ = Describe("LLM credentials forwarding", func() {
	Context("Agent step without NIRMATA_LLM_TOKEN in job env", func() {
		It("fails with credentials required when agent-executor is used", func() {
			workflowManifest := `
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Agent
metadata:
  name: e2e-llm-agent
  namespace: ottoflow
spec:
  prompt: "You are a helpful assistant."
  modelProvider: nirmata
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: e2e-llm-workflow
  namespace: ottoflow
spec:
  steps:
    - name: agentstep
      agentRef:
        name: e2e-llm-agent
        namespace: ottoflow
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: WorkflowRun
metadata:
  name: e2e-llm-run-no-creds
  namespace: ottoflow
spec:
  workflowRef:
    name: e2e-llm-workflow
    namespace: ottoflow
  # No spec.execution.job.env — runner has no NIRMATA_LLM_TOKEN;
  # agent-executor must reject with credentials required
`
			manifestFile, err := os.CreateTemp("", "ottoflow-e2e-llm-*.yaml")
			Expect(err).NotTo(HaveOccurred())
			_, err = manifestFile.WriteString(workflowManifest)
			Expect(err).NotTo(HaveOccurred())
			Expect(manifestFile.Close()).To(Succeed())

			By("creating Agent, Workflow, and WorkflowRun (no LLM env)")
			cmd := exec.Command("kubectl", "apply", "-f", manifestFile.Name())
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			DeferCleanup(func() {
				cmd := exec.Command("kubectl", "delete", "--ignore-not-found=true", "-f", manifestFile.Name())
				_, _ = utils.Run(cmd)
				_ = os.Remove(manifestFile.Name())
			})

			By("waiting for WorkflowRun to reach terminal phase")
			var phase string
			Eventually(func() string {
				cmd := exec.Command(
					"kubectl", "get", "workflowrun", "e2e-llm-run-no-creds",
					"-n", namespace,
					"-o", "jsonpath={.status.phase}",
				)
				output, err := utils.Run(cmd)
				if err != nil {
					return ""
				}
				return string(output)
			}, 3*time.Minute, time.Second).Should(Or(Equal("Succeeded"), Equal("Failed")))

			cmd = exec.Command(
				"kubectl", "get", "workflowrun", "e2e-llm-run-no-creds",
				"-n", namespace,
				"-o", "jsonpath={.status.phase}",
			)
			out, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			phase = string(out)

			By("verifying run failed with credentials required")
			Expect(phase).To(Equal("Failed"), "expected WorkflowRun to fail when no NIRMATA_LLM_TOKEN in job env")
			cmd = exec.Command(
				"kubectl", "get", "workflowrun", "e2e-llm-run-no-creds",
				"-n", namespace,
				"-o", "jsonpath={.status.message}",
			)
			msgOut, err := utils.Run(cmd)
			if err == nil && len(msgOut) > 0 {
				Expect(string(msgOut)).To(Or(
					ContainSubstring("credentials required"),
					ContainSubstring("Nirmata LLM credentials"),
				), "failure message should indicate LLM credentials required")
			}
		})
	})

	Context("Agent step with a Secret at the conventional well-known name, but no opt-in configured", func() {
		// The deployed default (config/generated/install-e2e.yaml) runs the controller with
		// --workflow-runner-llm-credentials-secret="" (unset). Automatic injection is opt-in: it
		// only happens once the cluster-wide flag/env var or the per-run
		// spec.execution.llmCredentialsSecret names a Secret. Simply creating a Secret under the
		// conventional name "ottoflow-llm-credentials", with no opt-in configured anywhere, must
		// NOT be injected. This is the negative case that guards against reintroducing an implicit
		// default.
		It("does not inject credentials when no opt-in is configured", func() {
			workflowManifest := `
apiVersion: v1
kind: Secret
metadata:
  name: ottoflow-llm-credentials
  namespace: ottoflow
type: Opaque
stringData:
  NIRMATA_LLM_TOKEN: "e2e-wellknown-placeholder-token"
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Agent
metadata:
  name: e2e-noopt-agent
  namespace: ottoflow
spec:
  prompt: "You are helpful."
  modelProvider: nirmata
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: e2e-noopt-workflow
  namespace: ottoflow
spec:
  steps:
    - name: agentstep
      agentRef:
        name: e2e-noopt-agent
        namespace: ottoflow
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: WorkflowRun
metadata:
  name: e2e-noopt-run
  namespace: ottoflow
spec:
  workflowRef:
    name: e2e-noopt-workflow
    namespace: ottoflow
  # No spec.execution.job.env and no spec.execution.llmCredentialsSecret opt-in.
  # A Secret named "ottoflow-llm-credentials" exists, but with no cluster-wide flag/env var
  # and no per-run override naming it, the controller must not look it up or inject it.
`
			manifestFile, err := os.CreateTemp("", "ottoflow-e2e-noopt-*.yaml")
			Expect(err).NotTo(HaveOccurred())
			_, err = manifestFile.WriteString(workflowManifest)
			Expect(err).NotTo(HaveOccurred())
			Expect(manifestFile.Close()).To(Succeed())

			By("creating Secret (well-known name), Agent, Workflow, and WorkflowRun (no opt-in)")
			cmd := exec.Command("kubectl", "apply", "-f", manifestFile.Name())
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			DeferCleanup(func() {
				cmd := exec.Command("kubectl", "delete", "--ignore-not-found=true", "-f", manifestFile.Name())
				_, _ = utils.Run(cmd)
				_ = os.Remove(manifestFile.Name())
			})

			By("waiting for WorkflowRun to reach terminal phase")
			Eventually(func() string {
				cmd := exec.Command(
					"kubectl", "get", "workflowrun", "e2e-noopt-run",
					"-n", namespace,
					"-o", "jsonpath={.status.phase}",
				)
				output, err := utils.Run(cmd)
				if err != nil {
					return ""
				}
				return string(output)
			}, 3*time.Minute, time.Second).Should(Or(Equal("Succeeded"), Equal("Failed")))

			By("verifying run failed with credentials required (no injection occurred)")
			cmd = exec.Command(
				"kubectl", "get", "workflowrun", "e2e-noopt-run",
				"-n", namespace,
				"-o", "jsonpath={.status.phase}",
			)
			out, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(out)).To(Equal("Failed"),
				"expected WorkflowRun to fail: no opt-in configured, so the well-known-named Secret must not be injected")

			cmd = exec.Command(
				"kubectl", "get", "workflowrun", "e2e-noopt-run",
				"-n", namespace,
				"-o", "jsonpath={.status.message}",
			)
			msgOut, err := utils.Run(cmd)
			if err == nil && len(msgOut) > 0 {
				Expect(string(msgOut)).To(Or(
					ContainSubstring("credentials required"),
					ContainSubstring("Nirmata LLM credentials"),
				), "failure message should indicate LLM credentials required, proving no injection happened")
			}
		})
	})

	Context("Agent step with spec.execution.llmCredentialsSecret opt-in (no spec.execution.job.env)", func() {
		// Note: this test uses namespace="ottoflow" (same as the controller) which means a regression
		// where the controller reads from its own namespace instead of workflowRun.Namespace would not
		// be caught here. Namespace isolation is covered by unit test
		// TestBuildWorkflowRunnerJob_InjectsWellKnownLLMCredentials.
		It("injects credentials when explicitly opted in via spec.execution.llmCredentialsSecret", func() {
			applyManifestAndAssertCredentialsForwarded(
				"creating Secret, Agent, Workflow, and WorkflowRun (opted in via spec, no LLM env)",
				"ottoflow-e2e-wk-*.yaml",
				`apiVersion: v1
kind: Secret
metadata:
  name: e2e-wk-creds
  namespace: ottoflow
type: Opaque
stringData:
  NIRMATA_LLM_TOKEN: "e2e-wellknown-placeholder-token"
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Agent
metadata:
  name: e2e-wk-agent
  namespace: ottoflow
spec:
  prompt: "You are helpful."
  modelProvider: nirmata
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: e2e-wk-workflow
  namespace: ottoflow
spec:
  steps:
    - name: agentstep
      agentRef:
        name: e2e-wk-agent
        namespace: ottoflow
      expressions:
        - name: result
          expression: '"ok"'
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: WorkflowRun
metadata:
  name: e2e-wk-run
  namespace: ottoflow
spec:
  workflowRef:
    name: e2e-wk-workflow
    namespace: ottoflow
  execution:
    llmCredentialsSecret:
      name: e2e-wk-creds
`,
				"e2e-wk-run",
			)
		})
	})

	Context("Agent step with NIRMATA_LLM_TOKEN in job env", func() {
		It("forwards credentials to agent-executor (run reaches terminal state)", func() {
			applyManifestAndAssertCredentialsForwarded(
				"creating Secret, Agent, Workflow, and WorkflowRun with LLM env",
				"ottoflow-e2e-llm-creds-*.yaml",
				`apiVersion: v1
kind: Secret
metadata:
  name: e2e-llm-creds
  namespace: ottoflow
type: Opaque
stringData:
  NIRMATA_LLM_TOKEN: "e2e-placeholder-token"
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Agent
metadata:
  name: e2e-llm-agent-with-creds
  namespace: ottoflow
spec:
  prompt: "You are helpful."
  modelProvider: nirmata
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: e2e-llm-workflow-with-creds
  namespace: ottoflow
spec:
  steps:
    - name: agentstep
      agentRef:
        name: e2e-llm-agent-with-creds
        namespace: ottoflow
      expressions:
        - name: result
          expression: '"ok"'
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: WorkflowRun
metadata:
  name: e2e-llm-run-with-creds
  namespace: ottoflow
spec:
  workflowRef:
    name: e2e-llm-workflow-with-creds
    namespace: ottoflow
  execution:
    job:
      env:
        - name: NIRMATA_LLM_TOKEN
          valueFrom:
            secretKeyRef:
              name: e2e-llm-creds
              key: NIRMATA_LLM_TOKEN
`,
				"e2e-llm-run-with-creds",
			)
		})
	})
})
