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

	"github.com/dylmarriner/scos-memory/packages/database"
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
}

var validTransitions = map[ToolState][]ToolState{
	StateProposed:    {StatePrepared, StateCancelled},
	StatePrepared:    {StateAuthorised, StateCancelled},
	StateAuthorised:  {StateExecuting, StateCancelled},
	StateExecuting:   {StateSucceeded, StateFailed, StateUnknown},
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
	database.TuneSQLDB(db)
	defer db.Close()

	s := &toolGovernanceServer{db: db}

	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", healthOK)
	mux.HandleFunc("/health/ready", readiness.Handler(func() error {
		return s.readinessCheck()
	}))

	mux.HandleFunc("/v1/tools", s.handleTools)
	mux.HandleFunc("/v1/tools/", s.handleToolByID)
	mux.HandleFunc("/v1/tools/register", s.handleToolRegister)
	mux.HandleFunc("/v1/tools/transition", s.handleToolTransition)
	mux.HandleFunc("/v1/operations", s.handleOperations)
	mux.HandleFunc("/v1/operations/", s.handleOperationByID)
	mux.HandleFunc("/v1/operations/start", s.handleOperationStart)
	mux.HandleFunc("/v1/operations/prepare", s.handleOperationPrepare)
	mux.HandleFunc("/v1/operations/complete", s.handleOperationComplete)
	mux.HandleFunc("/v1/operations/reconcile", s.handleReconcile)

	addr := os.Getenv("SCOS_TOOL_GOVERNANCE_HTTP_ADDR")
	if addr == "" {
		addr = ":8092"
	}
	log.Printf("tool governance listening on %s", addr)
	if err := tlsserve.ListenAndServe(addr, auth.ControlPlaneHandler(mux, auth.MustFromEnvironment())); err != nil {
		log.Fatal(err)
	}
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
		ToolID      string `json:"tool_id"`
		Input       any    `json:"input,omitempty"`
		TenantID    string `json:"tenant_id,omitempty"`
		PrincipalID string `json:"principal_id,omitempty"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
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

	tool, err := loadTool(ctx, tx, identity.TenantID, req.ToolID)
	if err == sql.ErrNoRows {
		http.Error(w, "tool not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "load tool: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if tool.State != StateAuthorised {
		http.Error(w, "tool must be authorised before execution", http.StatusConflict)
		return
	}
	opID, err := newID()
	if err != nil {
		http.Error(w, "generate operation id: "+err.Error(), http.StatusInternalServerError)
		return
	}
	op, err := startOperation(ctx, tx, identity, opID, tool.Name, tool.Version, idemKeyHeader, req.Input, tool.Props)
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
// capability. Unlike operation/start it auto-registers and auto-authorises
// an unknown tool name, since execution-proxy calls this before any explicit
// registration step exists for ad hoc tool invocations.
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
		OperationID string `json:"operation_id"`
		ToolName    string `json:"tool_name"`
		Input       any    `json:"input,omitempty"`
		TenantID    string `json:"tenant_id,omitempty"`
		PrincipalID string `json:"principal_id,omitempty"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.ToolName == "" {
		http.Error(w, "tool_name required", http.StatusBadRequest)
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

	tool, err := loadToolByName(ctx, tx, identity.TenantID, req.ToolName)
	switch {
	case err == sql.ErrNoRows:
		tool = &ToolDefinition{Name: req.ToolName, Version: version, Props: ToolProperties{ExternallySideEffecting: true, Reconcilable: true}}
		if err := registerTool(ctx, tx, identity, tool); err != nil {
			http.Error(w, "register tool: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if err := transitionTool(ctx, tx, identity.TenantID, tool.ID, StateAuthorised); err != nil {
			http.Error(w, "authorise tool: "+err.Error(), http.StatusInternalServerError)
			return
		}
		tool.State, tool.CurrentState = StateAuthorised, StateAuthorised
	case err != nil:
		http.Error(w, "load tool: "+err.Error(), http.StatusInternalServerError)
		return
	case tool.State != StateAuthorised:
		http.Error(w, "tool is not authorised", http.StatusForbidden)
		return
	}

	opID := req.OperationID
	if opID == "" {
		generated, err := newID()
		if err != nil {
			http.Error(w, "generate operation id: "+err.Error(), http.StatusInternalServerError)
			return
		}
		opID = generated
	}
	if _, err := startOperation(ctx, tx, identity, opID, tool.Name, tool.Version, idemKeyHeader, req.Input, tool.Props); err != nil {
		http.Error(w, "start operation: "+err.Error(), http.StatusInternalServerError)
		return
	}
	resp := GovernedOperationResponse{OperationID: opID, Prepared: true, Authorized: true}
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

type GovernedOperationResponse struct {
	OperationID string `json:"operation_id"`
	Prepared    bool   `json:"prepared"`
	Authorized  bool   `json:"authorized"`
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
		Result      string `json:"result"` // succeeded, failed, unknown
		Output      any    `json:"output,omitempty"`
		Error       string `json:"error,omitempty"`
		TenantID    string `json:"tenant_id,omitempty"`
		PrincipalID string `json:"principal_id,omitempty"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Result != "succeeded" && req.Result != "failed" && req.Result != "unknown" {
		http.Error(w, "result must be succeeded, failed, or unknown", http.StatusBadRequest)
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
	if op.State != StateExecuting && op.State != StateUnknown && op.State != StateFailed {
		http.Error(w, "only failed or unknown operations can be reconciled", http.StatusConflict)
		return
	}
	var newState ToolState
	switch req.Result {
	case "":
		newState = StateReconciling
	case "succeeded":
		newState = StateSucceeded
	case "failed":
		newState = StateFailed
	case "unknown":
		newState = StateReconciling
	default:
		http.Error(w, "result must be succeeded, failed, or unknown", http.StatusBadRequest)
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
