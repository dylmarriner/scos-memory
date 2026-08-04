package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewCompatServer(t *testing.T) {
	srv := newCompatServer()
	if srv == nil {
		t.Fatal("expected non-nil server")
	}
	if len(srv.adapters) != 10 {
		t.Fatalf("expected 10 default adapters, got %d", len(srv.adapters))
	}
}

func TestReadinessRequiresDatabase(t *testing.T) {
	// A server with no DATABASE_URL/postgres dependency configured must
	// report itself not-ready, not a hardcoded pass.
	srv := newCompatServer()
	if err := srv.readinessCheck(); err == nil {
		t.Fatal("expected readiness check to fail without a configured postgres dependency")
	}
}

func TestDefaultAdapters(t *testing.T) {
	srv := newCompatServer()

	native, ok := srv.adapters["native_sdk"]
	if !ok {
		t.Fatal("expected native_sdk adapter")
	}
	if native.Tier != "A" {
		t.Fatalf("expected Tier A for native_sdk, got %s", native.Tier)
	}

	watcher, ok := srv.adapters["session_watcher"]
	if !ok {
		t.Fatal("expected session_watcher adapter")
	}
	if watcher.Tier != "D" {
		t.Fatalf("expected Tier D for session_watcher, got %s", watcher.Tier)
	}
}

// TestAgentAdapterContractCoverage enforces governance ss23.4: every adapter
// must classify each required operation as supported or explicitly
// unsupported, with no gaps and no double-classification.
func TestAgentAdapterContractCoverage(t *testing.T) {
	srv := newCompatServer()
	for boundary, a := range srv.adapters {
		seen := make(map[string]string, len(requiredAdapterOperations))
		for _, op := range a.Supported {
			seen[op] = "supported"
		}
		for _, op := range a.Unsupported {
			if prev, ok := seen[op]; ok {
				t.Fatalf("%s: operation %q classified as both %s and unsupported", boundary, op, prev)
			}
			seen[op] = "unsupported"
		}
		for _, op := range requiredAdapterOperations {
			if _, ok := seen[op]; !ok {
				t.Errorf("%s: operation %q neither supported nor explicitly unsupported", boundary, op)
			}
		}
	}
}

// TestSupportedIntegrationBoundariesComplete enforces governance ss23.2's
// full boundary table is registered, not just a subset.
func TestSupportedIntegrationBoundariesComplete(t *testing.T) {
	srv := newCompatServer()
	required := []string{
		"native_sdk", "http_sidecar", "mcp_server", "model_api_proxy",
		"cli_wrapper", "provider_plugin", "lifecycle_hooks", "event_bridge",
		"session_watcher", "a2a_adapter",
	}
	for _, boundary := range required {
		if _, ok := srv.adapters[boundary]; !ok {
			t.Errorf("missing required integration boundary %q", boundary)
		}
	}
}

func TestUntestedBoundariesLabelledExperimental(t *testing.T) {
	srv := newCompatServer()
	for _, boundary := range []string{"model_api_proxy", "provider_plugin", "lifecycle_hooks", "event_bridge", "a2a_adapter"} {
		a, ok := srv.adapters[boundary]
		if !ok {
			t.Fatalf("expected %s adapter to be registered", boundary)
		}
		if a.Tested {
			t.Errorf("%s has no conformance evidence yet and should be labelled experimental (Tested=false)", boundary)
		}
	}
}

func TestCompatibilityTiersCoverage(t *testing.T) {
	srv := newCompatServer()
	tiers := map[string]bool{"A": false, "B": false, "C": false, "D": false}
	for _, a := range srv.adapters {
		tiers[a.Tier] = true
	}
	for tier, found := range tiers {
		if !found {
			t.Errorf("missing adapter for Tier %s", tier)
		}
	}
}

func TestRenderMarkdown(t *testing.T) {
	env := ContextEnvelope{
		AgentID:   "agent-1",
		SessionID: "session-1",
		Facts:     []string{"Fact 1", "Fact 2"},
		TaskState: "In progress",
		Sources:   []string{"memory::123"},
	}
	rendered := renderMarkdown(&env)
	if !strings.Contains(rendered, "## Facts") {
		t.Fatal("markdown should contain Facts section")
	}
	if !strings.Contains(rendered, "Fact 1") {
		t.Fatal("markdown should contain Fact 1")
	}
	if !strings.Contains(rendered, "memory::123") {
		t.Fatal("markdown should contain source reference")
	}
}

