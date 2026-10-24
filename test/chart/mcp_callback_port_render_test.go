/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package chart

import (
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The callback server binds a literal port (cmd/controller/main.go); the MCP default must differ.
func TestMCPAddrDiffersFromCallbackAddr(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(chartDir(t), "..", "..", "cmd", "controller", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`NewCallbackServer\(.*"([^"]*:\d+)"\)`).FindStringSubmatch(string(src))
	if m == nil {
		t.Fatal("callback server address literal not found in cmd/controller/main.go")
	}
	mcp := regexp.MustCompile(`--mcp-addr=(\S+)`).FindStringSubmatch(
		strings.Join(renderDocs(t, "--set", "controller.mcp.enabled=true"), "\n"))
	if mcp == nil {
		t.Fatal("--mcp-addr not rendered with controller.mcp.enabled=true")
	}
	_, mcpPort, err := net.SplitHostPort(mcp[1])
	if err != nil {
		t.Fatalf("--mcp-addr %q: %v", mcp[1], err)
	}
	_, callbackPort, err := net.SplitHostPort(m[1])
	if err != nil {
		t.Fatalf("callback address %q: %v", m[1], err)
	}
	if mcpPort == callbackPort {
		t.Errorf("default --mcp-addr %s shares a port with the callback server address %s", mcp[1], m[1])
	}
}
