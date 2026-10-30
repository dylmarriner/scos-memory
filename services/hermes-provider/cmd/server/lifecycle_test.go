package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	testTenantID    = "tenant-1"
	testPrincipalID = "principal-1"
	testSessionID   = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
)

// post issues an authenticated request against a handler and returns the
// recorder. Identity headers match what resolveIdentity expects.
func post(t *testing.T, handler http.HandlerFunc, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(encoded))
	r.Header.Set("X-SCOS-Tenant-ID", testTenantID)
	r.Header.Set("X-SCOS-Principal-ID", testPrincipalID)
	w := httptest.NewRecorder()
	handler(w, r)
	return w
}

func TestQueuePrefetchIsBoundedAndReportsSaturation(t *testing.T) {
	srv := newHermesServer()

	w := post(t, srv.handleQueuePrefetch, "/v1/provider/queue_prefetch", map[string]any{
		"session_id": testSessionID, "query": "next topic",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var first map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &first); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if first["queued"] != true {
		t.Fatalf("expected the request to be queued, got %v", first)
	}

	// Saturate the bounded queue; the surplus must be reported, not silently
	// dropped as though the cache had been warmed.
	for i := 0; i < maxPrefetchQueueDepth+5; i++ {
		post(t, srv.handleQueuePrefetch, "/v1/provider/queue_prefetch", map[string]any{
			"session_id": testSessionID,
		})
	}
	if depth := srv.prefetchQueue.depth(); depth > maxPrefetchQueueDepth {
		t.Fatalf("queue exceeded its bound: %d", depth)
	}
	w = post(t, srv.handleQueuePrefetch, "/v1/provider/queue_prefetch", map[string]any{
		"session_id": testSessionID,
	})
	var saturated map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &saturated); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if saturated["queued"] != false || saturated["reason"] != "queue_saturated" {
		t.Fatalf("saturation was not reported: %v", saturated)
	}
}

func TestQueuePrefetchRequiresValidSession(t *testing.T) {
	srv := newHermesServer()
	if w := post(t, srv.handleQueuePrefetch, "/v1/provider/queue_prefetch", map[string]any{}); w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing session_id, got %d", w.Code)
	}
	w := post(t, srv.handleQueuePrefetch, "/v1/provider/queue_prefetch", map[string]any{"session_id": "not-a-uuid"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for non-UUID session_id, got %d", w.Code)
	}
}

func TestSessionEndCancelsOnlyTheCallersWorkForThatSession(t *testing.T) {
	srv := newHermesServer()
	other := "11111111-2222-3333-4444-555555555555"

	post(t, srv.handleQueuePrefetch, "/v1/provider/queue_prefetch", map[string]any{"session_id": testSessionID})
	post(t, srv.handleQueuePrefetch, "/v1/provider/queue_prefetch", map[string]any{"session_id": other})
	// Another principal's work for the same session id is not the caller's.
	srv.prefetchQueue.enqueue(queuedPrefetch{TenantID: testTenantID, PrincipalID: "principal-2", SessionID: testSessionID})
	srv.warm.put(warmKey{testTenantID, testPrincipalID, testSessionID}, warmEntry{warmedAt: time.Now()})

	w := post(t, srv.handleSessionEnd, "/v1/provider/session_end", map[string]any{"session_id": testSessionID})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result["cancelled_prefetch"].(float64) != 1 || result["evicted_warm"].(float64) != 1 {
		t.Fatalf("expected one cancelled item and one evicted result, got %v", result)
	}
	if result["pending"].(float64) != 2 {
		t.Fatalf("expected the other session's and principal's work to remain, got %v", result["pending"])
	}
}

