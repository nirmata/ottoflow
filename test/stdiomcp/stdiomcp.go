/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

// Package stdiomcp runs a test binary as a one-tool stdio MCP server, so a test can see the
// environment a real stdio MCP child process inherits from the process that starts it.
package stdiomcp

import (
	"context"
	"os"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

const (
	// ServeEnv, set to any value in a child's environment, makes ServeIfChild serve.
	ServeEnv = "OTTOFLOW_TEST_SERVE_STDIO_MCP"
	// Tool is the server's only tool. It returns "KUBECONFIG=" followed by the child's KUBECONFIG.
	Tool = "kubeconfig"
)

// ServeIfChild serves MCP on stdin and stdout, then exits, when ServeEnv is set; otherwise it
// returns at once. Call it first in TestMain, and start the child as the test binary itself
// (os.Executable) with ServeEnv in its environment.
func ServeIfChild() {
	if os.Getenv(ServeEnv) == "" {
		return
	}
	s := server.NewMCPServer("stdiomcp", "0.0.1")
	s.AddTool(mcp.NewTool(Tool), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("KUBECONFIG=" + os.Getenv("KUBECONFIG")), nil
	})
	if err := server.ServeStdio(s); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}
