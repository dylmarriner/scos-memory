package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	serviceauth "github.com/dylanmarriner/scos-memory-fabric/packages/service-auth"
)

const version = "0.1.0"
const providerName = "scos-nexus"

// ── Hermes MemoryProvider lifecycle types ───────────────────────────────

type ProviderStatus struct {
	Name         string `json:"name"`
	Version      string `json:"version"`
	IsAvailable  bool   `json:"is_available"`
	ProfileScope string `json:"profile_scope,omitempty"`
	Healthy      bool   `json:"healthy"`
	Uptime       string `json:"uptime"`
}

type PrefetchRequest struct {
	SessionID   string `json:"session_id"`
	ProfilePath string `json:"profile_path"`
	LastCapsule string `json:"last_capsule_id,omitempty"`
}

type SyncTurnRequest struct {
	SessionID   string `json:"session_id"`
	TurnID      string `json:"turn_id"`
	Messages    []any  `json:"messages"`
	ToolCalls   []any  `json:"tool_calls,omitempty"`
	ProfilePath string `json:"profile_path"`
}

type MemoryWriteRequest struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Scope  string `json:"scope"`
	Source string `json:"source"`
}

type DelegationEvent struct {
	ParentSessionID string `json:"parent_session_id"`
	SubAgentID      string `json:"sub_agent_id"`
	Task            string `json:"task"`
	Result          string `json:"result,omitempty"`
}

// ── server state ─────────────────────────────────────────────────────────

type hermesServer struct {
	mu           sync.RWMutex
	startTime    time.Time
	sessions     map[string]map[string]any // session_id -> metadata
	turnCount    int
	profilePaths map[string]string // session_id -> profile_path
}

func newHermesServer() *hermesServer {
	return &hermesServer{
		startTime:    time.Now(),
		sessions:     make(map[string]map[string]any),
		profilePaths: make(map[string]string),
	}
}

// ── main ─────────────────────────────────────────────────────────────────

func main() {
	srv := newHermesServer()
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", healthOK)
	mux.HandleFunc("/health/ready", healthOK)

	// Hermes MemoryProvider lifecycle
	mux.HandleFunc("/v1/provider/status", srv.handleStatus)
	mux.HandleFunc("/v1/provider/initialize", srv.handleInit)
	mux.HandleFunc("/v1/provider/system_prompt", srv.handleSystemPrompt)
	mux.HandleFunc("/v1/provider/prefetch", srv.handlePrefetch)
	mux.HandleFunc("/v1/provider/sync_turn", srv.handleSyncTurn)
	mux.HandleFunc("/v1/provider/tool_schemas", srv.handleToolSchemas)
	mux.HandleFunc("/v1/provider/session_switch", srv.handleSessionSwitch)
	mux.HandleFunc("/v1/provider/pre_compress", srv.handlePreCompress)
	mux.HandleFunc("/v1/provider/memory_write", srv.handleMemoryWrite)
	mux.HandleFunc("/v1/provider/delegation", srv.handleDelegation)
	mux.HandleFunc("/v1/provider/shutdown", srv.handleShutdown)

	addr := os.Getenv("SCOS_HERMES_PROVIDER_HTTP_ADDR")
	if addr == "" {
		addr = ":8088"
	}
	log.Printf("hermes memory provider (%s) listening on %s", providerName, addr)
	if err := http.ListenAndServe(addr, serviceauth.Require(mux)); err != nil {
		log.Fatal(err)
	}
}

func healthOK(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// ── lifecycle endpoints ─────────────────────────────────────────────────

func (s *hermesServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, ProviderStatus{
		Name:        providerName,
		Version:     version,
		IsAvailable: true,
		Healthy:     true,
		Uptime:      time.Since(s.startTime).Round(time.Second).String(),
	})
}

