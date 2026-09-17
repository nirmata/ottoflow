/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package certmanager

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	tlsMgr "github.com/kyverno/pkg/tls"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
)

const (
	// bootstrapBudget bounds the synchronous bootstrap phase of Setup. Generous on purpose: the
	// informer model this replaces never crashed the process on a transient API error, and with
	// several replicas racing to create the same Secrets on a fresh install, one replica losing a
	// few create/update races (AlreadyExists/Conflict) and re-reading the winner's material is the
	// NORMAL path, not a failure. BootstrapWebhookCerts' own TLS-secret poll already tolerates
	// 60s, so a budget in the same range costs nothing.
	bootstrapBudget = 45 * time.Second
	// bootstrapRetryDelay starts the bootstrap backoff; it doubles per failed pass up to
	// bootstrapRetryDelayMax so a contended fresh install converges without hammering the API.
	bootstrapRetryDelay    = time.Second
	bootstrapRetryDelayMax = 8 * time.Second
	// reconcileRetryDelay is how long a failed steady-state renewal waits before retrying.
	reconcileRetryDelay = time.Minute
)

// renewalInterval is the steady-state renewal tick cadence. Every production caller passes
// tlsMgr.CertRenewalInterval (there is no per-deployment reason to vary it); tests that need a
// different cadence reassign this var directly instead of threading it through as a parameter.
var renewalInterval = tlsMgr.CertRenewalInterval

// Setup creates and fills the TLS cert Secrets and keeps them renewed, with the vendored
// kyverno/pkg/tls renewer as the SINGLE owner of certificate generation (no external
// cert-manager). RenewCA creates-or-fills the CA Secret when it is missing or empty (its
// writeSecret creates when ResourceVersion is empty) and rotates it near expiry; RenewTLS does
// the same for the leaf, signing against the newest stored CA cert. Wrong-type Secrets are
// deleted by the renewer itself (Secret type is immutable, so delete+recreate is the only
// rotation path) and recreated on a following pass. One implementation owning generation means
// bootstrap-time and rotation-time material can never diverge (CN, key format, SANs, EKU).
//
// The one repair the expiry-driven renewer cannot make — a stored leaf that is not signed by the
// stored CA (a replaced or backup-restored CA Secret) — is handled by
// repairLeafChain before each renewer pass: the mis-signed leaf is deleted so RenewTLS
// regenerates it against the CA that is actually stored. Each reconcile pass ends with the
// renewer's own chain validation, so a pass that deferred work (e.g. a wrong-type delete)
// reports "not converged" and is retried instead of leaving a broken pair until the next
// renewal tick.
func Setup(ctx context.Context, logger logr.Logger, config *rest.Config, namespace, serviceName string) error {
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return err
	}

	tlsConfig := &tlsMgr.Config{
		ServiceName: serviceName,
		Namespace:   namespace,
	}

	secretClient := clientset.CoreV1().Secrets(namespace)
	renewer := tlsMgr.NewCertRenewer(
		logger,
		secretClient,
		tlsMgr.CertRenewalInterval,
		tlsMgr.CAValidityDuration,
		tlsMgr.TLSValidityDuration,
		"", // server - empty for in-cluster
		tlsConfig,
	)

	caName := tlsMgr.GenerateRootCASecretName(tlsConfig)
	tlsName := tlsMgr.GenerateTLSPairSecretName(tlsConfig)

	reconcile := func(ctx context.Context) error {
		return reconcileCerts(ctx, secretClient, renewer, caName, tlsName, logger)
	}

	// Reconcile synchronously before returning, with backoff inside bootstrapBudget. Transient
	// API errors and multi-replica create/update races (AlreadyExists, Conflict) are ordinary
	// here — the losing replica's next pass sees the winner's material and no-ops — so give
	// convergence real time instead of crashing the controller on a slow peer.
	deadline := time.Now().Add(bootstrapBudget)
	delay := bootstrapRetryDelay
	var lastErr error
	for attempt := 1; ; attempt++ {
		if lastErr = reconcile(ctx); lastErr == nil {
			break
		}
		if time.Now().Add(delay).After(deadline) {
			return fmt.Errorf("cert bootstrap did not converge within %s: %w", bootstrapBudget, lastErr)
		}
		logger.Error(lastErr, "cert bootstrap reconcile failed; retrying", "attempt", attempt, "retryIn", delay)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		if delay *= 2; delay > bootstrapRetryDelayMax {
			delay = bootstrapRetryDelayMax
		}
	}

	// Steady-state renewal on a timer (replaces the old namespace-wide Secret informer). Each tick
	// drives the renewer; a failed tick retries at reconcileRetryDelay until it succeeds or the
	// context is cancelled, then the normal cadence resumes.
	go func() {
		ticker := time.NewTicker(renewalInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			for {
				if err := reconcile(ctx); err == nil {
					break
				} else {
					logger.Error(err, "cert renewal reconcile failed, retrying")
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(reconcileRetryDelay):
				}
			}
		}
	}()

	logger.Info("internal TLS certificate manager started",
		"namespace", namespace,
		"serviceName", serviceName,
		"caSecret", caName,
		"tlsSecret", tlsName,
	)
	return nil
}

