package main

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dylmarriner/scos-memory/packages/auth"
	hooksdk "github.com/dylmarriner/scos-memory/packages/hook-sdk"
)

const (
	hookSecret     = "agent-registry-hook-secret"
	hookTenant     = "11111111-1111-4111-8111-111111111111"
	hookPrincipal  = "22222222-2222-4222-8222-222222222222"
	otherPrincipal = "55555555-5555-4555-8555-555555555555"
	hookAgentID    = "33333333-3333-4333-8333-333333333333"
	legacyAgentID  = "66666666-6666-4666-8666-666666666666"
)

// fakeOwner is the registered_by_principal_id of the single agent the fake
// database holds: a principal id, or nil for a legacy row.
var fakeOwner driver.Value = hookPrincipal

// registryDriver is a minimal database/sql driver so the real handlers run
// end to end without Postgres. It mirrors the SQL's ownership filter
// (admin, legacy row, or owner) so the handler's 403/404 wiring is covered;
// the SQL itself is exercised against Postgres in registry_pg_test.go.
type registryDriver struct{}

func (registryDriver) Open(string) (driver.Conn, error) { return registryConn{}, nil }

type registryConn struct{}

func (registryConn) Prepare(q string) (driver.Stmt, error) { return registryStmt(q), nil }
func (registryConn) Close() error                          { return nil }
func (registryConn) Begin() (driver.Tx, error)             { return registryConn{}, nil }
func (registryConn) Commit() error                         { return nil }
func (registryConn) Rollback() error                       { return nil }

type registryStmt string

func (registryStmt) Close() error                               { return nil }
func (registryStmt) NumInput() int                              { return -1 }
func (registryStmt) Exec([]driver.Value) (driver.Result, error) { return driver.RowsAffected(1), nil }
func (q registryStmt) Query(args []driver.Value) (driver.Rows, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	operable := func() bool { return args[2] == true || fakeOwner == nil || fakeOwner == args[3] }
	switch {
	case strings.Contains(string(q), "INSERT INTO agent_registrations"):
		return rowsOf([]driver.Value{hookAgentID, now}), nil
	case strings.Contains(string(q), "agent_registrations_expire_stale"):
		return rowsOf([]driver.Value{hookAgentID, hookTenant, hookPrincipal}, []driver.Value{legacyAgentID, hookTenant, nil}), nil
	case strings.Contains(string(q), "SELECT EXISTS"):
		return rowsOf([]driver.Value{true}), nil
	case strings.Contains(string(q), "SET status='active'") && operable():
		return rowsOf([]driver.Value{now, "active", time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)}), nil
	case strings.Contains(string(q), "SET status='inactive'") && operable():
		return rowsOf([]driver.Value{"active"}), nil
	}
	return rowsOf(), nil
}

type registryRows struct{ rows [][]driver.Value }

func rowsOf(rows ...[]driver.Value) *registryRows { return &registryRows{rows: rows} }

func (r *registryRows) Columns() []string {
	if len(r.rows) == 0 {
		return []string{""}
	}
	return make([]string, len(r.rows[0]))
}
func (r *registryRows) Close() error { return nil }
func (r *registryRows) Next(dest []driver.Value) error {
	if len(r.rows) == 0 {
		return io.EOF
	}
	copy(dest, r.rows[0])
	r.rows = r.rows[1:]
	return nil
}

var registerRegistryDriver sync.Once

func hookedRegistry(t *testing.T, extensionManagerURL string, owner driver.Value) *agentRegistryServer {
	t.Helper()
	registerRegistryDriver.Do(func() { sql.Register("agent-registry-fake", registryDriver{}) })
	fakeOwner = owner
	db, err := sql.Open("agent-registry-fake", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &agentRegistryServer{db: db, hooks: hooksdk.NewClient("agent-registry", extensionManagerURL, hookSecret, nil)}
}

// recordingExtensionManager verifies bearers the way extension-manager does
// and forwards each dispatch body, annotated with the verified claims.
func recordingExtensionManager(t *testing.T, status int) (*httptest.Server, <-chan map[string]any) {
	t.Helper()
	got := make(chan map[string]any, 64)
	srv := httptest.NewServer(auth.AuthenticatedHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, _ := auth.CanonicalClaims(r.Context())
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		body["claims_tenant"], body["claims_principal"], body["claims_subject"] = claims.TenantID, claims.PrincipalID, claims.Subject
		got <- body
		if status != http.StatusOK {
			http.Error(w, "extension failed", status)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": []any{}})
	}), auth.Config{IssuerSecret: hookSecret, Identity: "extension-manager"}))
	t.Cleanup(srv.Close)
	return srv, got
}

// caller builds verified claims for principal in the test tenant.
func caller(principal string, scopes ...string) auth.Claims {
	return auth.Claims{TenantID: hookTenant, PrincipalID: principal, Subject: "user-7", Scopes: scopes, ExpiresAt: time.Now().Add(time.Minute)}
}

// callRegistry drives handler behind the real auth middleware with a signed
// bearer for claims, as a production request would arrive.
func callRegistry(t *testing.T, handler http.HandlerFunc, claims auth.Claims, body any) (int, string) {
	t.Helper()
	token, err := auth.SignClaims(claims, hookSecret)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/agents", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	auth.AuthenticatedHandler(handler, auth.Config{IssuerSecret: hookSecret, Identity: "agent-registry"}).ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func nextEvent(t *testing.T, got <-chan map[string]any) map[string]any {
	t.Helper()
	select {
	case body := <-got:
		return body
	case <-time.After(5 * time.Second):
		t.Fatal("lifecycle event was not dispatched")
		return nil
	}
}

func assertNoEvent(t *testing.T, got <-chan map[string]any) {
	t.Helper()
	select {
	case body := <-got:
		t.Fatalf("unexpected lifecycle event: %+v", body)
	case <-time.After(200 * time.Millisecond):
	}
}

