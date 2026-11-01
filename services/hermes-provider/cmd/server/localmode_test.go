package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

const testIssuerSecret = "local-mode-test-secret"

func localModeServer(t *testing.T) (http.Handler, *requestIdentity) {
	t.Helper()
	seen := &requestIdentity{}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, status, msg := resolveIdentity(r, "", "")
		if status != 0 {
			http.Error(w, msg, status)
			return
		}
		*seen = identity
		w.WriteHeader(http.StatusOK)
	})
	config := auth.Config{IssuerSecret: testIssuerSecret, Identity: "internal-service"}
	return localModeHandler(auth.AuthenticatedHandler(inner, config), &localIdentityToken{secret: testIssuerSecret}), seen
}

// A caller with no bearer token acts as the local identity, whatever
// identity headers it sends.
func TestLocalModeGivesAnUnauthenticatedCallerTheLocalIdentity(t *testing.T) {
	handler, seen := localModeServer(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/provider/status", nil)
	req.Header.Set("X-SCOS-Tenant-ID", "11111111-1111-1111-1111-111111111111")
	req.Header.Set("X-SCOS-Principal-ID", "11111111-1111-1111-1111-111111111111")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if seen.TenantID != localModeTenantID || seen.PrincipalID != localModePrincipalID {
		t.Fatalf("identity = %+v, want the local identity", *seen)
	}
}

// A presented token is verified and names its caller; an invalid one is
// refused rather than replaced by the local identity.
func TestLocalModeStillVerifiesAPresentedToken(t *testing.T) {
	handler, seen := localModeServer(t)
	other := "22222222-2222-2222-2222-222222222222"
	token, err := auth.SignClaims(auth.Claims{TenantID: other, PrincipalID: other, Subject: other, ExpiresAt: time.Now().Add(2 * time.Minute)}, testIssuerSecret)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/provider/status", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || seen.TenantID != other {
		t.Fatalf("status = %d, identity = %+v; want the token's identity", rec.Code, *seen)
	}

	forged, err := auth.SignClaims(auth.Claims{TenantID: other, PrincipalID: other, Subject: other, ExpiresAt: time.Now().Add(2 * time.Minute)}, "another-secret")
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, "/v1/provider/status", nil)
	req.Header.Set("Authorization", "Bearer "+forged)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("forged token status = %d, want 401", rec.Code)
	}
}
