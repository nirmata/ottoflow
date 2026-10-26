/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func lockScopeScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	return s
}

func TestCELNonMacroEvaluationsRunConcurrently(t *testing.T) {
	const n = 4
	var inFlight, maxInFlight int32
	var arrived int32
	all := make(chan struct{})
	c := interceptor.NewClient(fake.NewClientBuilder().WithScheme(lockScopeScheme()).Build(), interceptor.Funcs{
		List: func(ctx context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
			cur := atomic.AddInt32(&inFlight, 1)
			defer atomic.AddInt32(&inFlight, -1)
			for {
				m := atomic.LoadInt32(&maxInFlight)
				if cur <= m || atomic.CompareAndSwapInt32(&maxInFlight, m, cur) {
					break
				}
			}
			if atomic.AddInt32(&arrived, 1) == n {
				close(all)
			}
			select {
			case <-all:
			case <-time.After(500 * time.Millisecond):
			}
			return nil
		},
	})
	ev, err := NewCELEvaluator(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := ev.EvaluateExpression(context.Background(), `size(resource.List("v1", "configmaps", "default").items) >= 0`, nil); err != nil {
				t.Errorf("eval: %v", err)
			}
		}()
	}
	wg.Wait()
	t.Logf("maxInFlight=%d wall=%s", maxInFlight, time.Since(start).Round(time.Millisecond))
	if maxInFlight != n {
		t.Fatalf("expected %d concurrent evaluations inside prg.Eval, got max %d", n, maxInFlight)
	}
}

type lockScopeKey struct{}

func TestCELMacroSeesOwnEvaluationContext(t *testing.T) {
	aIn, bIn := make(chan struct{}), make(chan struct{})
	var aOnce, bOnce sync.Once
	var mu sync.Mutex
	var mismatches []string
	c := interceptor.NewClient(fake.NewClientBuilder().WithScheme(lockScopeScheme()).Build(), interceptor.Funcs{
		List: func(ctx context.Context, _ client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*corev1.EventList); !ok {
				return nil
			}
			lo := &client.ListOptions{}
			lo.ApplyOptions(opts)
			who := "a"
			if strings.Contains(lo.FieldSelector.String(), "involvedObject.name=b") {
				who = "b"
			}
			if got, _ := ctx.Value(lockScopeKey{}).(string); got != who {
				mu.Lock()
				mismatches = append(mismatches, who+" saw ctx of "+got)
				mu.Unlock()
			}
			if who == "b" {
				bOnce.Do(func() { close(bIn) })
				return nil
			}
			first := false
			aOnce.Do(func() { first = true; close(aIn) })
			if first {
				select {
				case <-bIn:
				case <-time.After(500 * time.Millisecond):
				}
			}
			return nil
		},
	})
	ev, err := NewCELEvaluator(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		ctx := context.WithValue(context.Background(), lockScopeKey{}, "a")
		if _, err := ev.EvaluateExpression(ctx, `size(resourceEvents("v1", "Pod", "default", "a")) + size(resourceEvents("v1", "Pod", "default", "a"))`, nil); err != nil {
			t.Errorf("eval a: %v", err)
		}
	}()
	<-aIn
	go func() {
		defer wg.Done()
		ctx := context.WithValue(context.Background(), lockScopeKey{}, "b")
		if _, err := ev.EvaluateExpression(ctx, `size(resourceEvents("v1", "Pod", "default", "b"))`, nil); err != nil {
			t.Errorf("eval b: %v", err)
		}
	}()
	wg.Wait()
	if len(mismatches) > 0 {
		t.Fatalf("macro closure saw another evaluation's context: %v", mismatches)
	}
}

