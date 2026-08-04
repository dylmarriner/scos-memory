package main

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
	"github.com/dylmarriner/scos-memory/packages/telemetry"
	"github.com/dylmarriner/scos-memory/services/readiness"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// serviceMetrics emits this service's §32 measures. It is nil-safe and a
// no-op when no collector is configured.
var serviceMetrics = telemetry.FromEnvironment("compatibility-gateway")

const version = "0.1.0"

var startTime = time.Now()

func handleVersion(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":         "ok",
		"version":        version,
		"uptime_seconds": int(time.Since(startTime).Seconds()),
	})
}

// ── types ────────────────────────────────────────────────────────────────

type Adapter struct {
	Boundary     string   `json:"boundary"`
	Name         string   `json:"name"`
	Tier         string   `json:"tier"` // A, B, C, D
	Capabilities []string `json:"capabilities"`
	Supported    []string `json:"supported_operations"`
	Unsupported  []string `json:"unsupported_operations"`
	HealthStatus string   `json:"health_status"`
	// Tested records whether this boundary has passing conformance evidence
	// (ss23.7). Boundaries without conformance evidence SHALL be labelled
	// experimental rather than presented as tested compatibility.
	Tested bool `json:"tested"`
}

// requiredAdapterOperations is the full agent adapter contract from
// governance ss23.4. Every registered adapter must classify each of these
// operations as either supported or explicitly unsupported.
var requiredAdapterOperations = []string{
	"detect", "install", "configure", "health",
	"identify_user", "identify_agent", "identify_session",
	"capture_ingress", "inject_context",
	"observe_model_request", "observe_model_response",
	"observe_tool_call", "observe_tool_result",
	"commit_turn", "checkpoint", "resume", "shutdown", "uninstall",
}

type AgentRegistration struct {
	TenantID     string   `json:"tenant_id,omitempty"`
	PrincipalID  string   `json:"principal_id,omitempty"`
	AgentID      string   `json:"agent_id"`
	Name         string   `json:"name"`
	Boundary     string   `json:"boundary"`
	Tier         string   `json:"tier"`
	Adapters     []string `json:"adapters"`
	Capabilities []string `json:"capabilities"`
	Status       string   `json:"status"`
	LastSeen     string   `json:"last_seen"`
}

type ContextEnvelope struct {
	TenantID             string   `json:"tenant_id,omitempty"`
	PrincipalID          string   `json:"principal_id,omitempty"`
	AgentID              string   `json:"agent_id"`
	SessionID            string   `json:"session_id"`
	GoverningConstraints string   `json:"governing_constraints,omitempty"`
	UserModel            string   `json:"user_model,omitempty"`
	Facts                []string `json:"facts,omitempty"`
	Learnings            []string `json:"learnings,omitempty"`
	TaskState            string   `json:"task_state,omitempty"`
	EpisodicHistory      []string `json:"episodic_history,omitempty"`
	Contradictions       []string `json:"contradictions,omitempty"`
	CodeRefs             []string `json:"code_references,omitempty"`
	Artifacts            []string `json:"artifacts,omitempty"`
	Sources              []string `json:"sources,omitempty"`
	RestorationRefs      []string `json:"restoration_references,omitempty"`
	Format               string   `json:"format"` // native, markdown, json, text
}

type AdapterContract struct {
	Operation string   `json:"operation"`
	Input     []string `json:"input_fields"`
	Output    []string `json:"output_fields"`
	Required  bool     `json:"required"`
}

// ── server state ─────────────────────────────────────────────────────────

// compatServer holds the static adapter catalog (compiled into the binary;
// there is no write path that mutates it, so it needs no lock) plus a
// connection to the durable store for the two things that actually change at
// runtime: agent registrations and context envelopes.
type compatServer struct {
	adapters map[string]*Adapter // boundary -> adapter, immutable after construction
	db       *sql.DB
}

func (s *compatServer) readinessCheck() error {
	if os.Getenv("DATABASE_URL") == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}
	if s.db == nil {
		return fmt.Errorf("postgres dependency is not configured")
	}
	return s.db.PingContext(context.Background())
}

