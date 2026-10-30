package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"
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

// handleQueuePrefetch pre-warms likely next-turn context without blocking the
// caller: the request is queued and acknowledged immediately, and the
// prefetch worker (prefetch.go) consumes it (§24.3).
func (s *hermesServer) handleQueuePrefetch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		TenantID    string `json:"tenant_id,omitempty"`
		PrincipalID string `json:"principal_id,omitempty"`
		SessionID   string `json:"session_id"`
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
		Query:       req.Query,
		MaxItems:    req.MaxItems,
		// Governed recall for the query runs later on the caller's behalf.
		authorization: r.Header.Get("Authorization"),
	})
	if !queued {
		// Saturation is reported rather than hidden: the caller learns the
		// pre-warm did not happen instead of assuming a warm cache.
		writeJSON(w, map[string]any{
			"queued": false, "reason": "queue_saturated", "queue_depth": s.prefetchQueue.depth(),
		})
		return
	}
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
	writeJSON(w, map[string]any{"session_id": req.SessionID, "recall": recall})
}

// handleSessionEnd cancels the finished session's pending pre-warm work and
// drops its warmed results: there is no next turn for them to serve. Every
// durable write (sync_turn, memory_write, pre_compress) was committed before
// it was acknowledged, so nothing else is pending for the session.
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

	// Only the caller's work for this session is touched; other sessions and
	// other principals' pre-warm requests stay queued.
	cancelled := s.prefetchQueue.cancel(func(item queuedPrefetch) bool {
		return item.TenantID == identity.TenantID && item.PrincipalID == identity.PrincipalID && item.SessionID == req.SessionID
	})
	evicted := s.warm.evict(func(key warmKey) bool {
		return key.tenantID == identity.TenantID && key.principalID == identity.PrincipalID && key.sessionID == req.SessionID
	})
	writeJSON(w, map[string]any{
		"session_id":         req.SessionID,
		"cancelled_prefetch": cancelled,
		"evicted_warm":       evicted,
		"pending":            s.prefetchQueue.depth(),
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
// stores it for the authenticated principal and profile scope.
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
	for _, key := range configKeys {
		if value, present := config[key]; present {
			if _, isString := value.(string); !isString {
				http.Error(w, key+" must be a string", http.StatusBadRequest)
				return
			}
		}
	}
	// The configuration names the identity it is for; it must be the
	// authenticated caller's.
	tenantID, _ := config["tenant_id"].(string)
	principalID, _ := config["principal_id"].(string)
	identity, status, msg := resolveIdentity(r, strings.TrimSpace(tenantID), strings.TrimSpace(principalID))
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	if strings.TrimSpace(tenantID) != identity.TenantID || strings.TrimSpace(principalID) != identity.PrincipalID {
		http.Error(w, "configuration identity does not match the authenticated identity", http.StatusForbidden)
		return
	}
	if s.db == nil {
		http.Error(w, "postgres dependency is not configured", http.StatusServiceUnavailable)
		return
	}
	scope, _ := config["profile_scope"].(string)
	saved, err := saveProviderConfig(r.Context(), s.db, identity, strings.TrimSpace(scope), config)
	if err != nil {
		http.Error(w, "save config: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"saved":         true,
		"profile_scope": saved.ProfileScope,
		"config":        saved.Config,
		"updated_at":    saved.ConfigUpdatedAt,
	})
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
		status := s.prefetchQueue.stats(identity.TenantID)
		status["tool"] = req.Tool
		status["provider"] = providerName
		// Queue outcomes describe past work, not present dependency health.
		// Keep status callable during outages, and never return raw database
		// errors or credentials. Every call uses a bounded live PostgreSQL ping.
		dbStatus := "unavailable"
		if s.db != nil {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			if err := s.db.PingContext(ctx); err != nil {
				dbStatus = "unhealthy"
			} else {
				dbStatus = "ready"
			}
			cancel()
		}
		status["database_status"] = dbStatus
		status["healthy"] = dbStatus == "ready"
		writeJSON(w, status)
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
		// The session a recall is scoped to is the one the caller named
		// (request field, or the tool argument); none named means
		// long-term memory only.
		sessionID := strings.TrimSpace(req.SessionID)
		if sessionID == "" {
			sessionID, _ = req.Arguments["session_id"].(string)
			sessionID = strings.TrimSpace(sessionID)
		}
		if sessionID != "" && !isUUID(sessionID) {
			http.Error(w, "session_id must be a session UUID", http.StatusBadRequest)
			return
		}
		if s.retrieval == nil {
			http.Error(w, "retrieval-orchestrator is not configured for scos_recall", http.StatusServiceUnavailable)
			return
		}
		result, err := s.retrieval.recall(r.Context(), r, identity, query, maxItems, sessionID)
		if err != nil {
			status := http.StatusBadGateway
			var ue *upstreamError
			if errors.As(err, &ue) {
				status = ue.status
			}
			http.Error(w, "recall: "+err.Error(), status)
			return
		}
		writeJSON(w, map[string]any{"tool": req.Tool, "served_by": "retrieval-orchestrator", "retrieval_run_id": result.RetrievalRunID, "recall": result.Memories,
			"degraded_sources": result.DegradedSources, "degraded_models": result.DegradedModels})
	default:
		// The inspection and correction tools are owned by
		// memory-intelligence; execute them there and return its answer.
		if s.memoryIntelligence == nil {
			http.Error(w, "memory-intelligence is not configured for "+req.Tool, http.StatusServiceUnavailable)
			return
		}
		result, status, err := s.memoryIntelligence.executeDelegatedTool(r.Context(), r, identity, req.Tool, req.Arguments)
		if err != nil {
			http.Error(w, err.Error(), status)
			return
		}
		writeJSON(w, map[string]any{"tool": req.Tool, "served_by": "memory-intelligence", "result": result})
	}
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
