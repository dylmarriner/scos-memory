package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dylmarriner/scos-memory/services/readiness"
)

// newResolveTestServer builds a securityServer configured to resolve
// against a stub Securestore at the given base URL, with no PostgreSQL
// dependency — matching how other handlers in this file are exercised with
// s.db == nil.
func newResolveTestServer(baseURL string) *securityServer {
	return &securityServer{
		issuerSecret:          "issuer-secret",
		securestoreURL:        baseURL,
		securestoreAPIKey:     "sss_test",
		securestoreHTTPClient: http.DefaultClient,
	}
}

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
	// Flip the first character deterministically. Flipping the last
	// character is flaky: with a 64-symbol token alphabet the last char is
	// "x" roughly 1/64 of the time, so `token[:len-1] + "x"` can equal the
	// original token and the tamper check would incorrectly pass.
	forged := "x" + token[1:]
	if forged == token {
		forged = "y" + token[1:]
	}
	if s.validToken(forged) {
		t.Fatal("expected tampered token to fail validation")
	}
}

// TestAuthRevokeRequiresProofOfPossession guards against revoking a token by
// public id alone. handleAuthRevoke must reject a "token" that has the right
// public id but an invalid (or missing) HMAC signature -- otherwise anyone
// who merely learns a token's public id (e.g. from a same-tenant audit log
// entry) could revoke it without ever having held the real bearer token.
func TestAuthRevokeRequiresProofOfPossession(t *testing.T) {
	t.Setenv("SCOS_SECURITY_ISSUER_SECRET", "issuer-secret")
	s := newSecurityServer()
	token, err := s.newTokenID()
	if err != nil {
		t.Fatal(err)
	}
	publicID := tokenPublicPart(token)

	for _, tc := range []struct {
		name  string
		token string
	}{
		{"public id alone, no signature", publicID},
		{"public id with forged signature", publicID + ".forged-signature"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(map[string]string{"token": tc.token})
			req := httptest.NewRequest(http.MethodPost, "/v1/auth/revoke", bytes.NewReader(body))
			rec := httptest.NewRecorder()
			s.handleAuthRevoke(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401 without a valid signature, got %d: %s", rec.Code, rec.Body.String())
			}
		})
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
	allowed, _ := evaluatePermission(check, &AuthToken{DataClass: ClassInternal}, time.Now().UTC())
	if !allowed {
		t.Fatal("public data should be allowed")
	}
}

func TestPermissionCheckSecret(t *testing.T) {
	check := &PermissionCheck{Principal: "user-1", Action: "read", Resource: "secret.doc", DataClass: ClassSecret, Tenant: "t-1"}
	now := time.Now().UTC()
	if ok, _ := evaluatePermission(check, &AuthToken{DataClass: ClassRestricted}, now); ok {
		t.Fatal("secret allowed for a restricted token")
	}
	if ok, reason := evaluatePermission(check, &AuthToken{DataClass: ClassSecret}, now); !ok {
		t.Fatalf("secret denied for a secret-granted token: %s", reason)
	}
}

