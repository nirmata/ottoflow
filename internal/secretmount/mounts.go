/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

// Package secretmount is the shared contract between the controller (which mounts
// Secret keys into the workflow-runner Job by reference — see
// internal/workflow/controller/secret_refs.go) and the runner process code that needs
// those values at execution time (internal/workflow/cluster, internal/workflow/executor,
// internal/agent). The runner Job holds no Secret RBAC at all, so every value it needs is
// either a plain pod env var (kubelet-resolved SecretKeyRef — used for the well-known LLM
// credentials Secret and any user-supplied spec.execution.job.env entry, never for MCP env
// creds; see buildSecretMounts) or a file under a path recorded in this map (kubeconfig, A2A
// CA/token, MCP auth secrets, MCP env creds).
package secretmount

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ErrNotFound marks the "this value simply isn't there" outcomes of Resolve/ReadFile: the
// key is not in the mount map, the mounted file does not exist (an Optional=true Secret
// volume whose Secret or key is absent), or — on the API path — the Secret or its key does
// not exist. Kubernetes' contract for an optional SecretKeyRef is that exactly these cases
// leave the variable unset, so callers test errors.Is(err, ErrNotFound) to honour Optional.
// Any OTHER error (a malformed OTTOFLOW_SECRET_MOUNTS, an unreadable mounted file, an API
// failure) is a broken runtime, not absence, and must be surfaced even for an optional ref.
var ErrNotFound = errors.New("secret value not found")

// EnvVar is the environment variable the controller sets on the runner Job carrying the
// JSON-encoded Mounts map (see internal/workflow/controller/secret_refs.go).
const EnvVar = "OTTOFLOW_SECRET_MOUNTS"

// KubeconfigDir is the fixed directory a WorkflowRun.Spec.ClusterRef.KubeConfigSecretRef is
// mounted into as a whole-secret volume (every key in the Secret becomes a file here,
// since the key to use is optional and probed at read time). Not part of the Mounts map
// because there is at most one per WorkflowRun and the path never varies.
const KubeconfigDir = "/etc/ottoflow/secrets/kubeconfig"

// dataKeyPattern is the Kubernetes API server's own rule for a Secret (or ConfigMap) data
// key: apimachinery's IsConfigMapKey regexp, `[-._a-zA-Z0-9]+`. Reproduced here rather than
// imported because ValidateDataKey also has to reject the three forms the API server refuses
// on top of that regexp ('.', '..', and any '..'-prefixed key), which live in the
// kube-apiserver's own validation package and not in apimachinery.
var dataKeyPattern = regexp.MustCompile(`^[-._a-zA-Z0-9]+$`)

// ValidateDataKey rejects any string that could not be a real Secret data key.
//
// Every "key" OttoFlow mounts or reads comes from a free-form CRD string field
// (MCPServer.spec.auth.secretRef.key, an env SecretKeyRef key,
// WorkflowRun.spec.clusterRef.kubeConfigSecretRef.key, ...) — none of which the API server
// validates against the Secret data-key rules, because none of them is a Secret data key at
// admission time. The controller then uses those strings as a projected FILENAME
// (corev1.KeyToPath.Path in secret_refs.go buildSecretMounts) and the runner uses them to
// build a FILE PATH under the mount directory (internal/workflow/cluster resolve.go,
// Mounts.ReadFile). An unconstrained value there is a path-traversal input, so it is checked
// once, here, against exactly the rules a Secret data key must already satisfy:
//
//	'/' and every other character outside [-._a-zA-Z0-9] is rejected by the API server's
//	IsConfigMapKey regexp; '.', '..' and any key starting with '..' are rejected separately
//	(verified against a live kube-apiserver: `data[..]: Invalid value: "..": must not be
//	'..'`, `data[subdir/file]: ... must consist of alphanumeric characters, '-', '_' or '.'`).
//
// So this can only ever reject a value that could never have matched a key in a real Secret:
// no working configuration is refused, and every accepted value is a single safe filename.
func ValidateDataKey(key string) error {
	switch {
	case key == "":
		return fmt.Errorf("secret data key is empty")
	case key == "." || key == "..":
		return fmt.Errorf("secret data key %q is not a valid Secret data key (must not be %q)", key, key)
	case strings.HasPrefix(key, ".."):
		return fmt.Errorf("secret data key %q is not a valid Secret data key (must not start with %q)", key, "..")
	case !dataKeyPattern.MatchString(key):
		return fmt.Errorf(
			"secret data key %q is not a valid Secret data key (must consist only of alphanumeric "+
				"characters, '-', '_' or '.'; a path separator can never appear in a Secret data key)", key)
	}
	return nil
}

