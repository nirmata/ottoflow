/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

// Command serve-a2a is a minimal A2A (agent-to-agent) server that exposes a single
// OttoFlow Workflow as an A2A agent. On a message/stream request it creates a
// WorkflowRun for the target Workflow and streams the run's progress and output back
// as A2A SSE events, the way kagent's UI consumes them.
//
// It builds its Kubernetes client via controller-runtime's config.GetConfig(), which
// uses in-cluster config when running in a pod and the local kubeconfig/current-context
// when run locally — so the same binary works both ways with no flags.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/a2a"
	"github.com/nirmata/ottoflow/internal/auth"
)

func main() {
	klog.InitFlags(nil)
	flag.Parse() // makes klog's -v usable; the auth rejection reasons log at V(4)

	workflowName := os.Getenv("WORKFLOW_NAME")
	if workflowName == "" {
		klog.Fatal("WORKFLOW_NAME is required")
	}
	namespace := envOr("WORKFLOW_NAMESPACE", "ottoflow")
	port := envOr("PORT", "8080")
	// ponytail: url is a local placeholder for the skeleton. The reconciler that
	// deploys this server BYO-style will set the real in-cluster Service URL later.
	publicURL := envOr("A2A_PUBLIC_URL", fmt.Sprintf("http://localhost:%s/", port))

	// A2A_RUN_TIMEOUT bounds how long a single A2A call waits for its WorkflowRun. A workflow's
	// real budget is workflow-specific (a chain of agent calls can be long), so it is tunable.
	// Empty leaves it at the server default; a non-empty invalid value fails fast at startup.
	var runTimeout time.Duration
	if v := os.Getenv("A2A_RUN_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			klog.Fatalf("invalid A2A_RUN_TIMEOUT %q: %v", v, err)
		}
		runTimeout = d
	}

	cfg, err := config.GetConfig()
	if err != nil {
		klog.Fatalf("loading kube config: %v", err)
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(ottoflowv1alpha1.AddToScheme(scheme))

	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		klog.Fatalf("creating kube client: %v", err)
	}

	srv := a2a.NewServer(k8sClient, workflowName, namespace, publicURL, runTimeout)
	mux := http.NewServeMux()
	srv.Register(mux)

	var handler http.Handler = mux
	if os.Getenv("A2A_ALLOW_UNAUTHENTICATED") == "true" {
		klog.Warning("A2A_ALLOW_UNAUTHENTICATED=true: serve-a2a accepts A2A calls with NO authentication. " +
			"Anyone who can reach this pod can create WorkflowRuns. Restrict access with a NetworkPolicy.")
	} else {
		kc, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			klog.Fatalf("creating kube clientset: %v", err)
		}
		// Callers need "get callers.ottoflow.nirmata.io/a2a-caller" in the Workflow's namespace and a token minted for
		// the dedicated audience; see docs/user/tasks/workflows-as-a2a-agents.md.
		handler = a2a.RequireAuth(
			auth.NewTokenReviewAndSARAuthenticator(kc, namespace, a2a.CallerResourceName).WithAudiences(a2a.TokenAudience),
			mux)
		// Without a ClusterRoleBinding for TokenReview/SubjectAccessReview every call is refused.
		// Say so at startup; readiness stays the agent-card probe, so this is a log line only.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		for _, m := range auth.MissingReviewerAccess(ctx, kc) {
			klog.Errorf("serve-a2a cannot authenticate callers, every request will get 401: %s. "+
				"Bind the a2a token-reviewer ClusterRole to this ServiceAccount with a ClusterRoleBinding.", m)
		}
		cancel()
	}

	addr := ":" + port
	klog.Infof("serve-a2a listening on %s (workflow=%s/%s)", addr, namespace, workflowName)
	// ReadHeaderTimeout bounds slow-header connections. No WriteTimeout: SSE responses run up to the run timeout.
	httpSrv := &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	if err := httpSrv.ListenAndServe(); err != nil {
		klog.Fatalf("server error: %v", err)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
