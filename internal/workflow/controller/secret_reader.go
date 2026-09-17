/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import "sigs.k8s.io/controller-runtime/pkg/client"

// preferAPIReader returns apiReader — the manager's direct (non-cached) reader — falling back to
// fallback when apiReader is nil (tests using a fake client, which has no separate API-reader
// concept). The WorkflowRun reconciler routes its Secret reads through this (directReader) so
// they never depend on the cache: a cache-backed Secret Get would start a cluster-wide Secret
// informer, which needs cluster-wide list/watch RBAC and, without it, hangs on an informer that
// can never sync. Secret caching is also disabled manager-wide (cmd/controller/main.go
// DisableFor), so the manager client's own Secret Gets elsewhere in the controller are live API
// calls too; this helper keeps the reconciler's reads explicit about it and gives tests a single
// seam to inject a reader through.
func preferAPIReader(apiReader, fallback client.Reader) client.Reader {
	if apiReader != nil {
		return apiReader
	}
	return fallback
}
