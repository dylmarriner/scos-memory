package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCanaryFailedPromotionRestoresStableRegistrations(t *testing.T) {
	srv, key := canaryFixture(t)
	var calls, stableCalls atomic.Int32
	stableReceiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		stableCalls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer stableReceiver.Close()
	stable := srv.extensions["canary.ext"].Manifest
	stable.HooksSubscribed = []string{"on_checkpoint"}
	stable.HookEndpoints = map[string]string{"on_checkpoint": stableReceiver.URL}
	stable.SkillsExported = []string{"stable-only"}
	stable = sealManifest(t, stable, key)
	srv.mu.Lock()
	srv.extensions["canary.ext"].Manifest = stable
	srv.registerExtensionLocked(stable)
	stableHook := srv.hooks["on_checkpoint"][0]
	stableHook.ConsecutiveFailures = 2
	srv.mu.Unlock()
	if err := srv.persist(context.Background()); err != nil {
		t.Fatal(err)
	}
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer receiver.Close()

	candidate := canaryManifest(t, key, "2.0.0")
	candidate.HooksSubscribed = []string{"on_checkpoint"}
	candidate.HookEndpoints = map[string]string{"on_checkpoint": receiver.URL}
	candidate.SkillsExported = []string{"candidate-only"}
	candidate = sealManifest(t, candidate, key)
	if res := startCanary(t, srv, candidate, CanaryScope{AgentIDs: []string{"agent-canary"}}); res.Code != http.StatusCreated {
		t.Fatalf("start canary: %d %s", res.Code, res.Body.String())
	}

	originalStore := srv.storePath
	srv.storePath = t.TempDir() // atomic rename to an existing directory fails
	failurePath := srv.storePath
	res := httptest.NewRecorder()
	srv.handleExtensionByID(res, operatorRequest(http.MethodPost, "/v1/extensions/canary.ext/canary/promote", nil))
	srv.storePath = originalStore
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected persistence failure, got %d: %s", res.Code, res.Body.String())
	}
	if got := srv.extensions["canary.ext"].Manifest.Version; got != "1.0.0" {
		t.Fatalf("stable manifest changed despite failed persist: %s", got)
	}
	if strings.Contains(res.Body.String(), failurePath) {
		t.Fatalf("storage path leaked in response: %s", res.Body.String())
	}
	if hooks := srv.hooks["on_checkpoint"]; len(hooks) != 1 || hooks[0] != stableHook ||
		hooks[0].Endpoint != stableReceiver.URL || hooks[0].ConsecutiveFailures != 2 {
		t.Fatalf("stable hook order, identity or health was not restored: %+v", hooks)
	}
	if _, ok := srv.skills["canary.ext/stable-only"]; !ok {
		t.Fatal("stable skill was not restored")
	}
	if _, ok := srv.skills["canary.ext/candidate-only"]; ok {
		t.Fatal("candidate skill leaked into stable registrations")
	}
	if got := resolve(t, srv, CanaryContext{AgentID: "agent-stable"}); got.Channel != "stable" {
		t.Fatalf("stable caller was switched: %+v", got)
	}
	if got := resolve(t, srv, CanaryContext{AgentID: "agent-canary"}); got.Channel != "canary" {
		t.Fatalf("active canary was lost: %+v", got)
	}

	for _, agent := range []string{"agent-stable", "agent-canary"} {
		req := httptest.NewRequest(http.MethodPost, "/v1/hooks/dispatch",
			bytes.NewReader(mustJSON(t, map[string]string{"event": "on_checkpoint", "agent_id": agent})))
		req.Header.Set("X-SCOS-Tenant-ID", "tenant-a")
		req.Header.Set("X-SCOS-Principal-ID", "principal-a")
		dispatch := httptest.NewRecorder()
		srv.handleHookDispatch(dispatch, req)
		if dispatch.Code != http.StatusOK {
			t.Fatalf("dispatch for %s: %d %s", agent, dispatch.Code, dispatch.Body.String())
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected only canary agent to reach candidate endpoint, got %d deliveries", got)
	}
	if got := stableCalls.Load(); got != 1 {
		t.Fatalf("expected stable agent to reach stable endpoint once, got %d deliveries", got)
	}
}

func TestCanaryFailedDecisionRestoresOriginalMetadata(t *testing.T) {
	for _, action := range []string{"promote", "rollback"} {
		t.Run(action, func(t *testing.T) {
			srv, key := canaryFixture(t)
			if res := startCanary(t, srv, canaryManifest(t, key, "2.0.0"),
				CanaryScope{Organisation: true}); res.Code != http.StatusCreated {
				t.Fatalf("start canary: %d %s", res.Code, res.Body.String())
			}
			before := "2020-01-01T00:00:00Z"
			srv.canaries["canary.ext"].UpdatedAt = before
			srv.storePath = t.TempDir()
			res := httptest.NewRecorder()
			srv.handleExtensionByID(res, operatorRequest(http.MethodPost,
				"/v1/extensions/canary.ext/canary/"+action, nil))
			if res.Code != http.StatusServiceUnavailable {
				t.Fatalf("expected 503, got %d: %s", res.Code, res.Body.String())
			}
			got := srv.canaries["canary.ext"]
			if got.Status != "active" || got.UpdatedAt != before {
				t.Fatalf("failed %s changed canary metadata: %+v", action, got)
			}
		})
	}
}

func TestCanaryFailedReplacementPreservesPreviousDecision(t *testing.T) {
	srv, key := canaryFixture(t)
	if res := startCanary(t, srv, canaryManifest(t, key, "2.0.0"),
		CanaryScope{Organisation: true}); res.Code != http.StatusCreated {
		t.Fatalf("start canary: %d %s", res.Code, res.Body.String())
	}
	res := httptest.NewRecorder()
	srv.handleExtensionByID(res, operatorRequest(http.MethodPost, "/v1/extensions/canary.ext/canary/rollback", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("rollback: %d %s", res.Code, res.Body.String())
	}
	srv.storePath = t.TempDir()
	if res := startCanary(t, srv, canaryManifest(t, key, "3.0.0"),
		CanaryScope{Organisation: true}); res.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", res.Code, res.Body.String())
	}
	prior := srv.canaries["canary.ext"]
	if prior == nil || prior.Status != "rolled_back" || prior.CanaryVersion != "2.0.0" {
		t.Fatalf("failed replacement discarded previous decision: %+v", prior)
	}
}
