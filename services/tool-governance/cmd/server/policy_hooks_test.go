package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dylmarriner/scos-memory/packages/auth"
	hooksdk "github.com/dylmarriner/scos-memory/packages/hook-sdk"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// These drive the real register/transition handlers against a migrated,
// disposable Postgres. Set SCOS_TOOL_GOVERNANCE_TEST_DSN to run them, e.g.
//
//	SCOS_TOOL_GOVERNANCE_TEST_DSN=postgres://scos:test@127.0.0.1:25432/scos_memory?sslmode=disable

const policyHookSecret = "issuer-secret-for-policy-hook-tests"

func testGovernanceDB(t *testing.T) (*sql.DB, requestIdentity) {
	t.Helper()
	dsn := os.Getenv("SCOS_TOOL_GOVERNANCE_TEST_DSN")
	if dsn == "" {
		t.Skip("SCOS_TOOL_GOVERNANCE_TEST_DSN not set; skipping tool-governance database test")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	var id requestIdentity
	if err := db.QueryRowContext(ctx, `INSERT INTO tenants (name) VALUES ($1) RETURNING id::text`, t.Name()).Scan(&id.TenantID); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	if err := db.QueryRowContext(ctx, `INSERT INTO principals (tenant_id, kind, external_id, display_name) VALUES ($1,'human',$2,$2) RETURNING id::text`,
		id.TenantID, t.Name()).Scan(&id.PrincipalID); err != nil {
		t.Fatalf("create principal: %v", err)
	}
	return db, id
}

// fakeExtensionManager verifies the dispatch bearer the way
// extension-manager does and records each accepted event.
func fakeExtensionManager(t *testing.T) (*hooksdk.Client, <-chan map[string]any) {
	t.Helper()
	got := make(chan map[string]any, 4)
	cfg := auth.Config{Token: "internal", IssuerSecret: policyHookSecret, Identity: "internal-service"}
	srv := httptest.NewServer(auth.AuthenticatedHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, ok := auth.CanonicalClaims(r.Context())
		if !ok {
			http.Error(w, "no claims", http.StatusUnauthorized)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		body["claims_tenant"] = claims.TenantID
		got <- body
		_ = json.NewEncoder(w).Encode(map[string]any{"results": []hooksdk.DispatchResult{{ExtensionID: "ext-policy", Status: "succeeded"}}})
	}), cfg))
	t.Cleanup(srv.Close)
	return hooksdk.NewClient("tool-governance", srv.URL, policyHookSecret, srv.Client()), got
}

func governedRequest(t *testing.T, handler http.HandlerFunc, path, key string, id requestIdentity, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	r.Header.Set("Idempotency-Key", key)
	r.Header.Set("X-SCOS-Tenant-ID", id.TenantID)
	r.Header.Set("X-SCOS-Principal-ID", id.PrincipalID)
	w := httptest.NewRecorder()
	handler(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

func expectPolicyChange(t *testing.T, got <-chan map[string]any, id requestIdentity, want map[string]any) {
	t.Helper()
	var body map[string]any
	select {
	case body = <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("on_policy_change was not dispatched")
	}
	if body["event"] != hooksdk.OnPolicyChange || body["tenant_id"] != id.TenantID || body["claims_tenant"] != id.TenantID || body["principal_id"] != id.PrincipalID {
		t.Fatalf("dispatch envelope = %+v", body)
	}
	payload, _ := body["payload"].(map[string]any)
	for k, v := range want {
		if payload[k] != v {
			t.Fatalf("payload[%s] = %v, want %v (payload %+v)", k, payload[k], v, payload)
		}
	}
}

func TestToolRegisterAndTransitionDispatchOnPolicyChange(t *testing.T) {
	db, id := testGovernanceDB(t)
	hooks, got := fakeExtensionManager(t)
	s := &toolGovernanceServer{db: db, hooks: hooks}

	w, out := governedRequest(t, s.handleToolRegister, "/v1/tools/register", "reg-"+t.Name(), id, map[string]any{"name": "deploy", "version": "2.1"})
	if w.Code != http.StatusOK {
		t.Fatalf("register: %d: %s", w.Code, w.Body.String())
	}
	toolID, _ := out["id"].(string)
	if toolID == "" || out["state"] != string(StateProposed) {
		t.Fatalf("response lost the tool fields: %+v", out)
	}
	expectPolicyChange(t, got, id, map[string]any{"policy_id": toolID, "version": "2.1", "change_type": policyCreated, "actor": id.PrincipalID, "state": "proposed"})
	hooksOut, _ := out["lifecycle_hooks"].(map[string]any)
	if results, _ := hooksOut["results"].([]any); len(results) != 1 || hooksOut["error"] != nil {
		t.Fatalf("dispatch results not reported: %+v", out)
	}

	w, out = governedRequest(t, s.handleToolTransition, "/v1/tools/transition", "tr-"+t.Name(), id, map[string]any{"tool_id": toolID, "new_state": "prepared"})
	if w.Code != http.StatusOK || out["state"] != "prepared" {
		t.Fatalf("transition: %d: %s", w.Code, w.Body.String())
	}
	expectPolicyChange(t, got, id, map[string]any{"policy_id": toolID, "version": "2.1", "change_type": policyUpdated, "actor": id.PrincipalID, "state": "prepared", "previous_state": "proposed"})

	// A replayed request changes nothing and so raises nothing.
	governedRequest(t, s.handleToolTransition, "/v1/tools/transition", "tr-"+t.Name(), id, map[string]any{"tool_id": toolID, "new_state": "prepared"})
	select {
	case body := <-got:
		t.Fatalf("idempotent replay dispatched %+v", body)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestPolicyChangeCommitsAndReportsWhenExtensionManagerFails(t *testing.T) {
	db, id := testGovernanceDB(t)
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "extension manager down", http.StatusServiceUnavailable)
	}))
	defer failing.Close()
	s := &toolGovernanceServer{db: db, hooks: hooksdk.NewClient("tool-governance", failing.URL, policyHookSecret, failing.Client())}

	w, out := governedRequest(t, s.handleToolRegister, "/v1/tools/register", "reg-"+t.Name(), id, map[string]any{"name": "deploy", "version": "1"})
	if w.Code != http.StatusOK {
		t.Fatalf("an authorised change must not fail on hook delivery: %d: %s", w.Code, w.Body.String())
	}
	hooksOut, _ := out["lifecycle_hooks"].(map[string]any)
	if msg, _ := hooksOut["error"].(string); !strings.Contains(msg, "extension manager down") {
		t.Fatalf("dispatch failure not reported: %+v", out)
	}
	var state string
	if err := db.QueryRow(`SELECT state::text FROM tool_definitions WHERE id = $1 AND tenant_id = $2`, out["id"], id.TenantID).Scan(&state); err != nil || state != "proposed" {
		t.Fatalf("policy change was not kept: state=%q err=%v", state, err)
	}
}

func TestPolicyChangeWithoutExtensionManagerKeepsResponseShape(t *testing.T) {
	var s toolGovernanceServer
	if hooks := s.dispatchPolicyChange(context.Background(), requestIdentity{TenantID: "t", PrincipalID: "p"}, &ToolDefinition{ID: "x"}, policyCreated, ""); hooks != nil {
		t.Fatalf("nil client reported %+v", hooks)
	}
	raw, _ := json.Marshal(governedToolResponse{ToolDefinition: &ToolDefinition{ID: "x", Name: "n"}})
	if strings.Contains(string(raw), "lifecycle_hooks") || !strings.Contains(string(raw), `"id":"x"`) {
		t.Fatalf("response shape changed: %s", raw)
	}
}
