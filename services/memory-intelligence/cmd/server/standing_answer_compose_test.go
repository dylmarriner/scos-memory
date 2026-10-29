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
		FactID:           "5cbe32c6-891c-4155-a9ec-6aa39e66704a",
		OwnerPrincipalID: "9cbe32c6-891c-4155-a9ec-6aa39e66704a",
		Summary:          "A verified fact.",
		Confidence:       0.75,
		Sensitivity:      "internal",
	}
	first := standingAnswerComposeFingerprint(ev, "7cbe32c6-891c-4155-a9ec-6aa39e66704a", "4cbe32c6-891c-4155-a9ec-6aa39e66704a")
	if first != "5129cb313ba1c2d52f0d230bf7def5ed603e54c43a239e16ef3125064369a0e9" || first != standingAnswerComposeFingerprint(ev, "7cbe32c6-891c-4155-a9ec-6aa39e66704a", "4cbe32c6-891c-4155-a9ec-6aa39e66704a") {
		t.Fatalf("fingerprint not stable SHA-256: %q", first)
	}
	ev.Summary = "A revised fact."
	if got := standingAnswerComposeFingerprint(ev, "7cbe32c6-891c-4155-a9ec-6aa39e66704a", "4cbe32c6-891c-4155-a9ec-6aa39e66704a"); got == first {
		t.Fatal("source summary changed without fingerprint changing")
	}
	ev.Summary = "A verified fact."
	ev.OwnerPrincipalID = "acbe32c6-891c-4155-a9ec-6aa39e66704a"
	if got := standingAnswerComposeFingerprint(ev, "7cbe32c6-891c-4155-a9ec-6aa39e66704a", "4cbe32c6-891c-4155-a9ec-6aa39e66704a"); got == first {
		t.Fatal("source owner changed without fingerprint changing")
	}
	ev.OwnerPrincipalID = "9cbe32c6-891c-4155-a9ec-6aa39e66704a"
	ev.Confidence = 0.76
	if got := standingAnswerComposeFingerprint(ev, "7cbe32c6-891c-4155-a9ec-6aa39e66704a", "4cbe32c6-891c-4155-a9ec-6aa39e66704a"); got == first {
		t.Fatal("confidence changed without fingerprint changing")
	}
	ev.Confidence = 0.75
	ev.VersionID = "8cbe32c6-891c-4155-a9ec-6aa39e66704a"
	if got := standingAnswerComposeFingerprint(ev, "7cbe32c6-891c-4155-a9ec-6aa39e66704a", "4cbe32c6-891c-4155-a9ec-6aa39e66704a"); got == first {
		t.Fatal("source version changed without fingerprint changing")
	}
}

// A draft reviewed in one tenant/namespace must not retain the same source
// fingerprint after its evidence is moved to another security boundary.
func TestP7ComposeFingerprintBindsTenantAndNamespace(t *testing.T) {
	ev := standingAnswerComposeEvidence{
		FactID:     "5cbe32c6-891c-4155-a9ec-6aa39e66704a",
		Summary:    "Same source bytes",
		Confidence: 0.8,
	}
	tenantA := "7cbe32c6-891c-4155-a9ec-6aa39e66704a"
	tenantB := "8cbe32c6-891c-4155-a9ec-6aa39e66704a"
	namespaceA := "4cbe32c6-891c-4155-a9ec-6aa39e66704a"
	namespaceB := "9cbe32c6-891c-4155-a9ec-6aa39e66704a"
	original := standingAnswerComposeFingerprint(ev, tenantA, namespaceA)
	if original == standingAnswerComposeFingerprint(ev, tenantB, namespaceA) {
		t.Fatal("fingerprint is not tenant-bound")
	}
	if original == standingAnswerComposeFingerprint(ev, tenantA, namespaceB) {
		t.Fatal("fingerprint is not namespace-bound")
	}
	if original != standingAnswerComposeFingerprint(ev, tenantA, namespaceA) {
		t.Fatal("fingerprint is not deterministic")
	}
}
