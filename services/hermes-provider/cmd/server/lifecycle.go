package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	hooksdk "github.com/dylmarriner/scos-memory/packages/hook-sdk"
)

// Remaining Hermes MemoryProvider lifecycle surface (GOVERNANCE.md §24.2).
//
// The turn/session/compression/delegation hooks live in main.go; this file
// adds the members that had no HTTP surface: queue_prefetch, handle_tool_call,
// on_turn_start, on_session_end, backup_paths, get_config_schema and
// save_config.

// configKeys is the setup contract §24.4 permits. Anything outside this set is
// rejected, and secrets are never among them: optional_auth_reference names a
// Hermes-managed secret rather than carrying its value.
var configKeys = []string{
	"base_url",
	"profile_scope",
	"tenant_id",
	"principal_id",
	"workspace",
	"transport_security",
	"optional_auth_reference",
}

// secretKeys must never be accepted inline; §24.4 keeps them in Hermes secret
// configuration rather than provider JSON.
var secretKeys = []string{"token", "password", "secret", "api_key", "auth_token"}

// maxPrefetchQueueDepth bounds the pre-warm queue so an unreachable backend
// cannot grow it without limit (§24.3 requires bounded queues).
const maxPrefetchQueueDepth = 256

// prefetchQueue holds pre-warm requests until they are drained. Entries are
// retained rather than dropped so shutdown can report replayable pending work.
type prefetchQueue struct {
	mu    sync.Mutex
	items []queuedPrefetch
}

type queuedPrefetch struct {
	TenantID    string `json:"tenant_id"`
	PrincipalID string `json:"principal_id"`
	SessionID   string `json:"session_id"`
	ProfilePath string `json:"profile_path,omitempty"`
	Query       string `json:"query,omitempty"`
	MaxItems    int    `json:"max_items,omitempty"`
}

// enqueue appends work, reporting false once the bound is reached.
func (q *prefetchQueue) enqueue(item queuedPrefetch) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) >= maxPrefetchQueueDepth {
		return false
	}
	q.items = append(q.items, item)
	return true
}

func (q *prefetchQueue) depth() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// drain removes and returns all queued work.
func (q *prefetchQueue) drain() []queuedPrefetch {
	q.mu.Lock()
	defer q.mu.Unlock()
	drained := q.items
	q.items = nil
	return drained
}

