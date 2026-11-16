/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	mcpproto "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

// unreachableMCPAddress refuses connections, so a client built for it never connects.
const unreachableMCPAddress = "http://127.0.0.1:1/mcp"

var refreshServerKey = types.NamespacedName{Namespace: "default", Name: "svc"}

func refreshScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = ottoflowv1alpha1.AddToScheme(s)
	_ = corev1.AddToScheme(s)
	return s
}

func refreshMCPServer(addr string) *ottoflowv1alpha1.MCPServer {
	return &ottoflowv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: refreshServerKey.Name, Namespace: refreshServerKey.Namespace, UID: "uid-1"},
		Spec: ottoflowv1alpha1.MCPServerSpec{
			Timeout:   "2s",
			Transport: ottoflowv1alpha1.TransportConfig{Type: "http", Address: addr},
		},
	}
}

// newRefreshManager returns a fake API client holding one MCPServer at addr (plus objs) and a
// manager that builds real clients through the default factory.
func newRefreshManager(t *testing.T, addr string, objs ...client.Object) (client.Client, *MCPClientManager) {
	t.Helper()
	k := fake.NewClientBuilder().WithScheme(refreshScheme()).WithObjects(append(objs, refreshMCPServer(addr))...).Build()
	return k, NewMCPClientManager(k, NewDefaultMCPClientFactory(k, true))
}

func updateMCPServer(t *testing.T, k client.Client, mutate func(*ottoflowv1alpha1.MCPServer)) {
	t.Helper()
	cur := &ottoflowv1alpha1.MCPServer{}
	if err := k.Get(context.Background(), refreshServerKey, cur); err != nil {
		t.Fatal(err)
	}
	mutate(cur)
	if err := k.Update(context.Background(), cur); err != nil {
		t.Fatal(err)
	}
}

func mustGetClient(t *testing.T, m *MCPClientManager) MCPClient {
	t.Helper()
	c, err := m.GetClient(context.Background(), refreshServerKey.Name, refreshServerKey.Namespace)
	if err != nil {
		t.Fatalf("GetClient: %v", err)
	}
	return c
}

func TestMCPClientManagerReusesClientForUnchangedServer(t *testing.T) {
	_, m := newRefreshManager(t, unreachableMCPAddress)
	if mustGetClient(t, m) != mustGetClient(t, m) {
		t.Fatal("unchanged MCPServer: a new client was built")
	}
}

func TestMCPClientManagerIgnoresLabelOnlyEdit(t *testing.T) {
	k, m := newRefreshManager(t, unreachableMCPAddress)
	c1 := mustGetClient(t, m)
	updateMCPServer(t, k, func(s *ottoflowv1alpha1.MCPServer) { s.Labels = map[string]string{"team": "a"} })
	if mustGetClient(t, m) != c1 {
		t.Fatal("label-only edit: a new client was built")
	}
}

func TestMCPClientManagerRebuildsOnAddressChange(t *testing.T) {
	k, m := newRefreshManager(t, unreachableMCPAddress)
	c1 := mustGetClient(t, m)
	updateMCPServer(t, k, func(s *ottoflowv1alpha1.MCPServer) { s.Spec.Transport.Address = "http://127.0.0.1:1/other" })
	if mustGetClient(t, m) == c1 {
		t.Fatal("address change not picked up: the old client was returned")
	}
}

func TestMCPClientManagerRebuildsOnSecretRotation(t *testing.T) {
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "token", Namespace: "default"},
		Data:       map[string][]byte{"value": []byte("old")},
	}
	k, m := newRefreshManager(t, unreachableMCPAddress, sec)
	updateMCPServer(t, k, func(s *ottoflowv1alpha1.MCPServer) {
		s.Spec.Auth = &ottoflowv1alpha1.AuthConfig{
			Type:      "bearer",
			SecretRef: &ottoflowv1alpha1.SecretReference{Name: "token", Key: "value"},
		}
	})
	c1 := mustGetClient(t, m)
	sec.Data["value"] = []byte("new")
	if err := k.Update(context.Background(), sec); err != nil {
		t.Fatal(err)
	}
	if mustGetClient(t, m) == c1 {
		t.Fatal("secret rotation not picked up: the old client was returned")
	}
}

