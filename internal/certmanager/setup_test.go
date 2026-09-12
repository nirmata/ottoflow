/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package certmanager

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/go-logr/logr"
	tlsMgr "github.com/kyverno/pkg/tls"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

const (
	testNamespace   = "ottoflow"
	testServiceName = "ottoflow-webhook"
)

// emptyTLSPlaceholder is a pre-existing but unfilled cert Secret: a kubernetes.io/tls Secret
// whose tls.crt and tls.key keys exist but are empty (the API server accepts this; the controller
// fills it by update). ResourceVersion is set to a non-empty value because a real API server
// always assigns one to a stored object — the fake clientset does not do this automatically for
// seed objects — and updateSecret's create-vs-update branch keys off exactly that field.
func emptyTLSPlaceholder(name string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, ResourceVersion: "1"},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       {},
			corev1.TLSPrivateKeyKey: {},
		},
	}
}

// assertValidServiceCerts fetches the CA and leaf Secrets for a service and asserts the filled
// material is a valid, chain-verifiable, PKCS#1-keyed server cert. This is the core regression
// guard: a PKCS#8 key, a missing ServerAuth EKU, or wrong SANs all fail here.
// namespace is deliberately not a parameter: every caller uses testNamespace, and the cert-name
// helpers below derive from it, so the fixtures could not satisfy any other value.
func assertValidServiceCerts(t *testing.T, ctx context.Context, secrets corev1client.SecretInterface, service string) {
	namespace := testNamespace
	t.Helper()

	caSecret, err := secrets.Get(ctx, GetRootCASecretName(service, namespace), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get CA secret: %v", err)
	}
	if len(caSecret.Data[corev1.TLSCertKey]) == 0 || len(caSecret.Data[corev1.TLSPrivateKeyKey]) == 0 {
		t.Fatalf("CA secret %q was not filled", caSecret.Name)
	}

	tlsSecret, err := secrets.Get(ctx, GetTLSPairSecretName(service, namespace), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get TLS secret: %v", err)
	}
	if len(tlsSecret.Data[corev1.TLSCertKey]) == 0 || len(tlsSecret.Data[corev1.TLSPrivateKeyKey]) == 0 {
		t.Fatalf("TLS secret %q was not filled", tlsSecret.Name)
	}

	// The leaf key must parse as PKCS#1 — the form the vendored renewer expects. A PKCS#8 key
	// would decode as a PEM block but fail here (and silently break renewal in production).
	keyBlock, _ := pem.Decode(tlsSecret.Data[corev1.TLSPrivateKeyKey])
	if keyBlock == nil {
		t.Fatal("leaf key is not PEM")
	}
	if _, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes); err != nil {
		t.Fatalf("leaf key is not PKCS#1 (renewer would fail): %v", err)
	}

	// The leaf must verify against the CA as a ServerAuth cert for <service>.<ns>.svc — the SAN
	// the apiserver dials. This transitively proves the CA was filled before the leaf.
	caBlock, _ := pem.Decode(caSecret.Data[corev1.TLSCertKey])
	if caBlock == nil {
		t.Fatal("CA cert is not PEM")
	}
	caCert, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}
	leafBlock, _ := pem.Decode(tlsSecret.Data[corev1.TLSCertKey])
	if leafBlock == nil {
		t.Fatal("leaf cert is not PEM")
	}
	leafCert, err := x509.ParseCertificate(leafBlock.Bytes)
	if err != nil {
		t.Fatalf("parse leaf cert: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	if _, err := leafCert.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSName:   service + "." + namespace + ".svc",
	}); err != nil {
		t.Fatalf("leaf does not verify against CA for %s.%s.svc: %v", service, namespace, err)
	}
}

// newFakeRenewer builds the vendored renewer over a fake clientset's Secret interface, exactly
// as Setup wires it in production, so reconcileCerts can be exercised without an API server.
func newFakeRenewer(clientset *fake.Clientset) (certReconciler, corev1client.SecretInterface) {
	secrets := clientset.CoreV1().Secrets(testNamespace)
	renewer := tlsMgr.NewCertRenewer(
		logr.Discard(), secrets,
		tlsMgr.CertRenewalInterval, tlsMgr.CAValidityDuration, tlsMgr.TLSValidityDuration,
		"", &tlsMgr.Config{ServiceName: testServiceName, Namespace: testNamespace},
	)
	return renewer, secrets
}

