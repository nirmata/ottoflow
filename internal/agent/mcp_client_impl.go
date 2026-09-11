/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/kubectl-ai/pkg/mcp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/secretmount"
)

// realMCPClient wraps kubectl-ai MCP client to implement our MCPClient interface
type realMCPClient struct {
	serverName        string
	client            *mcp.Client
	connectMu         sync.Mutex
	connected         bool
	connectionTimeout time.Duration // used for initial Connect() so stdio has time to start (e.g. uvx)
}

// ListTools returns metadata for all tools from the MCP server (for LLM tool registration).
func (c *realMCPClient) ListTools(ctx context.Context) ([]MCPToolMeta, error) {
	if err := c.ensureConnected(ctx); err != nil {
		return nil, err
	}

	tools, err := c.client.ListTools(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]MCPToolMeta, 0, len(tools))
	for _, t := range tools {
		out = append(out, MCPToolMeta{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		})
	}
	return out, nil
}

// CallTool calls an MCP tool and returns the result as interface{} (parses JSON when possible)
func (c *realMCPClient) CallTool(ctx context.Context, toolName string, arguments map[string]interface{}) (interface{}, error) {
	if err := c.ensureConnected(ctx); err != nil {
		return nil, err
	}

	result, err := c.client.CallTool(ctx, toolName, arguments)
	if err != nil {
		return nil, err
	}

	// Parse as JSON when possible for structured CEL access (e.g. toolResult.count)
	return parseToolResult(result)
}

// Close closes the MCP client connection
func (c *realMCPClient) Close() error {
	c.connectMu.Lock()
	defer c.connectMu.Unlock()
	if c.client == nil {
		return nil
	}
	klog.V(4).InfoS("Closing MCP client", "server", c.serverName)
	err := c.client.Close()
	c.client = nil
	c.connected = false
	return err
}

func (c *realMCPClient) ensureConnected(ctx context.Context) error {
	c.connectMu.Lock()
	defer c.connectMu.Unlock()
	if c.connected && c.client != nil {
		return nil
	}
	if c.client == nil {
		return fmt.Errorf("MCP client not initialized for server %s", c.serverName)
	}
	// Use a dedicated timeout for Connect so stdio servers (e.g. uvx) have time to start
	// even when the parent context (e.g. reconcile) has a short deadline.
	connectCtx := ctx
	if c.connectionTimeout > 0 {
		var cancel context.CancelFunc
		connectCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), c.connectionTimeout)
		defer cancel()
	}
	if err := c.client.Connect(connectCtx); err != nil {
		return fmt.Errorf("connecting to MCP server %s: %w", c.serverName, err)
	}
	c.connected = true
	return nil
}

// parseToolResult attempts to parse the tool result as JSON; returns raw string if not valid JSON
func parseToolResult(result string) (interface{}, error) {
	if result == "" {
		return "", nil
	}
	trimmed := strings.TrimSpace(result)
	if trimmed == "" {
		return result, nil
	}

	// Try JSON object or array
	if (strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}")) ||
		(strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]")) {
		var parsed interface{}
		if err := json.Unmarshal([]byte(result), &parsed); err == nil {
			return parsed, nil
		}
	}

	// Try JSON number
	if num, err := strconv.ParseFloat(trimmed, 64); err == nil {
		return num, nil
	}
	if b, err := strconv.ParseBool(trimmed); err == nil {
		return b, nil
	}

	return result, nil
}

