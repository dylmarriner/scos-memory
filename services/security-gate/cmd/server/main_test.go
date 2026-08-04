package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dylmarriner/scos-memory/services/readiness"
)

func TestNewSecurityServer(t *testing.T) {
	t.Setenv("SCOS_SECURITY_ISSUER_SECRET", "issuer-secret")
	s := newSecurityServer()
	if s == nil {
		t.Fatal("expected non-nil server")
	}
	if s.issuerSecret != "issuer-secret" {
		t.Fatal("expected issuer secret to be loaded from environment")
	}
}

func TestReadyRejectsMissingIssuerSecretWithoutExposingIt(t *testing.T) {
	t.Setenv("SCOS_SECURITY_ISSUER_SECRET", "")
	s := newSecurityServer()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health/ready", nil)
	readiness.Handler(s.ready)(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unexpected readiness response: %d %s", rec.Code, rec.Body.String())
	}
}

func TestReadyRejectsMissingDatabase(t *testing.T) {
	// A server with an issuer secret but no postgres dependency configured
	// must report itself not ready, not a hardcoded success.
	t.Setenv("SCOS_SECURITY_ISSUER_SECRET", "issuer-secret")
	s := newSecurityServer()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health/ready", nil)
	readiness.Handler(s.ready)(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected unhealthy status without a configured postgres dependency, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestHTTPAuthIssueRequiresIssuerSecret(t *testing.T) {
	t.Setenv("SCOS_SECURITY_ISSUER_SECRET", "issuer-secret")
	s := newSecurityServer()
	body := bytes.NewBufferString(`{"principal":"123e4567-e89b-12d3-a456-426614174000"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/issue", body)
	rec := httptest.NewRecorder()
	s.handleAuthIssue(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without issuer secret, got %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/auth/issue", bytes.NewBufferString(`{"principal":"123e4567-e89b-12d3-a456-426614174000"}`))
	req.Header.Set("X-SCOS-Issuer-Secret", "wrong-secret")
	rec = httptest.NewRecorder()
	s.handleAuthIssue(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with wrong issuer secret, got %d", rec.Code)
	}
}

func TestHTTPAuthIssueRequiresUUIDPrincipalAndTenant(t *testing.T) {
	t.Setenv("SCOS_SECURITY_ISSUER_SECRET", "issuer-secret")
	s := newSecurityServer()
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/issue", bytes.NewBufferString(`{"principal":"not-a-uuid","tenant":"123e4567-e89b-12d3-a456-426614174000"}`))
	req.Header.Set("X-SCOS-Issuer-Secret", "issuer-secret")
	rec := httptest.NewRecorder()
	s.handleAuthIssue(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for non-UUID principal, got %d", rec.Code)
	}
}

func TestPermissionCheckRequiresBearerToken(t *testing.T) {
	t.Setenv("SCOS_SECURITY_ISSUER_SECRET", "issuer-secret")
	s := newSecurityServer()
	req := httptest.NewRequest(http.MethodPost, "/v1/permissions/check", bytes.NewBufferString(`{"action":"read","resource":"doc"}`))
	rec := httptest.NewRecorder()
	s.handlePermissionCheck(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without bearer token, got %d", rec.Code)
	}
}

func TestPermissionCheckRejectsForgedToken(t *testing.T) {
	t.Setenv("SCOS_SECURITY_ISSUER_SECRET", "issuer-secret")
	s := newSecurityServer()
	req := httptest.NewRequest(http.MethodPost, "/v1/permissions/check", bytes.NewBufferString(`{"action":"read","resource":"doc"}`))
	req.Header.Set("Authorization", "Bearer forged.signature")
	rec := httptest.NewRecorder()
	s.handlePermissionCheck(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for forged token, got %d", rec.Code)
	}
}

func TestAuthValidateRejectsMalformedToken(t *testing.T) {
	t.Setenv("SCOS_SECURITY_ISSUER_SECRET", "issuer-secret")
	s := newSecurityServer()
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/validate", bytes.NewBufferString(`{"token":"garbage"}`))
	rec := httptest.NewRecorder()
	s.handleAuthValidate(rec, req)
	var result AuthResult
	if err := json.NewDecoder(rec.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Valid {
		t.Fatal("expected malformed token to be rejected")
	}
}

func TestNewTokenIDRoundTripsThroughValidToken(t *testing.T) {
	t.Setenv("SCOS_SECURITY_ISSUER_SECRET", "issuer-secret")
	s := newSecurityServer()
	token, err := s.newTokenID()
	if err != nil {
		t.Fatal(err)
	}
	if !s.validToken(token) {
		t.Fatal("expected freshly minted token to validate")
	}
	forged := token[:len(token)-1] + "x"
	if s.validToken(forged) {
		t.Fatal("expected tampered token to fail validation")
	}
}

func TestTokenPublicPart(t *testing.T) {
	if got := tokenPublicPart("abc.def"); got != "abc" {
		t.Fatalf("expected 'abc', got %q", got)
	}
}

func TestPermissionCheckPublic(t *testing.T) {
	check := &PermissionCheck{
		Principal: "user-1", Action: "read",
		Resource: "doc.md", DataClass: ClassPublic, Tenant: "t-1",
	}
	allowed := evaluatePermission(check)
	if !allowed {
		t.Fatal("public data should be allowed")
	}
}

func TestPermissionCheckSecret(t *testing.T) {
	check := &PermissionCheck{
		Principal: "user-1", Action: "read",
		Resource: "secret.doc", DataClass: ClassSecret, Tenant: "t-1",
	}
	allowed := evaluatePermission(check)
	if allowed {
		t.Fatal("secret should be denied by default")
	}
}

func TestClassRankOrdering(t *testing.T) {
	if classRank(ClassPublic) >= classRank(ClassSecret) {
		t.Fatal("expected secret to rank above public")
	}
	if classRank("unknown-class") != -1 {
		t.Fatal("expected unknown class to rank as -1")
	}
}

func TestClassifyPublicContent(t *testing.T) {
	cls := classifyContent("hello world public information")
	if cls != ClassPublic {
		t.Fatal("expected public classification")
	}
}

func TestClassifySecretContent(t *testing.T) {
	cls := classifyContent("the secret key is abc123")
	if cls != ClassSecret {
		t.Fatal("expected secret classification")
	}
}

func TestClassifyConfidentialContent(t *testing.T) {
	cls := classifyContent("this is confidential data")
	if cls != ClassConfidential {
		t.Fatal("expected confidential classification")
	}
}

func TestClassifyInternalContent(t *testing.T) {
	cls := classifyContent("internal memo for staff only")
	if cls != ClassInternal {
		t.Fatal("expected internal classification")
	}
}

func TestClassifyPasswordContent(t *testing.T) {
	cls := classifyContent("password = hunter2")
	if cls != ClassSecret {
		t.Fatal("expected secret classification for password")
	}
}

func TestDataClasses(t *testing.T) {
	if ClassPublic != "public" {
		t.Fatal("wrong public class")
	}
	if ClassInternal != "internal" {
		t.Fatal("wrong internal class")
	}
	if ClassConfidential != "confidential" {
		t.Fatal("wrong confidential class")
	}
	if ClassRestricted != "restricted" {
		t.Fatal("wrong restricted class")
	}
	if ClassSecret != "secret" {
		t.Fatal("wrong secret class")
	}
}

func TestIsUUID(t *testing.T) {
	if !isUUID("123e4567-e89b-12d3-a456-426614174000") {
		t.Fatal("expected valid UUID to be recognised")
	}
	for _, bad := range []string{"", "user-1", "not-a-uuid"} {
		if isUUID(bad) {
			t.Fatalf("expected %q to be rejected as a UUID", bad)
		}
	}
}

func TestResolveIdentityRequiresTenantAndPrincipal(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/v1/audit", nil)
	_, status, _ := resolveIdentity(r, "", "")
	if status != http.StatusUnauthorized {
		t.Fatalf("expected 401 without tenant/principal, got %d", status)
	}
}

func TestResolveIdentityFromHeaders(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/v1/audit", nil)
	r.Header.Set("X-SCOS-Tenant-ID", "tenant-1")
	r.Header.Set("X-SCOS-Principal-ID", "principal-1")
	identity, status, _ := resolveIdentity(r, "", "")
	if status != 0 {
		t.Fatalf("expected success, got status %d", status)
	}
	if identity.TenantID != "tenant-1" || identity.PrincipalID != "principal-1" {
		t.Fatalf("unexpected identity: %+v", identity)
	}
}

func TestAuditRecordJSONShape(t *testing.T) {
	record := AuditRecord{
		Action: "read", Principal: "user-1", Resource: "doc.md",
		Result: "allowed", Timestamp: "2026-01-01T00:00:00Z",
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	var decoded AuditRecord
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Action != "read" || decoded.Result != "allowed" {
		t.Fatalf("roundtrip failed: %+v", decoded)
	}
}

func TestSecretReferenceJSONShape(t *testing.T) {
	ref := SecretReference{ID: "sec-1", Name: "db-password", Provider: "vault", Ref: "secret/data/db", Scope: "production"}
	data, err := json.Marshal(ref)
	if err != nil {
		t.Fatal(err)
	}
	var decoded SecretReference
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Provider != "vault" {
		t.Fatal("provider mismatch")
	}
}
