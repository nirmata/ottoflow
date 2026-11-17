/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"context"
	"fmt"
	"net/http"
	"time"

	mcpserver "github.com/mark3labs/mcp-go/server"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// mcpServerName and mcpServerVersion identify this server in the MCP
	// initialize handshake.
	mcpServerName    = "ottoflow"
	mcpServerVersion = "v1alpha1"

	// defaultMCPCallTimeout bounds one tools/call. A workflow that outlives it
	// keeps running; the caller is told which WorkflowRun to look at.
	defaultMCPCallTimeout = 5 * time.Minute
	// defaultMCPPollInterval is how often a pending run is re-read while a
	// tools/call waits for it.
	defaultMCPPollInterval = 2 * time.Second

	mcpShutdownTimeout = 10 * time.Second
)

// Authenticator gates requests to the MCP endpoint. *auth.TokenReviewAndSARAuthenticator
// satisfies it.
type Authenticator interface {
	Middleware(next http.Handler) http.Handler
}

// MCPToolServer serves this cluster's Workflows to MCP clients, one tool per
// Workflow: tools/list enumerates them, tools/call creates a WorkflowRun and
// waits for it. It is the inbound counterpart to the MCPServer CRD, which
// describes servers OttoFlow dials out to.
type MCPToolServer struct {
	client        client.Client
	authenticator Authenticator
	addr          string
	callTimeout   time.Duration
	pollInterval  time.Duration

	mcp    *mcpserver.MCPServer
	server *http.Server

	// RunLedger counts runs admitted but not yet in the cache; the controller points it at the scheduler's.
	RunLedger *runLedger
}

// NewMCPToolServer builds a server listening on addr. authenticator is
// required: an MCP endpoint reachable from outside the cluster that anyone may
// call is a way to run every Workflow in it.
func NewMCPToolServer(c client.Client, authenticator Authenticator, addr string) (*MCPToolServer, error) {
	if authenticator == nil {
		return nil, fmt.Errorf("mcp server: authenticator is required")
	}

	s := &MCPToolServer{
		client:        c,
		authenticator: authenticator,
		addr:          addr,
		callTimeout:   defaultMCPCallTimeout,
		pollInterval:  defaultMCPPollInterval,
		RunLedger:     newRunLedger(),
	}

	// listChanged is not advertised. The tool set does follow the Workflows in
	// the cluster, but the transport below is stateless: there is no session to
	// push a notification to, so claiming the capability would promise a
	// notification that can never arrive. Every tools/list is rebuilt from the
	// cache instead, so a client that re-lists is never stale.
	s.mcp = mcpserver.NewMCPServer(mcpServerName, mcpServerVersion,
		mcpserver.WithToolCapabilities(false),
		mcpserver.WithRecovery(),
	)

	return s, nil
}

// NeedLeaderElection reports false: every replica serves.
//
// This is the one place this server departs from CallbackServer, which is
// leader-elected. A Service selects every replica, so a leader-only endpoint
// would black-hole the requests that land anywhere else. Nothing here needs a
// singleton — tools/list is a cache read and tools/call creates a WorkflowRun
// the controller then reconciles exactly as it would from any other trigger.
func (s *MCPToolServer) NeedLeaderElection() bool { return false }

// Start serves until ctx is cancelled. It satisfies manager.Runnable.
func (s *MCPToolServer) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("mcp-server")

	// Stateless, because NeedLeaderElection is false and a Service spreads
	// requests over every replica: a session held in one replica's memory would
	// make the next request to a different one fail with an unknown session.
	streamable := mcpserver.NewStreamableHTTPServer(s.mcp, mcpserver.WithStateLess(true))

	mux := http.NewServeMux()
	mux.Handle("/mcp", s.authenticator.Middleware(s.withFreshTools(streamable)))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	s.server = &http.Server{
		Addr:              s.addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("starting MCP server", "addr", s.addr, "path", "/mcp")
		if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mcpShutdownTimeout)
		defer cancel()
		if err := s.server.Shutdown(shutdownCtx); err != nil {
			logger.Error(err, "MCP server shutdown failed")
		}
		logger.Info("MCP server stopped")
		return nil
	}
}

// withFreshTools reconciles the registered tools with the Workflows in the
// cluster before each request.
//
// The tool set is derived state, and the alternative shapes are both worse: a
// background resync serves a stale tools/list for as long as its interval, and
// a watch would duplicate the manager's cache to maintain a registry that is
// only read here. Listing from that cache costs nothing per request.
func (s *MCPToolServer) withFreshTools(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := s.syncTools(r.Context()); err != nil {
			log.FromContext(r.Context()).Error(err, "listing workflows for MCP tools")
			http.Error(w, "cannot list workflows", http.StatusServiceUnavailable)
			return
		}
		next.ServeHTTP(w, r)
	})
}
