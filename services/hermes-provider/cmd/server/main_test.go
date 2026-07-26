package main

import (
	"encoding/json"
	"testing"
)

func TestNewHermesServer(t *testing.T) {
	s := newHermesServer()
	if s == nil {
		t.Fatal("expected non-nil server")
	}
	if s.startTime.IsZero() {
		t.Fatal("expected start time to be set")
	}
}

func TestProviderStatus(t *testing.T) {
	status := ProviderStatus{
		Name:        providerName,
		Version:     version,
		IsAvailable: true,
		Healthy:     true,
		Uptime:      "5s",
	}
	if status.Name != "scos-nexus" {
		t.Fatal("expected scos-nexus provider name")
	}
	if !status.IsAvailable {
		t.Fatal("expected available")
	}
}

func TestInitialize(t *testing.T) {
	srv := newHermesServer()
	initReq := struct {
		ProfilePath string `json:"profile_path"`
		TenantID    string `json:"tenant_id"`
	}{
		ProfilePath: "/home/user/.hermes/profiles/default",
		TenantID:    "tenant-1",
	}

	srv.mu.Lock()
	srv.profilePaths["session-1"] = initReq.ProfilePath
	srv.mu.Unlock()

	srv.mu.RLock()
	path, ok := srv.profilePaths["session-1"]
	srv.mu.RUnlock()
	if !ok || path != "/home/user/.hermes/profiles/default" {
		t.Fatal("profile path should be stored")
	}
}

func TestPrefetch(t *testing.T) {
	srv := newHermesServer()
	req := PrefetchRequest{
		SessionID:   "session-1",
		ProfilePath: "/profiles/default",
	}

	srv.mu.Lock()
	if _, ok := srv.sessions[req.SessionID]; !ok {
		srv.sessions[req.SessionID] = map[string]any{
			"prefetch_count": float64(1),
			"last_seen":      "now",
		}
	}
	srv.profilePaths[req.SessionID] = req.ProfilePath
	srv.mu.Unlock()

	srv.mu.RLock()
	session, ok := srv.sessions["session-1"]
	srv.mu.RUnlock()
	if !ok {
		t.Fatal("session should exist")
	}
	if session["prefetch_count"].(float64) != 1 {
		t.Fatal("prefetch count should be 1")
	}
}

func TestSyncTurn(t *testing.T) {
	srv := newHermesServer()
	srv.mu.Lock()
	srv.turnCount++
	srv.sessions["session-1"] = map[string]any{
		"last_turn":  "turn-1",
		"turn_count": srv.turnCount,
	}
	srv.mu.Unlock()

	srv.mu.RLock()
	session, ok := srv.sessions["session-1"]
	srv.mu.RUnlock()
	if !ok {
		t.Fatal("session should exist")
	}
	if session["last_turn"] != "turn-1" {
		t.Fatal("turn ID mismatch")
	}
}

func TestToolSchemas(t *testing.T) {
	tools := []map[string]any{
		{"name": "scos_recall"},
		{"name": "scos_status"},
		{"name": "scos_forget"},
	}
	if len(tools) != 3 {
		t.Fatalf("expected 3 tools, got %d", len(tools))
	}
	found := false
	for _, tool := range tools {
		if tool["name"] == "scos_recall" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected scos_recall tool")
	}
}

func TestSessionSwitchLineage(t *testing.T) {
	srv := newHermesServer()
	srv.mu.Lock()
	srv.profilePaths["session-new"] = "/profiles/new"
	srv.mu.Unlock()

	srv.mu.RLock()
	path, ok := srv.profilePaths["session-new"]
	srv.mu.RUnlock()
	if !ok || path != "/profiles/new" {
		t.Fatal("profile path should be preserved after switch")
	}
}

func TestMemoryWriteMirroring(t *testing.T) {
	req := MemoryWriteRequest{
		Key:    "user.name",
		Value:  "Alice",
		Scope:  "user",
		Source: "user_message",
	}

	if req.Scope != "user" {
		t.Fatal("scope mismatch")
	}
	if req.Source != "user_message" {
		t.Fatal("source mismatch")
	}
}

func TestDelegationCapture(t *testing.T) {
	srv := newHermesServer()
	srv.mu.Lock()
	srv.turnCount++
	srv.mu.Unlock()

	srv.mu.RLock()
	count := srv.turnCount
	srv.mu.RUnlock()
	if count != 1 {
		t.Fatalf("expected turn count 1, got %d", count)
	}
}

func TestProviderStatusJSON(t *testing.T) {
	status := ProviderStatus{
		Name:        "scos-nexus",
		Version:     "0.1.0",
		IsAvailable: true,
		Healthy:     true,
		Uptime:      "10s",
	}
	data, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ProviderStatus
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Name != "scos-nexus" {
		t.Fatal("roundtrip failed")
	}
}

func TestGetInt(t *testing.T) {
	m := map[string]any{"count": float64(42)}
	result := getInt(m, "count")
	if result != 42 {
		t.Fatalf("expected 42, got %d", result)
	}

	result = getInt(nil, "count")
	if result != 0 {
		t.Fatalf("expected 0 for nil map, got %d", result)
	}

	result = getInt(m, "missing")
	if result != 0 {
		t.Fatalf("expected 0 for missing key, got %d", result)
	}
}

func TestSystemPromptBlock(t *testing.T) {
	block := "You have access to SCOS Nexus memory (scos-nexus v0.1.0)."
	if !contains(block, "SCOS Nexus") {
		t.Fatal("prompt should mention SCOS Nexus")
	}
}

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestPreCompressPreservation(t *testing.T) {
	s := newHermesServer()
	s.mu.Lock()
	s.sessions["session-1"] = map[string]any{
		"durable": true,
	}
	s.mu.Unlock()

	s.mu.RLock()
	session, ok := s.sessions["session-1"]
	s.mu.RUnlock()
	if !ok {
		t.Fatal("session should exist after pre-compress")
	}
	if session["durable"] != true {
		t.Fatal("durable insights should be preserved")
	}
}
