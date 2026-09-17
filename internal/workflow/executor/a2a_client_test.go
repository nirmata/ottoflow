/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/secretmount"
)

var _ = Describe("a2aTaskResult.ToMap", func() {
	It("deserializes raw JSON to map", func() {
		r := &a2aTaskResult{raw: json.RawMessage(`{"status":{"state":"completed"},"artifacts":[{"parts":[{"kind":"text","text":"hello"}]}]}`)}
		m, err := r.ToMap()
		Expect(err).NotTo(HaveOccurred())
		Expect(m["status"]).NotTo(BeNil())
	})

	It("returns error for malformed JSON", func() {
		r := &a2aTaskResult{raw: json.RawMessage(`not-json`)}
		_, err := r.ToMap()
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("isTerminalState", func() {
	It("returns true for completed", func() { Expect(isTerminalState("completed")).To(BeTrue()) })
	It("returns true for failed", func() { Expect(isTerminalState("failed")).To(BeTrue()) })
	It("returns true for canceled", func() { Expect(isTerminalState("canceled")).To(BeTrue()) })
	It("returns true for rejected", func() { Expect(isTerminalState("rejected")).To(BeTrue()) })
	It("returns false for working", func() { Expect(isTerminalState("working")).To(BeFalse()) })
	It("returns false for submitted", func() { Expect(isTerminalState("submitted")).To(BeFalse()) })
	It("returns false for input-required", func() { Expect(isTerminalState("input-required")).To(BeFalse()) })
	It("returns false for empty string", func() { Expect(isTerminalState("")).To(BeFalse()) })
})

var _ = Describe("isFailureState", func() {
	It("returns true for failed", func() { Expect(isFailureState("failed")).To(BeTrue()) })
	It("returns true for canceled", func() { Expect(isFailureState("canceled")).To(BeTrue()) })
	It("returns true for rejected", func() { Expect(isFailureState("rejected")).To(BeTrue()) })
	It("returns false for completed", func() { Expect(isFailureState("completed")).To(BeFalse()) })
})

var _ = Describe("isPauseState", func() {
	It("returns true for input-required", func() { Expect(isPauseState("input-required")).To(BeTrue()) })
	It("returns true for auth-required", func() { Expect(isPauseState("auth-required")).To(BeTrue()) })
	It("returns false for working", func() { Expect(isPauseState("working")).To(BeFalse()) })
	It("returns false for completed", func() { Expect(isPauseState("completed")).To(BeFalse()) })
})

var _ = Describe("a2aClient.fetchAgentCard", func() {
	It("probes /.well-known/agent-card.json and succeeds on a 2xx response", func() {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			Expect(r.URL.Path).To(Equal("/.well-known/agent-card.json"))
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"test-agent","url":"http://localhost","preferredTransport":"jsonrpc"}`))
		}))
		defer srv.Close()

		c := &a2aClient{httpClient: srv.Client(), discoveryClient: srv.Client(), baseURL: srv.URL}
		err := c.fetchAgentCard(context.Background())
		Expect(err).NotTo(HaveOccurred())
	})

	It("returns error when agent card endpoint returns non-2xx", func() {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()

		c := &a2aClient{httpClient: srv.Client(), discoveryClient: srv.Client(), baseURL: srv.URL}
		err := c.fetchAgentCard(context.Background())
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("404"))
	})

	It("does not follow a redirect from the agent-card endpoint", func() {
		var redirectTargetHit atomic.Bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == a2aAgentCardPath {
				// Attempt to bounce the discovery probe elsewhere; the client must refuse.
				http.Redirect(w, r, "/elsewhere", http.StatusFound)
				return
			}
			// Reached only if the redirect was (wrongly) followed.
			redirectTargetHit.Store(true)
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		// Build the real discoveryClient (with CheckRedirect) via newA2AClient. srv.URL is a
		// cluster-local http host, so AllowInsecureHTTP permits it; testA2AHTTPClient is nil,
		// so newA2AClient wires its production discovery transport.
		step := &ottoflowv1alpha1.StepExternalAgentRef{URL: srv.URL, AllowInsecureHTTP: true}
		c, err := newA2AClient(context.Background(), step, nil, "default", true)
		Expect(err).NotTo(HaveOccurred())

		err = c.fetchAgentCard(context.Background())
		Expect(err).To(HaveOccurred())                  // 3xx is non-2xx → error
		Expect(err.Error()).To(ContainSubstring("302")) // got the redirect response itself
		Expect(redirectTargetHit.Load()).To(BeFalse())  // redirect target must never be reached
	})
})

var _ = Describe("a2aClient.sendTask", func() {
	It("returns result when task completes immediately", func() {
		var gotMethod string
		var gotWireVersion string
		var gotReq a2aJSONRPCRequest
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotWireVersion = r.Header.Get(a2aWireVersionHeader)
			_ = json.NewDecoder(r.Body).Decode(&gotReq)
			gotMethod = gotReq.Method
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(a2aJSONRPCResponse{
				JSONRPC: "2.0",
				Result:  json.RawMessage(`{"kind":"task","id":"srv-1","status":{"state":"completed"},"artifacts":[{"parts":[{"kind":"text","text":"done"}]}]}`),
				ID:      1,
			})
		}))
		defer srv.Close()

		c := &a2aClient{httpClient: srv.Client(), discoveryClient: srv.Client(), baseURL: srv.URL}
		result, err := c.sendTask(context.Background(), "analyze", 0)
		Expect(err).NotTo(HaveOccurred())
		m, err := result.ToMap()
		Expect(err).NotTo(HaveOccurred())
		Expect(m["id"]).To(Equal("srv-1"))

		// Every JSON-RPC request must advertise the A2A wire version header.
		Expect(gotWireVersion).To(Equal(a2aWireVersion))

		// The request must use the message/send method with a well-formed message envelope.
		Expect(gotMethod).To(Equal(a2aMessageSendMethod))
		params, ok := gotReq.Params.(map[string]interface{})
		Expect(ok).To(BeTrue())
		msg, ok := params["message"].(map[string]interface{})
		Expect(ok).To(BeTrue())
		Expect(msg["kind"]).To(Equal("message"))
		Expect(msg["messageId"]).To(Not(BeEmpty()))
		parts, ok := msg["parts"].([]interface{})
		Expect(ok).To(BeTrue())
		Expect(parts).NotTo(BeEmpty())
		part0, ok := parts[0].(map[string]interface{})
		Expect(ok).To(BeTrue())
		Expect(part0["kind"]).To(Equal("text"))
	})

	It("polls tasks/get with the server-assigned id when the task is non-terminal", func() {
		var callCount atomic.Int32
		var getReq a2aJSONRPCRequest
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := callCount.Add(1)
			w.Header().Set("Content-Type", "application/json")
			if n == 1 {
				// message/send: server assigns id "srv-2" and reports a non-terminal state.
				_ = json.NewEncoder(w).Encode(a2aJSONRPCResponse{
					JSONRPC: "2.0",
					Result:  json.RawMessage(`{"kind":"task","id":"srv-2","status":{"state":"working"}}`),
					ID:      1,
				})
				return
			}
			// tasks/get: capture the requested id, then report completed.
			_ = json.NewDecoder(r.Body).Decode(&getReq)
			_ = json.NewEncoder(w).Encode(a2aJSONRPCResponse{
				JSONRPC: "2.0",
				Result:  json.RawMessage(`{"kind":"task","id":"srv-2","status":{"state":"completed"},"artifacts":[]}`),
				ID:      1,
			})
		}))
		defer srv.Close()

		c := &a2aClient{httpClient: srv.Client(), discoveryClient: srv.Client(), baseURL: srv.URL, pollInterval: 10 * time.Millisecond}
		result, err := c.sendTask(context.Background(), "slow task", 0)
		Expect(err).NotTo(HaveOccurred())
		m, err := result.ToMap()
		Expect(err).NotTo(HaveOccurred())
		Expect(m["id"]).To(Equal("srv-2"))
		Expect(callCount.Load()).To(BeNumerically(">=", 2))

		// tasks/get must poll the SERVER-assigned id, not a client-generated one.
		Expect(getReq.Method).To(Equal(a2aTasksGetMethod))
		getParams, ok := getReq.Params.(map[string]interface{})
		Expect(ok).To(BeTrue())
		Expect(getParams["id"]).To(Equal("srv-2"))
	})

	It("returns result when the response is a message (kind=message)", func() {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(a2aJSONRPCResponse{
				JSONRPC: "2.0",
				Result:  json.RawMessage(`{"kind":"message","role":"agent","parts":[{"kind":"text","text":"hi there"}]}`),
				ID:      1,
			})
		}))
		defer srv.Close()

		c := &a2aClient{httpClient: srv.Client(), discoveryClient: srv.Client(), baseURL: srv.URL}
		result, err := c.sendTask(context.Background(), "greet", 0)
		Expect(err).NotTo(HaveOccurred())
		m, err := result.ToMap()
		Expect(err).NotTo(HaveOccurred())
		Expect(m["kind"]).To(Equal("message"))
		parts, ok := m["parts"].([]interface{})
		Expect(ok).To(BeTrue())
		Expect(parts).NotTo(BeEmpty())

		// The inline message is normalized into the task/artifact shape so that
		// a2aResult.artifacts[0].parts[0].text resolves the same as a task response.
		artifacts, ok := m["artifacts"].([]interface{})
		Expect(ok).To(BeTrue())
		Expect(artifacts).NotTo(BeEmpty())
		artifact0, ok := artifacts[0].(map[string]interface{})
		Expect(ok).To(BeTrue())
		artifactParts, ok := artifact0["parts"].([]interface{})
		Expect(ok).To(BeTrue())
		Expect(artifactParts).NotTo(BeEmpty())
		artifactPart0, ok := artifactParts[0].(map[string]interface{})
		Expect(ok).To(BeTrue())
		Expect(artifactPart0["text"]).To(Equal("hi there"))
	})

	// Non-success terminal states (failed/rejected) and pause states (input-required) all fail
	// the step on the first message/send response — the client must NOT poll in any of these cases.
	DescribeTable("returns an error without polling for non-success terminal and pause states",
		func(state, promptText, expectedErrSubstring string) {
			var callCount atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				callCount.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(a2aJSONRPCResponse{
					JSONRPC: "2.0",
					Result:  json.RawMessage(`{"kind":"task","id":"srv-x","status":{"state":"` + state + `"}}`),
					ID:      1,
				})
			}))
			defer srv.Close()

			c := &a2aClient{httpClient: srv.Client(), discoveryClient: srv.Client(), baseURL: srv.URL, pollInterval: 10 * time.Millisecond}
			_, err := c.sendTask(context.Background(), promptText, 0)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(expectedErrSubstring))
			Expect(callCount.Load()).To(Equal(int32(1))) // terminal/pause on first response — no polling
		},
		Entry("failed", "failed", "broken", "failed"),
		Entry("rejected", "rejected", "nope", "rejected"),
		Entry("input-required (pause)", "input-required", "interactive", "interaction"),
	)

	It("returns error on JSON-RPC error response", func() {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(a2aJSONRPCResponse{
				JSONRPC: "2.0",
				Error:   &a2aRPCError{Code: -32600, Message: "invalid request"},
				ID:      1,
			})
		}))
		defer srv.Close()

		c := &a2aClient{httpClient: srv.Client(), discoveryClient: srv.Client(), baseURL: srv.URL}
		_, err := c.sendTask(context.Background(), "bad", 0)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("JSON-RPC error"))
	})
})

var _ = Describe("newA2AClient URL validation", func() {
	It("rejects http:// URLs", func() {
		step := &ottoflowv1alpha1.StepExternalAgentRef{URL: "http://insecure.example.com"}
		_, err := newA2AClient(context.Background(), step, nil, "default", true)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("HTTPS"))
	})

	It("accepts https:// URLs with no secrets configured", func() {
		step := &ottoflowv1alpha1.StepExternalAgentRef{URL: "https://agent.example.com"}
		client, err := newA2AClient(context.Background(), step, nil, "default", true)
		Expect(err).NotTo(HaveOccurred())
		Expect(client).NotTo(BeNil())
		Expect(client.baseURL).To(Equal("https://agent.example.com"))
	})
})

var _ = Describe("newA2AClient transport gating", func() {
	It("accepts http:// to a cluster-local host when allowInsecureHTTP is set, without a bearer transport", func() {
		step := &ottoflowv1alpha1.StepExternalAgentRef{
			URL:               "http://kagent-controller.kagent.svc:8083",
			AllowInsecureHTTP: true,
		}
		c, err := newA2AClient(context.Background(), step, nil, "default", true)
		Expect(err).NotTo(HaveOccurred())
		Expect(c.baseURL).To(Equal("http://kagent-controller.kagent.svc:8083"))
		_, isBearer := c.httpClient.Transport.(*bearerAuthTransport)
		Expect(isBearer).To(BeFalse())
	})

	It("attaches a bearer transport for https:// with auth.secretRef", func() {
		scheme := runtime.NewScheme()
		Expect(corev1.AddToScheme(scheme)).To(Succeed())
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "tok", Namespace: "default"},
			Data:       map[string][]byte{"token": []byte("s3cr3t")},
		}
		k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
		step := &ottoflowv1alpha1.StepExternalAgentRef{
			URL:  "https://agent.example.com",
			Auth: &ottoflowv1alpha1.ExternalAgentAuth{SecretRef: &ottoflowv1alpha1.SecretReference{Name: "tok", Key: "token"}},
		}
		c, err := newA2AClient(context.Background(), step, k8s, "default", true)
		Expect(err).NotTo(HaveOccurred())
		_, isBearer := c.httpClient.Transport.(*bearerAuthTransport)
		Expect(isBearer).To(BeTrue())
	})
})

var _ = Describe("newA2AClient http:// round-trip", func() {
	It("fetches the agent card and completes a message/send over plaintext http", func() {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == a2aAgentCardPath {
				// Discovery probe: any 2xx JSON body satisfies fetchAgentCard.
				_, _ = w.Write([]byte(`{"name":"plaintext-agent","preferredTransport":"jsonrpc"}`))
				return
			}
			// JSON-RPC endpoint: return a completed task carrying a text artifact.
			_ = json.NewEncoder(w).Encode(a2aJSONRPCResponse{
				JSONRPC: "2.0",
				Result:  json.RawMessage(`{"kind":"task","id":"srv-http","status":{"state":"completed"},"artifacts":[{"parts":[{"kind":"text","text":"plaintext ok"}]}]}`),
				ID:      1,
			})
		}))
		defer srv.Close()

		// srv.URL is http://127.0.0.1:PORT — a cluster-local host, so transport validation
		// permits plaintext http when AllowInsecureHTTP is set. testA2AHTTPClient is nil, so
		// newA2AClient builds and drives its real http transport against the test server.
		step := &ottoflowv1alpha1.StepExternalAgentRef{URL: srv.URL, AllowInsecureHTTP: true}
		c, err := newA2AClient(context.Background(), step, nil, "default", true)
		Expect(err).NotTo(HaveOccurred())

		Expect(c.fetchAgentCard(context.Background())).To(Succeed())

		result, err := c.sendTask(context.Background(), "hello", 0)
		Expect(err).NotTo(HaveOccurred())
		m, err := result.ToMap()
		Expect(err).NotTo(HaveOccurred())

		artifacts, ok := m["artifacts"].([]interface{})
		Expect(ok).To(BeTrue())
		Expect(artifacts).NotTo(BeEmpty())
		artifact0, ok := artifacts[0].(map[string]interface{})
		Expect(ok).To(BeTrue())
		parts, ok := artifact0["parts"].([]interface{})
		Expect(ok).To(BeTrue())
		Expect(parts).NotTo(BeEmpty())
		part0, ok := parts[0].(map[string]interface{})
		Expect(ok).To(BeTrue())
		Expect(part0["text"]).To(Equal("plaintext ok"))
	})
})

// failingGetClient returns a client whose Get always fails the spec — used to prove a
// code path never touches the Secret API.
func failingGetClient() client.Client {
	base := fake.NewClientBuilder().Build()
	return interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			Fail("unexpected Secret API Get for " + key.String() + ": localExecutionMode=false must never call the Secret API")
			return nil
		},
	})
}

// generateTestCAPEM returns a minimal self-signed CA certificate in PEM form, suitable for
// x509.CertPool.AppendCertsFromPEM.
func generateTestCAPEM() []byte {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	Expect(err).NotTo(HaveOccurred())
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test-ca"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		IsCA:         true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	Expect(err).NotTo(HaveOccurred())
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

var _ = Describe("newA2AClient secret access (localExecutionMode=false, in-cluster runner)", func() {
	BeforeEach(func() {
		// secretmount.Load() memoizes OTTOFLOW_SECRET_MOUNTS for the process lifetime (it
		// never changes in the real runner process); each spec below sets it to a different
		// value, so reset the memoization before every case.
		secretmount.ResetForTest()
	})

	It("reads the CA bundle and bearer token from mounted files without calling the Secret API", func() {
		dir := GinkgoT().TempDir()
		caPath := filepath.Join(dir, "ca.crt")
		tokenPath := filepath.Join(dir, "token")
		Expect(os.WriteFile(caPath, generateTestCAPEM(), 0o600)).To(Succeed())
		Expect(os.WriteFile(tokenPath, []byte("mounted-token\n"), 0o600)).To(Succeed())

		mounts := secretmount.Mounts{
			secretmount.Key("default", "agent-ca", "ca.crt"):   caPath,
			secretmount.Key("default", "agent-token", "token"): tokenPath,
		}
		raw, err := json.Marshal(mounts)
		Expect(err).NotTo(HaveOccurred())
		GinkgoT().Setenv(secretmount.EnvVar, string(raw))

		step := &ottoflowv1alpha1.StepExternalAgentRef{
			URL:         "https://agent.example.com",
			CASecretRef: &ottoflowv1alpha1.NamespacedSecretRef{Name: "agent-ca"},
			Auth: &ottoflowv1alpha1.ExternalAgentAuth{
				SecretRef: &ottoflowv1alpha1.SecretReference{Name: "agent-token", Key: "token"},
			},
		}

		c, err := newA2AClient(context.Background(), step, failingGetClient(), "default", false)
		Expect(err).NotTo(HaveOccurred())
		Expect(c).NotTo(BeNil())

		bearer, isBearer := c.httpClient.Transport.(*bearerAuthTransport)
		Expect(isBearer).To(BeTrue())
		Expect(bearer.token).To(Equal("mounted-token"))

		// The mounted CA must have landed in the TLS config, not just been read.
		inner, ok := bearer.transport.(*http.Transport)
		Expect(ok).To(BeTrue())
		Expect(inner.TLSClientConfig.RootCAs).NotTo(BeNil())
	})

	It("errors without calling the Secret API when the CA mount is missing", func() {
		GinkgoT().Setenv(secretmount.EnvVar, `{}`)

		step := &ottoflowv1alpha1.StepExternalAgentRef{
			URL:         "https://agent.example.com",
			CASecretRef: &ottoflowv1alpha1.NamespacedSecretRef{Name: "agent-ca"},
		}

		_, err := newA2AClient(context.Background(), step, failingGetClient(), "default", false)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("loading CA secret default/agent-ca"))
		Expect(err.Error()).To(ContainSubstring("not mounted"))
	})

	It("errors without calling the Secret API when the bearer token mount is missing", func() {
		GinkgoT().Setenv(secretmount.EnvVar, `{}`)

		step := &ottoflowv1alpha1.StepExternalAgentRef{
			URL: "https://agent.example.com",
			Auth: &ottoflowv1alpha1.ExternalAgentAuth{
				SecretRef: &ottoflowv1alpha1.SecretReference{Name: "agent-token", Key: "token"},
			},
		}

		_, err := newA2AClient(context.Background(), step, failingGetClient(), "default", false)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("loading bearer token from secret"))
		Expect(err.Error()).To(ContainSubstring("not mounted"))
	})

	It("reads the bearer token through the Secret API in local execution mode when nothing is mounted", func() {
		GinkgoT().Setenv(secretmount.EnvVar, `{}`)
		scheme := runtime.NewScheme()
		Expect(corev1.AddToScheme(scheme)).To(Succeed())
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "tok", Namespace: "default"},
			Data:       map[string][]byte{"token": []byte("api-token")},
		}
		k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
		step := &ottoflowv1alpha1.StepExternalAgentRef{
			URL:  "https://agent.example.com",
			Auth: &ottoflowv1alpha1.ExternalAgentAuth{SecretRef: &ottoflowv1alpha1.SecretReference{Name: "tok", Key: "token"}},
		}
		c, err := newA2AClient(context.Background(), step, k8s, "default", true)
		Expect(err).NotTo(HaveOccurred())
		bearer, isBearer := c.httpClient.Transport.(*bearerAuthTransport)
		Expect(isBearer).To(BeTrue())
		Expect(bearer.token).To(Equal("api-token"))
	})
})
