package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNewAgentRegistry(t *testing.T) {
	s := newAgentRegistryServer()
	if s == nil {
		t.Fatal("expected non-nil server")
	}
}

func TestReadinessRequiresDatabase(t *testing.T) {
	s := newAgentRegistryServer()
	if s.readinessCheck() == nil {
		t.Fatal("expected readiness failure without a configured postgres dependency")
	}
}

func TestAgentRegisterRequiresGovernanceFields(t *testing.T) {
	if err := validateAgentRegistration(AgentRegistration{Name: "coder-agent"}); err == nil {
		t.Fatal("expected validation error for incomplete registration")
	}
	complete := AgentRegistration{
		Name: "coder-agent", Version: "1.0", Maintainer: "scos-team",
		Capabilities:  []string{"code-generation"},
		Inputs:        []string{"repository-context"},
		Outputs:       []string{"patch"},
		ModelProvider: "anthropic", CostPolicy: "standard", HealthEndpoint: "http://coder-agent/health/live",
	}
	if err := validateAgentRegistration(complete); err != nil {
		t.Fatalf("expected complete registration to validate, got %v", err)
	}
}

// TestAgentRegisterRequiresCapabilitiesInputsOutputs guards GOVERNANCE.md
// §29: "Every agent SHALL register identity, version, maintainer,
// capabilities, inputs, outputs, ...". Omitting any of the three must fail
// validation the same way an omitted maintainer does.
func TestAgentRegisterRequiresCapabilitiesInputsOutputs(t *testing.T) {
	base := AgentRegistration{
		Name: "coder-agent", Version: "1.0", Maintainer: "scos-team",
		Capabilities:  []string{"code-generation"},
		Inputs:        []string{"repository-context"},
		Outputs:       []string{"patch"},
		ModelProvider: "anthropic", CostPolicy: "standard", HealthEndpoint: "http://coder-agent/health/live",
	}

	missingCapabilities := base
	missingCapabilities.Capabilities = nil
	if err := validateAgentRegistration(missingCapabilities); err == nil {
		t.Fatal("expected validation error for missing capabilities")
	}

	missingInputs := base
	missingInputs.Inputs = nil
	if err := validateAgentRegistration(missingInputs); err == nil {
		t.Fatal("expected validation error for missing inputs")
	}

	missingOutputs := base
	missingOutputs.Outputs = nil
	if err := validateAgentRegistration(missingOutputs); err == nil {
		t.Fatal("expected validation error for missing outputs")
	}
}

func TestHandleAgentRegisterRejectsIncompleteBody(t *testing.T) {
	s := newAgentRegistryServer()
	body, _ := json.Marshal(map[string]string{"name": "coder-agent"})
	req := httptest.NewRequest(http.MethodPost, "/v1/agents/register", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleAgentRegister(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected bad request for incomplete registration, got %d", rec.Code)
	}
}

func TestHandleAgentByIDRejectsNonUUID(t *testing.T) {
	s := newAgentRegistryServer()
	req := httptest.NewRequest(http.MethodGet, "/v1/agents/agent-1", nil)
	rec := httptest.NewRecorder()
	s.handleAgentByID(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected bad request for non-UUID agent id, got %d", rec.Code)
	}
}

func TestHandleAgentHeartbeatRejectsNonUUID(t *testing.T) {
	s := newAgentRegistryServer()
	body, _ := json.Marshal(map[string]string{"agent_id": "agent-1"})
	req := httptest.NewRequest(http.MethodPost, "/v1/agents/heartbeat", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleAgentHeartbeat(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected bad request for non-UUID agent id, got %d", rec.Code)
	}
}

func TestHandleHandoffInitiateRequiresObjective(t *testing.T) {
	s := newAgentRegistryServer()
	body, _ := json.Marshal(map[string]string{"from_agent": "agent-a", "to_agent": "agent-b"})
	req := httptest.NewRequest(http.MethodPost, "/v1/handoffs/initiate", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleHandoffInitiate(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected bad request without an objective, got %d", rec.Code)
	}
}

func TestHandleHandoffByIDRejectsNonUUID(t *testing.T) {
	s := newAgentRegistryServer()
	req := httptest.NewRequest(http.MethodGet, "/v1/handoffs/handoff-1", nil)
	rec := httptest.NewRecorder()
	s.handleHandoffByID(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected bad request for non-UUID handoff id, got %d", rec.Code)
	}
}

func TestHandleHandoffRespondRejectsNonUUID(t *testing.T) {
	s := newAgentRegistryServer()
	body, _ := json.Marshal(map[string]string{"handoff_id": "handoff-1", "response": "accepted"})
	req := httptest.NewRequest(http.MethodPost, "/v1/handoffs/respond", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleHandoffRespond(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected bad request for non-UUID handoff id, got %d", rec.Code)
	}
}

func TestResolveIdentityRequiresTenantAndPrincipal(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/v1/agents", nil)
	_, status, _ := resolveIdentity(r, "", "")
	if status != http.StatusUnauthorized {
		t.Fatalf("expected 401 without tenant/principal, got %d", status)
	}
}

func TestResolveIdentityFromHeaders(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/v1/agents", nil)
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

// TestAgentEffectiveStatusReflectsRealStaleness proves the reported status
// changes when the underlying real state (elapsed time since last
// heartbeat) changes, rather than echoing back a persisted "active" value
// forever (ss42 prohibits unconditional/fabricated health claims -- this
// mirrors the fix applied to compatibility-gateway's adapter HealthStatus).
func TestAgentEffectiveStatusReflectsRealStaleness(t *testing.T) {
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)

	fresh := now.Add(-1 * time.Hour).Format(time.RFC3339)
	if got := agentEffectiveStatus("active", fresh, now); got != "active" {
		t.Fatalf("expected recently-seen agent to remain active, got %q", got)
	}

	stale := now.Add(-48 * time.Hour).Format(time.RFC3339)
	if got := agentEffectiveStatus("active", stale, now); got != "inactive" {
		t.Fatalf("expected agent silent for 48h to be reported inactive, got %q", got)
	}

	// A non-"active" persisted status (e.g. operator-set "degraded") must
	// pass through untouched regardless of staleness.
	if got := agentEffectiveStatus("degraded", stale, now); got != "degraded" {
		t.Fatalf("expected explicit non-active status to pass through, got %q", got)
	}

	// An unparseable last_seen_at fails open to the stored value rather than
	// fabricating a claim in either direction.
	if got := agentEffectiveStatus("active", "not-a-timestamp", now); got != "active" {
		t.Fatalf("expected unparseable last_seen to fail open to stored status, got %q", got)
	}
}

func TestIsUUID(t *testing.T) {
	if !isUUID("123e4567-e89b-12d3-a456-426614174000") {
		t.Fatal("expected valid UUID to be recognised")
	}
	for _, bad := range []string{"", "agent-1", "a1", "123e4567e89b12d3a456426614174000"} {
		if isUUID(bad) {
			t.Fatalf("expected %q to be rejected as a UUID", bad)
		}
	}
}

func TestJSONList(t *testing.T) {
	if string(jsonList(nil)) != "[]" {
		t.Fatal("expected nil slice to encode as an empty JSON array")
	}
	if string(jsonList([]string{"a", "b"})) != `["a","b"]` {
		t.Fatal("expected slice to round-trip through JSON")
	}
}

func TestNullableText(t *testing.T) {
	if nullableText("") != nil {
		t.Fatal("expected empty string to become nil")
	}
	if nullableText("x") != "x" {
		t.Fatal("expected non-empty string to pass through")
	}
}
