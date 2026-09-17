/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package cluster

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
	"github.com/nirmata/ottoflow/internal/secretmount"
)

// kubeconfigMountDir is secretmount.KubeconfigDir, held in a var so tests can point it at
// a temp directory instead of the real container path.
var kubeconfigMountDir = secretmount.KubeconfigDir

// ClientFromRESTConfig creates a controller-runtime client from a rest.Config.
func ClientFromRESTConfig(restConfig *rest.Config, scheme *runtime.Scheme) (client.Client, error) {
	c, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("create client from rest config: %w", err)
	}
	return c, nil
}

// RestConfigForClusterRef resolves the target cluster config for a WorkflowRun.
// When ClusterRef is nil or local is selected, in-cluster config is used.
//
// localExecutionMode distinguishes the CLI (true, has its own kubeconfig and full API
// access) from the in-cluster workflow-runner Job (false, which reads Secrets only from the
// files the controller mounted into the pod). For a KubeConfigSecretRef, the mounted file
// under secretmount.KubeconfigDir is always tried first; only when localExecutionMode is
// true does a missing mount fall back to a live Secret API read.
func RestConfigForClusterRef(
	ctx context.Context,
	controlClient client.Client,
	workflowRun *ottoflowv1alpha1.WorkflowRun,
	localExecutionMode bool,
) (*rest.Config, error) {
	if workflowRun == nil || workflowRun.Spec.ClusterRef == nil {
		return rest.InClusterConfig()
	}

	clusterRef := workflowRun.Spec.ClusterRef
	if clusterRef.Local != nil && *clusterRef.Local {
		return rest.InClusterConfig()
	}
	if clusterRef.KubeConfigFilePath != "" {
		return RestConfigFromKubeConfigFile(clusterRef.KubeConfigFilePath)
	}
	if clusterRef.KubeConfigSecretRef != nil {
		ref := clusterRef.KubeConfigSecretRef
		secretNamespace := ref.Namespace
		if secretNamespace == "" {
			secretNamespace = workflowRun.Namespace
		}

		kubeconfig, triedKeys, keyErr := kubeConfigBytesFromMountDir(kubeconfigMountDir, ref.Key)
		if keyErr != nil {
			// Fail closed on an unusable key in BOTH execution modes: the same value would be
			// handed to the live-Secret path below, where it can only ever miss.
			return nil, keyErr
		}
		if kubeconfig != nil {
			restConfig, err := restConfigFromKubeConfig(kubeconfig)
			if err != nil {
				return nil, fmt.Errorf("build rest config from mounted kubeconfig %s: %w", kubeconfigMountDir, err)
			}
			return restConfig, nil
		} else if !localExecutionMode {
			return nil, fmt.Errorf("kubeconfig not found in mounted secret directory %s (tried keys %v); the in-cluster runner does not read Secrets through the API", kubeconfigMountDir, triedKeys)
		}

		return RestConfigFromKubeConfigSecret(ctx, controlClient, secretNamespace, ref.Name, ref.Key)
	}

	return rest.InClusterConfig()
}

// ClientForClusterRef resolves a cluster ref and builds a controller-runtime client for it.
func ClientForClusterRef(
	ctx context.Context,
	controlClient client.Client,
	scheme *runtime.Scheme,
	workflowRun *ottoflowv1alpha1.WorkflowRun,
	localExecutionMode bool,
) (client.Client, error) {
	restConfig, err := RestConfigForClusterRef(ctx, controlClient, workflowRun, localExecutionMode)
	if err != nil {
		return nil, err
	}
	return ClientFromRESTConfig(restConfig, scheme)
}

// kubeConfigBytesFromMountDir probes for a mounted kubeconfig file under dir, trying
// dataKey if set, else the defaultKubeConfigKeys in order (mirrors the probe order used
// against a live Secret's Data map in kubeConfigBytesFromSecret). Returns nil bytes if no
// file is found at any of the tried keys.
//
// dataKey is WorkflowRun.spec.clusterRef.kubeConfigSecretRef.key — a free-form CRD string
// the API server never validates as a Secret data key — and it is joined into a filesystem
// path here. Without the secretmount.ValidateDataKey check it is a path-traversal input:
// a value like "../../../../var/run/secrets/kubernetes.io/serviceaccount/token" escapes the
// kubeconfig mount and makes the runner read (and report errors about) an unrelated file.
// A rejected key can never have named a key in a real Secret, so no working configuration
// is refused; the error names the offending value rather than silently probing the
// default keys, which would mask a typo as "kubeconfig not found".
func kubeConfigBytesFromMountDir(dir, dataKey string) ([]byte, []string, error) {
	triedKeys := defaultKubeConfigKeys
	if dataKey != "" {
		if err := secretmount.ValidateDataKey(dataKey); err != nil {
			return nil, nil, fmt.Errorf("clusterRef.kubeConfigSecretRef.key is unusable: %w", err)
		}
		triedKeys = []string{dataKey}
	}
	for _, key := range triedKeys {
		if b, err := os.ReadFile(filepath.Join(dir, key)); err == nil {
			return b, triedKeys, nil
		}
	}
	return nil, triedKeys, nil
}

// RestConfigFromKubeConfigSecret loads kubeconfig bytes from a Secret and returns a rest.Config.
func RestConfigFromKubeConfigSecret(
	ctx context.Context,
	k8sClient client.Client,
	secretNamespace, secretName, dataKey string,
) (*rest.Config, error) {
	kubeconfig, err := kubeConfigBytesFromSecret(ctx, k8sClient, secretNamespace, secretName, dataKey)
	if err != nil {
		return nil, err
	}
	restConfig, err := restConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("build rest config from kubeconfig secret %s/%s: %w", secretNamespace, secretName, err)
	}
	return restConfig, nil
}

// RestConfigFromKubeConfigFile loads kubeconfig bytes from a mounted file and returns a rest.Config.
func RestConfigFromKubeConfigFile(path string) (*rest.Config, error) {
	kubeconfig, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read kubeconfig file %s: %w", path, err)
	}
	restConfig, err := restConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("build rest config from kubeconfig file %s: %w", path, err)
	}
	return restConfig, nil
}

func kubeConfigBytesFromSecret(
	ctx context.Context,
	k8sClient client.Client,
	secretNamespace, secretName, dataKey string,
) ([]byte, error) {
	secret := &corev1.Secret{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: secretNamespace, Name: secretName}, secret); err != nil {
		return nil, fmt.Errorf("get kubeconfig secret %s/%s: %w", secretNamespace, secretName, err)
	}

	if dataKey != "" {
		if b, ok := secret.Data[dataKey]; ok {
			return b, nil
		}
		return nil, fmt.Errorf("secret %s/%s has no key %q", secretNamespace, secretName, dataKey)
	}

	for _, key := range defaultKubeConfigKeys {
		if b, ok := secret.Data[key]; ok {
			return b, nil
		}
	}

	return nil, fmt.Errorf("secret %s/%s has none of keys %v", secretNamespace, secretName, defaultKubeConfigKeys)
}
