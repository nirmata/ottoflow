//go:build e2e

/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package e2e

import (
	"encoding/base64"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Runner Secret delivery by mount.
//
// The workflow-runner Job holds no Secret RBAC at all, yet three step shapes need a
// Secret value at execution time: an MCPServer's auth credential (mcpToolCall), the
// kubeconfig behind spec.clusterRef.kubeConfigSecretRef, and an externalAgentRef's CA
// bundle and bearer token. The controller resolves each reference into a Secret volume
// under /etc/ottoflow/secrets and the runner reads the file; it never calls the Secret
// API (internal/secretmount).
//
// The zero-grant specs cannot see this path: a Secret that does not exist stops the pod
// at FailedMount before the runner starts, and a controller-side denial stops the run
// before a Job exists. Every spec here therefore uses a Secret that EXISTS, drives the
// runner all the way to the point where it needs the value, and then asserts two things
// at once: the value was used, and the runner's own ServiceAccount could not have read
// it. A runner that regressed to reading Secrets through the API would fail each of them
// with `secrets "<name>" is forbidden` — exactly the substring the assertions rule out.
var _ = Describe("Runner Secret delivery by mount", Label("hardening"), func() {

	// mcpCallerClusterRole is the chart's "may invoke workflows over MCP" ClusterRole for the
	// default release name; mcpEndpoint is the controller's MCP Service, enabled for the e2e
	// install by test/e2e/helm-values-e2e.yaml.
	const (
		mcpCallerClusterRole = "ottoflow-mcp-caller"
		mcpEndpoint          = "http://ottoflow-mcp." + namespace + ".svc:8084/mcp"
	)

	It("delivers an MCPServer bearer token to a mcpToolCall step through a mounted file", func() {
		const tenant = "e2e-mount-mcp"
		ensureNamespace(tenant)

		// The target is OttoFlow's own MCP endpoint, which authenticates every request with a
		// TokenReview and then asks, via SubjectAccessReview, whether that identity may `get`
		// the mcp-caller ConfigMap in the install namespace. So the bearer token has to be a
		// real ServiceAccount token that carries that permission: a token the server ignored,
		// or one the runner sent wrongly, fails the call. Success is therefore proof that the
		// exact bytes in the Secret travelled Secret -> mount -> runner -> Authorization header.
		mustApplyYAML(fmt.Sprintf(`
apiVersion: v1
kind: ServiceAccount
metadata:
  name: mcp-caller
  namespace: %[1]s
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: e2e-mcp-caller-%[1]s
  namespace: %[2]s
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: %[3]s
subjects:
  - kind: ServiceAccount
    name: mcp-caller
    namespace: %[1]s
`, tenant, namespace, mcpCallerClusterRole))
		mustCreateSecret(tenant, "mcp-token", map[string]string{
			"token": serviceAccountToken(tenant, "mcp-caller"),
		})

		// The exposed workflow is the tool the caller invokes; its output is the value that
		// must round-trip back through the tool result.
		mustApplyYAML(fmt.Sprintf(`
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: exposed-wf
  namespace: %[1]s
spec:
  mcpTool:
    enabled: true
    description: "Returns a fixed greeting."
  steps:
    - name: greet
      expressions:
        - name: greeting
          expression: '"hello from the exposed workflow"'
  outputs:
    - name: greeting
      expression: expressions.greeting
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: MCPServer
metadata:
  name: ottoflow-self
  namespace: %[1]s
spec:
  transport:
    type: http
    address: %[2]s
  auth:
    type: bearer
    secretRef:
      name: mcp-token
      key: token
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: mcp-caller-wf
  namespace: %[1]s
spec:
  steps:
    - name: call
      mcpToolCall:
        server: ottoflow-self
        tool: %[1]s__exposed-wf
  outputs:
    # A mcpToolCall step without its own outputs stores the tool result under the key
    # "result", and step outputs land in variables.*, not steps.<name>.*.
    - name: callResult
      expression: variables.result
`, tenant, mcpEndpoint))
		// The run goes in its own apply, after the Workflow is admitted (see mustApplyYAML).
		mustApplyYAML(fmt.Sprintf(`
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: WorkflowRun
metadata:
  name: mcp-caller-run
  namespace: %[1]s
spec:
  workflowRef:
    name: mcp-caller-wf
    namespace: %[1]s
`, tenant))

		expectRunSucceeds(tenant, "mcp-caller-run", 5*time.Minute)

		// The tool result is the MCP server's own record of the run it created: the callee
		// WorkflowRun's name and its outputs. Both must be there, or the call did not actually
		// run the workflow.
		Expect(runOutput(tenant, "mcp-caller-run", "callResult")).To(SatisfyAll(
			ContainSubstring(`"workflowRun":"exposed-wf-`),
			ContainSubstring("hello from the exposed workflow"),
		), "the mcpToolCall step succeeded but its result does not carry the callee WorkflowRun and "+
			"the exposed workflow's output, so the tool call did not actually run the workflow.%s",
			runDiagnostics(tenant, "mcp-caller-run"))

		Expect(runnerJobVolumes(tenant, "mcp-caller-run")).To(SatisfyAll(
			ContainSubstring(`"secretName":"mcp-token"`),
			ContainSubstring("ottoflow-secret-"),
		), "the runner Job carries no controller-minted Secret volume for the MCPServer's auth "+
			"Secret; the token must reach the runner as a mounted file, not through the API")

		Expect(canI("get", "secrets/mcp-token", tenant, tenant, runnerSAFor("mcp-caller-wf"))).To(Equal("no"),
			"the runner ServiceAccount can read the auth Secret through the API, so this spec no "+
				"longer proves the token came from the mount")
	})

	It("delivers the clusterRef kubeconfig to the runner through a mounted file", func() {
		const tenant = "e2e-mount-kubeconfig"
		ensureNamespace(tenant)

		// The tenant's own identity for the target cluster: a ServiceAccount that may list
		// Secrets in this namespace. The runner's ServiceAccount may not (asserted below), so a
		// run that returns these Secret NAMES can only have done so with the identity in the
		// mounted kubeconfig.
		mustApplyYAML(fmt.Sprintf(`
apiVersion: v1
kind: ServiceAccount
metadata:
  name: target-reader
  namespace: %[1]s
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: target-reader
  namespace: %[1]s
rules:
  - apiGroups: [""]
    resources: ["secrets"]
    verbs: ["get", "list"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: target-reader
  namespace: %[1]s
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: target-reader
subjects:
  - kind: ServiceAccount
    name: target-reader
    namespace: %[1]s
---
apiVersion: v1
kind: Secret
metadata:
  name: marker-secret-1
  namespace: %[1]s
  labels:
    e2e-marker: "true"
type: Opaque
stringData: {k: v}
---
apiVersion: v1
kind: Secret
metadata:
  name: marker-secret-2
  namespace: %[1]s
  labels:
    e2e-marker: "true"
type: Opaque
stringData: {k: v}
`, tenant))

		// Every namespace carries the cluster CA in kube-root-ca.crt; with a token for
		// target-reader that is a complete kubeconfig for the API server the runner already
		// sits next to.
		caPEM, err := kubectl("get", "configmap", "kube-root-ca.crt", "-n", tenant, "-o", `jsonpath={.data.ca\.crt}`)
		Expect(err).NotTo(HaveOccurred(), caPEM)
		Expect(caPEM).To(ContainSubstring("BEGIN CERTIFICATE"))
		kubeconfig := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
  - name: target
    cluster:
      server: https://kubernetes.default.svc
      certificate-authority-data: %s
users:
  - name: target-reader
    user:
      token: %s
contexts:
  - name: target
    context:
      cluster: target
      user: target-reader
current-context: target
`, base64.StdEncoding.EncodeToString([]byte(caPEM)), serviceAccountToken(tenant, "target-reader"))
		mustCreateSecret(tenant, "target-kubeconfig", map[string]string{"kubeconfig": kubeconfig})

		mustApplyYAML(fmt.Sprintf(`
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: kubeconfig-wf
  namespace: %[1]s
spec:
  steps:
    - name: listSecretNames
      resourceQuery:
        apiVersion: v1
        resource: Secret
        namespace: '"%[1]s"'
        labelSelector:
          e2e-marker: '"true"'
        outputs:
          secretNames: items.map(i, i.metadata.name)
  outputs:
    - name: secretNames
      expression: variables.secretNames
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: WorkflowRun
metadata:
  name: kubeconfig-run
  namespace: %[1]s
spec:
  workflowRef:
    name: kubeconfig-wf
    namespace: %[1]s
  clusterRef:
    kubeConfigSecretRef:
      name: target-kubeconfig
      key: kubeconfig
`, tenant))

		expectRunSucceeds(tenant, "kubeconfig-run", 4*time.Minute)

		Expect(runOutput(tenant, "kubeconfig-run", "secretNames")).To(SatisfyAll(
			ContainSubstring("marker-secret-1"),
			ContainSubstring("marker-secret-2"),
		), "the run succeeded but did not list the marker Secrets through the mounted kubeconfig's "+
			"identity.%s", runDiagnostics(tenant, "kubeconfig-run"))

		// The decisive half: the runner's own identity could not have produced that list.
		Expect(canI("list", "secrets", tenant, tenant, runnerSAFor("kubeconfig-wf"))).To(Equal("no"),
			"the runner ServiceAccount can list Secrets in %q, so this spec no longer proves the "+
				"kubeconfig identity was used", tenant)

		Expect(runnerJobVolumes(tenant, "kubeconfig-run")).To(ContainSubstring(`"secretName":"target-kubeconfig"`),
			"the runner Job carries no Secret volume for the kubeconfig; it must reach the runner as "+
				"a mounted file, not through the API")
	})

	It("delivers an externalAgentRef CA bundle and bearer token to the runner through mounted files", func() {
		const tenant = "e2e-mount-a2a"
		ensureNamespace(tenant)

		// The target is the agent-executor, the one TLS-only endpoint every install has. It
		// speaks no A2A, so the step is expected to FAIL — but at the HTTP layer, with the
		// status the agent-card probe got back. Reaching that layer at all requires a completed
		// TLS handshake against the CA bundle read from the mounted Secret, and the bearer token
		// is resolved from its mount before the first request is built. A runner that could not
		// read either would fail earlier and differently: with a Secret error (regression to API
		// reads) or an x509 error (CA not applied). Both are ruled out below.
		caB64, err := kubectl("get", "secret", agentExecutorCASecretDefault, "-n", namespace, "-o", `jsonpath={.data.tls\.crt}`)
		Expect(err).NotTo(HaveOccurred(), caB64)
		caPEM, err := base64.StdEncoding.DecodeString(caB64)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(caPEM)).To(ContainSubstring("BEGIN CERTIFICATE"))
		mustCreateSecret(tenant, "a2a-ca", map[string]string{"ca.crt": string(caPEM)})
		mustCreateSecret(tenant, "a2a-token", map[string]string{"token": "e2e-a2a-bearer-placeholder"})

		mustApplyYAML(fmt.Sprintf(`
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: a2a-wf
  namespace: %[1]s
spec:
  steps:
    - name: probe
      externalAgentRef:
        url: https://%[2]s.%[3]s.svc:8443
        protocol: a2a
        prompt: '"ping"'
        timeout: 30s
        auth:
          secretRef:
            name: a2a-token
            key: token
        caSecretRef:
          name: a2a-ca
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: WorkflowRun
metadata:
  name: a2a-run
  namespace: %[1]s
spec:
  workflowRef:
    name: a2a-wf
    namespace: %[1]s
`, tenant, agentExecutorDeploymentDefault, namespace))

		expectRunFailsWith(tenant, "a2a-run", "probe", "agent card fetch returned", 4*time.Minute)

		combined := runMessage(tenant, "a2a-run") + " || " + stepMessage(tenant, "a2a-run", "probe")
		Expect(combined).To(SatisfyAll(
			Not(ContainSubstring("x509")),
			Not(ContainSubstring("forbidden")),
			Not(ContainSubstring("loading CA secret")),
			Not(ContainSubstring("loading bearer token")),
			Not(ContainSubstring("no valid PEM")),
		), "the step failed before the TLS handshake completed, so the CA bundle or the bearer token "+
			"was not read from its mount. Message: %s", combined)

		Expect(runnerJobVolumes(tenant, "a2a-run")).To(SatisfyAll(
			ContainSubstring(`"secretName":"a2a-ca"`),
			ContainSubstring(`"secretName":"a2a-token"`),
		), "the runner Job carries no Secret volume for the CA bundle or the bearer token; they must "+
			"reach the runner as mounted files, not through the API")

		Expect(canI("get", "secrets/a2a-ca", tenant, tenant, runnerSAFor("a2a-wf"))).To(Equal("no"),
			"the runner ServiceAccount can read the CA Secret through the API, so this spec no "+
				"longer proves the bundle came from the mount")
	})
})
