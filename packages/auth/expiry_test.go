package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A valid signature cannot substitute for expiration. Previously such tokens
// authenticated forever when the exp field was absent, zero or negative.
func TestAuthenticatedHandlerRejectsUnexpiringSignedClaims(t *testing.T) {
	handler := AuthenticatedHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("unexpiring token reached a protected handler")
		w.WriteHeader(http.StatusNoContent)
	}), Config{IssuerSecret: "issuer"})

	for _, tc := range []struct {
		name    string
		payload string
	}{
		{"missing", `{"tenant_id":"tenant","principal_id":"principal"}`},
		{"zero", `{"tenant_id":"tenant","principal_id":"principal","exp":0}`},
		{"negative", `{"tenant_id":"tenant","principal_id":"principal","exp":-5}`},
		{"expired", `{"tenant_id":"tenant","principal_id":"principal","exp":1000000000}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/v1/private", nil)
			request.Header.Set("Authorization", "Bearer "+testJWT("issuer", tc.payload))
			result := httptest.NewRecorder()
			handler.ServeHTTP(result, request)
			if result.Code != http.StatusUnauthorized {
				t.Fatalf("signed %s expiration accepted: HTTP %d", tc.name, result.Code)
			}
		})
	}
}

func TestSignClaimsRequiresFutureExpiration(t *testing.T) {
	for _, tc := range []struct {
		name string
		exp  time.Time
	}{
		{"omitted", time.Time{}},
		{"expired", time.Now().Add(-time.Minute)},
		{"at-current-second", time.Unix(time.Now().Unix(), 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := SignClaims(Claims{TenantID: "tenant", PrincipalID: "principal", ExpiresAt: tc.exp}, "secret")
			if err == nil || !strings.Contains(err.Error(), "expir") {
				t.Fatalf("signer must reject %s expiration, got %v", tc.name, err)
			}
		})
	}
}

func TestSignClaimsWithFutureExpirationStillWorks(t *testing.T) {
	token, err := SignClaims(Claims{TenantID: "tenant", PrincipalID: "principal", ExpiresAt: time.Now().Add(2 * time.Minute)}, "secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = validateClaims(token, "secret"); err != nil {
		t.Fatalf("valid short-lived token rejected: %v", err)
	}
}
