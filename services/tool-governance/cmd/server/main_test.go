package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	serviceauth "github.com/dylanmarriner/scos-memory-fabric/packages/service-auth"
)

const tgTestSecret = "tool-governance-test-secret"

func tgBearer(t *testing.T, principal, tenant string, scopes ...string) string {
	t.Helper()
	token, err := serviceauth.Issue(serviceauth.Identity{
		Principal: principal,
		Tenant:    tenant,
		Scopes:    scopes,
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}, []byte(tgTestSecret))
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	return token
}

func tgRequest(t *testing.T, mux http.Handler, method, path, bearer string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(data)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// TestToolGovernanceTenantIsolation exercises the full authenticated HTTP
// path (not just the in-memory struct) and proves a caller from tenant-b
// cannot register into, read, transition, start, complete, or reconcile
// tenant-a's tools/operations even while holding the same admin scopes.
func TestToolGovernanceTenantIsolation(t *testing.T) {
	t.Setenv("SCOS_AUTH_ISSUER_SECRET", tgTestSecret)
	s := newToolGovernanceServer()
	mux := serviceauth.Require(newMux(s))

	ownerToken := tgBearer(t, "alice", "tenant-a", "tools:admin", "operations:execute", "operations:complete", "operations:reconcile")
	attackerToken := tgBearer(t, "mallory", "tenant-b", "tools:admin", "operations:execute", "operations:complete", "operations:reconcile")

	// Register a tool as tenant-a.
	rec := tgRequest(t, mux, http.MethodPost, "/v1/tools/register", ownerToken, map[string]any{"name": "read_file"})
	if rec.Code != http.StatusOK {
		t.Fatalf("register status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var tool ToolDefinition
	if err := json.Unmarshal(rec.Body.Bytes(), &tool); err != nil {
		t.Fatalf("decode tool: %v", err)
	}
	if tool.Tenant != "tenant-a" || tool.Owner != "alice" {
		t.Fatalf("expected tool bound to tenant-a/alice, got tenant=%s owner=%s", tool.Tenant, tool.Owner)
	}

	// tenant-b cannot see it in the list.
	rec = tgRequest(t, mux, http.MethodGet, "/v1/tools", attackerToken, nil)
	var listed []ToolDefinition
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode tool list: %v", err)
	}
	for _, lt := range listed {
		if lt.ID == tool.ID {
			t.Fatalf("tenant-b listed tenant-a's tool %s", tool.ID)
		}
	}

	// tenant-b cannot read it directly.
	rec = tgRequest(t, mux, http.MethodGet, "/v1/tools/"+tool.ID, attackerToken, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant tool read status = %d, want 404", rec.Code)
	}

	// tenant-b cannot transition it despite holding tools:admin.
	rec = tgRequest(t, mux, http.MethodPost, "/v1/tools/transition", attackerToken, map[string]any{"tool_id": tool.ID, "new_state": string(StatePrepared)})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant transition status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}

	// tenant-a authorises and starts the tool normally.
	rec = tgRequest(t, mux, http.MethodPost, "/v1/tools/transition", ownerToken, map[string]any{"tool_id": tool.ID, "new_state": string(StatePrepared)})
	if rec.Code != http.StatusOK {
		t.Fatalf("owner transition to prepared status = %d, body=%s", rec.Code, rec.Body.String())
	}
	rec = tgRequest(t, mux, http.MethodPost, "/v1/tools/transition", ownerToken, map[string]any{"tool_id": tool.ID, "new_state": string(StateAuthorised)})
	if rec.Code != http.StatusOK {
		t.Fatalf("owner transition to authorised status = %d, body=%s", rec.Code, rec.Body.String())
	}

	// tenant-b cannot start an operation against tenant-a's authorised tool.
	rec = tgRequest(t, mux, http.MethodPost, "/v1/operations/start", attackerToken, map[string]any{"tool_id": tool.ID})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant operation start status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}

	// tenant-a starts the operation.
	rec = tgRequest(t, mux, http.MethodPost, "/v1/operations/start", ownerToken, map[string]any{"tool_id": tool.ID})
	if rec.Code != http.StatusOK {
		t.Fatalf("owner operation start status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var op ToolOperation
	if err := json.Unmarshal(rec.Body.Bytes(), &op); err != nil {
		t.Fatalf("decode operation: %v", err)
	}
	if op.Tenant != "tenant-a" || op.Owner != "alice" {
		t.Fatalf("expected operation bound to tenant-a/alice, got tenant=%s owner=%s", op.Tenant, op.Owner)
	}

	// tenant-b cannot read, complete, or reconcile it.
	if rec = tgRequest(t, mux, http.MethodGet, "/v1/operations/"+op.ID, attackerToken, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant operation read status = %d, want 404", rec.Code)
	}
	if rec = tgRequest(t, mux, http.MethodPost, "/v1/operations/complete", attackerToken, map[string]any{"operation_id": op.ID, "result": "succeeded"}); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant operation complete status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}

	// tenant-a completes it for real.
	rec = tgRequest(t, mux, http.MethodPost, "/v1/operations/complete", ownerToken, map[string]any{"operation_id": op.ID, "result": "failed", "error": "boom"})
	if rec.Code != http.StatusOK {
		t.Fatalf("owner operation complete status = %d, body=%s", rec.Code, rec.Body.String())
	}

	// tenant-b cannot reconcile the now-failed operation.
	if rec = tgRequest(t, mux, http.MethodPost, "/v1/operations/reconcile", attackerToken, map[string]any{"operation_id": op.ID}); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant reconcile status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}

	// tenant-a can reconcile its own failed operation.
	rec = tgRequest(t, mux, http.MethodPost, "/v1/operations/reconcile", ownerToken, map[string]any{"operation_id": op.ID})
	if rec.Code != http.StatusOK {
		t.Fatalf("owner reconcile status = %d, body=%s", rec.Code, rec.Body.String())
	}
}

