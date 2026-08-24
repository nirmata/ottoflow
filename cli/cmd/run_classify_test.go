/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

func TestLooksLikeURL(t *testing.T) {
	cases := []struct {
		arg  string
		want bool
	}{
		{"https://example.com/foo.yaml", true},
		{"http://example.com/foo.yaml", true},
		{"HTTPS://EXAMPLE.COM/FOO.YAML", true},
		{"samples/foo.yaml", false},
		{"foo.yaml", false},
		{"-", false},
		{"", false},
		{"ftp://example.com/foo.yaml", false},
	}
	for _, tc := range cases {
		if got := looksLikeURL(tc.arg); got != tc.want {
			t.Errorf("looksLikeURL(%q) = %v, want %v", tc.arg, got, tc.want)
		}
	}
}

const workflowManifest = `apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: cluster-overview
  namespace: ottoflow
spec:
  steps: []
`

const workflowRunManifest = `apiVersion: ottoflow.nirmata.io/v1alpha1
kind: WorkflowRun
metadata:
  generateName: cluster-overview-
  namespace: ottoflow
spec:
  workflowRef:
    name: cluster-overview
    namespace: ottoflow
`

func TestParseWorkflowRunDoc_FindsWorkflowRun(t *testing.T) {
	wr, ok := parseWorkflowRunDoc([]byte(workflowRunManifest))
	if !ok {
		t.Fatal("expected a WorkflowRun to be found")
	}
	if wr.Spec.WorkflowRef.Name != "cluster-overview" {
		t.Errorf("expected workflowRef.name cluster-overview, got %q", wr.Spec.WorkflowRef.Name)
	}
}

func TestParseWorkflowRunDoc_ReturnsFalseForWorkflow(t *testing.T) {
	if _, ok := parseWorkflowRunDoc([]byte(workflowManifest)); ok {
		t.Fatal("expected a Workflow document to not be classified as a WorkflowRun")
	}
}

func TestParseWorkflowRunDoc_ReturnsFalseForNoKind(t *testing.T) {
	if _, ok := parseWorkflowRunDoc([]byte("just: some yaml\n")); ok {
		t.Fatal("expected data with no Kind to not be classified as a WorkflowRun")
	}
}

// A multi-document manifest (e.g. an Agent bundled with a WorkflowRun) must find the
// WorkflowRun regardless of which document it's in.
func TestParseWorkflowRunDoc_MultiDocFindsWorkflowRunAfterOtherKinds(t *testing.T) {
	multiDoc := "apiVersion: ottoflow.nirmata.io/v1alpha1\nkind: Agent\nmetadata:\n  name: a\n" +
		"spec:\n  prompt: hi\n  modelProvider: local\n---\n" + workflowRunManifest
	wr, ok := parseWorkflowRunDoc([]byte(multiDoc))
	if !ok {
		t.Fatal("expected the WorkflowRun in the second document to be found")
	}
	if wr.Spec.WorkflowRef.Name != "cluster-overview" {
		t.Errorf("expected workflowRef.name cluster-overview, got %q", wr.Spec.WorkflowRef.Name)
	}
}

func TestClassifyRunSource_LocalWorkflowFileReturnsManifest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cluster-overview.yaml")
	if err := os.WriteFile(path, []byte(workflowManifest), 0600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	manifest, run, err := classifyRunSource(&cobra.Command{}, context.Background(), path)
	if err != nil {
		t.Fatalf("classifyRunSource: %v", err)
	}
	if run != nil {
		t.Fatalf("expected a Workflow file to classify as local execution (nil run), got %+v", run)
	}
	if string(manifest) != workflowManifest {
		t.Errorf("expected the fetched bytes to be returned as the manifest, got %q", manifest)
	}
}

func TestClassifyRunSource_LocalWorkflowRunFileReturnsRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.yaml")
	if err := os.WriteFile(path, []byte(workflowRunManifest), 0600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	manifest, run, err := classifyRunSource(&cobra.Command{}, context.Background(), path)
	if err != nil {
		t.Fatalf("classifyRunSource: %v", err)
	}
	if run == nil {
		t.Fatal("expected a WorkflowRun file to classify as a WorkflowRun to apply in-cluster")
	}
	if manifest != nil {
		t.Errorf("expected no manifest bytes when a WorkflowRun is found, got %q", manifest)
	}
	if run.Spec.WorkflowRef.Name != "cluster-overview" {
		t.Errorf("expected workflowRef.name cluster-overview, got %q", run.Spec.WorkflowRef.Name)
	}
}

