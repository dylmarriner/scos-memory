package main

import "github.com/dylmarriner/scos-memory/packages/release"

import "github.com/dylmarriner/scos-memory/packages/auth"
import "github.com/dylmarriner/scos-memory/packages/tlsserve"

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/dylmarriner/scos-memory/packages/database"
	hooksdk "github.com/dylmarriner/scos-memory/packages/hook-sdk"
	"github.com/dylmarriner/scos-memory/services/readiness"
	_ "github.com/jackc/pgx/v5/stdlib"
)

const version = "0.1.0"
const providerName = "scos-nexus"
const recallLimit = 20

// sessionSwitchReasons are the lineage cases session_switch distinguishes.
var sessionSwitchReasons = map[string]bool{"resume": true, "branch": true, "reset": true, "compression": true, "switch": true}

// hermesContexts are the §24.6 execution contexts a turn is attributed to.
var hermesContexts = map[string]bool{"primary": true, "subagent": true, "cron": true, "flush": true}

func resolveHermesContext(v string) (string, error) {
	if v == "" {
		return "primary", nil
	}
	if !hermesContexts[v] {
		return "", fmt.Errorf("context must be one of primary, subagent, cron, flush")
	}
	return v, nil
}

// shutdownFlushTimeout bounds how long shutdown spends flushing queued work.
const shutdownFlushTimeout = 5 * time.Second

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
	TenantID    string `json:"tenant_id,omitempty"`
	PrincipalID string `json:"principal_id,omitempty"`
	NamespaceID string `json:"namespace_id"`
	SessionID   string `json:"session_id"`
	ProfilePath string `json:"profile_path"`
	LastCapsule string `json:"last_capsule_id,omitempty"`
}

type SyncTurnRequest struct {
	TenantID    string `json:"tenant_id,omitempty"`
	PrincipalID string `json:"principal_id,omitempty"`
	NamespaceID string `json:"namespace_id"`
	SessionID   string `json:"session_id"`
	TurnID      string `json:"turn_id"`
	Messages    []any  `json:"messages"`
	ToolCalls   []any  `json:"tool_calls,omitempty"`
	ProfilePath string `json:"profile_path"`
	// Context distinguishes §24.6 primary, subagent, cron and flush turns.
	Context string `json:"context,omitempty"`
}

type MemoryWriteRequest struct {
	TenantID    string `json:"tenant_id,omitempty"`
	PrincipalID string `json:"principal_id,omitempty"`
	NamespaceID string `json:"namespace_id"`
	Key         string `json:"key"`
	Value       string `json:"value"`
	Scope       string `json:"scope"`
	Source      string `json:"source"`
	ProfilePath string `json:"profile_path,omitempty"`
}

type DelegationEvent struct {
	TenantID        string `json:"tenant_id,omitempty"`
	PrincipalID     string `json:"principal_id,omitempty"`
	ParentSessionID string `json:"parent_session_id"`
	SubAgentID      string `json:"sub_agent_id"`
	Task            string `json:"task"`
	Result          string `json:"result,omitempty"`
}

type StoredTurn struct {
	SessionID string `json:"session_id"`
	TurnID    string `json:"turn_id"`
	Messages  []any  `json:"messages"`
	ToolCalls []any  `json:"tool_calls,omitempty"`
	CreatedAt string `json:"created_at"`
}

// ── server state ─────────────────────────────────────────────────────────

type hermesServer struct {
	startTime time.Time
	db        *sql.DB
	// prefetchQueue holds bounded pre-warm work for queue_prefetch (§24.3);
	// runPrefetchWorker processes it into warmCache.
	prefetchQueue  prefetchQueue
	prefetchSignal chan struct{}
	warmCache      warmCache
	prefetchWarmed atomic.Int64
	prefetchFailed atomic.Int64
	// memoryIntelligenceURL and upstreamClient serve the §24.5 tools that
	// memory-intelligence owns.
	memoryIntelligenceURL string
	upstreamClient        *http.Client
	// hooks raises §22.6 lifecycle events; nil when no extension manager is
	// configured (Notify is then a no-op).
	hooks *hooksdk.Client
}

// notify raises a lifecycle hook after the triggering work has committed.
func (s *hermesServer) notify(ctx context.Context, event string, id requestIdentity, payload map[string]any) {
	s.hooks.Notify(ctx, event, id.TenantID, id.PrincipalID, payload)
}

