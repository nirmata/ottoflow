/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/GoogleCloudPlatform/kubectl-ai/gollm"
	"github.com/GoogleCloudPlatform/kubectl-ai/pkg/tools"
)

// ottoflowMCPTool implements tools.Tool so the agent can invoke MCP tools during LLM execution.
type ottoflowMCPTool struct {
	serverName  string
	toolName    string
	description string
	schema      *gollm.FunctionDefinition
	client      MCPClient
}

// UniqueKey returns the key used in sessionTools map (server:tool) so the LLM can request this tool by name.
func (t *ottoflowMCPTool) UniqueKey() string {
	return t.serverName + ":" + t.toolName
}

// Name returns the tool name the LLM uses when invoking; we use "server:tool" for uniqueness.
func (t *ottoflowMCPTool) Name() string {
	return t.UniqueKey()
}

// Description returns the tool description.
func (t *ottoflowMCPTool) Description() string {
	return t.description
}

// FunctionDefinition returns the schema for the LLM.
func (t *ottoflowMCPTool) FunctionDefinition() *gollm.FunctionDefinition {
	return t.schema
}

// IsInteractive implements tools.Tool (non-interactive).
func (t *ottoflowMCPTool) IsInteractive(args map[string]any) (bool, error) {
	return false, nil
}

// CheckModifiesResource implements tools.Tool (conservative unknown for MCP tools).
func (t *ottoflowMCPTool) CheckModifiesResource(args map[string]any) string {
	return "unknown"
}

// Run invokes the MCP tool via our client.
func (t *ottoflowMCPTool) Run(ctx context.Context, args map[string]any) (any, error) {
	if t.client == nil {
		return nil, fmt.Errorf("MCP client not set for tool %s", t.UniqueKey())
	}
	return t.client.CallTool(ctx, t.toolName, args)
}

// buildSessionToolsFromMCP builds a map of tools.Tool for the agent from Agent.Spec.MCPTools ("server:tool" entries).
// It uses the given MCPClientProvider and namespace to get clients and list tools.
func buildSessionToolsFromMCP(ctx context.Context, mcpTools []string, namespace string, provider MCPClientProvider) (map[string]tools.Tool, error) {
	if provider == nil || len(mcpTools) == 0 {
		return nil, nil
	}

	wantSet := make(map[string]bool)
	servers := make(map[string]bool)
	for _, ref := range mcpTools {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		wantSet[ref] = true
		server, _, _ := strings.Cut(ref, ":")
		if server != "" {
			servers[server] = true
		}
	}
	if len(wantSet) == 0 {
		return nil, nil
	}

	sessionTools := make(map[string]tools.Tool)
	for serverName := range servers {
		client, err := provider.GetClient(ctx, serverName, namespace)
		if err != nil {
			return nil, fmt.Errorf("get MCP client for server %q: %w", serverName, err)
		}
		metaList, err := client.ListTools(ctx)
		if err != nil {
			// The client is shared with other conversations: drop it from the cache instead of
			// closing it, so the next execution builds a new one.
			if ev, ok := provider.(MCPClientEvicter); ok {
				ev.Evict(namespace, serverName, client)
			}
			return nil, fmt.Errorf("list tools from MCP server %q: %w", serverName, err)
		}
		for _, meta := range metaList {
			key := serverName + ":" + meta.Name
			if !wantSet[key] {
				continue
			}
			schema := &gollm.FunctionDefinition{
				Name:        key,
				Description: meta.Description,
				Parameters:  meta.InputSchema,
			}
			tool := &ottoflowMCPTool{
				serverName:  serverName,
				toolName:    meta.Name,
				description: meta.Description,
				schema:      schema,
				client:      client,
			}
			sessionTools[key] = tool
		}
		// Don't close client here; tools hold reference and may be used during conversation.
		// Caller or conversation close should clean up if needed.
	}
	return sessionTools, nil
}
