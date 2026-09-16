//go:build e2e

/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package e2e

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// A WorkflowRun author controls spec.execution.job.env, which the controller appends after its
// own runner env, and the kubelet keeps the last entry with a given name. OTTOFLOW_SECRET_MOUNTS
// is the controller's map from each approved Secret reference to the file the runner reads it
// from, so an author who could replace it could aim an approved credential reference at any file
// in the runner pod — its own ServiceAccount token included. The controller must refuse such a
// run before it creates anything for it.
var _ = Describe("Reserved runner environment", Label("hardening"), func() {

	const tenant = "e2e-reserved-env"

	BeforeEach(func() {
		ensureNamespace(tenant)
	})

	It("fails a run that sets OTTOFLOW_SECRET_MOUNTS in spec.execution.job.env, before any runner Job exists", func() {
		mustApplyYAML(fmt.Sprintf(`
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: reserved-env-wf
  namespace: %[1]s
spec:
  steps:
    - name: hello
      expressions:
        - name: result
          expression: '"hello"'
`, tenant))
		mustApplyYAML(fmt.Sprintf(`
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: WorkflowRun
metadata:
  name: reserved-env-run
  namespace: %[1]s
spec:
  workflowRef:
    name: reserved-env-wf
    namespace: %[1]s
  execution:
    job:
      env:
        - name: OTTOFLOW_SECRET_MOUNTS
          value: '{"%[1]s/some-secret/token":"/var/run/secrets/kubernetes.io/serviceaccount/token"}'
`, tenant))

		expectRunFailsWith(tenant, "reserved-env-run", "", "OTTOFLOW_SECRET_MOUNTS", 2*time.Minute)

		msg := runMessage(tenant, "reserved-env-run")
		Expect(msg).To(SatisfyAll(
			ContainSubstring("reserved"),
			ContainSubstring("spec.execution.job.env"),
		), "the run failed on the reserved name, but the message does not tell the author what to "+
			"remove. Message: %s", msg)
		Expect(runFailureReason(tenant, "reserved-env-run")).To(BeEmpty(),
			"an author-side spec error is not one of the classified failure reasons")

		// The refusal happens when the Job is built, ahead of every write for the run, so the Job
		// must never have existed — not merely be gone.
		jobName, _ := kubectl("get", "workflowrun", "reserved-env-run", "-n", tenant,
			"-o", "jsonpath={.status.execution.jobName}")
		Expect(jobName).To(BeEmpty(), "status records a runner Job for a run that must never get one")
		out, err := kubectl("get", "job", "reserved-env-run-runner", "-n", tenant)
		Expect(err).To(HaveOccurred(), "a runner Job exists for the refused run: %s", out)
		Expect(out).To(ContainSubstring("NotFound"), "unexpected error looking up the runner Job: %s", out)
	})
})
