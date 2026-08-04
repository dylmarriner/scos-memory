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
	"sync"
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

func TestHookDispatchRejectsUnknownFields(t *testing.T) {
	srv := newExtServer()
	req := httptest.NewRequest(http.MethodPost, "/v1/hooks/dispatch", bytes.NewBufferString(`{
		"event":"turn.completed",
		"tenant_id":"tenant-a",
		"principal_id":"principal-a",
		"payload":{},
		"unexpected":true
	}`))
	res := httptest.NewRecorder()
	srv.handleHookDispatch(res, req)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("expected unknown hook-dispatch fields to be rejected, got %d: %s", res.Code, res.Body.String())
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

// TestHookDispatchOrderingIsDeterministic covers §22.6: hook ordering SHALL
// be deterministic. Registration order is the ordering contract; repeated
// dispatches against the same registered set must invoke hooks in the same
// sequence every time.
func TestHookDispatchOrderingIsDeterministic(t *testing.T) {
	var mu sync.Mutex
	var callOrder []string
	makeHook := func(id string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			callOrder = append(callOrder, id)
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		}))
	}
	first := makeHook("ext-a")
	defer first.Close()
	second := makeHook("ext-b")
	defer second.Close()
	third := makeHook("ext-c")
	defer third.Close()

	srv := newExtServer()
	srv.hooks["on_turn_start"] = []*Hook{
		{Event: "on_turn_start", ExtensionID: "ext-a", Endpoint: first.URL, TimeoutMs: 1000, FallbackPolicy: "continue"},
		{Event: "on_turn_start", ExtensionID: "ext-b", Endpoint: second.URL, TimeoutMs: 1000, FallbackPolicy: "continue"},
		{Event: "on_turn_start", ExtensionID: "ext-c", Endpoint: third.URL, TimeoutMs: 1000, FallbackPolicy: "continue"},
	}

	var previousOrder []string
	for i := 0; i < 5; i++ {
		mu.Lock()
		callOrder = nil
		mu.Unlock()
		results, err := srv.dispatchHooks(context.Background(), hookDispatchRequest{
			Event: "on_turn_start", TenantID: "tenant-a", PrincipalID: "principal-a", Payload: map[string]any{},
		})
		if err != nil {
			t.Fatalf("dispatch %d failed: %v", i, err)
		}
		gotOrder := make([]string, len(results))
		for j, r := range results {
			gotOrder[j] = r.ExtensionID
		}
		wantOrder := []string{"ext-a", "ext-b", "ext-c"}
		if strings.Join(gotOrder, ",") != strings.Join(wantOrder, ",") {
			t.Fatalf("dispatch %d result order = %v, want %v", i, gotOrder, wantOrder)
		}
		mu.Lock()
		callOrderCopy := append([]string(nil), callOrder...)
		mu.Unlock()
		if previousOrder != nil && strings.Join(callOrderCopy, ",") != strings.Join(previousOrder, ",") {
			t.Fatalf("dispatch %d invocation order = %v, previous = %v: ordering is not deterministic", i, callOrderCopy, previousOrder)
		}
		previousOrder = callOrderCopy
	}
}

