package main

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/auth"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestP7RefreshRequiresAuthenticationAndForbidsCaching(t *testing.T) {
	srv := &engineServer{}
	path := "/v1/standing-answers/refresh"
	req := httptest.NewRequest(http.MethodPost, path,
		strings.NewReader(`{"fact_id":"4cbe32c6-891c-4155-a9ec-6aa39e66704a"}`))
	w := httptest.NewRecorder()
	srv.handleStandingAnswerRefresh(w, req)
	if w.Code != http.StatusUnauthorized || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("unauthenticated refresh status=%d headers=%v", w.Code, w.Header())
	}
	w = httptest.NewRecorder()
	srv.handleStandingAnswerRefresh(w, httptest.NewRequest(http.MethodGet, path, nil))
	if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("GET unexpectedly accepted: %d %v", w.Code, w.Header())
	}
}

func TestP7RefreshDoesNotExposeDatabaseErrors(t *testing.T) {
	db, err := sql.Open("pgx", "postgresql://secret_user:secret_pass@secret_host:7777/secret_db?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	srv := &engineServer{db: db}
	req := httptest.NewRequest(http.MethodPost, "/v1/standing-answers/refresh",
		strings.NewReader(`{"fact_id":"4cbe32c6-891c-4155-a9ec-6aa39e66704a"}`))
	req = req.WithContext(auth.ContextWithClaims(req.Context(), auth.Claims{
		TenantID:    "4cbe32c6-891c-4155-a9ec-6aa39e66704a",
		PrincipalID: "5cbe32c6-891c-4155-a9ec-6aa39e66704a",
	}))
	w := httptest.NewRecorder()
	srv.handleStandingAnswerRefresh(w, req)
	if w.Code != http.StatusInternalServerError || w.Body.String() != "standing answer refresh failed\n" {
		t.Fatalf("driver details exposed: status=%d body=%q", w.Code, w.Body.String())
	}
}
