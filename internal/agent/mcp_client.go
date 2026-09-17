/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/kubectl-ai/pkg/mcp"
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
}

// NewMCPClientManager creates a new MCP client manager
func NewMCPClientManager(k8sClient client.Client, factory MCPClientFactory) *MCPClientManager {
	return &MCPClientManager{
		client:   k8sClient,
		clients:  make(map[string]MCPClient),
		lastUsed: make(map[string]time.Time),
		factory:  factory,
	}
}

// GetClient gets or creates an MCP client for the given server
func (m *MCPClientManager) GetClient(ctx context.Context, serverName string, namespace string) (MCPClient, error) {
	key := fmt.Sprintf("%s/%s", namespace, serverName)

	m.mu.RLock()
	_, exists := m.clients[key]
	m.mu.RUnlock()

	if exists {
		m.mu.Lock()
		// Re-check under write lock: client may have been evicted between RLock and Lock.
		if c, ok := m.clients[key]; ok {
			m.lastUsed[key] = time.Now()
			m.mu.Unlock()
			return c, nil
		}
		m.mu.Unlock()
		// Client was evicted; fall through to create a new one.
	}

	// Create new client
	m.mu.Lock()
	defer m.mu.Unlock()

	// Double-check after acquiring write lock
	if client, exists := m.clients[key]; exists {
		m.lastUsed[key] = time.Now()
		return client, nil
	}

	// Get MCPServer CRD
	mcpServer := &ottoflowv1alpha1.MCPServer{}
	serverKey := types.NamespacedName{
		Name:      serverName,
		Namespace: namespace,
	}
	if err := m.client.Get(ctx, serverKey, mcpServer); err != nil {
		return nil, fmt.Errorf("failed to get MCPServer %s/%s: %w", namespace, serverName, err)
	}

	// Create client using factory
	newClient, err := m.factory.CreateClient(ctx, mcpServer)
	if err != nil {
		return nil, fmt.Errorf("failed to create MCP client: %w", err)
	}

	m.clients[key] = newClient
	m.lastUsed[key] = time.Now()
	return newClient, nil
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
			}
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
	return &realMCPClient{
		serverName:        mcpServer.Name,
		client:            mcpClient,
		connectionTimeout: connectionTimeout,
	}, nil
}
