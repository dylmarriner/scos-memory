//go:build integration

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/auth"
	"github.com/dylmarriner/scos-memory/packages/database"
)

// securityGateIntegrationDBs opens the owner connection (DATABASE_URL, used
// to apply migration 000049 and to seed/clean a throwaway tenant) and the
// deployed scos_app connection (SCOS_APP_TEST_DATABASE_URL) the handlers
// run on, so every query executes under FORCE ROW LEVEL SECURITY.
func securityGateIntegrationDBs(t *testing.T) (admin, app *sql.DB) {
	t.Helper()
	adminURL := os.Getenv("DATABASE_URL")
	appURL := os.Getenv("SCOS_APP_TEST_DATABASE_URL")
	if adminURL == "" || appURL == "" {
		t.Fatal("DATABASE_URL and SCOS_APP_TEST_DATABASE_URL must be set for security-gate integration tests")
	}
	for _, u := range []string{adminURL, appURL} {
		if err := database.RequireTLS(u); err != nil {
			t.Fatalf("integration database URL is not TLS-enabled: %v", err)
		}
	}
	var err error
	if admin, err = sql.Open("pgx", adminURL); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	if app, err = sql.Open("pgx", appURL); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.Close() })

	_, file, _, _ := runtime.Caller(0)
	migration := filepath.Join(filepath.Dir(file), "..", "..", "..", "..", "database", "migrations", "000049_secret_reference_data_class.sql")
	raw, err := os.ReadFile(migration)
	if err != nil {
		t.Fatalf("read migration 000049: %v", err)
	}
	if _, err := admin.Exec(string(raw)); err != nil {
		t.Fatalf("apply migration 000049: %v", err)
	}
	return admin, app
}

type gateFixture struct {
	tenantID, owner, outsider string
}

func seedGateTenant(t *testing.T, admin *sql.DB) gateFixture {
	t.Helper()
	ctx := context.Background()
	var f gateFixture
	if err := admin.QueryRowContext(ctx, `INSERT INTO tenants(name) VALUES ($1) RETURNING id::text`, t.Name()).Scan(&f.tenantID); err != nil {
		t.Fatal(err)
	}
	for _, p := range []*string{&f.owner, &f.outsider} {
		if err := admin.QueryRowContext(ctx, `INSERT INTO principals(tenant_id, kind, external_id, display_name) VALUES ($1,'human',gen_random_uuid()::text,$2) RETURNING id::text`, f.tenantID, t.Name()).Scan(p); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, stmt := range []string{
			`DELETE FROM audit_entries WHERE tenant_id = $1`,
			`DELETE FROM secret_references WHERE tenant_id = $1`,
			`DELETE FROM principals WHERE tenant_id = $1`,
			`DELETE FROM tenants WHERE id = $1`,
		} {
			if _, err := admin.ExecContext(context.Background(), stmt, f.tenantID); err != nil {
				t.Errorf("cleanup %q: %v", stmt, err)
			}
		}
	})
	return f
}

func gateCall(t *testing.T, handler http.HandlerFunc, claims auth.Claims, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var payload bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&payload).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &payload)
	req = req.WithContext(auth.ContextWithClaims(req.Context(), claims))
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