func TestPrefetchQueueIsConsumedInOrder(t *testing.T) {
	var q prefetchQueue
	q.enqueue(queuedPrefetch{SessionID: "a"})
	q.enqueue(queuedPrefetch{SessionID: "b"})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	first, ok := q.next(ctx)
	second, ok2 := q.next(ctx)
	if !ok || !ok2 || first.SessionID != "a" || second.SessionID != "b" || q.depth() != 0 {
		t.Fatalf("consumed %v %v, depth %d", first, second, q.depth())
	}

	// A waiting consumer wakes for work queued after it started waiting.
	got := make(chan queuedPrefetch, 1)
	go func() {
		item, _ := q.next(ctx)
		got <- item
	}()
	time.Sleep(10 * time.Millisecond)
	q.enqueue(queuedPrefetch{SessionID: "c"})
	select {
	case item := <-got:
		if item.SessionID != "c" {
			t.Fatalf("woke with %v", item)
		}
	case <-ctx.Done():
		t.Fatal("consumer was not woken by new work")
	}

	q.record("tenant-a", nil)
	q.record("tenant-a", errors.New("retrieval down for tenant-a session 42"))
	stats := q.stats("tenant-a")
	if stats["prefetch_warmed"].(int64) != 1 || stats["prefetch_failed"].(int64) != 1 || stats["prefetch_last_error"] != "retrieval down for tenant-a session 42" {
		t.Fatalf("stats = %v", stats)
	}
	// Another tenant sees none of tenant-a's activity or error text.
	other := q.stats("tenant-b")
	if other["prefetch_warmed"].(int64) != 0 || other["prefetch_failed"].(int64) != 0 || other["prefetch_last_error"] != nil || other["queue_depth"] != 0 {
		t.Fatalf("tenant-b sees tenant-a's prefetch status: %v", other)
	}
}

func TestWarmEntryIsServedOnlyWhileCurrent(t *testing.T) {
	var c warmCache
	key := warmKey{"t", "p", testSessionID}
	now := time.Now()
	c.put(key, warmEntry{sequence: 3, warmedAt: now})
	if _, ok := c.current(warmKey{"t", "other", testSessionID}, 3, now); ok {
		t.Fatal("a warm result must not be served to another principal")
	}
	if _, ok := c.current(key, 3, now.Add(time.Second)); !ok {
		t.Fatal("a current warm result was not served")
	}
	if _, ok := c.current(key, 4, now); ok {
		t.Fatal("a result warmed before a newer turn was served")
	}
	c.put(key, warmEntry{sequence: 3, warmedAt: now})
	if _, ok := c.current(key, 3, now.Add(prefetchWarmTTL+time.Second)); ok {
		t.Fatal("an expired warm result was served")
	}
}

func TestShutdownReportsDroppedWorkInsteadOfClaimingReplay(t *testing.T) {
	srv := newHermesServer()
	post(t, srv.handleQueuePrefetch, "/v1/provider/queue_prefetch", map[string]any{"session_id": testSessionID})
	w := post(t, srv.handleShutdown, "/v1/provider/shutdown", map[string]any{})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// No store is configured and one queued pre-warm was dropped: neither
	// flushed nor replayable may be claimed.
	if out["flushed"] != false || out["replayable"] != false || out["dropped_prefetch"].(float64) != 1 {
		t.Fatalf("shutdown = %v", out)
	}
	if srv.prefetchQueue.depth() != 0 {
		t.Fatalf("caller's work still queued: %d", srv.prefetchQueue.depth())
	}
}

