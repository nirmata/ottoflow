/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package agent

import (
	"context"
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

var _ = Describe("MCPClientManager", func() {
	var (
		ctx    context.Context
		scheme *runtime.Scheme
	)

	BeforeEach(func() {
		ctx = context.Background()
		scheme = runtime.NewScheme()
		utilruntime.Must(ottoflowv1alpha1.AddToScheme(scheme))
		utilruntime.Must(corev1.AddToScheme(scheme))
	})

	It("NewMCPClientManager creates manager with empty client map", func() {
		k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
		factory := &mockMCPClientFactory{}
		mgr := NewMCPClientManager(k8sClient, factory)
		Expect(mgr).NotTo(BeNil())
	})

	It("GetClient returns error when MCPServer not found", func() {
		k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
		factory := &mockMCPClientFactory{}
		mgr := NewMCPClientManager(k8sClient, factory)
		_, err := mgr.GetClient(ctx, "missing", "default")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("failed to get MCPServer"))
		Expect(factory.createCount).To(Equal(0))
	})

	It("GetClient creates client via factory and caches it", func() {
		mcpServer := &ottoflowv1alpha1.MCPServer{
			ObjectMeta: metav1.ObjectMeta{Name: "svc1", Namespace: "default"},
			Spec: ottoflowv1alpha1.MCPServerSpec{
				Transport: ottoflowv1alpha1.TransportConfig{
					Type:    "stdio",
					Command: []string{"echo"},
				},
			},
		}
		k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcpServer).Build()
		mockClient := &mockMCPClientForBuildSessionTools{}
		factory := &mockMCPClientFactory{client: mockClient}
		mgr := NewMCPClientManager(k8sClient, factory)

		c1, err := mgr.GetClient(ctx, "svc1", "default")
		Expect(err).NotTo(HaveOccurred())
		Expect(c1).To(Equal(mockClient))
		Expect(factory.createCount).To(Equal(1))

		c2, err := mgr.GetClient(ctx, "svc1", "default")
		Expect(err).NotTo(HaveOccurred())
		Expect(c2).To(Equal(c1))
		Expect(factory.createCount).To(Equal(1))
	})

	It("GetClient returns same client when called concurrently for same server", func() {
		mcpServer := &ottoflowv1alpha1.MCPServer{
			ObjectMeta: metav1.ObjectMeta{Name: "svc1", Namespace: "default"},
			Spec: ottoflowv1alpha1.MCPServerSpec{
				Transport: ottoflowv1alpha1.TransportConfig{
					Type:    "stdio",
					Command: []string{"echo"},
				},
			},
		}
		k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcpServer).Build()
		mockClient := &mockMCPClientForBuildSessionTools{}
		factory := &mockMCPClientFactory{client: mockClient}
		mgr := NewMCPClientManager(k8sClient, factory)
		var c1, c2 MCPClient
		var err1, err2 error
		done := make(chan struct{})
		go func() {
			c1, err1 = mgr.GetClient(ctx, "svc1", "default")
			done <- struct{}{}
		}()
		go func() {
			c2, err2 = mgr.GetClient(ctx, "svc1", "default")
			done <- struct{}{}
		}()
		<-done
		<-done
		Expect(err1).NotTo(HaveOccurred())
		Expect(err2).NotTo(HaveOccurred())
		Expect(c1).To(Equal(c2))
		Expect(factory.createCount).To(Equal(1))
	})

	It("GetClient returns error when factory fails", func() {
		mcpServer := &ottoflowv1alpha1.MCPServer{
			ObjectMeta: metav1.ObjectMeta{Name: "svc1", Namespace: "default"},
			Spec: ottoflowv1alpha1.MCPServerSpec{
				Transport: ottoflowv1alpha1.TransportConfig{
					Type:    "stdio",
					Command: []string{"echo"},
				},
			},
		}
		k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcpServer).Build()
		factory := &mockMCPClientFactory{err: errors.New("create failed")}
		mgr := NewMCPClientManager(k8sClient, factory)

		_, err := mgr.GetClient(ctx, "svc1", "default")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("create failed"))
	})

	It("Close closes all cached clients", func() {
		mcpServer := &ottoflowv1alpha1.MCPServer{
			ObjectMeta: metav1.ObjectMeta{Name: "svc1", Namespace: "default"},
			Spec: ottoflowv1alpha1.MCPServerSpec{
				Transport: ottoflowv1alpha1.TransportConfig{
					Type:    "stdio",
					Command: []string{"echo"},
				},
			},
		}
		k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcpServer).Build()
		mockClient := &mockMCPClientForBuildSessionTools{}
		factory := &mockMCPClientFactory{client: mockClient}
		mgr := NewMCPClientManager(k8sClient, factory)
		_, _ = mgr.GetClient(ctx, "svc1", "default")
		err := mgr.Close()
		Expect(err).NotTo(HaveOccurred())
	})

	It("Close returns error when a client fails to close", func() {
		mcpServer1 := &ottoflowv1alpha1.MCPServer{
			ObjectMeta: metav1.ObjectMeta{Name: "svc1", Namespace: "default"},
			Spec: ottoflowv1alpha1.MCPServerSpec{
				Transport: ottoflowv1alpha1.TransportConfig{Type: "stdio", Command: []string{"echo"}},
			},
		}
		mcpServer2 := &ottoflowv1alpha1.MCPServer{
			ObjectMeta: metav1.ObjectMeta{Name: "svc2", Namespace: "default"},
			Spec: ottoflowv1alpha1.MCPServerSpec{
				Transport: ottoflowv1alpha1.TransportConfig{Type: "stdio", Command: []string{"echo"}},
			},
		}
		k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcpServer1, mcpServer2).Build()
		closeErr := errors.New("close failed")
		factory := &mockMCPClientFactory{client: &mockMCPClientWithCloseError{err: closeErr}}
		mgr := NewMCPClientManager(k8sClient, factory)
		_, _ = mgr.GetClient(ctx, "svc1", "default")
		_, _ = mgr.GetClient(ctx, "svc2", "default")
		err := mgr.Close()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("close failed"))
	})
})

