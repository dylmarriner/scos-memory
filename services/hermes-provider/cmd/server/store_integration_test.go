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
	"sync"
	"testing"
	"time"

	hooksdk "github.com/dylmarriner/scos-memory/packages/hook-sdk"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// Database-backed §24 lifecycle tests. SCOS_HERMES_STORE_TEST_DSN is an
// owner connection to a migrated database (used to seed fixtures). When
// SCOS_HERMES_APP_TEST_DSN is also set, the handlers run as that restricted
// role, exactly as in production, so row-level security is exercised too.

type hermesFixture struct {
	srv                          *hermesServer
	owner                        *sql.DB
	tenant, principal, namespace string
}

func newHermesFixture(t *testing.T) *hermesFixture {
	t.Helper()
	dsn := os.Getenv("SCOS_HERMES_STORE_TEST_DSN")
	if dsn == "" {
		t.Skip("SCOS_HERMES_STORE_TEST_DSN not set; skipping hermes-provider PostgreSQL integration test")
	}
	owner, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	handlerDB := owner
	if app := os.Getenv("SCOS_HERMES_APP_TEST_DSN"); app != "" {
		if handlerDB, err = sql.Open("pgx", app); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = handlerDB.Close() })
	}
	f := &hermesFixture{srv: newHermesServer(), owner: owner}
	f.srv.db = handlerDB
	ctx := context.Background()
	if err := owner.QueryRowContext(ctx, `INSERT INTO tenants(name) VALUES ($1) RETURNING id`, t.Name()).Scan(&f.tenant); err != nil {
		t.Fatal(err)
	}
	if err := owner.QueryRowContext(ctx, `INSERT INTO principals(tenant_id,kind,external_id,display_name) VALUES ($1,'human',$2,$2) RETURNING id`, f.tenant, t.Name()).Scan(&f.principal); err != nil {
		t.Fatal(err)
	}
	if err := owner.QueryRowContext(ctx, `INSERT INTO namespaces(tenant_id,owner_principal_id,canonical_name) VALUES ($1,$2,'hermes') RETURNING id`, f.tenant, f.principal).Scan(&f.namespace); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.ExecContext(ctx, `INSERT INTO namespace_memberships(namespace_id,principal_id,can_read,can_write_candidates) VALUES ($1,$2,true,true)`, f.namespace, f.principal); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *hermesFixture) call(t *testing.T, handler http.HandlerFunc, body any) map[string]any {
	t.Helper()
	encoded, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(encoded))
	r.Header.Set("X-SCOS-Tenant-ID", f.tenant)
	r.Header.Set("X-SCOS-Principal-ID", f.principal)
	w := httptest.NewRecorder()
	handler(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPGSaveConfigPersistsForTheCaller(t *testing.T) {
	f := newHermesFixture(t)
	cfg := map[string]any{"base_url": "https://scos.internal", "tenant_id": f.tenant, "principal_id": f.principal, "profile_scope": "work"}
	if out := f.call(t, f.srv.handleSaveConfig, cfg); out["saved"] != true {
		t.Fatalf("save: %v", out)
	}
	cfg["base_url"] = "https://scos.internal/v2"
	f.call(t, f.srv.handleSaveConfig, cfg)
	var stored string
	var rows int
	if err := f.owner.QueryRow(`SELECT count(*), max(config->>'base_url') FROM hermes_provider_configs WHERE tenant_id=$1 AND principal_id=$2 AND profile_scope='work'`, f.tenant, f.principal).Scan(&rows, &stored); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || stored != "https://scos.internal/v2" {
		t.Fatalf("expected one upserted row with the latest config, got %d rows, %q", rows, stored)
	}
}

func TestPGSyncTurnKeepsTheClientTurnAndToolCalls(t *testing.T) {
	f := newHermesFixture(t)
	session := newTestSession(t)
	body := map[string]any{
		"namespace_id": f.namespace, "session_id": session, "turn_id": "hermes-turn-7",
		"messages":   []any{map[string]any{"role": "user", "content": "deploy the api"}},
		"tool_calls": []any{map[string]any{"name": "shell", "arguments": map[string]any{"cmd": "make deploy"}}},
	}
	out := f.call(t, f.srv.handleSyncTurn, body)
	want := derivedTurnID(session, "hermes-turn-7")
	if out["turn_id"] != want || out["client_turn_id"] != "hermes-turn-7" {
		t.Fatalf("stored turn must be derived from the client turn: %v", out)
	}
	var roles []string
	rows, err := f.owner.Query(`SELECT role FROM messages WHERE turn_id=$1 ORDER BY committed_at, role`, want)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var r string
		_ = rows.Scan(&r)
		roles = append(roles, r)
	}
	rows.Close()
	if len(roles) != 2 || !(hasValue(roles, "user") && hasValue(roles, "tool")) {
		t.Fatalf("tool calls must be journalled with the turn, got roles %v", roles)
	}
	if dup := f.call(t, f.srv.handleSyncTurn, body); dup["duplicate"] != true {
		t.Fatalf("a replayed sync must be idempotent: %v", dup)
	}
}

