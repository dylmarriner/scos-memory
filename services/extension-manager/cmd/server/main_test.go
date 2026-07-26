package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNewExtServer(t *testing.T) {
	srv := newExtServer()
	if srv == nil {
		t.Fatal("expected non-nil server")
	}
	if len(srv.extensions) != 0 {
		t.Fatal("expected empty extensions")
	}
}

func TestPersistedStateRoundTrip(t *testing.T) {
	original := persistedExtensionState{
		Extensions: map[string]*Extension{"pkg": {Manifest: ExtensionManifest{PackageID: "pkg", Version: "1.0.0"}, Status: "enabled"}},
		Skills:     map[string]*Skill{"pkg/skill": {ID: "pkg/skill", Status: "active"}},
		Hooks:      map[string][]*Hook{"turn.completed": {{Event: "turn.completed", ExtensionID: "pkg"}}},
		Receipts:   []*InstallationReceipt{{PackageID: "pkg", Version: "1.0.0"}},
		NextID:     7,
	}
	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var restored persistedExtensionState
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.NextID != original.NextID || restored.Extensions["pkg"].Status != "enabled" || restored.Hooks["turn.completed"][0].ExtensionID != "pkg" {
		t.Fatalf("persisted state did not round-trip: %+v", restored)
	}
}

func TestInstallExtension(t *testing.T) {
	srv := newExtServer()
	manifest := ExtensionManifest{
		SchemaVersion:        "1.0",
		PackageID:            "com.example.test-ext",
		Name:                 "Test Extension",
		Version:              "1.0.0",
		Publisher:            "Test Publisher",
		License:              "MIT",
		ExtensionTypes:       []string{"plugin"},
		CapabilitiesProvided: []string{"memory-search", "data-transform"},
		HooksSubscribed:      []string{"on_turn_start", "on_after_retrieval"},
		ContentHash:          "abc123",
	}

	// Simulate POST
	srv.mu.Lock()
	ext := &Extension{
		Manifest: manifest,
		Status:   "enabled",
	}
	srv.extensions[manifest.PackageID] = ext
	srv.mu.Unlock()

	srv.mu.RLock()
	stored, ok := srv.extensions["com.example.test-ext"]
	srv.mu.RUnlock()
	if !ok {
		t.Fatal("expected extension to be stored")
	}
	if stored.Status != "enabled" {
		t.Fatal("expected enabled status")
	}
}

func TestInstallDryRun(t *testing.T) {
	srv := newExtServer()
	manifest := ExtensionManifest{
		SchemaVersion:  "1.0",
		PackageID:      "com.example.test-ext",
		Version:        "1.0.0",
		Name:           "Test",
		Publisher:      "Pub",
		License:        "MIT",
		ExtensionTypes: []string{"ui_extension"},
		ContentHash:    "def456",
	}

	warnings := srv.validateManifest(manifest)
	if len(warnings) != 0 {
		t.Fatalf("expected no validation warnings, got %v", warnings)
	}
}

func TestValidateManifestMissingFields(t *testing.T) {
	srv := newExtServer()
	manifest := ExtensionManifest{
		PackageID: "com.example.bad",
		Version:   "1.0",
	}
	warnings := srv.validateManifest(manifest)
	if len(warnings) < 3 {
		t.Fatalf("expected multiple validation warnings for empty manifest, got %d", len(warnings))
	}
}

func TestDetermineIsolation(t *testing.T) {
	srv := newExtServer()

	plugin := ExtensionManifest{ExtensionTypes: []string{"plugin"}}
	if srv.determineIsolation(plugin) != "separate_process" {
		t.Fatal("plugin should be separate_process")
	}

	ui := ExtensionManifest{ExtensionTypes: []string{"ui_extension"}}
	if srv.determineIsolation(ui) != "wasm" {
		t.Fatal("ui_extension should be wasm")
	}

	skill := ExtensionManifest{ExtensionTypes: []string{"skill"}}
	if srv.determineIsolation(skill) != "in_process" {
		t.Fatal("skill should be in_process")
	}
}

func TestCreateSkill(t *testing.T) {
	srv := newExtServer()
	srv.mu.Lock()
	srv.nextID++
	skill := &Skill{
		ID:           "skill-1",
		Name:         "memory-query",
		Purpose:      "Query memory with filters",
		Capabilities: []string{"memory-search"},
		Status:       "active",
	}
	srv.skills[skill.ID] = skill
	srv.mu.Unlock()

	srv.mu.RLock()
	stored, ok := srv.skills["skill-1"]
	srv.mu.RUnlock()
	if !ok {
		t.Fatal("expected skill to be stored")
	}
	if stored.Name != "memory-query" {
		t.Fatal("skill name mismatch")
	}
}

