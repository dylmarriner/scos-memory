package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// The manifest is signed, but the separately persisted hook registry is not.
// A corrupted registry must never redirect a signed mandatory hook or silently
// downgrade it to an optional one after a restart.
func TestStoredHookRegistryIsRebuiltFromVerifiedManifest(t *testing.T) {
	var expected, redirected atomic.Int32
	trusted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		expected.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer trusted.Close()
	untrusted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirected.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer untrusted.Close()

	store := filepath.Join(t.TempDir(), "extensions.json")
	srv := newExtServerAt(store)
	manifest := sealManifest(t, ExtensionManifest{
		SchemaVersion: "1.0", PackageID: "signed-hook", Name: "Signed Hook", Version: "1.0.0",
		Publisher: "Test", License: "MIT", ExtensionTypes: []string{"hook"},
		RequiredCoreVersion: "^0.1.0", Isolation: isolationRemoteEndpoint,
		HooksSubscribed:      []string{"on_checkpoint"},
		HookEndpoints:        map[string]string{"on_checkpoint": trusted.URL},
		HookFallbackPolicies: map[string]string{"on_checkpoint": "fail_closed"},
		RollbackPlan:         "uninstall and restore previous package version",
	}, withSigningKey(t))
	install := httptest.NewRecorder()
	srv.handleInstall(install, operatorRequest(http.MethodPost, "/v1/install", bytes.NewReader(mustJSON(t, manifest))))
	if install.Code != http.StatusOK {
		t.Fatalf("install status %d: %s", install.Code, install.Body.String())
	}

	raw, err := os.ReadFile(store)
	if err != nil {
		t.Fatal(err)
	}
	var state persistedExtensionState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	state.Hooks["on_checkpoint"][0].Endpoint = untrusted.URL
	state.Hooks["on_checkpoint"][0].FallbackPolicy = "continue"
	state.Hooks["on_checkpoint"][0].Type = "synchronous"
	state.Hooks["on_checkpoint"][0].ConsecutiveFailures = 2
	// A duplicate registry entry must not cause duplicate external delivery.
	duplicate := *state.Hooks["on_checkpoint"][0]
	state.Hooks["on_checkpoint"] = append(state.Hooks["on_checkpoint"], &duplicate)
	state.Hooks["on_before_tool"] = []*Hook{{Event: "on_before_tool", ExtensionID: "signed-hook", Endpoint: untrusted.URL, FallbackPolicy: "continue"}}
	tampered, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store, tampered, 0600); err != nil {
		t.Fatal(err)
	}

	restored := newExtServerAt(store)
	hooks := restored.hooks["on_checkpoint"]
	if len(hooks) != 1 || hooks[0].Endpoint != trusted.URL || hooks[0].FallbackPolicy != "fail_closed" || hooks[0].Type != "blocking" || hooks[0].ConsecutiveFailures != 2 {
		t.Fatalf("persisted hook registry overrode signed manifest: %+v", hooks)
	}
	if len(restored.hooks["on_before_tool"]) != 0 {
		t.Fatal("injected unsubscribed hook survived restart")
	}
	dispatch := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/hooks/dispatch", bytes.NewBufferString(`{"event":"on_checkpoint"}`))
	req.Header.Set("X-SCOS-Tenant-ID", "tenant-a")
	req.Header.Set("X-SCOS-Principal-ID", "principal-a")
	restored.handleHookDispatch(dispatch, req)
	if dispatch.Code != http.StatusOK || expected.Load() != 1 || redirected.Load() != 0 {
		t.Fatalf("dispatch=%d trusted=%d untrusted=%d body=%s", dispatch.Code, expected.Load(), redirected.Load(), dispatch.Body.String())
	}
}

func TestStoredMissingMandatoryHookIsRestored(t *testing.T) {
	store := filepath.Join(t.TempDir(), "extensions.json")
	srv := newExtServerAt(store)
	manifest := sealManifest(t, ExtensionManifest{
		SchemaVersion: "1.0", PackageID: "mandatory", Name: "Mandatory", Version: "1.0.0",
		Publisher: "Test", License: "MIT", ExtensionTypes: []string{"hook"},
		RequiredCoreVersion: "^0.1.0", Isolation: isolationRemoteEndpoint,
		HooksSubscribed:      []string{"on_checkpoint"},
		HookEndpoints:        map[string]string{"on_checkpoint": "http://127.0.0.1:9999/hook"},
		HookFallbackPolicies: map[string]string{"on_checkpoint": "fail_closed"},
		RollbackPlan:         "uninstall and restore previous package version",
	}, withSigningKey(t))
	install := httptest.NewRecorder()
	srv.handleInstall(install, operatorRequest(http.MethodPost, "/v1/install", bytes.NewReader(mustJSON(t, manifest))))
	if install.Code != http.StatusOK {
		t.Fatalf("install status %d: %s", install.Code, install.Body.String())
	}
	raw, err := os.ReadFile(store)
	if err != nil {
		t.Fatal(err)
	}
	var state persistedExtensionState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	delete(state.Hooks, "on_checkpoint")
	raw, err = json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store, raw, 0600); err != nil {
		t.Fatal(err)
	}
	restored := newExtServerAt(store)
	hooks := restored.hooks["on_checkpoint"]
	if len(hooks) != 1 || hooks[0].FallbackPolicy != "fail_closed" {
		t.Fatalf("missing mandatory hook silently bypassed after restart: %+v", hooks)
	}
}

func TestStoredManifestCannotBeRewrittenWithOnlyARecomputedHash(t *testing.T) {
	store := filepath.Join(t.TempDir(), "extensions.json")
	srv := newExtServerAt(store)
	manifest := sealManifest(t, ExtensionManifest{
		SchemaVersion: "1.0", PackageID: "signed-hook", Name: "Signed Hook", Version: "1.0.0",
		Publisher: "Test", License: "MIT", ExtensionTypes: []string{"hook"},
		RequiredCoreVersion: "^0.1.0", Isolation: isolationRemoteEndpoint,
		HooksSubscribed:      []string{"on_checkpoint"},
		HookEndpoints:        map[string]string{"on_checkpoint": "http://127.0.0.1:9999/hook"},
		HookFallbackPolicies: map[string]string{"on_checkpoint": "fail_closed"},
		RollbackPlan:         "uninstall and restore previous package version",
	}, withSigningKey(t))
	install := httptest.NewRecorder()
	srv.handleInstall(install, operatorRequest(http.MethodPost, "/v1/install", bytes.NewReader(mustJSON(t, manifest))))
	if install.Code != http.StatusOK {
		t.Fatalf("install status %d: %s", install.Code, install.Body.String())
	}
	raw, err := os.ReadFile(store)
	if err != nil {
		t.Fatal(err)
	}
	var state persistedExtensionState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	ext := state.Extensions["signed-hook"]
	ext.Manifest.HookEndpoints["on_checkpoint"] = "https://untrusted.example/hook"
	ext.Manifest.HookFallbackPolicies["on_checkpoint"] = "continue"
	hash, err := manifestContentHash(ext.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	ext.Manifest.ContentHash = hash // attacker can recompute hash, not Ed25519 signature
	raw, err = json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store, raw, 0600); err != nil {
		t.Fatal(err)
	}
	restored := newExtServerAt(store)
	if got := restored.extensions["signed-hook"].Status; got != "disabled" {
		t.Fatalf("tampered signed manifest remains active after restart: %s", got)
	}
}
