/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package cluster

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/agent"
	"github.com/nirmata/ottoflow/internal/workflow/executor"
	"github.com/nirmata/ottoflow/test/stdiomcp"
)

func TestMain(m *testing.M) {
	stdiomcp.ServeIfChild()
	os.Exit(m.Run())
}

func TestKubeConfigPathForClusterRef(t *testing.T) {
	dir := t.TempDir()
	// Only the second default key exists, so the probe order is exercised.
	mounted := filepath.Join(dir, "kubeconfig")
	explicit := filepath.Join(dir, "value")
	for _, p := range []string{mounted, explicit} {
		if err := os.WriteFile(p, []byte(validKubeconfig), 0600); err != nil {
			t.Fatalf("write kubeconfig: %v", err)
		}
	}
	useMountDir(t, dir)
	local := true

	tests := []struct {
		name       string
		clusterRef *ottoflowv1alpha1.ClusterRef
		want       string
		wantErr    bool
	}{
		{name: "nil clusterRef returns no path", clusterRef: nil, want: ""},
		{
			name:       "local wins over a file path, as in RestConfigForClusterRef",
			clusterRef: &ottoflowv1alpha1.ClusterRef{Local: &local, KubeConfigFilePath: "/etc/target/kubeconfig"},
			want:       "",
		},
		{
			name:       "kubeConfigFilePath",
			clusterRef: &ottoflowv1alpha1.ClusterRef{KubeConfigFilePath: "/etc/target/kubeconfig"},
			want:       "/etc/target/kubeconfig",
		},
		{
			name:       "kubeConfigSecretRef probes the default keys",
			clusterRef: &ottoflowv1alpha1.ClusterRef{KubeConfigSecretRef: &ottoflowv1alpha1.KubeConfigSecretRef{Name: "target"}},
			want:       mounted,
		},
		{
			name: "kubeConfigSecretRef with an explicit key",
			clusterRef: &ottoflowv1alpha1.ClusterRef{
				KubeConfigSecretRef: &ottoflowv1alpha1.KubeConfigSecretRef{Name: "target", Key: "value"},
			},
			want: explicit,
		},
		{
			name: "kubeConfigSecretRef with a missing key",
			clusterRef: &ottoflowv1alpha1.ClusterRef{
				KubeConfigSecretRef: &ottoflowv1alpha1.KubeConfigSecretRef{Name: "target", Key: "absent"},
			},
			wantErr: true,
		},
		{
			name: "kubeConfigSecretRef with a traversal key",
			clusterRef: &ottoflowv1alpha1.ClusterRef{
				KubeConfigSecretRef: &ottoflowv1alpha1.KubeConfigSecretRef{Name: "target", Key: "../escape"},
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := KubeConfigPathForClusterRef(tt.clusterRef)
			if (err != nil) != tt.wantErr {
				t.Fatalf("KubeConfigPathForClusterRef() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("path = %q, want %q", got, tt.want)
			}
		})
	}
}

// A stdio MCP server started through the runner's MCP manager gets the path the context carries
// as KUBECONFIG, not the shell's; an explicit KUBECONFIG in the MCPServer's spec.env still wins.
func TestKubeConfigPathReachesStdioMCPServer(t *testing.T) {
	t.Setenv("KUBECONFIG", "/ambient")
	dir := t.TempDir()
	mounted := filepath.Join(dir, "config")
	if err := os.WriteFile(mounted, []byte(validKubeconfig), 0600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	useMountDir(t, dir)

	run := workflowRunWithKubeConfigSecretRef("")
	path, err := KubeConfigPathForClusterRef(run.Spec.ClusterRef)
	if err != nil {
		t.Fatalf("KubeConfigPathForClusterRef: %v", err)
	}
	ctx := agent.WithKubeConfigPath(context.Background(), path)

	if got, want := callStdioKubeConfigTool(t, ctx, nil), "KUBECONFIG="+mounted; got != want {
		t.Errorf("stdio MCP server saw %q, want %q", got, want)
	}
	specEnv := []corev1.EnvVar{{Name: "KUBECONFIG", Value: "/from/spec"}}
	if got, want := callStdioKubeConfigTool(t, ctx, specEnv), "KUBECONFIG=/from/spec"; got != want {
		t.Errorf("with spec.env KUBECONFIG, stdio MCP server saw %q, want %q", got, want)
	}
}

// callStdioKubeConfigTool starts this test binary as a stdio MCPServer with env added to its
// spec.env, through the runner's MCP manager, and returns what its tool reports.
func callStdioKubeConfigTool(t *testing.T, ctx context.Context, env []corev1.EnvVar) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	server := &ottoflowv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "env-probe", Namespace: "default"},
		Spec: ottoflowv1alpha1.MCPServerSpec{
			Transport: ottoflowv1alpha1.TransportConfig{Type: "stdio", Command: []string{exe}},
			Env:       append([]corev1.EnvVar{{Name: stdiomcp.ServeEnv, Value: "1"}}, env...),
		},
	}
	manager, err := executor.NewMCPManager(fake.NewClientBuilder().WithScheme(newResolveTestScheme(t)).WithObjects(server).Build(), false)
	if err != nil {
		t.Fatalf("NewMCPManager: %v", err)
	}
	defer func() { _ = manager.Close() }()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c, err := manager.GetClient(ctx, server.Name, server.Namespace)
	if err != nil {
		t.Fatalf("GetClient: %v", err)
	}
	result, err := c.CallTool(ctx, stdiomcp.Tool, nil)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	return fmt.Sprint(result)
}
