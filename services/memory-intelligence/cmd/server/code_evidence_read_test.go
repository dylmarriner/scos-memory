package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestP6CodeEvidenceReadRejectsInvalidSelectors(t *testing.T) {
	s := &engineServer{}
	symbol := "4cbe32c6-891c-4155-a9ec-6aa39e66704a"
	for _, path := range []string{
		"/v1/evidence-links/by-code",
		"/v1/evidence-links/by-code?code_symbol_version_id=invalid",
		"/v1/evidence-links/by-code?code_symbol_version_id=" + symbol + "&limit=0",
		"/v1/evidence-links/by-code?code_symbol_version_id=" + symbol + "&limit=101",
		"/v1/evidence-links/by-code?code_symbol_version_id=" + symbol + "&limit=NaN",
		"/v1/evidence-links/by-code?code_symbol_version_id=" + symbol + "&after=invalid",
		"/v1/evidence-links/by-code?code_symbol_version_id=" + symbol + "&code_symbol_version_id=" + symbol,
		"/v1/evidence-links/by-code?code_symbol_version_id=" + symbol + "&unknown=yes",
	} {
		w := httptest.NewRecorder()
		s.handleCodeEvidenceLinksRead(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("GET %s returned %d, want 400: %s", path, w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	s.handleCodeEvidenceLinksRead(w, httptest.NewRequest(http.MethodPost,
		"/v1/evidence-links/by-code?code_symbol_version_id="+symbol, nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST returned %d", w.Code)
	}
}
