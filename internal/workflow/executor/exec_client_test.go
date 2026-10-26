/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/agent"
)

var _ = Describe("NewOttoFlowAgentExecutor", func() {
	It("defaults the agent executor to RoutingAgentExecutor, not the Nirmata delegate directly", func() {
		// Regression guard: the default construction path must resolve to the
		// router (which dispatches nirmata/empty to the Nirmata delegate and
		// everything else to DefaultAgentExecutor), not straight to the
		// Nirmata delegate — that would silently drop DefaultAgentExecutor.
		exec := NewOttoFlowAgentExecutor(nil, nil)
		Expect(exec.agentExecutor).To(BeAssignableToTypeOf(&agent.RoutingAgentExecutor{}))
	})
})

var _ = Describe("buildExecURL", func() {
	It("uses short-form svc DNS name (no .cluster.local) to match TLS cert SANs", func() {
		url := buildExecURL("", "agent-executor", "ottoflow-system", "default", "my-agent")
		Expect(url).To(Equal("https://agent-executor.ottoflow-system.svc:8443/api/exec/default/my-agent"))
		Expect(url).NotTo(ContainSubstring(".cluster.local"))
	})

	It("uses the test URL base when provided", func() {
		url := buildExecURL("http://localhost:9090/api/exec", "agent-executor", "ottoflow-system", "default", "my-agent")
		Expect(url).To(Equal("http://localhost:9090/api/exec/default/my-agent"))
	})
})

var _ = Describe("isRetryableExecErr", func() {
	It("returns false for context.Canceled", func() {
		Expect(isRetryableExecErr(context.Canceled)).To(BeFalse())
	})

	It("returns false for context.DeadlineExceeded", func() {
		Expect(isRetryableExecErr(context.DeadlineExceeded)).To(BeFalse())
	})

	It("returns false for io.EOF (the request may have reached the server)", func() {
		Expect(isRetryableExecErr(io.EOF)).To(BeFalse())
	})

	It("returns false for io.ErrUnexpectedEOF (the request may have reached the server)", func() {
		Expect(isRetryableExecErr(io.ErrUnexpectedEOF)).To(BeFalse())
	})

	It("returns true for a dial error, including a dial timeout", func() {
		Expect(isRetryableExecErr(&net.OpError{Op: "dial", Err: io.ErrClosedPipe})).To(BeTrue())
		Expect(isRetryableExecErr(&net.OpError{Op: "dial", Err: context.DeadlineExceeded})).To(BeTrue())
	})

	It("returns false for read and write errors (read: reset after send; write: handshake RST or partial send)", func() {
		Expect(isRetryableExecErr(&net.OpError{Op: "read", Err: io.ErrClosedPipe})).To(BeFalse())
		Expect(isRetryableExecErr(&net.OpError{Op: "write", Err: io.ErrClosedPipe})).To(BeFalse())
	})

	It("returns false for nil", func() {
		Expect(isRetryableExecErr(nil)).To(BeFalse())
	})
})

