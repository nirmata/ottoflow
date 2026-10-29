/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr/funcr"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// An API server that drops podReplacementPolicy (feature gate off) leaves the retry free to overlap a
// shutting-down runner. The controller says so once, and stays quiet when the field is kept.
func TestRunnerJobWarnsOnceWhenPodReplacementPolicyDropped(t *testing.T) {
	for name, drop := range map[string]bool{"dropped": true, "kept": false} {
		t.Run(name, func(t *testing.T) {
			warnPodReplacementPolicyDropped = sync.Once{}
			warnings := 0
			ctx := log.IntoContext(context.Background(), funcr.New(func(_, args string) {
				if strings.Contains(args, "podReplacementPolicy") {
					warnings++
				}
			}, funcr.Options{}))
			for _, run := range []string{"run-a", "run-b"} {
				wf, secret, wr := reservedEnvFixture([]corev1.EnvVar{{Name: "TEAM_SETTING", Value: "on"}})
				wr.Name = run
				clusterRole := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "ottoflow-role"}}
				c := fake.NewClientBuilder().WithScheme(unitTestScheme).WithStatusSubresource(wr).
					WithObjects(wf, secret, wr, clusterRole).
					WithInterceptorFuncs(interceptor.Funcs{
						Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
							if j, ok := obj.(*batchv1.Job); ok && drop {
								j.Spec.PodReplacementPolicy = nil // what an API server with the gate off stores
							}
							return c.Create(ctx, obj, opts...)
						},
					}).Build()
				r := &WorkflowRunReconciler{Client: c, Scheme: unitTestScheme, RunnerConfig: RunnerConfig{
					RunnerServiceAccount:   "controller-manager",
					RunnerClusterRole:      "ottoflow-role",
					AgentExecutorNamespace: "ottoflow",
				}}
				if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: run, Namespace: wr.Namespace}}); err != nil {
					t.Fatalf("Reconcile %s: %v", run, err)
				}
				if err := c.Get(ctx, types.NamespacedName{Name: run + "-runner", Namespace: wr.Namespace}, &batchv1.Job{}); err != nil {
					t.Fatalf("runner Job for %s not created: %v", run, err)
				}
			}
			want := 0
			if drop {
				want = 1
			}
			if warnings != want {
				t.Fatalf("podReplacementPolicy warnings = %d over two Job creates, want %d", warnings, want)
			}
		})
	}
}
