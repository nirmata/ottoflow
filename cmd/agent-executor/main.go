/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"net/http"
	_ "net/http/pprof" // registers /debug/pprof handlers on DefaultServeMux
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/agent"
	"github.com/nirmata/ottoflow/internal/auth"
	"github.com/nirmata/ottoflow/internal/workflow/executor"
)

const (
	defaultTLSPort = 8443
	healthzPath    = "/healthz"
	readyzPath     = "/readyz"
	tlsCertPath    = "/etc/tls/tls.crt"
	tlsKeyPath     = "/etc/tls/tls.key"
)

// startProfiler starts a plain HTTP server on the given port exposing the
// standard Go pprof endpoints (/debug/pprof/). It is intentionally separate
// from the TLS server so that pprof is never accidentally reachable via
// the production port.
//
// The server runs for the lifetime of the process; it is not shut down on
// SIGTERM because profiling during shutdown is often useful.
func startProfiler(port int) {
	addr := fmt.Sprintf("localhost:%d", port)
	klog.Infof("pprof profiler listening on http://%s/debug/pprof/ (--profile flag is set; disable in production)", addr)
	go func() {
		// DefaultServeMux already has the pprof handlers registered via the
		// blank import of net/http/pprof above.
		if err := http.ListenAndServe(addr, nil); err != nil {
			klog.Errorf("pprof server error: %v", err)
		}
	}()
}

func main() {
	klog.InitFlags(nil)
	tlsPort := flag.Int("tls-port", defaultTLSPort, "TLS server port")
	callerNamespaceFlag := flag.String("agent-executor-caller-namespace", "ottoflow",
		"Namespace for RBAC auth: agent executor checks get configmaps/agent-executor-caller via SubjectAccessReview")
	enableProfile := flag.Bool("profile", false,
		"Enable pprof profiling endpoint (CPU, heap, goroutine). Never enable in production.")
	profilePort := flag.Int("profiler-port", 6060,
		"Port for the pprof HTTP server (only used when --profile is set)")
	flag.Parse()

	// Route controller-runtime's root logger (used by certwatcher for cert-rotation
	// errors) through klog instead of the default NullLogSink. agent-executor already
	// configures logging via klog.InitFlags, so klog.Background() reuses that setup
	// rather than wiring a second, conflicting flag set (e.g. textlogger's -v/-vmodule
	// flags, as cmd/controller does, would collide with klog.InitFlags here).
	ctrl.SetLogger(klog.Background())

	if *enableProfile {
		startProfiler(*profilePort)
	}

	// Create Kubernetes client
	k8sClient, err := createK8sClient()
	if err != nil {
		klog.Fatalf("Failed to create Kubernetes client: %v", err)
	}

	// Create Kubernetes clientset for TokenReview and SubjectAccessReview APIs
	clientset, err := createK8sClientset()
	if err != nil {
		klog.Fatalf("Failed to create Kubernetes clientset: %v", err)
	}

	// RBAC auth: only identities that have get on configmaps/agent-executor-caller in caller namespace are allowed
	authenticator := auth.NewTokenReviewAndSARAuthenticator(
		clientset, *callerNamespaceFlag, auth.AgentExecutorCallerResourceName)

	// Create MCP client provider for agent tool registration, then agent executor.
	// useAPISecretAccess is true here: the agent-executor reads MCP Secrets through the API
	// under its own ServiceAccount; nothing mounts Secret files into this pod.
	mcpFactory := agent.NewDefaultMCPClientFactory(k8sClient, true)
	mcpManager := agent.NewMCPClientManager(k8sClient, mcpFactory)
	agentExecutor := executor.NewOttoFlowAgentExecutor(k8sClient, mcpManager)

	// Create HTTP mux
	mux := http.NewServeMux()

	// Register lightweight exec endpoint for internal Agent CRD calls.
	execHandler := executor.NewExecHandler(agentExecutor)
	mux.Handle("/api/exec/", authenticator.Middleware(http.StripPrefix("/api/exec", execHandler)))

	// Register health checks (no authentication required)
	mux.HandleFunc(healthzPath, healthzHandler)
	mux.HandleFunc(readyzPath, readyzHandler)

	// Root context cancelled on SIGINT/SIGTERM, shared by the cert watcher and shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Watch the mounted cert/key so a rotated cert is served without a pod restart.
	certWatcher, err := certwatcher.New(tlsCertPath, tlsKeyPath)
	if err != nil {
		klog.Fatalf("Failed to create certificate watcher: %v", err)
	}
	go func() {
		if err := certWatcher.Start(ctx); err != nil && ctx.Err() == nil {
			klog.Errorf("Certificate watcher stopped: %v", err)
		}
	}()

	// Start TLS server (HTTPS only); the cert is supplied dynamically by the watcher.
	tlsAddr := fmt.Sprintf(":%d", *tlsPort)
	tlsServer := createTLSServer(tlsAddr, mux, certWatcher.GetCertificate)

	// Graceful shutdown when the root context is cancelled. serverStopped is closed
	// once in-flight requests have drained (or the 10s timeout elapses), so main can
	// wait for the drain to finish before returning.
	serverStopped := make(chan struct{})
	go func() {
		<-ctx.Done()
		klog.Info("Shutting down server...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := tlsServer.Shutdown(shutdownCtx); err != nil {
			klog.Errorf("Server shutdown error: %v", err)
		}
		close(serverStopped)
	}()

	klog.Infof("Starting AgentExecutor TLS service on %s", tlsAddr)
	if err := tlsServer.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
		klog.Fatalf("TLS server failed: %v", err)
	}

	// Wait for graceful shutdown to finish draining in-flight requests before exiting.
	<-serverStopped
}

func healthzHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}

func readyzHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}

func createK8sClient() (client.Client, error) {
	// Try in-cluster config first
	cfg, err := config.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get Kubernetes config: %w", err)
	}

	scheme := createScheme()

	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("failed to create Kubernetes client: %w", err)
	}

	return k8sClient, nil
}

func createK8sClientset() (kubernetes.Interface, error) {
	cfg, err := config.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get Kubernetes config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create Kubernetes clientset: %w", err)
	}

	return clientset, nil
}

func createTLSServer(
	addr string,
	handler http.Handler,
	getCertificate func(*tls.ClientHelloInfo) (*tls.Certificate, error),
) *http.Server {
	return &http.Server{
		Addr:    addr,
		Handler: handler,
		TLSConfig: &tls.Config{
			MinVersion:     tls.VersionTLS12,
			GetCertificate: getCertificate,
		},
	}
}

func createScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()

	// Add OttoFlow types
	utilruntime.Must(ottoflowv1alpha1.AddToScheme(scheme))

	// Add Kubernetes core types
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	return scheme
}