func TestPGPrefetchServesWarmRecallUntilANewTurn(t *testing.T) {
	f := newHermesFixture(t)
	session := newTestSession(t)
	f.call(t, f.srv.handleSyncTurn, map[string]any{"namespace_id": f.namespace, "session_id": session, "turn_id": "t1",
		"messages": []any{map[string]any{"role": "user", "content": "first"}}})
	prefetch := map[string]any{"namespace_id": f.namespace, "session_id": session}

	if out := f.call(t, f.srv.handlePrefetch, prefetch); out["cache_hit"] != false {
		t.Fatalf("nothing warmed yet, so prefetch must compute live: %v", out)
	}
	f.call(t, f.srv.handleQueuePrefetch, map[string]any{"session_id": session})
	if err := f.srv.warm(context.Background(), f.srv.prefetchQueue.drain()[0]); err != nil {
		t.Fatal(err)
	}
	if out := f.call(t, f.srv.handlePrefetch, prefetch); out["cache_hit"] != true || len(out["recall"].([]any)) != 1 {
		t.Fatalf("warmed recall must be served from the cache: %v", out)
	}
	f.call(t, f.srv.handleSyncTurn, map[string]any{"namespace_id": f.namespace, "session_id": session, "turn_id": "t2",
		"messages": []any{map[string]any{"role": "user", "content": "second"}}})
	if out := f.call(t, f.srv.handlePrefetch, prefetch); out["cache_hit"] != false || len(out["recall"].([]any)) != 2 {
		t.Fatalf("a new turn must invalidate warmed recall: %v", out)
	}
}

func TestPGSessionIDOwnedByAnotherTenantIsRefused(t *testing.T) {
	a, b := newHermesFixture(t), newHermesFixture(t)
	session := newTestSession(t)
	a.call(t, a.srv.handleSyncTurn, map[string]any{"namespace_id": a.namespace, "session_id": session, "turn_id": "t1",
		"messages": []any{map[string]any{"role": "user", "content": "mine"}}})
	encoded, _ := json.Marshal(map[string]any{"namespace_id": b.namespace, "session_id": session, "turn_id": "t1",
		"messages": []any{map[string]any{"role": "user", "content": "theirs"}}})
	r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(encoded))
	r.Header.Set("X-SCOS-Tenant-ID", b.tenant)
	r.Header.Set("X-SCOS-Principal-ID", b.principal)
	w := httptest.NewRecorder()
	b.srv.handleSyncTurn(w, r)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "unavailable") {
		t.Fatalf("another tenant's session id must be refused clearly, got %d %s", w.Code, w.Body.String())
	}
}

