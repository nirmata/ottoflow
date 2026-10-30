/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const ctxBindingListExpr = `dyn(resource.List("v1","Pod","a",{})).items.size()`

func TestCELReusedVarsHonourLaterDeadline(t *testing.T) {
	k8s := fake.NewClientBuilder().WithScheme(lockScopeScheme()).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, l client.ObjectList, o ...client.ListOption) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
				return c.List(ctx, l, o...)
			}
		},
	}).Build()
	ev, err := NewCELEvaluator(k8s, nil)
	if err != nil {
		t.Fatal(err)
	}
	vars := map[string]interface{}{}
	if _, err := ev.EvaluateExpression(context.Background(), "1 + 1", vars); err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel2()
	start := time.Now()
	_, err = ev.EvaluateExpression(ctx2, ctxBindingListExpr, vars)
	if el := time.Since(start); el > 700*time.Millisecond {
		t.Errorf("evaluation ran %v past its 200ms deadline (err=%v)", el, err)
	}
}

func TestCELPrometheusMetricsCancelledQueryReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(400 * time.Millisecond):
		}
	}))
	defer srv.Close()
	pc, err := NewHTTPPrometheusClient(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	ev, err := NewCELEvaluatorWithMetrics(nil, nil, nil, pc, nil, nil, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	res, err := ev.EvaluateExpression(ctx, `prometheusMetrics("up", "5m")`, map[string]interface{}{})
	if err == nil {
		t.Fatalf("expected an error for a cancelled query, got result %v", res)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error does not wrap context.DeadlineExceeded: %v", err)
	}
}

func TestCELPrometheusMetricsUnrelatedErrorOnCancelledCtxKeepsErrorMap(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	requirePrometheusErrorMap(t, ctx, errors.New("upstream: context canceled"))
}

func TestCELPrometheusMetricsDeadlineErrorOnLiveCtxKeepsErrorMap(t *testing.T) {
	requirePrometheusErrorMap(t, context.Background(), fmt.Errorf("dial tcp: %w", context.DeadlineExceeded))
}

func requirePrometheusErrorMap(t *testing.T, ctx context.Context, queryErr error) {
	t.Helper()
	ev, err := NewCELEvaluatorWithMetrics(nil, nil, nil, &mockPrometheusClient{err: queryErr}, nil, nil, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := ev.EvaluateExpression(ctx, `prometheusMetrics("up", "5m")`, map[string]interface{}{})
	if err != nil {
		t.Fatalf("expected the error map, got error: %v", err)
	}
	if m, ok := res.(map[string]interface{}); !ok || m["error"] == nil {
		t.Errorf("expected a result map with an error field, got %v", res)
	}
}
