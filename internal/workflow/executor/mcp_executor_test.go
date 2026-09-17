/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/agent"
	"github.com/nirmata/ottoflow/internal/secretmount"
)

// mockMCPClient implements agent.MCPClient for tests
type mockMCPClient struct {
	callToolResult interface{}
	callToolErr    error
}

func (m *mockMCPClient) CallTool(ctx context.Context, toolName string, arguments map[string]interface{}) (interface{}, error) {
	if m.callToolErr != nil {
		return nil, m.callToolErr
	}
	return m.callToolResult, nil
}

func (m *mockMCPClient) ListTools(ctx context.Context) ([]agent.MCPToolMeta, error) {
	return nil, nil
}

func (m *mockMCPClient) Close() error { return nil }

// mockMCPManager returns a fixed MCP client
type mockMCPManager struct {
	client agent.MCPClient
}

func (m *mockMCPManager) GetClient(ctx context.Context, serverName string, namespace string) (agent.MCPClient, error) {
	return m.client, nil
}

func (m *mockMCPManager) Close() error { return nil }

var _ = Describe("MCPManager", func() {
	It("NewMCPManager and GetClient can be called", func() {
		scheme := runtime.NewScheme()
		utilruntime.Must(ottoflowv1alpha1.AddToScheme(scheme))
		utilruntime.Must(clientgoscheme.AddToScheme(scheme))
		k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
		manager, err := NewMCPManager(k8sClient, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(manager).NotTo(BeNil())
		defer manager.Close() //nolint:errcheck
		ctx := context.Background()
		_, err = manager.GetClient(ctx, "test-server", "default")
		// May succeed or fail depending on MCPServer CRD; we only care that the path is exercised
		_ = err
	})
})

var _ = Describe("MCP Tool Call Step Execution", func() {
	var (
		ctx              context.Context
		k8sClient        client.Client
		scheme           *runtime.Scheme
		workflowRun      *ottoflowv1alpha1.WorkflowRun
		workflowExecutor *WorkflowExecutor
		mockMCP          *mockMCPManager
	)

	BeforeEach(func() {
		ctx = context.Background()
		scheme = runtime.NewScheme()
		utilruntime.Must(ottoflowv1alpha1.AddToScheme(scheme))
		utilruntime.Must(clientgoscheme.AddToScheme(scheme))
		workflowRun = &ottoflowv1alpha1.WorkflowRun{
			ObjectMeta: metav1.ObjectMeta{Name: "test-run", Namespace: "default"},
			Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "test-wf"}},
		}
		k8sClient = fake.NewClientBuilder().WithScheme(scheme).Build()
	})

	It("should execute MCP tool call and write result to step outputs", func() {
		mockMCP = &mockMCPManager{
			client: &mockMCPClient{
				callToolResult: map[string]interface{}{"output": "hello from tool", "count": float64(42)},
			},
		}
		var err error
		workflowExecutor, err = NewWorkflowExecutorWithAgentExecutorAndMCPManager(
			k8sClient, nil, nil, nil, workflowRun, agent.NewMockAgentExecutor(), mockMCP, false, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())

		workflow := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "test-wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{
						Name: "mcpStep",
						MCPToolCall: &ottoflowv1alpha1.StepMCPToolCall{
							Server: "my-server",
							Tool:   "echo",
							Arguments: map[string]string{
								"message": `"hello"`,
							},
						},
						Outputs: []ottoflowv1alpha1.Output{
							{Name: "out", Expression: "toolResult.output"},
							{Name: "n", Expression: "int(toolResult.count)"},
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, workflow)).To(Succeed())

		err = workflowExecutor.ExecuteWorkflow(ctx, workflow, workflowRun)
		Expect(err).NotTo(HaveOccurred())
		Expect(workflowRun.Status.Phase).To(Equal(ottoflowv1alpha1.WorkflowRunPhaseSucceeded))
		Expect(workflowRun.Status.StepStatuses["mcpStep"].Phase).To(Equal(ottoflowv1alpha1.StepPhaseSucceeded))

		ctxData, err := workflowExecutor.GetContextManager().ReadContext(ctx)
		Expect(err).NotTo(HaveOccurred())
		variables := ctxData["variables"].(map[string]interface{})
		Expect(variables["out"]).To(Equal("hello from tool"))
		Expect(variables["n"]).To(Equal(int64(42)))
	})

	It("should store raw tool result when step has no output definitions", func() {
		mockMCP = &mockMCPManager{
			client: &mockMCPClient{
				callToolResult: "raw string result",
			},
		}
		var err error
		workflowExecutor, err = NewWorkflowExecutorWithAgentExecutorAndMCPManager(
			k8sClient, nil, nil, nil, workflowRun, agent.NewMockAgentExecutor(), mockMCP, false, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())

		workflow := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "test-wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{
						Name: "mcpStep",
						MCPToolCall: &ottoflowv1alpha1.StepMCPToolCall{
							Server: "s", Tool: "t",
							Arguments: map[string]string{},
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, workflow)).To(Succeed())

		err = workflowExecutor.ExecuteWorkflow(ctx, workflow, workflowRun)
		Expect(err).NotTo(HaveOccurred())
		ctxData, _ := workflowExecutor.GetContextManager().ReadContext(ctx)
		variables := ctxData["variables"].(map[string]interface{})
		Expect(variables).To(HaveKey("result"))
		Expect(variables["result"]).To(Equal("raw string result"))
	})

	It("should fail when MCP client returns error", func() {
		mockMCP = &mockMCPManager{
			client: &mockMCPClient{callToolErr: context.DeadlineExceeded},
		}
		var err error
		workflowExecutor, err = NewWorkflowExecutorWithAgentExecutorAndMCPManager(
			k8sClient, nil, nil, nil, workflowRun, agent.NewMockAgentExecutor(), mockMCP, false, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())

		workflow := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "test-wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{
						Name: "mcpStep",
						MCPToolCall: &ottoflowv1alpha1.StepMCPToolCall{
							Server: "s", Tool: "t",
							Arguments: map[string]string{},
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, workflow)).To(Succeed())

		err = workflowExecutor.ExecuteWorkflow(ctx, workflow, workflowRun)
		Expect(err).To(HaveOccurred())
		Expect(workflowRun.Status.StepStatuses["mcpStep"].Phase).To(Equal(ottoflowv1alpha1.StepPhaseFailed))
	})
})

