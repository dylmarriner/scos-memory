package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

// A historical code request must be exactly one JSON document. Accepting a
// second value silently makes the wire request ambiguous for callers, logs,
// proxies and audit systems even when the first object is valid.
func TestPGHistoricalQueryAndImpactRejectTrailingJSON(t *testing.T) {
	store, db := testPGStore(t)
	tenant := testTenant(t, db)
	repo, err := store.EnsureRepository(t.Context(), tenant, "/single-json/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	srv := &server{pgStore: store}
	for _, path := range []string{"/v2/code/query", "/v2/code/impact"} {
		t.Run(path, func(t *testing.T) {
			base := fmt.Sprintf(`{"repository_id":%q,"branch":"main","query":"pkg.fn","as_of_commit":"missing"}`, repo)
			request := func(raw string) *httptest.ResponseRecorder {
				t.Helper()
				r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(raw))
				r = r.WithContext(auth.ContextWithClaims(r.Context(), auth.Claims{TenantID: tenant, PrincipalID: "p"}))
				w := httptest.NewRecorder()
				if path == "/v2/code/query" {
					srv.handlePGQuery(w, r)
				} else {
					srv.handlePGImpact(w, r)
				}
				return w
			}
			for _, suffix := range []string{"\n{}", "\ntrue", "\nnull", "\ninvalid"} {
				got := request(base + suffix)
				if got.Code != http.StatusBadRequest {
					t.Errorf("suffix %q: got HTTP %d, want 400; body=%s", suffix, got.Code, got.Body.String())
				}
			}
			got := request(base + "\n \t")
			if got.Code != http.StatusNotFound {
				t.Errorf("trailing whitespace should be valid JSON; got HTTP %d, want 404; body=%s", got.Code, got.Body.String())
			}
		})
	}
}
