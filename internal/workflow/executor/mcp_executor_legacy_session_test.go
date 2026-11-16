/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"

	mcpproto "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

// Same scenario as "a later run reconnects after the MCP server dropped the session", but the
// server rejects server/discover, so the client stays on the sessionful legacy handshake. That
// test's server answers server/discover, so its client holds no session and recovers even
// without eviction; this is the test that fails if eviction breaks.
var _ = It("a later run reconnects after a legacy MCP server dropped the session", func() {
	ms := server.NewMCPServer("test", "1")
	ms.AddTool(mcpproto.NewTool("echo"), func(ctx context.Context, r mcpproto.CallToolRequest) (*mcpproto.CallToolResult, error) {
		return mcpproto.NewToolResultText("hi"), nil
	})
	h := server.NewStreamableHTTPServer(ms)
	// While down is true the server answers 404, which the client reads as a terminated session.
	var down, sawSession atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Mcp-Method") == "server/discover" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"Method not found"}}`))
			return
		}
		if r.Header.Get("Mcp-Session-Id") != "" {
			sawSession.Store(true)
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
	Expect(sawSession.Load()).To(BeTrue(), "no request carried Mcp-Session-Id: the client is not on the legacy session path")
	down.Store(true)
	Expect(runMCPEchoWorkflow(ctx, k, wf, m)).To(HaveOccurred())
	down.Store(false)
	Expect(runMCPEchoWorkflow(ctx, k, wf, m)).To(Succeed(), "the dead session was reused after the server came back")
})