func TestCELMacroLockClassifier(t *testing.T) {
	ev, err := NewCELEvaluator(fake.NewClientBuilder().WithScheme(lockScopeScheme()).Build(), nil)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		expr string
		want bool
	}{
		{`resource.List("v1", "pods", "default")`, false},
		{`resource.Get("v1", "pods", "default", "x")`, false},
		{`format("%s", ["x"])`, false},
		{`http.Get("http://example.invalid", {})`, false},
		{`resourceEvents("v1", "Pod", "default", "a")`, true},
		{`resource.GetLogs("default", "p", "c", 100)`, true},
		{`resourceLogs("default", "p", "c", "x", "y")`, true},
		{`resourceMetrics("v1", "Pod", "default", "a", "cpu")`, true},
		{`resourceMetricsList("default")`, true},
		{`prometheusMetrics("up", "30s")`, true},
		{`[1, 2].map(x, size(resourceEvents("v1", "Pod", "default", "a")))`, true},
		{`variables.items.filter(i, size(resourceEvents("v1", "Pod", "default", i.name)) > 0)`, true},
		{`true ? resourceMetricsList("default") : []`, true},
		{`false ? [] : resourceMetricsList("default")`, true},
		{`has(variables.x) && size(resourceEvents("v1", "Pod", "default", "a")) > 0`, true},
		{`format("%s", [string(size(resourceEvents("v1", "Pod", "default", "a")))])`, true},
		{`resourceEvents(variables.a, variables.b, variables.c, variables.d)`, true},
		{`resourceEvents(dyn("v1"), dyn("Pod"), dyn("default"), dyn("a"))`, true},
		{`{"k": resourceMetricsList("default")}`, true},
		{`[resourceMetricsList("default")]`, true},
		{`"resourceEvents(\"v1\", \"Pod\", \"default\", \"a\")"`, false},
	}
	for _, c := range cases {
		if _, err := ev.getOrCompileProgram(c.expr); err != nil {
			t.Errorf("compile %q: %v", c.expr, err)
			continue
		}
		cp, _ := ev.getOrCompileProgram(c.expr)
		if cp.readsMacroCtx != c.want {
			t.Errorf("%q: got locked=%v want %v", c.expr, cp.readsMacroCtx, c.want)
		}
		if cp2, _ := ev.getOrCompileProgram(c.expr); cp2.readsMacroCtx != c.want {
			t.Errorf("%q: flag lost on cache hit", c.expr)
		}
	}
}

func TestCELMacroAndNonMacroEvaluationsOverlap(t *testing.T) {
	var inFlight, maxInFlight int32
	var mismatches int32
	listIn := make(chan struct{})
	var once sync.Once
	c := interceptor.NewClient(fake.NewClientBuilder().WithScheme(lockScopeScheme()).Build(), interceptor.Funcs{
		List: func(ctx context.Context, _ client.WithWatch, list client.ObjectList, _ ...client.ListOption) error {
			cur := atomic.AddInt32(&inFlight, 1)
			defer atomic.AddInt32(&inFlight, -1)
			for {
				m := atomic.LoadInt32(&maxInFlight)
				if cur <= m || atomic.CompareAndSwapInt32(&maxInFlight, m, cur) {
					break
				}
			}
			if _, isEvents := list.(*corev1.EventList); isEvents {
				if got, _ := ctx.Value(lockScopeKey{}).(string); got != "macro" {
					atomic.AddInt32(&mismatches, 1)
				}
				return nil
			}
			once.Do(func() { close(listIn) })
			time.Sleep(300 * time.Millisecond)
			return nil
		},
	})
	ev, err := NewCELEvaluator(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		ctx := context.WithValue(context.Background(), lockScopeKey{}, "plain")
		if _, err := ev.EvaluateExpression(ctx, `size(resource.List("v1", "configmaps", "default").items) >= 0`, nil); err != nil {
			t.Errorf("plain: %v", err)
		}
	}()
	<-listIn
	go func() {
		defer wg.Done()
		ctx := context.WithValue(context.Background(), lockScopeKey{}, "macro")
		if _, err := ev.EvaluateExpression(ctx, `size(resourceEvents("v1", "Pod", "default", "a"))`, nil); err != nil {
			t.Errorf("macro: %v", err)
		}
	}()
	wg.Wait()
	t.Logf("maxInFlight=%d mismatches=%d", maxInFlight, mismatches)
	if maxInFlight != 2 || mismatches != 0 {
		t.Fatalf("mixed case: maxInFlight=%d mismatches=%d", maxInFlight, mismatches)
	}
}
