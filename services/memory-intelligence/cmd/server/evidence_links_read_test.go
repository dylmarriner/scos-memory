package main

import (
	"database/sql"
	"github.com/dylmarriner/scos-memory/packages/auth"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestP6EvidenceLinkReadbackRejectsInvalidSelectors(t *testing.T) {
	s := &engineServer{}
	fact := "4cbe32c6-891c-4155-a9ec-6aa39e66704a"
	cases := []string{
		"/v1/evidence-links",
		"/v1/evidence-links?fact_id=not-a-uuid",
		"/v1/evidence-links?fact_id=" + fact + "&limit=0",
		"/v1/evidence-links?fact_id=" + fact + "&limit=-1",
		"/v1/evidence-links?fact_id=" + fact + "&limit=101",
		"/v1/evidence-links?fact_id=" + fact + "&limit=abc",
		"/v1/evidence-links?fact_id=" + fact + "&after=not-a-uuid",
		"/v1/evidence-links?fact_id=" + fact + "&fact_id=" + fact,
		"/v1/evidence-links?fact_id=" + fact + "&unknown=ignored",
	}
	for _, path := range cases {
		t.Run(path, func(t *testing.T) {
			w := httptest.NewRecorder()
			s.handleEvidenceLinks(w, httptest.NewRequest(http.MethodGet, path, nil))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("GET %s = %d (%s), want 400", path, w.Code, strings.TrimSpace(w.Body.String()))
			}
		})
	}
}

func TestP6EvidenceLinkReadbackDoesNotLeakDatabaseErrors(t *testing.T) {
	db, err := sql.Open("pgx", "postgresql://unused:unused@127.0.0.1:1/unused?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	srv := &engineServer{db: db}
	req := httptest.NewRequest(http.MethodGet, "/v1/evidence-links?fact_id=4cbe32c6-891c-4155-a9ec-6aa39e66704a", nil)
	req = req.WithContext(auth.ContextWithClaims(req.Context(), auth.Claims{
		TenantID:    "4cbe32c6-891c-4155-a9ec-6aa39e66704a",
		PrincipalID: "5cbe32c6-891c-4155-a9ec-6aa39e66704a",
	}))
	w := httptest.NewRecorder()
	srv.handleEvidenceLinks(w, req)
	if w.Code != http.StatusInternalServerError || w.Body.String() != "internal evidence-link read failure\n" {
		t.Fatalf("internal DB error exposed: %d %q", w.Code, w.Body.String())
	}
}