// certReconciler is the subset of the vendored renewer reconcileCerts drives. kyverno's
// NewCertRenewer returns a concrete type satisfying all three methods.
type certReconciler interface {
	tlsMgr.CertRenewer
	ValidateCert(context.Context) (bool, error)
}

// reconcileCerts drives one full pass: chain repair, then the renewer (which creates, fills, or
// rotates as needed), then the renewer's own end-state validation, so a pass that deferred work
// — e.g. the renewer deletes a wrong-type Secret and returns, leaving recreation to a later
// pass — reports "not converged" and gets retried by the caller instead of being silently
// accepted with a missing or broken pair.
func reconcileCerts(ctx context.Context, secretClient corev1client.SecretInterface, renewer certReconciler, caName, tlsName string, logger logr.Logger) error {
	// The vendored renewer PANICS on a cert Secret whose tls.key is present but not PEM at all
	// (pemToPrivateKey dereferences pem.Decode's result unchecked) — the exact shape of a
	// hand-created empty placeholder. Such material can never be used or rotated, so delete the
	// Secret first and let the renewer recreate it.
	for _, name := range []string{caName, tlsName} {
		if err := deleteSecretWithUndecodableKey(ctx, secretClient, name, logger); err != nil {
			return err
		}
	}
	if err := repairLeafChain(ctx, secretClient, caName, tlsName, logger); err != nil {
		return err
	}
	if err := renewer.RenewCA(ctx); err != nil {
		return err
	}
	if err := renewer.RenewTLS(ctx); err != nil {
		return err
	}
	ok, err := renewer.ValidateCert(ctx)
	if err != nil {
		return fmt.Errorf("validating cert Secrets after reconcile: %w", err)
	}
	if !ok {
		return fmt.Errorf("cert Secrets %q/%q are not yet a valid chain; retrying", caName, tlsName)
	}
	return nil
}

// deleteSecretWithUndecodableKey removes a cert Secret whose tls.key is PRESENT but contains no
// PEM block at all — e.g. a hand-created placeholder with empty data values. That state is
// unusable (nothing can sign or rotate with it) and, worse, the vendored renewer's
// pemToPrivateKey dereferences pem.Decode's nil result and panics on it, so it can never be
// healed in place. A key that PEM-decodes but fails to parse (e.g. PKCS#8) is deliberately NOT
// deleted: the renewer reports that as an ordinary error, and destroying possibly-real foreign
// material on a parse mismatch would be worse than surfacing the error.
func deleteSecretWithUndecodableKey(ctx context.Context, secretClient corev1client.SecretInterface, name string, logger logr.Logger) error {
	secret, err := secretClient.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("reading cert Secret %q: %w", name, err)
	}
	keyBytes, present := secret.Data[corev1.TLSPrivateKeyKey]
	if !present || keyBytes == nil {
		return nil // absent key: the renewer treats the Secret as unfilled and fills it
	}
	if block, _ := pem.Decode(keyBytes); block != nil {
		return nil // decodable; leave interpretation to the renewer
	}
	logger.Info("cert Secret holds a private key that is not PEM (e.g. an empty placeholder); deleting so it is regenerated",
		"secret", name)
	if err := secretClient.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting unusable cert Secret %q: %w", name, err)
	}
	return nil
}