// reconcileUntilConverged drives reconcileCerts the way Setup's bootstrap loop does (bounded
// retries), because a single pass can legitimately defer work — the renewer deletes a wrong-type
// Secret and leaves recreation to the next pass.
func reconcileUntilConverged(t *testing.T, ctx context.Context, secrets corev1client.SecretInterface, renewer certReconciler) {
	t.Helper()
	caName := GetRootCASecretName(testServiceName, testNamespace)
	tlsName := GetTLSPairSecretName(testServiceName, testNamespace)
	var err error
	for range 5 {
		if err = reconcileCerts(ctx, secrets, renewer, caName, tlsName, logr.Discard()); err == nil {
			return
		}
	}
	t.Fatalf("reconcileCerts did not converge within 5 passes: %v", err)
}

// testCAMaterial mints a throwaway CA (cert+key) for fixtures. Test-only: production generation
// is owned entirely by the vendored renewer.
func testCAMaterial(t *testing.T, cn string, key *rsa.PrivateKey) (*x509.Certificate, *rsa.PrivateKey) {
	t.Helper()
	if key == nil {
		var err error
		key, err = rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(0),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key
}

// testLeafSignedBy mints a valid leaf for the test service signed by the given CA. Test-only.
func testLeafSignedBy(t *testing.T, caCert *x509.Certificate, caKey *rsa.PrivateKey) ([]byte, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: testServiceName},
		DNSNames: []string{
			testServiceName,
			testServiceName + "." + testNamespace,
			testServiceName + "." + testNamespace + ".svc",
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, key.Public(), caKey)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return certPEM, keyPEM
}

// certToPEMTest encodes certs into one PEM bundle (test-only mirror of the renewer's storage
// format, [oldCA, newCA] on rotation).
func certToPEMTest(certs ...*x509.Certificate) []byte {
	out := make([]byte, 0, 2048*len(certs))
	for _, c := range certs {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})...)
	}
	return out
}

