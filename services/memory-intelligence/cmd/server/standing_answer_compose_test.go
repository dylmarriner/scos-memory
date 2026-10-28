package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

func TestP7ComposeRejectsMalformedAndAmbiguousEvidenceBeforeDatabase(t *testing.T) {
	const ns = "4cbe32c6-891c-4155-a9ec-6aa39e66704a"
	const a = "5cbe32c6-891c-4155-a9ec-6aa39e66704a"
	const b = "6cbe32c6-891c-4155-a9ec-6aa39e66704a"
	cases := []string{
		`{}`,
		`{"namespace_id":"` + ns + `","fact_ids":[]}`,
		`{"namespace_id":"` + ns + `","fact_ids":["` + a + `"]}`,
		`{"namespace_id":"` + ns + `","fact_ids":["` + a + `","` + a + `"]}`,
		`{"namespace_id":"` + ns + `","fact_ids":["` + a + `","not-uuid"]}`,
		`{"namespace_id":"` + ns + `","fact_ids":["` + a + `","` + b + `"],"draft_text":"forged"}`,
		`{"namespace_id":"` + ns + `","namespace_id":"` + ns + `","fact_ids":["` + a + `","` + b + `"]}`,
		`{"namespace_id":"` + ns + `","fact_ids":["` + a + `","` + b + `"]} {}`,
	}
	for _, body := range cases {
		t.Run(body, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/standing-answers/compose", strings.NewReader(body))
			(&engineServer{}).handleStandingAnswerCompose(rec, req)
			if rec.Code != http.StatusBadRequest || rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("status=%d headers=%v body=%s", rec.Code, rec.Header(), rec.Body.String())
			}
		})
	}
}

func TestP7ComposeRequiresAuthenticationAndFailsClosedWithoutDatabase(t *testing.T) {
	body := `{"namespace_id":"4cbe32c6-891c-4155-a9ec-6aa39e66704a","fact_ids":["5cbe32c6-891c-4155-a9ec-6aa39e66704a","6cbe32c6-891c-4155-a9ec-6aa39e66704a"]}`
	srv := &engineServer{}
	req := httptest.NewRequest(http.MethodPost, "/v1/standing-answers/compose", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.handleStandingAnswerCompose(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", rec.Code)
	}
	req = httptest.NewRequest(http.MethodPost, "/v1/standing-answers/compose", strings.NewReader(body))
	req = req.WithContext(auth.ContextWithClaims(req.Context(), auth.Claims{
		TenantID:    "4cbe32c6-891c-4155-a9ec-6aa39e66704a",
		PrincipalID: "7cbe32c6-891c-4155-a9ec-6aa39e66704a",
	}))
	rec = httptest.NewRecorder()
	srv.handleStandingAnswerCompose(rec, req)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("no DB status=%d headers=%v", rec.Code, rec.Header())
	}
}