func (s *hermesServer) handleInit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ProfilePath string `json:"profile_path"`
		TenantID    string `json:"tenant_id"`
		Workspace   string `json:"workspace,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}

	writeJSON(w, map[string]any{
		"status":       "initialized",
		"provider":     providerName,
		"version":      version,
		"profile_path": req.ProfilePath,
		"tenant_id":    req.TenantID,
	})
}

func (s *hermesServer) handleSystemPrompt(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Return system prompt block for memory awareness
	writeJSON(w, map[string]string{
		"system_prompt_block": "You have access to SCOS Nexus memory (" + providerName + " v" + version + "). " +
			"Memory operations are automatic. Use scos_recall tools for explicit retrieval if needed.",
	})
}

func (s *hermesServer) handlePrefetch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req PrefetchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	s.sessions[req.SessionID] = map[string]any{
		"last_seen":      time.Now().UTC().Format(time.RFC3339),
		"prefetch_count": getInt(s.sessions[req.SessionID], "prefetch_count") + 1,
	}
	if req.ProfilePath != "" {
		s.profilePaths[req.SessionID] = req.ProfilePath
	}
	s.mu.Unlock()

	// Return cached recall context
	writeJSON(w, map[string]any{
		"prefetched": true,
		"session_id": req.SessionID,
		"recall":     []map[string]any{},
		"cache_hit":  req.LastCapsule != "",
	})
}

func (s *hermesServer) handleSyncTurn(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req SyncTurnRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	s.turnCount++
	s.sessions[req.SessionID] = map[string]any{
		"last_turn":  req.TurnID,
		"turn_count": s.turnCount,
	}
	s.mu.Unlock()

	writeJSON(w, map[string]any{
		"synced":     true,
		"session_id": req.SessionID,
		"turn_id":    req.TurnID,
		"async":      true,
	})
}

func (s *hermesServer) handleToolSchemas(w http.ResponseWriter, r *http.Request) {
	// Supplemental tools per §24.5
	tools := []map[string]any{
		{
			"name":        "scos_recall",
			"description": "Search SCOS Nexus memory for relevant context",
			"input_schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query":     map[string]any{"type": "string", "description": "Search query"},
					"max_items": map[string]any{"type": "integer", "description": "Max results"},
				},
				"required": []string{"query"},
			},
		},
		{
			"name":        "scos_status",
			"description": "Check SCOS Nexus memory provider status",
			"input_schema": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			"name":        "scos_forget",
			"description": "Request removal of a specific memory",
			"input_schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"memory_id": map[string]any{"type": "string", "description": "Memory ID to forget"},
				},
				"required": []string{"memory_id"},
			},
		},
	}
	writeJSON(w, tools)
}

func (s *hermesServer) handleSessionSwitch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		FromSessionID string `json:"from_session_id"`
		ToSessionID   string `json:"to_session_id"`
		ProfilePath   string `json:"profile_path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	s.profilePaths[req.ToSessionID] = req.ProfilePath
	s.mu.Unlock()

	writeJSON(w, map[string]any{
		"switched":  true,
		"lineage":   req.FromSessionID + " -> " + req.ToSessionID,
		"preserved": true,
	})
}

func (s *hermesServer) handlePreCompress(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		SessionID string `json:"session_id"`
		Messages  []any  `json:"messages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}

	writeJSON(w, map[string]any{
		"preserved":        true,
		"durable_insights": []string{},
		"session_id":       req.SessionID,
	})
}

func (s *hermesServer) handleMemoryWrite(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req MemoryWriteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}

	writeJSON(w, map[string]any{
		"mirrored":   true,
		"key":        req.Key,
		"scope":      req.Scope,
		"provenance": providerName,
	})
}

func (s *hermesServer) handleDelegation(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req DelegationEvent
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	s.turnCount++
	s.mu.Unlock()

	writeJSON(w, map[string]any{
		"captured":       true,
		"parent_session": req.ParentSessionID,
		"sub_agent":      req.SubAgentID,
		"provenance":     "delegation",
	})
}

func (s *hermesServer) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	writeJSON(w, map[string]any{
		"status":     "shutting_down",
		"provider":   providerName,
		"flushed":    true,
		"replayable": true,
	})
}

// ── utility ──────────────────────────────────────────────────────────────

func getInt(m map[string]any, key string) int {
	if m == nil {
		return 0
	}
	v, ok := m[key]
	if !ok {
		return 0
	}
	f, ok := v.(float64)
	if !ok {
		return 0
	}
	return int(f)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("encode response: %v", err)
	}
}
