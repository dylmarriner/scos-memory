package tlsserve

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func generateTestCertPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{Organization: []string{"scos-test"}},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		IsCA:         true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestListenAndServeFailsClosedWhenRequiredWithoutCert(t *testing.T) {
	t.Setenv("SCOS_TLS_CERT_FILE", "")
	t.Setenv("SCOS_TLS_KEY_FILE", "")
	t.Setenv("SCOS_TLS_REQUIRED", "true")
	if err := ListenAndServe(":0", nil); err == nil {
		t.Fatal("expected an error when TLS is required but no certificate is configured")
	}
}

func TestTrustedClientUsesSystemPoolWithoutCAFile(t *testing.T) {
	t.Setenv("SCOS_TLS_CA_FILE", "")
	client, err := TrustedClient(5 * time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if client.Timeout != 5*time.Second {
		t.Fatalf("expected timeout to be preserved, got %v", client.Timeout)
	}
}

func TestTrustedClientRejectsInvalidCAFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(path, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCOS_TLS_CA_FILE", path)
	if _, err := TrustedClient(5 * time.Second); err == nil {
		t.Fatal("expected an error for an invalid CA file")
	}
}

func TestTrustedClientLoadsValidCAFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(path, generateTestCertPEM(t), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCOS_TLS_CA_FILE", path)
	client, err := TrustedClient(3 * time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if client.Transport == nil {
		t.Fatal("expected a configured transport when a CA file is set")
	}
}

func TestPartialTLSConfigIsAnError(t *testing.T) {
	for _, env := range [][2]string{{"SCOS_TLS_CERT_FILE", "/tmp/cert.pem"}, {"SCOS_TLS_KEY_FILE", "/tmp/key.pem"}} {
		t.Setenv("SCOS_TLS_CERT_FILE", "")
		t.Setenv("SCOS_TLS_KEY_FILE", "")
		t.Setenv(env[0], env[1])
		if err := ListenAndServe("127.0.0.1:0", http.NotFoundHandler()); err == nil || !strings.Contains(err.Error(), "set together") {
			t.Fatalf("%s alone: expected a configuration error, got %v", env[0], err)
		}
	}
}

func TestServeAppliesDefaultTimeouts(t *testing.T) {
	t.Setenv("SCOS_TLS_CERT_FILE", "")
	t.Setenv("SCOS_TLS_KEY_FILE", "")
	t.Setenv("SCOS_TLS_REQUIRED", "")
	srv := &http.Server{Addr: "127.0.0.1:0", Handler: http.NotFoundHandler()}
	done := make(chan error, 1)
	go func() { done <- Serve(srv) }()
	time.Sleep(50 * time.Millisecond)
	_ = srv.Close()
	<-done
	if srv.ReadHeaderTimeout != DefaultReadHeaderTimeout || srv.IdleTimeout != DefaultIdleTimeout {
		t.Fatalf("defaults not applied: header=%v idle=%v", srv.ReadHeaderTimeout, srv.IdleTimeout)
	}
}