func TestConfigSchemaMatchesGovernedContract(t *testing.T) {
	srv := newHermesServer()
	r := httptest.NewRequest(http.MethodGet, "/v1/provider/config_schema", nil)
	w := httptest.NewRecorder()
	srv.handleConfigSchema(w, r)

	var schema struct {
		Properties map[string]any `json:"properties"`
		Required   []string       `json:"required"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &schema); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(schema.Properties) != len(configKeys) {
		t.Fatalf("expected %d config keys, got %d", len(configKeys), len(schema.Properties))
	}
	for _, key := range configKeys {
		if _, ok := schema.Properties[key]; !ok {
			t.Errorf("missing governed config key: %s", key)
		}
	}
}

func TestSaveConfigValidation(t *testing.T) {
	srv := newHermesServer()
	valid := map[string]any{
		"base_url": "https://scos.internal", "tenant_id": testTenantID, "principal_id": testPrincipalID,
	}

	// A valid configuration passes validation and is refused only because
	// there is nowhere to store it: save_config never reports an unsaved
	// configuration as saved.
	if w := post(t, srv.handleSaveConfig, "/v1/provider/save_config", valid); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("valid config without a store: %d %s", w.Code, w.Body.String())
	}
	// The configuration must be for the authenticated identity.
	mine := map[string]any{"base_url": "https://scos.internal", "tenant_id": testTenantID, "principal_id": "someone-else"}
	if w := post(t, srv.handleSaveConfig, "/v1/provider/save_config", mine); w.Code != http.StatusForbidden {
		t.Fatalf("config for another principal: %d %s", w.Code, w.Body.String())
	}

	// §24.4: secrets must never be accepted inline.
	for _, banned := range secretKeys {
		body := map[string]any{}
		for k, v := range valid {
			body[k] = v
		}
		body[banned] = "hunter2"
		w := post(t, srv.handleSaveConfig, "/v1/provider/save_config", body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("inline secret %q was accepted", banned)
		}
	}

	// Unknown keys and missing required fields are both rejected.
	w := post(t, srv.handleSaveConfig, "/v1/provider/save_config", map[string]any{
		"base_url": "https://scos.internal", "tenant_id": "t-1", "principal_id": "u-1", "nonsense": "x",
	})
	if w.Code != http.StatusBadRequest {
		t.Errorf("unknown key was accepted: %d", w.Code)
	}
	if w := post(t, srv.handleSaveConfig, "/v1/provider/save_config", map[string]any{
		"base_url": "https://scos.internal",
	}); w.Code != http.StatusBadRequest {
		t.Errorf("missing required field was accepted: %d", w.Code)
	}
}

// TestBackupPathsDoesNotFabricateAPath guards §42: the provider must not
// invent a profile-local backup path when authority lives in PostgreSQL.
func TestBackupPathsDoesNotFabricateAPath(t *testing.T) {
	srv := newHermesServer()
	r := httptest.NewRequest(http.MethodGet, "/v1/provider/backup_paths", nil)
	w := httptest.NewRecorder()
	srv.handleBackupPaths(w, r)

	var result struct {
		Paths     []string `json:"paths"`
		Authority string   `json:"authority"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(result.Paths) != 0 {
		t.Fatalf("expected no fabricated paths, got %v", result.Paths)
	}
	if result.Authority != "postgresql" {
		t.Fatalf("expected the real authority to be named, got %q", result.Authority)
	}
}

func TestToolCallRejectsUnknownTool(t *testing.T) {
	srv := newHermesServer()
	w := post(t, srv.handleToolCall, "/v1/provider/tool_call", map[string]any{"tool": "scos_nonsense"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown tool, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "unknown tool") {
		t.Fatalf("unhelpful error: %s", w.Body.String())
	}
}

func TestToolCallServesStatusAndExecutesDelegatedTools(t *testing.T) {
	type seen struct{ method, uri, tenant, cp, body string }
	var calls []seen
	mi := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		calls = append(calls, seen{r.Method, r.URL.RequestURI(), r.Header.Get("X-SCOS-Tenant-ID"), r.Header.Get("X-SCOS-Control-Plane-Token"), string(raw)})
		if strings.HasSuffix(r.URL.Path, "/missing") {
			http.Error(w, "fact not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"path":"`+r.URL.Path+`"}`)
	}))
	defer mi.Close()

	srv := newHermesServer()
	srv.memoryIntelligence = &memoryIntelligence{baseURL: mi.URL, controlPlaneToken: "cp", client: mi.Client()}

	w := post(t, srv.handleToolCall, "/v1/provider/tool_call", map[string]any{"tool": "scos_status"})
	if w.Code != http.StatusOK {
		t.Fatalf("scos_status failed: %d %s", w.Code, w.Body.String())
	}

	cases := []struct {
		tool       string
		args       map[string]any
		wantMethod string
		wantURI    string
	}{
		{"scos_user_model", nil, http.MethodGet, "/v1/dashboard/user-model"},
		{"scos_memory_graph", nil, http.MethodGet, "/v1/dashboard/memory-graph"},
		{"scos_explain", map[string]any{"memory_id": "f1"}, http.MethodGet, "/v1/facts/f1"},
		{"scos_correct", map[string]any{"memory_id": "f1", "value": "new", "reason": "typo"}, http.MethodPatch, "/v1/facts/f1"},
		{"scos_forget", map[string]any{"memory_id": "f1", "cascade": true}, http.MethodDelete, "/v1/facts/f1?cascade=true"},
	}
	for _, c := range cases {
		calls = nil
		w := post(t, srv.handleToolCall, "/v1/provider/tool_call", map[string]any{"tool": c.tool, "arguments": c.args})
		if w.Code != http.StatusOK {
			t.Fatalf("%s failed: %d %s", c.tool, w.Code, w.Body.String())
		}
		var result map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatalf("decode %s: %v", c.tool, err)
		}
		if result["served_by"] != "memory-intelligence" || result["result"] == nil {
			t.Errorf("%s returned no executed result: %v", c.tool, result)
		}
		if len(calls) != 1 || calls[0].method != c.wantMethod || calls[0].uri != c.wantURI || calls[0].tenant == "" || calls[0].cp != "cp" {
			t.Fatalf("%s upstream call = %+v", c.tool, calls)
		}
		if c.tool == "scos_correct" && !strings.Contains(calls[0].body, `"content":"new"`) {
			t.Fatalf("correct body = %s", calls[0].body)
		}
	}

	w = post(t, srv.handleToolCall, "/v1/provider/tool_call", map[string]any{"tool": "scos_explain", "arguments": map[string]any{"memory_id": "missing"}})
	if w.Code != http.StatusNotFound {
		t.Fatalf("upstream 404 should propagate, got %d", w.Code)
	}
	w = post(t, srv.handleToolCall, "/v1/provider/tool_call", map[string]any{"tool": "scos_forget"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing memory_id should be 400, got %d", w.Code)
	}

	unconfigured := newHermesServer()
	w = post(t, unconfigured.handleToolCall, "/v1/provider/tool_call", map[string]any{"tool": "scos_user_model"})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured delegation should be 503, got %d", w.Code)
	}
}

