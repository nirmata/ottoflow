/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package agent

import "context"

type kubeConfigPathKey struct{}

// WithKubeConfigPath returns a context whose stdio MCP servers start with KUBECONFIG=path.
// An MCPServer's own spec.env KUBECONFIG still wins.
func WithKubeConfigPath(ctx context.Context, path string) context.Context {
	return context.WithValue(ctx, kubeConfigPathKey{}, path)
}

func kubeConfigPathFromContext(ctx context.Context) string {
	path, _ := ctx.Value(kubeConfigPathKey{}).(string)
	return path
}
