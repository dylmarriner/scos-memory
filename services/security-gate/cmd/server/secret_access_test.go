package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

// authorizedResolveCaller is a caller whose signed claims put it inside a
// "tenant"-scoped reference and clear it for the secret class.
func authorizedResolveCaller() secretCaller {
	return secretCaller{
		identity: requestIdentity{TenantID: "tenant-1", PrincipalID: "principal-1"},
		agentID:  "agent-1",
		traceID:  "trace-1",
		scopes:   []string{"data_class:secret"},
		claims:   true,
	}
}

func securestoreStub(t *testing.T, calls *int) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"credentialId": "cred-1", "value": "s3cr3t-value", "version": 1})
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

func TestParseSecretScope(t *testing.T) {
	selectors, err := parseSecretScope("principal:p-1, agent:a-1 tenant")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(selectors) != 3 || selectors[0].kind != "principal" || selectors[0].value != "p-1" ||
		selectors[1].kind != "agent" || selectors[1].value != "a-1" || selectors[2].kind != "tenant" {
		t.Fatalf("unexpected selectors: %+v", selectors)
	}
	if selectors, err := parseSecretScope(""); err != nil || len(selectors) != 0 {
		t.Fatalf("empty scope must parse to no selectors, got %+v, %v", selectors, err)
	}
	for _, bad := range []string{"namespace:n-1", "principal:", "agent:", "everyone", "*"} {
		if _, err := parseSecretScope(bad); err == nil {
			t.Fatalf("expected scope %q to be rejected", bad)
		}
	}
}

func TestCallerClearance(t *testing.T) {
	cases := []struct {
		scopes []string
		want   DataClass
	}{
		{nil, ""},
		{[]string{"memory:read", "memory:write"}, ""},
		{[]string{"data_class:internal"}, ClassInternal},
		{[]string{"data_class:internal", "data_class:restricted", "data_class:confidential"}, ClassRestricted},
		{[]string{"data_class:bogus"}, ""},
		{[]string{"admin"}, ClassSecret},
	}
	for _, tc := range cases {
		if got := (secretCaller{scopes: tc.scopes}).clearance(); got != tc.want {
			t.Fatalf("clearance(%v) = %q, want %q", tc.scopes, got, tc.want)
		}
	}
}

func TestAuthorizeSecretResolve(t *testing.T) {
	base := authorizedResolveCaller()
	cases := []struct {
		name    string
		caller  secretCaller
		ref     SecretReference
		allowed bool
		reason  string
	}{
		{"tenant scope with clearance", base, SecretReference{Scope: "tenant", DataClass: ClassSecret}, true, ""},
		{"principal selector matches", base, SecretReference{Scope: "principal:principal-1", DataClass: ClassSecret}, true, ""},
		{"agent selector matches claims agent", base, SecretReference{Scope: "agent:agent-1", DataClass: ClassConfidential}, true, ""},
		{"other principal", base, SecretReference{Scope: "principal:principal-2", DataClass: ClassInternal}, false, "outside the secret reference scope"},
		{"other agent", base, SecretReference{Scope: "agent:agent-2", DataClass: ClassInternal}, false, "outside the secret reference scope"},
		{"agent selector without claims agent", secretCaller{identity: base.identity, scopes: base.scopes, claims: true}, SecretReference{Scope: "agent:agent-1", DataClass: ClassInternal}, false, "outside the secret reference scope"},
		{"empty scope", base, SecretReference{Scope: "", DataClass: ClassInternal}, false, "no resolve scope"},
		{"malformed stored scope", base, SecretReference{Scope: "everyone", DataClass: ClassInternal}, false, "scope is invalid"},
		{"insufficient clearance", secretCaller{identity: base.identity, scopes: []string{"data_class:confidential"}, claims: true}, SecretReference{Scope: "tenant", DataClass: ClassSecret}, false, "does not cover data class secret"},
		{"no clearance scope", secretCaller{identity: base.identity, scopes: []string{"memory:read"}, claims: true}, SecretReference{Scope: "tenant", DataClass: ClassPublic}, false, "does not cover data class public"},
		{"unknown stored class", base, SecretReference{Scope: "tenant", DataClass: "top-secret"}, false, "unknown data class"},
		{"no signed claims", secretCaller{identity: base.identity}, SecretReference{Scope: "tenant", DataClass: ClassPublic}, false, "signed caller claims"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			allowed, reason := authorizeSecretResolve(tc.caller, &tc.ref)
			if allowed != tc.allowed || !strings.Contains(reason, tc.reason) {
				t.Fatalf("authorizeSecretResolve = (%v, %q), want (%v, ...%q...)", allowed, reason, tc.allowed, tc.reason)
			}
		})
	}
}

