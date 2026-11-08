package controller

import (
	"context"
	"slices"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

// waitOpen waits for the i-th watch to be opened (the re-open backs off by watchReopenDelay).
func (f *fakeWatcherFactory) waitOpen(t *testing.T, i int) *fakeWatch {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if w := f.get(i); w != nil {
			return w
		}
	}
	t.Fatalf("watch %d was never opened", i)
	return nil
}

// resumeFixture registers a ConfigMap event trigger with selectors (and optional operations)
// on the shared fake factory. Each open's options are read from f.lastOpts right after
// waitOpen: the next open cannot happen until the test closes that watch.
func resumeFixture(t *testing.T, ops ...string) (*fakeWatcherFactory, func() int) {
	t.Helper()
	f := &fakeWatcherFactory{}
	return f, resumeFixtureWith(t, f, ops...)
}

func resumeFixtureWith(t *testing.T, f ResourceWatcherFactory, ops ...string) func() int {
	t.Helper()
	wf := eventTestWorkflow("resume-wf", ottoflowv1alpha1.Trigger{Event: &ottoflowv1alpha1.EventTrigger{
		Resources:     []ottoflowv1alpha1.EventResource{{APIVersion: "v1", Kind: "ConfigMap", Namespace: "default"}},
		LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "x"}},
		FieldSelector: "metadata.name!=skip",
		Operations:    ops,
	}})
	fc := makeFakeClient(wf)
	tm := NewTriggerManagerWithWatcherFactory(fc, unitTestScheme, nil, f)
	tm.SetEventTriggerAllowedNamespaces([]string{"default"})
	if err := tm.RegisterWorkflow(t.Context(), wf); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tm.UnregisterWorkflow(context.Background(), wf) })
	return func() int { return countRuns(t, fc) }
}

func configMapAt(rv string) *unstructured.Unstructured {
	u := makeUnstructured("cm1", "default", map[string]interface{}{"apiVersion": "v1", "kind": "ConfigMap"})
	u.SetResourceVersion(rv)
	return u
}

func bookmarkAt(rv string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]interface{}{"resourceVersion": rv},
	}}
}

// A re-opened watch resumes from the first BOOKMARK's resourceVersion (not the ADDED
// burst's), keeps the selectors, and a BOOKMARK never creates a run.
func TestEventWatchReopenResumesFromBookmark(t *testing.T) {
	t.Parallel() // ~1 s in the re-open backoff (watchReopenDelay)
	f, runs := resumeFixture(t)
	w0 := f.waitOpen(t, 0)
	o0 := f.lastOpts
	w0.ch <- watch.Event{Type: watch.Added, Object: configMapAt("100")}
	w0.ch <- watch.Event{Type: watch.Bookmark, Object: bookmarkAt("120")}
	w0.Stop()
	f.waitOpen(t, 1)
	o1 := f.lastOpts

	if o0.ResourceVersion != "" || !o0.AllowWatchBookmarks {
		t.Errorf("open 0: resourceVersion=%q allowWatchBookmarks=%v, want empty and true", o0.ResourceVersion, o0.AllowWatchBookmarks)
	}
	if o1.ResourceVersion != "120" || !o1.AllowWatchBookmarks {
		t.Errorf("open 1: resourceVersion=%q allowWatchBookmarks=%v, want 120 and true", o1.ResourceVersion, o1.AllowWatchBookmarks)
	}
	if o1.LabelSelector != o0.LabelSelector || o1.FieldSelector != o0.FieldSelector {
		t.Errorf("selectors changed on re-open: %q/%q -> %q/%q", o0.LabelSelector, o0.FieldSelector, o1.LabelSelector, o1.FieldSelector)
	}
	if n := runs(); n != 1 {
		t.Errorf("runs = %d, want 1 (the ADDED only; a BOOKMARK must not run)", n)
	}
}

