package e2e_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openai/tunnel-client/pkg/config"
	tctransport "github.com/openai/tunnel-client/pkg/transport"
)

// tlsFixtures is a throwaway PKI generated per test run: two unrelated CAs
// in one PEM bundle and a 127.0.0.1 server certificate issued by the first.
// Generating it keeps private keys out of the repository and means the
// fixture can never expire.
type tlsFixtures struct {
	serverCertPEM []byte
	serverKeyPEM  []byte
	caBundlePEM   []byte
}

func newTLSFixtures(t *testing.T) tlsFixtures {
	t.Helper()
	newKey := func() *ecdsa.PrivateKey {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatalf("generate key: %v", err)
		}
		return key
	}
	certPEM := func(der []byte) []byte {
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	}
	newCA := func(serial int64, name string) (*x509.Certificate, *ecdsa.PrivateKey, []byte) {
		key := newKey()
		tmpl := &x509.Certificate{
			SerialNumber:          big.NewInt(serial),
			Subject:               pkix.Name{CommonName: name},
			NotBefore:             time.Now().Add(-time.Hour),
			NotAfter:              time.Now().Add(24 * time.Hour),
			KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
			BasicConstraintsValid: true,
			IsCA:                  true,
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
		if err != nil {
			t.Fatalf("create CA %s: %v", name, err)
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatalf("parse CA %s: %v", name, err)
		}
		return cert, key, certPEM(der)
	}
	issuer, issuerKey, issuerPEM := newCA(1, "tunnel-client e2e root A")
	_, _, otherPEM := newCA(2, "tunnel-client e2e root B")

	serverKey := newKey()
	serverTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.IPv6loopback},
		DNSNames:     []string{"localhost"},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTmpl, issuer, &serverKey.PublicKey, issuerKey)
	if err != nil {
		t.Fatalf("create server certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(serverKey)
	if err != nil {
		t.Fatalf("marshal server key: %v", err)
	}
	return tlsFixtures{
		serverCertPEM: certPEM(serverDER),
		serverKeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		caBundlePEM:   append(issuerPEM, otherPEM...),
	}
}

func TestCABundleControlsTLSHandshake(t *testing.T) {
	fixtures := newTLSFixtures(t)
	certPEM, keyPEM, caPEM := fixtures.serverCertPEM, fixtures.serverKeyPEM, fixtures.caBundlePEM

	serverCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("load server key pair: %v", err)
	}

	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{serverCert}}
	server.StartTLS()
	defer server.Close()

	clientNoBundle := newHTTPClient(t, "")
	_, err = clientNoBundle.Get(server.URL)
	if err == nil {
		t.Fatalf("expected TLS handshake to fail without ca-bundle")
	}
	if !strings.Contains(err.Error(), "certificate signed by unknown authority") {
		t.Fatalf("expected unknown authority error, got %v", err)
	}

	caPath := writeTempFile(t, "ca-bundle.pem", caPEM)
	clientWithBundle := newHTTPClient(t, caPath)
	resp, err := clientWithBundle.Get(server.URL)
	if err != nil {
		t.Fatalf("expected TLS handshake to succeed with ca-bundle: %v", err)
	}
	_ = resp.Body.Close()
}

func TestCABundleParsesMultipleCertificates(t *testing.T) {
	caPEM := newTLSFixtures(t).caBundlePEM
	certs := mustParsePEMCerts(t, caPEM)
	if len(certs) < 2 {
		t.Fatalf("expected multiple certificates in CA bundle, found %d", len(certs))
	}

	caPath := writeTempFile(t, "ca-bundle.pem", caPEM)
	cfg := loadConfigWithBundle(t, caPath)
	if cfg.TLS == nil || cfg.TLS.RootCAs == nil {
		t.Fatal("expected TLS bundle with RootCAs")
	}
	subjects := cfg.TLS.RootCAs.Subjects()
	for _, cert := range certs {
		if !subjectsContain(subjects, cert.RawSubject) {
			t.Fatalf("expected bundle to include certificate subject %q", cert.Subject.String())
		}
	}
}

func newHTTPClient(t *testing.T, bundlePath string) *http.Client {
	t.Helper()
	cfg := loadConfigWithBundle(t, bundlePath)
	transport, err := tctransport.CloneDefaultWithBundle(cfg.TLS)
	if err != nil {
		t.Fatalf("build transport: %v", err)
	}
	// Race-enabled standalone builds run this package alongside the rest of
	// the public test suite. Keep a bounded failure guard, but leave enough
	// headroom for a loaded CI worker to schedule the local TLS handshake.
	return &http.Client{Transport: transport, Timeout: 5 * time.Second}
}

func loadConfigWithBundle(t *testing.T, bundlePath string) *config.Config {
	t.Helper()
	args := []string{
		"--control-plane.tunnel-id", "tunnel_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"--mcp.server-url", "https://mcp.example",
	}
	if bundlePath != "" {
		args = append(args, "--ca-bundle", bundlePath)
	}
	cfg, err := config.Load(args, func(key string) (string, bool) {
		if key == "CONTROL_PLANE_API_KEY" {
			return "control-key", true
		}
		return "", false
	})
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg
}

func writeTempFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	return path
}

func mustParsePEMCerts(t *testing.T, data []byte) []*x509.Certificate {
	t.Helper()
	var certs []*x509.Certificate
	for len(data) > 0 {
		block, rest := pem.Decode(data)
		if block == nil {
			break
		}
		if block.Type == "CERTIFICATE" {
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				t.Fatalf("parse certificate: %v", err)
			}
			certs = append(certs, cert)
		}
		data = rest
	}
	if len(certs) == 0 {
		t.Fatal("no certificates parsed from bundle")
	}
	return certs
}

func subjectsContain(subjects [][]byte, subject []byte) bool {
	for _, existing := range subjects {
		if string(existing) == string(subject) {
			return true
		}
	}
	return false
}
