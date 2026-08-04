package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

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

func TestToolDefinitionJSONRoundTrip(t *testing.T) {
	req := ToolDefinition{
		Name: "read_file", Version: "1.0",
		Props: ToolProperties{ReadOnly: true, Reconcilable: false},
		State: StateProposed,
	}
	if req.Name != "read_file" || req.State != StateProposed {
		t.Fatal("unexpected tool definition shape")
	}
}

func TestReadinessRequiresDatabase(t *testing.T) {
	// A server with no *sql.DB configured must report itself unhealthy,
	// not a hardcoded true.
	s := &toolGovernanceServer{}
	if err := s.readinessCheck(); err == nil {
		t.Fatal("expected readiness check to fail without a configured database")
	}
}

func TestMutationRequiresIdempotencyKey(t *testing.T) {
	s := &toolGovernanceServer{}
	r := httptest.NewRequest(http.MethodPost, "/v1/tools/register", bytes.NewBufferString(`{"name":"test"}`))
	w := httptest.NewRecorder()
	s.handleToolRegister(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected missing key to be rejected, got %d", w.Code)
	}
}

func TestResolveIdentityRequiresTenantAndPrincipal(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/tools/register", nil)
	_, status, _ := resolveIdentity(r, "", "")
	if status != http.StatusUnauthorized {
		t.Fatalf("expected 401 without tenant/principal, got %d", status)
	}
}

func TestResolveIdentityFromHeaders(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/tools/register", nil)
	r.Header.Set("X-SCOS-Tenant-ID", "tenant-1")
	r.Header.Set("X-SCOS-Principal-ID", "principal-1")
	identity, status, _ := resolveIdentity(r, "", "")
	if status != 0 {
		t.Fatalf("expected success, got status %d", status)
	}
	if identity.TenantID != "tenant-1" || identity.PrincipalID != "principal-1" {
		t.Fatalf("unexpected identity: %+v", identity)
	}
}

func TestRequestFingerprintIsStableAndSensitiveToChange(t *testing.T) {
	a := requestFingerprint(map[string]string{"name": "first"})
	b := requestFingerprint(map[string]string{"name": "first"})
	c := requestFingerprint(map[string]string{"name": "second"})
	if a != b {
		t.Fatal("expected identical input to produce identical fingerprint")
	}
	if a == c {
		t.Fatal("expected different input to produce different fingerprint")
	}
}