// secretGetFailsClient wraps base so that any Get of a Secret fails the spec while every other
// Get (the MCPServer lookup) passes through — proving the MCP manager never reads Secrets via
// the API when localExecutionMode is false.
func secretGetFailsClient(base client.WithWatch) client.Client {
	return interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, isSecret := obj.(*corev1.Secret); isSecret {
				Fail("unexpected Secret API Get for " + key.String() + ": localExecutionMode=false must resolve MCP credentials from mounted files")
				return nil
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
}

var _ = Describe("MCP manager secret access wiring", func() {
	var (
		ctx       context.Context
		scheme    *runtime.Scheme
		mcpServer *ottoflowv1alpha1.MCPServer
	)

	BeforeEach(func() {
		ctx = context.Background()
		scheme = runtime.NewScheme()
		utilruntime.Must(ottoflowv1alpha1.AddToScheme(scheme))
		utilruntime.Must(clientgoscheme.AddToScheme(scheme))
		// secretmount.Load() memoizes OTTOFLOW_SECRET_MOUNTS for the process lifetime; each
		// spec below sets it to a different value, so reset the memoization before every case.
		secretmount.ResetForTest()
		mcpServer = &ottoflowv1alpha1.MCPServer{
			ObjectMeta: metav1.ObjectMeta{Name: "creds-srv", Namespace: "default"},
			Spec: ottoflowv1alpha1.MCPServerSpec{
				Transport: ottoflowv1alpha1.TransportConfig{Type: "stdio", Command: []string{"echo"}},
				Env: []corev1.EnvVar{{
					Name: "TOKEN",
					ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "mcp-creds"},
						Key:                  "token",
					}},
				}},
			},
		}
	})

	mountToken := func(value string) {
		path := filepath.Join(GinkgoT().TempDir(), "token")
		Expect(os.WriteFile(path, []byte(value), 0o600)).To(Succeed())
		raw, err := json.Marshal(secretmount.Mounts{secretmount.Key("default", "mcp-creds", "token"): path})
		Expect(err).NotTo(HaveOccurred())
		GinkgoT().Setenv(secretmount.EnvVar, string(raw))
	}

	It("NewMCPManager(localExecutionMode=false) builds the client from the mounted credential without reading Secrets through the API", func() {
		mountToken("mounted")
		k8s := secretGetFailsClient(fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcpServer).Build())
		manager, err := NewMCPManager(k8s, false)
		Expect(err).NotTo(HaveOccurred())
		defer manager.Close() //nolint:errcheck
		c, err := manager.GetClient(ctx, "creds-srv", "default")
		Expect(err).NotTo(HaveOccurred())
		Expect(c).NotTo(BeNil())
	})

	It("NewMCPManager(localExecutionMode=false) fails client creation when the credential is not mounted, rather than reading the API", func() {
		GinkgoT().Setenv(secretmount.EnvVar, "{}")
		k8s := secretGetFailsClient(fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcpServer).Build())
		manager, err := NewMCPManager(k8s, false)
		Expect(err).NotTo(HaveOccurred())
		defer manager.Close() //nolint:errcheck
		_, err = manager.GetClient(ctx, "creds-srv", "default")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("not mounted"))
	})

	// The executor is what the in-cluster runner actually constructs (with a nil MCP manager
	// and localExecutionMode=false), so the flag must reach the MCP manager from there.
	It("a WorkflowExecutor built with localExecutionMode=false and no MCP manager resolves MCP credentials from the mounts", func() {
		mountToken("mounted")
		k8s := secretGetFailsClient(fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcpServer).Build())
		wr := &ottoflowv1alpha1.WorkflowRun{ObjectMeta: metav1.ObjectMeta{Name: "run", Namespace: "default"}}
		exec, err := NewWorkflowExecutorWithAgentExecutorAndMCPManager(
			k8s, nil, nil, nil, wr, agent.NewMockAgentExecutor(), nil, false, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())
		defer exec.mcpManager.Close() //nolint:errcheck
		c, err := exec.mcpManager.GetClient(ctx, "creds-srv", "default")
		Expect(err).NotTo(HaveOccurred())
		Expect(c).NotTo(BeNil())
	})
})
