package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

// handleHookDispatch used to trust the JSON body's tenant_id/principal_id
// outright with no cross-check against the caller's authenticated identity.
// Reaching this service as an MCP tool call forwarded through mcp-gateway,
// a caller authenticated as one tenant could name any other tenant in the
// body and have hooks dispatched -- and external webhook endpoints
// notified -- as that victim tenant, receiving an X-SCOS-Tenant-ID header
// naming a tenant the caller does not belong to. This is the same bug class
// as code-intelligence's confirmed cross-tenant IDOR (see that service's
// TestRequireTenantPrefersClaimsOverSpoofedHeader): a verified identity must
// win over anything the caller supplies itself.
func TestHandleHookDispatchRejectsSpoofedTenantInBody(t *testing.T) {
	srv := newExtServer()
	srv.hooks["turn.completed"] = []*Hook{{
		Event: "turn.completed", ExtensionID: "hook-ext", Endpoint: "https://127.0.0.1:0", TimeoutMs: 1000,
	}}

	body := `{"event":"turn.completed","tenant_id":"victim-tenant","principal_id":"victim","payload":{}}`
	r := httptest.NewRequest(http.MethodPost, "/v1/hooks/dispatch", bytes.NewBufferString(body))
	ctx := auth.ContextWithClaims(r.Context(), auth.Claims{TenantID: "attacker-tenant", PrincipalID: "attacker"})
	r = r.WithContext(ctx)

	w := httptest.NewRecorder()
	srv.handleHookDispatch(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a body tenant that doesn't match the authenticated identity, got %d: %s", w.Code, w.Body.String())
	}
}

// A caller supplying no tenant_id/principal_id at all (the shape
// autonomous-control-plane's dispatchHook actually sends) must still
// dispatch, using the authenticated identity.
func TestHandleHookDispatchUsesAuthenticatedIdentityWhenBodyOmitsTenant(t *testing.T) {
	srv := newExtServer()
	called := make(chan string, 1)
	hookServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called <- r.Header.Get("X-SCOS-Tenant-ID")
		w.WriteHeader(http.StatusOK)
	}))
	defer hookServer.Close()
	srv.extensions["hook-ext"] = &Extension{Manifest: ExtensionManifest{PackageID: "hook-ext"}, Status: "enabled"}
	srv.hooks["turn.completed"] = []*Hook{{
		Event: "turn.completed", ExtensionID: "hook-ext", Endpoint: hookServer.URL, TimeoutMs: 1000,
	}}

	body := `{"event":"turn.completed","payload":{}}`
	r := httptest.NewRequest(http.MethodPost, "/v1/hooks/dispatch", bytes.NewBufferString(body))
	ctx := auth.ContextWithClaims(r.Context(), auth.Claims{TenantID: "tenant-a", PrincipalID: "principal-a"})
	r = r.WithContext(ctx)

	w := httptest.NewRecorder()
	srv.handleHookDispatch(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	select {
	case tenant := <-called:
		if tenant != "tenant-a" {
			t.Fatalf("expected the hook to be dispatched as the authenticated tenant, got %q", tenant)
		}
	default:
		t.Fatal("hook endpoint was not invoked")
	}
}