func TestClassifyRunSource_MissingFileSurfacesError(t *testing.T) {
	_, _, err := classifyRunSource(&cobra.Command{}, context.Background(), filepath.Join(t.TempDir(), "missing.yaml"))
	if err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

// The whole point of classifyRunSource is that a URL is fetched exactly once and the bytes are
// reused by the caller -- never fetched a second time. Prove it by counting requests.
func TestClassifyRunSource_URLFetchedExactlyOnce(t *testing.T) {
	withAllowInsecureURL(t, true)
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, _ = w.Write([]byte(workflowManifest))
	}))
	defer srv.Close()

	manifest, run, err := classifyRunSource(&cobra.Command{}, context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("classifyRunSource: %v", err)
	}
	if run != nil {
		t.Fatalf("expected a Workflow URL to classify as local execution, got %+v", run)
	}
	if string(manifest) != workflowManifest {
		t.Errorf("expected the fetched body to be returned as the manifest, got %q", manifest)
	}
	if requests != 1 {
		t.Errorf("expected exactly 1 HTTP request, got %d", requests)
	}
}

func TestClassifyRunSource_URLWorkflowRunClassifiedForCluster(t *testing.T) {
	withAllowInsecureURL(t, true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(workflowRunManifest))
	}))
	defer srv.Close()

	_, run, err := classifyRunSource(&cobra.Command{}, context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("classifyRunSource: %v", err)
	}
	if run == nil {
		t.Fatal("expected a WorkflowRun served from a URL to be classified for in-cluster apply")
	}
}

// runWorkflowFromStream must not re-fetch when handed preloaded data -- regression guard for the
// no-double-fetch property classifyRunSource exists to provide. runFile is deliberately pointed
// at an address nothing listens on: if runWorkflowFromStream ever fetches instead of reusing
// preloadedData, this dials and fails immediately.
func TestRunWorkflowFromStream_UsesPreloadedDataWithoutRefetching(t *testing.T) {
	oldRunFile, oldNamespace := runFile, namespace
	runFile = "http://127.0.0.1:1/unreachable.yaml"
	namespace = "ottoflow"
	t.Cleanup(func() { runFile, namespace = oldRunFile, oldNamespace })

	err := runWorkflowFromStream(&cobra.Command{}, context.Background(), nil, []byte(workflowManifest))
	if err != nil {
		t.Fatalf("expected preloaded data to be used without a network fetch, got error: %v", err)
	}
}

func fakeClientForOttoflow(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(ottoflowv1alpha1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// A preloaded WorkflowRun (from a bare file-path/URL argument classified by classifyRunSource)
// must be applied in-cluster as-is, bypassing resolveWorkflowRunSpec's normal
// name-from-args/--workflow resolution entirely -- proven here by calling with nil args and no
// --workflow flag, which resolveWorkflowRunSpec would otherwise reject as "workflow name is
// required".
func TestRunWorkflowInCluster_PreloadedRunBypassesNameResolution(t *testing.T) {
	oldWatch, oldWorkflowName, oldNamespace := watch, workflowName, namespace
	watch, workflowName, namespace = false, "", "ottoflow"
	t.Cleanup(func() { watch, workflowName, namespace = oldWatch, oldWorkflowName, oldNamespace })

	wf := &ottoflowv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-overview", Namespace: "ottoflow"},
	}
	k8sClient := fakeClientForOttoflow(t, wf)

	preloaded := &ottoflowv1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "cluster-overview-", Namespace: "ottoflow"},
		Spec: ottoflowv1alpha1.WorkflowRunSpec{
			WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "cluster-overview", Namespace: "ottoflow"},
		},
	}

	if err := runWorkflowInCluster(context.Background(), k8sClient, nil, preloaded); err != nil {
		t.Fatalf("runWorkflowInCluster with a preloaded run: %v", err)
	}

	var runs ottoflowv1alpha1.WorkflowRunList
	if err := k8sClient.List(context.Background(), &runs); err != nil {
		t.Fatalf("list WorkflowRuns: %v", err)
	}
	if len(runs.Items) != 1 {
		t.Fatalf("expected exactly 1 WorkflowRun created, got %d", len(runs.Items))
	}
	if got := runs.Items[0].Spec.WorkflowRef.Name; got != "cluster-overview" {
		t.Errorf("expected workflowRef.name cluster-overview, got %q", got)
	}
}

// Without a preloaded run, the normal args/--workflow resolution still applies and rejects a
// missing workflow name -- confirms the bypass above is specific to the preloaded path, not a
// change in the no-args behavior.
func TestRunWorkflowInCluster_NoPreloadedRunRequiresWorkflowName(t *testing.T) {
	oldWorkflowName := workflowName
	workflowName = ""
	t.Cleanup(func() { workflowName = oldWorkflowName })

	k8sClient := fakeClientForOttoflow(t)
	err := runWorkflowInCluster(context.Background(), k8sClient, nil, nil)
	if err == nil {
		t.Fatal("expected an error when no workflow name is available and no run is preloaded")
	}
	if !strings.Contains(err.Error(), "workflow name is required") {
		t.Errorf("expected a 'workflow name is required' error, got: %v", err)
	}
}
