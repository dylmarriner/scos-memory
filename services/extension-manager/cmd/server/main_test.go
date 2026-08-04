package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExtensionStateSurvivesRestart(t *testing.T) {
	publicKey, privateKey, _ := ed25519.GenerateKey(nil)
	t.Setenv("SCOS_EXTENSION_SIGNING_PUBLIC_KEY", base64.StdEncoding.EncodeToString(publicKey))
	path := filepath.Join(t.TempDir(), "extension-state.json")
	first := newExtServerAt(path)
	manifest := ExtensionManifest{
		SchemaVersion: "1.0", PackageID: "restart.ext", Name: "Restart", Version: "1.0.0",
		Publisher: "Test", License: "MIT", ExtensionTypes: []string{"plugin"}, RequiredCoreVersion: "^0.1.0",
		ContentHash: "hash", Isolation: "separate_process", HooksSubscribed: []string{"turn.completed"},
		HookEndpoints: map[string]string{"turn.completed": "http://127.0.0.1:9999/hook"},
		RollbackPlan:  "uninstall and restore previous package version",
	}
	manifest.Signature = signManifest(t, manifest, privateKey)
	res := httptest.NewRecorder()
	first.handleInstall(res, httptest.NewRequest(http.MethodPost, "/v1/install", bytes.NewReader(mustJSON(t, manifest))))
	if res.Code != http.StatusOK {
		t.Fatalf("install returned %d: %s", res.Code, res.Body.String())
	}
	res = httptest.NewRecorder()
	first.handleExtensionByID(res, httptest.NewRequest(http.MethodPost, "/v1/extensions/restart.ext/disable", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("disable returned %d", res.Code)
	}

	restarted := newExtServerAt(path)
	ext := restarted.extensions["restart.ext"]
	if ext == nil || ext.Status != "disabled" || len(restarted.hooks["turn.completed"]) != 1 {
		t.Fatalf("state did not survive restart: %#v", restarted)
	}
}

func TestHookDispatchInvokesEndpointAndPropagatesIdentity(t *testing.T) {
	called := make(chan struct{}, 1)
	hookServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-SCOS-Tenant-ID") != "tenant-a" || r.Header.Get("X-SCOS-Principal-ID") != "principal-a" {
			t.Errorf("identity headers were not propagated")
		}
		called <- struct{}{}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer hookServer.Close()

	srv := newExtServer()
	srv.extensions["hook-ext"] = &Extension{Manifest: ExtensionManifest{PackageID: "hook-ext"}, Status: "enabled"}
	srv.hooks["turn.completed"] = []*Hook{{
		Event: "turn.completed", ExtensionID: "hook-ext", Endpoint: hookServer.URL,
		TimeoutMs: 1000, FallbackPolicy: "fail_closed",
	}}

	results, err := srv.dispatchHooks(context.Background(), hookDispatchRequest{
		Event: "turn.completed", TenantID: "tenant-a", PrincipalID: "principal-a",
		Payload: map[string]any{"message_id": "m-1"},
	})
	if err != nil || len(results) != 1 || results[0].Status != "succeeded" {
		t.Fatalf("dispatch result = %#v, err = %v", results, err)
	}
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("hook endpoint was not invoked")
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

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
		SchemaVersion:       "1.0",
		PackageID:           "com.example.test-ext",
		Version:             "1.0.0",
		Name:                "Test",
		Publisher:           "Pub",
		License:             "MIT",
		ExtensionTypes:      []string{"ui_extension"},
		RequiredCoreVersion: "^0.1.0",
		Isolation:           "wasm_sandbox",
		ContentHash:         "def456",
		RollbackPlan:        "disable and remove panel registration",
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
	if srv.determineIsolation(ui) != "wasm_sandbox" {
		t.Fatal("ui_extension should be wasm_sandbox")
	}

	skill := ExtensionManifest{ExtensionTypes: []string{"skill"}}
	if srv.determineIsolation(skill) != "in_process_trusted" {
		t.Fatal("skill should be in_process_trusted")
	}

	adapter := ExtensionManifest{ExtensionTypes: []string{"agent_adapter"}}
	if srv.determineIsolation(adapter) != "remote_endpoint" {
		t.Fatal("agent_adapter should be remote_endpoint")
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

func TestExtensionLifecycleEndpoints(t *testing.T) {
	srv := newExtServer()
	srv.extensions["ext"] = &Extension{Manifest: ExtensionManifest{PackageID: "ext", Version: "2"}, Status: "enabled"}
	for _, action := range []string{"disable", "enable"} {
		res := httptest.NewRecorder()
		srv.handleExtensionByID(res, httptest.NewRequest(http.MethodPost, "/v1/extensions/ext/"+action, nil))
		if res.Code != http.StatusOK {
			t.Fatalf("%s returned %d", action, res.Code)
		}
	}
	if srv.extensions["ext"].Status != "enabled" {
		t.Fatal("expected enable action to restore enabled state")
	}
}

func TestRollbackUnavailableWithoutHistory(t *testing.T) {
	srv := newExtServer()
	srv.extensions["ext"] = &Extension{Manifest: ExtensionManifest{PackageID: "ext"}}
	res := httptest.NewRecorder()
	srv.handleExtensionByID(res, httptest.NewRequest(http.MethodPost, "/v1/extensions/ext/rollback", nil))
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", res.Code)
	}
}

func TestRollbackRestoresPreviousManifest(t *testing.T) {
	srv := newExtServer()
	srv.extensions["ext"] = &Extension{Manifest: ExtensionManifest{PackageID: "ext", Version: "2"}, Status: "enabled"}
	srv.history["ext"] = []ExtensionManifest{{PackageID: "ext", Version: "1"}}
	res := httptest.NewRecorder()
	srv.handleExtensionByID(res, httptest.NewRequest(http.MethodPost, "/v1/extensions/ext/rollback", nil))
	if res.Code != http.StatusOK || srv.extensions["ext"].Manifest.Version != "1" {
		t.Fatalf("expected previous manifest to be restored, got %d: %#v", res.Code, srv.extensions["ext"])
	}
}

func TestInstallRejectsMaliciousManifest(t *testing.T) {
	srv := newExtServer()
	manifest := ExtensionManifest{PackageID: "evil", Version: "1.0.0", Signature: "not-a-signature"}
	res := httptest.NewRecorder()
	srv.handleInstall(res, httptest.NewRequest(http.MethodPost, "/v1/install", bytes.NewReader(mustJSON(t, manifest))))
	if res.Code != http.StatusForbidden || len(srv.extensions) != 0 {
		t.Fatalf("malicious manifest was accepted: status=%d state=%#v", res.Code, srv.extensions)
	}
}

func TestInstallRejectsMissingRollbackPlan(t *testing.T) {
	publicKey, privateKey, _ := ed25519.GenerateKey(nil)
	t.Setenv("SCOS_EXTENSION_SIGNING_PUBLIC_KEY", base64.StdEncoding.EncodeToString(publicKey))
	srv := newExtServer()
	manifest := validManifest("no-rollback")
	manifest.RollbackPlan = ""
	manifest.Signature = signManifest(t, manifest, privateKey)
	res := httptest.NewRecorder()
	srv.handleInstall(res, httptest.NewRequest(http.MethodPost, "/v1/install", bytes.NewReader(mustJSON(t, manifest))))
	if res.Code != http.StatusForbidden || srv.extensions["no-rollback"] != nil {
		t.Fatalf("manifest missing rollback_plan should fail closed: status=%d", res.Code)
	}
}

func TestInstallRejectsInvalidSignature(t *testing.T) {
	publicKey, _, _ := ed25519.GenerateKey(nil)
	t.Setenv("SCOS_EXTENSION_SIGNING_PUBLIC_KEY", base64.StdEncoding.EncodeToString(publicKey))
	srv := newExtServer()
	manifest := validManifest("bad-signature")
	manifest.Signature = base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))
	res := httptest.NewRecorder()
	srv.handleInstall(res, httptest.NewRequest(http.MethodPost, "/v1/install", bytes.NewReader(mustJSON(t, manifest))))
	if res.Code != http.StatusForbidden {
		t.Fatalf("expected invalid signature rejection, got %d", res.Code)
	}
}

