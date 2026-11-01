package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// An empty selector is not a wildcard. In particular, a caller that omitted
// its agent/profile/workspace/project must not inherit a canary by matching "".
func TestCanaryScopeRejectsBlankSelectorsWithoutStateChange(t *testing.T) {
	tests := []struct {
		name  string
		scope CanaryScope
	}{
		{"empty_agent", CanaryScope{AgentIDs: []string{""}}},
		{"whitespace_profile", CanaryScope{ProfileIDs: []string{" \t "}}},
		{"empty_workspace", CanaryScope{WorkspaceIDs: []string{""}}},
		{"empty_project", CanaryScope{ProjectIDs: []string{""}}},
		{"mixed_valid_and_blank", CanaryScope{AgentIDs: []string{"agent-allowed", ""}}},
		{"percentage_with_blank", CanaryScope{Percentage: 20, ProjectIDs: []string{""}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, key := canaryFixture(t)
			before, err := os.ReadFile(srv.storePath)
			if err != nil {
				t.Fatal(err)
			}
			res := startCanary(t, srv, canaryManifest(t, key, "2.0.0"), tc.scope)
			if res.Code != http.StatusBadRequest {
				t.Fatalf("blank selector accepted: %d %s", res.Code, res.Body.String())
			}
			if len(srv.canaries) != 0 {
				t.Fatalf("rejected request mutated canaries: %+v", srv.canaries)
			}
			after, err := os.ReadFile(srv.storePath)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Fatal("rejected request mutated persisted state")
			}
		})
	}
}

func TestCanaryPercentageDoesNotSelectUnidentifiedCaller(t *testing.T) {
	srv, key := canaryFixture(t)
	if res := startCanary(t, srv, canaryManifest(t, key, "2.0.0"), CanaryScope{Percentage: 100}); res.Code != http.StatusCreated {
		t.Fatalf("canary start: %d %s", res.Code, res.Body.String())
	}
	if got := resolve(t, srv, CanaryContext{}); got.Channel != "stable" {
		t.Fatalf("anonymous caller selected by percentage rollout: %+v", got)
	}
	if got := resolve(t, srv, CanaryContext{AgentID: "  \t "}); got.Channel != "stable" {
		t.Fatalf("whitespace-only caller selected by percentage rollout: %+v", got)
	}
	if got := resolve(t, srv, CanaryContext{AgentID: "known"}); got.Channel != "canary" {
		t.Fatalf("identified caller not selected by 100%% rollout: %+v", got)
	}
}

// A persisted canary scope is unsigned. Invalid selector state must fail
// closed during load even when the candidate manifest itself is signed.
func TestCanaryPersistedBlankSelectorFailsClosed(t *testing.T) {
	srv, key := canaryFixture(t)
	if res := startCanary(t, srv, canaryManifest(t, key, "2.0.0"), CanaryScope{AgentIDs: []string{"allowed"}}); res.Code != http.StatusCreated {
		t.Fatalf("canary start: %d %s", res.Code, res.Body.String())
	}
	raw, err := os.ReadFile(srv.storePath)
	if err != nil {
		t.Fatal(err)
	}
	var state persistedExtensionState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	state.Canaries["canary.ext"].Scope.AgentIDs = []string{""}
	raw, err = json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(srv.storePath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	restored := newExtServerAt(srv.storePath)
	if got := resolve(t, restored, CanaryContext{}); got.Channel != "stable" {
		t.Fatalf("blank persisted selector activated canary: %+v", got)
	}
	if got := restored.canaries["canary.ext"]; got.Status != "rolled_back" || !strings.Contains(got.StatusReason, "scope") {
		t.Fatalf("tampered canary not disabled: %+v", got)
	}
}

// Real HTTP delivery must keep an unidentified event on the stable endpoint
// even when a 100% percentage canary exists.
func TestCanaryAnonymousDispatchStaysOnStableEndpoint(t *testing.T) {
	key := withSigningKey(t)
	srv := newExtServer()
	stable, candidate := newCountingEndpoint(t), newCountingEndpoint(t)
	if res := installForTest(t, srv, sealManifest(t, hookManifest("ext", "1.0.0", stable.URL), key)); res.Code != http.StatusOK {
		t.Fatalf("stable install: %d %s", res.Code, res.Body.String())
	}
	body := mustJSON(t, map[string]any{
		"manifest": sealManifest(t, hookManifest("ext", "2.0.0", candidate.URL), key),
		"scope":    CanaryScope{Percentage: 100},
	})
	res := httptest.NewRecorder()
	srv.handleExtensionByID(res, operatorRequest(http.MethodPost, "/v1/extensions/ext/canary", bytes.NewReader(body)))
	if res.Code != http.StatusCreated {
		t.Fatalf("canary start: %d %s", res.Code, res.Body.String())
	}
	dispatch := func(payload string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/hooks/dispatch", strings.NewReader(payload))
		req.Header.Set("X-SCOS-Tenant-ID", "tenant-a")
		req.Header.Set("X-SCOS-Principal-ID", "principal-a")
		out := httptest.NewRecorder()
		srv.handleHookDispatch(out, req)
		if out.Code != http.StatusOK {
			t.Fatalf("dispatch: %d %s", out.Code, out.Body.String())
		}
	}
	dispatch(`{"event":"on_turn_start"}`)
	if stable.count() != 1 || candidate.count() != 0 {
		t.Fatalf("anonymous event routed to candidate: stable=%d candidate=%d", stable.count(), candidate.count())
	}
	dispatch(`{"event":"on_turn_start","agent_id":"known"}`)
	if stable.count() != 1 || candidate.count() != 1 {
		t.Fatalf("identified event not routed to candidate: stable=%d candidate=%d", stable.count(), candidate.count())
	}
}
