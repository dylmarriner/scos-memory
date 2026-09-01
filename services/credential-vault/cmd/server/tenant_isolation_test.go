package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

// resolveIdentity has the same claims-vs-header shape as the other
// tenant-aware services in this fleet, where trusting a caller-supplied
// header over verified claims was a confirmed cross-tenant IDOR in
// code-intelligence (see that service's
// TestRequireTenantPrefersClaimsOverSpoofedHeader). This is the regression
// guard for the same bug class here: a caller authenticated as one tenant
// must never be able to nominate a different tenant via the
// X-SCOS-Tenant-ID header.
func TestResolveIdentityIgnoresSpoofedHeaderWhenClaimsPresent(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/v1/credentials", nil)
	r.Header.Set("X-SCOS-Tenant-ID", "victim-tenant")
	r.Header.Set("X-SCOS-Principal-ID", "victim")
	ctx := auth.ContextWithClaims(r.Context(), auth.Claims{TenantID: "attacker-tenant", PrincipalID: "attacker"})
	r = r.WithContext(ctx)

	identity, status, msg := resolveIdentity(r)
	if status != 0 {
		t.Fatalf("expected success, got status %d (%q)", status, msg)
	}
	if identity.TenantID != "attacker-tenant" || identity.PrincipalID != "attacker" {
		t.Fatalf("expected the authenticated identity to win over the spoofed header, got %+v", identity)
	}
}
