package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func canaryScopeTestKey(t *testing.T) {
	t.Helper()
	t.Setenv("SCOS_CANARY_SCOPE_HMAC_KEY", base64.StdEncoding.EncodeToString([]byte("scos-test-canary-scope-hmac-key-32!")))
}

func rewriteCanaryState(t *testing.T, path string, change func(*CanaryDeployment)) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state persistedExtensionState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	change(state.Canaries["canary.ext"])
	raw, err = json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestCanaryPersistedValidLookingScopeWideningFailsClosed(t *testing.T) {
	canaryScopeTestKey(t)
	srv, key := canaryFixture(t)
	if res := startCanary(t, srv, canaryManifest(t, key, "2.0.0"), CanaryScope{AgentIDs: []string{"allowed"}}); res.Code != http.StatusCreated {
		t.Fatalf("start canary: %d %s", res.Code, res.Body.String())
	}
	if got := resolve(t, newExtServerAt(srv.storePath), CanaryContext{AgentID: "allowed"}); got.Channel != "canary" {
		t.Fatalf("untampered restart did not restore active canary: %+v", got)
	}
	rewriteCanaryState(t, srv.storePath, func(d *CanaryDeployment) { d.Scope = CanaryScope{Organisation: true} })
	restored := newExtServerAt(srv.storePath)
	if got := resolve(t, restored, CanaryContext{AgentID: "unapproved"}); got.Channel != "stable" {
		t.Fatalf("tampered persisted scope activated unapproved caller: %+v", got)
	}
	if d := restored.canaries["canary.ext"]; d.Status != "rolled_back" || !strings.Contains(d.StatusReason, "scope integrity") {
		t.Fatalf("tampered canary not disabled with scope-integrity evidence: %+v", d)
	}
}

func TestCanaryStartRequiresIndependentScopeAuthenticationKey(t *testing.T) {
	t.Setenv("SCOS_CANARY_SCOPE_HMAC_KEY", "")
	srv, key := canaryFixture(t)
	before, err := os.ReadFile(srv.storePath)
	if err != nil {
		t.Fatal(err)
	}
	res := startCanary(t, srv, canaryManifest(t, key, "2.0.0"), CanaryScope{Organisation: true})
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("unkeyed canary activation accepted: %d %s", res.Code, res.Body.String())
	}
	if len(srv.canaries) != 0 {
		t.Fatal("unkeyed canary activation mutated in-memory state")
	}
	after, err := os.ReadFile(srv.storePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("unkeyed canary activation mutated durable state")
	}
}

func TestCanaryPersistedActivationReplayAfterRollbackFailsClosed(t *testing.T) {
	canaryScopeTestKey(t)
	srv, key := canaryFixture(t)
	if res := startCanary(t, srv, canaryManifest(t, key, "2.0.0"), CanaryScope{AgentIDs: []string{"allowed"}}); res.Code != http.StatusCreated {
		t.Fatalf("start canary: %d %s", res.Code, res.Body.String())
	}
	// Operator rollback changes the durable decision. A forged active status
	// must not revive an old cohort on restart.
	res := rollbackCanaryForScopeTest(srv)
	if res != http.StatusOK {
		t.Fatalf("rollback returned %d", res)
	}
	rewriteCanaryState(t, srv.storePath, func(d *CanaryDeployment) { d.Status = "active" })
	restored := newExtServerAt(srv.storePath)
	if got := resolve(t, restored, CanaryContext{AgentID: "allowed"}); got.Channel != "stable" {
		t.Fatalf("rolled-back canary was reactivated by state tampering: %+v", got)
	}
}

func rollbackCanaryForScopeTest(s *extServer) int {
	res := httptest.NewRecorder()
	s.handleExtensionByID(res, operatorRequest(http.MethodPost, "/v1/extensions/canary.ext/canary/rollback", nil))
	return res.Code
}

// Key loss or replacement cannot turn a previously authenticated cohort into
// an unverified active rollout after restart.
func TestCanaryPersistedScopeRejectsMissingOrRotatedKey(t *testing.T) {
	canaryScopeTestKey(t)
	srv, key := canaryFixture(t)
	if res := startCanary(t, srv, canaryManifest(t, key, "2.0.0"), CanaryScope{AgentIDs: []string{"allowed"}}); res.Code != http.StatusCreated {
		t.Fatalf("start canary: %d %s", res.Code, res.Body.String())
	}
	for _, tc := range []struct{ name, value string }{
		{"missing", ""},
		{"rotated", base64.StdEncoding.EncodeToString([]byte("a-different-operator-held-hmac-key-32"))},
		{"malformed", "not-base64"},
		{"short", base64.StdEncoding.EncodeToString([]byte("too-short"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SCOS_CANARY_SCOPE_HMAC_KEY", tc.value)
			restored := newExtServerAt(srv.storePath)
			if got := resolve(t, restored, CanaryContext{AgentID: "allowed"}); got.Channel != "stable" {
				t.Fatalf("canary selected without correct key: %+v", got)
			}
			if d := restored.canaries["canary.ext"]; d.Status != "rolled_back" {
				t.Fatalf("unverifiable canary not disabled: %+v", d)
			}
		})
	}
}

// An apparently valid percentage rollout must not replace a narrow approved
// cohort after persisted-state tampering, including at the hook side effect.
func TestCanaryPersistedScopeTamperingDoesNotDispatchCandidate(t *testing.T) {
	canaryScopeTestKey(t)
	key := withSigningKey(t)
	path := filepath.Join(t.TempDir(), "state.json")
	srv := newExtServerAt(path)
	var stableCalls, candidateCalls atomic.Int32
	stable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		stableCalls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer stable.Close()
	candidate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		candidateCalls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer candidate.Close()
	stableManifest := sealManifest(t, hookManifest("canary.ext", "1.0.0", stable.URL), key)
	if res := installForTest(t, srv, stableManifest); res.Code != http.StatusOK {
		t.Fatalf("stable install: %d %s", res.Code, res.Body.String())
	}
	canaryManifest := sealManifest(t, hookManifest("canary.ext", "2.0.0", candidate.URL), key)
	if res := startCanary(t, srv, canaryManifest, CanaryScope{AgentIDs: []string{"allowed"}}); res.Code != http.StatusCreated {
		t.Fatalf("canary start: %d %s", res.Code, res.Body.String())
	}
	rewriteCanaryState(t, path, func(d *CanaryDeployment) { d.Scope = CanaryScope{Percentage: 100} })
	restored := newExtServerAt(path)
	req := httptest.NewRequest(http.MethodPost, "/v1/hooks/dispatch", bytes.NewReader(mustJSON(t, map[string]any{
		"event": "on_turn_start", "agent_id": "unapproved",
	})))
	req.Header.Set("X-SCOS-Tenant-ID", "tenant-a")
	req.Header.Set("X-SCOS-Principal-ID", "principal-a")
	res := httptest.NewRecorder()
	restored.handleHookDispatch(res, req)
	if res.Code != http.StatusOK || stableCalls.Load() != 1 || candidateCalls.Load() != 0 {
		t.Fatalf("tampered cohort delivered to candidate: status=%d stable=%d candidate=%d body=%s",
			res.Code, stableCalls.Load(), candidateCalls.Load(), res.Body.String())
	}
}