func newCompatServer() *compatServer {
	s := &compatServer{adapters: make(map[string]*Adapter)}
	s.registerDefaultAdapters()
	return s
}

func (s *compatServer) registerDefaultAdapters() {
	s.adapters["native_sdk"] = &Adapter{
		Boundary:     "native_sdk",
		Name:         "Native Go SDK",
		Tier:         "A",
		Capabilities: []string{"full_ingress", "retrieval", "tools", "completion", "checkpoint", "recovery"},
		Supported: []string{"detect", "install", "configure", "health", "identify_user", "identify_agent",
			"identify_session", "capture_ingress", "inject_context", "observe_model_request",
			"observe_model_response", "observe_tool_call", "observe_tool_result", "commit_turn",
			"checkpoint", "resume", "shutdown", "uninstall"},
		Tested: true,
	}

	s.adapters["http_sidecar"] = &Adapter{
		Boundary:     "http_sidecar",
		Name:         "HTTP Sidecar",
		Tier:         "B",
		Capabilities: []string{"pre_turn_recall", "post_turn_persistence", "lifecycle_hooks"},
		Supported: []string{"detect", "install", "configure", "health", "identify_user", "identify_agent",
			"identify_session", "capture_ingress", "inject_context", "commit_turn", "checkpoint",
			"shutdown", "uninstall"},
		Unsupported: []string{"observe_model_request", "observe_model_response", "observe_tool_call",
			"observe_tool_result", "resume"},
		Tested: true,
	}

	s.adapters["mcp_server"] = &Adapter{
		Boundary:     "mcp_server",
		Name:         "MCP Server",
		Tier:         "C",
		Capabilities: []string{"tool_recall", "automatic_transcript_capture"},
		Supported: []string{"detect", "install", "configure", "health", "identify_user", "identify_agent",
			"identify_session", "capture_ingress", "inject_context", "observe_tool_call",
			"observe_tool_result", "commit_turn", "shutdown", "uninstall"},
		Unsupported: []string{"observe_model_request", "observe_model_response", "checkpoint", "resume"},
		Tested:      true,
	}

	s.adapters["cli_wrapper"] = &Adapter{
		Boundary:     "cli_wrapper",
		Name:         "CLI Wrapper",
		Tier:         "C",
		Capabilities: []string{"transcript_capture", "context_injection"},
		Supported: []string{"detect", "install", "configure", "health", "identify_user", "identify_agent",
			"identify_session", "capture_ingress", "inject_context", "commit_turn", "shutdown", "uninstall"},
		Unsupported: []string{"observe_model_request", "observe_model_response", "observe_tool_call",
			"observe_tool_result", "checkpoint", "resume"},
		Tested: true,
	}

	s.adapters["session_watcher"] = &Adapter{
		Boundary:     "session_watcher",
		Name:         "Session/File Watcher",
		Tier:         "D",
		Capabilities: []string{"passive_transcript_ingestion", "next_session_context"},
		Supported:    []string{"detect", "install", "configure", "health", "capture_ingress", "inject_context", "shutdown", "uninstall"},
		Unsupported: []string{"identify_user", "identify_agent", "identify_session", "observe_model_request",
			"observe_model_response", "observe_tool_call", "observe_tool_result", "commit_turn", "checkpoint", "resume"},
		Tested: true,
	}

	s.adapters["model_api_proxy"] = &Adapter{
		Boundary:     "model_api_proxy",
		Name:         "Model API Proxy",
		Tier:         "C",
		Capabilities: []string{"tool_recall", "automatic_transcript_capture"},
		Supported: []string{"detect", "install", "configure", "health", "capture_ingress",
			"inject_context", "observe_model_request", "observe_model_response", "commit_turn",
			"shutdown", "uninstall"},
		Unsupported: []string{"identify_user", "identify_agent", "identify_session",
			"observe_tool_call", "observe_tool_result", "checkpoint", "resume"},
		Tested: false, // experimental: no conformance evidence yet, ss23.7
	}

	s.adapters["provider_plugin"] = &Adapter{
		Boundary:     "provider_plugin",
		Name:         "Provider Plugin",
		Tier:         "B",
		Capabilities: []string{"pre_turn_recall", "post_turn_persistence", "lifecycle_hooks"},
		Supported: []string{"detect", "install", "configure", "health", "identify_user", "identify_agent",
			"identify_session", "capture_ingress", "inject_context", "commit_turn", "shutdown", "uninstall"},
		Unsupported: []string{"observe_model_request", "observe_model_response", "observe_tool_call",
			"observe_tool_result", "checkpoint", "resume"},
		Tested: false, // experimental: no conformance evidence yet, ss23.7
	}

	s.adapters["lifecycle_hooks"] = &Adapter{
		Boundary:     "lifecycle_hooks",
		Name:         "Lifecycle Hooks",
		Tier:         "B",
		Capabilities: []string{"pre_turn_recall", "post_turn_persistence", "lifecycle_hooks"},
		Supported: []string{"detect", "install", "configure", "health", "identify_session",
			"capture_ingress", "inject_context", "observe_tool_call", "observe_tool_result",
			"commit_turn", "shutdown", "uninstall"},
		Unsupported: []string{"identify_user", "identify_agent", "observe_model_request",
			"observe_model_response", "checkpoint", "resume"},
		Tested: false, // experimental: no conformance evidence yet, ss23.7
	}

	s.adapters["event_bridge"] = &Adapter{
		Boundary:     "event_bridge",
		Name:         "Event Bridge",
		Tier:         "C",
		Capabilities: []string{"tool_recall", "automatic_transcript_capture"},
		Supported:    []string{"detect", "install", "configure", "health", "capture_ingress", "inject_context", "commit_turn", "shutdown", "uninstall"},
		Unsupported: []string{"identify_user", "identify_agent", "identify_session", "observe_model_request",
			"observe_model_response", "observe_tool_call", "observe_tool_result", "checkpoint", "resume"},
		Tested: false, // experimental: no conformance evidence yet, ss23.7
	}

	s.adapters["a2a_adapter"] = &Adapter{
		Boundary:     "a2a_adapter",
		Name:         "A2A Adapter",
		Tier:         "B",
		Capabilities: []string{"pre_turn_recall", "post_turn_persistence", "lifecycle_hooks"},
		Supported: []string{"detect", "install", "configure", "health", "identify_agent",
			"identify_session", "capture_ingress", "inject_context", "commit_turn", "shutdown", "uninstall"},
		Unsupported: []string{"identify_user", "observe_model_request", "observe_model_response",
			"observe_tool_call", "observe_tool_result", "checkpoint", "resume"},
		Tested: false, // experimental: no conformance evidence yet, ss23.7
	}
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
	defer db.Close()

	srv := newCompatServer()
	srv.db = db

	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", healthOK)
	mux.HandleFunc("/health/ready", readiness.Handler(func() error {
		return srv.readinessCheck()
	}))
	mux.HandleFunc("/v1/version", handleVersion)

	// Adapters
	mux.HandleFunc("/v1/adapters", srv.handleAdapters)
	mux.HandleFunc("/v1/adapters/", srv.handleAdapterByBoundary)
	mux.HandleFunc("/v1/compatibility", srv.handleCompatibility)

	// Agents
	mux.HandleFunc("/v1/agents", srv.handleAgents)
	mux.HandleFunc("/v1/agents/", srv.handleAgentByID)

	// Context envelope
	mux.HandleFunc("/v1/envelope", srv.handleEnvelope)
	mux.HandleFunc("/v1/envelope/render", srv.handleRender)

	// Translation
	mux.HandleFunc("/v1/translate", srv.handleTranslate)

	addr := os.Getenv("SCOS_COMPATIBILITY_GATEWAY_HTTP_ADDR")
	if addr == "" {
		addr = ":8087"
	}
	log.Printf("compatibility gateway listening on %s", addr)
	if err := tlsserve.ListenAndServe(addr, auth.AuthenticatedHandler(mux, auth.MustFromEnvironment())); err != nil {
		log.Fatal(err)
	}
}

