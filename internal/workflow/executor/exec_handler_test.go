/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/agent"
)

// buildExecTestScheme returns a scheme with ottoflow and k8s types registered.
func buildExecTestScheme() *k8sruntime.Scheme {
	s := k8sruntime.NewScheme()
	utilruntime.Must(ottoflowv1alpha1.AddToScheme(s))
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	return s
}

// newExecHandler creates an ExecHandler backed by a fake k8s client and mock executor.
func newExecHandler(agentCRD *ottoflowv1alpha1.Agent, mock *agent.MockAgentExecutor) *ExecHandler {
	k8sClient := fake.NewClientBuilder().WithScheme(buildExecTestScheme()).WithObjects(agentCRD).Build()
	oe := NewOttoFlowAgentExecutorWithAgentExecutor(k8sClient, mock)
	return NewExecHandler(oe)
}

// doExecPOST sends a POST to the handler and returns the recorder.
func doExecPOST(handler http.Handler, path string, body interface{}, llmEnvMap map[string]string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(http.MethodPost, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if len(llmEnvMap) > 0 {
		data, _ := json.Marshal(llmEnvMap)
		req.Header.Set(XLLMEnvHeader, base64.StdEncoding.EncodeToString(data))
	}
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

var _ = Describe("ExecHandler", func() {
	const (
		ns        = "default"
		agentName = "test-agent"
		mockResp  = "analysis complete"
	)

	var (
		agentCRD *ottoflowv1alpha1.Agent
		mock     *agent.MockAgentExecutor
		llmEnv   map[string]string
	)

	BeforeEach(func() {
		agentCRD = &ottoflowv1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: agentName, Namespace: ns},
			Spec: ottoflowv1alpha1.AgentSpec{
				Prompt:        "You are a test agent",
				ModelProvider: "openai",
			},
		}
		mock = agent.NewMockAgentExecutor()
		mock.SetDefaultResponse(mockResp)
		llmEnv = map[string]string{"OPENAI_API_KEY": "test-key"}
	})

	Describe("successful execution", func() {
		It("returns 200 with content", func() {
			h := newExecHandler(agentCRD, mock)
			rr := doExecPOST(h, fmt.Sprintf("/%s/%s", ns, agentName),
				ExecRequest{Prompt: "run test"}, llmEnv)

			Expect(rr.Code).To(Equal(http.StatusOK))
			var resp ExecResponse
			Expect(json.NewDecoder(rr.Body).Decode(&resp)).To(Succeed())
			Expect(resp.Content).To(Equal(mockResp))
		})

		It("forwards context data to ExecuteAgent", func() {
			h := newExecHandler(agentCRD, mock)
			ctx := map[string]interface{}{"key": "value"}
			doExecPOST(h, fmt.Sprintf("/%s/%s", ns, agentName),
				ExecRequest{Prompt: "run test", Context: ctx}, llmEnv)

			history := mock.GetCallHistory()
			Expect(history).To(HaveLen(1))
			Expect(history[0].Context).To(HaveKeyWithValue("key", "value"))
		})
	})

	Describe("Nirmata provider credential validation", func() {
		BeforeEach(func() {
			agentCRD.Spec.ModelProvider = "nirmata"
		})

		It("returns 400 when no LLM token provided", func() {
			h := newExecHandler(agentCRD, mock)
			rr := doExecPOST(h, fmt.Sprintf("/%s/%s", ns, agentName),
				ExecRequest{Prompt: "run test"}, nil)

			Expect(rr.Code).To(Equal(http.StatusBadRequest))
			var errResp ExecErrorResponse
			Expect(json.NewDecoder(rr.Body).Decode(&errResp)).To(Succeed())
			Expect(errResp.Error).To(ContainSubstring("Nirmata LLM credentials required"))
		})

		It("returns 200 when NIRMATA_LLM_TOKEN is present", func() {
			h := newExecHandler(agentCRD, mock)
			rr := doExecPOST(h, fmt.Sprintf("/%s/%s", ns, agentName),
				ExecRequest{Prompt: "run test"},
				map[string]string{"NIRMATA_LLM_TOKEN": "tok"})

			Expect(rr.Code).To(Equal(http.StatusOK))
		})
	})

	Describe("error handling", func() {
		It("returns 405 for non-POST methods", func() {
			h := newExecHandler(agentCRD, mock)
			req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/%s/%s", ns, agentName), nil)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			Expect(rr.Code).To(Equal(http.StatusMethodNotAllowed))
		})

		It("returns 400 for malformed path (single segment)", func() {
			h := newExecHandler(agentCRD, mock)
			rr := doExecPOST(h, "/onlyone", ExecRequest{Prompt: "x"}, llmEnv)
			Expect(rr.Code).To(Equal(http.StatusBadRequest))
		})

		It("returns 400 for malformed JSON body", func() {
			h := newExecHandler(agentCRD, mock)
			req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/%s/%s", ns, agentName),
				strings.NewReader("{invalid json"))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			Expect(rr.Code).To(Equal(http.StatusBadRequest))
		})

		It("returns 404 when agent CRD does not exist", func() {
			h := newExecHandler(agentCRD, mock)
			rr := doExecPOST(h, fmt.Sprintf("/%s/nonexistent", ns),
				ExecRequest{Prompt: "x"}, llmEnv)
			Expect(rr.Code).To(Equal(http.StatusNotFound))
		})

		It("returns 500 when ExecuteAgent fails", func() {
			mock.SetDefaultError(fmt.Errorf("LLM unavailable"))
			h := newExecHandler(agentCRD, mock)
			rr := doExecPOST(h, fmt.Sprintf("/%s/%s", ns, agentName),
				ExecRequest{Prompt: "x"}, llmEnv)

			Expect(rr.Code).To(Equal(http.StatusInternalServerError))
			var errResp ExecErrorResponse
			Expect(json.NewDecoder(rr.Body).Decode(&errResp)).To(Succeed())
			Expect(errResp.Error).To(ContainSubstring("LLM unavailable"))
		})
	})
})