// TestResolveSecretValueOutOfScopeDeniedAndAudited is the regression guard
// for the defect where resolve ignored the reference's scope: a caller in
// the right tenant but outside the scope must get 403, Securestore must not
// be contacted, and the denial must be audited with the server-derived
// identity and the reference's data class.
func TestResolveSecretValueOutOfScopeDeniedAndAudited(t *testing.T) {
	calls := 0
	s := newResolveTestServer(securestoreStub(t, &calls).URL)
	var audited []AuditRecord
	s.auditHook = func(record AuditRecord) { audited = append(audited, record) }

	caller := authorizedResolveCaller()
	ref := &SecretReference{ID: "sec-1", Provider: "securestore", Ref: "cred-1", Scope: "principal:someone-else", DataClass: ClassConfidential}
	rec := httptest.NewRecorder()
	s.resolveSecretValue(rec, httptest.NewRequest(http.MethodPost, "/v1/secrets/sec-1/resolve", nil), caller, ref)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for an out-of-scope caller, got %d: %s", rec.Code, rec.Body.String())
	}
	if calls != 0 {
		t.Fatalf("securestore must not be contacted for a denied resolve, got %d calls", calls)
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("s3cr3t-value")) {
		t.Fatal("denied resolve leaked the value")
	}
	if len(audited) != 1 {
		t.Fatalf("expected exactly one audit record for the denial, got %d", len(audited))
	}
	got := audited[0]
	if got.Result != "denied" || got.Action != "restricted_retrieval" || got.Resource != "sec-1" ||
		got.Principal != "principal-1" || got.Tenant != "tenant-1" || got.AgentID != "agent-1" ||
		got.TraceID != "trace-1" || got.DataClass != ClassConfidential || !strings.Contains(got.Reason, "outside the secret reference scope") {
		t.Fatalf("unexpected denial audit record: %+v", got)
	}
}

func TestResolveSecretValueInsufficientClearanceDeniedAndAudited(t *testing.T) {
	calls := 0
	s := newResolveTestServer(securestoreStub(t, &calls).URL)
	var audited []AuditRecord
	s.auditHook = func(record AuditRecord) { audited = append(audited, record) }

	caller := authorizedResolveCaller()
	caller.scopes = []string{"data_class:restricted"}
	ref := &SecretReference{ID: "sec-1", Provider: "securestore", Ref: "cred-1", Scope: "tenant", DataClass: ClassSecret}
	rec := httptest.NewRecorder()
	s.resolveSecretValue(rec, httptest.NewRequest(http.MethodPost, "/v1/secrets/sec-1/resolve", nil), caller, ref)

	if rec.Code != http.StatusForbidden || calls != 0 {
		t.Fatalf("expected 403 without contacting securestore, got %d (%d calls): %s", rec.Code, calls, rec.Body.String())
	}
	if len(audited) != 1 || audited[0].Result != "denied" || audited[0].DataClass != ClassSecret {
		t.Fatalf("expected a denied audit record carrying the secret's data class, got %+v", audited)
	}
}

func TestResolveSecretValueInScopeAllowedAndAudited(t *testing.T) {
	calls := 0
	s := newResolveTestServer(securestoreStub(t, &calls).URL)
	var audited []AuditRecord
	s.auditHook = func(record AuditRecord) { audited = append(audited, record) }

	caller := authorizedResolveCaller()
	caller.scopes = []string{"data_class:confidential"}
	ref := &SecretReference{ID: "sec-1", Provider: "securestore", Ref: "cred-1", Scope: "principal:principal-1", DataClass: ClassConfidential}
	rec := httptest.NewRecorder()
	s.resolveSecretValue(rec, httptest.NewRequest(http.MethodPost, "/v1/secrets/sec-1/resolve", nil), caller, ref)

	if rec.Code != http.StatusOK || calls != 1 {
		t.Fatalf("expected 200 after one securestore call, got %d (%d calls): %s", rec.Code, calls, rec.Body.String())
	}
	if len(audited) != 1 || audited[0].Result != "allowed" || audited[0].AgentID != "agent-1" || audited[0].DataClass != ClassConfidential {
		t.Fatalf("unexpected allowed audit record: %+v", audited)
	}
}

