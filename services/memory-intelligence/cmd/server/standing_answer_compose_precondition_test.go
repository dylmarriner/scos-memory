package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

func TestP7ComposeRejectsInvalidExpectedDraftFingerprintBeforeDatabase(t *testing.T) {
	for _, fingerprint := range []string{"short", strings.Repeat("G", 64), strings.Repeat("A", 64), strings.Repeat("0", 65), ""} {
		t.Run(fingerprint, func(t *testing.T) {
			body := `{"namespace_id":"4cbe32c6-891c-4155-a9ec-6aa39e66704a","fact_ids":["5cbe32c6-891c-4155-a9ec-6aa39e66704a","6cbe32c6-891c-4155-a9ec-6aa39e66704a"],"expected_draft_fingerprint":"` + fingerprint + `"}`
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/standing-answers/compose", strings.NewReader(body))
			(&engineServer{}).handleStandingAnswerCompose(rec, req)
			if rec.Code != http.StatusBadRequest || rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("invalid expected fingerprint accepted: status=%d body=%q", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestP7ComposeAcceptsWellFormedExpectedFingerprintBeforeDatabase(t *testing.T) {
	body := `{"namespace_id":"4cbe32c6-891c-4155-a9ec-6aa39e66704a","fact_ids":["5cbe32c6-891c-4155-a9ec-6aa39e66704a","6cbe32c6-891c-4155-a9ec-6aa39e66704a"],"expected_draft_fingerprint":"` + strings.Repeat("0", 64) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/standing-answers/compose", strings.NewReader(body))
	req = req.WithContext(auth.ContextWithClaims(req.Context(), auth.Claims{TenantID: "4cbe32c6-891c-4155-a9ec-6aa39e66704a", PrincipalID: "7cbe32c6-891c-4155-a9ec-6aa39e66704a"}))
	rec := httptest.NewRecorder()
	(&engineServer{}).handleStandingAnswerCompose(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("valid expected fingerprint rejected before database: status=%d body=%s", rec.Code, rec.Body.String())
	}
}
