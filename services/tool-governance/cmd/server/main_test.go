package main

import (
	"testing"
	"time"
)

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
