package main

import "github.com/dylmarriner/scos-memory/packages/telemetry"
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

	"github.com/dylmarriner/scos-memory/packages/database"
	executionadapters "github.com/dylmarriner/scos-memory/packages/execution-adapters"
	"github.com/dylmarriner/scos-memory/services/readiness"
	_ "github.com/jackc/pgx/v5/stdlib"
)

const version = "0.1.0"

// ── tool lifecycle state machine (§28) ───────────────────────────────────

// ToolState mirrors the database's operation_status enum
// (database/migrations/000001_initial.sql) so tool and operation lifecycles
// share one vocabulary end to end.
type ToolState string

const (
	StateProposed    ToolState = "proposed"
	StatePrepared    ToolState = "prepared"
	StateAuthorised  ToolState = "authorised"
	StateExecuting   ToolState = "executing"
	StateSucceeded   ToolState = "succeeded"
	StateFailed      ToolState = "failed"
	StateCancelled   ToolState = "cancelled"
	StateUnknown     ToolState = "unknown"
	StateReconciling ToolState = "reconciling"
	StateReconciled  ToolState = "reconciled"
)

type ToolProperties struct {
	ReadOnly                bool `json:"read_only"`
	Destructive             bool `json:"destructive"`
	Idempotent              bool `json:"idempotent"`
	ExternallySideEffecting bool `json:"externally_side_effecting"`
	ConfirmationGated       bool `json:"confirmation_gated"`
	DryRunCapable           bool `json:"dry_run_capable"`
	Reconcilable            bool `json:"reconcilable"`
}

type ToolDefinition struct {
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	Description  string         `json:"description"`
	Version      string         `json:"version"`
	Props        ToolProperties `json:"properties"`
	State        ToolState      `json:"state"`
	CurrentState ToolState      `json:"current_state"`
	Owner        string         `json:"owner"`
	CreatedAt    string         `json:"created_at"`
	UpdatedAt    string         `json:"updated_at"`
}

type ToolOperation struct {
	ID          string    `json:"id"`
	ToolID      string    `json:"tool_id"`
	State       ToolState `json:"state"`
	Input       any       `json:"input,omitempty"`
	Output      any       `json:"output,omitempty"`
	Error       string    `json:"error,omitempty"`
	RetryCount  int       `json:"retry_count"`
	ParentOp    string    `json:"parent_op,omitempty"`
	StartedAt   string    `json:"started_at"`
	CompletedAt string    `json:"completed_at,omitempty"`
	// The governance decision recorded with the operation's tool_executions
	// row: the submitted plan, the risk the caller declared, the risk
	// governance decided on, whether the caller confirmed, and the tool
	// properties the decision was checked against.
	Plan               *OperationPlan  `json:"plan,omitempty"`
	RequestedRisk      string          `json:"requested_risk_level,omitempty"`
	RiskLevel          string          `json:"risk_level,omitempty"`
	Confirmed          bool            `json:"confirmed"`
	DeclaredProperties *ToolProperties `json:"declared_properties,omitempty"`
}

var validTransitions = map[ToolState][]ToolState{
	StateProposed:   {StatePrepared, StateCancelled},
	StatePrepared:   {StateAuthorised, StateCancelled},
	StateAuthorised: {StateExecuting, StateCancelled},
	// Executing -> Cancelled is a dry run: authorised and planned, then
	// deliberately not run.
	StateExecuting:   {StateSucceeded, StateFailed, StateUnknown, StateCancelled},
	StateSucceeded:   {},
	StateFailed:      {StateReconciling},
	StateCancelled:   {},
	StateUnknown:     {StateReconciling, StateSucceeded, StateFailed},
	StateReconciling: {StateReconciled, StateFailed},
	StateReconciled:  {},
}

func canTransition(from, to ToolState) bool {
	for _, candidate := range validTransitions[from] {
		if candidate == to {
			return true
		}
	}
	return false
}

// ── server ───────────────────────────────────────────────────────────────

type toolGovernanceServer struct {
	db *sql.DB
}

func (s *toolGovernanceServer) readinessCheck() error {
	if s.db == nil {
		return fmt.Errorf("postgres dependency is not configured")
	}
	return s.db.PingContext(context.Background())
}