func healthOK(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// ── adapters ─────────────────────────────────────────────────────────────

// adapterHealthStatus derives a boundary's health from real signals rather
// than a fixed claim (ss42 prohibits fabricated operational state): untested
// boundaries (ss23.7) are reported as experimental regardless of traffic, and
// tested boundaries are "healthy" only if a real agent has registered
// against them and reported activity within the last 24 hours; otherwise
// they are truthfully reported as having no recent traffic rather than
// unconditionally "healthy".
const adapterHealthWindow = 24 * time.Hour

func (s *compatServer) adapterHealthStatus(ctx context.Context, tenantID, boundary string, tested bool) string {
	if !tested {
		return "experimental_untested"
	}
	var lastSeen sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT MAX(ai.last_seen_at)
		FROM adapter_instances ai
		JOIN agent_adapters aa ON aa.id = ai.adapter_id
		WHERE ai.tenant_id = $1 AND aa.boundary = $2 AND ai.status = 'active'
	`, tenantID, boundary).Scan(&lastSeen)
	if err != nil || !lastSeen.Valid {
		return "no_recent_traffic"
	}
	if time.Since(lastSeen.Time) <= adapterHealthWindow {
		return "healthy"
	}
	return "no_recent_traffic"
}

// snapshotAdapter returns a copy of the adapter with HealthStatus populated
// from current real agent activity for the caller's tenant.
func (s *compatServer) snapshotAdapter(ctx context.Context, tenantID string, a *Adapter) *Adapter {
	snap := *a
	snap.HealthStatus = s.adapterHealthStatus(ctx, tenantID, a.Boundary, a.Tested)
	return &snap
}

func (s *compatServer) handleAdapters(w http.ResponseWriter, r *http.Request) {
	identity, status, msg := resolveIdentity(r, "", "")
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	adapters := make([]*Adapter, 0, len(s.adapters))
	for _, a := range s.adapters {
		snapshot := s.snapshotAdapter(r.Context(), identity.TenantID, a)
		adapters = append(adapters, snapshot)
		// §32 adapter compatibility tier, reported per boundary from the
		// computed snapshot rather than the declared registration.
		serviceMetrics.Gauge(telemetry.MetricAdapterTier, 1, "count", map[string]string{
			"boundary": snapshot.Boundary,
			"tier":     snapshot.Tier,
			"health":   snapshot.HealthStatus,
		}, telemetry.Correlation{Tenant: identity.TenantID})
	}
	writeJSON(w, adapters)
}

func (s *compatServer) handleAdapterByBoundary(w http.ResponseWriter, r *http.Request) {
	boundary := strings.TrimPrefix(r.URL.Path, "/v1/adapters/")
	adapter, ok := s.adapters[boundary]
	if !ok {
		http.Error(w, "adapter not found", http.StatusNotFound)
		return
	}
	identity, status, msg := resolveIdentity(r, "", "")
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	writeJSON(w, s.snapshotAdapter(r.Context(), identity.TenantID, adapter))
}

func (s *compatServer) handleCompatibility(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	identity, status, msg := resolveIdentity(r, "", "")
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	adapters := make([]*Adapter, 0, len(s.adapters))
	for _, adapter := range s.adapters {
		adapters = append(adapters, s.snapshotAdapter(r.Context(), identity.TenantID, adapter))
	}
	agents, err := listAgentInstances(r.Context(), s.db, identity.TenantID)
	if err != nil {
		writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"error": "list agents: " + err.Error()})
		return
	}
	writeJSON(w, map[string]any{
		"status":      "available",
		"adapters":    adapters,
		"agents":      agents,
		"agent_count": len(agents),
	})
}

// ── agents ───────────────────────────────────────────────────────────────

func (s *compatServer) handleAgents(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		identity, status, msg := resolveIdentity(r, "", "")
		if status != 0 {
			http.Error(w, msg, status)
			return
		}
		agents, err := listAgentInstances(r.Context(), s.db, identity.TenantID)
		if err != nil {
			writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"error": "list agents: " + err.Error()})
			return
		}
		writeJSON(w, agents)

	case http.MethodPost:
		var reg AgentRegistration
		if err := json.NewDecoder(r.Body).Decode(&reg); err != nil {
			http.Error(w, "invalid registration: "+err.Error(), http.StatusBadRequest)
			return
		}
		if reg.AgentID == "" {
			http.Error(w, "agent_id required", http.StatusBadRequest)
			return
		}
		identity, status, msg := resolveIdentity(r, reg.TenantID, reg.PrincipalID)
		if status != 0 {
			http.Error(w, msg, status)
			return
		}
		// Auto-detect tier from boundary
		var adapter *Adapter
		if reg.Boundary != "" {
			adapter = s.adapters[reg.Boundary]
			if adapter != nil {
				reg.Tier = adapter.Tier
				reg.Adapters = adapter.Supported
				reg.Capabilities = adapter.Capabilities
			}
		}
		if adapter == nil {
			http.Error(w, "boundary must reference a known adapter", http.StatusBadRequest)
			return
		}
		if reg.Status == "" {
			reg.Status = "active"
		}
		reg.LastSeen = time.Now().UTC().Format(time.RFC3339)

		ctx := r.Context()
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"error": "begin transaction: " + err.Error()})
			return
		}
		defer tx.Rollback()
		if err := setPrincipalContext(ctx, tx, identity); err != nil {
			writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"error": "set tenant context: " + err.Error()})
			return
		}
		adapterRowID, err := ensureAdapterCatalogRow(ctx, tx, identity.TenantID, adapter)
		if err != nil {
			writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
			return
		}
		if err := upsertAgentInstance(ctx, tx, identity.TenantID, adapterRowID, reg); err != nil {
			writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
			return
		}
		if err := tx.Commit(); err != nil {
			writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"error": "commit: " + err.Error()})
			return
		}
		writeJSON(w, reg)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *compatServer) handleAgentByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/agents/")
	identity, status, msg := resolveIdentity(r, "", "")
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	agent, ok, err := getAgentInstance(r.Context(), s.db, identity.TenantID, id)
	if err != nil {
		writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"error": "look up agent: " + err.Error()})
		return
	}
	if !ok {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	writeJSON(w, agent)
}

// ── context envelope ─────────────────────────────────────────────────────

func (s *compatServer) handleEnvelope(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var env ContextEnvelope
		if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
			http.Error(w, "invalid envelope: "+err.Error(), http.StatusBadRequest)
			return
		}
		if env.AgentID == "" || env.SessionID == "" {
			http.Error(w, "agent_id and session_id required", http.StatusBadRequest)
			return
		}
		if env.Format == "" {
			env.Format = "json"
		}
		identity, status, msg := resolveIdentity(r, env.TenantID, env.PrincipalID)
		if status != 0 {
			http.Error(w, msg, status)
			return
		}

		ctx := r.Context()
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"error": "begin transaction: " + err.Error()})
			return
		}
		defer tx.Rollback()
		if err := setPrincipalContext(ctx, tx, identity); err != nil {
			writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"error": "set tenant context: " + err.Error()})
			return
		}
		if err := upsertEnvelope(ctx, tx, identity.TenantID, env); err != nil {
			writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
			return
		}
		if err := tx.Commit(); err != nil {
			writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"error": "commit: " + err.Error()})
			return
		}
		writeJSON(w, env)

	case http.MethodGet:
		sessionID := r.URL.Query().Get("session_id")
		if sessionID == "" {
			http.Error(w, "session_id query param required", http.StatusBadRequest)
			return
		}
		identity, status, msg := resolveIdentity(r, "", "")
		if status != 0 {
			http.Error(w, msg, status)
			return
		}
		env, ok, err := getEnvelope(r.Context(), s.db, identity.TenantID, sessionID)
		if err != nil {
			writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"error": "look up envelope: " + err.Error()})
			return
		}
		if !ok {
			http.Error(w, "envelope not found", http.StatusNotFound)
			return
		}
		writeJSON(w, env)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ── render endpoint ──────────────────────────────────────────────────────

func (s *compatServer) handleRender(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		TenantID     string `json:"tenant_id,omitempty"`
		PrincipalID  string `json:"principal_id,omitempty"`
		SessionID    string `json:"session_id"`
		TargetFormat string `json:"target_format"` // markdown, text, json
		AgentID      string `json:"agent_id,omitempty"`
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

	env, ok, err := getEnvelope(r.Context(), s.db, identity.TenantID, req.SessionID)
	if err != nil {
		writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"error": "look up envelope: " + err.Error()})
		return
	}
	if !ok {
		http.Error(w, "envelope not found", http.StatusNotFound)
		return
	}

	rendered := renderEnvelope(env, req.TargetFormat)
	writeJSON(w, map[string]string{
		"format":  req.TargetFormat,
		"content": rendered,
	})
}

// ── translate endpoint ───────────────────────────────────────────────────

func (s *compatServer) handleTranslate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		EventType    string `json:"event_type"`
		SourceData   any    `json:"source_data"`
		SourceFormat string `json:"source_format"`
		TargetFormat string `json:"target_format"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Translate native event to SCOS common event model
	translated := translateEvent(req.EventType, req.SourceData, req.SourceFormat)
	writeJSON(w, map[string]any{
		"event_type":    req.EventType,
		"translated":    translated,
		"target_format": "scos_common",
	})
}

// ── envelope rendering ───────────────────────────────────────────────────

func renderEnvelope(env *ContextEnvelope, format string) string {
	switch format {
	case "markdown":
		return renderMarkdown(env)
	case "text":
		return renderText(env)
	default:
		return renderJSON(env)
	}
}

func renderMarkdown(env *ContextEnvelope) string {
	var b strings.Builder
	b.WriteString("# Context Envelope\n\n")
	b.WriteString(fmt.Sprintf("**Agent:** %s  \n", env.AgentID))
	b.WriteString(fmt.Sprintf("**Session:** %s  \n", env.SessionID))

	if env.GoverningConstraints != "" {
		b.WriteString("\n## Governing Constraints\n\n")
		b.WriteString(env.GoverningConstraints + "\n")
	}
	if env.UserModel != "" {
		b.WriteString("\n## User Model\n\n")
		b.WriteString(env.UserModel + "\n")
	}
	if len(env.Facts) > 0 {
		b.WriteString("\n## Facts\n\n")
		for _, f := range env.Facts {
			b.WriteString(fmt.Sprintf("- %s\n", f))
		}
	}
	if len(env.Learnings) > 0 {
		b.WriteString("\n## Learnings\n\n")
		for _, l := range env.Learnings {
			b.WriteString(fmt.Sprintf("- %s\n", l))
		}
	}
	if env.TaskState != "" {
		b.WriteString("\n## Task State\n\n")
		b.WriteString(env.TaskState + "\n")
	}
	if len(env.EpisodicHistory) > 0 {
		b.WriteString("\n## Episodic History\n\n")
		for _, h := range env.EpisodicHistory {
			b.WriteString(fmt.Sprintf("- %s\n", h))
		}
	}
	if len(env.Contradictions) > 0 {
		b.WriteString("\n## Contradictions\n\n")
		for _, c := range env.Contradictions {
			b.WriteString(fmt.Sprintf("- ⚠ %s\n", c))
		}
	}
	if len(env.CodeRefs) > 0 {
		b.WriteString("\n## Code\n\n")
		for _, c := range env.CodeRefs {
			b.WriteString(fmt.Sprintf("- `%s`\n", c))
		}
	}
	if len(env.Artifacts) > 0 {
		b.WriteString("\n## Artifacts\n\n")
		for _, a := range env.Artifacts {
			b.WriteString(fmt.Sprintf("- %s\n", a))
		}
	}
	if len(env.Sources) > 0 {
		b.WriteString("\n## Sources\n\n")
		for _, s := range env.Sources {
			b.WriteString(fmt.Sprintf("- `%s`\n", s))
		}
	}
	if len(env.RestorationRefs) > 0 {
		b.WriteString("\n## Restoration References\n\n")
		for _, r := range env.RestorationRefs {
			b.WriteString(fmt.Sprintf("- `%s`\n", r))
		}
	}
	return b.String()
}

func renderText(env *ContextEnvelope) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("Context for Agent %s (Session %s)\n", env.AgentID, env.SessionID))
	b.WriteString("=" + strings.Repeat("=", len(env.AgentID)+len(env.SessionID)+20) + "\n\n")

	if env.GoverningConstraints != "" {
		b.WriteString("GOVERNING CONSTRAINTS:\n" + env.GoverningConstraints + "\n\n")
	}
	if len(env.Facts) > 0 {
		b.WriteString("FACTS:\n")
		for _, f := range env.Facts {
			b.WriteString("  * " + f + "\n")
		}
		b.WriteString("\n")
	}
	if len(env.Learnings) > 0 {
		b.WriteString("LEARNINGS:\n")
		for _, l := range env.Learnings {
			b.WriteString("  * " + l + "\n")
		}
		b.WriteString("\n")
	}
	if env.TaskState != "" {
		b.WriteString("TASK STATE:\n" + env.TaskState + "\n\n")
	}
	if len(env.EpisodicHistory) > 0 {
		b.WriteString("EPISODIC HISTORY:\n")
		for _, h := range env.EpisodicHistory {
			b.WriteString("  * " + h + "\n")
		}
		b.WriteString("\n")
	}
	if len(env.Contradictions) > 0 {
		b.WriteString("CONTRADICTIONS:\n")
		for _, c := range env.Contradictions {
			b.WriteString("  ! " + c + "\n")
		}
		b.WriteString("\n")
	}
	if len(env.CodeRefs) > 0 {
		b.WriteString("CODE:\n")
		for _, c := range env.CodeRefs {
			b.WriteString("  * " + c + "\n")
		}
		b.WriteString("\n")
	}
	if len(env.Artifacts) > 0 {
		b.WriteString("ARTIFACTS:\n")
		for _, a := range env.Artifacts {
			b.WriteString("  * " + a + "\n")
		}
		b.WriteString("\n")
	}
	if len(env.Sources) > 0 {
		b.WriteString("SOURCES:\n")
		for _, s := range env.Sources {
			b.WriteString("  * " + s + "\n")
		}
		b.WriteString("\n")
	}
	if len(env.RestorationRefs) > 0 {
		b.WriteString("RESTORATION REFERENCES:\n")
		for _, r := range env.RestorationRefs {
			b.WriteString("  * " + r + "\n")
		}
		b.WriteString("\n")
	}
	return b.String()
}

func renderJSON(env *ContextEnvelope) string {
	data, _ := json.MarshalIndent(env, "", "  ")
	return string(data)
}

// ── event translation ────────────────────────────────────────────────────

func translateEvent(eventType string, sourceData any, sourceFormat string) map[string]any {
	translated := map[string]any{
		"event_type":    eventType,
		"source_format": sourceFormat,
		"scos_version":  version,
		"translated_at": time.Now().UTC().Format(time.RFC3339),
	}

	switch sourceFormat {
	case "openai":
		if data, ok := sourceData.(map[string]any); ok {
			translated["content"] = data
			translated["role"] = data["role"]
			translated["provenance"] = "openai_compatible"
		}
	case "anthropic":
		translated["provenance"] = "anthropic"
		translated["content"] = sourceData
	case "mcp":
		translated["provenance"] = "mcp"
		translated["tool_call"] = sourceData
	default:
		translated["provenance"] = sourceFormat
		translated["content"] = sourceData
	}

	return translated
}

// ── utility ──────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("encode response: %v", err)
	}
}

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("encode response: %v", err)
	}
}