// A stream cut before any BOOKMARK re-opens fresh: the ADDED burst carries each object's own
// RV, not a resume point, so an UPDATE-only trigger must not see replayed history.
func TestEventWatchReopenBeforeBookmarkStartsFresh(t *testing.T) {
	t.Parallel() // ~1 s in the re-open backoff (watchReopenDelay)
	f, runs := resumeFixture(t, "UPDATE")
	w0 := f.waitOpen(t, 0)
	w0.ch <- watch.Event{Type: watch.Added, Object: configMapAt("211")}
	w0.Stop()
	f.waitOpen(t, 1)

	if o := f.lastOpts; o.ResourceVersion != "" {
		t.Errorf("open 1 resourceVersion = %q, want empty before any bookmark", o.ResourceVersion)
	}
	if n := runs(); n != 0 {
		t.Errorf("runs = %d, want 0 (an UPDATE-only trigger ignores the ADDED burst)", n)
	}
}

// A 410 ERROR event makes no run and drops the kept resourceVersion, so the re-open
// is a fresh one instead of looping on a stale version.
func TestEventWatchReopenAfterGoneStartsFresh(t *testing.T) {
	t.Parallel() // ~1 s in the re-open backoff (watchReopenDelay)
	f, runs := resumeFixture(t)
	w0 := f.waitOpen(t, 0)
	w0.ch <- watch.Event{Type: watch.Bookmark, Object: bookmarkAt("130")}
	w0.ch <- watch.Event{Type: watch.Error, Object: &apierrors.NewResourceExpired("too old resource version").ErrStatus}
	w0.Stop()
	f.waitOpen(t, 1)

	if o := f.lastOpts; o.ResourceVersion != "" {
		t.Errorf("open 1 resourceVersion = %q, want empty after a 410", o.ResourceVersion)
	}
	if n := runs(); n != 0 {
		t.Errorf("runs = %d, want 0 (neither the BOOKMARK nor the ERROR may run)", n)
	}
}

// goneOnOpen fails the n-th Watch call (0-based) with a direct 410, as a Watch call itself can.
type goneOnOpen struct {
	*fakeWatcherFactory
	n, calls int
	rvs      []string // ResourceVersion of every Watch call
}

func (g *goneOnOpen) Watch(ctx context.Context, gvr schema.GroupVersionResource, ns string, opts metav1.ListOptions) (watch.Interface, error) {
	g.mu.Lock()
	i := g.calls
	g.calls++
	g.rvs = append(g.rvs, opts.ResourceVersion)
	g.mu.Unlock()
	if i == g.n {
		return nil, apierrors.NewResourceExpired("too old resource version")
	}
	return g.fakeWatcherFactory.Watch(ctx, gvr, ns, opts)
}

// A 410 returned by the Watch call itself (not an in-stream ERROR) drops the kept
// resourceVersion: the next open is fresh and its stream is untrusted until a BOOKMARK.
func TestEventWatchReopenAfterDirectGoneStartsFresh(t *testing.T) {
	t.Parallel() // ~4 s in the re-open backoffs (watchReopenDelay, then doubled)
	f := &fakeWatcherFactory{}
	g := &goneOnOpen{fakeWatcherFactory: f, n: 1}
	runs := resumeFixtureWith(t, g, "UPDATE")
	w0 := f.waitOpen(t, 0)
	w0.ch <- watch.Event{Type: watch.Bookmark, Object: bookmarkAt("140")}
	w0.Stop()
	w1 := f.waitOpen(t, 1) // Watch call 1 got the 410; call 2 is the retry
	w1.ch <- watch.Event{Type: watch.Added, Object: configMapAt("150")}
	w1.Stop()
	f.waitOpen(t, 2)

	f.mu.Lock()
	defer f.mu.Unlock()
	if want := []string{"", "140", "", ""}; !slices.Equal(g.rvs, want) {
		t.Errorf("Watch resourceVersions = %q, want %q (410 on call 1, then fresh, and fresh again: no BOOKMARK on the fresh stream)", g.rvs, want)
	}
	if n := runs(); n != 0 {
		t.Errorf("runs = %d, want 0 (an UPDATE-only trigger ignores the replayed ADDED)", n)
	}
}
