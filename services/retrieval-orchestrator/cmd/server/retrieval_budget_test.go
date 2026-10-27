package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

// Regression: untrusted request budgets must not multiply every retrieval lane's
// PostgreSQL candidate count or exhaust reranker/token allocation work.
func TestP4RetrievalBudgetRejectsUnboundedRequestsBeforeDatabase(t *testing.T) {
	handler := auth.AuthenticatedHandler(newRetrievalMux(&server{}), auth.Config{Token: "shared-secret", Identity: "internal-service"})
	for _, tc := range []struct {
		name, body, wantField string
	}{
		{"oversized result count", `{"query":"code","max_results":101}`, "max_results"},
		{"oversized token budget", `{"query":"memory","token_budget":32769}`, "token_budget"},
		{"max integer result count", `{"query":"code","max_results":2147483647}`, "max_results"},
		{"max integer token budget", `{"query":"memory","token_budget":2147483647}`, "token_budget"},
		{"negative result count", `{"query":"code","max_results":-1}`, "max_results"},
		{"negative token budget", `{"query":"memory","token_budget":-1}`, "token_budget"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/retrieve", bytes.NewBufferString(tc.body))
			req.Header.Set("Authorization", "Bearer shared-secret")
			req.Header.Set("X-SCOS-Tenant-ID", "tenant-a")
			req.Header.Set("X-SCOS-Principal-ID", "principal-a")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400 before DB: %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.wantField) {
				t.Fatalf("wrong validation reason: %s", rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "database") || strings.Contains(rec.Body.String(), "panic") {
				t.Fatalf("unexpected internal detail: %s", rec.Body.String())
			}
		})
	}
}

func TestP4RetrievalBudgetBoundsAndDefaults(t *testing.T) {
	for _, tc := range []struct {
		results, tokens, wantResults, wantTokens int
	}{
		{0, 0, defaultMaxResults, defaultTokenBudget},
		{1, 1, 1, 1},
		{100, 32768, 100, 32768},
	} {
		t.Run(fmt.Sprintf("%d-%d", tc.results, tc.tokens), func(t *testing.T) {
			results, tokens, err := validateRetrievalBudgets(tc.results, tc.tokens)
			if err != nil || results != tc.wantResults || tokens != tc.wantTokens {
				t.Fatalf("got (%d,%d,%v), want (%d,%d,nil)", results, tokens, err, tc.wantResults, tc.wantTokens)
			}
		})
	}
}