// ── identity resolution ──────────────────────────────────────────────────

// resolveIdentity determines the tenant/principal a request acts as. Claims
// carried on an authenticated bearer token take precedence; internal
// SCOS-Tenant/Principal headers (falling back to body-supplied tenant/
// principal) are used by direct service-to-service calls (execution-proxy,
// autonomous-control-plane) and by tests that invoke handlers without the
// auth middleware in front of them.
//
// This used to also require auth.Identity(r.Context()) == "" before trying
// the header/body fallback, on the theory that a caller who is
// authenticated-but-claims-less should be rejected outright. That check was
// dead in production: auth.AuthenticatedHandler sets an identity on the
// context for *every* successfully authenticated request, claims-bearing or
// not (see packages/auth/middleware.go), and no internal caller in this
// fabric ever mints a signed claims token when talking to tool-governance --
// only cloud-control-plane calls auth.SignClaims, for external ingress.
// execution-proxy (and other internal callers) authenticate with the plain
// shared SCOS_INTERNAL_SERVICE_TOKEN and pass tenant_id/principal_id in the
// request body instead. With the old check in place every such call was
// unconditionally rejected with 401, permanently breaking
// /v1/operations/prepare and /v1/operations/reconcile in production even
// though no test caught it -- every existing test invoked resolveIdentity
// directly with a bare context that never had an identity set at all, so
// the dead branch was never exercised the way production traffic exercises
// it. See TestResolveIdentityReachableForStaticServiceToken, which drives
// the real auth.AuthenticatedHandler wrapper the way ControlPlaneHandler
// does in main(), for the regression coverage.
func resolveIdentity(r *http.Request, bodyTenant, bodyPrincipal string) (requestIdentity, int, string) {
	if claims, ok := auth.CanonicalClaims(r.Context()); ok {
		if (bodyTenant != "" && bodyTenant != claims.TenantID) || (bodyPrincipal != "" && bodyPrincipal != claims.PrincipalID) {
			return requestIdentity{}, http.StatusForbidden, "request identity does not match authenticated identity"
		}
		return requestIdentity{TenantID: claims.TenantID, PrincipalID: claims.PrincipalID}, 0, ""
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
	if err := database.RequireNoBypassRLSDB(context.Background(), db); err != nil {
		log.Fatalf("refusing to start: %v", err)
	}
	database.TuneSQLDB(db)
	defer db.Close()

	s := &toolGovernanceServer{db: db}

	mux := s.routes()

	addr := os.Getenv("SCOS_TOOL_GOVERNANCE_HTTP_ADDR")
	if addr == "" {
		addr = ":8092"
	}
	log.Printf("tool governance listening on %s", addr)
	if err := tlsserve.ListenAndServe(addr, telemetry.Instrument("tool-governance", auth.ControlPlaneHandler(mux, auth.MustFromEnvironment()))); err != nil {
		log.Fatal(err)
	}
}

// toolAdminScope is the operator scope required to change a tenant's tool
// catalog. The shared static service token carries no claims and so no
// scopes: execution-proxy and other internal callers can prepare
// operations on authorised tools but can never register or authorise one.
const toolAdminScope = "tools:admin"

func (s *toolGovernanceServer) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", healthOK)
	mux.HandleFunc("/health/version", release.Handler("tool-governance"))
	mux.HandleFunc("/health/ready", readiness.Handler(func() error {
		return s.readinessCheck()
	}))

	mux.HandleFunc("/v1/tools", s.handleTools)
	mux.HandleFunc("/v1/tools/", s.handleToolByID)
	mux.Handle("/v1/tools/register", auth.RequireScope(toolAdminScope, http.HandlerFunc(s.handleToolRegister)))
	mux.Handle("/v1/tools/transition", auth.RequireScope(toolAdminScope, http.HandlerFunc(s.handleToolTransition)))
	mux.HandleFunc("/v1/operations", s.handleOperations)
	mux.HandleFunc("/v1/operations/", s.handleOperationByID)
	mux.HandleFunc("/v1/operations/start", s.handleOperationStart)
	mux.HandleFunc("/v1/operations/prepare", s.handleOperationPrepare)
	mux.HandleFunc("/v1/operations/complete", s.handleOperationComplete)
	mux.HandleFunc("/v1/operations/reconcile", s.handleReconcile)
	return mux
}

