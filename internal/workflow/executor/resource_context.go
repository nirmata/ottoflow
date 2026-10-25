/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"context"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/pager"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// listPageSize is the default number of items fetched per API call during list pagination.
	listPageSize = 500
	// listMaxItems is the most items one ListResources call returns; a longer list fails closed.
	listMaxItems = 10000
	// listPageTimeout is the per-page deadline for each client.List() call. A slow API server
	// response that exceeds this fails fast rather than blocking until the workflow deadline,
	// allowing the caller to retry if appropriate.
	listPageTimeout = 45 * time.Second
)

// resourceContext implements Kyverno's resource.ContextInterface using controller-runtime client
type resourceContext struct {
	client    client.Client
	namespace string
	pageSize  int64           // items per page; 0 falls back to listPageSize
	ctx       context.Context // per-evaluation context; falls back to context.Background() when nil
}

func (c *resourceContext) effectivePageSize() int64 {
	if c.pageSize > 0 {
		return c.pageSize
	}
	return listPageSize
}

func (c *resourceContext) getCtx() context.Context {
	if c.ctx != nil {
		return c.ctx
	}
	return context.Background()
}

// GetResource retrieves a single Kubernetes resource by name
func (c *resourceContext) GetResource(apiVersion, resourceName, namespace, name string) (*unstructured.Unstructured, error) {
	if c.client == nil {
		return nil, fmt.Errorf("kubernetes client not available (no kubeconfig); this workflow requires a cluster")
	}

	// Use default namespace if empty
	ns := namespace
	if ns == "" {
		ns = c.namespace
	}

	// Parse GroupVersion
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return nil, fmt.Errorf("invalid apiVersion '%s': %w", apiVersion, err)
	}

	// Convert resource name to kind
	// Kyverno accepts both resource names (e.g., "deployments", "configmaps") and kinds (e.g., "Deployment", "ConfigMap")
	kind := convertResourceNameToKind(resourceName)

	// Get resource
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   gv.Group,
		Version: gv.Version,
		Kind:    kind,
	})

	key := client.ObjectKey{Name: name, Namespace: ns}
	err = c.client.Get(c.getCtx(), key, obj)
	if err != nil {
		return nil, fmt.Errorf("failed to get resource '%s/%s/%s/%s': %w", apiVersion, kind, ns, name, err)
	}

	return obj, nil
}

// ListResources lists Kubernetes resources with optional label filtering.
// Pagination is handled by client-go's ListPager, which:
//   - fetches in pages of effectivePageSize() items per API call
//   - transparently restarts from scratch when the etcd watch cache compacts
//     mid-pagination (HTTP 410 StatusReasonExpired / FullListIfExpired=true)
//   - streams items via EachListItemWithAlloc so the backing slice of each page
//     is not retained after the callback returns, reducing peak memory pressure
//
// Each page fetch runs under a per-page deadline (listPageTimeout) so a single
// slow API server response cannot block until the workflow's global deadline.
func (c *resourceContext) ListResources(apiVersion, resourceName, namespace string, labels map[string]string) (*unstructured.UnstructuredList, error) {
	if c.client == nil {
		return nil, fmt.Errorf("kubernetes client not available (no kubeconfig); this workflow requires a cluster")
	}

	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return nil, fmt.Errorf("invalid apiVersion '%s': %w", apiVersion, err)
	}

	kind := convertResourceNameToKind(resourceName)
	gvkList := schema.GroupVersionKind{
		Group:   gv.Group,
		Version: gv.Version,
		Kind:    fmt.Sprintf("%sList", kind),
	}

	nsStr := namespace
	if nsStr == "" {
		nsStr = "all namespaces"
	}

	// Build the base set of controller-runtime ListOptions that don't change per page.
	baseOpts := make([]client.ListOption, 0, 2)
	if namespace != "" {
		baseOpts = append(baseOpts, client.InNamespace(namespace))
	}
	if len(labels) > 0 {
		baseOpts = append(baseOpts, client.MatchingLabels(labels))
	}

	// pager.ListPager bridges metav1.ListOptions (Limit, Continue) to controller-runtime
	// client.ListOption. The pager sets opts.Limit = p.PageSize on the first call and
	// opts.Continue from the previous response on subsequent calls.
	p := pager.New(func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
		page := &unstructured.UnstructuredList{}
		page.SetGroupVersionKind(gvkList)

		pageOpts := make([]client.ListOption, len(baseOpts), len(baseOpts)+2)
		copy(pageOpts, baseOpts)
		if opts.Limit > 0 {
			pageOpts = append(pageOpts, client.Limit(opts.Limit))
		}
		if opts.Continue != "" {
			pageOpts = append(pageOpts, client.Continue(opts.Continue))
		}

		// Per-page deadline: decoupled from the workflow's global context so a single
		// slow API server response fails fast and can be retried rather than blocking
		// until the workflow deadline fires.
		pageCtx, cancel := context.WithTimeout(ctx, listPageTimeout)
		defer cancel()
		return page, c.client.List(pageCtx, page, pageOpts...)
	})
	p.PageSize = c.effectivePageSize()
	p.PageBufferSize = 0 // buffer 0: at most one page is fetched ahead, so the cap also bounds pages fetched
	// FullListIfExpired (default true): if the etcd watch cache compacts mid-pagination
	// the pager transparently restarts from scratch instead of surfacing an HTTP 410.

	result := &unstructured.UnstructuredList{}
	result.SetGroupVersionKind(gvkList)

	// EachListItemWithAlloc avoids retaining references to each page's backing slice
	// after the callback returns, reducing GC pressure for large namespaces.
	err = p.EachListItemWithAlloc(c.getCtx(), metav1.ListOptions{}, func(obj runtime.Object) error {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			return fmt.Errorf("unexpected object type %T in list '%s/%s'", obj, apiVersion, kind)
		}
		if len(result.Items) >= listMaxItems {
			return fmt.Errorf("more than %d items; narrow the list by namespace or labels, or use a resourceQuery step with limit (returns only the first limit items)", listMaxItems)
		}
		result.Items = append(result.Items, *u)
		// Propagate workflow context cancellation between items so the loop
		// exits promptly when the step times out or the workflow is deleted.
		return c.getCtx().Err()
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list resources '%s/%s' in %s: %w", apiVersion, kind, nsStr, err)
	}

	return result, nil
}

