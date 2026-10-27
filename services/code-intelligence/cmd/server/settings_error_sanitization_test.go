package main

import (
	"database/sql"
	"net/http"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestPGSettingsHTTPDoesNotLeakDatabaseErrors(t *testing.T) {
	db, err := sql.Open("pgx", "postgres://unused:unused@localhost/unused?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	srv := &server{pgStore: newPGCodeStore(db)}
	tenant := "00000000-0000-4000-8000-000000000001"
	for _, tc := range []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodGet, "/v2/code/settings", nil},
		{http.MethodPut, "/v2/code/settings", map[string]any{"durable_store_enabled": false}},
		{http.MethodPost, "/v2/code/repos", map[string]any{"canonical_path": "/test"}},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			w := pgRequest(t, srv, tc.method, tc.path, tenant, tc.body)
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "database is closed") || strings.Contains(w.Body.String(), "sql:") {
				t.Fatalf("internal database error leaked: %q", w.Body.String())
			}
		})
	}
}