// Mounts maps a canonical secret-ref key (see Key) to the file path it is mounted at.
type Mounts map[string]string

// Key returns the canonical lookup key for one Secret data key. Both the controller
// (building the map in secret_refs.go) and the runner (looking values up) must derive
// this key the same way, so a value mounted for one step/resource is found regardless of
// which step, MCPServer, or Agent referenced it.
func Key(namespace, name, key string) string {
	return namespace + "/" + name + "/" + key
}

// mountsOnce memoizes Load's result: OTTOFLOW_SECRET_MOUNTS is set once by the controller
// when it creates the runner Job (see internal/workflow/controller/secret_refs.go) and never
// changes for the life of the process, so re-parsing it on every call — several of them per
// step, one inside a per-env-var loop — is wasted work. sync.OnceValues caches both outcomes,
// so a parse failure is cached and re-returned on every call rather than silently swallowed
// after the first one.
var mountsOnce = sync.OnceValues(loadMountsFromEnv)

// Load returns OTTOFLOW_SECRET_MOUNTS parsed from the environment, memoized for the life of
// the process (see mountsOnce). Returns an empty, non-nil map when the env var is unset
// (local execution mode, or a workflow with no keyed secret refs) so callers can look up
// unconditionally without a nil check.
func Load() (Mounts, error) {
	return mountsOnce()
}

// ResetForTest clears Load's memoization so the next call re-reads the environment. Load is
// memoized on the assumption that OTTOFLOW_SECRET_MOUNTS never changes in a real process
// (see mountsOnce); a test in another package that sets it to different values across
// multiple cases in one test binary must call this before each such change.
func ResetForTest() {
	mountsOnce = sync.OnceValues(loadMountsFromEnv)
}

// loadMountsFromEnv does the actual parsing; Load (via mountsOnce) calls it exactly once.
func loadMountsFromEnv() (Mounts, error) {
	raw := os.Getenv(EnvVar)
	if raw == "" {
		return Mounts{}, nil
	}
	var m Mounts
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", EnvVar, err)
	}
	return m, nil
}

// Resolve returns the value of one Secret data key, via a live API Get when useAPI is true
// (the CLI, which has full Secret RBAC) or via the controller-mounted file otherwise (the
// in-cluster workflow-runner Job, which holds none). This is the single dual-path
// implementation for every caller that needs one Secret key regardless of execution mode,
// so the two paths cannot drift in behaviour or error wording.
func Resolve(ctx context.Context, k8sClient client.Client, useAPI bool, namespace, name, key string) ([]byte, error) {
	if useAPI {
		secret := &corev1.Secret{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, secret); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("getting secret %s/%s: %v: %w", namespace, name, err, ErrNotFound)
			}
			return nil, fmt.Errorf("getting secret %s/%s: %w", namespace, name, err)
		}
		v, ok := secret.Data[key]
		if !ok {
			return nil, fmt.Errorf("key %q not found in secret %s/%s: %w", key, namespace, name, ErrNotFound)
		}
		return v, nil
	}
	mounts, err := Load()
	if err != nil {
		return nil, fmt.Errorf("loading secret mounts: %w", err)
	}
	return mounts.ReadFile(namespace, name, key)
}

// ReadFile returns the contents of the mounted file for (namespace, name, key), or an
// error if no such mount exists or the file cannot be read.
func (m Mounts) ReadFile(namespace, name, key string) ([]byte, error) {
	path, ok := m[Key(namespace, name, key)]
	if !ok {
		return nil, fmt.Errorf("secret %s/%s key %q is not mounted into the runner (not in %s): %w",
			namespace, name, key, EnvVar, ErrNotFound)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// An Optional=true Secret volume projects nothing when the Secret or key is
			// absent, so the mount-map entry exists but the file does not — that is the
			// "unset optional variable" case, not a broken runtime.
			return nil, fmt.Errorf("reading mounted secret %s/%s key %q at %s: %v: %w",
				namespace, name, key, path, err, ErrNotFound)
		}
		return nil, fmt.Errorf("reading mounted secret %s/%s key %q at %s: %w", namespace, name, key, path, err)
	}
	return b, nil
}
