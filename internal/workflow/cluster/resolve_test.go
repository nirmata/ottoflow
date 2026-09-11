/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package cluster

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

// validKubeconfig is a minimal but structurally valid kubeconfig, parseable by
// clientcmd.RESTConfigFromKubeConfig.
const validKubeconfig = `
apiVersion: v1
kind: Config
clusters:
- name: test
  cluster:
    server: https://example.invalid:6443
contexts:
- name: test
  context:
    cluster: test
    user: test
current-context: test
users:
- name: test
  user:
    token: fake-token
`

func newResolveTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(ottoflowv1alpha1.AddToScheme(s))
	return s
}

// failGetClient wraps a fake client so any Get call fails the test — used to prove a code
// path never touches the Kubernetes API.
func failGetClient(t *testing.T, base client.WithWatch) client.Client {
	t.Helper()
	return interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			t.Fatalf("unexpected Secret API Get for %s/%s: the in-cluster runner must never call the Secret API", key.Namespace, key.Name)
			return nil
		},
	})
}

// useMountDir points the kubeconfig probe at dir for the duration of the test.
func useMountDir(t *testing.T, dir string) {
	t.Helper()
	origDir := kubeconfigMountDir
	kubeconfigMountDir = dir
	t.Cleanup(func() { kubeconfigMountDir = origDir })
}

func workflowRunWithKubeConfigSecretRef(key string) *ottoflowv1alpha1.WorkflowRun {
	return &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "run"},
		Spec: ottoflowv1alpha1.WorkflowRunSpec{
			ClusterRef: &ottoflowv1alpha1.ClusterRef{
				KubeConfigSecretRef: &ottoflowv1alpha1.KubeConfigSecretRef{
					Name: "target-kubeconfig",
					Key:  key,
				},
			},
		},
	}
}

// TestRestConfigForClusterRef_MountedFileUsedWithoutAPICall proves that when a kubeconfig
// file exists at the mounted path, it is used directly and the Secret API is never touched
// — regardless of localExecutionMode.
func TestRestConfigForClusterRef_MountedFileUsedWithoutAPICall(t *testing.T) {
	dir := t.TempDir()
	useMountDir(t, dir)

	if err := os.WriteFile(filepath.Join(dir, "config"), []byte(validKubeconfig), 0o600); err != nil {
		t.Fatalf("write mounted kubeconfig: %v", err)
	}

	scheme := newResolveTestScheme(t)
	fakeClient := failGetClient(t, fake.NewClientBuilder().WithScheme(scheme).Build())

	run := workflowRunWithKubeConfigSecretRef("")

	for _, localExecutionMode := range []bool{true, false} {
		restConfig, err := RestConfigForClusterRef(context.Background(), fakeClient, run, localExecutionMode)
		if err != nil {
			t.Fatalf("localExecutionMode=%v: unexpected error: %v", localExecutionMode, err)
		}
		if restConfig.Host != "https://example.invalid:6443" {
			t.Fatalf("localExecutionMode=%v: unexpected host %q", localExecutionMode, restConfig.Host)
		}
	}
}

// TestRestConfigForClusterRef_ExplicitKeyProbesOnlyThatFile proves that an explicit
// kubeConfigSecretRef.key selects exactly that file under the mount, and the default key
// names are not probed as a fallback.
func TestRestConfigForClusterRef_ExplicitKeyProbesOnlyThatFile(t *testing.T) {
	dir := t.TempDir()
	useMountDir(t, dir)

	// Only the default-named file exists; the explicit key names a file that does not.
	if err := os.WriteFile(filepath.Join(dir, "config"), []byte(validKubeconfig), 0o600); err != nil {
		t.Fatalf("write mounted kubeconfig: %v", err)
	}

	scheme := newResolveTestScheme(t)
	fakeClient := failGetClient(t, fake.NewClientBuilder().WithScheme(scheme).Build())

	_, err := RestConfigForClusterRef(context.Background(), fakeClient, workflowRunWithKubeConfigSecretRef("custom.yaml"), false)
	if err == nil {
		t.Fatal("expected an error: the explicit key names a file that is not mounted")
	}
	if !strings.Contains(err.Error(), "[custom.yaml]") {
		t.Errorf("expected the error to list only the explicit key as tried, got: %v", err)
	}
}