func TestPGShutdownFlushesQueuedWork(t *testing.T) {
	f := newHermesFixture(t)
	session := newTestSession(t)
	f.call(t, f.srv.handleSyncTurn, map[string]any{"namespace_id": f.namespace, "session_id": session, "turn_id": "t1",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}}})
	f.call(t, f.srv.handleQueuePrefetch, map[string]any{"session_id": session})
	out := f.call(t, f.srv.handleShutdown, map[string]any{})
	if out["flushed"] != true || out["processed"].(float64) != 1 || out["replayable_count"].(float64) != 0 {
		t.Fatalf("shutdown must process queued work and report nothing left to replay: %v", out)
	}
}

// newTestSession returns a fresh session UUID: session ids are global, so a
// fixed id would collide with the one a previous run left in another tenant.
func newTestSession(t *testing.T) string {
	t.Helper()
	id, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func hasValue(values []string, v string) bool {
	for _, x := range values {
		if x == v {
			return true
		}
	}
	return false
}

// TestPGSyncTurnEnqueuesConsolidation checks the handoff to the
// consolidation worker: without the turn.completed event and outbox job, a
// turn journalled through this provider never becomes memory.
func TestPGSyncTurnEnqueuesConsolidation(t *testing.T) {
	f := newHermesFixture(t)
	session := newTestSession(t)
	out := f.call(t, f.srv.handleSyncTurn, map[string]any{"namespace_id": f.namespace, "session_id": session, "turn_id": "t1",
		"messages": []any{map[string]any{"role": "user", "content": "The billing service uses PostgreSQL 17."}}})
	turn := out["turn_id"].(string)
	var events, jobs int
	if err := f.owner.QueryRow(`SELECT count(*) FROM events WHERE aggregate_id=$1 AND event_type='turn.completed'`, turn).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := f.owner.QueryRow(`SELECT count(*) FROM outbox_jobs WHERE tenant_id=$1 AND job_type='turn.completed' AND payload->>'turn_id'=$2 AND status='pending'`, f.tenant, turn).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if events != 1 || jobs != 1 {
		t.Fatalf("sync_turn must announce the turn for consolidation: %d events, %d jobs", events, jobs)
	}
}

// hookRecorder is a stand-in extension manager that records dispatched
// lifecycle events.
type hookRecorder struct {
	mu     sync.Mutex
	events []map[string]any
}

func (h *hookRecorder) serve(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	h.mu.Lock()
	h.events = append(h.events, body)
	h.mu.Unlock()
	_ = json.NewEncoder(w).Encode(map[string]any{"results": []any{}})
}

// await waits for an event named name whose payload matches key=value.
func (h *hookRecorder) await(t *testing.T, name, key string, value any) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		for _, e := range h.events {
			if p, _ := e["payload"].(map[string]any); e["event"] == name && (key == "" || p[key] == value) {
				h.mu.Unlock()
				return
			}
		}
		h.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("hook %s (%s=%v) was not raised; got %v", name, key, value, h.events)
}