// TestSecretResolveEnforcesScopeAndDataClassAgainstPostgres drives the real
// /v1/secrets and /v1/secrets/{id}/resolve handlers under the scos_app
// role: the stored scope and data class round-trip through the database,
// an out-of-scope caller and an under-cleared caller are refused and both
// refusals land in audit_entries, and the in-scope, cleared caller gets the
// value.
func TestSecretResolveEnforcesScopeAndDataClassAgainstPostgres(t *testing.T) {
	admin, app := securityGateIntegrationDBs(t)
	f := seedGateTenant(t, admin)

	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"credentialId": "cred-1", "value": "live-secret", "version": 4})
	}))
	defer upstream.Close()
	s := newResolveTestServer(upstream.URL)
	s.db = app

	owner := auth.Claims{TenantID: f.tenantID, PrincipalID: f.owner, AgentID: "agent-owner", Scopes: []string{"data_class:confidential"}}
	outsider := auth.Claims{TenantID: f.tenantID, PrincipalID: f.outsider, AgentID: "agent-outsider", Scopes: []string{"data_class:secret"}}
	underCleared := auth.Claims{TenantID: f.tenantID, PrincipalID: f.owner, AgentID: "agent-owner", Scopes: []string{"data_class:internal"}}

	rec := gateCall(t, s.handleSecrets, owner, http.MethodPost, "/v1/secrets", map[string]any{
		"id": "db-password", "name": "db password", "provider": "securestore", "ref": "cred-1",
		"scope": "principal:" + f.owner, "data_class": "confidential",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create secret reference: %d %s", rec.Code, rec.Body.String())
	}
	rec = gateCall(t, s.handleSecretByID, owner, http.MethodGet, "/v1/secrets/db-password", nil)
	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte(`"data_class":"confidential"`)) {
		t.Fatalf("expected stored data_class to round-trip, got %d %s", rec.Code, rec.Body.String())
	}

	for name, claims := range map[string]auth.Claims{"out of scope": outsider, "under-cleared": underCleared} {
		rec = gateCall(t, s.handleSecretByID, claims, http.MethodPost, "/v1/secrets/db-password/resolve", nil)
		if rec.Code != http.StatusForbidden || bytes.Contains(rec.Body.Bytes(), []byte("live-secret")) {
			t.Fatalf("%s: expected 403 without the value, got %d %s", name, rec.Code, rec.Body.String())
		}
	}
	if upstreamCalls != 0 {
		t.Fatalf("denied resolves must not reach securestore, got %d calls", upstreamCalls)
	}

	type auditRow struct {
		actor, decision, dataClass, agentID, reason string
	}
	rows, err := admin.Query(`
		SELECT actor_id::text, decision, details->>'data_class', details->>'agent_id', details->>'reason'
		FROM audit_entries
		WHERE tenant_id = $1 AND action = 'restricted_retrieval' AND resource_id = 'db-password' AND decision = 'denied'
		ORDER BY occurred_at`, f.tenantID)
	if err != nil {
		t.Fatal(err)
	}
	var denials []auditRow
	for rows.Next() {
		var r auditRow
		if err := rows.Scan(&r.actor, &r.decision, &r.dataClass, &r.agentID, &r.reason); err != nil {
			t.Fatal(err)
		}
		denials = append(denials, r)
	}
	rows.Close()
	if len(denials) != 2 {
		t.Fatalf("expected both denied resolves to be audited, got %+v", denials)
	}
	byActor := map[string]auditRow{}
	for _, d := range denials {
		byActor[d.actor] = d
		if d.dataClass != "confidential" {
			t.Fatalf("denial audit must carry the secret's data class, got %+v", d)
		}
	}
	if d := byActor[f.outsider]; d.agentID != "agent-outsider" || d.reason != "caller is outside the secret reference scope" {
		t.Fatalf("unexpected out-of-scope denial audit: %+v", d)
	}
	if d := byActor[f.owner]; d.agentID != "agent-owner" || d.reason != "caller clearance does not cover data class confidential" {
		t.Fatalf("unexpected under-cleared denial audit: %+v", d)
	}

	rec = gateCall(t, s.handleSecretByID, owner, http.MethodPost, "/v1/secrets/db-password/resolve", nil)
	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte("live-secret")) {
		t.Fatalf("expected in-scope cleared caller to resolve, got %d %s", rec.Code, rec.Body.String())
	}
	var allowed int
	if err := admin.QueryRow(`SELECT count(*) FROM audit_entries WHERE tenant_id = $1 AND action = 'restricted_retrieval' AND resource_id = 'db-password' AND decision = 'allowed' AND actor_id = $2 AND details->>'reason' = 'secret value resolved via securestore' AND details->>'data_class' = 'confidential'`, f.tenantID, f.owner).Scan(&allowed); err != nil {
		t.Fatal(err)
	}
	if allowed != 1 {
		t.Fatalf("expected one allowed resolve audit entry, got %d", allowed)
	}

	// A reference created before scopes were enforced (no scope) defaults
	// to the secret class and cannot be resolved by anyone.
	if _, err := admin.Exec(`INSERT INTO secret_references (tenant_id, ref_id, name, provider, ref) VALUES ($1,'legacy','legacy','securestore','cred-legacy')`, f.tenantID); err != nil {
		t.Fatal(err)
	}
	rec = gateCall(t, s.handleSecretByID, auth.Claims{TenantID: f.tenantID, PrincipalID: f.owner, Scopes: []string{"admin"}}, http.MethodPost, "/v1/secrets/legacy/resolve", nil)
	if rec.Code != http.StatusForbidden || !bytes.Contains(rec.Body.Bytes(), []byte("no resolve scope")) {
		t.Fatalf("expected an unscoped legacy reference to be unresolvable, got %d %s", rec.Code, rec.Body.String())
	}
}

