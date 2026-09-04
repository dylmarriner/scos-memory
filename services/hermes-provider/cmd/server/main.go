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
	"time"

	"github.com/dylmarriner/scos-memory/packages/database"
	"github.com/dylmarriner/scos-memory/services/readiness"
	_ "github.com/jackc/pgx/v5/stdlib"
)

const version = "0.1.0"
const providerName = "scos-nexus"
const recallLimit = 20

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
	// prefetchQueue holds bounded pre-warm work for queue_prefetch (§24.3).
	prefetchQueue prefetchQueue
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
	return &hermesServer{startTime: time.Now()}
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
	if err := s.ensureSession(ctx, tx, identity, req.SessionID, req.NamespaceID); err != nil {
		http.Error(w, "ensure session: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
		return
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
	recall, err := recentTurns(ctx, readTx, identity.TenantID, req.SessionID, recallLimit)
	if err != nil {
		http.Error(w, "load recall: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]any{
		"prefetched": true,
		"session_id": req.SessionID,
		"recall":     recall,
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
	if req.SessionID == "" || req.TurnID == "" {
		http.Error(w, "session_id and turn_id are required", http.StatusBadRequest)
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

	if err := s.ensureSession(ctx, tx, identity, req.SessionID, req.NamespaceID); err != nil {
		http.Error(w, "ensure session: "+err.Error(), http.StatusBadRequest)
		return
	}
	generatedTurnID, err := newID()
	if err != nil {
		http.Error(w, "generate turn id: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := insertTurnWithMessages(ctx, tx, identity, req.SessionID, generatedTurnID, req.Messages); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	result := map[string]any{
		"synced":     true,
		"session_id": req.SessionID,
		"turn_id":    req.TurnID,
		"async":      false,
		"duplicate":  false,
	}
	if err := recordIdempotent(ctx, tx, identity, idemKey, "hermes_sync_turn", requestHash, result); err != nil {
		http.Error(w, "record idempotency: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
		return
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
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.ToSessionID == "" {
		http.Error(w, "to_session_id is required", http.StatusBadRequest)
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
	if err := s.ensureSession(ctx, tx, identity, req.ToSessionID, req.NamespaceID); err != nil {
		http.Error(w, "ensure session: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.FromSessionID != "" {
		if err := appendEvent(ctx, tx, identity.TenantID, req.ToSessionID, "session.switched", "session/"+req.ToSessionID, identity.PrincipalID,
			map[string]string{"from_session_id": req.FromSessionID, "to_session_id": req.ToSessionID}); err != nil {
			http.Error(w, "record switch event: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
		return
	}

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
	if err := s.ensureSession(ctx, tx, identity, req.SessionID, req.NamespaceID); err != nil {
		http.Error(w, "ensure session: "+err.Error(), http.StatusBadRequest)
		return
	}
	generatedTurnID, err := newID()
	if err != nil {
		http.Error(w, "generate turn id: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := insertTurnWithMessages(ctx, tx, identity, req.SessionID, generatedTurnID, req.Messages); err != nil {
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
	flushed := s.readinessCheck() == nil
	writeJSON(w, map[string]any{
		"status":     "shutting_down",
		"provider":   providerName,
		"flushed":    flushed,
		"replayable": true,
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