// PostResource performs API operations (e.g., SubjectAccessReview)
// Currently not implemented as it's not needed for basic resource operations
func (c *resourceContext) PostResource(apiVersion, resourceName, namespace string, data map[string]any) (*unstructured.Unstructured, error) {
	return nil, fmt.Errorf("PostResource not implemented")
}

// ToGVR converts apiVersion and kind to GroupVersionResource
func (c *resourceContext) ToGVR(apiVersion, kind string) (*schema.GroupVersionResource, error) {
	parts := strings.Split(apiVersion, "/")
	group := ""
	version := ""
	if len(parts) == 1 {
		version = parts[0]
	} else {
		group = parts[0]
		version = parts[1]
	}

	// Convert kind to resource name (lowercase, pluralized)
	resource := strings.ToLower(kind) + "s"

	return &schema.GroupVersionResource{
		Group:    group,
		Version:  version,
		Resource: resource,
	}, nil
}

// convertResourceNameToKind converts a resource name (e.g., "deployments", "configmaps") or kind (e.g., "Deployment", "ConfigMap") to proper Kind format
// Handles common Kubernetes resource name patterns
func convertResourceNameToKind(resourceName string) string {
	// If already in Kind format (starts with capital), return as-is
	if len(resourceName) > 0 && resourceName[0] >= 'A' && resourceName[0] <= 'Z' {
		return resourceName
	}

	// Convert lowercase resource name to Kind
	// Handle common plural forms
	lower := strings.ToLower(resourceName)

	// Map of common resource names to kinds
	resourceToKind := map[string]string{
		"deployments":            "Deployment",
		"configmaps":             "ConfigMap",
		"secrets":                "Secret",
		"services":               "Service",
		"pods":                   "Pod",
		"nodes":                  "Node",
		"namespaces":             "Namespace",
		"persistentvolumes":      "PersistentVolume",
		"persistentvolumeclaims": "PersistentVolumeClaim",
		"serviceaccounts":        "ServiceAccount",
		"roles":                  "Role",
		"rolebindings":           "RoleBinding",
		"clusterroles":           "ClusterRole",
		"clusterrolebindings":    "ClusterRoleBinding",
	}

	if kind, ok := resourceToKind[lower]; ok {
		return kind
	}

	// Generic conversion: remove trailing 's' and capitalize
	if strings.HasSuffix(lower, "s") {
		singular := lower[:len(lower)-1]
		// Capitalize first letter
		if len(singular) > 0 {
			return strings.ToUpper(singular[:1]) + singular[1:]
		}
	}

	// Fallback: capitalize first letter
	if len(lower) > 0 {
		return strings.ToUpper(lower[:1]) + lower[1:]
	}

	return resourceName
}
