package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
)

// A persisted canary's unsigned hook cache must never supersede the signed
// candidate manifest after restart, including for events with no stable hook.
func TestStoredCanaryHooksCannotRedirectOrInjectEvents(t *testing.T) {
	srv, key := canaryFixture(t)
	var trustedCalls, divertedCalls atomic.Int32
	trusted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		trustedCalls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer trusted.Close()
	diverted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		divertedCalls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer diverted.Close()

	candidate := canaryManifest(t, key, "2.0.0")
	candidate.HooksSubscribed = []string{"on_checkpoint"}
	candidate.HookEndpoints = map[string]string{"on_checkpoint": trusted.URL}
	candidate.HookFallbackPolicies = map[string]string{"on_checkpoint": "fail_closed"}
	candidate = sealManifest(t, candidate, key)
	if res := startCanary(t, srv, candidate, CanaryScope{Organisation: true}); res.Code != http.StatusCreated {
		t.Fatalf("start canary: %d %s", res.Code, res.Body.String())
	}
	raw, err := os.ReadFile(srv.storePath)
	if err != nil {
		t.Fatal(err)
	}
	var state persistedExtensionState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	deployment := state.Canaries["canary.ext"]
	deployment.Hooks["on_checkpoint"].Endpoint = diverted.URL
	deployment.Hooks["on_checkpoint"].FallbackPolicy = "continue"
	deployment.Hooks["on_checkpoint"].ConsecutiveFailures = 2
	deployment.Hooks["on_before_tool"] = &Hook{Event: "on_before_tool", ExtensionID: "canary.ext", Endpoint: diverted.URL, FallbackPolicy: "continue"}
	raw, err = json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(srv.storePath, raw, 0600); err != nil {
		t.Fatal(err)
	}

	restored := newExtServerAt(srv.storePath)
	hooks := restored.canaries["canary.ext"].Hooks
	if len(hooks) != 1 || hooks["on_checkpoint"] == nil || hooks["on_checkpoint"].Endpoint != trusted.URL || hooks["on_checkpoint"].FallbackPolicy != "fail_closed" || hooks["on_checkpoint"].ConsecutiveFailures != 2 {
		t.Fatalf("unsigned canary hook cache overrode signed candidate: %+v", hooks)
	}
	for _, event := range []string{"on_checkpoint", "on_before_tool"} {
		res := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/hooks/dispatch", bytes.NewReader(mustJSON(t, map[string]string{"event": event})))
		req.Header.Set("X-SCOS-Tenant-ID", "tenant-a")
		req.Header.Set("X-SCOS-Principal-ID", "principal-a")
		restored.handleHookDispatch(res, req)
		if res.Code != http.StatusOK {
			t.Fatalf("dispatch %s: %d %s", event, res.Code, res.Body.String())
		}
	}
	if trustedCalls.Load() != 1 || divertedCalls.Load() != 0 {
		t.Fatalf("trusted=%d diverted=%d", trustedCalls.Load(), divertedCalls.Load())
	}
}

// A candidate manifest with a recomputed hash but an invalid signature must
// not be selected or dispatched after restoring local persisted state.
func TestStoredCanaryTamperedManifestFailsClosed(t *testing.T) {
	srv, key := canaryFixture(t)
	candidate := canaryManifest(t, key, "2.0.0")
	if res := startCanary(t, srv, candidate, CanaryScope{Organisation: true}); res.Code != http.StatusCreated {
		t.Fatalf("start canary: %d %s", res.Code, res.Body.String())
	}
	raw, err := os.ReadFile(srv.storePath)
	if err != nil {
		t.Fatal(err)
	}
	var state persistedExtensionState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	deployment := state.Canaries["canary.ext"]
	deployment.CanaryManifest.Name = "tampered candidate"
	hash, err := manifestContentHash(deployment.CanaryManifest)
	if err != nil {
		t.Fatal(err)
	}
	deployment.CanaryManifest.ContentHash = hash
	raw, err = json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(srv.storePath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	restored := newExtServerAt(srv.storePath)
	if got := resolve(t, restored, CanaryContext{AgentID: "anyone"}); got.Channel != "stable" || got.Version != "1.0.0" {
		t.Fatalf("unsigned tampered candidate selected after restart: %+v", got)
	}
}

// A missing persisted candidate subscription must be reconstructed from the
// verified manifest; mutable deployment metadata cannot switch the candidate.
func TestStoredCanaryMissingHookAndVersionBinding(t *testing.T) {
	for _, tc := range []struct {
		name          string
		tamperVersion bool
	}{
		{name: "missing_hook"}, {name: "version_mismatch", tamperVersion: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, key := canaryFixture(t)
			candidate := canaryManifest(t, key, "2.0.0")
			candidate.HooksSubscribed = []string{"on_checkpoint"}
			candidate.HookEndpoints = map[string]string{"on_checkpoint": "http://127.0.0.1:9999/hook"}
			candidate.HookFallbackPolicies = map[string]string{"on_checkpoint": "fail_closed"}
			candidate = sealManifest(t, candidate, key)
			if res := startCanary(t, srv, candidate, CanaryScope{Organisation: true}); res.Code != http.StatusCreated {
				t.Fatalf("start canary: %d %s", res.Code, res.Body.String())
			}
			raw, err := os.ReadFile(srv.storePath)
			if err != nil {
				t.Fatal(err)
			}
			var state persistedExtensionState
			if err := json.Unmarshal(raw, &state); err != nil {
				t.Fatal(err)
			}
			d := state.Canaries["canary.ext"]
			if tc.tamperVersion {
				d.CanaryVersion = "9.9.9"
			} else {
				d.Hooks = nil
			}
			raw, err = json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(srv.storePath, raw, 0600); err != nil {
				t.Fatal(err)
			}
			restored := newExtServerAt(srv.storePath)
			if tc.tamperVersion {
				if got := resolve(t, restored, CanaryContext{AgentID: "anyone"}); got.Channel != "stable" {
					t.Fatalf("untrusted version selected: %+v", got)
				}
				if restored.canaries["canary.ext"].StatusReason == "" {
					t.Fatal("missing canary integrity reason")
				}
			} else if h := restored.canaries["canary.ext"].Hooks["on_checkpoint"]; h == nil || h.FallbackPolicy != "fail_closed" {
				t.Fatalf("missing mandatory canary hook was not restored: %+v", h)
			}
		})
	}
}
