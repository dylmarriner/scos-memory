package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

func TestSessionEndDrainsOnlyThatSession(t *testing.T) {
	srv := newHermesServer()
	other := "11111111-2222-3333-4444-555555555555"

	post(t, srv.handleQueuePrefetch, "/v1/provider/queue_prefetch", map[string]any{"session_id": testSessionID})
	post(t, srv.handleQueuePrefetch, "/v1/provider/queue_prefetch", map[string]any{"session_id": other})

	w := post(t, srv.handleSessionEnd, "/v1/provider/session_end", map[string]any{"session_id": testSessionID})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result["drained"].(float64) != 1 {
		t.Fatalf("expected 1 drained item, got %v", result["drained"])
	}
	// The other session's pre-warm work must survive as replayable pending work.
	if result["pending"].(float64) != 1 {
		t.Fatalf("expected the other session's work to remain, got %v", result["pending"])
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
		"base_url": "https://scos.internal", "tenant_id": "t-1", "principal_id": "u-1",
	}

	if w := post(t, srv.handleSaveConfig, "/v1/provider/save_config", valid); w.Code != http.StatusOK {
		t.Fatalf("valid config rejected: %d %s", w.Code, w.Body.String())
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

func TestToolCallServesStatusAndDelegatesOthers(t *testing.T) {
	srv := newHermesServer()

	w := post(t, srv.handleToolCall, "/v1/provider/tool_call", map[string]any{"tool": "scos_status"})
	if w.Code != http.StatusOK {
		t.Fatalf("scos_status failed: %d %s", w.Code, w.Body.String())
	}

	// Tools owned by memory-intelligence report the boundary rather than
	// fabricating a local answer.
	for _, tool := range []string{"scos_user_model", "scos_memory_graph", "scos_explain", "scos_correct", "scos_forget"} {
		w := post(t, srv.handleToolCall, "/v1/provider/tool_call", map[string]any{"tool": tool})
		if w.Code != http.StatusOK {
			t.Fatalf("%s failed: %d", tool, w.Code)
		}
		var result map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatalf("decode %s: %v", tool, err)
		}
		if result["delegated_to"] != "memory-intelligence" {
			t.Errorf("%s did not report its owning service: %v", tool, result)
		}
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
