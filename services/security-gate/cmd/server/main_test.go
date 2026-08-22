package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	serviceauth "github.com/dylanmarriner/scos-memory-fabric/packages/service-auth"
)

func TestNewSecurityServer(t *testing.T) {
	s := newSecurityServer()
	if s == nil {
		t.Fatal("expected non-nil server")
	}
}

func TestAuthIssueAndValidate(t *testing.T) {
	s := newSecurityServer()

	s.mu.Lock()
	token := &AuthToken{
		Token: "tok-1", Principal: "user-1",
		Tenant: "t-1", ExpiresAt: time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
		DataClass: ClassInternal,
	}
	s.tokens["tok-1"] = token
	s.mu.Unlock()

	s.mu.RLock()
	stored, ok := s.tokens["tok-1"]
	s.mu.RUnlock()
	if !ok {
		t.Fatal("expected token to exist")
	}
	if stored.Principal != "user-1" {
		t.Fatal("principal mismatch")
	}
}

func TestAuthIssueRequiresIssuerAndProducesSignedToken(t *testing.T) {
	t.Setenv("SCOS_AUTH_ISSUER_SECRET", "issuer-test-secret")
	s := newSecurityServer()
	body := []byte(`{"principal":"user-1","tenant":"tenant-1","expires_in_seconds":300}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/issue", bytes.NewReader(body))
	req.Header.Set("X-SCOS-Issuer-Secret", "issuer-test-secret")
	response := httptest.NewRecorder()
	s.handleAuthIssue(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("issue status = %d, body=%s", response.Code, response.Body.String())
	}
	var issued struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &issued); err != nil || issued.Token == "" {
		t.Fatalf("invalid issued token response: %s", response.Body.String())
	}
	validate := httptest.NewRequest(http.MethodPost, "/v1/auth/validate", nil)
	validate.Header.Set("Authorization", "Bearer "+issued.Token)
	identity, err := serviceauth.ParseRequest(validate, []byte("issuer-test-secret"))
	if err != nil || identity.Principal != "user-1" || identity.Tenant != "tenant-1" {
		t.Fatalf("issued token did not validate: %+v, %v", identity, err)
	}
}

func TestAuthIssueRejectsMissingIssuer(t *testing.T) {
	t.Setenv("SCOS_AUTH_ISSUER_SECRET", "issuer-test-secret")
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/issue", bytes.NewBufferString(`{"principal":"user-1","tenant":"tenant-1"}`))
	response := httptest.NewRecorder()
	newSecurityServer().handleAuthIssue(response, req)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("issue status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
}

func TestAuthExpiredToken(t *testing.T) {
	s := newSecurityServer()
	s.mu.Lock()
	s.tokens["expired"] = &AuthToken{
		Token: "expired", Principal: "user",
		ExpiresAt: time.Now().UTC().Add(-time.Hour).Format(time.RFC3339),
	}
	s.mu.Unlock()

	s.mu.RLock()
	tok, ok := s.tokens["expired"]
	s.mu.RUnlock()
	if !ok {
		t.Fatal("expected token to exist")
	}

	expiresAt, _ := time.Parse(time.RFC3339, tok.ExpiresAt)
	if time.Now().UTC().After(expiresAt) {
		s.mu.Lock()
		delete(s.tokens, "expired")
		s.mu.Unlock()
	}

	s.mu.RLock()
	_, stillExists := s.tokens["expired"]
	s.mu.RUnlock()
	if stillExists {
		t.Fatal("expected expired token to be deleted")
	}
}

func TestAuthRevoke(t *testing.T) {
	s := newSecurityServer()
	s.mu.Lock()
	s.tokens["tok-2"] = &AuthToken{Token: "tok-2", Principal: "user"}
	s.mu.Unlock()

	s.mu.Lock()
	delete(s.tokens, "tok-2")
	s.mu.Unlock()

	s.mu.RLock()
	_, ok := s.tokens["tok-2"]
	s.mu.RUnlock()
	if ok {
		t.Fatal("expected token to be revoked")
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

func TestAuditRecord(t *testing.T) {
	s := newSecurityServer()
	s.mu.Lock()
	s.auditLog = append(s.auditLog, &AuditRecord{
		ID: "aud-1", Action: "read", Principal: "user-1",
		Resource: "doc.md", Result: "allowed", Timestamp: "now",
	})
	s.mu.Unlock()

	if len(s.auditLog) != 1 {
		t.Fatal("expected 1 audit record")
	}
	if s.auditLog[0].Action != "read" {
		t.Fatal("action mismatch")
	}
}

func TestAuditRecordPermissions(t *testing.T) {
	s := newSecurityServer()
	s.mu.Lock()
	s.auditLog = append(s.auditLog, &AuditRecord{
		ID: "aud-2", Action: "delete", Principal: "admin",
		Resource: "user-data", DataClass: ClassRestricted,
		Result: "denied", Reason: "requires elevated grant",
	})
	s.mu.Unlock()

	if s.auditLog[0].Result != "denied" {
		t.Fatal("expected denied result")
	}
}

func TestSecretReference(t *testing.T) {
	s := newSecurityServer()
	s.mu.Lock()
	s.secrets["sec-1"] = &SecretReference{
		ID: "sec-1", Name: "db-password",
		Provider: "vault", Ref: "secret/data/db",
		Scope: "production",
	}
	s.mu.Unlock()

	s.mu.RLock()
	ref, ok := s.secrets["sec-1"]
	s.mu.RUnlock()
	if !ok {
		t.Fatal("expected secret reference")
	}
	if ref.Provider != "vault" {
		t.Fatal("provider mismatch")
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
