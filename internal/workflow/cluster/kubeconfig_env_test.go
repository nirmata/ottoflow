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

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/agent"
	"github.com/nirmata/ottoflow/test/stdiomcp"
)

func TestMain(m *testing.M) {
	stdiomcp.ServeIfChild()
	os.Exit(m.Run())
}

func TestExportKubeConfigForClusterRef(t *testing.T) {
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
		{name: "nil clusterRef leaves KUBECONFIG alone", clusterRef: nil, want: ""},
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
			// Developer shells usually set KUBECONFIG; start from empty so only the export shows.
			t.Setenv("KUBECONFIG", "")
			err := ExportKubeConfigForClusterRef(tt.clusterRef)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ExportKubeConfigForClusterRef() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got := os.Getenv("KUBECONFIG"); got != tt.want {
				t.Errorf("KUBECONFIG = %q, want %q", got, tt.want)
			}
		})
	}
}

// A stdio MCP server started after the export, through the same client factory the runner uses,
// sees the target kubeconfig; an explicit KUBECONFIG in the MCPServer's spec.env still wins.
func TestExportedKubeConfigReachesStdioMCPServer(t *testing.T) {
	t.Setenv("KUBECONFIG", "")
	dir := t.TempDir()
	mounted := filepath.Join(dir, "config")
	if err := os.WriteFile(mounted, []byte(validKubeconfig), 0600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	useMountDir(t, dir)

	run := workflowRunWithKubeConfigSecretRef("")
	if err := ExportKubeConfigForClusterRef(run.Spec.ClusterRef); err != nil {
		t.Fatalf("ExportKubeConfigForClusterRef: %v", err)
	}

	if got, want := callStdioKubeConfigTool(t, nil), "KUBECONFIG="+mounted; got != want {
		t.Errorf("stdio MCP server saw %q, want %q", got, want)
	}
	specEnv := []corev1.EnvVar{{Name: "KUBECONFIG", Value: "/from/spec"}}
	if got, want := callStdioKubeConfigTool(t, specEnv), "KUBECONFIG=/from/spec"; got != want {
		t.Errorf("with spec.env KUBECONFIG, stdio MCP server saw %q, want %q", got, want)
	}
}

// callStdioKubeConfigTool starts this test binary as a stdio MCPServer with env added to its
// spec.env, and returns what its tool reports.
func callStdioKubeConfigTool(t *testing.T, env []corev1.EnvVar) string {
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := agent.NewDefaultMCPClientFactory(nil, false).CreateClient(ctx, server)
	if err != nil {
		t.Fatalf("CreateClient: %v", err)
	}
	defer func() { _ = c.Close() }()
	result, err := c.CallTool(ctx, stdiomcp.Tool, nil)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	return fmt.Sprint(result)
}
