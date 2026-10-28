package main

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

func TestP7StandingAnswerReadRejectsInvalidSelectorsBeforeDatabase(t *testing.T) {
	srv := &engineServer{}
	ns := "4cbe32c6-891c-4155-a9ec-6aa39e66704a"
	cases := []string{
		"/v1/standing-answers",
		"/v1/standing-answers?namespace_id=no&question_key=hello",
		"/v1/standing-answers?namespace_id=" + ns,
		"/v1/standing-answers?namespace_id=" + ns + "&question_key=%20%20",
		"/v1/standing-answers?namespace_id=" + ns + "&question_key=hello&question_key=world",
		"/v1/standing-answers?namespace_id=" + ns + "&question_key=hello&unknown=1",
		"/v1/standing-answers?namespace_id=" + ns + "&namespace_id=" + ns + "&question_key=hello",
		"/v1/standing-answers?namespace_id=" + ns + "&question_key=" + strings.Repeat("a", 1025),
	}
	for _, path := range cases {
		t.Run(path[:min(len(path), 100)], func(t *testing.T) {
			w := httptest.NewRecorder()
			srv.handleStandingAnswerRead(w, httptest.NewRequest(http.MethodGet, path, nil))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
			}
		})
	}
	w := httptest.NewRecorder()
	srv.handleStandingAnswerRead(w, httptest.NewRequest(http.MethodPost, "/v1/standing-answers?namespace_id="+ns+"&question_key=hello", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status=%d", w.Code)
	}
}

func TestP7StandingAnswerReadRequiresIdentity(t *testing.T) {
	srv := &engineServer{}
	path := "/v1/standing-answers?namespace_id=4cbe32c6-891c-4155-a9ec-6aa39e66704a&question_key=hello"
	w := httptest.NewRecorder()
	srv.handleStandingAnswerRead(w, httptest.NewRequest(http.MethodGet, path, nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status=%d", w.Code)
	}
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req = req.WithContext(auth.ContextWithClaims(req.Context(), auth.Claims{TenantID: "", PrincipalID: ""}))
	w = httptest.NewRecorder()
	srv.handleStandingAnswerRead(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("missing claims status=%d", w.Code)
	}
}

func TestP7StandingAnswerReadDoesNotExposeDatabaseErrors(t *testing.T) {
	db, err := sql.Open("pgx", "postgresql://unused:unused@127.0.0.1:1/unused?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	srv := &engineServer{db: db}
	req := httptest.NewRequest(http.MethodGet, "/v1/standing-answers?namespace_id=4cbe32c6-891c-4155-a9ec-6aa39e66704a&question_key=hello", nil)
	req = req.WithContext(auth.ContextWithClaims(req.Context(), auth.Claims{
		TenantID:    "4cbe32c6-891c-4155-a9ec-6aa39e66704a",
		PrincipalID: "5cbe32c6-891c-4155-a9ec-6aa39e66704a",
	}))
	w := httptest.NewRecorder()
	srv.handleStandingAnswerRead(w, req)
	if w.Code != http.StatusInternalServerError || w.Body.String() != "internal standing-answer read failure\n" {
		t.Fatalf("database details exposed: status=%d body=%q", w.Code, w.Body.String())
	}
}

// A browser or shared intermediary must not reuse an answer after its fact
// expires or its reader loses namespace membership.
func TestP7StandingAnswerReadForbidsHTTPCaching(t *testing.T) {
	srv := &engineServer{}
	req := httptest.NewRequest(http.MethodGet,
		"/v1/standing-answers?namespace_id=4cbe32c6-891c-4155-a9ec-6aa39e66704a&question_key=hello", nil)
	w := httptest.NewRecorder()
	srv.handleStandingAnswerRead(w, req)
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("cache control=%q, want no-store", got)
	}
}
