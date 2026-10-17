//go:build integration

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/dylmarriner/scos-memory/packages/auth"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// hermesFixture seeds a tenant, two principals and a namespace the first
// principal belongs to, as the owner role, and runs handlers as the runtime
// role under RLS.
type hermesFixture struct {
	t                       *testing.T
	srv                     *hermesServer
	tenant, owner, outsider string
	namespace               string
}

func newHermesFixture(t *testing.T) *hermesFixture {
	t.Helper()
	adminURL, appURL := os.Getenv("DATABASE_URL"), os.Getenv("SCOS_APP_TEST_DATABASE_URL")
	if adminURL == "" || appURL == "" {
		t.Fatal("DATABASE_URL (owner) and SCOS_APP_TEST_DATABASE_URL (runtime role) must be set")
	}
	ctx := context.Background()
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	app, err := sql.Open("pgx", appURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.Close() })

	f := &hermesFixture{t: t, srv: newHermesServer()}
	f.srv.db = app
	must := func(query string, dest *string, args ...any) {
		t.Helper()
		if err := admin.QueryRowContext(ctx, query, args...).Scan(dest); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	must(`INSERT INTO tenants (name) VALUES ('hermes-lifecycle-it') RETURNING id::text`, &f.tenant)
	must(`INSERT INTO principals (tenant_id, kind, external_id, display_name) VALUES ($1, 'human', 'hermes-owner-'||gen_random_uuid(), 'Owner') RETURNING id::text`, &f.owner, f.tenant)
	must(`INSERT INTO principals (tenant_id, kind, external_id, display_name) VALUES ($1, 'human', 'hermes-outsider-'||gen_random_uuid(), 'Outsider') RETURNING id::text`, &f.outsider, f.tenant)
	must(`INSERT INTO namespaces (tenant_id, owner_principal_id, canonical_name, sensitivity_ceiling) VALUES ($1, $2, 'hermes-lifecycle-it', 'internal') RETURNING id::text`, &f.namespace, f.tenant, f.owner)
	if _, err := admin.ExecContext(ctx, `INSERT INTO namespace_memberships (namespace_id, principal_id, can_read, can_write_candidates, can_promote, can_administer) VALUES ($1, $2, true, true, true, true)`, f.namespace, f.owner); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *hermesFixture) call(handler http.HandlerFunc, principal, path string, body any) (int, map[string]any) {
	f.t.Helper()
	encoded, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(encoded))
	r = r.WithContext(auth.ContextWithClaims(r.Context(), auth.Claims{TenantID: f.tenant, PrincipalID: principal}))
	r.Header.Set("Authorization", "Bearer it-token")
	w := httptest.NewRecorder()
	handler(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// newSession returns a session id unique to this run; session ids are
// global, so a fixed one would collide with an earlier run's tenant.
func (f *hermesFixture) newSession() string {
	f.t.Helper()
	id, err := newID()
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}

// warmNext consumes one queued pre-warm the way runPrefetchWorker does.
func (f *hermesFixture) warmNext() error {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	item, ok := f.srv.prefetchQueue.next(ctx)
	if !ok {
		f.t.Fatal("no queued pre-warm to consume")
	}
	err := f.srv.warmPrefetch(ctx, item)
	f.srv.prefetchQueue.record(err)
	return err
}

func TestSyncTurnStoresToolCallsAndReturnsTheStoredTurnID(t *testing.T) {
	f := newHermesFixture(t)
	session := f.newSession()
	turn := map[string]any{
		"namespace_id": f.namespace, "session_id": session, "turn_id": "hermes-turn:abc",
		"messages":   []any{map[string]any{"role": "user", "content": "deploy it"}, map[string]any{"role": "assistant", "content": "deployed"}},
		"tool_calls": []any{map[string]any{"name": "shell", "arguments": map[string]any{"cmd": "make deploy"}}},
	}
	code, synced := f.call(f.srv.handleSyncTurn, f.owner, "/v1/provider/sync_turn", turn)
	if code != http.StatusOK {
		t.Fatalf("sync_turn = %d %v", code, synced)
	}
	stored, _ := synced["turn_id"].(string)
	if !isUUID(stored) || synced["client_turn_id"] != "hermes-turn:abc" || synced["tool_calls"].(float64) != 1 {
		t.Fatalf("sync_turn did not return the stored turn: %v", synced)
	}

	code, replay := f.call(f.srv.handleSyncTurn, f.owner, "/v1/provider/sync_turn", turn)
	if code != http.StatusOK || replay["duplicate"] != true || replay["turn_id"] != stored {
		t.Fatalf("duplicate sync_turn = %d %v, want the same stored turn %s", code, replay, stored)
	}

	code, pre := f.call(f.srv.handlePrefetch, f.owner, "/v1/provider/prefetch", map[string]any{"namespace_id": f.namespace, "session_id": session})
	if code != http.StatusOK {
		t.Fatalf("prefetch = %d %v", code, pre)
	}
	recall := pre["recall"].([]any)
	if len(recall) != 1 {
		t.Fatalf("prefetch recalled %d turns, want 1", len(recall))
	}
	got := recall[0].(map[string]any)
	calls, _ := got["tool_calls"].([]any)
	if got["turn_id"] != stored || len(calls) != 1 || calls[0].(map[string]any)["name"] != "shell" {
		t.Fatalf("recalled turn lost its id or tool calls: %v", got)
	}
}

func TestQueuedPrefetchIsConsumedAndServedOnlyWhileCurrent(t *testing.T) {
	f := newHermesFixture(t)
	session := f.newSession()
	var gotAuth string
	ro := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"retrieval_run_id":"run-warm","sections":[{"heading":"Relevant","items":[{"memory_id":"m1","title":"Deploys","content":"Tuesdays","fused_score":0.04,"rank":1}]}]}`)
	}))
	defer ro.Close()
	f.srv.retrieval = &retrievalOrchestrator{baseURL: ro.URL, client: ro.Client()}

	syncTurn := func(id, text string) {
		t.Helper()
		code, out := f.call(f.srv.handleSyncTurn, f.owner, "/v1/provider/sync_turn", map[string]any{
			"namespace_id": f.namespace, "session_id": session, "turn_id": id,
			"messages": []any{map[string]any{"role": "user", "content": text}},
		})
		if code != http.StatusOK {
			t.Fatalf("sync_turn = %d %v", code, out)
		}
	}
	prefetch := func() map[string]any {
		t.Helper()
		code, out := f.call(f.srv.handlePrefetch, f.owner, "/v1/provider/prefetch", map[string]any{"namespace_id": f.namespace, "session_id": session})
		if code != http.StatusOK {
			t.Fatalf("prefetch = %d %v", code, out)
		}
		return out
	}

	syncTurn("t1", "when do we deploy")
	if out := prefetch(); out["cache_hit"] != false {
		t.Fatalf("nothing was warmed, yet cache_hit = %v", out["cache_hit"])
	}

	code, queued := f.call(f.srv.handleQueuePrefetch, f.owner, "/v1/provider/queue_prefetch", map[string]any{"session_id": session, "query": "deploy day", "max_items": 3})
	if code != http.StatusOK || queued["queued"] != true {
		t.Fatalf("queue_prefetch = %d %v", code, queued)
	}
	if err := f.warmNext(); err != nil {
		t.Fatalf("pre-warm failed: %v", err)
	}
	if gotAuth != "Bearer it-token" {
		t.Fatalf("governed recall did not run as the requester: Authorization %q", gotAuth)
	}
	warm := prefetch()
	if warm["cache_hit"] != true || warm["warm_query"] != "deploy day" || len(warm["warm_recall"].([]any)) != 1 || len(warm["recall"].([]any)) != 1 {
		t.Fatalf("warmed prefetch = %v", warm)
	}

	// Another principal never sees the owner's warmed result.
	code, other := f.call(f.srv.handlePrefetch, f.outsider, "/v1/provider/prefetch", map[string]any{"namespace_id": f.namespace, "session_id": session})
	if code == http.StatusOK && other["cache_hit"] == true {
		t.Fatalf("outsider was served the owner's warm result: %v", other)
	}

	// A newer turn makes the warmed result stale.
	syncTurn("t2", "and the rollback plan")
	stale := prefetch()
	if stale["cache_hit"] != false || len(stale["recall"].([]any)) != 2 {
		t.Fatalf("stale warm result served: %v", stale)
	}

	code, status := f.call(f.srv.handleToolCall, f.owner, "/v1/provider/tool_call", map[string]any{"tool": "scos_status"})
	if code != http.StatusOK || status["prefetch_warmed"].(float64) != 1 {
		t.Fatalf("scos_status = %d %v", code, status)
	}
}

func TestSaveConfigAndInitializePersistPerPrincipal(t *testing.T) {
	f := newHermesFixture(t)
	config := map[string]any{
		"base_url": "https://scos.internal", "tenant_id": f.tenant, "principal_id": f.owner,
		"profile_scope": "work", "transport_security": "mtls", "optional_auth_reference": "SCOS_TOKEN",
	}
	code, saved := f.call(f.srv.handleSaveConfig, f.owner, "/v1/provider/save_config", config)
	if code != http.StatusOK || saved["saved"] != true || saved["updated_at"] == "" {
		t.Fatalf("save_config = %d %v", code, saved)
	}

	code, initialized := f.call(f.srv.handleInit, f.owner, "/v1/provider/initialize", map[string]any{
		"profile_path": "/home/owner/.hermes/profiles/work", "profile_scope": "work", "workspace": "repo-a",
	})
	if code != http.StatusOK {
		t.Fatalf("initialize = %d %v", code, initialized)
	}
	stored, _ := initialized["config"].(map[string]any)
	if initialized["configured"] != true || stored["base_url"] != "https://scos.internal" || stored["transport_security"] != "mtls" ||
		initialized["profile_path"] != "/home/owner/.hermes/profiles/work" || initialized["workspace"] != "repo-a" || initialized["initialized_at"] == "" {
		t.Fatalf("initialize did not return the saved setup: %v", initialized)
	}

	// A different scope, and a different principal, have no saved setup.
	code, other := f.call(f.srv.handleInit, f.owner, "/v1/provider/initialize", map[string]any{"profile_path": "/home/owner/.hermes/profiles/home"})
	if code != http.StatusOK || other["configured"] != false {
		t.Fatalf("default scope initialize = %d %v", code, other)
	}
	code, outsider := f.call(f.srv.handleInit, f.outsider, "/v1/provider/initialize", map[string]any{"profile_path": "/p", "profile_scope": "work"})
	if code != http.StatusOK || outsider["configured"] != false {
		t.Fatalf("outsider saw the owner's setup: %d %v", code, outsider)
	}

	// Saving again replaces the setup and keeps the profile binding.
	config["transport_security"] = "tls"
	if code, _ := f.call(f.srv.handleSaveConfig, f.owner, "/v1/provider/save_config", config); code != http.StatusOK {
		t.Fatalf("second save_config = %d", code)
	}
	_, again := f.call(f.srv.handleInit, f.owner, "/v1/provider/initialize", map[string]any{
		"profile_path": "/home/owner/.hermes/profiles/work", "profile_scope": "work",
	})
	if again["config"].(map[string]any)["transport_security"] != "tls" {
		t.Fatalf("saved setup was not replaced: %v", again)
	}
}

// TestSessionIDHeldByAnotherTenantIsAConflict: session ids are global, so a
// second tenant naming one the first already uses must be told so -- not
// accepted and then failed with a 500 when the turn is written.
func TestSessionIDHeldByAnotherTenantIsAConflict(t *testing.T) {
	first := newHermesFixture(t)
	second := newHermesFixture(t)
	session := first.newSession()
	turn := func(f *hermesFixture) map[string]any {
		return map[string]any{
			"namespace_id": f.namespace, "session_id": session, "turn_id": "turn-" + f.tenant,
			"messages": []any{map[string]any{"role": "user", "content": "hello"}},
		}
	}
	if code, out := first.call(first.srv.handleSyncTurn, first.owner, "/v1/provider/sync_turn", turn(first)); code != http.StatusOK {
		t.Fatalf("first tenant sync_turn = %d %v", code, out)
	}
	if code, out := second.call(second.srv.handleSyncTurn, second.owner, "/v1/provider/sync_turn", turn(second)); code != http.StatusConflict {
		t.Fatalf("second tenant reusing the session id = %d %v, want 409", code, out)
	}
}

func TestSyncedAndPreCompressedTurnsAreQueuedForConsolidation(t *testing.T) {
	f := newHermesFixture(t)
	session := f.newSession()
	turn := map[string]any{
		"namespace_id": f.namespace, "session_id": session, "turn_id": "hermes-turn:consolidate",
		"messages": []any{map[string]any{"role": "user", "content": "we deploy on Fridays"}},
	}
	code, synced := f.call(f.srv.handleSyncTurn, f.owner, "/v1/provider/sync_turn", turn)
	if code != http.StatusOK {
		t.Fatalf("sync_turn = %d %v", code, synced)
	}
	// The same turn id with different content is refused, not a duplicate.
	changed := map[string]any{"namespace_id": f.namespace, "session_id": session, "turn_id": "hermes-turn:consolidate",
		"messages": []any{map[string]any{"role": "user", "content": "we deploy on Mondays"}}}
	if code, out := f.call(f.srv.handleSyncTurn, f.owner, "/v1/provider/sync_turn", changed); code != http.StatusConflict {
		t.Fatalf("changed retry = %d %v, want 409", code, out)
	}
	code, pre := f.call(f.srv.handlePreCompress, f.owner, "/v1/provider/pre_compress", map[string]any{
		"namespace_id": f.namespace, "session_id": session,
		"messages": []any{map[string]any{"role": "assistant", "content": "noted: deploys are on Fridays"}},
	})
	if code != http.StatusOK || pre["consolidation"] != "queued" {
		t.Fatalf("pre_compress = %d %v", code, pre)
	}

	admin, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	for _, turnID := range []string{synced["turn_id"].(string), pre["turn_id"].(string)} {
		var jobs, events int
		if err := admin.QueryRow(`SELECT count(*) FROM outbox_jobs WHERE tenant_id=$1 AND operation_id=$2 AND job_type='turn.completed' AND payload->>'turn_id'=$3`, f.tenant, turnID+":completed", turnID).Scan(&jobs); err != nil {
			t.Fatal(err)
		}
		if err := admin.QueryRow(`SELECT count(*) FROM events WHERE tenant_id=$1 AND aggregate_id::text=$2 AND event_type='turn.completed'`, f.tenant, turnID).Scan(&events); err != nil {
			t.Fatal(err)
		}
		if jobs != 1 || events != 1 {
			t.Fatalf("turn %s: %d outbox jobs, %d completion events; want 1 and 1", turnID, jobs, events)
		}
	}
}