// TestEveryGovernedToolIsRoutable ensures the dispatch table and the advertised
// schema list cannot drift apart.
func TestEveryGovernedToolIsRoutable(t *testing.T) {
	srv := newHermesServer()
	r := httptest.NewRequest(http.MethodGet, "/v1/provider/tool_schemas", nil)
	w := httptest.NewRecorder()
	srv.handleToolSchemas(w, r)

	var tools []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &tools); err != nil {
		t.Fatalf("decode: %v", err)
	}
	routable := governedToolNames()
	for _, tool := range tools {
		name, _ := tool["name"].(string)
		if !routable[name] {
			t.Errorf("advertised tool %q has no dispatch path", name)
		}
	}
	if len(routable) != len(tools) {
		t.Fatalf("dispatch table (%d) and advertised tools (%d) disagree", len(routable), len(tools))
	}
}

func TestLifecycleHandlersRejectWrongMethod(t *testing.T) {
	srv := newHermesServer()
	handlers := map[string]http.HandlerFunc{
		"queue_prefetch": srv.handleQueuePrefetch,
		"turn_start":     srv.handleTurnStart,
		"session_end":    srv.handleSessionEnd,
		"tool_call":      srv.handleToolCall,
		"save_config":    srv.handleSaveConfig,
	}
	for name, handler := range handlers {
		r := httptest.NewRequest(http.MethodGet, "/v1/provider/"+name, nil)
		w := httptest.NewRecorder()
		handler(w, r)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s accepted GET: %d", name, w.Code)
		}
	}
}

// Status must not imply the memory fabric is healthy when PostgreSQL is absent.
// A stale successful prefetch is not evidence of present database reachability.
func TestScosStatusReportsUnavailableDatabase(t *testing.T) {
	srv := newHermesServer()
	w := post(t, srv.handleToolCall, "/v1/provider/tool_call", map[string]any{"tool": "scos_status"})
	if w.Code != http.StatusOK {
		t.Fatalf("status must remain queryable during outage: %d %s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if got["healthy"] != false || got["database_status"] != "unavailable" {
		t.Fatalf("status must report database unavailable, got %v", got)
	}
}

// An open sql.DB is not proof that PostgreSQL is actually reachable.
func TestScosStatusReportsUnreachableDatabase(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("pgx", "postgres://scos:fake@"+address+"/scos_memory?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	srv := newHermesServer()
	srv.db = db
	w := post(t, srv.handleToolCall, "/v1/provider/tool_call", map[string]any{"tool": "scos_status"})
	if w.Code != http.StatusOK {
		t.Fatalf("status query should survive PostgreSQL outage: %d %s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["healthy"] != false || got["database_status"] != "unhealthy" {
		t.Fatalf("unreachable Postgres must be reported unhealthy: %v", got)
	}
}
