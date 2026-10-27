package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dylmarriner/scos-memory/packages/auth"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A real pgx connection failure must not disclose the internal DSN to an
// authenticated caller. This exercises the namespace lookup, not a stub DB.
func TestP5NamespaceLookupFailureSanitizesDatabaseError(t *testing.T) {
	cfg, err := pgxpool.ParseConfig("postgres://private_retrieval_operator:secret@127.0.0.1:1/private_memory?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.ConnectTimeout = 500 * time.Millisecond
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	handler := auth.AuthenticatedHandler(newRetrievalMux(&server{db: pool}), auth.Config{
		Token: "test-service-token", Identity: "internal-service",
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/retrieve", strings.NewReader(`{"tenant_id":"tenant-a","principal_id":"principal-a","query":"internal records"}`))
	request.Header.Set("Authorization", "Bearer test-service-token")
	request.Header.Set("X-SCOS-Tenant-ID", "tenant-a")
	request.Header.Set("X-SCOS-Principal-ID", "principal-a")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, request)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("unexpected status: %d, body %q", rec.Code, rec.Body.String())
	}
	for _, secret := range []string{"private_retrieval_operator", "private_memory", "127.0.0.1", "connection refused", "failed to connect", "dial tcp"} {
		if strings.Contains(strings.ToLower(rec.Body.String()), strings.ToLower(secret)) {
			t.Errorf("response disclosed internal database detail %q: %s", secret, rec.Body.String())
		}
	}
	if !strings.Contains(rec.Body.String(), "retrieval temporarily unavailable") {
		t.Errorf("response did not give a safe, actionable error: %q", rec.Body.String())
	}
}
