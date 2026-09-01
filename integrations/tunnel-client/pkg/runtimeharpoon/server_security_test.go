package runtimeharpoon

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/openai/tunnel-client/pkg/runtimeconfig"
)

func TestRuntimeHarpoonBlocksUnallowlistedRedirectBeforeDial(t *testing.T) {
	t.Parallel()

	var blockedRequests atomic.Int32
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		blockedRequests.Add(1)
		_, _ = w.Write([]byte("escaped"))
	}))
	t.Cleanup(blocked.Close)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, blocked.URL+"/escape", http.StatusFound)
	}))
	t.Cleanup(redirector.Close)

	server := newRuntimeHarpoonSecurityServer(t, []Target{{
		Label:   "primary",
		BaseURL: runtimeHarpoonSecurityURL(t, redirector.URL),
	}})
	_, err := server.callTarget(context.Background(), callTargetRequest{
		Label:  "primary",
		Method: http.MethodGet,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "redirect blocked")
	require.NotContains(t, err.Error(), blocked.URL)
	require.Zero(t, blockedRequests.Load(), "blocked redirect target must never receive a request")
}

func TestRuntimeHarpoonEnforcesRedirectLimit(t *testing.T) {
	t.Parallel()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.Path, http.StatusFound)
	}))
	t.Cleanup(redirector.Close)

	server := newRuntimeHarpoonSecurityServer(t, []Target{{
		Label:   "loop",
		BaseURL: runtimeHarpoonSecurityURL(t, redirector.URL+"/"),
	}})
	maxRedirects := 1
	_, err := server.callTarget(context.Background(), callTargetRequest{
		Label:        "loop",
		Method:       http.MethodGet,
		MaxRedirects: &maxRedirects,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "redirect limit exceeded")
}

func TestRuntimeHarpoonBlocksMethodOverrideHeaders(t *testing.T) {
	t.Parallel()

	overrideHeaders := []string{
		"X-HTTP-Method",
		"X-HTTP-Method-Override",
		"X-Method-Override",
	}
	type receivedRequest struct {
		headers         http.Header
		effectiveMethod string
	}
	received := make(chan receivedRequest, 1)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		effectiveMethod := r.Method
		for _, header := range overrideHeaders {
			if method := r.Header.Get(header); method != "" {
				effectiveMethod = method
			}
		}
		received <- receivedRequest{
			headers:         r.Header.Clone(),
			effectiveMethod: effectiveMethod,
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(target.Close)

	server := newRuntimeHarpoonSecurityServer(t, []Target{{
		Label:   "method-override",
		BaseURL: runtimeHarpoonSecurityURL(t, target.URL),
	}})
	requestHeaders := map[string]string{
		"Accept": "application/json",
	}
	for _, header := range overrideHeaders {
		requestHeaders[header] = http.MethodDelete
	}

	_, err := server.callTarget(context.Background(), callTargetRequest{
		Label:   "method-override",
		Method:  http.MethodPost,
		Headers: requestHeaders,
	})
	require.NoError(t, err)

	request := <-received
	require.Equal(t, http.MethodPost, request.effectiveMethod)
	require.Equal(t, "application/json", request.headers.Get("Accept"))
	for _, header := range overrideHeaders {
		require.Empty(t, request.headers.Get(header))
	}
}

func newRuntimeHarpoonSecurityServer(t *testing.T, targets []Target) *Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry, err := NewRegistry(logger, true, targets)
	require.NoError(t, err)
	server, err := NewServer(&runtimeconfig.HarpoonConfig{
		AllowPlaintextHTTP: true,
		MaxResponseBytes:   1024,
		MaxRedirects:       5,
	}, registry, logger)
	require.NoError(t, err)
	return server
}

func runtimeHarpoonSecurityURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	require.NoError(t, err)
	return parsed
}
