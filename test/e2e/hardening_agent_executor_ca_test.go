//go:build e2e

/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package e2e

import (
	"encoding/json"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Agent-executor CA delivery.
//
// A runner that calls the agent-executor must verify its internal TLS certificate, so it
// needs the CA. In a tenant namespace the controller holds no Secret read and no Secret
// create, so the CA cannot arrive as a Secret copy: the controller publishes the
// CERTIFICATE ONLY, as a ConfigMap named after the CA Secret, and the runner mounts that.
// A workflow with no agent step never talks to the agent-executor and gets no CA at all.
//
// These specs pin both halves against a tenant namespace with no grants of any kind.
var _ = Describe("Agent-executor CA delivery", Label("hardening"), func() {

	const tenant = "e2e-ca-delivery"

	BeforeEach(func() {
		ensureNamespace(tenant)
	})

	It("publishes the CA certificate as a ConfigMap and runs an agent-step workflow with no Secret grant", func() {
		// The Agent has no LLM credentials on purpose: the agent-executor answers with an
		// HTTP-level error, which is all this spec needs from it — an HTTP status can only
		// arrive after the TLS handshake against the mounted CA has succeeded. failurePolicy
		// Continue lets the run finish Succeeded on the step after it.
		mustApplyYAML(fmt.Sprintf(`
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Agent
metadata:
  name: ca-probe-agent
  namespace: %[1]s
spec:
  prompt: "You are a probe."
  modelProvider: openai
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: ca-agent-wf
  namespace: %[1]s
spec:
  steps:
    - name: askAgent
      failurePolicy: Continue
      agentRef:
        name: ca-probe-agent
    - name: after
      dependsOn: [askAgent]
      expressions:
        - name: msg
          expression: '"reached the step after the agent step"'
`, tenant))
		// The run goes in its own apply, after the Workflow is admitted (see mustApplyYAML).
		mustApplyYAML(fmt.Sprintf(`
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: WorkflowRun
metadata:
  name: ca-agent-run
  namespace: %[1]s
spec:
  workflowRef:
    name: ca-agent-wf
    namespace: %[1]s
`, tenant))

		expectRunSucceeds(tenant, "ca-agent-run", 4*time.Minute)

		Expect(stepMessage(tenant, "ca-agent-run", "askAgent")).To(ContainSubstring("exec endpoint returned"),
			"the agent step did not get an HTTP-level answer from the agent-executor, so the TLS "+
				"handshake against the published CA cannot be confirmed.%s", runDiagnostics(tenant, "ca-agent-run"))

		By("checking the published ConfigMap holds only the certificate")
		rawData, err := kubectl("get", "configmap", agentExecutorCASecretDefault, "-n", tenant, "-o", "jsonpath={.data}")
		Expect(err).NotTo(HaveOccurred(), "the CA ConfigMap %s/%s was not published: %s", tenant, agentExecutorCASecretDefault, rawData)
		var data map[string]string
		Expect(json.Unmarshal([]byte(rawData), &data)).To(Succeed(), rawData)
		Expect(data).To(HaveLen(1), "the CA ConfigMap must carry exactly one key; got %v", data)
		Expect(data).To(HaveKey("ca.crt"))
		Expect(data["ca.crt"]).To(ContainSubstring("BEGIN CERTIFICATE"))
		Expect(data["ca.crt"]).NotTo(ContainSubstring("PRIVATE KEY"),
			"the CA ConfigMap carries a private key; only the certificate may leave the install namespace")

		binaryData, _ := kubectl("get", "configmap", agentExecutorCASecretDefault, "-n", tenant, "-o", "jsonpath={.binaryData}")
		Expect(binaryData).To(BeEmpty(), "the CA ConfigMap must not carry binaryData; got %s", binaryData)

		label, _ := kubectl("get", "configmap", agentExecutorCASecretDefault, "-n", tenant,
			"-o", `jsonpath={.metadata.labels.app\.kubernetes\.io/part-of}`)
		Expect(label).To(Equal("ottoflow"),
			"the published CA ConfigMap must carry %s so the controller can tell its own copy from a "+
				"same-named object it must not overwrite", partOfLabel)

		By("checking no Secret carried the CA into the tenant namespace")
		out, err := kubectl("get", "secret", agentExecutorCASecretDefault, "-n", tenant)
		Expect(err).To(HaveOccurred(),
			"a Secret named %s exists in tenant namespace %q; the CA must reach a tenant as a ConfigMap, "+
				"never as a Secret copy: %s", agentExecutorCASecretDefault, tenant, out)
		Expect(canI("get", "secrets/"+agentExecutorCASecretDefault, tenant, namespace, controllerServiceAccount)).To(Equal("no"),
			"the controller can read a CA Secret in the tenant namespace; the run above must have "+
				"succeeded without any such grant")

		By("checking the runner mounted the ConfigMap, not a Secret")
		vols := runnerJobVolumes(tenant, "ca-agent-run")
		Expect(vols).To(SatisfyAll(
			ContainSubstring(`"name":"agent-executor-ca"`),
			ContainSubstring(`"configMap":{`),
			ContainSubstring(fmt.Sprintf(`"name":%q`, agentExecutorCASecretDefault)),
			Not(ContainSubstring(fmt.Sprintf(`"secretName":%q`, agentExecutorCASecretDefault))),
		), "the runner Job's CA volume is not a ConfigMap volume named after the CA Secret: %s", vols)
	})

	It("mounts no CA and publishes nothing for a workflow without an agent step", func() {
		mustApplyYAML(fmt.Sprintf(`
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: ca-agentless-wf
  namespace: %[1]s
spec:
  steps:
    - name: hello
      expressions:
        - name: msg
          expression: '"no agent step here"'
  outputs:
    - name: msg
      expression: expressions.msg
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: WorkflowRun
metadata:
  name: ca-agentless-run
  namespace: %[1]s
spec:
  workflowRef:
    name: ca-agentless-wf
    namespace: %[1]s
`, tenant))

		expectRunSucceeds(tenant, "ca-agentless-run", 3*time.Minute)
		Expect(runOutput(tenant, "ca-agentless-run", "msg")).To(ContainSubstring("no agent step here"))

		vols := runnerJobVolumes(tenant, "ca-agentless-run")
		Expect(vols).To(SatisfyAll(
			Not(ContainSubstring("agent-executor-ca")),
			Not(ContainSubstring(agentExecutorCASecretDefault)),
		), "a workflow with no agent step must not mount the agent-executor CA at all: %s", vols)

		out, err := kubectl("get", "configmap", agentExecutorCASecretDefault, "-n", tenant)
		Expect(err).To(HaveOccurred(),
			"the CA ConfigMap was published into %q although nothing in the run needed it: %s", tenant, out)
	})

	It("refuses a same-named ConfigMap it does not own, leaves it untouched, and starts no runner Job", func() {
		// A tenant who can write ConfigMaps in their own namespace can pre-create one under the
		// name the controller publishes to. If the controller adopted it, the runner would trust
		// whatever certificate the tenant put there and present its ServiceAccount token to any
		// server holding the matching key. The controller's own copy carries the part-of label
		// (first spec above); an unlabelled one must fail the run before any runner Job exists,
		// and must be left exactly as the tenant wrote it, because overwriting it would also
		// overwrite an object some other operator legitimately owns.
		const marker = "NOT-THE-AGENT-EXECUTOR-CA"
		mustApplyYAML(fmt.Sprintf(`
apiVersion: v1
kind: ConfigMap
metadata:
  name: %[1]s
  namespace: %[2]s
data:
  ca.crt: |
    -----BEGIN CERTIFICATE-----
    %[3]s
    -----END CERTIFICATE-----
`, agentExecutorCASecretDefault, tenant, marker))

		mustApplyYAML(fmt.Sprintf(`
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Agent
metadata:
  name: ca-hostile-agent
  namespace: %[1]s
spec:
  prompt: "You are a probe."
  modelProvider: openai
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: ca-hostile-wf
  namespace: %[1]s
spec:
  steps:
    - name: askAgent
      agentRef:
        name: ca-hostile-agent
`, tenant))
		// The run goes in its own apply, after the Workflow is admitted (see mustApplyYAML).
		mustApplyYAML(fmt.Sprintf(`
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: WorkflowRun
metadata:
  name: ca-hostile-run
  namespace: %[1]s
spec:
  workflowRef:
    name: ca-hostile-wf
    namespace: %[1]s
`, tenant))

		expectRunFailsWith(tenant, "ca-hostile-run", "", "not managed by OttoFlow", 3*time.Minute)

		By("checking the tenant's ConfigMap was left untouched")
		data, err := kubectl("get", "configmap", agentExecutorCASecretDefault, "-n", tenant, "-o", `jsonpath={.data.ca\.crt}`)
		Expect(err).NotTo(HaveOccurred(), data)
		Expect(data).To(ContainSubstring(marker),
			"the controller overwrote a ConfigMap it does not own; refusing to trust it and taking it over "+
				"are different behaviours, and only the first is safe")
		label, _ := kubectl("get", "configmap", agentExecutorCASecretDefault, "-n", tenant,
			"-o", `jsonpath={.metadata.labels.app\.kubernetes\.io/part-of}`)
		Expect(label).To(BeEmpty(), "the controller labelled a ConfigMap it does not own as its own")

		By("checking no runner Job was created")
		// Merely asserting the run Failed would also pass if a Job had started, mounted the
		// tenant's certificate, and failed later; the run must never have had a Job at all.
		jobName, _ := kubectl("get", "workflowrun", "ca-hostile-run", "-n", tenant,
			"-o", "jsonpath={.status.execution.jobName}")
		Expect(jobName).To(BeEmpty(), lazyRunDiagnostics(tenant, "ca-hostile-run",
			"a runner Job was recorded for a run whose CA was untrusted."))
		// -o name, not a table: for an empty list kubectl prints "No resources found" to stderr
		// in table mode, and the helper returns stdout and stderr combined.
		jobs := kubectlIgnoreErr("get", "jobs", "-n", tenant,
			"-l", "ottoflow.nirmata.io/workflowrun=ca-hostile-run", "-o", "name")
		Expect(jobs).To(BeEmpty(), "a runner Job exists for a run whose CA was untrusted: %s", jobs)
	})
})
