/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/onsi/gomega"
)

// The streamable HTTP server refuses a loopback request whose Host is not local (DNS-rebinding
// guard). A proxy forwarding over 127.0.0.1 must therefore rewrite Host.
func TestMCPToolServerRejectsLoopbackRequestsWithANonLocalHost(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	s := newTestMCPServer(t)
	s.addr = addr
	done := make(chan error, 1)
	go func() { done <- s.Start(t.Context()) }()
	t.Cleanup(func() { <-done })

	gomega.NewWithT(t).Eventually(func() error {
		select {
		case err := <-done:
			done <- err // the cleanup still needs a value
			t.Fatalf("Start returned before the server came up: %v", err)
		default:
		}
		resp, err := http.Get("http://" + addr + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
		}
		return err
	}, 5*time.Second, 10*time.Millisecond).Should(gomega.Succeed(), "server did not come up")

	const mcpInit = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26",` +
		`"capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`
	post := func(host string) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/mcp", strings.NewReader(mcpInit))
		req.Header.Set("Content-Type", "application/json")
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := post("mcp.example.com"); code != http.StatusForbidden || !strings.Contains(body, "Forbidden: invalid Host header") {
		t.Errorf("Host mcp.example.com: got %d %q; want 403 with \"Forbidden: invalid Host header\"", code, strings.TrimSpace(body))
	}
	_, port, _ := net.SplitHostPort(addr)
	if code, body := post("localhost:" + port); code != http.StatusOK {
		t.Errorf("Host localhost:%s: got %d %q; want 200", port, code, strings.TrimSpace(body))
	}
}
