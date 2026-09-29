package main

import (
	"bytes"
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

// journalDriver is a minimal database/sql driver so the real journal
// handlers run end to end without Postgres: every statement succeeds, a
// RETURNING of last_sequence yields 1, a RETURNING id echoes the first
// argument, and every other query returns no rows (no idempotent replay, no
// prior event).
type journalDriver struct{}

func (journalDriver) Open(string) (driver.Conn, error) { return journalConn{}, nil }

type journalConn struct{}

func (journalConn) Prepare(q string) (driver.Stmt, error) { return journalStmt(q), nil }
func (journalConn) Close() error                          { return nil }
func (journalConn) Begin() (driver.Tx, error)             { return journalConn{}, nil }
func (journalConn) Commit() error                         { return nil }
func (journalConn) Rollback() error                       { return nil }

type journalStmt string

func (journalStmt) Close() error                               { return nil }
func (journalStmt) NumInput() int                              { return -1 }
func (journalStmt) Exec([]driver.Value) (driver.Result, error) { return driver.RowsAffected(1), nil }
func (q journalStmt) Query(args []driver.Value) (driver.Rows, error) {
	switch {
	case strings.Contains(string(q), "RETURNING last_sequence"):
		return &journalRows{row: []driver.Value{int64(1)}}, nil
	case strings.Contains(string(q), "RETURNING id"):
		return &journalRows{row: []driver.Value{args[0]}}, nil
	}
	return &journalRows{}, nil
}

type journalRows struct{ row []driver.Value }

func (r *journalRows) Columns() []string { return make([]string, max(len(r.row), 1)) }
func (r *journalRows) Close() error      { return nil }
func (r *journalRows) Next(dest []driver.Value) error {
	if r.row == nil {
		return io.EOF
	}
	copy(dest, r.row)
	r.row = nil
	return nil
}

var registerJournalDriver sync.Once

const (
	hookSecret    = "memory-gateway-hook-secret"
	hookTenant    = "11111111-1111-4111-8111-111111111111"
	hookPrincipal = "22222222-2222-4222-8222-222222222222"
	hookAgent     = "33333333-3333-4333-8333-333333333333"
)

// hookedGateway serves the real routes behind the real auth middleware, with
// hooks dispatched to extensionManagerURL.
func hookedGateway(t *testing.T, extensionManagerURL string) http.Handler {
	t.Helper()
	registerJournalDriver.Do(func() { sql.Register("memory-gateway-journal", journalDriver{}) })
	db, err := sql.Open("memory-gateway-journal", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	validator, err := newEventValidator("")
	if err != nil {
		t.Fatal(err)
	}
	s := server{db: db, validator: validator, hooks: hooksdk.NewClient("memory-gateway", extensionManagerURL, hookSecret, nil)}
	return auth.AuthenticatedHandler(s.routes(), auth.Config{IssuerSecret: hookSecret, Identity: "memory-gateway"})
}

// recordingExtensionManager verifies bearers the way extension-manager does
// and forwards each dispatch body, annotated with the verified claims.
func recordingExtensionManager(t *testing.T, status int) (*httptest.Server, <-chan map[string]any) {
	t.Helper()
	got := make(chan map[string]any, 8)
	srv := httptest.NewServer(auth.AuthenticatedHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, _ := auth.CanonicalClaims(r.Context())
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		body["claims_tenant"], body["claims_principal"] = claims.TenantID, claims.PrincipalID
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

func callGateway(t *testing.T, h http.Handler, path string, body any) (int, map[string]any) {
	t.Helper()
	token, err := auth.SignClaims(auth.Claims{TenantID: hookTenant, PrincipalID: hookPrincipal, AgentID: hookAgent, Subject: "user-7", ExpiresAt: time.Now().Add(time.Minute)}, hookSecret)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Idempotency-Key", path)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
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

func assertEvent(t *testing.T, body map[string]any, event string, want map[string]any) {
	t.Helper()
	if body["event"] != event || body["tenant_id"] != hookTenant || body["principal_id"] != hookPrincipal ||
		body["claims_tenant"] != hookTenant || body["claims_principal"] != hookPrincipal {
		t.Fatalf("%s: wrong event or identity: %+v", event, body)
	}
	payload, _ := body["payload"].(map[string]any)
	for k, v := range want {
		if payload[k] != v {
			t.Fatalf("%s: payload[%q] = %v, want %v (payload %+v)", event, k, payload[k], v, payload)
		}
	}
	if _, leaked := payload["content"]; leaked {
		t.Fatalf("%s: payload carries message content: %+v", event, payload)
	}
}

func TestSessionAndTurnLifecycleHooksFollowJournalWrites(t *testing.T) {
	em, got := recordingExtensionManager(t, http.StatusOK)
	h := hookedGateway(t, em.URL)

	code, session := callGateway(t, h, "/v1/sessions", map[string]string{"namespace_id": "44444444-4444-4444-8444-444444444444"})
	if code != http.StatusCreated {
		t.Fatalf("create session: %d %v", code, session)
	}
	sessionID, _ := session["id"].(string)
	assertEvent(t, nextEvent(t, got), hooksdk.OnSessionStart, map[string]any{"session_id": sessionID, "agent_id": hookAgent})

	code, turn := callGateway(t, h, "/v1/sessions/"+sessionID+"/messages", map[string]string{"role": "user", "content": "secret prompt text"})
	if code != http.StatusCreated {
		t.Fatalf("append message: %d %v", code, turn)
	}
	assertEvent(t, nextEvent(t, got), hooksdk.OnTurnReceived, map[string]any{
		"session_id": sessionID, "turn_id": turn["turn_id"], "message_id": turn["message_id"], "agent_id": hookAgent, "role": "user",
	})

	code, ended := callGateway(t, h, "/v1/sessions/"+sessionID+"/end", nil)
	if code != http.StatusOK || ended["status"] != "ended" {
		t.Fatalf("end session: %d %v", code, ended)
	}
	assertEvent(t, nextEvent(t, got), hooksdk.OnSessionEnd, map[string]any{"session_id": sessionID, "agent_id": hookAgent})
}

func TestFailingExtensionManagerDoesNotFailJournalWrites(t *testing.T) {
	em, got := recordingExtensionManager(t, http.StatusServiceUnavailable)
	h := hookedGateway(t, em.URL)

	code, session := callGateway(t, h, "/v1/sessions", map[string]string{"namespace_id": "44444444-4444-4444-8444-444444444444"})
	if code != http.StatusCreated {
		t.Fatalf("create session failed with a failing extension manager: %d %v", code, session)
	}
	assertEvent(t, nextEvent(t, got), hooksdk.OnSessionStart, map[string]any{"session_id": session["id"]})

	code, turn := callGateway(t, h, "/v1/sessions/"+session["id"].(string)+"/messages", map[string]string{"role": "user", "content": "hi"})
	if code != http.StatusCreated {
		t.Fatalf("append message failed with a failing extension manager: %d %v", code, turn)
	}
	nextEvent(t, got)
}
