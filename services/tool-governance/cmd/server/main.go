package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const version = "0.1.0"

// ── tool lifecycle state machine (§28) ───────────────────────────────────

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
	mu         sync.RWMutex
	tools      map[string]*ToolDefinition
	operations map[string]*ToolOperation
}

func newToolGovernanceServer() *toolGovernanceServer {
	return &toolGovernanceServer{
		tools:      make(map[string]*ToolDefinition),
		operations: make(map[string]*ToolOperation),
	}
}

func main() {
	s := newToolGovernanceServer()
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", healthOK)
	mux.HandleFunc("/health/ready", healthOK)

	mux.HandleFunc("/v1/tools", s.handleTools)
	mux.HandleFunc("/v1/tools/", s.handleToolByID)
	mux.HandleFunc("/v1/tools/register", s.handleToolRegister)
	mux.HandleFunc("/v1/tools/transition", s.handleToolTransition)
	mux.HandleFunc("/v1/operations", s.handleOperations)
	mux.HandleFunc("/v1/operations/", s.handleOperationByID)
	mux.HandleFunc("/v1/operations/start", s.handleOperationStart)
	mux.HandleFunc("/v1/operations/complete", s.handleOperationComplete)
	mux.HandleFunc("/v1/operations/reconcile", s.handleReconcile)

	addr := os.Getenv("SCOS_TOOL_GOVERNANCE_HTTP_ADDR")
	if addr == "" {
		addr = ":8092"
	}
	log.Printf("tool governance listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

func healthOK(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// ── tools ────────────────────────────────────────────────────────────────

func (s *toolGovernanceServer) handleTools(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	var tools []*ToolDefinition
	for _, t := range s.tools {
		tools = append(tools, t)
	}
	s.mu.RUnlock()
	writeJSON(w, tools)
}

func (s *toolGovernanceServer) handleToolByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/tools/")
	s.mu.RLock()
	tool, ok := s.tools[id]
	s.mu.RUnlock()
	if !ok {
		http.Error(w, "tool not found", http.StatusNotFound)
		return
	}
	writeJSON(w, tool)
}

func (s *toolGovernanceServer) handleToolRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var tool ToolDefinition
	if err := json.NewDecoder(r.Body).Decode(&tool); err != nil {
		http.Error(w, "invalid tool: "+err.Error(), http.StatusBadRequest)
		return
	}
	if tool.Name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	tool.ID = fmt.Sprintf("tool-%d", len(s.tools)+1)
	tool.State = StateProposed
	tool.CurrentState = StateProposed
	tool.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	tool.UpdatedAt = tool.CreatedAt
	s.tools[tool.ID] = &tool
	s.mu.Unlock()
	writeJSON(w, tool)
}

func (s *toolGovernanceServer) handleToolTransition(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ToolID   string    `json:"tool_id"`
		NewState ToolState `json:"new_state"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	tool, ok := s.tools[req.ToolID]
	if !ok {
		s.mu.Unlock()
		http.Error(w, "tool not found", http.StatusNotFound)
		return
	}

	if !canTransition(tool.State, req.NewState) {
		s.mu.Unlock()
		http.Error(w, fmt.Sprintf("invalid transition from %s to %s", tool.State, req.NewState), http.StatusBadRequest)
		return
	}

	tool.State = req.NewState
	tool.CurrentState = req.NewState
	tool.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	s.mu.Unlock()
	writeJSON(w, tool)
}

// ── operations ───────────────────────────────────────────────────────────

func (s *toolGovernanceServer) handleOperations(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	var ops []*ToolOperation
	for _, o := range s.operations {
		ops = append(ops, o)
	}
	s.mu.RUnlock()
	writeJSON(w, ops)
}

func (s *toolGovernanceServer) handleOperationByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/operations/")
	s.mu.RLock()
	op, ok := s.operations[id]
	s.mu.RUnlock()
	if !ok {
		http.Error(w, "operation not found", http.StatusNotFound)
		return
	}
	writeJSON(w, op)
}

func (s *toolGovernanceServer) handleOperationStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()
	var req struct {
		ToolID string `json:"tool_id"`
		Input  any    `json:"input,omitempty"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	tool, ok := s.tools[req.ToolID]
	if !ok {
		s.mu.Unlock()
		http.Error(w, "tool not found", http.StatusNotFound)
		return
	}
	if tool.State != StateAuthorised {
		s.mu.Unlock()
		http.Error(w, "tool must be authorised before execution", http.StatusConflict)
		return
	}
	op := &ToolOperation{
		ID:        fmt.Sprintf("op-%d", len(s.operations)+1),
		ToolID:    req.ToolID,
		State:     StateExecuting,
		Input:     req.Input,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}
	s.operations[op.ID] = op
	s.mu.Unlock()
	writeJSON(w, op)
}

func (s *toolGovernanceServer) handleOperationComplete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()
	var req struct {
		OperationID string `json:"operation_id"`
		Result      string `json:"result"` // succeeded, failed, unknown
		Output      any    `json:"output,omitempty"`
		Error       string `json:"error,omitempty"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	op, ok := s.operations[req.OperationID]
	if !ok {
		s.mu.Unlock()
		http.Error(w, "operation not found", http.StatusNotFound)
		return
	}
	if op.State != StateExecuting {
		s.mu.Unlock()
		http.Error(w, "operation is not executing", http.StatusConflict)
		return
	}
	if req.Result != "succeeded" && req.Result != "failed" && req.Result != "unknown" {
		s.mu.Unlock()
		http.Error(w, "result must be succeeded, failed, or unknown", http.StatusBadRequest)
		return
	}
	op.CompletedAt = time.Now().UTC().Format(time.RFC3339)
	switch req.Result {
	case "succeeded":
		op.State = StateSucceeded
	case "failed":
		op.State = StateFailed
	default:
		op.State = StateUnknown
	}
	op.Output = req.Output
	op.Error = req.Error
	s.mu.Unlock()
	writeJSON(w, op)
}

func (s *toolGovernanceServer) handleReconcile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()
	var req struct {
		OperationID string `json:"operation_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	op, ok := s.operations[req.OperationID]
	if !ok {
		s.mu.Unlock()
		http.Error(w, "operation not found", http.StatusNotFound)
		return
	}
	if op.State != StateUnknown && op.State != StateFailed {
		s.mu.Unlock()
		http.Error(w, "only failed or unknown operations can be reconciled", http.StatusConflict)
		return
	}
	op.State = StateReconciling
	s.mu.Unlock()
	writeJSON(w, op)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("encode response: %v", err)
	}
}
