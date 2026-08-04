package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The outbox endpoints are the operational surface §14 requires for poison
// work. These tests cover the behaviour that does not need a live database:
// route registration and the not-ready guard. Their query behaviour against
// real rows is exercised by the integration suite, which has a database.

func TestOutboxRoutesAreRegistered(t *testing.T) {
	s := server{}
	routes := s.routes()
	for _, path := range []string{"/v1/outbox/status", "/v1/outbox/dead-letters"} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("X-SCOS-Tenant-ID", "tenant-1")
		r.Header.Set("X-SCOS-Principal-ID", "principal-1")
		w := httptest.NewRecorder()
		routes.ServeHTTP(w, r)
		// Without a database the handler reports not-ready rather than 404,
		// which proves the route exists and reached its handler.
		if w.Code == http.StatusNotFound {
			t.Errorf("%s is not registered", path)
		}
	}
}

func TestOutboxHandlersGuardOnReadiness(t *testing.T) {
	s := server{db: nil}
	for name, handler := range map[string]http.HandlerFunc{
		"status":       s.handleOutboxStatus,
		"dead-letters": s.handleDeadLetters,
	} {
		r := httptest.NewRequest(http.MethodGet, "/v1/outbox/"+name, nil)
		w := httptest.NewRecorder()
		handler(w, r)
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s returned %d with no database, expected 503", name, w.Code)
		}
	}
}

func TestDeadLetterLimitIsBounded(t *testing.T) {
	// The dead-letter listing reuses the turn limit parser, so an unbounded or
	// negative limit cannot reach the query.
	if _, err := parseTurnLimit("-1"); err == nil {
		t.Error("negative limit accepted")
	}
	limit, err := parseTurnLimit("100000")
	if err != nil {
		t.Fatalf("large limit errored: %v", err)
	}
	if limit > 100 {
		t.Errorf("limit was not capped, got %d", limit)
	}
}
