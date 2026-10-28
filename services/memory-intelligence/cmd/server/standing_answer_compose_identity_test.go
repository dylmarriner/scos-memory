package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

func TestP7ComposeRejectsInvalidAuthenticatedUUIDClaimsBeforeDatabase(t *testing.T) {
	body := `{"namespace_id":"4cbe32c6-891c-4155-a9ec-6aa39e66704a","fact_ids":["5cbe32c6-891c-4155-a9ec-6aa39e66704a","6cbe32c6-891c-4155-a9ec-6aa39e66704a"]}`
	validTenant := "7cbe32c6-891c-4155-a9ec-6aa39e66704a"
	validPrincipal := "8cbe32c6-891c-4155-a9ec-6aa39e66704a"
	cases := []struct {
		name, tenant, principal string
	}{
		{"malformed tenant", "not-a-uuid", validPrincipal},
		{"zero tenant", "00000000-0000-0000-0000-000000000000", validPrincipal},
		{"malformed principal", validTenant, "not-a-uuid"},
		{"zero principal", validTenant, "00000000-0000-0000-0000-000000000000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/standing-answers/compose", strings.NewReader(body))
			req = req.WithContext(auth.ContextWithClaims(req.Context(), auth.Claims{
				TenantID: tc.tenant, PrincipalID: tc.principal,
			}))
			rec := httptest.NewRecorder()
			(&engineServer{}).handleStandingAnswerCompose(rec, req)
			if rec.Code != http.StatusUnauthorized || rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("invalid identity status=%d headers=%v body=%s", rec.Code, rec.Header(), rec.Body.String())
			}
		})
	}
}
