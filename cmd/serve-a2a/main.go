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
	"fmt"
	"net/http"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/a2a"
)

func main() {
	klog.InitFlags(nil)

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

	addr := ":" + port
	klog.Infof("serve-a2a listening on %s (workflow=%s/%s)", addr, namespace, workflowName)
	if err := http.ListenAndServe(addr, mux); err != nil {
		klog.Fatalf("server error: %v", err)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