func TestRenderText(t *testing.T) {
	env := ContextEnvelope{
		AgentID:   "agent-1",
		SessionID: "session-1",
		Facts:     []string{"Important fact"},
	}
	rendered := renderText(&env)
	if !strings.Contains(rendered, "FACTS:") {
		t.Fatal("text should contain FACTS section")
	}
	if !strings.Contains(rendered, "Important fact") {
		t.Fatal("text should contain the fact")
	}
}

// TestRenderPreservesAllEnvelopeSections enforces governance ss23.5: the
// envelope must render into the target agent's supported form without
// dropping any of its declared sections.
func TestRenderPreservesAllEnvelopeSections(t *testing.T) {
	env := ContextEnvelope{
		AgentID:         "agent-1",
		SessionID:       "session-1",
		EpisodicHistory: []string{"turn-1 summary"},
		CodeRefs:        []string{"pkg/foo.go:42"},
		Artifacts:       []string{"artifact::report"},
		RestorationRefs: []string{"checkpoint::abc"},
	}
	md := renderMarkdown(&env)
	for _, want := range []string{"turn-1 summary", "pkg/foo.go:42", "artifact::report", "checkpoint::abc"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown rendering dropped section content %q", want)
		}
	}
	text := renderText(&env)
	if !strings.Contains(text, "turn-1 summary") {
		t.Error("text rendering dropped episodic history")
	}
}

func TestRenderJSON(t *testing.T) {
	env := ContextEnvelope{
		AgentID:   "agent-1",
		SessionID: "session-1",
	}
	rendered := renderJSON(&env)
	if !strings.Contains(rendered, `"agent_id"`) {
		t.Fatal("JSON should contain agent_id")
	}
}

func TestRenderEmptyEnvelope(t *testing.T) {
	env := ContextEnvelope{AgentID: "a", SessionID: "s"}
	rendered := renderText(&env)
	if !strings.Contains(rendered, "Context for Agent a") {
		t.Fatal("should render basic info")
	}
}

func TestJSONRoundTripEnvelope(t *testing.T) {
	env := ContextEnvelope{
		AgentID:   "agent-test",
		SessionID: "session-1",
		Facts:     []string{"fact 1"},
		Format:    "json",
	}
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ContextEnvelope
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.AgentID != "agent-test" {
		t.Fatal("roundtrip failed")
	}
}

func TestTranslateOpenAIToSCOS(t *testing.T) {
	source := map[string]any{
		"role":    "user",
		"content": "Hello SCOS",
	}
	translated := translateEvent("message", source, "openai")
	if translated["provenance"] != "openai_compatible" {
		t.Fatal("expected openai_compatible provenance")
	}
	if translated["role"] != "user" {
		t.Fatal("expected role to be preserved")
	}
}

func TestTranslateMCPToSCOS(t *testing.T) {
	source := map[string]any{
		"tool":  "read_file",
		"input": "test.txt",
	}
	translated := translateEvent("tool_call", source, "mcp")
	if translated["provenance"] != "mcp" {
		t.Fatal("expected mcp provenance")
	}
}

func TestTranslateAnthropicToSCOS(t *testing.T) {
	source := map[string]any{"text": "Hello"}
	translated := translateEvent("message", source, "anthropic")
	if translated["provenance"] != "anthropic" {
		t.Fatal("expected anthropic provenance")
	}
}

func TestTranslateDefault(t *testing.T) {
	source := "raw data"
	translated := translateEvent("unknown", source, "custom")
	if translated["provenance"] != "custom" {
		t.Fatal("expected custom provenance")
	}
}

func TestResolveIdentityRequiresTenantAndPrincipal(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/agents", nil)
	_, status, _ := resolveIdentity(r, "", "")
	if status != http.StatusUnauthorized {
		t.Fatalf("expected 401 without tenant/principal, got %d", status)
	}
}

func TestResolveIdentityFromHeaders(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/agents", nil)
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

func TestAgentByIDMissingRequiresIdentity(t *testing.T) {
	srv := newCompatServer()
	r := httptest.NewRequest(http.MethodGet, "/v1/agents/unknown", nil)
	w := httptest.NewRecorder()
	srv.handleAgentByID(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without tenant/principal identity, got %d", w.Code)
	}
}