// handleQueuePrefetch pre-warms likely next-turn context without blocking the
// caller: the request is queued and acknowledged immediately (§24.3).
func (s *hermesServer) handleQueuePrefetch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		TenantID    string `json:"tenant_id,omitempty"`
		PrincipalID string `json:"principal_id,omitempty"`
		SessionID   string `json:"session_id"`
		ProfilePath string `json:"profile_path,omitempty"`
		Query       string `json:"query,omitempty"`
		MaxItems    int    `json:"max_items,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.SessionID == "" {
		http.Error(w, "session_id is required", http.StatusBadRequest)
		return
	}
	if !isUUID(req.SessionID) {
		http.Error(w, "session_id must be a UUID", http.StatusBadRequest)
		return
	}
	identity, status, msg := resolveIdentity(r, req.TenantID, req.PrincipalID)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}

	queued := s.prefetchQueue.enqueue(queuedPrefetch{
		TenantID:    identity.TenantID,
		PrincipalID: identity.PrincipalID,
		SessionID:   req.SessionID,
		ProfilePath: req.ProfilePath,
		Query:       req.Query,
		MaxItems:    req.MaxItems,
	})
	if !queued {
		// Saturation is reported rather than hidden: the caller learns the
		// pre-warm did not happen instead of assuming a warm cache.
		writeJSON(w, map[string]any{
			"queued": false, "reason": "queue_saturated", "queue_depth": s.prefetchQueue.depth(),
		})
		return
	}
	s.signalPrefetch()
	writeJSON(w, map[string]any{"queued": true, "queue_depth": s.prefetchQueue.depth()})
}

// handleTurnStart hydrates context at turn start so continuity never depends
// on the model calling a memory tool (§24.3).
func (s *hermesServer) handleTurnStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		TenantID    string `json:"tenant_id,omitempty"`
		PrincipalID string `json:"principal_id,omitempty"`
		SessionID   string `json:"session_id"`
		TurnID      string `json:"turn_id,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.SessionID == "" || !isUUID(req.SessionID) {
		http.Error(w, "session_id must be a UUID", http.StatusBadRequest)
		return
	}
	identity, status, msg := resolveIdentity(r, req.TenantID, req.PrincipalID)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}

	ctx := r.Context()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		http.Error(w, "begin transaction: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	if err := setPrincipalContext(ctx, tx, identity); err != nil {
		http.Error(w, "set principal context: "+err.Error(), http.StatusInternalServerError)
		return
	}
	recall, err := recentTurns(ctx, tx, identity.TenantID, req.SessionID, recallLimit)
	if err != nil {
		http.Error(w, "load recall: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.notify(ctx, hooksdk.OnTurnStart, identity, map[string]any{"session_id": req.SessionID, "turn_id": req.TurnID})
	writeJSON(w, map[string]any{"session_id": req.SessionID, "recall": recall})
}

// handleSessionEnd retires a finished session's provider state: its queued
// pre-warm requests and cached recall serve no later turn, so both are
// discarded (and counted); other sessions' work is untouched.
func (s *hermesServer) handleSessionEnd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		TenantID    string `json:"tenant_id,omitempty"`
		PrincipalID string `json:"principal_id,omitempty"`
		SessionID   string `json:"session_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.SessionID == "" || !isUUID(req.SessionID) {
		http.Error(w, "session_id must be a UUID", http.StatusBadRequest)
		return
	}
	identity, status, msg := resolveIdentity(r, req.TenantID, req.PrincipalID)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}

	var retained []queuedPrefetch
	discarded := 0
	for _, item := range s.prefetchQueue.drain() {
		if item.TenantID == identity.TenantID && item.SessionID == req.SessionID {
			discarded++
			continue
		}
		retained = append(retained, item)
	}
	for _, item := range retained {
		s.prefetchQueue.enqueue(item)
	}
	evicted := s.warmCache.evictSession(identity.TenantID, req.SessionID)
	s.notify(r.Context(), hooksdk.OnSessionEnd, identity, map[string]any{"session_id": req.SessionID})
	writeJSON(w, map[string]any{
		"session_id":            req.SessionID,
		"discarded_prefetches":  discarded,
		"evicted_cache_entries": evicted,
		"pending":               s.prefetchQueue.depth(),
	})
}

// handleBackupPaths reports the paths Hermes should include in profile backups.
func (s *hermesServer) handleBackupPaths(w http.ResponseWriter, r *http.Request) {
	// SCOS Nexus keeps authority in PostgreSQL rather than profile-local files,
	// so Hermes has no provider file to back up. Saying so explicitly is the
	// honest answer; §42 forbids reporting fabricated state, and an invented
	// path would be exactly that.
	writeJSON(w, map[string]any{
		"paths":     []string{},
		"authority": "postgresql",
		"note":      "Provider state is server-side; back up the SCOS database, not the Hermes profile.",
	})
}

// handleConfigSchema returns the smallest necessary setup contract (§24.4).
func (s *hermesServer) handleConfigSchema(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"type": "object",
		"properties": map[string]any{
			"base_url":           map[string]any{"type": "string", "description": "SCOS Nexus memory-service base URL (not an LLM/provider inference URL)"},
			"profile_scope":      map[string]any{"type": "string", "description": "Hermes profile scope"},
			"tenant_id":          map[string]any{"type": "string"},
			"principal_id":       map[string]any{"type": "string"},
			"workspace":          map[string]any{"type": "string"},
			"transport_security": map[string]any{"type": "string", "enum": []string{"tls", "mtls"}},
			"optional_auth_reference": map[string]any{
				"type":        "string",
				"description": "Name of a Hermes-managed secret. Never the secret value.",
			},
		},
		"required": []string{"base_url", "tenant_id", "principal_id"},
	})
}

// handleSaveConfig validates configuration against the governed contract and
// persists it for the calling principal and Hermes profile scope.
func (s *hermesServer) handleSaveConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var config map[string]any
	if err := json.NewDecoder(r.Body).Decode(&config); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}

	allowed := make(map[string]bool, len(configKeys))
	for _, k := range configKeys {
		allowed[k] = true
	}
	var unknown []string
	for k := range config {
		// Reject inline secrets outright rather than storing them (§24.4).
		for _, banned := range secretKeys {
			if strings.EqualFold(k, banned) {
				http.Error(w, k+" must be stored in Hermes secret configuration", http.StatusBadRequest)
				return
			}
		}
		if !allowed[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		http.Error(w, "unsupported configuration keys: "+strings.Join(unknown, ", "), http.StatusBadRequest)
		return
	}
	for _, required := range []string{"base_url", "tenant_id", "principal_id"} {
		if value, _ := config[required].(string); strings.TrimSpace(value) == "" {
			http.Error(w, required+" is required", http.StatusBadRequest)
			return
		}
	}
	for key, value := range config {
		if _, ok := value.(string); !ok {
			http.Error(w, key+" must be a string", http.StatusBadRequest)
			return
		}
	}
	tenantID, principalID := config["tenant_id"].(string), config["principal_id"].(string)
	identity, status, msg := resolveIdentity(r, tenantID, principalID)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	// A configuration belongs to the caller: it cannot be written for, or
	// point at, another tenant or principal.
	if identity.TenantID != tenantID || identity.PrincipalID != principalID {
		http.Error(w, "tenant_id and principal_id must be the authenticated caller's", http.StatusForbidden)
		return
	}
	profileScope, _ := config["profile_scope"].(string)
	encoded, err := json.Marshal(config)
	if err != nil {
		http.Error(w, "encode configuration: "+err.Error(), http.StatusInternalServerError)
		return
	}
	ctx := r.Context()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		http.Error(w, "begin transaction: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	if err := setPrincipalContext(ctx, tx, identity); err != nil {
		http.Error(w, "set principal context: "+err.Error(), http.StatusInternalServerError)
		return
	}
	var updatedAt time.Time
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO hermes_provider_configs (tenant_id, principal_id, profile_scope, config)
		VALUES ($1, $2, $3, $4::jsonb)
		ON CONFLICT (tenant_id, principal_id, profile_scope)
		DO UPDATE SET config = EXCLUDED.config, updated_at = now()
		RETURNING updated_at`, identity.TenantID, identity.PrincipalID, profileScope, string(encoded)).Scan(&updatedAt); err != nil {
		http.Error(w, "save configuration: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"saved": true, "profile_scope": profileScope, "updated_at": updatedAt.UTC().Format(time.RFC3339), "config": config})
}

// handleToolCall dispatches the supplemental §24.5 tools. These are
// supplemental only: automatic recall and persistence continue when no tool is
// ever called.
func (s *hermesServer) handleToolCall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		TenantID    string         `json:"tenant_id,omitempty"`
		PrincipalID string         `json:"principal_id,omitempty"`
		SessionID   string         `json:"session_id,omitempty"`
		Tool        string         `json:"tool"`
		Arguments   map[string]any `json:"arguments,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	identity, status, msg := resolveIdentity(r, req.TenantID, req.PrincipalID)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	if !governedToolNames()[req.Tool] {
		http.Error(w, "unknown tool: "+req.Tool, http.StatusBadRequest)
		return
	}

	switch req.Tool {
	case "scos_status":
		writeJSON(w, map[string]any{
			"tool":              req.Tool,
			"provider":          "scos-nexus",
			"queue_depth":       s.prefetchQueue.depth(),
			"prefetches_warmed": s.prefetchWarmed.Load(),
			"prefetches_failed": s.prefetchFailed.Load(),
		})
	case "scos_recall":
		query, _ := req.Arguments["query"].(string)
		if strings.TrimSpace(query) == "" {
			http.Error(w, "arguments.query is required for scos_recall", http.StatusBadRequest)
			return
		}
		maxItems := 10
		switch v := req.Arguments["max_items"].(type) {
		case float64:
			maxItems = int(v)
		case json.Number:
			if n, err := v.Int64(); err == nil {
				maxItems = int(n)
			}
		}
		ctx := r.Context()
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			http.Error(w, "begin transaction: "+err.Error(), http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()
		if err := setPrincipalContext(ctx, tx, identity); err != nil {
			http.Error(w, "set principal context: "+err.Error(), http.StatusInternalServerError)
			return
		}
		memories, err := searchMemories(ctx, tx, identity.TenantID, query, maxItems)
		if err != nil {
			http.Error(w, "search memories: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"tool": req.Tool, "recall": memories})
	default:
		// The inspection and correction tools are owned by memory-intelligence
		// (user model, graph, fact history and correction); the call is made
		// there as the caller, so its authorisation and audit apply.
		result, status, err := s.callMemoryIntelligence(r, identity, req.Tool, req.Arguments)
		if err != nil {
			http.Error(w, err.Error(), status)
			return
		}
		writeJSON(w, map[string]any{"tool": req.Tool, "result": result})
	}
}

// callMemoryIntelligence performs one §24.5 tool against memory-intelligence
// with the same arguments mcp-gateway accepts for these tools.
func (s *hermesServer) callMemoryIntelligence(r *http.Request, identity requestIdentity, tool string, args map[string]any) (any, int, error) {
	stringArg := func(name string) string { v, _ := args[name].(string); return strings.TrimSpace(v) }
	var method, path string
	var body any
	switch tool {
	case "scos_user_model":
		method, path = http.MethodGet, "/v1/dashboard/user-model"
	case "scos_memory_graph":
		method, path = http.MethodGet, "/v1/memory/graph"
	case "scos_explain", "scos_correct", "scos_forget":
		memoryID := stringArg("memory_id")
		if !isUUID(memoryID) {
			return nil, http.StatusBadRequest, fmt.Errorf("arguments.memory_id must be a UUID for %s", tool)
		}
		path = "/v1/facts/" + url.PathEscape(memoryID)
		switch tool {
		case "scos_explain":
			method = http.MethodGet
		case "scos_correct":
			value := stringArg("value")
			if value == "" {
				return nil, http.StatusBadRequest, fmt.Errorf("arguments.value is required for scos_correct")
			}
			method, body = http.MethodPatch, map[string]any{"content": value, "reason": stringArg("reason")}
		case "scos_forget":
			method = http.MethodDelete
			if cascade, _ := args["cascade"].(bool); cascade {
				path += "?cascade=true"
			}
		}
	default:
		return nil, http.StatusBadRequest, fmt.Errorf("tool %s has no memory-intelligence route", tool)
	}

	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, http.StatusInternalServerError, err
		}
		payload = bytes.NewReader(encoded)
	}
	upstream, err := http.NewRequestWithContext(r.Context(), method, s.memoryIntelligenceURL+path, payload)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if body != nil {
		upstream.Header.Set("Content-Type", "application/json")
	}
	upstream.Header.Set("Authorization", r.Header.Get("Authorization"))
	upstream.Header.Set("X-SCOS-Tenant-ID", identity.TenantID)
	upstream.Header.Set("X-SCOS-Principal-ID", identity.PrincipalID)
	resp, err := s.upstreamClient.Do(upstream)
	if err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("memory-intelligence unavailable: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, http.StatusBadGateway, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("memory-intelligence %s %s: %s", method, path, strings.TrimSpace(string(raw)))
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]any{"status": resp.StatusCode}, 0, nil
	}
	var result any
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("memory-intelligence returned invalid JSON: %w", err)
	}
	return result, 0, nil
}

// governedToolNames is the §24.5 tool set.
func governedToolNames() map[string]bool {
	names := make(map[string]bool)
	for name := range governedToolPaths() {
		names[name] = true
	}
	return names
}

// governedToolPaths maps each governed tool to the service endpoint that owns
// its authoritative implementation.
func governedToolPaths() map[string]string {
	return map[string]string{
		"scos_recall":       "",
		"scos_status":       "",
		"scos_explain":      "/v1/facts/",
		"scos_user_model":   "/v1/dashboard/user-model",
		"scos_memory_graph": "/v1/memory/graph",
		"scos_correct":      "/v1/facts/",
		"scos_forget":       "/v1/facts/",
	}
}
