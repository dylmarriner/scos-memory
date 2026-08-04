// Package tlsserve provides the shared TLS listener and internal HTTP client
// used by every SCOS service, per GOVERNANCE.md §27 ("Production SHALL use
// TLS"). It intentionally still allows plaintext for local development so
// tests and single-binary runs are not forced to manage certificates, but a
// governed deployment sets SCOS_TLS_REQUIRED=true to fail closed instead.
package tlsserve

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"time"
)

// ListenAndServe starts an HTTPS listener when SCOS_TLS_CERT_FILE and
// SCOS_TLS_KEY_FILE are both set, and plaintext HTTP otherwise. When
// SCOS_TLS_REQUIRED=true and no certificate is configured, it fails closed
// rather than silently downgrading to plaintext.
func ListenAndServe(addr string, handler http.Handler) error {
	certFile := os.Getenv("SCOS_TLS_CERT_FILE")
	keyFile := os.Getenv("SCOS_TLS_KEY_FILE")
	if certFile != "" && keyFile != "" {
		return http.ListenAndServeTLS(addr, certFile, keyFile, handler)
	}
	if os.Getenv("SCOS_TLS_REQUIRED") == "true" {
		return fmt.Errorf("SCOS_TLS_CERT_FILE and SCOS_TLS_KEY_FILE are required when SCOS_TLS_REQUIRED=true")
	}
	return http.ListenAndServe(addr, handler)
}

// Serve runs a preconfigured *http.Server, using TLS when SCOS_TLS_CERT_FILE
// and SCOS_TLS_KEY_FILE are both set and plaintext otherwise, mirroring
// ListenAndServe for callers that need custom server timeouts.
func Serve(server *http.Server) error {
	certFile := os.Getenv("SCOS_TLS_CERT_FILE")
	keyFile := os.Getenv("SCOS_TLS_KEY_FILE")
	if certFile != "" && keyFile != "" {
		return server.ListenAndServeTLS(certFile, keyFile)
	}
	if os.Getenv("SCOS_TLS_REQUIRED") == "true" {
		return fmt.Errorf("SCOS_TLS_CERT_FILE and SCOS_TLS_KEY_FILE are required when SCOS_TLS_REQUIRED=true")
	}
	return server.ListenAndServe()
}

// TrustedClient returns an *http.Client for calling other internal SCOS
// services. When SCOS_TLS_CA_FILE names a PEM file, it is used as the sole
// trust anchor so internal calls verify the shared deployment certificate
// rather than either trusting the public CA pool or skipping verification.
// With no CA file configured, it returns a client using the system trust
// store, matching an environment with no internal TLS deployed yet.
func TrustedClient(timeout time.Duration) (*http.Client, error) {
	caFile := os.Getenv("SCOS_TLS_CA_FILE")
	if caFile == "" {
		return &http.Client{Timeout: timeout}, nil
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read SCOS_TLS_CA_FILE: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("SCOS_TLS_CA_FILE does not contain a valid PEM certificate")
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		},
	}, nil
}
