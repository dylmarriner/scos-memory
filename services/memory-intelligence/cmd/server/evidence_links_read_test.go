package main

import (
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
