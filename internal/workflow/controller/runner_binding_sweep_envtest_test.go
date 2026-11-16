/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

func TestRunnerBindingSweep_RemovesBindingsOfDeletedServiceAccount(t *testing.T) {
	cfg, c := startSweepEnvtest(t)
	ctx := context.Background()
	g := gomega.NewWithT(t)

	const ns = "sweep-red"
	rc := RunnerConfig{RunnerClusterRole: "runner-role", AgentExecutorCallerRole: "caller-role"}
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatalf("create ns: %v", err)
	}

	seed := &WorkflowRunReconciler{Client: c, APIReader: c, Scheme: scheme.Scheme, RunnerConfig: rc}
	wr := &ottoflowv1alpha1.WorkflowRun{ObjectMeta: metav1.ObjectMeta{Name: "run", Namespace: ns}}
	for _, sa := range []string{"gone-runner", "live-runner"} {
		if err := c.Create(ctx, runnerServiceAccount(sa, ns)); err != nil {
			t.Fatalf("create ServiceAccount %s: %v", sa, err)
		}
		if err := seed.ensureRunnerAccess(ctx, wr, nil, sa, false); err != nil {
			t.Fatalf("ensureRunnerAccess %s: %v", sa, err)
		}
	}

	chart := func(name string) *rbacv1.ClusterRoleBinding {
		b := buildRunnerClusterRoleBinding(name, "chart-role", "chart-sa", ns)
		b.Labels["app.kubernetes.io/component"] = "controller"
		return b
	}
	multi := buildRunnerClusterRoleBinding(workflowRunnerRoleBindingName(ns, "multi"), "runner-role", "multi", ns)
	multi.Subjects = append(multi.Subjects, rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Name: "other", Namespace: ns})
	kept := make([]string, 0, 6)
	kept = append(kept, workflowRunnerRoleBindingName(ns, "live-runner"), agentExecutorCallerRoleBindingName(ns, "live-runner"))
	for _, b := range []*rbacv1.ClusterRoleBinding{
		chart("rel-rolebinding"), chart("rel-agent-executor-rolebinding"),
		chart("rel-agent-executor-caller-controller"), multi,
	} {
		if err := c.Create(ctx, b); err != nil {
			t.Fatalf("create binding %s: %v", b.Name, err)
		}
		kept = append(kept, b.Name)
	}

	if err := c.Delete(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "gone-runner", Namespace: ns}}); err != nil {
		t.Fatalf("delete ServiceAccount: %v", err)
	}

	startSweepManager(t, cfg, rc)

	g.Eventually(func() bool { return bindingsExist(c, ns, "gone-runner") }, 15*time.Second, 100*time.Millisecond).
		Should(gomega.BeFalse(), "bindings of the deleted ServiceAccount are still present after the manager started")

	requireBindings(t, c, true, kept...)
}

func startSweepManager(t *testing.T, cfg *rest.Config, rc RunnerConfig) {
	t.Helper()
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme.Scheme, Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0",
		Controller: config.Controller{SkipNameValidation: ptr.To(true)},
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	r := &WorkflowRunReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Scheme: scheme.Scheme, RunnerConfig: rc}
	if err := r.SetupWithManager(mgr); err != nil {
		t.Fatalf("SetupWithManager: %v", err)
	}
	mgrCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = mgr.Start(mgrCtx) }()
}

func bindingsExist(c client.Client, ns, sa string) bool {
	for _, name := range []string{workflowRunnerRoleBindingName(ns, sa), agentExecutorCallerRoleBindingName(ns, sa)} {
		if err := c.Get(context.Background(), types.NamespacedName{Name: name}, &rbacv1.ClusterRoleBinding{}); !apierrors.IsNotFound(err) {
			return true
		}
	}
	return false
}

func requireBindings(t *testing.T, c client.Client, want bool, names ...string) {
	t.Helper()
	for _, name := range names {
		err := c.Get(context.Background(), types.NamespacedName{Name: name}, &rbacv1.ClusterRoleBinding{})
		if exists := err == nil; exists != want {
			t.Errorf("ClusterRoleBinding %s exists = %v, want %v (Get err = %v)", name, exists, want, err)
		}
	}
}

// sweepEnv is one envtest API server (same version as the suite) shared by the sweep tests and
// stopped in TestMain, so a package run boots it once. Kept in this file so the sweep tests build
// on their own.
var sweepEnv struct {
	once   sync.Once
	env    *envtest.Environment
	cfg    *rest.Config
	client client.Client
	err    error
}

func TestMain(m *testing.M) {
	code := m.Run()
	if sweepEnv.env != nil {
		_ = sweepEnv.env.Stop()
	}
	os.Exit(code)
}

// startSweepEnvtest returns the shared envtest config and a direct client, starting it on first use.
func startSweepEnvtest(t *testing.T) (*rest.Config, client.Client) {
	t.Helper()
	sweepEnv.once.Do(func() {
		_, filename, _, _ := runtime.Caller(0)
		repoRoot := filepath.Join(filepath.Dir(filename), "..", "..", "..")
		sweepEnv.env = &envtest.Environment{
			CRDDirectoryPaths:           []string{filepath.Join(repoRoot, "config", "crd", "bases")},
			ErrorIfCRDPathMissing:       true,
			BinaryAssetsDirectory:       filepath.Join(repoRoot, "bin", "k8s"),
			DownloadBinaryAssets:        true,
			DownloadBinaryAssetsVersion: "1.36.2",
		}
		cfg, err := sweepEnv.env.Start()
		if err != nil {
			sweepEnv.err = fmt.Errorf("envtest start: %w", err)
			return
		}
		if err := ottoflowv1alpha1.AddToScheme(scheme.Scheme); err != nil {
			sweepEnv.err = fmt.Errorf("add scheme: %w", err)
			return
		}
		sweepEnv.cfg = cfg
		sweepEnv.client, sweepEnv.err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	})
	if sweepEnv.err != nil {
		t.Fatal(sweepEnv.err)
	}
	return sweepEnv.cfg, sweepEnv.client
}