func (s *hermesServer) readinessCheck() error {
	if os.Getenv("DATABASE_URL") == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}
	if s.db == nil {
		return fmt.Errorf("postgres dependency is not configured")
	}
	return s.db.PingContext(context.Background())
}

func newHermesServer() *hermesServer {
	return &hermesServer{startTime: time.Now(), prefetchSignal: make(chan struct{}, 1)}
}

// ── identity resolution ──────────────────────────────────────────────────

// resolveIdentity determines the tenant/principal a request acts as. Claims
// carried on an authenticated bearer token take precedence; internal
// SCOS-Tenant/Principal headers are the fallback used by direct
// service-to-service calls and by tests that invoke handlers without the
// auth middleware in front of them. A caller-supplied tenant/principal on
// the JSON body must agree with whichever identity was authenticated.
func resolveIdentity(r *http.Request, bodyTenant, bodyPrincipal string) (requestIdentity, int, string) {
	if claims, ok := auth.CanonicalClaims(r.Context()); ok {
		if (bodyTenant != "" && bodyTenant != claims.TenantID) || (bodyPrincipal != "" && bodyPrincipal != claims.PrincipalID) {
			return requestIdentity{}, http.StatusForbidden, "request identity does not match authenticated identity"
		}
		return requestIdentity{TenantID: claims.TenantID, PrincipalID: claims.PrincipalID}, 0, ""
	}
	if auth.Identity(r.Context()) != "" {
		return requestIdentity{}, http.StatusUnauthorized, "authenticated token has no tenant and principal claims"
	}
	tenantID := strings.TrimSpace(r.Header.Get("X-SCOS-Tenant-ID"))
	principalID := strings.TrimSpace(r.Header.Get("X-SCOS-Principal-ID"))
	if tenantID == "" {
		tenantID = bodyTenant
	}
	if principalID == "" {
		principalID = bodyPrincipal
	}
	if tenantID == "" || principalID == "" {
		return requestIdentity{}, http.StatusUnauthorized, "tenant_id and principal_id are required"
	}
	return requestIdentity{TenantID: tenantID, PrincipalID: principalID}, 0, ""
}

// ── main ─────────────────────────────────────────────────────────────────

func main() {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		log.Fatal("DATABASE_URL is required")
	}
	if err := database.RequireTLS(databaseURL); err != nil {
		log.Fatal(err)
	}
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		log.Fatalf("connect to database: %v", err)
	}
	database.TuneSQLDB(db)
	defer db.Close()

	srv := newHermesServer()
	srv.db = db
	srv.memoryIntelligenceURL = strings.TrimRight(strings.TrimSpace(os.Getenv("SCOS_MEMORY_INTELLIGENCE_URL")), "/")
	if srv.memoryIntelligenceURL == "" {
		srv.memoryIntelligenceURL = "https://memory-intelligence:8089"
	}
	if srv.upstreamClient, err = tlsserve.TrustedClient(15 * time.Second); err != nil {
		log.Fatalf("memory-intelligence client: %v", err)
	}
	if srv.hooks, err = hooksdk.ClientFromEnvironment("hermes-provider"); err != nil {
		log.Fatalf("lifecycle hook client: %v", err)
	}
	workerCtx, stopWorker := context.WithCancel(context.Background())
	defer stopWorker()
	go srv.runPrefetchWorker(workerCtx)

	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", healthOK)
	mux.HandleFunc("/health/version", release.Handler("hermes-provider"))
	mux.HandleFunc("/health/ready", readiness.Handler(func() error {
		return srv.readinessCheck()
	}))

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
	mux.HandleFunc("/v1/provider/queue_prefetch", srv.handleQueuePrefetch)
	mux.HandleFunc("/v1/provider/turn_start", srv.handleTurnStart)
	mux.HandleFunc("/v1/provider/session_end", srv.handleSessionEnd)
	mux.HandleFunc("/v1/provider/tool_call", srv.handleToolCall)
	mux.HandleFunc("/v1/provider/backup_paths", srv.handleBackupPaths)
	mux.HandleFunc("/v1/provider/config_schema", srv.handleConfigSchema)
	mux.HandleFunc("/v1/provider/save_config", srv.handleSaveConfig)

	addr := os.Getenv("SCOS_HERMES_PROVIDER_HTTP_ADDR")
	if addr == "" {
		addr = ":8088"
	}
	log.Printf("hermes memory provider (%s) listening on %s", providerName, addr)
	if err := tlsserve.ListenAndServe(addr, auth.AuthenticatedHandler(mux, auth.MustFromEnvironment())); err != nil {
		log.Fatal(err)
	}
}

