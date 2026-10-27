package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

// TestResolveIdentityStaticTokenRequiresBodyHeaderAgreement exercises the
// actual shared-token middleware path. Headers supplied by the trusted gateway
// select the identity; a contradictory request body must fail closed.
func TestResolveIdentityStaticTokenRequiresBodyHeaderAgreement(t *testing.T) {
	cases := []struct {
		name, bodyTenant, bodyPrincipal string
		wantStatus                      int
	}{
		{"tenant override", "foreign-tenant", "real-principal", http.StatusForbidden},
		{"principal override", "real-tenant", "foreign-principal", http.StatusForbidden},
		{"both override", "foreign-tenant", "foreign-principal", http.StatusForbidden},
		{"matching identity", "real-tenant", "real-principal", http.StatusNoContent},
		{"omitted identity", "", "", http.StatusNoContent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := auth.AuthenticatedHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				tenant, principal, status, msg := resolveIdentity(r, tc.bodyTenant, tc.bodyPrincipal)
				if status != 0 {
					http.Error(w, msg, status)
					return
				}
				if tenant != "real-tenant" || principal != "real-principal" {
					t.Errorf("resolved unexpected identity: %q/%q", tenant, principal)
				}
				w.WriteHeader(http.StatusNoContent)
			}), auth.Config{Token: "shared-secret", Identity: "internal-service"})
			req := httptest.NewRequest(http.MethodPost, "/v1/retrieve", nil)
			req.Header.Set("Authorization", "Bearer shared-secret")
			req.Header.Set("X-SCOS-Tenant-ID", "real-tenant")
			req.Header.Set("X-SCOS-Principal-ID", "real-principal")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}
}

// TestRetrieveStaticTokenRejectsConflictingBody exercises the production
// handler/mux boundary and rejects the mismatch before database access.
func TestRetrieveStaticTokenRejectsConflictingBody(t *testing.T) {
	handler := auth.AuthenticatedHandler(newRetrievalMux(&server{}), auth.Config{
		Token: "shared-secret", Identity: "internal-service",
	})
	for _, body := range []string{
		`{"tenant_id":"foreign-tenant","principal_id":"real-principal","query":"secret"}`,
		`{"tenant_id":"real-tenant","principal_id":"foreign-principal","query":"secret"}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/v1/retrieve", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer shared-secret")
		req.Header.Set("X-SCOS-Tenant-ID", "real-tenant")
		req.Header.Set("X-SCOS-Principal-ID", "real-principal")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 for %s: %s", rec.Code, body, rec.Body.String())
		}
	}
}