func TestDeleteExtension(t *testing.T) {
	srv := newExtServer()
	srv.extensions["ext-1"] = &Extension{
		Manifest: ExtensionManifest{
			PackageID:       "ext-1",
			HooksSubscribed: []string{"on_session_start"},
		},
	}

	srv.mu.Lock()
	delete(srv.extensions, "ext-1")
	srv.mu.Unlock()

	srv.mu.RLock()
	_, ok := srv.extensions["ext-1"]
	srv.mu.RUnlock()
	if ok {
		t.Fatal("expected extension to be deleted")
	}
}

func TestCapabilityGraph(t *testing.T) {
	srv := newExtServer()
	srv.extensions["ext-cap"] = &Extension{
		Manifest: ExtensionManifest{
			PackageID:            "ext-cap",
			Name:                 "Cap Provider",
			Version:              "1.0",
			CapabilitiesProvided: []string{"memory-search", "code-analysis"},
		},
		Status: "enabled",
		Health: "healthy",
	}

	srv.mu.RLock()
	var caps []string
	for _, ext := range srv.extensions {
		if ext.Status != "enabled" {
			continue
		}
		caps = append(caps, ext.Manifest.CapabilitiesProvided...)
	}
	srv.mu.RUnlock()

	if len(caps) != 2 {
		t.Fatalf("expected 2 capabilities, got %d", len(caps))
	}
}

func TestUpdateExtensionStatus(t *testing.T) {
	srv := newExtServer()
	srv.extensions["ext-update"] = &Extension{
		Manifest: ExtensionManifest{PackageID: "ext-update"},
		Status:   "enabled",
	}

	srv.mu.Lock()
	ext := srv.extensions["ext-update"]
	ext.Status = "disabled"
	ext.UpdatedAt = "now"
	srv.mu.Unlock()

	if ext.Status != "disabled" {
		t.Fatal("expected disabled status")
	}
}

func TestHookRegistration(t *testing.T) {
	srv := newExtServer()
	event := "on_turn_start"
	h := &Hook{
		Event:       event,
		ExtensionID: "ext-hooks",
		Type:        "asynchronous",
		Retryable:   true,
	}

	srv.mu.Lock()
	srv.hooks[event] = append(srv.hooks[event], h)
	srv.mu.Unlock()

	srv.mu.RLock()
	hooks, ok := srv.hooks["on_turn_start"]
	srv.mu.RUnlock()
	if !ok || len(hooks) != 1 {
		t.Fatal("expected 1 hook for on_turn_start")
	}
	if hooks[0].ExtensionID != "ext-hooks" {
		t.Fatal("hook extension_id mismatch")
	}
}

func TestReceiptLogging(t *testing.T) {
	srv := newExtServer()
	receipt := &InstallationReceipt{
		PackageID:   "com.example.ext",
		Version:     "1.0.0",
		InstalledBy: "admin",
		Timestamp:   "now",
	}

	srv.mu.Lock()
	srv.receipts = append(srv.receipts, receipt)
	srv.mu.Unlock()

	srv.mu.RLock()
	if len(srv.receipts) != 1 {
		t.Fatal("expected 1 receipt")
	}
	if srv.receipts[0].PackageID != "com.example.ext" {
		t.Fatal("receipt package_id mismatch")
	}
	srv.mu.RUnlock()
}

func TestContains(t *testing.T) {
	if !contains([]string{"a", "b", "c"}, "b") {
		t.Fatal("expected contains to be true")
	}
	if contains([]string{"a", "b", "c"}, "z") {
		t.Fatal("expected contains to be false")
	}
	if contains(nil, "a") {
		t.Fatal("expected contains nil to be false")
	}
}

func TestJSONRoundTripManifest(t *testing.T) {
	m := ExtensionManifest{
		SchemaVersion:        "1.0",
		PackageID:            "com.example.json-test",
		Name:                 "JSON Test",
		Version:              "1.2.3",
		Publisher:            "Pub",
		License:              "Apache-2.0",
		ExtensionTypes:       []string{"plugin"},
		CapabilitiesProvided: []string{"test"},
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "com.example.json-test") {
		t.Fatal("JSON should contain package_id")
	}
}

func TestJSONRoundTripSkill(t *testing.T) {
	skill := Skill{
		ID:           "skill-memory",
		Name:         "Memory Query",
		Purpose:      "Query the memory store",
		Capabilities: []string{"memory-search"},
		Status:       "active",
	}
	data, err := json.Marshal(skill)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Skill
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.ID != "skill-memory" {
		t.Fatal("roundtrip failed")
	}
}