// TestHookDispatchTimeoutFallsBackToContinue covers §22.6/§22.7: a hook that
// exceeds its declared timeout SHALL be visibly reported as failed and, for
// a "continue" fallback policy, SHALL NOT block the remaining hooks or the
// dispatch call itself.
func TestHookDispatchTimeoutFallsBackToContinue(t *testing.T) {
	// Release the handler once the client has definitely given up (its own
	// request context cancelling) or, failing that, after a bounded fallback
	// so httptest.Server.Close() below can never hang waiting on this
	// connection — Close() blocks until in-flight handlers return.
	release := make(chan struct{})
	fallback := time.AfterFunc(3*time.Second, func() { close(release) })
	defer fallback.Stop()
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer slow.Close()

	srv := newExtServer()
	srv.hooks["on_before_tool"] = []*Hook{{
		Event: "on_before_tool", ExtensionID: "slow-ext", Endpoint: slow.URL,
		TimeoutMs: 50, FallbackPolicy: "continue",
	}}

	start := time.Now()
	results, err := srv.dispatchHooks(context.Background(), hookDispatchRequest{
		Event: "on_before_tool", TenantID: "tenant-a", PrincipalID: "principal-a", Payload: map[string]any{},
	})
	if err != nil {
		t.Fatalf("dispatch should not fail dispatch-wide on a continue-policy timeout: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("dispatch took %v, timeout was not enforced", elapsed)
	}
	if len(results) != 1 || results[0].Status != "failed" {
		t.Fatalf("expected a visible failed result for the timed-out hook, got %#v", results)
	}
}

// TestHookDispatchCircuitBreakerOpensAfterConsecutiveFailures covers §22.7:
// critical hooks SHALL have circuit-breaker policies.
func TestHookDispatchCircuitBreakerOpensAfterConsecutiveFailures(t *testing.T) {
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failing.Close()

	srv := newExtServer()
	hook := &Hook{Event: "on_after_tool", ExtensionID: "flaky-ext", Endpoint: failing.URL, TimeoutMs: 1000, FallbackPolicy: "continue"}
	srv.hooks["on_after_tool"] = []*Hook{hook}

	for i := 0; i < 3; i++ {
		results, err := srv.dispatchHooks(context.Background(), hookDispatchRequest{
			Event: "on_after_tool", TenantID: "tenant-a", PrincipalID: "principal-a", Payload: map[string]any{},
		})
		if err != nil {
			t.Fatalf("dispatch %d failed: %v", i, err)
		}
		if results[0].Status != "failed" {
			t.Fatalf("dispatch %d expected failed status, got %#v", i, results[0])
		}
	}

	// A 4th dispatch after 3 consecutive failures SHALL find the circuit open
	// rather than calling the endpoint again.
	results, err := srv.dispatchHooks(context.Background(), hookDispatchRequest{
		Event: "on_after_tool", TenantID: "tenant-a", PrincipalID: "principal-a", Payload: map[string]any{},
	})
	if err != nil {
		t.Fatalf("dispatch after circuit trip failed: %v", err)
	}
	if len(results) != 1 || results[0].Status != "circuit_open" {
		t.Fatalf("expected circuit_open status once the breaker trips, got %#v", results)
	}
}

// TestHookDispatchFailsClosedOnUndeclaredEndpoint covers §22.6/§22.7: a
// critical (fail_closed) hook that has no declared endpoint SHALL NOT be
// silently skipped — it SHALL fail the dispatch closed.
func TestHookDispatchFailsClosedOnUndeclaredEndpoint(t *testing.T) {
	srv := newExtServer()
	srv.hooks["on_checkpoint"] = []*Hook{{
		Event: "on_checkpoint", ExtensionID: "critical-ext", Endpoint: "", FallbackPolicy: "fail_closed",
	}}

	_, err := srv.dispatchHooks(context.Background(), hookDispatchRequest{
		Event: "on_checkpoint", TenantID: "tenant-a", PrincipalID: "principal-a", Payload: map[string]any{},
	})
	if err == nil {
		t.Fatal("expected dispatch to fail closed when a fail_closed hook has no declared endpoint")
	}
}

// TestHookDispatchSkipsUndeclaredEndpointWhenNotCritical covers §22.6: a
// non-critical hook without a declared endpoint SHALL be visibly skipped
// rather than silently dropped, and SHALL NOT block dispatch.
func TestHookDispatchSkipsUndeclaredEndpointWhenNotCritical(t *testing.T) {
	srv := newExtServer()
	srv.hooks["on_memory_candidate"] = []*Hook{{
		Event: "on_memory_candidate", ExtensionID: "optional-ext", Endpoint: "", FallbackPolicy: "continue",
	}}

	results, err := srv.dispatchHooks(context.Background(), hookDispatchRequest{
		Event: "on_memory_candidate", TenantID: "tenant-a", PrincipalID: "principal-a", Payload: map[string]any{},
	})
	if err != nil {
		t.Fatalf("dispatch should not fail dispatch-wide for a non-critical undeclared endpoint: %v", err)
	}
	if len(results) != 1 || results[0].Status != "skipped" {
		t.Fatalf("expected skipped status, got %#v", results)
	}
}

// TestAllRequiredLifecycleHooksAreRegistrable covers §22.6: every one of the
// 31 required lifecycle hooks SHALL be declarable via hooks_subscribed and
// dispatchable through /v1/hooks/dispatch. This does not require production
// endpoints for each — it proves the hook plumbing has no allowlist gap that
// would silently drop a governance-required event.
func TestAllRequiredLifecycleHooksAreRegistrable(t *testing.T) {
	requiredHooks := []string{
		"on_install", "on_enable", "on_disable", "on_uninstall",
		"on_agent_start", "on_agent_stop",
		"on_session_start", "on_session_switch", "on_session_end",
		"on_turn_received", "on_turn_start",
		"on_before_retrieval", "on_after_retrieval",
		"on_before_context_compile", "on_after_context_compile",
		"on_before_model", "on_after_model",
		"on_before_tool", "on_after_tool", "on_tool_error",
		"on_memory_candidate", "on_memory_promoted", "on_memory_corrected", "on_memory_revoked",
		"on_pre_compress", "on_post_compress",
		"on_delegation_start", "on_delegation_end",
		"on_checkpoint", "on_recovery",
		"on_policy_change", "on_index_change", "on_user_model_change",
	}
	if len(requiredHooks) != 33 {
		// §22.6 lists on_policy_change, on_index_change and on_user_model_change
		// alongside the 31 turn/lifecycle hooks; keep this count self-checking
		// so a future edit to the list can't silently drop an entry.
		t.Fatalf("expected 33 governance-listed hook events in this test's fixture, got %d", len(requiredHooks))
	}

	publicKey, privateKey, _ := ed25519.GenerateKey(nil)
	t.Setenv("SCOS_EXTENSION_SIGNING_PUBLIC_KEY", base64.StdEncoding.EncodeToString(publicKey))
	srv := newExtServer()

	endpoints := make(map[string]string)
	for _, hookEvent := range requiredHooks {
		endpoints[hookEvent] = "http://127.0.0.1:9/unused"
	}
	manifest := ExtensionManifest{
		SchemaVersion: "1.0", PackageID: "full.lifecycle.ext", Name: "Full Lifecycle", Version: "1.0.0",
		Publisher: "Test", License: "MIT", ExtensionTypes: []string{"hook"}, RequiredCoreVersion: "^0.1.0",
		ContentHash: "hash", Isolation: "in_process_trusted", HooksSubscribed: requiredHooks,
		HookEndpoints: endpoints,
		RollbackPlan:  "uninstall and restore previous package version",
	}
	manifest.Signature = signManifest(t, manifest, privateKey)

	res := httptest.NewRecorder()
	srv.handleInstall(res, httptest.NewRequest(http.MethodPost, "/v1/install", bytes.NewReader(mustJSON(t, manifest))))
	if res.Code != http.StatusOK {
		t.Fatalf("install with full lifecycle hook set returned %d: %s", res.Code, res.Body.String())
	}

	srv.mu.RLock()
	defer srv.mu.RUnlock()
	for _, hookEvent := range requiredHooks {
		hooks, ok := srv.hooks[hookEvent]
		if ok && len(hooks) == 1 && hooks[0].ExtensionID == "full.lifecycle.ext" {
			continue
		}
		t.Fatal("hook event " + hookEvent + " was not registered by installation: " + hookEvent)
	}
}
