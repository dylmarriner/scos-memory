package auth

import (
	"testing"
	"time"
)

func TestSignClaimsRoundTrip(t *testing.T) {
	token, err := SignClaims(Claims{TenantID: "tenant", PrincipalID: "principal", Subject: "user", ExpiresAt: time.Now().Add(time.Minute), Scopes: []string{"memory:read"}}, "secret")
	if err != nil {
		t.Fatal(err)
	}
	claims, err := validateClaims(token, "secret")
	if err != nil {
		t.Fatal(err)
	}
	if claims.TenantID != "tenant" || claims.PrincipalID != "principal" || len(claims.Scopes) != 1 {
		t.Fatalf("unexpected claims: %+v", claims)
	}
}
