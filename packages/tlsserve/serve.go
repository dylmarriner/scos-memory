// Package tlsserve provides the shared TLS listener and internal HTTP client
// used by every SCOS service, per GOVERNANCE.md §27 ("Production SHALL use
// TLS"). It intentionally still allows plaintext for local development so
// tests and single-binary runs are not forced to manage certificates, but a
// governed deployment sets SCOS_TLS_REQUIRED=true to fail closed instead.
package tlsserve

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dylmarriner/scos-memory/packages/telemetry"
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

// ServeGracefully runs Serve and shuts the server down cleanly on SIGINT or
// SIGTERM: it stops accepting new connections and gives in-flight requests
// shutdownTimeout to finish before returning. Callers that need clean
// container shutdown (draining a load balancer, finishing an in-flight
// journal write) should use this instead of Serve directly; Serve is kept
// unchanged for callers and tests that manage their own lifecycle.
//
// It returns nil on a clean signal-triggered shutdown, or the error from
// Serve/Shutdown otherwise. http.ErrServerClosed is treated as success.
func ServeGracefully(server *http.Server, shutdownTimeout time.Duration, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- Serve(server)
	}()

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		stop()
		logger.Info("shutdown signal received, draining in-flight requests", "timeout", shutdownTimeout)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("graceful shutdown did not complete cleanly", "error", err)
			return err
		}
		logger.Info("shutdown complete")
		return nil
	}
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
		return &http.Client{Timeout: timeout, Transport: telemetry.ProcessTransport(nil)}, nil
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
		Transport: telemetry.ProcessTransport(&http.Transport{
			Proxy:           http.ProxyFromEnvironment,
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		}),
	}, nil
}