func assertAgentEvent(t *testing.T, body map[string]any, event, agentID, reason string) {
	t.Helper()
	payload, _ := body["payload"].(map[string]any)
	if body["event"] != event || body["tenant_id"] != hookTenant || body["principal_id"] != hookPrincipal ||
		body["claims_tenant"] != hookTenant || body["claims_principal"] != hookPrincipal ||
		payload["agent_id"] != agentID || payload["reason"] != reason {
		t.Fatalf("want %s/%s for %s, got %+v", event, reason, agentID, body)
	}
}

func completeRegistration() AgentRegistration {
	return AgentRegistration{
		Name: "coder-agent", Version: "1.0", Maintainer: "scos-team",
		Capabilities: []string{"code-generation"}, Inputs: []string{"repository-context"}, Outputs: []string{"patch"},
		ModelProvider: "anthropic", CostPolicy: "standard", HealthEndpoint: "http://coder-agent/health/live",
	}
}

func TestAgentLifecycleHooksFollowPersistence(t *testing.T) {
	em, got := recordingExtensionManager(t, http.StatusOK)
	s := hookedRegistry(t, em.URL, hookPrincipal)
	owner := caller(hookPrincipal)

	if code, body := callRegistry(t, s.handleAgentRegister, owner, completeRegistration()); code != http.StatusOK {
		t.Fatalf("register: %d %s", code, body)
	}
	assertAgentEvent(t, nextEvent(t, got), hooksdk.OnAgentStart, hookAgentID, "registered")

	if code, body := callRegistry(t, s.handleAgentHeartbeat, owner, map[string]string{"agent_id": hookAgentID}); code != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", code, body)
	}
	assertAgentEvent(t, nextEvent(t, got), hooksdk.OnAgentStart, hookAgentID, "reactivated")

	if code, body := callRegistry(t, s.handleAgentDeregister, owner, map[string]string{"agent_id": hookAgentID}); code != http.StatusOK {
		t.Fatalf("deregister: %d %s", code, body)
	}
	assertAgentEvent(t, nextEvent(t, got), hooksdk.OnAgentStop, hookAgentID, "deregistered")
}

func TestFailingExtensionManagerDoesNotFailAgentRegistration(t *testing.T) {
	em, got := recordingExtensionManager(t, http.StatusServiceUnavailable)
	s := hookedRegistry(t, em.URL, hookPrincipal)

	if code, body := callRegistry(t, s.handleAgentRegister, caller(hookPrincipal), completeRegistration()); code != http.StatusOK {
		t.Fatalf("register failed with a failing extension manager: %d %s", code, body)
	}
	assertAgentEvent(t, nextEvent(t, got), hooksdk.OnAgentStart, hookAgentID, "registered")

	if code, body := callRegistry(t, s.handleAgentDeregister, caller(hookPrincipal), map[string]string{"agent_id": hookAgentID}); code != http.StatusOK {
		t.Fatalf("deregister failed with a failing extension manager: %d %s", code, body)
	}
	assertAgentEvent(t, nextEvent(t, got), hooksdk.OnAgentStop, hookAgentID, "deregistered")
}

func TestRegisterRequiresPrincipalID(t *testing.T) {
	s := hookedRegistry(t, "", hookPrincipal)
	if code, body := callRegistry(t, s.handleAgentRegister, caller("user-7"), completeRegistration()); code != http.StatusBadRequest {
		t.Fatalf("non-UUID principal registered an agent: %d %s", code, body)
	}
}

func TestHeartbeatAndDeregisterRequireOwnerOrAdmin(t *testing.T) {
	em, got := recordingExtensionManager(t, http.StatusOK)
	s := hookedRegistry(t, em.URL, otherPrincipal)
	stranger := caller(hookPrincipal)
	agent := map[string]string{"agent_id": hookAgentID}

	for name, h := range map[string]http.HandlerFunc{"heartbeat": s.handleAgentHeartbeat, "deregister": s.handleAgentDeregister} {
		if code, body := callRegistry(t, h, stranger, agent); code != http.StatusForbidden {
			t.Fatalf("%s by non-owner: %d %s", name, code, body)
		}
		if code, body := callRegistry(t, h, caller(hookPrincipal, "agents:admin"), agent); code != http.StatusOK {
			t.Fatalf("%s by agents:admin: %d %s", name, code, body)
		}
		if code, body := callRegistry(t, h, caller(hookPrincipal, "admin"), agent); code != http.StatusOK {
			t.Fatalf("%s by global admin: %d %s", name, code, body)
		}
	}
	for range 4 { // two starts, two stops from the admin calls
		nextEvent(t, got)
	}

	// Legacy rows with no recorded principal keep their prior tenant-wide access.
	fakeOwner = nil
	if code, body := callRegistry(t, s.handleAgentHeartbeat, stranger, agent); code != http.StatusOK {
		t.Fatalf("heartbeat on legacy row: %d %s", code, body)
	}
}

func TestStaleSweepRaisesStopOncePerOwnedAgent(t *testing.T) {
	em, got := recordingExtensionManager(t, http.StatusOK)
	s := hookedRegistry(t, em.URL, hookPrincipal)

	n, err := s.sweepStaleAgents(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("sweep = %d, %v; want 2 agents marked inactive", n, err)
	}
	body := nextEvent(t, got)
	assertAgentEvent(t, body, hooksdk.OnAgentStop, hookAgentID, "stale")
	if body["claims_subject"] != "service:agent-registry" {
		t.Fatalf("sweep event should carry claims minted for the service, got subject %v", body["claims_subject"])
	}
	assertNoEvent(t, got) // the legacy row has no principal: skipped, logged
}