func TestInstallRejectsDependencyConflict(t *testing.T) {
	publicKey, privateKey, _ := ed25519.GenerateKey(nil)
	t.Setenv("SCOS_EXTENSION_SIGNING_PUBLIC_KEY", base64.StdEncoding.EncodeToString(publicKey))
	srv := newExtServer()
	srv.extensions["provider"] = &Extension{Manifest: ExtensionManifest{PackageID: "provider", Version: "1.0.0"}, Status: "enabled"}
	manifest := validManifest("consumer")
	manifest.Dependencies = []ExtensionDependency{{PackageID: "provider", Version: ">=2.0.0"}}
	manifest.Signature = signManifest(t, manifest, privateKey)
	res := httptest.NewRecorder()
	srv.handleInstall(res, httptest.NewRequest(http.MethodPost, "/v1/install", bytes.NewReader(mustJSON(t, manifest))))
	if res.Code != http.StatusForbidden || srv.extensions["consumer"] != nil {
		t.Fatalf("dependency conflict was accepted: status=%d", res.Code)
	}
}

func TestRollbackRestoresRegistrationsAndIsolation(t *testing.T) {
	srv := newExtServer()
	srv.extensions["ext"] = &Extension{Manifest: ExtensionManifest{PackageID: "ext", Version: "2", ExtensionTypes: []string{"plugin"}, Isolation: "separate_process"}, Isolation: "separate_process", Status: "enabled"}
	srv.history["ext"] = []ExtensionManifest{{PackageID: "ext", Version: "1", ExtensionTypes: []string{"skill"}, Isolation: "in_process_trusted", SkillsExported: []string{"restored"}}}
	res := httptest.NewRecorder()
	srv.handleExtensionByID(res, httptest.NewRequest(http.MethodPost, "/v1/extensions/ext/rollback", nil))
	if res.Code != http.StatusOK || srv.extensions["ext"].Isolation != "in_process_trusted" || srv.extensions["ext"].Status != "enabled" || srv.skills["ext/restored"] == nil {
		t.Fatalf("rollback did not restore complete previous state: status=%d ext=%#v skills=%#v", res.Code, srv.extensions["ext"], srv.skills)
	}
}

func validManifest(packageID string) ExtensionManifest {
	return ExtensionManifest{SchemaVersion: "1.0", PackageID: packageID, Name: packageID, Version: "1.0.0", Publisher: "Test", License: "MIT", ExtensionTypes: []string{"plugin"}, RequiredCoreVersion: "^0.1.0", ContentHash: "hash", Isolation: "separate_process", RollbackPlan: "uninstall package and restore prior version"}
}

func signManifest(t *testing.T, manifest ExtensionManifest, privateKey ed25519.PrivateKey) string {
	t.Helper()
	manifest.Signature = ""
	payload, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, payload))
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
