/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"

	"github.com/mark3labs/mcp-go/client/transport"
	mcpproto "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/agent"
)

// evictingMCPManager records Evict calls.
type evictingMCPManager struct {
	mockMCPManager
	evicted []agent.MCPClient
}

func (m *evictingMCPManager) Evict(namespace, serverName string, c agent.MCPClient) {
	m.evicted = append(m.evicted, c)
}

func mcpEvictScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	utilruntime.Must(ottoflowv1alpha1.AddToScheme(scheme))
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	return scheme
}

func mcpEchoWorkflow() *ottoflowv1alpha1.Workflow {
	return &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
		Spec: ottoflowv1alpha1.WorkflowSpec{Steps: []ottoflowv1alpha1.Step{{
			Name:        "s",
			MCPToolCall: &ottoflowv1alpha1.StepMCPToolCall{Server: "svc", Tool: "echo", Arguments: map[string]string{}},
		}}},
	}
}

func runMCPEchoWorkflow(ctx context.Context, k client.Client, wf *ottoflowv1alpha1.Workflow, m MCPManager) error {
	run := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "default"},
		Spec:       ottoflowv1alpha1.WorkflowRunSpec{WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: wf.Name}},
	}
	we, err := NewWorkflowExecutorWithAgentExecutorAndMCPManager(
		k, nil, nil, nil, run, agent.NewMockAgentExecutor(), m, false, 0, 5, nil)
	Expect(err).NotTo(HaveOccurred())
	return we.ExecuteWorkflow(ctx, wf, run)
}

var _ = DescribeTable("mcpToolCall evicts the cached client only when its session was terminated",
	func(callErr error, wantEvicted int) {
		ctx := context.Background()
		k := fake.NewClientBuilder().WithScheme(mcpEvictScheme()).Build()
		mcpClient := &mockMCPClient{callToolErr: callErr}
		m := &evictingMCPManager{mockMCPManager: mockMCPManager{client: mcpClient}}
		wf := mcpEchoWorkflow()
		Expect(k.Create(ctx, wf)).To(Succeed())

		Expect(runMCPEchoWorkflow(ctx, k, wf, m)).To(HaveOccurred())
		Expect(m.evicted).To(HaveLen(wantEvicted))
		for _, c := range m.evicted {
			Expect(c).To(BeIdenticalTo(mcpClient))
		}
	},
	Entry("session terminated", fmt.Errorf("error calling tool echo: %w", transport.ErrSessionTerminated), 1),
	Entry("other error", context.DeadlineExceeded, 0),
)

var _ = It("a later run reconnects after the MCP server dropped the session", func() {
	ms := server.NewMCPServer("test", "1")
	ms.AddTool(mcpproto.NewTool("echo"), func(ctx context.Context, r mcpproto.CallToolRequest) (*mcpproto.CallToolResult, error) {
		return mcpproto.NewToolResultText("hi"), nil
	})
	h := server.NewStreamableHTTPServer(ms)
	// While down is true the server answers 404, which the client reads as a terminated session.
	var down atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			http.NotFound(w, r)
			return
		}
		h.ServeHTTP(w, r)
	}))
	DeferCleanup(ts.Close)

	ctx := context.Background()
	mcpServer := &ottoflowv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default", UID: "uid-1"},
		Spec: ottoflowv1alpha1.MCPServerSpec{
			Timeout:   "5s",
			Transport: ottoflowv1alpha1.TransportConfig{Type: "http", Address: ts.URL + "/mcp"},
		},
	}
	wf := mcpEchoWorkflow()
	k := fake.NewClientBuilder().WithScheme(mcpEvictScheme()).WithObjects(mcpServer, wf).Build()
	m, err := NewMCPManager(k, true)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(m.Close)

	Expect(runMCPEchoWorkflow(ctx, k, wf, m)).To(Succeed())
	down.Store(true)
	Expect(runMCPEchoWorkflow(ctx, k, wf, m)).To(HaveOccurred())
	down.Store(false)
	Expect(runMCPEchoWorkflow(ctx, k, wf, m)).To(Succeed(), "the dead session was reused after the server came back")
})
