/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package agent

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

func setRetireGrace(m *MCPClientManager, d time.Duration) {
	m.mu.Lock()
	m.retireGrace = d
	m.mu.Unlock()
}

// isClosed reports whether a real client was closed: a closed client answers "not initialized".
func isClosed(c MCPClient) bool {
	_, err := c.ListTools(context.Background())
	return err != nil && strings.Contains(err.Error(), "not initialized")
}

func TestMCPClientManagerClosesRetiredClientAfterGrace(t *testing.T) {
	k, m := newRefreshManager(t, unreachableMCPAddress)
	c1 := mustGetClient(t, m)
	updateMCPServer(t, k, func(s *ottoflowv1alpha1.MCPServer) { s.Spec.Transport.Address = "http://127.0.0.1:1/other" })
	mustGetClient(t, m) // retires c1
	if isClosed(c1) {
		t.Fatal("retired client was closed inside the grace period")
	}
	setRetireGrace(m, 0)
	mustGetClient(t, m) // sweeps c1; the close runs in the background
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if isClosed(c1) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("retired client never closed")
}

func TestMCPClientManagerCloseClosesRetiredClients(t *testing.T) {
	k, m := newRefreshManager(t, unreachableMCPAddress)
	c1 := mustGetClient(t, m)
	updateMCPServer(t, k, func(s *ottoflowv1alpha1.MCPServer) { s.Spec.Transport.Address = "http://127.0.0.1:1/other" })
	mustGetClient(t, m) // retires c1
	_ = m.Close()
	if !isClosed(c1) {
		t.Fatal("Close left a retired client open")
	}
}

func TestMCPClientManagerConcurrentCallersAfterChangeShareOneClient(t *testing.T) {
	k, m := newRefreshManager(t, unreachableMCPAddress)
	c1 := mustGetClient(t, m)
	updateMCPServer(t, k, func(s *ottoflowv1alpha1.MCPServer) { s.Spec.Transport.Address = "http://127.0.0.1:1/other" })
	var wg sync.WaitGroup
	got := make([]MCPClient, 16)
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i], _ = m.GetClient(context.Background(), refreshServerKey.Name, refreshServerKey.Namespace)
		}(i)
	}
	wg.Wait()
	for _, g := range got {
		if g == nil || g != got[0] || g == c1 {
			t.Fatal("concurrent callers after a change got different or stale clients")
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.clients) != 1 || len(m.retired) != 1 {
		t.Fatalf("cache=%d retired=%d, want 1 and 1", len(m.clients), len(m.retired))
	}
}

// stubClient is a fingerprinted client whose Close either blocks until release is closed or
// records that it ran.
type stubClient struct {
	mockMCPClientForBuildSessionTools
	fingerprint string
	release     chan struct{} // non-nil: Close blocks until it is closed
	closed      atomic.Bool
}

func (s *stubClient) Fingerprint() string { return s.fingerprint }

func (s *stubClient) Close() error {
	if s.release != nil {
		<-s.release
	}
	s.closed.Store(true)
	return nil
}

// stubFactory builds a stubClient fingerprinted by address. Clients for servers named
// "wedged" block in Close until release is closed.
type stubFactory struct{ release chan struct{} }

func (f *stubFactory) CreateClient(ctx context.Context, s *ottoflowv1alpha1.MCPServer) (MCPClient, error) {
	c := &stubClient{fingerprint: s.Spec.Transport.Address}
	if s.Name == "wedged" {
		c.release = f.release
	}
	return c, nil
}

// newStubManager returns a manager over one MCPServer per name, and retire, which caches a
// client for name, changes that server's address so the next GetClient retires the client, and
// returns the retired client.
func newStubManager(t *testing.T, names ...string) (*MCPClientManager, func(name string) *stubClient) {
	t.Helper()
	builder := fake.NewClientBuilder().WithScheme(refreshScheme())
	for _, n := range names {
		builder = builder.WithObjects(&ottoflowv1alpha1.MCPServer{
			ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: "default"},
			Spec:       ottoflowv1alpha1.MCPServerSpec{Transport: ottoflowv1alpha1.TransportConfig{Type: "http", Address: unreachableMCPAddress}},
		})
	}
	k := builder.Build()
	f := &stubFactory{release: make(chan struct{})}
	t.Cleanup(func() { close(f.release) })
	m := NewMCPClientManager(k, f)
	retire := func(name string) *stubClient {
		old := mustGetClientFor(t, m, name).(*stubClient)
		cur := &ottoflowv1alpha1.MCPServer{}
		if err := k.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, cur); err != nil {
			t.Fatal(err)
		}
		cur.Spec.Transport.Address += "/next"
		if err := k.Update(context.Background(), cur); err != nil {
			t.Fatal(err)
		}
		if mustGetClientFor(t, m, name) == MCPClient(old) {
			t.Fatal("address change did not retire the client")
		}
		return old
	}
	return m, retire
}

// A retired client whose Close never returns (a stdio child that ignores the close request)
// must not stall GetClient for any server.
func TestMCPClientManagerBlockingCloseDoesNotStallGetClient(t *testing.T) {
	m, retire := newStubManager(t, "wedged", "other")
	retire("wedged")
	setRetireGrace(m, 0)
	done := make(chan struct{})
	go func() {
		_, _ = m.GetClient(context.Background(), "wedged", "default") // sweep reaches the blocking Close
		_, _ = m.GetClient(context.Background(), "other", "default")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("GetClient stalled behind a retired client's blocking Close")
	}
}

// One retired client whose Close never returns must not keep the others in the same sweep open.
func TestMCPClientManagerBlockingCloseDoesNotPinOtherRetirees(t *testing.T) {
	m, retire := newStubManager(t, "wedged", "healthy")
	retire("wedged") // retired first, so it heads the sweep's batch
	healthy := retire("healthy")
	setRetireGrace(m, 0)
	go func() { _, _ = m.GetClient(context.Background(), "healthy", "default") }() // sweeps both retirees
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if healthy.closed.Load() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("a wedged retiree kept the rest of its sweep open")
}

func mustGetClientFor(t *testing.T, m *MCPClientManager, name string) MCPClient {
	t.Helper()
	c, err := m.GetClient(context.Background(), name, "default")
	if err != nil {
		t.Fatalf("GetClient(%s): %v", name, err)
	}
	return c
}