// TestRestConfigForClusterRef_RunnerModeErrorsWithoutAPICall proves that when no mounted
// file exists and localExecutionMode is false (the in-cluster runner), resolution fails
// without ever calling the Secret API.
func TestRestConfigForClusterRef_RunnerModeErrorsWithoutAPICall(t *testing.T) {
	dir := t.TempDir() // empty: no mounted kubeconfig file
	useMountDir(t, dir)

	scheme := newResolveTestScheme(t)
	fakeClient := failGetClient(t, fake.NewClientBuilder().WithScheme(scheme).Build())

	run := workflowRunWithKubeConfigSecretRef("")

	_, err := RestConfigForClusterRef(context.Background(), fakeClient, run, false)
	if err == nil {
		t.Fatal("expected an error when no mounted kubeconfig exists and localExecutionMode is false")
	}
	if !strings.Contains(err.Error(), "not found in mounted secret directory") {
		t.Errorf("expected the mount-miss error, got: %v", err)
	}
}

// TestRestConfigForClusterRef_LocalModeFallsBackToAPI proves that when no mounted file
// exists and localExecutionMode is true (the CLI), resolution falls back to the live Secret
// API read.
func TestRestConfigForClusterRef_LocalModeFallsBackToAPI(t *testing.T) {
	dir := t.TempDir() // empty: no mounted kubeconfig file
	useMountDir(t, dir)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "target-kubeconfig"},
		Data:       map[string][]byte{"config": []byte(validKubeconfig)},
	}

	scheme := newResolveTestScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()

	run := workflowRunWithKubeConfigSecretRef("")

	restConfig, err := RestConfigForClusterRef(context.Background(), fakeClient, run, true)
	if err != nil {
		t.Fatalf("unexpected error falling back to API in local execution mode: %v", err)
	}
	if restConfig.Host != "https://example.invalid:6443" {
		t.Fatalf("unexpected host %q", restConfig.Host)
	}
}

// clusterRef.kubeConfigSecretRef.key is a free-form CRD string the API server never validates
// as a Secret data key, and the runner joins it into a filesystem path under the kubeconfig
// mount. Without the secretmount.ValidateDataKey check it is a path-traversal input: the runner
// reads (and reports errors about) a file that is not the kubeconfig at all — the runner pod
// mounts its own ServiceAccount token under /var/run/secrets/kubernetes.io/serviceaccount.
func TestRestConfigForClusterRef_RejectsTraversalInKubeConfigSecretRefKey(t *testing.T) {
	dir := t.TempDir()
	// Plant the traversal target so the test fails loudly if the escape ever succeeds, rather
	// than passing because the file happened not to exist.
	outside := filepath.Join(filepath.Dir(dir), "escaped-target")
	if err := os.WriteFile(outside, []byte(validKubeconfig), 0o600); err != nil {
		t.Fatalf("write traversal target: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(outside) })

	useMountDir(t, dir)

	for _, key := range []string{
		"../escaped-target",
		"../../../../var/run/secrets/kubernetes.io/serviceaccount/token",
		"..",
		".",
		"sub/dir",
	} {
		t.Run(key, func(t *testing.T) {
			wr := workflowRunWithKubeConfigSecretRef(key)
			// localExecutionMode=false (the in-cluster runner) and =true (the CLI) must BOTH
			// refuse: the same unusable key would only ever miss on the live-Secret path too.
			for _, local := range []bool{false, true} {
				_, err := RestConfigForClusterRef(context.Background(), nil, wr, local)
				if err == nil {
					t.Fatalf("localExecutionMode=%v: expected key %q to be rejected", local, key)
				}
				if !strings.Contains(err.Error(), "kubeConfigSecretRef.key is unusable") {
					t.Errorf("localExecutionMode=%v: expected the key-validation error, got: %v", local, err)
				}
			}
		})
	}
}

// A legitimate key must still resolve, so the guard above cannot drift into refusing real
// configurations.
func TestRestConfigForClusterRef_AcceptsValidKubeConfigSecretRefKey(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "my-config.yaml"), []byte(validKubeconfig), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	useMountDir(t, dir)

	wr := workflowRunWithKubeConfigSecretRef("my-config.yaml")
	if _, err := RestConfigForClusterRef(context.Background(), nil, wr, false); err != nil {
		t.Fatalf("a valid Secret data key must resolve from the mount: %v", err)
	}
}
