/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

// maxUncheckedEventNote keeps the Event note under the API's 1 KiB limit (an over-long note is dropped).
const maxUncheckedEventNote = 1023

// errRunnerRefUnresolved marks a build failure for a minted Secret volume that is missing or lacks a key.
var errRunnerRefUnresolved = errors.New("runner secret reference unresolved")

// secretMountFailureReason maps errRunnerRefUnresolved to its failure reason and defers to
// secretAccessDeniedReason for everything else.
func secretMountFailureReason(err error) ottoflowv1alpha1.WorkflowRunFailureReason {
	if errors.Is(err, errRunnerRefUnresolved) {
		return ottoflowv1alpha1.WorkflowRunFailureReasonRunnerRefUnresolved
	}
	return secretAccessDeniedReason(err)
}

// checkSecretMounts fails early on a missing Secret or projected key among the mandatory volumes
// buildSecretMounts minted, when the controller is allowed to read that exact Secret. Volumes it
// cannot check (review denied or inconclusive, any read error other than NotFound) are returned by
// origin: the kubelet mounts with node credentials, so the controller's own read says nothing there.
// Optional volumes are skipped entirely. Messages name only the origin, Secret and the missing key.
func (r *WorkflowRunReconciler) checkSecretMounts(ctx context.Context, namespace string, volumes []corev1.Volume, origins map[string]string) ([]string, error) {
	var notChecked []string
	for _, v := range volumes {
		s := v.Secret
		if s == nil || (s.Optional != nil && *s.Optional) {
			continue
		}
		key := types.NamespacedName{Namespace: namespace, Name: s.SecretName}
		if allowed, err := canGetSecret(ctx, r.Client, key); err != nil || !allowed {
			notChecked = append(notChecked, origins[v.Name])
			continue
		}
		sec := &corev1.Secret{}
		if err := r.directReader().Get(ctx, key, sec); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("%w: %s: Secret %q not found in namespace %q", errRunnerRefUnresolved, origins[v.Name], key.Name, namespace)
			}
			klog.V(2).InfoS("pre-check read of runner Secret failed; leaving it to the kubelet", "namespace", namespace, "secret", key.Name, "err", err)
			notChecked = append(notChecked, origins[v.Name])
			continue
		}
		for _, it := range s.Items {
			if _, ok := sec.Data[it.Key]; !ok {
				return nil, fmt.Errorf("%w: %s: Secret %q has no key %q", errRunnerRefUnresolved, origins[v.Name], key.Name, it.Key)
			}
		}
	}
	return notChecked, nil
}

// recordUncheckedSecretMounts emits one Normal Event per build attempt that left mounts unchecked.
func (r *WorkflowRunReconciler) recordUncheckedSecretMounts(workflowRun *ottoflowv1alpha1.WorkflowRun, notChecked []string) {
	if r.EventRecorder == nil || len(notChecked) == 0 {
		return
	}
	const prefix = "Secret mounts not verified before creating the runner Job (the controller cannot read them): "
	note, listed := prefix, 0
	for _, o := range notChecked {
		sep := ""
		if listed > 0 {
			sep = "; "
		}
		if len(note)+len(sep)+len(o)+len(" … and 999999 more") > maxUncheckedEventNote {
			break
		}
		note += sep + o
		listed++
	}
	if more := len(notChecked) - listed; more > 0 {
		note = strings.TrimRight(note, " ") + fmt.Sprintf(" … and %d more", more)
	}
	r.EventRecorder.Eventf(workflowRun, nil, corev1.EventTypeNormal, "SecretMountsNotChecked", "SecretMountCheck", "%s", note)
}
