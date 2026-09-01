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

// ── agent registration (§29) ─────────────────────────────────────────────

type AgentRegistration struct {
	ID                string   `json:"id,omitempty"`
	TenantID          string   `json:"tenant_id,omitempty"`
	PrincipalID       string   `json:"principal_id,omitempty"`
	Name              string   `json:"name"`
	Version           string   `json:"version"`
	Maintainer        string   `json:"maintainer"`
	Capabilities      []string `json:"capabilities"`
	Inputs            []string `json:"inputs"`
	Outputs           []string `json:"outputs"`
	ToolPermissions   []string `json:"tool_permissions,omitempty"`
	MemoryPermissions []string `json:"memory_permissions,omitempty"`
	MaxClassification string   `json:"max_classification"`
	ModelProvider     string   `json:"model_provider"`
	CostPolicy        string   `json:"cost_policy"`
	HealthEndpoint    string   `json:"health_endpoint"`
	Status            string   `json:"status"` // active, inactive, degraded
	RegisteredAt      string   `json:"registered_at"`
	LastSeen          string   `json:"last_seen"`
}

// ── handoff packet ───────────────────────────────────────────────────────

type HandoffPacket struct {
	ID            string   `json:"id,omitempty"`
	TenantID      string   `json:"tenant_id,omitempty"`
	PrincipalID   string   `json:"principal_id,omitempty"`
	FromAgent     string   `json:"from_agent"`
	ToAgent       string   `json:"to_agent"`
	Objective     string   `json:"objective"`
	Constraints   []string `json:"constraints,omitempty"`
	Namespaces    []string `json:"authorised_namespaces,omitempty"`
	MemoryRefs    []string `json:"memory_references,omitempty"`
	ArtifactRefs  []string `json:"artifact_references,omitempty"`
	CompletedWork []string `json:"completed_work,omitempty"`
	OpenWork      []string `json:"open_work,omitempty"`
	KnownFailures []string `json:"known_failures,omitempty"`
	Acceptance    string   `json:"acceptance_criteria,omitempty"`
	ExpiresAt     string   `json:"expires_at,omitempty"`
	Status        string   `json:"status"` // pending, accepted, rejected, completed, expired
	CreatedAt     string   `json:"created_at"`
}

// ── server ───────────────────────────────────────────────────────────────

type agentRegistryServer struct {
	db *sql.DB
}

func newAgentRegistryServer() *agentRegistryServer {
	return &agentRegistryServer{}
}

func (s *agentRegistryServer) readinessCheck() error {
	if os.Getenv("DATABASE_URL") == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}
	if s.db == nil {
		return fmt.Errorf("postgres dependency is not configured")
	}
	return s.db.PingContext(context.Background())
}

// ── identity resolution ──────────────────────────────────────────────────
//
// resolveIdentity determines the tenant/principal a request acts as. Claims
// carried on an authenticated bearer token take precedence; internal
// SCOS-Tenant/Principal headers are the fallback used by direct
// service-to-service calls and by tests that invoke handlers without the
// auth middleware in front of them. A caller-supplied tenant/principal on
// the JSON body must agree with whichever identity was authenticated.
// This mirrors the convention in services/hermes-provider/cmd/server/main.go.

type requestIdentity struct {
	TenantID    string
	PrincipalID string
}

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

func setTenantContext(ctx context.Context, tx *sql.Tx, tenantID string) error {
	_, err := tx.ExecContext(ctx, `SELECT set_config('scos.tenant_id',$1,true)`, tenantID)
	return err
}

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

	s := &agentRegistryServer{db: db}

	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", healthOK)
	mux.HandleFunc("/health/version", release.Handler("agent-registry"))
	mux.HandleFunc("/health/ready", readiness.Handler(s.readinessCheck))

	// Agent registration
	mux.HandleFunc("/v1/agents", s.handleAgents)
	mux.HandleFunc("/v1/agents/", s.handleAgentByID)
	mux.HandleFunc("/v1/agents/register", s.handleAgentRegister)
	mux.HandleFunc("/v1/agents/heartbeat", s.handleAgentHeartbeat)

	// Handoffs
	mux.HandleFunc("/v1/handoffs", s.handleHandoffs)
	mux.HandleFunc("/v1/handoffs/", s.handleHandoffByID)
	mux.HandleFunc("/v1/handoffs/initiate", s.handleHandoffInitiate)
	mux.HandleFunc("/v1/handoffs/respond", s.handleHandoffRespond)

	addr := os.Getenv("SCOS_AGENT_REGISTRY_HTTP_ADDR")
	if addr == "" {
		addr = ":8093"
	}
	log.Printf("agent registry listening on %s", addr)
	if err := tlsserve.ListenAndServe(addr, auth.AuthenticatedHandler(mux, auth.MustFromEnvironment())); err != nil {
		log.Fatal(err)
	}
}

