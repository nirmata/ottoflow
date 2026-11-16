/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package certmanager

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"slices"
	"testing"
	"time"

	"github.com/go-logr/logr"
	tlsMgr "github.com/kyverno/pkg/tls"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// TestCARotation_LeafStaysVerifiable drives the vendored renewer through a real CA rotation and
// checks two properties. A2: a stored leaf verifies against the stored CA bundle until its own
// expiry. A1: rotation reuses the CA key. A1 is not over-specification: a client that cached the
// pre-rotation CA cert (a runner keeps the CA it loaded at start-up) verifies leaves signed after
// rotation only because the new CA cert has the same key and subject.
//
// Pass 1 issues a CA that expires in 1h and a leaf with the production leaf validity
// (TLSValidityDuration). Pass 2 runs with the production durations, so the CA is inside its
// renewal window and rotates, while the leaf is still outside its own window and is kept. A2
// therefore checks the production leaf lifetime against the rotated CA's production lifetime
// (CAValidityDuration).
//
// No t.Parallel: NewCertRenewer writes a package-level logger in the vendored module.
func TestCARotation_LeafStaysVerifiable(t *testing.T) {
	ctx := context.Background()
	caName := GetRootCASecretName(testServiceName, testNamespace)
	tlsName := GetTLSPairSecretName(testServiceName, testNamespace)
	clientset := fake.NewSimpleClientset()
	secrets := clientset.CoreV1().Secrets(testNamespace)
	get := func(name string) *corev1.Secret {
		t.Helper()
		sec, err := secrets.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get %s: %v", name, err)
		}
		return sec
	}

	pass1 := tlsMgr.NewCertRenewer(
		logr.Discard(), secrets,
		time.Minute, time.Hour, tlsMgr.TLSValidityDuration,
		"", &tlsMgr.Config{ServiceName: testServiceName, Namespace: testNamespace},
	)
	reconcileUntilConverged(t, ctx, secrets, pass1)
	pass1CAs := parseCertificatesPEM(get(caName).Data[corev1.TLSCertKey])
	if len(pass1CAs) != 1 {
		t.Fatalf("want 1 CA cert after pass 1, got %d", len(pass1CAs))
	}
	pass1CA := pass1CAs[0]
	pass1Leaf := get(tlsName).Data[corev1.TLSCertKey]

	// The fake clientset leaves ResourceVersion empty on created objects, and the renewer creates
	// rather than updates when it is empty (see emptyTLSPlaceholder). Stamp both Secrets so pass 2
	// updates them as it would against a real API server.
	for _, name := range []string{caName, tlsName} {
		sec := get(name)
		sec.ResourceVersion = "1"
		if _, err := secrets.Update(ctx, sec, metav1.UpdateOptions{}); err != nil {
			t.Fatalf("stamp ResourceVersion on %s: %v", name, err)
		}
	}

	pass2, _ := newFakeRenewer(clientset)
	reconcileUntilConverged(t, ctx, secrets, pass2)

	caSecret := get(caName)
	caCerts := parseCertificatesPEM(caSecret.Data[corev1.TLSCertKey])

	// A0: the CA rotated and the pass-1 leaf was kept, so the checks below are not vacuous.
	if !slices.ContainsFunc(caCerts, func(c *x509.Certificate) bool { return !c.Equal(pass1CA) }) {
		t.Fatalf("A0: no stored CA cert other than the pass-1 cert after pass 2 (%d cert(s)); the CA did not rotate",
			len(caCerts))
	}
	if !bytes.Equal(get(tlsName).Data[corev1.TLSCertKey], pass1Leaf) {
		t.Fatal("A0: pass 2 replaced the pass-1 leaf")
	}

	// A1: rotation reuses the pass-1 CA key: the stored key and the newest stored cert both carry it.
	keyBlock, _ := pem.Decode(caSecret.Data[corev1.TLSPrivateKeyKey])
	if keyBlock == nil {
		t.Fatal("CA key is not PEM")
	}
	caKey, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes)
	if err != nil {
		t.Fatalf("parse CA key: %v", err)
	}
	stored, newest := &caKey.PublicKey, caCerts[len(caCerts)-1]
	if !stored.Equal(pass1CA.PublicKey) || !stored.Equal(newest.PublicKey) {
		t.Errorf("A1: CA key not reused on rotation (stored==pass1: %v, stored==newest: %v)",
			stored.Equal(pass1CA.PublicKey), stored.Equal(newest.PublicKey))
	}

	// A2: the stored leaf still verifies against the stored CA bundle just before its own expiry.
	leafCerts := parseCertificatesPEM(get(tlsName).Data[corev1.TLSCertKey])
	if len(leafCerts) == 0 {
		t.Fatal("leaf secret holds no certificate")
	}
	leaf := leafCerts[0]
	roots := x509.NewCertPool()
	for _, c := range caCerts {
		roots.AddCert(c)
	}
	at := leaf.NotAfter.Add(-time.Minute)
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:       roots,
		DNSName:     testServiceName + "." + testNamespace + ".svc",
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		CurrentTime: at,
	}); err != nil {
		t.Errorf("A2: stored leaf does not verify against the stored CA bundle at %s: %v",
			at.Format(time.RFC3339), err)
	}
}