// buildMCPClientConfig converts MCPServer CRD to kubectl-ai mcp.ClientConfig.
//
// useAPISecretAccess selects how the Secrets behind spec.env[].valueFrom.secretKeyRef and
// spec.auth are read: true does a live client.Client.Get; false reads the files the
// controller mounted into the pod (see internal/secretmount) and never touches the Secret
// API. The in-cluster workflow-runner Job passes false.
func buildMCPClientConfig(ctx context.Context, k8sClient client.Client, mcpServer *ottoflowv1alpha1.MCPServer, useAPISecretAccess bool) (mcp.ClientConfig, error) {
	cfg := mcp.ClientConfig{
		Name: mcpServer.Name,
	}

	transport := mcpServer.Spec.Transport
	switch transport.Type {
	case "stdio":
		if len(transport.Command) == 0 {
			return cfg, fmt.Errorf("stdio transport requires command")
		}
		cfg.Command = transport.Command[0]
		if len(transport.Command) > 1 {
			cfg.Args = transport.Command[1:]
		}
		if mcpServer.Spec.Timeout != "" {
			if d, err := time.ParseDuration(mcpServer.Spec.Timeout); err == nil && d > 0 {
				cfg.Timeout = int(d.Seconds())
			}
		}
		if cfg.Timeout <= 0 {
			cfg.Timeout = 90
		}
		// Resolve env from MCPServer spec
		for _, ev := range mcpServer.Spec.Env {
			val, present, err := resolveEnvValue(ctx, k8sClient, mcpServer.Namespace, &ev, useAPISecretAccess)
			if err != nil {
				return cfg, fmt.Errorf("MCPServer %s/%s env %q: %w", mcpServer.Namespace, mcpServer.Name, ev.Name, err)
			}
			if !present {
				// Optional SecretKeyRef whose Secret/key is absent: Kubernetes leaves such a
				// variable UNSET rather than setting it to "". An empty credential would only
				// fail later, opaquely, inside the MCP server. Omit it.
				klog.V(2).Infof("MCPServer %s/%s: optional env %q has no value; leaving it unset",
					mcpServer.Namespace, mcpServer.Name, ev.Name)
				continue
			}
			cfg.Env = append(cfg.Env, fmt.Sprintf("%s=%s", ev.Name, val))
		}
	case "http", "sse":
		if transport.Address == "" {
			return cfg, fmt.Errorf("http/sse transport requires address")
		}
		cfg.URL = transport.Address
		cfg.UseStreaming = (transport.Type == "sse")
		if mcpServer.Spec.Timeout != "" {
			if d, err := time.ParseDuration(mcpServer.Spec.Timeout); err == nil {
				cfg.Timeout = int(d.Seconds())
			}
		}
		cfg.Headers = transport.Headers
		// Resolve auth
		if mcpServer.Spec.Auth != nil {
			authCfg, oauthCfg, err := resolveAuthConfigs(ctx, k8sClient, mcpServer, useAPISecretAccess)
			if err != nil {
				return cfg, err
			}
			cfg.Auth = authCfg
			cfg.OAuthConfig = oauthCfg
		}
	default:
		return cfg, fmt.Errorf("unsupported transport type: %s", transport.Type)
	}

	return cfg, nil
}

// resolveEnvValue returns ev's value, resolving it from a Secret when ev.ValueFrom.SecretKeyRef
// is set. A resolution failure is returned as an error, like every other Secret read in this
// file: silently substituting "" would launch the stdio MCP server with an empty credential,
// and that surfaces only later as an opaque upstream 401 instead of an actionable error here.
//
// The second return value reports whether the variable has a value at all. It is false only
// for an OPTIONAL SecretKeyRef (SecretKeySelector.Optional) whose Secret or key is absent:
// Kubernetes' contract for that case is that the variable is left unset, not set to the empty
// string, so the caller omits it rather than delivering an empty credential. A NON-optional
// ref that cannot be resolved is still an error, never a silent omission.
func resolveEnvValue(ctx context.Context, k8sClient client.Client, namespace string, ev *corev1.EnvVar, useAPISecretAccess bool) (string, bool, error) {
	if ev.Value != "" {
		return ev.Value, true, nil
	}
	if ev.ValueFrom != nil && ev.ValueFrom.SecretKeyRef != nil {
		sel := ev.ValueFrom.SecretKeyRef
		v, err := secretmount.Resolve(ctx, k8sClient, useAPISecretAccess, namespace, sel.Name, sel.Key)
		if err != nil {
			// Optional means "leave the variable unset when the value simply isn't there"
			// (Kubernetes' SecretKeyRef contract) — NOT "swallow every failure". A broken
			// runtime (a malformed OTTOFLOW_SECRET_MOUNTS, an unreadable mounted file, an API
			// error other than NotFound) must surface even for an optional ref, or a real
			// misconfiguration silently degrades into a missing env var.
			if sel.Optional != nil && *sel.Optional && errors.Is(err, secretmount.ErrNotFound) {
				return "", false, nil
			}
			return "", false, fmt.Errorf("resolving env %q: %w", ev.Name, err)
		}
		return string(v), true, nil
	}
	return "", true, nil
}

