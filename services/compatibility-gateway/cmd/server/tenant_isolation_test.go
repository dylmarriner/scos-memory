package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

// resolveIdentity has the same claims-vs-header/body shape as
// code-intelligence's requireTenant, where trusting a caller-supplied
// header over verified claims was a confirmed cross-tenant IDOR (see that
// service's TestRequireTenantPrefersClaimsOverSpoofedHeader). These are the
// regression guards for the same bug class here: a caller authenticated as
// one tenant must never be able to nominate a different tenant via the
// request body or the X-SCOS-Tenant-ID header.

func TestResolveIdentityRejectsBodyMismatchWithClaims(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/agents", nil)
	ctx := auth.ContextWithClaims(r.Context(), auth.Claims{TenantID: "attacker-tenant", PrincipalID: "attacker"})
	r = r.WithContext(ctx)

	identity, status, msg := resolveIdentity(r, "victim-tenant", "attacker")
	if status != http.StatusForbidden {
		t.Fatalf("expected 403 for a body tenant that doesn't match claims, got %d (%q), identity=%+v", status, msg, identity)
	}
}

func TestResolveIdentityIgnoresSpoofedHeaderWhenClaimsPresent(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/v1/agents", nil)
	r.Header.Set("X-SCOS-Tenant-ID", "victim-tenant")
	ctx := auth.ContextWithClaims(r.Context(), auth.Claims{TenantID: "attacker-tenant", PrincipalID: "attacker"})
	r = r.WithContext(ctx)

	identity, status, msg := resolveIdentity(r, "", "")
	if status != 0 {
		t.Fatalf("expected success, got status %d (%q)", status, msg)
	}
	if identity.TenantID != "attacker-tenant" {
		t.Fatalf("expected the authenticated tenant %q to win over the spoofed header, got %q", "attacker-tenant", identity.TenantID)
	}
}
