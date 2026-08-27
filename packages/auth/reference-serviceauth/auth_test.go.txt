package serviceauth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestIssueAndParseRequest(t *testing.T) {
	secret := []byte("test-secret")
	token, err := Issue(Identity{Principal: "p1", Tenant: "t1", ExpiresAt: time.Now().Add(time.Hour).Unix()}, secret)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/data", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	identity, err := ParseRequest(req, secret)
	if err != nil {
		t.Fatal(err)
	}
	if identity.Principal != "p1" || identity.Tenant != "t1" || identity.JTI == "" {
		t.Fatalf("unexpected identity: %+v", identity)
	}
}

func TestParseRequestRejectsTamperingAndExpiry(t *testing.T) {
	secret := []byte("test-secret")
	token, err := Issue(Identity{Principal: "p1", Tenant: "t1", ExpiresAt: time.Now().Add(time.Hour).Unix()}, secret)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token+"x")
	if _, err := ParseRequest(req, secret); err == nil {
		t.Fatal("tampered token accepted")
	}
	expired, err := Issue(Identity{Principal: "p1", Tenant: "t1", ExpiresAt: time.Now().Add(-time.Hour).Unix()}, secret)
	if err == nil || expired != "" {
		t.Fatal("expired identity issued")
	}
}

func TestRequireAllowsHealthAndRejectsUnauthenticatedData(t *testing.T) {
	t.Setenv("SCOS_AUTH_ISSUER_SECRET", "test-secret")
	handler := Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if health.Code != http.StatusNoContent {
		t.Fatalf("health status = %d", health.Code)
	}
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/v1/data", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.Code)
	}
}

func TestRequireScopeRejectsUnscopedIdentity(t *testing.T) {
	t.Setenv("SCOS_AUTH_ISSUER_SECRET", "test-secret")
	token, err := Issue(Identity{Principal: "p", Tenant: "t", ExpiresAt: time.Now().Add(time.Hour).Unix()}, []byte("test-secret"))
	if err != nil {
		t.Fatal(err)
	}
	handler := Require(RequireScope("admin:write", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})))
	req := httptest.NewRequest(http.MethodPost, "/v1/admin", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusForbidden {
		t.Fatalf("scope status = %d, want %d", response.Code, http.StatusForbidden)
	}
}
