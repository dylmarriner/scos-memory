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

// P7 review evidence must be pinned to the exact source bytes and version,
// not merely its fact UUID. A later approval workflow can re-read the source
// and compare this digest before accepting a draft.
func TestP7ComposeEvidenceFingerprintTracksVersionAndContent(t *testing.T) {
	ev := standingAnswerComposeEvidence{
		FactID:     "5cbe32c6-891c-4155-a9ec-6aa39e66704a",
		Summary:    "A verified fact.",
		Confidence: 0.75,
	}
	first := standingAnswerComposeFingerprint(ev)
	if first != "d9e601342ffa2f0fe1b155e14ae26cd69f6be5cf0b4ff73a332c081242d0e85f" || first != standingAnswerComposeFingerprint(ev) {
		t.Fatalf("fingerprint not stable SHA-256: %q", first)
	}
	ev.Summary = "A revised fact."
	if got := standingAnswerComposeFingerprint(ev); got == first {
		t.Fatal("source summary changed without fingerprint changing")
	}
	ev.Summary = "A verified fact."
	ev.Confidence = 0.76
	if got := standingAnswerComposeFingerprint(ev); got == first {
		t.Fatal("confidence changed without fingerprint changing")
	}
	ev.Confidence = 0.75
	ev.VersionID = "8cbe32c6-891c-4155-a9ec-6aa39e66704a"
	if got := standingAnswerComposeFingerprint(ev); got == first {
		t.Fatal("source version changed without fingerprint changing")
	}
}