var _ = Describe("executeAgentViaExecHTTP retry", func() {
	var (
		exec         *WorkflowExecutor
		server       *httptest.Server
		origURL      string
		origBaseWait time.Duration

		// minimal args reused across tests
		workflowRun *ottoflowv1alpha1.WorkflowRun
		agentCRD    *ottoflowv1alpha1.Agent
		agentRef    *ottoflowv1alpha1.StepAgentRef
	)

	BeforeEach(func() {
		origURL = testExecURLBase
		origBaseWait = execHTTPRetryBaseWait
		// Use 1ms so retry tests complete in microseconds rather than seconds.
		execHTTPRetryBaseWait = time.Millisecond

		// Use a real (non-mock) AgentExecutor so executeAgentViaExecHTTP
		// falls through to the HTTP path rather than the mock short-circuit.
		exec = &WorkflowExecutor{
			agentExecutor: agent.NewDefaultAgentExecutor(nil),
		}

		workflowRun = &ottoflowv1alpha1.WorkflowRun{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
		}
		agentCRD = &ottoflowv1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "test-agent"},
		}
		agentRef = &ottoflowv1alpha1.StepAgentRef{}
	})

	AfterEach(func() {
		testExecURLBase = origURL
		execHTTPRetryBaseWait = origBaseWait
		if server != nil {
			server.Close()
			server = nil
		}
	})

	It("does not retry a 500: the agent already ran, a retry would run it again", func() {
		var attempts atomic.Int32
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attempts.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(ExecErrorResponse{Error: "agent crashed"})
		}))
		testExecURLBase = server.URL + "/api/exec"

		_, _, _, err := exec.executeAgentViaExecHTTP(
			context.Background(), workflowRun, agentCRD, agentRef,
			"prompt", nil, "agent-executor", "ottoflow-system",
		)

		Expect(err).To(MatchError(ContainSubstring("500: agent crashed")))
		Expect(attempts.Load()).To(Equal(int32(1)))
	})

	It("does not retry when the server closes the connection after receiving the request", func() {
		var attempts atomic.Int32
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attempts.Add(1)
			conn, _, _ := w.(http.Hijacker).Hijack()
			_ = conn.Close()
		}))
		testExecURLBase = server.URL + "/api/exec"

		_, _, _, err := exec.executeAgentViaExecHTTP(
			context.Background(), workflowRun, agentCRD, agentRef,
			"prompt", nil, "agent-executor", "ottoflow-system",
		)

		Expect(err).To(HaveOccurred())
		Expect(attempts.Load()).To(Equal(int32(1)))
	})

	It("retries a dial timeout while the caller context is live", func() {
		// Deterministic dial timeout: dial with an already-expired sub-context. This
		// yields "i/o timeout" (Is context.DeadlineExceeded) with Op=dial, with no
		// dependence on the network or OS.
		var dials atomic.Int32
		exec.execHTTPClient = &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				dials.Add(1)
				dctx, cancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer cancel()
				return (&net.Dialer{}).DialContext(dctx, network, addr)
			},
		}}
		exec.execHTTPClientOnce.Do(func() {})
		testExecURLBase = "http://127.0.0.1:1/api/exec"

		_, _, _, err := exec.executeAgentViaExecHTTP(
			context.Background(), workflowRun, agentCRD, agentRef,
			"prompt", nil, "agent-executor", "ottoflow-system",
		)

		Expect(err).To(MatchError(ContainSubstring("i/o timeout")))
		Expect(dials.Load()).To(Equal(int32(execHTTPMaxRetries)))
	})

	It("does not retry on a 401 — returns error after a single attempt", func() {
		var attempts atomic.Int32
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attempts.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
		}))
		testExecURLBase = server.URL + "/api/exec"

		_, _, _, err := exec.executeAgentViaExecHTTP(
			context.Background(), workflowRun, agentCRD, agentRef,
			"prompt", nil, "agent-executor", "ottoflow-system",
		)

		Expect(err).To(HaveOccurred())
		Expect(attempts.Load()).To(Equal(int32(1)), "4xx must not be retried")
	})

	It("returns error after all attempts are exhausted on persistent connection refused", func() {
		var dials atomic.Int32
		exec.execHTTPClient = &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				dials.Add(1)
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
		}}
		exec.execHTTPClientOnce.Do(func() {})
		testExecURLBase = refusedURL()

		_, _, _, err := exec.executeAgentViaExecHTTP(
			context.Background(), workflowRun, agentCRD, agentRef,
			"prompt", nil, "agent-executor", "ottoflow-system",
		)

		Expect(err).To(MatchError(ContainSubstring("connection refused")))
		Expect(dials.Load()).To(Equal(int32(execHTTPMaxRetries)))
	})

	It("stops retrying immediately when the context is cancelled during backoff", func() {
		// This test needs the backoff sleep to be longer than the context deadline.
		// Override the 1ms test default so the relationship is unambiguous:
		// context expires at 10ms, well inside the 50ms backoff window.
		execHTTPRetryBaseWait = 50 * time.Millisecond
		defer func() { execHTTPRetryBaseWait = time.Millisecond }()

		// Connection refused is retryable, so the loop reaches the backoff.
		var dials atomic.Int32
		exec.execHTTPClient = &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				dials.Add(1)
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
		}}
		exec.execHTTPClientOnce.Do(func() {})
		testExecURLBase = refusedURL()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()

		_, _, _, err := exec.executeAgentViaExecHTTP(
			ctx, workflowRun, agentCRD, agentRef,
			"prompt", nil, "agent-executor", "ottoflow-system",
		)

		Expect(err).To(HaveOccurred())
		Expect(dials.Load()).To(Equal(int32(1)),
			"context cancellation during backoff must stop retrying immediately")
	})
})

// refusedURL returns an exec URL base whose port has no listener (connection refused).
func refusedURL() string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	Expect(err).NotTo(HaveOccurred())
	addr := l.Addr().String()
	Expect(l.Close()).To(Succeed())
	return "http://" + addr + "/api/exec"
}