// TestReconcileCerts_FakeClient exercises the renewer-owned reconcile against a fake clientset:
// creation from nothing, filling hand-created empty Secrets, steady-state no-op, wrong-type
// delete+recreate convergence, foreign-signed-leaf repair, and rotation-bundle handling.
func TestReconcileCerts_FakeClient(t *testing.T) {
	ctx := context.Background()
	caName := GetRootCASecretName(testServiceName, testNamespace)
	tlsName := GetTLSPairSecretName(testServiceName, testNamespace)

	t.Run("creates and fills both Secrets from nothing", func(t *testing.T) {
		clientset := fake.NewSimpleClientset()
		renewer, secrets := newFakeRenewer(clientset)
		reconcileUntilConverged(t, ctx, secrets, renewer)
		assertValidServiceCerts(t, ctx, secrets, testServiceName)
	})

	t.Run("fills hand-created empty TLS-typed Secrets via update", func(t *testing.T) {
		clientset := fake.NewSimpleClientset(
			emptyTLSPlaceholder(caName),
			emptyTLSPlaceholder(tlsName),
		)
		renewer, secrets := newFakeRenewer(clientset)
		reconcileUntilConverged(t, ctx, secrets, renewer)
		assertValidServiceCerts(t, ctx, secrets, testServiceName)
	})

	t.Run("steady state issues no writes", func(t *testing.T) {
		clientset := fake.NewSimpleClientset()
		renewer, secrets := newFakeRenewer(clientset)
		reconcileUntilConverged(t, ctx, secrets, renewer)

		clientset.ClearActions()
		reconcileUntilConverged(t, ctx, secrets, renewer)
		for _, a := range clientset.Actions() {
			switch a.GetVerb() {
			case "get", "list", "watch":
			default:
				t.Fatalf("steady-state reconcile issued a %s on %s; expected reads only",
					a.GetVerb(), a.GetResource().Resource)
			}
		}
	})

	t.Run("deletes and recreates a wrong-type cert Secret", func(t *testing.T) {
		// An operator recreated the CA Secret by hand as Opaque. Type is immutable, so the
		// renewer deletes it (one pass) and recreates it (a following pass); reconcileCerts'
		// end-state validation is what turns that deferral into a retry instead of a silent
		// half-done bootstrap.
		wrongType := emptyTLSPlaceholder(caName)
		wrongType.Type = corev1.SecretTypeOpaque
		clientset := fake.NewSimpleClientset(wrongType, emptyTLSPlaceholder(tlsName))
		renewer, secrets := newFakeRenewer(clientset)
		reconcileUntilConverged(t, ctx, secrets, renewer)
		assertValidServiceCerts(t, ctx, secrets, testServiceName)
		got, err := secrets.Get(ctx, caName, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if got.Type != corev1.SecretTypeTLS {
			t.Errorf("recreated CA Secret has type %q, want %q", got.Type, corev1.SecretTypeTLS)
		}
	})

	// The foreign-signed-leaf cases are the regression guard for the CA-replaced-but-leaf-kept
	// bug: a leaf full of VALID material signed by a CA that is not stored anywhere is exactly
	// the state a cluster is left in when the CA Secret is deleted or recreated by hand. The
	// vendored renewer is expiry-only and would keep that leaf ~150 days while every webhook
	// client rejects it; repairLeafChain is what heals it.

	t.Run("regenerates a filled leaf that no stored CA signed", func(t *testing.T) {
		foreignCA, foreignKey := testCAMaterial(t, "foreign-ca", nil)
		leafPEM, leafKeyPEM := testLeafSignedBy(t, foreignCA, foreignKey)
		foreignLeaf := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: tlsName, Namespace: testNamespace, ResourceVersion: "1"},
			Type:       corev1.SecretTypeTLS,
			Data:       map[string][]byte{corev1.TLSCertKey: leafPEM, corev1.TLSPrivateKeyKey: leafKeyPEM},
		}
		clientset := fake.NewSimpleClientset(foreignLeaf) // no CA Secret at all
		renewer, secrets := newFakeRenewer(clientset)
		reconcileUntilConverged(t, ctx, secrets, renewer)
		assertValidServiceCerts(t, ctx, secrets, testServiceName)
		got, err := secrets.Get(ctx, tlsName, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(got.Data[corev1.TLSCertKey], leafPEM) {
			t.Error("leaf cert was left untouched; it is still signed by the old, now-absent CA")
		}
	})

	t.Run("keeps a leaf signed by the OLD cert of a rotated CA bundle", func(t *testing.T) {
		// After a CA rotation the renewer stores [oldCA, newCA] (same key) and clients trust the
		// whole bundle, so an old-signed leaf still verifies everywhere. repairLeafChain must
		// verify against EVERY stored cert — checking only the newest would churn the leaf on
		// every pass for a year.
		oldCA, caKey := testCAMaterial(t, "rotated-ca-old", nil)
		newCA, _ := testCAMaterial(t, "rotated-ca-new", caKey)
		leafPEM, leafKeyPEM := testLeafSignedBy(t, oldCA, caKey)
		caSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: caName, Namespace: testNamespace, ResourceVersion: "1"},
			Type:       corev1.SecretTypeTLS,
			Data: map[string][]byte{
				corev1.TLSCertKey:       certToPEMTest(oldCA, newCA),
				corev1.TLSPrivateKeyKey: pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(caKey)}),
			},
		}
		leafSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: tlsName, Namespace: testNamespace, ResourceVersion: "1"},
			Type:       corev1.SecretTypeTLS,
			Data:       map[string][]byte{corev1.TLSCertKey: leafPEM, corev1.TLSPrivateKeyKey: leafKeyPEM},
		}
		clientset := fake.NewSimpleClientset(caSecret, leafSecret)
		_, secrets := newFakeRenewer(clientset)
		if err := repairLeafChain(ctx, secrets, caName, tlsName, logr.Discard()); err != nil {
			t.Fatalf("repairLeafChain: %v", err)
		}
		got, err := secrets.Get(ctx, tlsName, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("leaf must not be deleted: %v", err)
		}
		if !bytes.Equal(got.Data[corev1.TLSCertKey], leafPEM) {
			t.Error("old-CA-signed leaf was churned although the old cert is still in the stored bundle")
		}
	})

	t.Run("repairs against a rotated CA bundle by signing with the newest cert", func(t *testing.T) {
		// With [oldCA, newCA] stored, the repair path must end with a leaf that verifies against
		// the stored bundle — the renewer signs with the NEWEST cert — so the CA parse must not
		// stop at the first PEM block.
		oldCA, caKey := testCAMaterial(t, "rotated-ca-old", nil)
		newCA, _ := testCAMaterial(t, "rotated-ca-new", caKey)
		foreignCA, foreignKey := testCAMaterial(t, "foreign-ca", nil)
		leafPEM, leafKeyPEM := testLeafSignedBy(t, foreignCA, foreignKey)
		caSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: caName, Namespace: testNamespace, ResourceVersion: "1"},
			Type:       corev1.SecretTypeTLS,
			Data: map[string][]byte{
				corev1.TLSCertKey:       certToPEMTest(oldCA, newCA),
				corev1.TLSPrivateKeyKey: pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(caKey)}),
			},
		}
		leafSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: tlsName, Namespace: testNamespace, ResourceVersion: "1"},
			Type:       corev1.SecretTypeTLS,
			Data:       map[string][]byte{corev1.TLSCertKey: leafPEM, corev1.TLSPrivateKeyKey: leafKeyPEM},
		}
		clientset := fake.NewSimpleClientset(caSecret, leafSecret)
		renewer, secrets := newFakeRenewer(clientset)
		reconcileUntilConverged(t, ctx, secrets, renewer)

		got, err := secrets.Get(ctx, tlsName, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		leafBlock, _ := pem.Decode(got.Data[corev1.TLSCertKey])
		leafCert, err := x509.ParseCertificate(leafBlock.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		if err := leafCert.CheckSignatureFrom(newCA); err != nil {
			t.Errorf("repaired leaf is not signed by the NEWEST stored CA cert: %v", err)
		}
	})

	// The counterpart guard: a leaf that IS correctly signed must be left strictly alone, so the
	// chain check cannot turn every startup into a needless rewrite of both Secrets.
	t.Run("keeps a correctly-signed leaf untouched", func(t *testing.T) {
		clientset := fake.NewSimpleClientset()
		renewer, secrets := newFakeRenewer(clientset)
		reconcileUntilConverged(t, ctx, secrets, renewer)
		before, err := secrets.Get(ctx, tlsName, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		reconcileUntilConverged(t, ctx, secrets, renewer)
		after, err := secrets.Get(ctx, tlsName, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(after.Data[corev1.TLSCertKey], before.Data[corev1.TLSCertKey]) {
			t.Error("a correctly-signed leaf was regenerated; the chain check is causing write amplification")
		}
	})
}

// TestSetup_Envtest is the regression guard against a live API server: it runs Setup for both
// services with none of the four cert Secrets pre-created, and asserts every one ends up created
// and valid. It also asserts a second Setup is a UID-unchanged no-op (the renewer owns steady
// state). Skips when envtest assets are unavailable.
func TestSetup_Envtest(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")

	env := &envtest.Environment{
		// Prefer KUBEBUILDER_ASSETS (set by `make test`); fall back to auto-download for CI.
		BinaryAssetsDirectory:       filepath.Join(repoRoot, "bin", "k8s"),
		DownloadBinaryAssets:        true,
		DownloadBinaryAssetsVersion: "1.29.0",
	}
	cfg, err := env.Start()
	if err != nil {
		t.Skipf("envtest unavailable, skipping cert bootstrap regression test: %v", err)
	}
	defer func() { _ = env.Stop() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := logr.Discard()

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("clientset: %v", err)
	}
	if _, err := clientset.CoreV1().Namespaces().Create(ctx,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNamespace}}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	secrets := clientset.CoreV1().Secrets(testNamespace)

	const agentService = "ottoflow-agent-executor"
	services := []string{testServiceName, agentService}

	// None of the four cert Secrets are pre-created: Setup must create them itself (Kyverno's
	// writeSecret shape), not require a pre-existing placeholder.

	// A long interval keeps the background ticker from firing during the test; Setup's synchronous
	// reconcile does the fill before it returns. renewalInterval is a package var (not a Setup
	// parameter) precisely so tests can do this; restore it so other tests in this package see
	// the production default.
	originalRenewalInterval := renewalInterval
	renewalInterval = time.Hour
	defer func() { renewalInterval = originalRenewalInterval }()

	for _, svc := range services {
		if err := Setup(ctx, logger, cfg, testNamespace, svc); err != nil {
			t.Fatalf("Setup(%s): %v", svc, err)
		}
		assertValidServiceCerts(t, ctx, secrets, svc)
	}

	// A second Setup must be a UID-unchanged no-op: fill skips (already filled) and the renewer
	// leaves fresh certs alone.
	before, err := secrets.Get(ctx, GetTLSPairSecretName(testServiceName, testNamespace), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get TLS before: %v", err)
	}
	if err := Setup(ctx, logger, cfg, testNamespace, testServiceName); err != nil {
		t.Fatalf("second Setup: %v", err)
	}
	after, err := secrets.Get(ctx, GetTLSPairSecretName(testServiceName, testNamespace), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get TLS after: %v", err)
	}
	if before.UID != after.UID {
		t.Errorf("TLS secret UID changed across reconciles: %s -> %s (expected no-op)", before.UID, after.UID)
	}
	if before.ResourceVersion != after.ResourceVersion {
		t.Errorf("TLS secret was rewritten on second reconcile (RV %s -> %s); renewer should no-op",
			before.ResourceVersion, after.ResourceVersion)
	}
}