func TestHandleSecretsRejectsInvalidPolicy(t *testing.T) {
	for _, body := range []string{
		`{"id":"sec-1","name":"n","provider":"securestore","ref":"cred-1","scope":"everyone"}`,
		`{"id":"sec-1","name":"n","provider":"securestore","ref":"cred-1","scope":"tenant","data_class":"top-secret"}`,
	} {
		s := newResolveTestServer("")
		req := httptest.NewRequest(http.MethodPost, "/v1/secrets", bytes.NewBufferString(body))
		req.Header.Set("X-SCOS-Tenant-ID", "tenant-1")
		req.Header.Set("X-SCOS-Principal-ID", "principal-1")
		rec := httptest.NewRecorder()
		s.handleSecrets(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for %s, got %d: %s", body, rec.Code, rec.Body.String())
		}
	}
}

func TestValidateSecretReferencePolicyDefaultsToSecret(t *testing.T) {
	ref := SecretReference{Scope: "tenant"}
	if err := validateSecretReferencePolicy(&ref); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if ref.DataClass != ClassSecret {
		t.Fatalf("expected an unset data class to default to secret, got %q", ref.DataClass)
	}
}

// TestHandleAuditRecordDoesNotTrustCallerFields is the regression guard for
// the forgeable audit trail: the stored record's action, decision, actor and
// agent come from the server, and the caller's claims about what happened
// are kept only as a labelled caller annotation.
func TestHandleAuditRecordDoesNotTrustCallerFields(t *testing.T) {
	s := newResolveTestServer("")
	var audited []AuditRecord
	s.auditHook = func(record AuditRecord) { audited = append(audited, record) }

	body := `{"action":"restricted_retrieval","result":"allowed","resource":"sec-9","data_class":"secret","reason":"approved by security team","session_id":"sess-x","trace_id":"forged-trace"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/audit/record", bytes.NewBufferString(body))
	req = req.WithContext(auth.ContextWithClaims(req.Context(), auth.Claims{TenantID: "tenant-1", PrincipalID: "principal-1", AgentID: "agent-1", TraceID: "claims-trace"}))
	rec := httptest.NewRecorder()
	s.handleAuditRecord(rec, req)

	if len(audited) != 1 {
		t.Fatalf("expected one audit record to be built, got %d (status %d: %s)", len(audited), rec.Code, rec.Body.String())
	}
	got := audited[0]
	if got.Action != "caller_annotation" || got.Result != "recorded" || got.DataClass != "" {
		t.Fatalf("caller-chosen action/result/data_class leaked into attested fields: %+v", got)
	}
	if got.Principal != "principal-1" || got.Tenant != "tenant-1" || got.AgentID != "agent-1" || got.TraceID != "claims-trace" {
		t.Fatalf("expected identity fields from verified claims, got %+v", got)
	}
	if got.Timestamp != "" {
		t.Fatalf("timestamp must be assigned by the database, not the request: %q", got.Timestamp)
	}
	a := got.CallerAnnotation
	if a == nil || a.Action != "restricted_retrieval" || a.Result != "allowed" || a.DataClass != "secret" ||
		a.Reason != "approved by security team" || a.SessionID != "sess-x" || a.TraceID != "forged-trace" {
		t.Fatalf("expected the caller's assertions under caller_annotation, got %+v", a)
	}
}

func TestHandleAuditRecordRejectsMismatchedIdentityFields(t *testing.T) {
	claims := auth.Claims{TenantID: "tenant-1", PrincipalID: "principal-1", AgentID: "agent-1"}
	for _, body := range []string{
		`{"action":"x","resource":"r","principal":"victim"}`,
		`{"action":"x","resource":"r","tenant":"other-tenant"}`,
		`{"action":"x","resource":"r","agent_id":"agent-2"}`,
	} {
		s := newResolveTestServer("")
		var audited []AuditRecord
		s.auditHook = func(record AuditRecord) { audited = append(audited, record) }
		req := httptest.NewRequest(http.MethodPost, "/v1/audit/record", bytes.NewBufferString(body))
		req = req.WithContext(auth.ContextWithClaims(req.Context(), claims))
		rec := httptest.NewRecorder()
		s.handleAuditRecord(rec, req)
		if rec.Code != http.StatusForbidden || len(audited) != 0 {
			t.Fatalf("expected 403 and no record for %s, got %d with %d records", body, rec.Code, len(audited))
		}
	}
}

func TestHandleAuditRecordRequiresActionAndResource(t *testing.T) {
	s := newResolveTestServer("")
	req := httptest.NewRequest(http.MethodPost, "/v1/audit/record", bytes.NewBufferString(`{"action":"x"}`))
	req = req.WithContext(auth.ContextWithClaims(req.Context(), auth.Claims{TenantID: "tenant-1", PrincipalID: "principal-1"}))
	rec := httptest.NewRecorder()
	s.handleAuditRecord(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without a resource, got %d", rec.Code)
	}
}