func TestMCPClientManagerErrorsForDeletedServer(t *testing.T) {
	k, m := newRefreshManager(t, unreachableMCPAddress)
	mustGetClient(t, m)
	if err := k.Delete(context.Background(), refreshMCPServer(unreachableMCPAddress)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.GetClient(context.Background(), refreshServerKey.Name, refreshServerKey.Namespace); err == nil {
		t.Fatal("deleted MCPServer: the cached client was still returned")
	}
}

// A transient API error fails the call but keeps the cached client; NotFound drops it.
func TestMCPClientManagerKeepsClientOnTransientGetError(t *testing.T) {
	var failWith error
	base := fake.NewClientBuilder().WithScheme(refreshScheme()).WithObjects(refreshMCPServer(unreachableMCPAddress)).Build()
	k := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if failWith != nil {
				return failWith
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	m := NewMCPClientManager(k, NewDefaultMCPClientFactory(k, true))
	c1 := mustGetClient(t, m)

	failWith = apierrors.NewServiceUnavailable("api server unavailable")
	if _, err := m.GetClient(context.Background(), refreshServerKey.Name, refreshServerKey.Namespace); err == nil {
		t.Fatal("GetClient succeeded while the MCPServer could not be read")
	}
	failWith = nil
	if mustGetClient(t, m) != c1 {
		t.Fatal("transient Get error evicted a healthy client")
	}

	failWith = apierrors.NewNotFound(schema.GroupResource{Resource: "mcpservers"}, refreshServerKey.Name)
	_, _ = m.GetClient(context.Background(), refreshServerKey.Name, refreshServerKey.Namespace)
	failWith = nil
	if mustGetClient(t, m) == c1 {
		t.Fatal("NotFound did not evict the cached client")
	}
}

// A failed ListTools must not close the shared cached client: the next attempt retries the
// connection instead of failing with "MCP client not initialized".
func TestBuildSessionToolsRetriesAfterListToolsFailure(t *testing.T) {
	_, m := newRefreshManager(t, unreachableMCPAddress)
	ctx := context.Background()
	if _, err := buildSessionToolsFromMCP(ctx, []string{"svc:x"}, "default", m); err == nil {
		t.Fatal("expected a connection error from an unreachable server")
	}
	_, err := buildSessionToolsFromMCP(ctx, []string{"svc:x"}, "default", m)
	if err == nil || strings.Contains(err.Error(), "not initialized") {
		t.Fatalf("second attempt reused a closed client: %v", err)
	}
}

// newRestartableMCPServer serves one echo tool over streamable HTTP. While down is true it
// answers 404, which an MCP client reads as a terminated session (what a server restart causes).
// Each wrap, if given, decorates the MCP handler (inside the down check), in order.
func newRestartableMCPServer(t *testing.T, wrap ...func(http.Handler) http.Handler) (*httptest.Server, *atomic.Bool) {
	t.Helper()
	ms := server.NewMCPServer("test", "1")
	ms.AddTool(mcpproto.NewTool("echo"), func(ctx context.Context, r mcpproto.CallToolRequest) (*mcpproto.CallToolResult, error) {
		return mcpproto.NewToolResultText("hi"), nil
	})
	var h http.Handler = server.NewStreamableHTTPServer(ms)
	for _, f := range wrap {
		h = f(h)
	}
	var down atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			http.NotFound(w, r)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts, &down
}

// After the server drops the session, the next agent execution builds a fresh client and works
// again once the server is back, instead of reusing the dead session.
func TestBuildSessionToolsRecoversAfterSessionTerminated(t *testing.T) {
	ts, down := newRestartableMCPServer(t)
	_, m := newRefreshManager(t, ts.URL+"/mcp")
	ctx := context.Background()
	if _, err := buildSessionToolsFromMCP(ctx, []string{"svc:echo"}, "default", m); err != nil {
		t.Fatalf("first execution: %v", err)
	}
	down.Store(true)
	if _, err := buildSessionToolsFromMCP(ctx, []string{"svc:echo"}, "default", m); err == nil {
		t.Fatal("expected ListTools to fail while the server answers 404")
	}
	down.Store(false)
	tools, err := buildSessionToolsFromMCP(ctx, []string{"svc:echo"}, "default", m)
	if err != nil {
		t.Fatalf("execution after the server came back: %v", err)
	}
	if _, ok := tools["svc:echo"]; !ok {
		t.Fatalf("echo tool missing after recovery: %v", tools)
	}
}

// A server that rejects server/discover leaves the client on the sessionful (Mcp-Session-Id)
// handshake. The cached client must still be evicted when that server drops the session.
// TestBuildSessionToolsRecoversAfterSessionTerminated's server answers server/discover, so its
// client holds no session and passes without eviction; this test is the eviction guard.
func TestBuildSessionToolsRecoversAfterSessionTerminatedOnLegacyServer(t *testing.T) {
	var sawSession atomic.Bool
	ts, down := newRestartableMCPServer(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Mcp-Method") == "server/discover" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"Method not found"}}`))
				return
			}
			if r.Header.Get("Mcp-Session-Id") != "" {
				sawSession.Store(true)
			}
			h.ServeHTTP(w, r)
		})
	})
	_, m := newRefreshManager(t, ts.URL+"/mcp")
	ctx := context.Background()

	if _, err := buildSessionToolsFromMCP(ctx, []string{"svc:echo"}, "default", m); err != nil {
		t.Fatalf("first execution: %v", err)
	}
	if !sawSession.Load() {
		t.Fatal("no request carried Mcp-Session-Id: the client is not on the legacy session path")
	}
	down.Store(true)
	if _, err := buildSessionToolsFromMCP(ctx, []string{"svc:echo"}, "default", m); err == nil {
		t.Fatal("expected ListTools to fail while the server answers 404")
	}
	down.Store(false)
	if _, err := buildSessionToolsFromMCP(ctx, []string{"svc:echo"}, "default", m); err != nil {
		t.Fatalf("execution after the server came back: %v", err)
	}
}

