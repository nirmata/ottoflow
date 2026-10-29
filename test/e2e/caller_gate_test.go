//go:build e2e

/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package e2e

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nirmata/ottoflow/test/utils"
)

// Caller gates: the chart's caller ClusterRoles must grant what the servers' SubjectAccessReview asks.
// Each pair is minted the same way, so a 401 for the runner identity is attributable to the SAR.
// Unlabelled: the CI e2e workflow runs on push to main and manual dispatch only.
var _ = Describe("Caller gates", Ordered, func() {
	const (
		tenant   = "e2e-caller-gate"
		wfName   = "gate-wf"
		a2aAud   = "ottoflow-a2a"
		a2aProbe = `{"jsonrpc":"2.0","id":1,"method":"e2e/probe"}`
		mcpInit  = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26",` +
			`"capabilities":{},"clientInfo":{"name":"e2e","version":"0"}}}`
	)
	var mcpURL, a2aURL string
	runnerSA := runnerSAFor(wfName)

	BeforeAll(func() {
		ensureNamespace(tenant)
		mustApplyYAML(fmt.Sprintf(`
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: %[2]s
  namespace: %[1]s
spec:
  steps:
    - name: noop
      expressions:
        - name: v
          expression: '"x"'
`, tenant, wfName))
		// The Workflow must outlive the specs: it owns the runner ServiceAccount.
		mustApplyYAML(fmt.Sprintf(`
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: WorkflowRun
metadata:
  name: %[2]s-run
  namespace: %[1]s
spec:
  workflowRef:
    name: %[2]s
`, tenant, wfName))
		Eventually(func() error {
			_, err := kubectl("get", "sa", runnerSA, "-n", tenant)
			return err
		}, 2*time.Minute, 2*time.Second).Should(Succeed())

		mustApplyYAML(fmt.Sprintf(`
apiVersion: v1
kind: ServiceAccount
metadata:
  name: caller
  namespace: %[1]s
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: e2e-gate-mcp-caller
  namespace: %[2]s
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: ottoflow-mcp-caller}
subjects: [{kind: ServiceAccount, name: caller, namespace: %[1]s}]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: e2e-gate-a2a-caller
  namespace: %[1]s
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: ottoflow-a2a-caller}
subjects: [{kind: ServiceAccount, name: caller, namespace: %[1]s}]
`, tenant, namespace))

		mcpURL = "http://" + portForward("svc/ottoflow-mcp", namespace, "8084") + "/mcp"
		a2aURL = "http://" + startServeA2A(tenant, wfName) + "/"
	})

	// expect polls until the server answers with want: RBAC and the port-forward settle asynchronously.
	expect := func(url, token, body string, want int, contains string) {
		GinkgoHelper()
		Eventually(func(g Gomega) {
			req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
			g.Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+token)
			resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
			g.Expect(err).NotTo(HaveOccurred())
			defer func() { _ = resp.Body.Close() }()
			b, _ := io.ReadAll(resp.Body)
			g.Expect(resp.StatusCode).To(Equal(want), "body: %s", b)
			g.Expect(string(b)).To(ContainSubstring(contains))
		}, time.Minute, time.Second).Should(Succeed())
	}

	It("MCP accepts a caller bound to ottoflow-mcp-caller", func() {
		expect(mcpURL, serviceAccountToken(tenant, "caller"), mcpInit, http.StatusOK, `"jsonrpc"`)
	})
	It("MCP refuses the runner ServiceAccount", func() {
		expect(mcpURL, serviceAccountToken(tenant, runnerSA), mcpInit, http.StatusUnauthorized, "")
	})
	It("A2A accepts a caller bound to ottoflow-a2a-caller", func() {
		expect(a2aURL, serviceAccountToken(tenant, "caller", a2aAud), a2aProbe, http.StatusOK, "-32601")
	})
	It("A2A refuses the runner ServiceAccount", func() {
		expect(a2aURL, serviceAccountToken(tenant, runnerSA, a2aAud), a2aProbe, http.StatusUnauthorized, "")
	})
})

var portForwardLine = regexp.MustCompile(`127\.0\.0\.1:(\d+)`)

// portForward forwards target's port to a OS-chosen local port and returns "127.0.0.1:N".
func portForward(target, ns, port string) string {
	GinkgoHelper()
	cmd := exec.Command("kubectl", "port-forward", target, "-n", ns, ":"+port)
	out, err := cmd.StdoutPipe()
	Expect(err).NotTo(HaveOccurred())
	Expect(cmd.Start()).To(Succeed())
	DeferCleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	addr := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if m := portForwardLine.FindStringSubmatch(sc.Text()); m != nil {
				addr <- "127.0.0.1:" + m[1]
				break
			}
		}
		_, _ = io.Copy(io.Discard, out) // keep kubectl from blocking on a full pipe
	}()
	select {
	case a := <-addr:
		return a
	case <-time.After(30 * time.Second):
		Fail("kubectl port-forward " + target + " printed no local address")
		return ""
	}
}

// startServeA2A builds and runs cmd/serve-a2a as a local process against the test kubeconfig and
// returns its address. It runs the real binary's wiring (namespace, audience, resource name); only
// its own TokenReview/SAR identity is the test's, not the chart's token-reviewer role.
func startServeA2A(ns, wf string) string {
	GinkgoHelper()
	bin := filepath.Join(GinkgoT().TempDir(), "serve-a2a")
	_, err := utils.Run(exec.Command("go", "build", "-o", bin, "github.com/nirmata/ottoflow/cmd/serve-a2a"))
	Expect(err).NotTo(HaveOccurred())
	l, err := net.Listen("tcp", "127.0.0.1:0")
	Expect(err).NotTo(HaveOccurred())
	port := fmt.Sprint(l.Addr().(*net.TCPAddr).Port)
	Expect(l.Close()).To(Succeed())
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), "WORKFLOW_NAME="+wf, "WORKFLOW_NAMESPACE="+ns, "PORT="+port)
	cmd.Stdout, cmd.Stderr = GinkgoWriter, GinkgoWriter
	Expect(cmd.Start()).To(Succeed())
	DeferCleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return "127.0.0.1:" + port
}
