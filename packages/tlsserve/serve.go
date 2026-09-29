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
)

// Server timeouts applied when the caller leaves them unset. There is no
// default WriteTimeout: it would cut off streaming responses (SSE, chunked
// turn output), so handlers bound their own work with request contexts.
const (
	DefaultReadHeaderTimeout = 10 * time.Second
	DefaultIdleTimeout       = 120 * time.Second
)

// ListenAndServe starts an HTTPS listener when SCOS_TLS_CERT_FILE and
// SCOS_TLS_KEY_FILE are both set, and plaintext HTTP when neither is. Setting
// only one is a configuration error, and SCOS_TLS_REQUIRED=true with no
// certificate fails closed, so a typo can never silently downgrade to
// plaintext.
func ListenAndServe(addr string, handler http.Handler) error {
	return Serve(&http.Server{Addr: addr, Handler: handler})
}

// Serve runs a preconfigured *http.Server with the same TLS selection as
// ListenAndServe, filling in DefaultReadHeaderTimeout and DefaultIdleTimeout
// when the caller has not set them (a server without a header timeout can be
// held open indefinitely by slow clients).
func Serve(server *http.Server) error {
	certFile, keyFile, err := tlsFiles()
	if err != nil {
		return err
	}
	if server.ReadHeaderTimeout == 0 {
		server.ReadHeaderTimeout = DefaultReadHeaderTimeout
	}
	if server.IdleTimeout == 0 {
		server.IdleTimeout = DefaultIdleTimeout
	}
	if certFile != "" {
		return server.ListenAndServeTLS(certFile, keyFile)
	}
	return server.ListenAndServe()
}

// tlsFiles returns the configured certificate and key paths, both empty for
// plaintext, or an error for a partial or required-but-missing configuration.
func tlsFiles() (string, string, error) {
	certFile := os.Getenv("SCOS_TLS_CERT_FILE")
	keyFile := os.Getenv("SCOS_TLS_KEY_FILE")
	switch {
	case certFile != "" && keyFile != "":
		return certFile, keyFile, nil
	case certFile != "" || keyFile != "":
		return "", "", fmt.Errorf("SCOS_TLS_CERT_FILE and SCOS_TLS_KEY_FILE must be set together")
	case os.Getenv("SCOS_TLS_REQUIRED") == "true":
		return "", "", fmt.Errorf("SCOS_TLS_CERT_FILE and SCOS_TLS_KEY_FILE are required when SCOS_TLS_REQUIRED=true")
	}
	return "", "", nil
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