var _ = Describe("ParseLLMEnvHeader", func() {
	It("returns nil for empty header", func() {
		Expect(ParseLLMEnvHeader("")).To(BeNil())
	})

	It("returns nil for invalid base64", func() {
		Expect(ParseLLMEnvHeader("not-base64!!!")).To(BeNil())
	})

	It("returns nil for valid base64 but invalid JSON", func() {
		Expect(ParseLLMEnvHeader(base64.StdEncoding.EncodeToString([]byte("{bad")))).To(BeNil())
	})

	It("decodes a valid header", func() {
		data, _ := json.Marshal(map[string]string{"KEY": "val"})
		result := ParseLLMEnvHeader(base64.StdEncoding.EncodeToString(data))
		Expect(result).To(HaveKeyWithValue("KEY", "val"))
	})
})

var _ = Describe("executeAgentViaExecHTTP", func() {
	const (
		ns        = "default"
		agentName = "exec-agent"
		mockResp  = "exec result"
	)

	var (
		agentCRD    *ottoflowv1alpha1.Agent
		workflowRun *ottoflowv1alpha1.WorkflowRun
		agentRef    *ottoflowv1alpha1.StepAgentRef
		fakeServer  *httptest.Server
		k8sClient   = fake.NewClientBuilder().WithScheme(func() *k8sruntime.Scheme {
			s := k8sruntime.NewScheme()
			utilruntime.Must(ottoflowv1alpha1.AddToScheme(s))
			utilruntime.Must(clientgoscheme.AddToScheme(s))
			return s
		}()).Build()
	)

	BeforeEach(func() {
		agentCRD = &ottoflowv1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: agentName, Namespace: ns},
			Spec: ottoflowv1alpha1.AgentSpec{
				Prompt:        "You are a test agent",
				ModelProvider: "openai",
			},
		}
		workflowRun = &ottoflowv1alpha1.WorkflowRun{
			ObjectMeta: metav1.ObjectMeta{Name: "wf-run", Namespace: ns},
		}
		agentRef = &ottoflowv1alpha1.StepAgentRef{Name: agentName}
	})

	AfterEach(func() {
		if fakeServer != nil {
			fakeServer.Close()
			fakeServer = nil
		}
		testExecURLBase = ""
	})

	It("calls MockAgentExecutor directly without HTTP", func() {
		mockExec := agent.NewMockAgentExecutor()
		mockExec.SetDefaultResponse(mockResp)
		e, err := NewWorkflowExecutorWithAgentExecutor(k8sClient, nil, nil, nil, workflowRun, mockExec, false, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())
		e.controlClient = k8sClient

		response, _, outputs, err := e.executeAgentViaExecHTTP(
			GinkgoT().Context(), workflowRun, agentCRD, agentRef,
			"do something", nil, "svc", "ottoflow")

		Expect(err).To(BeNil())
		Expect(response).To(Equal(mockResp))
		Expect(outputs).To(BeNil())
	})

	It("POSTs to the exec endpoint and returns content", func() {
		expected := ExecResponse{Content: mockResp}
		fakeServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			Expect(r.Method).To(Equal(http.MethodPost))
			Expect(r.URL.Path).To(Equal(fmt.Sprintf("/%s/%s", ns, agentName)))
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(expected)
		}))
		testExecURLBase = fakeServer.URL

		e, err := NewWorkflowExecutorWithAgentExecutor(k8sClient, nil, nil, nil, workflowRun, &execHTTPStub{}, false, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())
		e.controlClient = k8sClient

		response, _, outputs, err := e.executeAgentViaExecHTTP(
			GinkgoT().Context(), workflowRun, agentCRD, agentRef,
			"do something", nil, "svc", "ottoflow")

		Expect(err).To(BeNil())
		Expect(response).To(Equal(mockResp))
		Expect(outputs).To(BeNil())
	})

	It("returns error when exec endpoint returns non-200 with error JSON", func() {
		fakeServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(ExecErrorResponse{Error: "agent crashed"})
		}))
		testExecURLBase = fakeServer.URL

		e, err := NewWorkflowExecutorWithAgentExecutor(k8sClient, nil, nil, nil, workflowRun, &execHTTPStub{}, false, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())
		e.controlClient = k8sClient

		_, _, _, err = e.executeAgentViaExecHTTP(
			GinkgoT().Context(), workflowRun, agentCRD, agentRef,
			"do something", nil, "svc", "ottoflow")

		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("agent crashed"))
	})

	It("sends X-LLM-Env header", func() {
		var capturedHeader string
		fakeServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			capturedHeader = r.Header.Get(XLLMEnvHeader)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(ExecResponse{Content: "ok"})
		}))
		testExecURLBase = fakeServer.URL

		GinkgoT().Setenv("NIRMATA_LLM_TOKEN", "fake-key-for-test")

		e, err := NewWorkflowExecutorWithAgentExecutor(k8sClient, nil, nil, nil, workflowRun, &execHTTPStub{}, false, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())
		e.controlClient = k8sClient

		_, _, _, err = e.executeAgentViaExecHTTP(
			GinkgoT().Context(), workflowRun, agentCRD, agentRef,
			"test", nil, "svc", "ottoflow")

		Expect(err).To(BeNil())
		Expect(capturedHeader).NotTo(BeEmpty())
	})
})

// execHTTPStub is not *agent.MockAgentExecutor so executeAgentViaExecHTTP takes the HTTP path.
type execHTTPStub struct{}

func (execHTTPStub) ExecuteAgent(_ context.Context, _ *ottoflowv1alpha1.Agent, _ string, _ map[string]interface{}, _ string) (string, agent.AgentTokenUsage, error) {
	return "", agent.AgentTokenUsage{}, nil
}

var _ agent.AgentExecutor = (*execHTTPStub)(nil)