// mockRealMCPClientBuilder implements RealMCPClientBuilder for tests.
type mockRealMCPClientBuilder struct {
	client MCPClient
	err    error
}

func (m *mockRealMCPClientBuilder) Build(ctx context.Context, serverName string, connectionTimeout time.Duration, cfg interface{}) (MCPClient, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.client, nil
}

var _ = Describe("DefaultMCPClientFactory CreateClient with builder", func() {
	var (
		ctx       context.Context
		scheme    *runtime.Scheme
		k8sClient client.Client
	)

	BeforeEach(func() {
		ctx = context.Background()
		scheme = runtime.NewScheme()
		utilruntime.Must(ottoflowv1alpha1.AddToScheme(scheme))
		utilruntime.Must(corev1.AddToScheme(scheme))
		k8sClient = fake.NewClientBuilder().WithScheme(scheme).Build()
	})

	It("createStdioClient path uses builder and returns client", func() {
		mockClient := &mockMCPClientForBuildSessionTools{}
		builder := &mockRealMCPClientBuilder{client: mockClient}
		f := NewDefaultMCPClientFactoryWithBuilder(k8sClient, builder, true)
		mcpServer := &ottoflowv1alpha1.MCPServer{
			ObjectMeta: metav1.ObjectMeta{Name: "stdio-srv", Namespace: "default"},
			Spec: ottoflowv1alpha1.MCPServerSpec{
				Transport: ottoflowv1alpha1.TransportConfig{
					Type:    "stdio",
					Command: []string{"/bin/echo"},
				},
			},
		}
		c, err := f.CreateClient(ctx, mcpServer)
		Expect(err).NotTo(HaveOccurred())
		Expect(c).To(Equal(mockClient))
	})

	It("createHTTPClient path uses builder and returns client", func() {
		mockClient := &mockMCPClientForBuildSessionTools{}
		builder := &mockRealMCPClientBuilder{client: mockClient}
		f := NewDefaultMCPClientFactoryWithBuilder(k8sClient, builder, true)
		mcpServer := &ottoflowv1alpha1.MCPServer{
			ObjectMeta: metav1.ObjectMeta{Name: "http-srv", Namespace: "default"},
			Spec: ottoflowv1alpha1.MCPServerSpec{
				Transport: ottoflowv1alpha1.TransportConfig{
					Type:    "http",
					Address: "https://mcp.example.com",
				},
			},
		}
		c, err := f.CreateClient(ctx, mcpServer)
		Expect(err).NotTo(HaveOccurred())
		Expect(c).To(Equal(mockClient))
	})

	It("createRealMCPClient returns builder error when Build fails", func() {
		builder := &mockRealMCPClientBuilder{err: errors.New("build failed")}
		f := NewDefaultMCPClientFactoryWithBuilder(k8sClient, builder, true)
		mcpServer := &ottoflowv1alpha1.MCPServer{
			ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"},
			Spec: ottoflowv1alpha1.MCPServerSpec{
				Transport: ottoflowv1alpha1.TransportConfig{Type: "stdio", Command: []string{"echo"}},
			},
		}
		_, err := f.CreateClient(ctx, mcpServer)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("build failed"))
	})
})

var _ = Describe("DefaultMCPClientFactory CreateClient without builder (real client)", func() {
	var (
		ctx       context.Context
		scheme    *runtime.Scheme
		k8sClient client.Client
	)

	BeforeEach(func() {
		ctx = context.Background()
		scheme = runtime.NewScheme()
		utilruntime.Must(ottoflowv1alpha1.AddToScheme(scheme))
		utilruntime.Must(corev1.AddToScheme(scheme))
		k8sClient = fake.NewClientBuilder().WithScheme(scheme).Build()
	})

	It("creates realMCPClient for stdio transport when builder not set", func() {
		f := NewDefaultMCPClientFactory(k8sClient, true)
		mcpServer := &ottoflowv1alpha1.MCPServer{
			ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"},
			Spec: ottoflowv1alpha1.MCPServerSpec{
				Transport: ottoflowv1alpha1.TransportConfig{
					Type:    "stdio",
					Command: []string{"echo"},
				},
			},
		}
		c, err := f.CreateClient(ctx, mcpServer)
		Expect(err).NotTo(HaveOccurred())
		Expect(c).NotTo(BeNil())
		_ = c.Close()
	})
})

// mockMCPClientWithCloseError implements MCPClient with Close returning an error.
type mockMCPClientWithCloseError struct {
	mockMCPClientForBuildSessionTools
	err error
}

func (m *mockMCPClientWithCloseError) Close() error {
	return m.err
}

// mockMCPClientFactory implements MCPClientFactory for tests.
type mockMCPClientFactory struct {
	client      MCPClient
	err         error
	createCount int
}

func (m *mockMCPClientFactory) CreateClient(ctx context.Context, mcpServer *ottoflowv1alpha1.MCPServer) (MCPClient, error) {
	m.createCount++
	if m.err != nil {
		return nil, m.err
	}
	return m.client, nil
}
