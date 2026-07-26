package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNewCompatServer(t *testing.T) {
	srv := newCompatServer()
	if srv == nil {
		t.Fatal("expected non-nil server")
	}
	if len(srv.adapters) != 5 {
		t.Fatalf("expected 5 default adapters, got %d", len(srv.adapters))
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

func TestRegisterAgent(t *testing.T) {
	srv := newCompatServer()
	reg := AgentRegistration{
		AgentID:  "agent-claude",
		Name:     "Claude Desktop",
		Boundary: "http_sidecar",
		Status:   "active",
	}

	srv.mu.Lock()
	if adapter, ok := srv.adapters[reg.Boundary]; ok {
		reg.Tier = adapter.Tier
		reg.Adapters = adapter.Supported
		reg.Capabilities = adapter.Capabilities
	}
	reg.LastSeen = "now"
	srv.agents[reg.AgentID] = &reg
	srv.mu.Unlock()

	srv.mu.RLock()
	stored, ok := srv.agents["agent-claude"]
	srv.mu.RUnlock()
	if !ok {
		t.Fatal("expected agent to be registered")
	}
	if stored.Tier != "B" {
		t.Fatalf("expected Tier B for http_sidecar, got %s", stored.Tier)
	}
}

func TestContextEnvelopeStore(t *testing.T) {
	srv := newCompatServer()
	env := ContextEnvelope{
		AgentID:   "agent-test",
		SessionID: "session-1",
		Format:    "json",
		Facts:     []string{"PostgreSQL is the authority"},
	}

	srv.mu.Lock()
	srv.envelopes[env.SessionID] = &env
	srv.mu.Unlock()

	srv.mu.RLock()
	stored, ok := srv.envelopes["session-1"]
	srv.mu.RUnlock()
	if !ok {
		t.Fatal("expected envelope to be stored")
	}
	if len(stored.Facts) != 1 {
		t.Fatal("expected 1 fact")
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
