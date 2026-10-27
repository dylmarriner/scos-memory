package main

import (
	"bytes"
	"context"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dylmarriner/scos-memory/packages/auth"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The downstream service is real HTTP, not a fake client. Its error body may
// contain internal connection details and must not become a log message.
func TestP5CodeQueryFailureDoesNotPropagateDownstreamBody(t *testing.T) {
	const secret = "postgres://private_code_operator:secret@internal-db:5432/private_code"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "backend failed: "+secret, http.StatusServiceUnavailable)
	}))
	defer upstream.Close()
	client := &codeIntelligenceClient{baseURL: upstream.URL, httpClient: upstream.Client()}
	_, err := client.Query(context.Background(), "tenant", "principal", "repo", "main", "find code", 10)
	if err == nil {
		t.Fatal("expected downstream error")
	}
	if got := err.Error(); got != "code-intelligence returned HTTP 503" {
		t.Fatalf("unexpected status-only downstream error: %v", err)
	}
}

func TestP5CodeDegradationDoesNotLogDownstreamBody(t *testing.T) {
	const secret = "postgres://private_code_operator:secret@internal-db:5432/private_code"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "backend failed: "+secret, http.StatusServiceUnavailable)
	}))
	defer upstream.Close()
	cfg, err := pgxpool.ParseConfig("postgres://private_retrieval_operator:secret@127.0.0.1:1/private_memory?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.ConnectTimeout = 500 * time.Millisecond
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	srv := &server{db: pool, codeIntelligence: &codeIntelligenceClient{baseURL: upstream.URL, httpClient: upstream.Client()}}
	handler := auth.AuthenticatedHandler(newRetrievalMux(srv), auth.Config{Token: "test-service-token", Identity: "internal-service"})
	req := httptest.NewRequest(http.MethodPost, "/v1/retrieve", strings.NewReader(`{"tenant_id":"tenant-a","principal_id":"principal-a","query":"find code function","repository_id":"repo-a","branch":"main"}`))
	req.Header.Set("Authorization", "Bearer test-service-token")
	req.Header.Set("X-SCOS-Tenant-ID", "tenant-a")
	req.Header.Set("X-SCOS-Principal-ID", "principal-a")

	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previous)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if !strings.Contains(logs.String(), "code-intelligence retrieval degraded") {
		t.Fatalf("missing degradation diagnostic: %q", logs.String())
	}
	if strings.Contains(logs.String(), secret) || strings.Contains(logs.String(), "private_code_operator") {
		t.Fatalf("downstream sensitive body leaked to logs: %s", logs.String())
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("downstream sensitive body leaked to response: %s", rec.Body.String())
	}

	// A transport error is a separate exposure path: url.Error embeds the
	// request URL, which may itself contain credentials. This catches a future
	// regression from type-only logging back to raw error logging even when
	// the upstream HTTP status error has already been sanitized.
	const transportSecret = "private_transport_operator"
	srv.codeIntelligence = &codeIntelligenceClient{
		baseURL:    "http://" + transportSecret + ":secret@127.0.0.1:1",
		httpClient: &http.Client{Timeout: 500 * time.Millisecond},
	}
	logs.Reset()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/retrieve", strings.NewReader(`{"tenant_id":"tenant-a","principal_id":"principal-a","query":"find code function","repository_id":"repo-a","branch":"main"}`))
	req2.Header.Set("Authorization", "Bearer test-service-token")
	req2.Header.Set("X-SCOS-Tenant-ID", "tenant-a")
	req2.Header.Set("X-SCOS-Principal-ID", "principal-a")
	handler.ServeHTTP(httptest.NewRecorder(), req2)
	if !strings.Contains(logs.String(), "code-intelligence retrieval degraded") {
		t.Fatalf("missing transport degradation diagnostic: %q", logs.String())
	}
	if strings.Contains(logs.String(), transportSecret) {
		t.Fatalf("transport credentials leaked to logs: %s", logs.String())
	}
}

// Bounded discard must preserve keep-alive for short error responses, without
// making any of the discarded text available to callers or logs.
func TestP5CodeQueryShortErrorReusesHTTPConnection(t *testing.T) {
	var connections atomic.Int64
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "private downstream detail", http.StatusServiceUnavailable)
	}))
	upstream.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	upstream.Start()
	defer upstream.Close()
	client := &codeIntelligenceClient{baseURL: upstream.URL, httpClient: upstream.Client()}
	for i := 0; i < 2; i++ {
		_, err := client.Query(context.Background(), "tenant", "principal", "repo", "main", "find code", 10)
		if err == nil || err.Error() != "code-intelligence returned HTTP 503" {
			t.Fatalf("query %d unexpected error: %v", i, err)
		}
	}
	if got := connections.Load(); got != 1 {
		t.Fatalf("short error body should reuse one HTTP connection, got %d", got)
	}
}