// TestAuditRecordStoresCallerFieldsOnlyAsAnnotation proves through the
// database that POST /v1/audit/record cannot forge an audited fact: the
// persisted action, decision, actor and data class are the server's, and
// what the caller asserted is read back only under caller_annotation.
func TestAuditRecordStoresCallerFieldsOnlyAsAnnotation(t *testing.T) {
	admin, app := securityGateIntegrationDBs(t)
	f := seedGateTenant(t, admin)
	s := newResolveTestServer("")
	s.db = app
	caller := auth.Claims{TenantID: f.tenantID, PrincipalID: f.owner, AgentID: "agent-owner", TraceID: "claims-trace"}

	rec := gateCall(t, s.handleAuditRecord, caller, http.MethodPost, "/v1/audit/record", map[string]any{
		"action": "restricted_retrieval", "result": "allowed", "resource": "db-password",
		"data_class": "secret", "reason": "approved by security", "session_id": "sess-1", "trace_id": "forged",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("record annotation: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		AuditID string `json:"audit_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	var action, decision, actor, dataClass string
	if err := admin.QueryRow(`SELECT action, decision, actor_id::text, COALESCE(details->>'data_class','') FROM audit_entries WHERE id = $1`, created.AuditID).Scan(&action, &decision, &actor, &dataClass); err != nil {
		t.Fatal(err)
	}
	if action != "caller_annotation" || decision != "recorded" || actor != f.owner || dataClass != "" {
		t.Fatalf("caller fields were persisted as fact: action=%q decision=%q actor=%q data_class=%q", action, decision, actor, dataClass)
	}
	var forgedAllowed int
	if err := admin.QueryRow(`SELECT count(*) FROM audit_entries WHERE tenant_id = $1 AND action = 'restricted_retrieval' AND decision = 'allowed'`, f.tenantID).Scan(&forgedAllowed); err != nil {
		t.Fatal(err)
	}
	if forgedAllowed != 0 {
		t.Fatalf("a caller annotation must never appear as an allowed restricted_retrieval, found %d", forgedAllowed)
	}

	rec = gateCall(t, s.handleAuditByID, caller, http.MethodGet, "/v1/audit/"+created.AuditID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("read annotation: %d %s", rec.Code, rec.Body.String())
	}
	var read AuditRecord
	if err := json.Unmarshal(rec.Body.Bytes(), &read); err != nil {
		t.Fatal(err)
	}
	if read.Tenant != f.tenantID {
		t.Fatalf("read-back tenant = %q, want the tenant the entry is stored under (%s)", read.Tenant, f.tenantID)
	}
	if read.Action != "caller_annotation" || read.Result != "recorded" || read.AgentID != "agent-owner" || read.TraceID != "claims-trace" || read.DataClass != "" {
		t.Fatalf("unexpected attested fields on read-back: %+v", read)
	}
	a := read.CallerAnnotation
	if a == nil || a.Action != "restricted_retrieval" || a.Result != "allowed" || a.DataClass != "secret" || a.SessionID != "sess-1" || a.TraceID != "forged" {
		t.Fatalf("expected caller assertions under caller_annotation on read-back, got %+v", a)
	}

	// Naming another principal is refused outright, not recorded.
	rec = gateCall(t, s.handleAuditRecord, caller, http.MethodPost, "/v1/audit/record", map[string]any{
		"action": "x", "resource": "r", "principal": f.outsider,
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when the body names another principal, got %d", rec.Code)
	}
}
