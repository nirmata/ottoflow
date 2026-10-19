/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/kubectl-ai/pkg/mcp"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

// DefaultMCPClientIdleTimeout is the default idle timeout for MCP clients.
const DefaultMCPClientIdleTimeout = 30 * time.Minute

// MCPClientManager manages MCP client connections
type MCPClientManager struct {
	client      client.Client
	clients     map[string]MCPClient
	lastUsed    map[string]time.Time
	mu          sync.RWMutex
	factory     MCPClientFactory
	evictCancel context.CancelFunc // non-nil after StartEviction; cancelled by Close
	// fingerprints holds, per cached client, the fingerprint of the config it was built from.
	fingerprints map[string]string
	// versions counts, per server, how many times its cached client was stored or removed. A
	// refresh that sees it move between its read and its store knows the cache changed in the
	// meantime.
	versions map[string]uint64
	// retired holds replaced or evicted clients. Other callers may still be using one, so it is
	// closed only once retireGrace has passed.
	retired     []retiredClient
	retireGrace time.Duration
}

// MCPClientEvicter drops a cached MCP client that is known to be broken, so the next GetClient
// builds a new one.
type MCPClientEvicter interface {
	// Evict removes the cached client for the server if it is still c (any client when c is nil).
	Evict(namespace, serverName string, c MCPClient)
}

type retiredClient struct {
	c  MCPClient
	at time.Time
}

// NewMCPClientManager creates a new MCP client manager
func NewMCPClientManager(k8sClient client.Client, factory MCPClientFactory) *MCPClientManager {
	return &MCPClientManager{
		client:       k8sClient,
		clients:      make(map[string]MCPClient),
		lastUsed:     make(map[string]time.Time),
		fingerprints: make(map[string]string),
		versions:     make(map[string]uint64),
		factory:      factory,
		retireGrace:  DefaultMCPClientIdleTimeout,
	}
}

// GetClient returns the MCP client for the given server. It re-reads the MCPServer on every
// call and builds a candidate client through the factory, which resolves its Secrets. The cached
// client is returned while its config fingerprint still matches; otherwise the candidate
// replaces it and the old client is retired. The reads and the build run outside the lock.
func (m *MCPClientManager) GetClient(ctx context.Context, serverName string, namespace string) (MCPClient, error) {
	name := types.NamespacedName{Name: serverName, Namespace: namespace}
	c, superseded, err := m.refresh(ctx, name, true)
	if superseded {
		// Another caller, or idle eviction, stored or removed a client while this one was
		// reading and building, possibly from a newer config, so read again instead of
		// overwriting it. The second read does not check again: if a third caller stores a
		// client between the second read and its store, this call can still store an older
		// client over it or after it. A later GetClient that reads and builds successfully
		// corrects it.
		c, _, err = m.refresh(ctx, name, false)
	}
	return c, err
}

// refresh reads the MCPServer, builds a candidate client and stores it unless the cached client
// has the same fingerprint. With yieldToNewer set, it stores nothing and reports superseded when
// the candidate does not match the cached client (or nothing is cached) and the count has moved
// since this refresh began.
func (m *MCPClientManager) refresh(ctx context.Context, name types.NamespacedName, yieldToNewer bool) (MCPClient, bool, error) {
	key := name.String()
	m.mu.RLock()
	version := m.versions[key]
	m.mu.RUnlock()

	mcpServer := &ottoflowv1alpha1.MCPServer{}
	if err := m.client.Get(ctx, name, mcpServer); err != nil {
		// A transient API error keeps the cached client; only a deleted server drops it.
		if apierrors.IsNotFound(err) {
			m.Evict(name.Namespace, name.Name, nil)
		}
		return nil, false, fmt.Errorf("failed to get MCPServer %s: %w", key, err)
	}
	candidate, err := m.factory.CreateClient(ctx, mcpServer)
	if err != nil {
		return nil, false, fmt.Errorf("failed to create MCP client: %w", err)
	}
	fingerprint := fingerprintOf(candidate) + "|" + string(mcpServer.UID)

	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweepRetiredLocked()
	now := time.Now()
	cur, ok := m.clients[key]
	if ok && m.fingerprints[key] == fingerprint {
		// The candidate has not connected yet, so dropping it releases nothing.
		m.lastUsed[key] = now
		return cur, false, nil
	}
	if yieldToNewer && m.versions[key] != version {
		return nil, true, nil
	}
	if ok {
		m.retired = append(m.retired, retiredClient{c: cur, at: now})
	}
	m.clients[key] = candidate
	m.fingerprints[key] = fingerprint
	m.lastUsed[key] = now
	m.versions[key]++
	return candidate, false, nil
}