func healthOK(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// ── tools ────────────────────────────────────────────────────────────────

func (s *toolGovernanceServer) handleTools(w http.ResponseWriter, r *http.Request) {
	identity, status, msg := resolveIdentity(r, "", "")
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	tools, err := listTools(r.Context(), s.db, identity.TenantID)
	if err != nil {
		http.Error(w, "list tools: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, tools)
}

func (s *toolGovernanceServer) handleToolByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/tools/")
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
	if err := setPrincipalContext(ctx, tx, identity); err != nil {
		http.Error(w, "set principal context: "+err.Error(), http.StatusInternalServerError)
		return
	}
	tool, err := loadTool(ctx, tx, identity.TenantID, id)
	if err == sql.ErrNoRows {
		http.Error(w, "tool not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "load tool: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, tool)
}

func (s *toolGovernanceServer) handleToolRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	idemKeyHeader, ok := requiredIdempotencyKey(w, r)
	if !ok {
		return
	}
	var req struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Version     string         `json:"version"`
		Props       ToolProperties `json:"properties"`
		TenantID    string         `json:"tenant_id,omitempty"`
		PrincipalID string         `json:"principal_id,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid tool: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	identity, status, msg := resolveIdentity(r, req.TenantID, req.PrincipalID)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	fingerprint := requestFingerprint(req)

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

	idemKey := "tool-register:" + idemKeyHeader
	var replayed ToolDefinition
	found, err := idempotentReplay(ctx, tx, identity, idemKey, fingerprint, &replayed)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if found {
		if err := tx.Commit(); err != nil {
			http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, replayed)
		return
	}

	tool := &ToolDefinition{Name: req.Name, Description: req.Description, Version: req.Version, Props: req.Props}
	if err := registerTool(ctx, tx, identity, tool); err != nil {
		http.Error(w, "register tool: "+err.Error(), http.StatusConflict)
		return
	}
	if err := recordIdempotent(ctx, tx, identity, idemKey, "tool-register", fingerprint, tool); err != nil {
		http.Error(w, "record idempotency: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, tool)
}

func (s *toolGovernanceServer) handleToolTransition(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	idemKeyHeader, ok := requiredIdempotencyKey(w, r)
	if !ok {
		return
	}
	var req struct {
		ToolID      string    `json:"tool_id"`
		NewState    ToolState `json:"new_state"`
		TenantID    string    `json:"tenant_id,omitempty"`
		PrincipalID string    `json:"principal_id,omitempty"`
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
	fingerprint := requestFingerprint(req)

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

	idemKey := "tool-transition:" + idemKeyHeader
	var replayed ToolDefinition
	found, err := idempotentReplay(ctx, tx, identity, idemKey, fingerprint, &replayed)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if found {
		if err := tx.Commit(); err != nil {
			http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, replayed)
		return
	}

	tool, err := loadTool(ctx, tx, identity.TenantID, req.ToolID)
	if err == sql.ErrNoRows {
		http.Error(w, "tool not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "load tool: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !canTransition(tool.State, req.NewState) {
		http.Error(w, fmt.Sprintf("invalid transition from %s to %s", tool.State, req.NewState), http.StatusBadRequest)
		return
	}
	if err := transitionTool(ctx, tx, identity.TenantID, req.ToolID, req.NewState); err != nil {
		http.Error(w, "transition tool: "+err.Error(), http.StatusInternalServerError)
		return
	}
	tool.State, tool.CurrentState = req.NewState, req.NewState
	if err := recordIdempotent(ctx, tx, identity, idemKey, "tool-transition", fingerprint, tool); err != nil {
		http.Error(w, "record idempotency: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, tool)
}

// ── operations ───────────────────────────────────────────────────────────

func (s *toolGovernanceServer) handleOperations(w http.ResponseWriter, r *http.Request) {
	identity, status, msg := resolveIdentity(r, "", "")
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	ops, err := listOperations(r.Context(), s.db, identity.TenantID)
	if err != nil {
		http.Error(w, "list operations: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, ops)
}

func (s *toolGovernanceServer) handleOperationByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/operations/")
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
	if err := setPrincipalContext(ctx, tx, identity); err != nil {
		http.Error(w, "set principal context: "+err.Error(), http.StatusInternalServerError)
		return
	}
	op, err := loadOperation(ctx, tx, identity.TenantID, id)
	if err == sql.ErrNoRows {
		http.Error(w, "operation not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "load operation: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, op)
}

func (s *toolGovernanceServer) handleOperationStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	idemKeyHeader, ok := requiredIdempotencyKey(w, r)
	if !ok {
		return
	}
	defer r.Body.Close()
	var req struct {
		ToolID      string        `json:"tool_id,omitempty"`
		ToolName    string        `json:"tool_name,omitempty"`
		OperationID string        `json:"operation_id,omitempty"`
		Input       any           `json:"input,omitempty"`
		Plan        OperationPlan `json:"plan"`
		Risk        string        `json:"risk,omitempty"`
		Confirmed   bool          `json:"confirmed,omitempty"`
		TenantID    string        `json:"tenant_id,omitempty"`
		PrincipalID string        `json:"principal_id,omitempty"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if (req.ToolID == "") == (req.ToolName == "") {
		http.Error(w, "exactly one of tool_id or tool_name is required", http.StatusBadRequest)
		return
	}
	if req.ToolID != "" && !isUUID(req.ToolID) {
		http.Error(w, "tool_id must be a UUID", http.StatusBadRequest)
		return
	}
	if req.OperationID != "" && !isUUID(req.OperationID) {
		http.Error(w, "operation_id must be a UUID", http.StatusBadRequest)
		return
	}
	identity, status, msg := resolveIdentity(r, req.TenantID, req.PrincipalID)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	fingerprint := requestFingerprint(req)

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

	idemKey := "operation-start:" + idemKeyHeader
	var replayed ToolOperation
	found, err := idempotentReplay(ctx, tx, identity, idemKey, fingerprint, &replayed)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if found {
		if err := tx.Commit(); err != nil {
			http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, replayed)
		return
	}

	var tool *ToolDefinition
	if req.ToolID != "" {
		tool, err = loadTool(ctx, tx, identity.TenantID, req.ToolID)
		if err == sql.ErrNoRows {
			http.Error(w, "tool not found", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, "load tool: "+err.Error(), http.StatusInternalServerError)
			return
		}
	} else {
		var ok bool
		if tool, ok = resolveOperationTool(ctx, w, tx, identity.TenantID, req.ToolName); !ok {
			return
		}
	}
	decision, refusal := decideOperation(tool, operationRequest{Risk: req.Risk, Plan: req.Plan, Confirmed: req.Confirmed, OperationID: req.OperationID})
	if refusal != nil {
		http.Error(w, refusal.Message, refusal.Status)
		return
	}
	opID, ok := s.claimOperationID(ctx, w, tx, identity.TenantID, req.OperationID)
	if !ok {
		return
	}
	op, err := startOperation(ctx, tx, identity, opID, tool.Name, tool.Version, idemKeyHeader, req.Input, tool.Props, decision, false)
	if err != nil {
		http.Error(w, "start operation: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := recordIdempotent(ctx, tx, identity, idemKey, "operation-start", fingerprint, op); err != nil {
		http.Error(w, "record idempotency: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, op)
}

// handleOperationPrepare is the boundary used by execution-proxy. It issues
// an operation only after the governance service has classified and accepted
// the tool request; callers cannot turn an arbitrary operation ID into a
// capability, and cannot bring a tool into existence by naming it. The tool
// must already be registered for the tenant (through the admin-scoped
// /v1/tools/register and /v1/tools/transition) or be one of the fabric's
// built-in tools seeded in builtin_tool_definitions; any other name is
// refused. The caller's plan and risk are decoded, checked against the
// tool's declared properties by decideOperation, and persisted with the
// operation. A prepare naming an operation /v1/operations/start already
// started hands that operation to the executor (claimForExecutor): the
// response then carries the confirmation and risk recorded on it, which is
// the only confirmation execution-proxy acts on.
func (s *toolGovernanceServer) handleOperationPrepare(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	idemKeyHeader, ok := requiredIdempotencyKey(w, r)
	if !ok {
		return
	}
	defer r.Body.Close()
	var req struct {
		OperationID string        `json:"operation_id"`
		ToolName    string        `json:"tool_name"`
		Input       any           `json:"input,omitempty"`
		Plan        OperationPlan `json:"plan"`
		Risk        string        `json:"risk,omitempty"`
		Confirmed   bool          `json:"confirmed,omitempty"`
		TenantID    string        `json:"tenant_id,omitempty"`
		PrincipalID string        `json:"principal_id,omitempty"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.ToolName == "" {
		http.Error(w, "tool_name required", http.StatusBadRequest)
		return
	}
	if req.OperationID != "" && !isUUID(req.OperationID) {
		http.Error(w, "operation_id must be a UUID", http.StatusBadRequest)
		return
	}
	identity, status, msg := resolveIdentity(r, req.TenantID, req.PrincipalID)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	fingerprint := requestFingerprint(req)

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

	idemKey := "operation-prepare:" + idemKeyHeader
	var replayed GovernedOperationResponse
	found, err := idempotentReplay(ctx, tx, identity, idemKey, fingerprint, &replayed)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if found {
		if err := tx.Commit(); err != nil {
			http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, replayed)
		return
	}

	tool, ok := resolveOperationTool(ctx, w, tx, identity.TenantID, req.ToolName)
	if !ok {
		return
	}
	var resp GovernedOperationResponse
	started, err := s.startedOperationFor(ctx, tx, identity.TenantID, req.OperationID)
	switch {
	case err != nil:
		http.Error(w, "load operation: "+err.Error(), http.StatusInternalServerError)
		return
	case started != nil:
		// The executor names an operation governance already started (the
		// control plane's governed operation): it runs under that
		// operation's recorded decision and confirmation, never one it
		// asserts now.
		var refusal *policyRefusal
		if resp, refusal = claimForExecutor(ctx, tx, identity, tool, started, req.Risk, req.Plan); refusal != nil {
			http.Error(w, refusal.Message, refusal.Status)
			return
		}
	default:
		decision, refusal := decideOperation(tool, operationRequest{Risk: req.Risk, Plan: req.Plan, Confirmed: req.Confirmed, OperationID: req.OperationID})
		if refusal != nil {
			http.Error(w, refusal.Message, refusal.Status)
			return
		}
		opID, ok := s.claimOperationID(ctx, w, tx, identity.TenantID, req.OperationID)
		if !ok {
			return
		}
		// The executor preparing its own operation holds it from the start,
		// so no later prepare can claim it again.
		if _, err := startOperation(ctx, tx, identity, opID, tool.Name, tool.Version, idemKeyHeader, req.Input, tool.Props, decision, true); err != nil {
			http.Error(w, "start operation: "+err.Error(), http.StatusInternalServerError)
			return
		}
		resp = GovernedOperationResponse{OperationID: opID, Prepared: true, Authorized: true, RiskLevel: decision.EffectiveRisk, Confirmed: decision.Confirmed}
	}
	if err := recordIdempotent(ctx, tx, identity, idemKey, "operation-prepare", fingerprint, resp); err != nil {
		http.Error(w, "record idempotency: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, resp)
}

// claimOperationID returns the operation ID to start: the caller's, when it
// names one that does not exist yet, or a freshly generated one. A caller
// ID that already names an operation is refused with 409 -- a retry of the
// same request replays through its Idempotency-Key instead, so a second
// prepare under the same ID would be a second execution.
func (s *toolGovernanceServer) claimOperationID(ctx context.Context, w http.ResponseWriter, tx *sql.Tx, tenantID, requested string) (string, bool) {
	if requested == "" {
		generated, err := newID()
		if err != nil {
			http.Error(w, "generate operation id: "+err.Error(), http.StatusInternalServerError)
			return "", false
		}
		return generated, true
	}
	exists, err := operationExists(ctx, tx, tenantID, requested)
	if err != nil {
		http.Error(w, "check operation: "+err.Error(), http.StatusInternalServerError)
		return "", false
	}
	if exists {
		http.Error(w, fmt.Sprintf("operation %s already exists", requested), http.StatusConflict)
		return "", false
	}
	return requested, true
}

// startedOperationFor returns the existing operation a prepare names, or
// nil when it names none (or one that does not exist yet), in which case
// prepare starts a new operation.
func (s *toolGovernanceServer) startedOperationFor(ctx context.Context, tx *sql.Tx, tenantID, operationID string) (*startedOperation, error) {
	if operationID == "" {
		return nil, nil
	}
	started, err := loadStartedOperation(ctx, tx, tenantID, operationID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return started, err
}

// claimForExecutor hands an operation governance already started to the
// executor that names it in a prepare. The executor must be the tool the
// operation was started for, acting for the principal that started it, and
// must plan the command the operation was started with. The executor's plan
// is decided again, with the confirmation recorded on the operation -- not
// one the executor asserts -- and at the higher of the recorded risk and
// the plan's own grade, so a destructive command runs only under an
// operation recorded as confirmed. An operation is claimed once: a second
// prepare naming it is refused, so one confirmation authorises exactly one
// execution.
func claimForExecutor(ctx context.Context, tx *sql.Tx, identity requestIdentity, tool *ToolDefinition, started *startedOperation, risk string, plan OperationPlan) (GovernedOperationResponse, *policyRefusal) {
	opID := started.ID
	switch {
	case started.ToolID != tool.Name:
		return GovernedOperationResponse{}, &policyRefusal{http.StatusConflict, fmt.Sprintf("operation %s was started for tool %q, not %q", opID, started.ToolID, tool.Name)}
	case started.Claimed:
		return GovernedOperationResponse{}, &policyRefusal{http.StatusConflict, fmt.Sprintf("operation %s already exists and has been taken by an executor", opID)}
	case started.StartedBy != identity.PrincipalID:
		return GovernedOperationResponse{}, &policyRefusal{http.StatusForbidden, fmt.Sprintf("operation %s was started by another principal", opID)}
	case started.State != StateExecuting:
		return GovernedOperationResponse{}, &policyRefusal{http.StatusConflict, fmt.Sprintf("operation %s is %s, not awaiting execution", opID, started.State)}
	}
	recorded := plannedCommand(started.Plan)
	if recorded == "" {
		return GovernedOperationResponse{}, &policyRefusal{http.StatusConflict, fmt.Sprintf("operation %s was started without a planned command", opID)}
	}
	if submitted := strings.TrimSpace(plan.OriginalRequest); submitted != recorded {
		return GovernedOperationResponse{}, &policyRefusal{http.StatusConflict, fmt.Sprintf("operation %s was started for command %q, not %q", opID, recorded, submitted)}
	}
	decision, refusal := decideOperation(tool, operationRequest{Risk: risk, Plan: plan, Confirmed: started.Confirmed, OperationID: opID})
	if refusal != nil {
		return GovernedOperationResponse{}, refusal
	}
	effective := decision.EffectiveRisk
	if riskRank[started.RiskLevel] > riskRank[effective] {
		effective = started.RiskLevel
	}
	if effective == executionadapters.RiskDestructive && !started.Confirmed {
		return GovernedOperationResponse{}, &policyRefusal{http.StatusForbidden, fmt.Sprintf("operation %s has no recorded confirmation; a destructive operation is not permitted", opID)}
	}
	claimed, err := claimStartedOperation(ctx, tx, opID, effective)
	if err != nil {
		return GovernedOperationResponse{}, &policyRefusal{http.StatusInternalServerError, "claim operation: " + err.Error()}
	}
	if !claimed {
		return GovernedOperationResponse{}, &policyRefusal{http.StatusConflict, fmt.Sprintf("operation %s already exists and has been taken by an executor", opID)}
	}
	return GovernedOperationResponse{OperationID: opID, Prepared: true, Authorized: true, RiskLevel: effective, Confirmed: started.Confirmed}, nil
}

// plannedCommand is the command an operation's recorded plan names.
func plannedCommand(plan *OperationPlan) string {
	if plan == nil {
		return ""
	}
	if original := strings.TrimSpace(plan.OriginalRequest); original != "" {
		return original
	}
	return strings.TrimSpace(plan.ExecutedRequest)
}

// GovernedOperationResponse is the capability prepare issues. RiskLevel is
// the risk governance decided on, which may be higher than the caller
// declared when the planned command grades higher.
type GovernedOperationResponse struct {
	OperationID string `json:"operation_id"`
	Prepared    bool   `json:"prepared"`
	Authorized  bool   `json:"authorized"`
	RiskLevel   string `json:"risk_level,omitempty"`
	Confirmed   bool   `json:"confirmed"`
}

func (s *toolGovernanceServer) handleOperationComplete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	idemKeyHeader, ok := requiredIdempotencyKey(w, r)
	if !ok {
		return
	}
	defer r.Body.Close()
	var req struct {
		OperationID string `json:"operation_id"`
		Result      string `json:"result"` // succeeded, failed, unknown, cancelled (dry run)
		Output      any    `json:"output,omitempty"`
		Error       string `json:"error,omitempty"`
		TenantID    string `json:"tenant_id,omitempty"`
		PrincipalID string `json:"principal_id,omitempty"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Result != "succeeded" && req.Result != "failed" && req.Result != "unknown" && req.Result != "cancelled" {
		http.Error(w, "result must be succeeded, failed, unknown or cancelled", http.StatusBadRequest)
		return
	}
	identity, status, msg := resolveIdentity(r, req.TenantID, req.PrincipalID)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	fingerprint := requestFingerprint(req)

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

	idemKey := "operation-complete:" + idemKeyHeader
	var replayed ToolOperation
	found, err := idempotentReplay(ctx, tx, identity, idemKey, fingerprint, &replayed)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if found {
		if err := tx.Commit(); err != nil {
			http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, replayed)
		return
	}

	op, err := loadOperation(ctx, tx, identity.TenantID, req.OperationID)
	if err == sql.ErrNoRows {
		http.Error(w, "operation not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "load operation: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if op.State != StateExecuting {
		http.Error(w, "operation is not executing", http.StatusConflict)
		return
	}
	var newState ToolState
	switch req.Result {
	case "succeeded":
		newState = StateSucceeded
	case "failed":
		newState = StateFailed
	case "cancelled":
		newState = StateCancelled
	default:
		newState = StateUnknown
	}
	if err := completeOperation(ctx, tx, identity.TenantID, req.OperationID, newState, req.Output, req.Error); err != nil {
		http.Error(w, "complete operation: "+err.Error(), http.StatusInternalServerError)
		return
	}
	op.State, op.Output, op.Error = newState, req.Output, req.Error
	if err := recordIdempotent(ctx, tx, identity, idemKey, "operation-complete", fingerprint, op); err != nil {
		http.Error(w, "record idempotency: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, op)
}

func (s *toolGovernanceServer) handleReconcile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	idemKeyHeader, ok := requiredIdempotencyKey(w, r)
	if !ok {
		return
	}
	defer r.Body.Close()
	var req struct {
		OperationID string `json:"operation_id"`
		Result      string `json:"result,omitempty"`
		Output      any    `json:"output,omitempty"`
		TenantID    string `json:"tenant_id,omitempty"`
		PrincipalID string `json:"principal_id,omitempty"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	identity, status, msg := resolveIdentity(r, req.TenantID, req.PrincipalID)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	fingerprint := requestFingerprint(req)

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

	idemKey := "operation-reconcile:" + idemKeyHeader
	var replayed ToolOperation
	found, err := idempotentReplay(ctx, tx, identity, idemKey, fingerprint, &replayed)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if found {
		if err := tx.Commit(); err != nil {
			http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, replayed)
		return
	}

	op, err := loadOperation(ctx, tx, identity.TenantID, req.OperationID)
	if err == sql.ErrNoRows {
		http.Error(w, "operation not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "load operation: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if op.State != StateExecuting && op.State != StateUnknown && op.State != StateFailed && op.State != StateReconciling {
		http.Error(w, "only executing, failed, unknown or reconciling operations can be reconciled", http.StatusConflict)
		return
	}
	// Settling an executing operation is its completion; resolving a failed
	// or uncertain outcome after the fact is reconciliation proper, which
	// only a tool declared reconcilable supports. The properties checked are
	// the snapshot recorded when the operation started.
	if op.State != StateExecuting && (op.DeclaredProperties == nil || !op.DeclaredProperties.Reconcilable) {
		http.Error(w, fmt.Sprintf("tool %q is not declared reconcilable; its %s operations cannot be reconciled", op.ToolID, op.State), http.StatusConflict)
		return
	}
	newState, err := reconciledState(op.State, req.Result)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if err := completeOperation(ctx, tx, identity.TenantID, req.OperationID, newState, req.Output, op.Error); err != nil {
		http.Error(w, "reconcile operation: "+err.Error(), http.StatusInternalServerError)
		return
	}
	op.State, op.Output = newState, req.Output
	if err := recordIdempotent(ctx, tx, identity, idemKey, "operation-reconcile", fingerprint, op); err != nil {
		http.Error(w, "record idempotency: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, op)
}

// reconciledState is where a reconcile with result takes an operation in
// state from, following validTransitions. An executing operation settles
// directly (succeeded, failed or unknown). A failed or unknown one enters
// reconciling while its outcome is established ("" or "unknown"), and once
// it is: an effect found applied is reconciled, one found not applied is
// failed. Reaching reconciled or failed from failed or unknown passes
// through reconciling, as the transition table requires.
func reconciledState(state ToolState, result string) (ToolState, error) {
	var target ToolState
	switch {
	case result != "" && result != "succeeded" && result != "failed" && result != "unknown":
		return "", fmt.Errorf("result must be succeeded, failed, or unknown")
	case state == StateExecuting && result == "":
		return "", fmt.Errorf("settling an executing operation needs a result: succeeded, failed, or unknown")
	case state == StateExecuting:
		target = ToolState(result)
	case result == "" || result == "unknown":
		target = StateReconciling
	case result == "succeeded":
		target = StateReconciled
	default:
		target = StateFailed
	}
	if canTransition(state, target) || (canTransition(state, StateReconciling) && canTransition(StateReconciling, target)) {
		return target, nil
	}
	return "", fmt.Errorf("an operation in state %s cannot be reconciled to %s", state, target)
}

func requiredIdempotencyKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		http.Error(w, "Idempotency-Key header required", http.StatusBadRequest)
		return "", false
	}
	return key, true
}

func requestFingerprint(v any) string {
	b, _ := json.Marshal(v)
	return sha256Hex(string(b))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("encode response: %v", err)
	}
}

// resolveOperationTool finds the named tool an operation will run: the
// tenant's registered tool, or -- for one of the fabric's own built-in tools
// -- the tenant's copy materialised from the catalog. Anything else is
// refused; nothing is registered on a caller's say-so. It writes the
// refusal itself and reports whether the caller may continue.
func resolveOperationTool(ctx context.Context, w http.ResponseWriter, tx *sql.Tx, tenantID, name string) (*ToolDefinition, bool) {
	tool, err := loadToolByName(ctx, tx, tenantID, name)
	if err == sql.ErrNoRows {
		builtin, builtinErr := loadBuiltinTool(ctx, tx, name)
		switch {
		case builtinErr == sql.ErrNoRows:
			http.Error(w, fmt.Sprintf("tool %q is not registered for this tenant; register it through /v1/tools/register and authorise it through /v1/tools/transition", name), http.StatusForbidden)
			return nil, false
		case builtinErr != nil:
			http.Error(w, "load built-in tool: "+builtinErr.Error(), http.StatusInternalServerError)
			return nil, false
		}
		tool, err = materialiseBuiltinTool(ctx, tx, tenantID, builtin)
	}
	if err != nil {
		http.Error(w, "load tool: "+err.Error(), http.StatusInternalServerError)
		return nil, false
	}
	return tool, true
}