// repairLeafChain deletes the leaf cert Secret when its certificate is not signed by ANY
// certificate in the stored CA Secret, so the renewer's next RenewTLS regenerates it against the
// CA that actually exists. This is the one broken state the expiry-only vendored renewer never
// heals: a CA Secret that was replaced (wrong-type recreate, a backup restore) leaves a leaf
// that every webhook client rejects the moment the new CA is published,
// while the renewer would keep it until expiry — up to ~150 days.
//
// Verification is against EVERY stored CA certificate, not only the newest: on rotation the
// renewer stores [oldCA, newCA] (reusing the CA key) and the full bundle is what clients trust,
// so a leaf signed by the old cert still verifies everywhere and must not be churned. Signature
// check only (CheckSignatureFrom), no expiry check — expiry-driven rotation stays the renewer's
// job, and duplicating it here would regenerate the leaf on every pass inside the renewal window.
//
// Conservative on anything unreadable: a missing/empty leaf is the renewer's create path, and
// with no decodable CA certificate there is nothing trustworthy to verify against (and nothing
// to re-sign with), so the leaf is left alone for the renewer to sort out.
func repairLeafChain(ctx context.Context, secretClient corev1client.SecretInterface, caName, tlsName string, logger logr.Logger) error {
	leaf, err := secretClient.Get(ctx, tlsName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil // nothing to repair; RenewTLS creates it
		}
		return fmt.Errorf("reading leaf cert Secret %q for chain repair: %w", tlsName, err)
	}
	leafBlock, _ := pem.Decode(leaf.Data[corev1.TLSCertKey])
	if leafBlock == nil {
		return nil // empty/undecodable leaf is the renewer's fill path
	}
	leafCert, err := x509.ParseCertificate(leafBlock.Bytes)
	if err != nil {
		return nil // ditto
	}

	caSecret, err := secretClient.Get(ctx, caName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil // no CA stored: RenewCA will create one; do not guess about the leaf yet
		}
		return fmt.Errorf("reading CA Secret %q for chain repair: %w", caName, err)
	}
	caCerts := parseCertificatesPEM(caSecret.Data[corev1.TLSCertKey])
	if len(caCerts) == 0 {
		return nil // no decodable CA to verify against — conservative keep
	}
	for _, ca := range caCerts {
		if leafCert.CheckSignatureFrom(ca) == nil {
			return nil // signed by a stored CA cert; nothing to repair
		}
	}
	logger.Info("leaf cert Secret is not signed by any stored CA certificate; deleting so it is regenerated",
		"leafSecret", tlsName, "caSecret", caName)
	if err := secretClient.Delete(ctx, tlsName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting mis-signed leaf cert Secret %q: %w", tlsName, err)
	}
	return nil
}

// parseCertificatesPEM decodes every parsable CERTIFICATE block in raw, skipping anything else.
func parseCertificatesPEM(raw []byte) []*x509.Certificate {
	var certs []*x509.Certificate
	for {
		block, rest := pem.Decode(raw)
		if block == nil {
			return certs
		}
		raw = rest
		if cert, err := x509.ParseCertificate(block.Bytes); err == nil {
			certs = append(certs, cert)
		}
	}
}

// GetTLSPairSecretName returns the secret name used for the TLS cert (for mounting in agent-executor).
func GetTLSPairSecretName(serviceName, namespace string) string {
	return tlsMgr.GenerateTLSPairSecretName(&tlsMgr.Config{
		ServiceName: serviceName,
		Namespace:   namespace,
	})
}

// GetRootCASecretName returns the secret name used for the CA cert.
func GetRootCASecretName(serviceName, namespace string) string {
	return tlsMgr.GenerateRootCASecretName(&tlsMgr.Config{
		ServiceName: serviceName,
		Namespace:   namespace,
	})
}