func TestPermissionCheckDeniesUnknownClassAndExpiredWindow(t *testing.T) {
	now := time.Now().UTC()
	token := &AuthToken{DataClass: ClassSecret}
	if ok, reason := evaluatePermission(&PermissionCheck{DataClass: "top-secret-ish"}, token, now); ok || reason != "unknown data class" {
		t.Fatalf("unknown class = %v %q", ok, reason)
	}
	past := now.Add(-time.Minute).Format(time.RFC3339)
	if ok, _ := evaluatePermission(&PermissionCheck{DataClass: ClassPublic, ExpiresAt: past}, token, now); ok {
		t.Fatal("expired authorisation window allowed")
	}
	if ok, _ := evaluatePermission(&PermissionCheck{DataClass: ClassPublic, ExpiresAt: "tomorrow"}, token, now); ok {
		t.Fatal("unparseable expires_at allowed")
	}
	future := now.Add(time.Hour).Format(time.RFC3339)
	if ok, _ := evaluatePermission(&PermissionCheck{DataClass: ClassPublic, ExpiresAt: future}, token, now); !ok {
		t.Fatal("unexpired window denied")
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

func TestValidSecretProvidersAcceptsSecurestore(t *testing.T) {
	for _, provider := range []string{"env", "vault", "aws-secrets", "gcp-sm", "securestore"} {
		if !validSecretProviders[provider] {
			t.Fatalf("expected %q to be a valid provider", provider)
		}
	}
	if validSecretProviders["not-a-real-provider"] {
		t.Fatal("expected unknown provider to be rejected")
	}
}

func TestHandleSecretsRejectsUnsupportedProvider(t *testing.T) {
	s := newResolveTestServer("")
	body := bytes.NewBufferString(`{"id":"sec-1","name":"db-password","provider":"unsupported","ref":"whatever"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/secrets", body)
	req.Header.Set("X-SCOS-Tenant-ID", "tenant-1")
	req.Header.Set("X-SCOS-Principal-ID", "principal-1")
	rec := httptest.NewRecorder()
	s.handleSecrets(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unsupported provider, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleSecretResolveRequiresAuthorizedIdentity(t *testing.T) {
	s := newResolveTestServer("https://securestore.invalid")
	req := httptest.NewRequest(http.MethodPost, "/v1/secrets/sec-1/resolve", nil)
	rec := httptest.NewRecorder()
	s.handleSecretResolve(rec, req, "sec-1")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without an authenticated identity, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleSecretResolveOnlyAllowsPost(t *testing.T) {
	s := newResolveTestServer("https://securestore.invalid")
	req := httptest.NewRequest(http.MethodGet, "/v1/secrets/sec-1/resolve", nil)
	req.Header.Set("X-SCOS-Tenant-ID", "tenant-1")
	req.Header.Set("X-SCOS-Principal-ID", "principal-1")
	rec := httptest.NewRecorder()
	s.handleSecretResolve(rec, req, "sec-1")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for GET, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleSecretResolveFailsClosedWhenNotConfigured(t *testing.T) {
	s := newResolveTestServer("")
	req := httptest.NewRequest(http.MethodPost, "/v1/secrets/sec-1/resolve", nil)
	req.Header.Set("X-SCOS-Tenant-ID", "tenant-1")
	req.Header.Set("X-SCOS-Principal-ID", "principal-1")
	rec := httptest.NewRecorder()
	s.handleSecretResolve(rec, req, "sec-1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when securestore is not configured, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestResolveSecretValueRejectsNonSecurestoreProvider(t *testing.T) {
	s := newResolveTestServer("https://securestore.invalid")
	identity := requestIdentity{TenantID: "tenant-1", PrincipalID: "principal-1"}
	ref := &SecretReference{ID: "sec-1", Name: "db-password", Provider: "vault", Ref: "secret/data/db"}
	req := httptest.NewRequest(http.MethodPost, "/v1/secrets/sec-1/resolve", nil)
	rec := httptest.NewRecorder()
	s.resolveSecretValue(rec, req, identity, ref)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a non-securestore reference, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestResolveSecretValueSuccessAuditsAndReturnsValue(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sss_test" {
			t.Errorf("expected agent API key bearer, got %q", r.Header.Get("Authorization"))
		}
		if r.URL.Path != "/v1/secrets/cred-1" {
			t.Errorf("unexpected upstream path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"credentialId":   "cred-1",
			"provider":       "aws-secrets",
			"credentialType": "database-password",
			"authScheme":     "static",
			"value":          "s3cr3t-value",
			"version":        "3",
		})
	}))
	defer upstream.Close()

	s := newResolveTestServer(upstream.URL)
	var audited []AuditRecord
	s.auditHook = func(record AuditRecord) { audited = append(audited, record) }

	identity := requestIdentity{TenantID: "tenant-1", PrincipalID: "principal-1"}
	ref := &SecretReference{ID: "sec-1", Name: "db-password", Provider: "securestore", Ref: "cred-1"}
	req := httptest.NewRequest(http.MethodPost, "/v1/secrets/sec-1/resolve", nil)
	rec := httptest.NewRecorder()
	s.resolveSecretValue(rec, req, identity, ref)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Value != "s3cr3t-value" {
		t.Fatalf("expected resolved secret value in response, got %q", resp.Value)
	}
	if len(audited) != 1 {
		t.Fatalf("expected exactly one audit record, got %d", len(audited))
	}
	if audited[0].Action != "restricted_retrieval" || audited[0].Result != "allowed" || audited[0].Resource != "sec-1" {
		t.Fatalf("unexpected audit record: %+v", audited[0])
	}
}

func TestResolveSecretValueFailsClosedWhenSecurestoreUnreachable(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	unreachableURL := upstream.URL
	upstream.Close() // nothing is listening once the request is issued

	s := newResolveTestServer(unreachableURL)
	var audited []AuditRecord
	s.auditHook = func(record AuditRecord) { audited = append(audited, record) }

	identity := requestIdentity{TenantID: "tenant-1", PrincipalID: "principal-1"}
	ref := &SecretReference{ID: "sec-1", Name: "db-password", Provider: "securestore", Ref: "cred-1"}
	req := httptest.NewRequest(http.MethodPost, "/v1/secrets/sec-1/resolve", nil)
	rec := httptest.NewRecorder()
	s.resolveSecretValue(rec, req, identity, ref)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 when securestore is unreachable, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(audited) != 1 || audited[0].Result != "error" {
		t.Fatalf("expected an error audit record for the failed resolve, got %+v", audited)
	}
	// A failed upstream call must never leak a value into the response body.
	if bytes.Contains(rec.Body.Bytes(), []byte("value")) {
		t.Fatalf("fail-closed response must not contain a value field: %s", rec.Body.String())
	}
}

func TestResolveSecretValueFailsClosedOnApprovalRequired(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"credentialId": "cred-1",
			"status":       "approval_required",
		})
	}))
	defer upstream.Close()

	s := newResolveTestServer(upstream.URL)
	identity := requestIdentity{TenantID: "tenant-1", PrincipalID: "principal-1"}
	ref := &SecretReference{ID: "sec-1", Name: "db-password", Provider: "securestore", Ref: "cred-1"}
	req := httptest.NewRequest(http.MethodPost, "/v1/secrets/sec-1/resolve", nil)
	rec := httptest.NewRecorder()
	s.resolveSecretValue(rec, req, identity, ref)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when securestore requires approval, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestResolveSecretValueFailsClosedOnUpstreamErrorStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "denied by policy", http.StatusForbidden)
	}))
	defer upstream.Close()

	s := newResolveTestServer(upstream.URL)
	identity := requestIdentity{TenantID: "tenant-1", PrincipalID: "principal-1"}
	ref := &SecretReference{ID: "sec-1", Name: "db-password", Provider: "securestore", Ref: "cred-1"}
	req := httptest.NewRequest(http.MethodPost, "/v1/secrets/sec-1/resolve", nil)
	rec := httptest.NewRecorder()
	s.resolveSecretValue(rec, req, identity, ref)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 for a non-2xx securestore response, got %d: %s", rec.Code, rec.Body.String())
	}
}