// A server restricted to older protocol versions rejects the new handshake; setting
// Mcp-Protocol-Version to an older accepted version under spec.transport.headers makes the
// client use the earlier handshake. Pins docs/user/upgrades/mcp-protocol-negotiation.md.
func TestMCPProtocolVersionHeaderWorkaround(t *testing.T) {
	ms := server.NewMCPServer("test", "1")
	ms.AddTool(mcpproto.NewTool("echo"), func(ctx context.Context, r mcpproto.CallToolRequest) (*mcpproto.CallToolResult, error) {
		return mcpproto.NewToolResultText("hi"), nil
	})
	ts := httptest.NewServer(server.NewStreamableHTTPServer(ms,
		server.WithStreamableHTTPProtocolVersions("2025-11-25", "2025-06-18", "2025-03-26")))
	t.Cleanup(ts.Close)

	listTools := func(headers map[string]string) error {
		k, m := newRefreshManager(t, ts.URL+"/mcp")
		defer func() { _ = m.Close() }()
		updateMCPServer(t, k, func(s *ottoflowv1alpha1.MCPServer) { s.Spec.Transport.Headers = headers })
		c, err := m.GetClient(context.Background(), refreshServerKey.Name, refreshServerKey.Namespace)
		if err != nil {
			return err
		}
		_, err = c.ListTools(context.Background())
		return err
	}

	if err := listTools(nil); err == nil || !strings.Contains(err.Error(), "unsupported protocol version") {
		t.Fatalf("without the header: want unsupported protocol version, got %v", err)
	}
	if err := listTools(map[string]string{"Mcp-Protocol-Version": "2025-11-25"}); err != nil {
		t.Fatalf("with the header: %v", err)
	}
}
