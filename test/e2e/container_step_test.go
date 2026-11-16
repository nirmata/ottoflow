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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nirmata/ottoflow/test/utils"
)

var _ = Describe("container step", Ordered, func() {
	It("runs an allowlisted image as a child Job and exposes its stdout", func() {
		manifest := `
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: e2e-container-step
  namespace: ottoflow
spec:
  steps:
    - name: emit
      container:
        image: busybox:1.36
        args: ["sh", "-c", "echo '{\"answer\": 42}'"]
        timeout: 2m
      outputs:
        - name: answer
          expression: "json.unmarshal(step.stdout).answer"
  outputs:
    - name: answer
      expression: "variables.answer"
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: WorkflowRun
metadata:
  name: e2e-container-step-run
  namespace: ottoflow
spec:
  workflowRef:
    name: e2e-container-step
    namespace: ottoflow
`
		f, err := os.CreateTemp("", "ottoflow-e2e-container-*.yaml")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = os.Remove(f.Name()) })
		_, err = f.WriteString(manifest)
		Expect(err).NotTo(HaveOccurred())
		Expect(f.Close()).To(Succeed())

		_, err = utils.Run(exec.Command("kubectl", "apply", "-f", f.Name()))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_, _ = utils.Run(exec.Command("kubectl", "delete", "--ignore-not-found=true", "-f", f.Name()))
		})

		get := func(path string) string {
			out, err := utils.Run(exec.Command("kubectl", "get", "workflowrun", "e2e-container-step-run",
				"-n", namespace, "-o", "jsonpath="+path))
			if err != nil {
				return ""
			}
			return strings.TrimSpace(string(out))
		}
		By("waiting for the WorkflowRun to succeed")
		Eventually(func() string { return get("{.status.phase}") }, 4*time.Minute, 2*time.Second).Should(Equal("Succeeded"),
			func() string { return get("{.status.message}") })
		Expect(get("{.status.outputs.answer}")).To(Equal("42"))

		By("verifying the container step Job was deleted")
		Eventually(func() string {
			out, _ := utils.Run(exec.Command("kubectl", "get", "jobs", "-n", namespace,
				"-l", "ottoflow.nirmata.io/step=emit", "-o", "name"))
			return strings.TrimSpace(string(out))
		}, time.Minute, 2*time.Second).Should(BeEmpty())
	})

	It("rejects an image that is not on the allowlist at admission", func() {
		manifest := `
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: e2e-container-step-denied
  namespace: ottoflow
spec:
  steps:
    - name: emit
      container:
        image: alpine:3.20
`
		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = strings.NewReader(manifest)
		out, err := utils.Run(cmd)
		Expect(err).To(HaveOccurred())
		Expect(string(out) + err.Error()).To(ContainSubstring("allowlist"))
	})
})