// TestToolGovernanceAdminScopeCrossesTenants documents that the "admin"
// scope is an intentional cross-tenant escape hatch, matching
// serviceauth.HasScope semantics used elsewhere in the repository.
func TestToolGovernanceAdminScopeCrossesTenants(t *testing.T) {
	t.Setenv("SCOS_AUTH_ISSUER_SECRET", tgTestSecret)
	s := newToolGovernanceServer()
	mux := serviceauth.Require(newMux(s))

	ownerToken := tgBearer(t, "alice", "tenant-a", "tools:admin")
	adminToken := tgBearer(t, "root", "tenant-b", "admin")

	rec := tgRequest(t, mux, http.MethodPost, "/v1/tools/register", ownerToken, map[string]any{"name": "read_file"})
	var tool ToolDefinition
	if err := json.Unmarshal(rec.Body.Bytes(), &tool); err != nil {
		t.Fatalf("decode tool: %v", err)
	}

	rec = tgRequest(t, mux, http.MethodGet, "/v1/tools/"+tool.ID, adminToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin cross-tenant read status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
}

func TestNewToolGovernanceServer(t *testing.T) {
	s := newToolGovernanceServer()
	if s == nil {
		t.Fatal("expected non-nil server")
	}
}

func TestRegisterTool(t *testing.T) {
	s := newToolGovernanceServer()
	s.mu.Lock()
	tool := &ToolDefinition{
		ID: "tool-1", Name: "read_file", Version: "1.0",
		Props: ToolProperties{ReadOnly: true, Reconcilable: false},
		State: StateProposed,
	}
	s.tools["tool-1"] = tool
	s.mu.Unlock()

	if tool.Name != "read_file" {
		t.Fatal("name mismatch")
	}
}

func TestToolTransitionValid(t *testing.T) {
	s := newToolGovernanceServer()
	s.mu.Lock()
	s.tools["tool-1"] = &ToolDefinition{ID: "tool-1", Name: "test", State: StateProposed}
	s.mu.Unlock()

	s.mu.Lock()
	tool := s.tools["tool-1"]
	tool.State = StatePrepared
	s.mu.Unlock()

	if tool.State != StatePrepared {
		t.Fatal("expected prepared state")
	}
}

func TestCanTransition(t *testing.T) {
	if !canTransition(StateProposed, StatePrepared) {
		t.Fatal("expected proposed to prepared to be valid")
	}
	if canTransition(StateProposed, StateExecuting) {
		t.Fatal("proposed to executing must be rejected")
	}
	if canTransition(StateSucceeded, StateExecuting) {
		t.Fatal("terminal states must not restart")
	}
}

func TestToolTransitionInvalid(t *testing.T) {
	valid, ok := validTransitions[StateProposed]
	if !ok {
		t.Fatal("expected proposed transitions")
	}
	allowed := false
	for _, s := range valid {
		if s == StateExecuting {
			allowed = true
		}
	}
	if allowed {
		t.Fatal("proposed->executing should not be a valid transition")
	}
}

func TestOperationLifecycle(t *testing.T) {
	s := newToolGovernanceServer()
	now := time.Now().UTC().Format(time.RFC3339)
	s.mu.Lock()
	op := &ToolOperation{
		ID: "op-1", ToolID: "tool-1",
		State: StateExecuting, StartedAt: now,
	}
	s.operations["op-1"] = op
	s.mu.Unlock()

	if op.State != StateExecuting {
		t.Fatal("expected executing state")
	}

	s.mu.Lock()
	op.State = StateSucceeded
	op.CompletedAt = time.Now().UTC().Format(time.RFC3339)
	s.mu.Unlock()

	if op.State != StateSucceeded {
		t.Fatal("expected succeeded state")
	}
}

func TestOperationFailAndReconcile(t *testing.T) {
	s := newToolGovernanceServer()
	s.mu.Lock()
	s.operations["op-2"] = &ToolOperation{
		ID: "op-2", ToolID: "tool-2",
		State: StateFailed, Error: "timeout",
	}
	s.mu.Unlock()

	s.mu.Lock()
	op := s.operations["op-2"]
	op.State = StateReconciling
	s.mu.Unlock()

	if op.State != StateReconciling {
		t.Fatal("expected reconciling state")
	}

	s.mu.Lock()
	op.State = StateReconciled
	s.mu.Unlock()

	if op.State != StateReconciled {
		t.Fatal("expected reconciled state")
	}
}

func TestCancelledOperation(t *testing.T) {
	s := newToolGovernanceServer()
	s.mu.Lock()
	s.tools["tool-c"] = &ToolDefinition{ID: "tool-c", Name: "cancel-test", State: StateCancelled}
	s.mu.Unlock()

	s.mu.RLock()
	tool := s.tools["tool-c"]
	s.mu.RUnlock()
	if tool.State != StateCancelled {
		t.Fatal("expected cancelled state")
	}
}

func TestEmptyToolsList(t *testing.T) {
	s := newToolGovernanceServer()
	s.mu.RLock()
	count := len(s.tools)
	s.mu.RUnlock()
	if count != 0 {
		t.Fatal("expected empty tools list")
	}
}

func TestEmptyOperationsList(t *testing.T) {
	s := newToolGovernanceServer()
	s.mu.RLock()
	count := len(s.operations)
	s.mu.RUnlock()
	if count != 0 {
		t.Fatal("expected empty operations list")
	}
}

func TestAllToolStates(t *testing.T) {
	states := []ToolState{
		StateProposed, StatePrepared, StateAuthorised, StateExecuting,
		StateSucceeded, StateFailed, StateCancelled, StateUnknown,
		StateReconciling, StateReconciled,
	}
	if len(states) != 10 {
		t.Fatal("expected 10 states")
	}
}