// fingerprintOf returns the client's config fingerprint, or "" when the client has none (it is
// then reused until evicted).
func fingerprintOf(c MCPClient) string {
	if f, ok := c.(interface{ Fingerprint() string }); ok {
		return f.Fingerprint()
	}
	return ""
}

// Evict implements MCPClientEvicter. The removed client is retired, not closed, because other
// callers may still hold it.
func (m *MCPClientManager) Evict(namespace, serverName string, c MCPClient) {
	key := types.NamespacedName{Name: serverName, Namespace: namespace}.String()
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.clients[key]
	if !ok || (c != nil && cur != c) {
		return
	}
	delete(m.clients, key)
	delete(m.fingerprints, key)
	delete(m.lastUsed, key)
	m.versions[key]++
	m.retired = append(m.retired, retiredClient{c: cur, at: time.Now()})
}

// sweepRetiredLocked closes retired clients older than retireGrace. Each Close runs in its own
// goroutine, outside the lock: a stdio Close waits for the child process with no kill, so one
// child that never exits must not block callers or the other closes.
func (m *MCPClientManager) sweepRetiredLocked() {
	now := time.Now()
	m.retired = slices.DeleteFunc(m.retired, func(r retiredClient) bool {
		if now.Sub(r.at) < m.retireGrace {
			return false
		}
		go func() { _ = r.c.Close() }()
		return true
	})
}

// evictIdle closes and removes clients that have not been used within maxIdle duration.
func (m *MCPClientManager) evictIdle(maxIdle time.Duration) {
	type idleClient struct {
		key    string
		client MCPClient
	}

	var clientsToClose []idleClient

	m.mu.Lock()
	now := time.Now()
	for key, last := range m.lastUsed {
		if now.Sub(last) > maxIdle {
			if c, ok := m.clients[key]; ok {
				clientsToClose = append(clientsToClose, idleClient{key: key, client: c})
				delete(m.clients, key)
				m.versions[key]++
			}
			delete(m.fingerprints, key)
			delete(m.lastUsed, key)
		}
	}
	m.mu.Unlock()

	for _, idle := range clientsToClose {
		if err := idle.client.Close(); err != nil {
			klog.V(2).InfoS("Error closing idle MCP client", "key", idle.key, "error", err)
		}
	}
}

// StartEviction starts a background goroutine that evicts idle clients on the given interval.
// The goroutine is stopped when Close is called. Calling StartEviction a second time replaces
// the previous eviction goroutine (the old one is stopped first).
func (m *MCPClientManager) StartEviction(interval time.Duration, maxIdle time.Duration) {
	evictCtx, cancel := context.WithCancel(context.Background())

	m.mu.Lock()
	if m.evictCancel != nil {
		m.evictCancel() // stop any previously running eviction goroutine
	}
	m.evictCancel = cancel
	m.mu.Unlock()

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				m.evictIdle(maxIdle)
			case <-evictCtx.Done():
				return
			}
		}
	}()
}