func TestPGLifecycleHooksAreRaised(t *testing.T) {
	f := newHermesFixture(t)
	rec := &hookRecorder{}
	ext := httptest.NewTLSServer(http.HandlerFunc(rec.serve))
	defer ext.Close()
	f.srv.hooks = hooksdk.NewClient("hermes-provider", ext.URL, "hook-test-secret", ext.Client())

	parent, child := newTestSession(t), newTestSession(t)
	f.call(t, f.srv.handleSyncTurn, map[string]any{"namespace_id": f.namespace, "session_id": parent, "turn_id": "t1",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}}})
	rec.await(t, hooksdk.OnSessionStart, "session_id", parent)

	out := f.call(t, f.srv.handleSessionSwitch, map[string]any{"namespace_id": f.namespace, "from_session_id": parent, "to_session_id": child, "reason": "branch"})
	if out["preserved"] != true || out["reason"] != "branch" {
		t.Fatalf("switch: %v", out)
	}
	rec.await(t, hooksdk.OnSessionSwitch, "reason", "branch")
	rec.await(t, hooksdk.OnSessionStart, "session_id", child)

	f.call(t, f.srv.handleDelegation, map[string]any{"parent_session_id": parent, "sub_agent_id": "researcher", "task": "look it up"})
	rec.await(t, hooksdk.OnDelegationStart, "sub_agent_id", "researcher")
	f.call(t, f.srv.handleDelegation, map[string]any{"parent_session_id": parent, "sub_agent_id": "researcher", "task": "look it up", "result": "found"})
	rec.await(t, hooksdk.OnDelegationEnd, "sub_agent_id", "researcher")

	f.call(t, f.srv.handlePreCompress, map[string]any{"namespace_id": f.namespace, "session_id": parent,
		"messages": []any{map[string]any{"role": "user", "content": "keep this"}}})
	rec.await(t, hooksdk.OnPreCompress, "session_id", parent)
	rec.await(t, hooksdk.OnPostCompress, "session_id", parent)

	// An unknown switch reason is rejected rather than recorded.
	encoded, _ := json.Marshal(map[string]any{"namespace_id": f.namespace, "to_session_id": child, "reason": "teleport"})
	r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(encoded))
	r.Header.Set("X-SCOS-Tenant-ID", f.tenant)
	r.Header.Set("X-SCOS-Principal-ID", f.principal)
	w := httptest.NewRecorder()
	f.srv.handleSessionSwitch(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown reason: %d", w.Code)
	}
}

func TestPGTurnContextAndProfileAreDistinguished(t *testing.T) {
	f := newHermesFixture(t)
	session := newTestSession(t)
	out := f.call(t, f.srv.handleSyncTurn, map[string]any{"namespace_id": f.namespace, "session_id": session, "turn_id": "cron-1",
		"context": "cron", "profile_path": "/home/u/.hermes/profiles/ops",
		"messages": []any{map[string]any{"role": "user", "content": "nightly report"}}})
	var recorded, profile string
	if err := f.owner.QueryRow(`SELECT data->>'context', data->>'profile_path' FROM events WHERE aggregate_id=$1 AND event_type='turn.completed'`, out["turn_id"]).Scan(&recorded, &profile); err != nil {
		t.Fatal(err)
	}
	if recorded != "cron" || profile != "/home/u/.hermes/profiles/ops" {
		t.Fatalf("turn context/profile not recorded: %q %q", recorded, profile)
	}

	// A context outside the §24.6 set is refused.
	encoded, _ := json.Marshal(map[string]any{"namespace_id": f.namespace, "session_id": session, "turn_id": "x", "context": "daemon", "messages": []any{}})
	r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(encoded))
	r.Header.Set("X-SCOS-Tenant-ID", f.tenant)
	r.Header.Set("X-SCOS-Principal-ID", f.principal)
	w := httptest.NewRecorder()
	f.srv.handleSyncTurn(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown context: %d", w.Code)
	}

	// Recall warmed for one Hermes profile is not served to another.
	f.call(t, f.srv.handleQueuePrefetch, map[string]any{"session_id": session, "profile_path": "/profiles/ops"})
	if err := f.srv.warm(context.Background(), f.srv.prefetchQueue.drain()[0]); err != nil {
		t.Fatal(err)
	}
	if got := f.call(t, f.srv.handlePrefetch, map[string]any{"namespace_id": f.namespace, "session_id": session, "profile_path": "/profiles/personal"}); got["cache_hit"] != false {
		t.Fatalf("another profile must not see this profile's warmed recall: %v", got)
	}
	if got := f.call(t, f.srv.handlePrefetch, map[string]any{"namespace_id": f.namespace, "session_id": session, "profile_path": "/profiles/ops"}); got["cache_hit"] != true {
		t.Fatalf("the warming profile must hit its cache: %v", got)
	}
}