func healthOK(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// ── lifecycle endpoints ─────────────────────────────────────────────────

func (s *hermesServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	healthy := s.readinessCheck() == nil
	writeJSON(w, ProviderStatus{
		Name:        providerName,
		Version:     version,
		IsAvailable: healthy,
		Healthy:     healthy,
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
	created, err := s.ensureSession(ctx, tx, identity, req.SessionID, req.NamespaceID)
	if err != nil {
		http.Error(w, "ensure session: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if created {
		s.notify(ctx, hooksdk.OnSessionStart, identity, map[string]any{"session_id": req.SessionID, "namespace_id": req.NamespaceID, "source": "prefetch"})
	}

	// A fresh transaction: the one above already committed, so its
	// transaction-local scos.tenant_id/scos.principal_id (set true, i.e.
	// scoped to that transaction) no longer apply on this connection or any
	// other one recentTurns' query might land on. Without re-setting it
	// here, RLS's fail-closed policies see an empty current_setting() and
	// silently return zero rows regardless of what's actually stored.
	readTx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		http.Error(w, "begin transaction: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer readTx.Rollback()
	if err := setPrincipalContext(ctx, readTx, identity); err != nil {
		http.Error(w, "set principal context: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Pre-warmed recall is served only when it is fresh and was computed
	// for this same tenant, principal and session; cache_hit says which
	// path answered rather than echoing what the client sent.
	if entry, ok := s.warmCache.fresh(warmKey(identity.TenantID, identity.PrincipalID, req.ProfilePath, req.SessionID), time.Now()); ok {
		writeJSON(w, map[string]any{
			"prefetched": true,
			"session_id": req.SessionID,
			"recall":     entry.Recall,
			"memories":   entry.Memories,
			"cache_hit":  true,
			"warmed_at":  entry.WarmedAt.UTC().Format(time.RFC3339),
		})
		return
	}
	recall, err := recentTurns(ctx, readTx, identity.TenantID, req.SessionID, recallLimit)
	if err != nil {
		http.Error(w, "load recall: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]any{
		"prefetched": true,
		"session_id": req.SessionID,
		"recall":     recall,
		"cache_hit":  false,
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
	if req.SessionID == "" || req.TurnID == "" {
		http.Error(w, "session_id and turn_id are required", http.StatusBadRequest)
		return
	}
	turnContext, err := resolveHermesContext(req.Context)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
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

	idemKey := "hermes_sync_turn:" + req.SessionID + ":" + req.TurnID
	requestHash := sha256Hex(req.SessionID, req.TurnID)
	var replayed map[string]any
	found, err := idempotentReplay(ctx, tx, identity, idemKey, requestHash, &replayed)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if found {
		if err := tx.Commit(); err != nil {
			http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
			return
		}
		replayed["duplicate"] = true
		writeJSON(w, replayed)
		return
	}

	sessionCreated, err := s.ensureSession(ctx, tx, identity, req.SessionID, req.NamespaceID)
	if err != nil {
		http.Error(w, "ensure session: "+err.Error(), http.StatusBadRequest)
		return
	}
	// The stored turn is the client's turn: its own id when that is a UUID,
	// otherwise a stable id derived from (session, client turn id), so the
	// same Hermes turn always maps to the same journal turn.
	storedTurnID := req.TurnID
	if !isUUID(storedTurnID) {
		storedTurnID = derivedTurnID(req.SessionID, req.TurnID)
	}
	// Tool calls are part of the turn (§24.2 sync_turn ingests the complete
	// turn); they are journalled as tool messages after the conversation.
	messages := append([]any{}, req.Messages...)
	for _, call := range req.ToolCalls {
		encoded, err := json.Marshal(call)
		if err != nil {
			http.Error(w, "encode tool call: "+err.Error(), http.StatusBadRequest)
			return
		}
		messages = append(messages, map[string]any{"role": "tool", "content": string(encoded)})
	}
	if err := insertTurnWithMessages(ctx, tx, identity, req.SessionID, storedTurnID, messages); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := enqueueConsolidation(ctx, tx, identity, req.SessionID, storedTurnID, map[string]string{"context": turnContext, "profile_path": req.ProfilePath}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	result := map[string]any{
		"synced":         true,
		"session_id":     req.SessionID,
		"turn_id":        storedTurnID,
		"client_turn_id": req.TurnID,
		"context":        turnContext,
		"tool_calls":     len(req.ToolCalls),
		// Durable before the response: the turn is committed in this
		// request, so an acknowledged sync can never be lost.
		"async":     false,
		"duplicate": false,
	}
	if err := recordIdempotent(ctx, tx, identity, idemKey, "hermes_sync_turn", requestHash, result); err != nil {
		http.Error(w, "record idempotency: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Pre-warmed recall for this session predates the new turn.
	s.warmCache.evictSession(identity.TenantID, req.SessionID)
	if sessionCreated {
		s.notify(ctx, hooksdk.OnSessionStart, identity, map[string]any{"session_id": req.SessionID, "namespace_id": req.NamespaceID, "source": "sync_turn"})
	}
	writeJSON(w, result)
}

func (s *hermesServer) handleToolSchemas(w http.ResponseWriter, r *http.Request) {
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
		{
			"name":        "scos_explain",
			"description": "Explain why a memory is held and what evidence supports it",
			"input_schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"memory_id": map[string]any{"type": "string", "description": "Memory ID to explain"},
				},
				"required": []string{"memory_id"},
			},
		},
		{
			"name":        "scos_user_model",
			"description": "Inspect the stored user representation",
			"input_schema": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			"name":        "scos_memory_graph",
			"description": "Inspect memory entities and their relationships",
			"input_schema": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			"name":        "scos_correct",
			"description": "Correct a stored memory, preserving the superseded version",
			"input_schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"memory_id": map[string]any{"type": "string", "description": "Memory ID to correct"},
					"value":     map[string]any{"type": "string", "description": "Corrected value"},
				},
				"required": []string{"memory_id", "value"},
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
		TenantID      string `json:"tenant_id,omitempty"`
		PrincipalID   string `json:"principal_id,omitempty"`
		NamespaceID   string `json:"namespace_id"`
		FromSessionID string `json:"from_session_id"`
		ToSessionID   string `json:"to_session_id"`
		ProfilePath   string `json:"profile_path"`
		// Reason distinguishes the §24.2 lineage cases: resume, branch,
		// reset or compression (or a plain switch).
		Reason string `json:"reason,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.ToSessionID == "" {
		http.Error(w, "to_session_id is required", http.StatusBadRequest)
		return
	}
	if req.Reason == "" {
		req.Reason = "switch"
	}
	if !sessionSwitchReasons[req.Reason] {
		http.Error(w, "reason must be one of resume, branch, reset, compression, switch", http.StatusBadRequest)
		return
	}
	if req.FromSessionID != "" && !isUUID(req.FromSessionID) {
		http.Error(w, "from_session_id must be a UUID", http.StatusBadRequest)
		return
	}
	if !isUUID(req.ToSessionID) {
		http.Error(w, "to_session_id must be a UUID", http.StatusBadRequest)
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
	toCreated, err := s.ensureSession(ctx, tx, identity, req.ToSessionID, req.NamespaceID)
	if err != nil {
		http.Error(w, "ensure session: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.FromSessionID != "" {
		if err := appendEvent(ctx, tx, identity.TenantID, req.ToSessionID, "session.switched", "session/"+req.ToSessionID, identity.PrincipalID,
			map[string]string{"from_session_id": req.FromSessionID, "to_session_id": req.ToSessionID, "reason": req.Reason}); err != nil {
			http.Error(w, "record switch event: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if toCreated {
		s.notify(ctx, hooksdk.OnSessionStart, identity, map[string]any{"session_id": req.ToSessionID, "namespace_id": req.NamespaceID, "source": "session_switch"})
	}
	s.notify(ctx, hooksdk.OnSessionSwitch, identity, map[string]any{"from_session_id": req.FromSessionID, "to_session_id": req.ToSessionID, "reason": req.Reason})
	// Lineage is preserved only when there was a prior session to link.
	lineage := ""
	if req.FromSessionID != "" {
		lineage = req.FromSessionID + " -> " + req.ToSessionID
	}
	writeJSON(w, map[string]any{
		"switched":  true,
		"reason":    req.Reason,
		"lineage":   lineage,
		"preserved": req.FromSessionID != "",
	})
}

func (s *hermesServer) handlePreCompress(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		TenantID    string `json:"tenant_id,omitempty"`
		PrincipalID string `json:"principal_id,omitempty"`
		NamespaceID string `json:"namespace_id"`
		SessionID   string `json:"session_id"`
		Messages    []any  `json:"messages"`
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
	if _, err := s.ensureSession(ctx, tx, identity, req.SessionID, req.NamespaceID); err != nil {
		http.Error(w, "ensure session: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.notify(ctx, hooksdk.OnPreCompress, identity, map[string]any{"session_id": req.SessionID, "messages": len(req.Messages)})
	generatedTurnID, err := newID()
	if err != nil {
		http.Error(w, "generate turn id: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := insertTurnWithMessages(ctx, tx, identity, req.SessionID, generatedTurnID, req.Messages); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// The messages about to be compressed away go through consolidation
	// like any turn, so durable facts in them become candidate memories.
	if err := enqueueConsolidation(ctx, tx, identity, req.SessionID, generatedTurnID, map[string]string{"context": "primary", "trigger": "pre_compress"}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	insightID := generatedTurnID
	if err := appendEvent(ctx, tx, identity.TenantID, req.SessionID, "session.pre_compress_insight", "session/"+req.SessionID, identity.PrincipalID,
		map[string]string{"turn_id": insightID}); err != nil {
		http.Error(w, "record insight event: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
		return
	}

	s.notify(ctx, hooksdk.OnPostCompress, identity, map[string]any{"session_id": req.SessionID, "preserved_turn_id": insightID})
	writeJSON(w, map[string]any{
		"preserved":        true,
		"durable_insights": []string{insightID},
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
	if req.Key == "" || req.NamespaceID == "" {
		http.Error(w, "key and namespace_id are required", http.StatusBadRequest)
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
	memoryID, versionNumber, err := upsertMemory(ctx, tx, identity, req.NamespaceID, req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]any{
		"mirrored":       true,
		"key":            req.Key,
		"scope":          req.Scope,
		"provenance":     providerName,
		"memory_id":      memoryID,
		"version_number": versionNumber,
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
	if req.ParentSessionID == "" || req.SubAgentID == "" {
		http.Error(w, "parent_session_id and sub_agent_id are required", http.StatusBadRequest)
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
	aggregateID := req.ParentSessionID
	if !isUUID(aggregateID) {
		generated, genErr := newID()
		if genErr != nil {
			http.Error(w, "generate aggregate id: "+genErr.Error(), http.StatusInternalServerError)
			return
		}
		aggregateID = generated
	}
	if err := appendEvent(ctx, tx, identity.TenantID, aggregateID, "session.delegation", "session/"+req.ParentSessionID, identity.PrincipalID,
		map[string]string{"parent_session_id": req.ParentSessionID, "sub_agent_id": req.SubAgentID, "task": req.Task, "result": req.Result}); err != nil {
		http.Error(w, "record delegation event: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// A delegation without a result is its start; one carrying the result
	// is its end.
	event := hooksdk.OnDelegationStart
	if req.Result != "" {
		event = hooksdk.OnDelegationEnd
	}
	s.notify(ctx, event, identity, map[string]any{"parent_session_id": req.ParentSessionID, "sub_agent_id": req.SubAgentID, "task": req.Task})
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
	// Flush: process queued pre-warm work now, bounded so shutdown cannot
	// hang on a slow backend. Whatever does not complete is handed back to
	// the caller, which can replay it through queue_prefetch after restart.
	ctx, cancel := context.WithTimeout(r.Context(), shutdownFlushTimeout)
	defer cancel()
	var pending []queuedPrefetch
	processed := 0
	for _, item := range s.prefetchQueue.drain() {
		if ctx.Err() != nil {
			pending = append(pending, item)
			continue
		}
		if err := s.warm(ctx, item); err != nil {
			pending = append(pending, item)
			continue
		}
		processed++
	}
	if pending == nil {
		pending = []queuedPrefetch{}
	}
	writeJSON(w, map[string]any{
		"status":           "shutting_down",
		"provider":         providerName,
		"flushed":          len(pending) == 0,
		"processed":        processed,
		"replayable":       pending,
		"replayable_count": len(pending),
	})
}

// ── utility ──────────────────────────────────────────────────────────────

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
				return false
			}
		}
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("encode response: %v", err)
	}
}