// Close stops the background eviction goroutine and closes all managed clients.
func (m *MCPClientManager) Close() error {
	m.mu.Lock()
	if m.evictCancel != nil {
		m.evictCancel()
		m.evictCancel = nil
	}

	var errs []error
	for _, client := range m.clients {
		if err := client.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	for _, r := range m.retired {
		if err := r.c.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	m.retired = nil
	m.mu.Unlock()

	if len(errs) > 0 {
		return fmt.Errorf("errors closing clients: %v", errs)
	}

	return nil
}

// DefaultMCPClientFactory implements MCPClientFactory
type DefaultMCPClientFactory struct {
	k8sClient          client.Client
	realClientBuilder  RealMCPClientBuilder // optional; when set, used instead of mcp.NewClient (for tests)
	useAPISecretAccess bool                 // true: resolve Secrets via live k8sClient.Get; false: read from controller-mounted files (internal/secretmount)
}

// NewDefaultMCPClientFactory creates a new MCP client factory. useAPISecretAccess selects how
// MCP env/auth Secrets are resolved: true does a live client.Client.Get (the agent-executor
// and the CLI, which read Secrets under their own identity); false reads from the files the
// controller mounted into the pod (the in-cluster workflow-runner Job, which never reads
// Secrets through the API).
func NewDefaultMCPClientFactory(k8sClient client.Client, useAPISecretAccess bool) *DefaultMCPClientFactory {
	return &DefaultMCPClientFactory{k8sClient: k8sClient, useAPISecretAccess: useAPISecretAccess}
}

// NewDefaultMCPClientFactoryWithBuilder creates a factory with an optional RealMCPClientBuilder (for tests).
func NewDefaultMCPClientFactoryWithBuilder(k8sClient client.Client, builder RealMCPClientBuilder, useAPISecretAccess bool) *DefaultMCPClientFactory {
	return &DefaultMCPClientFactory{k8sClient: k8sClient, realClientBuilder: builder, useAPISecretAccess: useAPISecretAccess}
}

// CreateClient creates an MCP client from MCPServer CRD
func (f *DefaultMCPClientFactory) CreateClient(ctx context.Context, mcpServer *ottoflowv1alpha1.MCPServer) (MCPClient, error) {
	transport := mcpServer.Spec.Transport

	switch transport.Type {
	case "stdio":
		return f.createStdioClient(ctx, mcpServer)
	case "http", "sse":
		return f.createHTTPClient(ctx, mcpServer)
	default:
		return nil, fmt.Errorf("unsupported transport type: %s", transport.Type)
	}
}

// createStdioClient creates a stdio-based MCP client using kubectl-ai MCP package
func (f *DefaultMCPClientFactory) createStdioClient(ctx context.Context, mcpServer *ottoflowv1alpha1.MCPServer) (MCPClient, error) {
	klog.V(2).InfoS("Creating stdio MCP client", "server", mcpServer.Name)
	return f.createRealMCPClient(ctx, mcpServer)
}

// createHTTPClient creates an HTTP/SSE-based MCP client using kubectl-ai MCP package
func (f *DefaultMCPClientFactory) createHTTPClient(ctx context.Context, mcpServer *ottoflowv1alpha1.MCPServer) (MCPClient, error) {
	klog.V(2).InfoS("Creating HTTP/SSE MCP client", "server", mcpServer.Name, "transport", mcpServer.Spec.Transport.Type)
	return f.createRealMCPClient(ctx, mcpServer)
}

// createRealMCPClient builds MCP client config from MCPServer and returns a real MCP client
func (f *DefaultMCPClientFactory) createRealMCPClient(ctx context.Context, mcpServer *ottoflowv1alpha1.MCPServer) (MCPClient, error) {
	cfg, err := buildMCPClientConfig(ctx, f.k8sClient, mcpServer, f.useAPISecretAccess)
	if err != nil {
		return nil, fmt.Errorf("building MCP client config: %w", err)
	}
	connectionTimeout := 90 * time.Second
	if mcpServer.Spec.Timeout != "" {
		if d, err := time.ParseDuration(mcpServer.Spec.Timeout); err == nil && d > 0 {
			connectionTimeout = d
		}
	}
	if f.realClientBuilder != nil {
		return f.realClientBuilder.Build(ctx, mcpServer.Name, connectionTimeout, cfg)
	}
	mcpClient := mcp.NewClient(cfg)
	// The fingerprint covers every resolved input, Secret values included, so a rotated Secret
	// or an edited spec yields a new client. Only the hash is kept. ClientConfig holds plain
	// values only, so Marshal cannot fail.
	resolved, _ := json.Marshal(struct {
		Config  mcp.ClientConfig
		Timeout time.Duration
	}{cfg, connectionTimeout})
	sum := sha256.Sum256(resolved)
	return &realMCPClient{
		fingerprint:       hex.EncodeToString(sum[:]),
		serverName:        mcpServer.Name,
		client:            mcpClient,
		connectionTimeout: connectionTimeout,
	}, nil
}