func healthOK(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// ── agent handlers ───────────────────────────────────────────────────────

func (s *agentRegistryServer) handleAgents(w http.ResponseWriter, r *http.Request) {
	identity, status, msg := resolveIdentity(r, "", "")
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
	if err := setTenantContext(ctx, tx, identity.TenantID); err != nil {
		http.Error(w, "set tenant context: "+err.Error(), http.StatusInternalServerError)
		return
	}
	agents, err := queryAgents(ctx, tx, "tenant_id=$1", identity.TenantID)
	if err != nil {
		http.Error(w, "query agents: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, agents)
}

func (s *agentRegistryServer) handleAgentByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/agents/")
	if !isUUID(id) {
		http.Error(w, "agent id must be a UUID", http.StatusBadRequest)
		return
	}
	identity, status, msg := resolveIdentity(r, "", "")
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
	if err := setTenantContext(ctx, tx, identity.TenantID); err != nil {
		http.Error(w, "set tenant context: "+err.Error(), http.StatusInternalServerError)
		return
	}
	agents, err := queryAgents(ctx, tx, "tenant_id=$1 AND id=$2", identity.TenantID, id)
	if err != nil {
		http.Error(w, "query agent: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if len(agents) == 0 {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	writeJSON(w, agents[0])
}

func (s *agentRegistryServer) handleAgentRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var reg AgentRegistration
	if err := json.NewDecoder(r.Body).Decode(&reg); err != nil {
		http.Error(w, "invalid registration: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := validateAgentRegistration(reg); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	identity, status, msg := resolveIdentity(r, reg.TenantID, reg.PrincipalID)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	if reg.MaxClassification == "" {
		reg.MaxClassification = "confidential"
	}

	ctx := r.Context()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		http.Error(w, "begin transaction: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	if err := setTenantContext(ctx, tx, identity.TenantID); err != nil {
		http.Error(w, "set tenant context: "+err.Error(), http.StatusInternalServerError)
		return
	}

	var id, registeredAt string
	err = tx.QueryRowContext(ctx, `
		INSERT INTO agent_registrations
			(tenant_id, name, version, maintainer, capabilities, inputs, outputs,
			 tool_permissions, memory_permissions, max_classification, model_provider,
			 cost_policy, health_endpoint, status)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,'active')
		RETURNING id, registered_at
	`, identity.TenantID, reg.Name, reg.Version, reg.Maintainer,
		jsonList(reg.Capabilities), jsonList(reg.Inputs), jsonList(reg.Outputs),
		jsonList(reg.ToolPermissions), jsonList(reg.MemoryPermissions), reg.MaxClassification,
		reg.ModelProvider, reg.CostPolicy, reg.HealthEndpoint).Scan(&id, &registeredAt)
	if err != nil {
		http.Error(w, "persist agent registration: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
		return
	}

	reg.ID = id
	reg.TenantID = identity.TenantID
	reg.Status = "active"
	reg.RegisteredAt = registeredAt
	reg.LastSeen = registeredAt
	writeJSON(w, reg)
}

// validateAgentRegistration enforces GOVERNANCE.md §29: every agent SHALL
// register identity, version, maintainer, capabilities, inputs, outputs,
// model provider, cost policy and health endpoint. Tool/memory permissions
// and max classification may legitimately be absent or defaulted (an agent
// with no tool access is still a valid agent), so they are not required
// here, but capabilities/inputs/outputs describe what the agent *is* and
// are as mandatory as name/version/maintainer.
func validateAgentRegistration(reg AgentRegistration) error {
	var missing []string
	if reg.Name == "" {
		missing = append(missing, "name")
	}
	if reg.Version == "" {
		missing = append(missing, "version")
	}
	if reg.Maintainer == "" {
		missing = append(missing, "maintainer")
	}
	if len(reg.Capabilities) == 0 {
		missing = append(missing, "capabilities")
	}
	if len(reg.Inputs) == 0 {
		missing = append(missing, "inputs")
	}
	if len(reg.Outputs) == 0 {
		missing = append(missing, "outputs")
	}
	if reg.ModelProvider == "" {
		missing = append(missing, "model_provider")
	}
	if reg.CostPolicy == "" {
		missing = append(missing, "cost_policy")
	}
	if reg.HealthEndpoint == "" {
		missing = append(missing, "health_endpoint")
	}
	if len(missing) > 0 {
		return fmt.Errorf("agent registration missing required field(s): %s", strings.Join(missing, ", "))
	}
	return nil
}

func (s *agentRegistryServer) handleAgentHeartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		AgentID     string `json:"agent_id"`
		TenantID    string `json:"tenant_id,omitempty"`
		PrincipalID string `json:"principal_id,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !isUUID(req.AgentID) {
		http.Error(w, "agent_id must be a UUID", http.StatusBadRequest)
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
	if err := setTenantContext(ctx, tx, identity.TenantID); err != nil {
		http.Error(w, "set tenant context: "+err.Error(), http.StatusInternalServerError)
		return
	}
	var lastSeen string
	err = tx.QueryRowContext(ctx, `
		UPDATE agent_registrations SET status='active', last_seen_at=now()
		WHERE id=$1 AND tenant_id=$2
		RETURNING last_seen_at
	`, req.AgentID, identity.TenantID).Scan(&lastSeen)
	if err == sql.ErrNoRows {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "persist heartbeat: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"status": "acknowledged", "last_seen": lastSeen})
}

// queryAgents runs a SELECT over agent_registrations with the given WHERE
// clause (always tenant-scoped by the caller) and decodes each row back into
// the public AgentRegistration shape.
// agentStalenessWindow bounds how long a registered agent may go without a
// heartbeat before it is still truthfully reportable as "active". Without
// this, a row's persisted status='active' (set once at registration or the
// last heartbeat) would be echoed back forever even if the agent crashed
// years ago -- an unconditional, unverified claim of health, which is the
// same ss42 fabrication class already fixed for compatibility-gateway's
// adapter HealthStatus.
const agentStalenessWindow = 24 * time.Hour

// agentEffectiveStatus derives the status actually reported to callers from
// the persisted status plus real elapsed time since last_seen_at, rather
// than trusting the stored value indefinitely. Non-"active" statuses (e.g.
// an operator explicitly marking an agent "degraded") are passed through
// unchanged. An unparseable last_seen_at fails open to the stored value
// rather than fabricating a claim in either direction.
func agentEffectiveStatus(dbStatus, lastSeen string, now time.Time) string {
	if dbStatus != "active" {
		return dbStatus
	}
	seen, err := time.Parse(time.RFC3339, lastSeen)
	if err != nil {
		seen, err = time.Parse(time.RFC3339Nano, lastSeen)
	}
	if err != nil {
		return dbStatus
	}
	if now.Sub(seen) > agentStalenessWindow {
		return "inactive"
	}
	return dbStatus
}

func queryAgents(ctx context.Context, tx *sql.Tx, where string, args ...any) ([]*AgentRegistration, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, tenant_id, name, version, maintainer, capabilities, inputs, outputs,
		       tool_permissions, memory_permissions, max_classification, model_provider,
		       cost_policy, health_endpoint, status, registered_at, last_seen_at
		FROM agent_registrations WHERE `+where+` ORDER BY registered_at`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var agents []*AgentRegistration
	for rows.Next() {
		var a AgentRegistration
		var capabilities, inputs, outputs, toolPerms, memPerms []byte
		if err := rows.Scan(&a.ID, &a.TenantID, &a.Name, &a.Version, &a.Maintainer,
			&capabilities, &inputs, &outputs, &toolPerms, &memPerms, &a.MaxClassification,
			&a.ModelProvider, &a.CostPolicy, &a.HealthEndpoint, &a.Status, &a.RegisteredAt, &a.LastSeen); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(capabilities, &a.Capabilities)
		_ = json.Unmarshal(inputs, &a.Inputs)
		_ = json.Unmarshal(outputs, &a.Outputs)
		_ = json.Unmarshal(toolPerms, &a.ToolPermissions)
		_ = json.Unmarshal(memPerms, &a.MemoryPermissions)
		a.Status = agentEffectiveStatus(a.Status, a.LastSeen, time.Now())
		agents = append(agents, &a)
	}
	return agents, rows.Err()
}

// ── handoff handlers ─────────────────────────────────────────────────────

func (s *agentRegistryServer) handleHandoffs(w http.ResponseWriter, r *http.Request) {
	identity, status, msg := resolveIdentity(r, "", "")
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
	if err := setTenantContext(ctx, tx, identity.TenantID); err != nil {
		http.Error(w, "set tenant context: "+err.Error(), http.StatusInternalServerError)
		return
	}
	packets, err := queryHandoffs(ctx, tx, "tenant_id=$1", identity.TenantID)
	if err != nil {
		http.Error(w, "query handoffs: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, packets)
}

func (s *agentRegistryServer) handleHandoffByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/handoffs/")
	if !isUUID(id) {
		http.Error(w, "handoff id must be a UUID", http.StatusBadRequest)
		return
	}
	identity, status, msg := resolveIdentity(r, "", "")
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
	if err := setTenantContext(ctx, tx, identity.TenantID); err != nil {
		http.Error(w, "set tenant context: "+err.Error(), http.StatusInternalServerError)
		return
	}
	packets, err := queryHandoffs(ctx, tx, "tenant_id=$1 AND id=$2", identity.TenantID, id)
	if err != nil {
		http.Error(w, "query handoff: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if len(packets) == 0 {
		http.Error(w, "handoff not found", http.StatusNotFound)
		return
	}
	writeJSON(w, packets[0])
}

func (s *agentRegistryServer) handleHandoffInitiate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var p HandoffPacket
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		http.Error(w, "invalid handoff: "+err.Error(), http.StatusBadRequest)
		return
	}
	if p.Objective == "" {
		http.Error(w, "objective required", http.StatusBadRequest)
		return
	}
	identity, status, msg := resolveIdentity(r, p.TenantID, p.PrincipalID)
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
	if err := setTenantContext(ctx, tx, identity.TenantID); err != nil {
		http.Error(w, "set tenant context: "+err.Error(), http.StatusInternalServerError)
		return
	}

	var id, createdAt string
	err = tx.QueryRowContext(ctx, `
		INSERT INTO agent_handoffs
			(tenant_id, from_agent, to_agent, objective, constraints, authorised_namespaces,
			 memory_references, artifact_references, completed_work, open_work, known_failures,
			 acceptance_criteria, expires_at, status)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,NULLIF($13,'')::timestamptz,'pending')
		RETURNING id, created_at
	`, identity.TenantID, p.FromAgent, p.ToAgent, p.Objective,
		jsonList(p.Constraints), jsonList(p.Namespaces), jsonList(p.MemoryRefs), jsonList(p.ArtifactRefs),
		jsonList(p.CompletedWork), jsonList(p.OpenWork), jsonList(p.KnownFailures),
		nullableText(p.Acceptance), p.ExpiresAt).Scan(&id, &createdAt)
	if err != nil {
		http.Error(w, "persist handoff: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
		return
	}

	p.ID = id
	p.TenantID = identity.TenantID
	p.Status = "pending"
	p.CreatedAt = createdAt
	writeJSON(w, p)
}

func (s *agentRegistryServer) handleHandoffRespond(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		HandoffID   string `json:"handoff_id"`
		Response    string `json:"response"` // accepted, rejected
		TenantID    string `json:"tenant_id,omitempty"`
		PrincipalID string `json:"principal_id,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !isUUID(req.HandoffID) {
		http.Error(w, "handoff_id must be a UUID", http.StatusBadRequest)
		return
	}
	newStatus := "rejected"
	if req.Response == "accepted" {
		newStatus = "accepted"
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
	if err := setTenantContext(ctx, tx, identity.TenantID); err != nil {
		http.Error(w, "set tenant context: "+err.Error(), http.StatusInternalServerError)
		return
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE agent_handoffs SET status=$1, updated_at=now()
		WHERE id=$2 AND tenant_id=$3
	`, newStatus, req.HandoffID, identity.TenantID)
	if err != nil {
		http.Error(w, "persist handoff response: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		http.Error(w, "handoff not found", http.StatusNotFound)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"id": req.HandoffID, "status": newStatus})
}

// queryHandoffs runs a SELECT over agent_handoffs with the given WHERE
// clause (always tenant-scoped by the caller) and decodes each row back into
// the public HandoffPacket shape.
func queryHandoffs(ctx context.Context, tx *sql.Tx, where string, args ...any) ([]*HandoffPacket, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, tenant_id, from_agent, to_agent, objective, constraints, authorised_namespaces,
		       memory_references, artifact_references, completed_work, open_work, known_failures,
		       COALESCE(acceptance_criteria,''), COALESCE(expires_at::text,''), status, created_at
		FROM agent_handoffs WHERE `+where+` ORDER BY created_at`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var packets []*HandoffPacket
	for rows.Next() {
		var p HandoffPacket
		var constraints, namespaces, memRefs, artRefs, completed, open, failures []byte
		if err := rows.Scan(&p.ID, &p.TenantID, &p.FromAgent, &p.ToAgent, &p.Objective,
			&constraints, &namespaces, &memRefs, &artRefs, &completed, &open, &failures,
			&p.Acceptance, &p.ExpiresAt, &p.Status, &p.CreatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(constraints, &p.Constraints)
		_ = json.Unmarshal(namespaces, &p.Namespaces)
		_ = json.Unmarshal(memRefs, &p.MemoryRefs)
		_ = json.Unmarshal(artRefs, &p.ArtifactRefs)
		_ = json.Unmarshal(completed, &p.CompletedWork)
		_ = json.Unmarshal(open, &p.OpenWork)
		_ = json.Unmarshal(failures, &p.KnownFailures)
		packets = append(packets, &p)
	}
	return packets, rows.Err()
}

func jsonList(v []string) []byte {
	if v == nil {
		v = []string{}
	}
	b, _ := json.Marshal(v)
	return b
}

func nullableText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("encode response: %v", err)
	}
}