// resolveAuthConfigs returns AuthConfig and OAuthConfig for kubectl-ai MCP client
func resolveAuthConfigs(ctx context.Context, k8sClient client.Client, mcpServer *ottoflowv1alpha1.MCPServer, useAPISecretAccess bool) (*mcp.AuthConfig, *mcp.OAuthConfig, error) {
	auth := mcpServer.Spec.Auth
	if auth == nil {
		return nil, nil, nil
	}

	// Map CRD auth types to kubectl-ai (apiKey -> api-key)
	authType := auth.Type
	if authType == "apiKey" {
		authType = "api-key"
	}
	ac := &mcp.AuthConfig{Type: authType}
	var oauthCfg *mcp.OAuthConfig

	switch auth.Type {
	case "bearer", "apiKey":
		if auth.SecretRef == nil {
			return nil, nil, fmt.Errorf("secretRef is required for %s auth", auth.Type)
		}
		ns := auth.SecretRef.Namespace
		if ns == "" {
			ns = mcpServer.Namespace
		}
		token, err := secretmount.Resolve(ctx, k8sClient, useAPISecretAccess, ns, auth.SecretRef.Name, auth.SecretRef.Key)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to get auth secret: %w", err)
		}
		ac.Token = string(token)
		if auth.Type == "apiKey" {
			ac.ApiKey = ac.Token
		}
	case "basic":
		if auth.SecretRef == nil {
			return nil, nil, fmt.Errorf("secretRef is required for basic auth")
		}
		ns := auth.SecretRef.Namespace
		if ns == "" {
			ns = mcpServer.Namespace
		}
		username, err := secretmount.Resolve(ctx, k8sClient, useAPISecretAccess, ns, auth.SecretRef.Name, "username")
		if err != nil {
			return nil, nil, fmt.Errorf("basic auth: resolving username: %w", err)
		}
		password, err := secretmount.Resolve(ctx, k8sClient, useAPISecretAccess, ns, auth.SecretRef.Name, "password")
		if err != nil {
			return nil, nil, fmt.Errorf("basic auth: resolving password: %w", err)
		}
		ac.Username = string(username)
		ac.Password = string(password)
		if ac.Username == "" || ac.Password == "" {
			return nil, nil, fmt.Errorf("secret for basic auth must contain non-empty username and password keys")
		}
	case "oauth2":
		if auth.OAuth2 == nil {
			return nil, nil, fmt.Errorf("oauth2 auth type requires oauth2 config")
		}
		oauth2 := auth.OAuth2
		oauthCfg = &mcp.OAuthConfig{
			TokenURL: oauth2.TokenURL,
			Scopes:   oauth2.Scopes,
		}
		if oauth2.ClientCredentialsRef != nil {
			ns := oauth2.ClientCredentialsRef.Namespace
			if ns == "" {
				ns = mcpServer.Namespace
			}
			clientID, err := secretmount.Resolve(ctx, k8sClient, useAPISecretAccess, ns, oauth2.ClientCredentialsRef.Name, "client_id")
			if err != nil {
				return nil, nil, fmt.Errorf("oauth2 clientCredentialsRef: resolving client_id: %w", err)
			}
			clientSecret, err := secretmount.Resolve(ctx, k8sClient, useAPISecretAccess, ns, oauth2.ClientCredentialsRef.Name, "client_secret")
			if err != nil {
				return nil, nil, fmt.Errorf("oauth2 clientCredentialsRef: resolving client_secret: %w", err)
			}
			oauthCfg.ClientID = string(clientID)
			oauthCfg.ClientSecret = string(clientSecret)
		} else if oauth2.ClientID != "" && oauth2.ClientSecretRef != nil {
			oauthCfg.ClientID = oauth2.ClientID
			ns := oauth2.ClientSecretRef.Namespace
			if ns == "" {
				ns = mcpServer.Namespace
			}
			v, err := secretmount.Resolve(ctx, k8sClient, useAPISecretAccess, ns, oauth2.ClientSecretRef.Name, oauth2.ClientSecretRef.Key)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to get OAuth2 client secret: %w", err)
			}
			oauthCfg.ClientSecret = string(v)
		}
		if oauthCfg.ClientID == "" || oauthCfg.ClientSecret == "" {
			return nil, nil, fmt.Errorf("oauth2 requires client_id and client_secret")
		}
	}

	return ac, oauthCfg, nil
}
