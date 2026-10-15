package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

// countingEndpoint is a hook endpoint that counts deliveries.
type countingEndpoint struct {
	*httptest.Server
	mu    sync.Mutex
	calls int
}

func newCountingEndpoint(t *testing.T) *countingEndpoint {
	t.Helper()
	c := &countingEndpoint{}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.calls++
		c.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(c.Close)
	return c
}

func (c *countingEndpoint) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func hookManifest(id, version, endpoint string) ExtensionManifest {
	m := validManifest(id)
	m.Version = version
	m.HooksSubscribed = []string{"on_turn_start"}
	m.HookEndpoints = map[string]string{"on_turn_start": endpoint}
	m.SkillsExported = []string{"summarise"}
	return m
}

func installForTest(t *testing.T, srv *extServer, m ExtensionManifest) *httptest.ResponseRecorder {
	t.Helper()
	res := httptest.NewRecorder()
	srv.handleInstall(res, httptest.NewRequest(http.MethodPost, "/v1/install", bytes.NewReader(mustJSON(t, m))))
	return res
}

func dispatchForTest(t *testing.T, srv *extServer, req hookDispatchRequest) []hookDispatchResult {
	t.Helper()
	results, err := srv.dispatchHooks(context.Background(), req)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	return results
}

func TestInstallVerifiesContentHash(t *testing.T) {
	key := withSigningKey(t)
	srv := newExtServer()

	// A hash that does not match the content is refused even when the
	// publisher signed it.
	m := validManifest("hashed")
	m.ContentHash = "sha256:" + strings.Repeat("0", 64)
	m.Signature = signManifest(t, m, key)
	if res := installForTest(t, srv, m); res.Code != http.StatusForbidden || !strings.Contains(res.Body.String(), "content_hash") {
		t.Fatalf("mismatched content hash install = %d %s", res.Code, res.Body.String())
	}

	// Content changed after sealing: hash no longer matches.
	sealed := sealManifest(t, validManifest("hashed"), key)
	sealed.Permissions = []string{"filesystem.root"}
	sealed.Signature = signManifest(t, sealed, key)
	if res := installForTest(t, srv, sealed); res.Code != http.StatusForbidden {
		t.Fatalf("content altered after hashing install = %d", res.Code)
	}

	if res := installForTest(t, srv, sealManifest(t, validManifest("hashed"), key)); res.Code != http.StatusOK {
		t.Fatalf("correctly sealed install = %d %s", res.Code, res.Body.String())
	}
}

func TestEnableReverifiesStoredManifest(t *testing.T) {
	key := withSigningKey(t)
	srv := newExtServer()
	endpoint := newCountingEndpoint(t)
	if res := installForTest(t, srv, sealManifest(t, hookManifest("ext", "1.0.0", endpoint.URL), key)); res.Code != http.StatusOK {
		t.Fatalf("install = %d %s", res.Code, res.Body.String())
	}
	res := httptest.NewRecorder()
	srv.handleExtensionByID(res, httptest.NewRequest(http.MethodPost, "/v1/extensions/ext/disable", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("disable = %d", res.Code)
	}
	// Stored state is tampered with while disabled.
	srv.extensions["ext"].Manifest.HookEndpoints["on_turn_start"] = "https://attacker.example/hook"
	res = httptest.NewRecorder()
	srv.handleExtensionByID(res, httptest.NewRequest(http.MethodPost, "/v1/extensions/ext/enable", nil))
	if res.Code != http.StatusConflict || srv.extensions["ext"].Status != "disabled" {
		t.Fatalf("enable of tampered manifest = %d, status %s", res.Code, srv.extensions["ext"].Status)
	}
	// PATCH goes through the same gate and accepts only enabled/disabled.
	res = httptest.NewRecorder()
	srv.handleExtensionByID(res, httptest.NewRequest(http.MethodPatch, "/v1/extensions/ext", strings.NewReader(`{"status":"enabled"}`)))
	if res.Code != http.StatusConflict || srv.extensions["ext"].Status != "disabled" {
		t.Fatalf("PATCH enable of tampered manifest = %d", res.Code)
	}
	res = httptest.NewRecorder()
	srv.handleExtensionByID(res, httptest.NewRequest(http.MethodPatch, "/v1/extensions/ext", strings.NewReader(`{"status":"active"}`)))
	if res.Code != http.StatusBadRequest {
		t.Fatalf("PATCH arbitrary status = %d", res.Code)
	}
}

func TestEnableDisableGatesHookDispatchAndSkills(t *testing.T) {
	key := withSigningKey(t)
	srv := newExtServer()
	endpoint := newCountingEndpoint(t)
	if res := installForTest(t, srv, sealManifest(t, hookManifest("ext", "1.0.0", endpoint.URL), key)); res.Code != http.StatusOK {
		t.Fatalf("install = %d %s", res.Code, res.Body.String())
	}
	req := hookDispatchRequest{Event: "on_turn_start", TenantID: "t", PrincipalID: "p"}
	listSkills := func(query string) []Skill {
		res := httptest.NewRecorder()
		srv.handleSkills(res, httptest.NewRequest(http.MethodGet, "/v1/skills"+query, nil))
		var out []Skill
		if err := json.Unmarshal(res.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	action := func(verb string) {
		res := httptest.NewRecorder()
		srv.handleExtensionByID(res, httptest.NewRequest(http.MethodPost, "/v1/extensions/ext/"+verb, nil))
		if res.Code != http.StatusOK {
			t.Fatalf("%s = %d %s", verb, res.Code, res.Body.String())
		}
	}

	if got := dispatchForTest(t, srv, req); len(got) != 1 || got[0].Status != "succeeded" || endpoint.count() != 1 {
		t.Fatalf("enabled dispatch = %+v, calls %d", got, endpoint.count())
	}
	if skills := listSkills(""); len(skills) != 1 || skills[0].ID != "ext/summarise" || skills[0].Status != "active" {
		t.Fatalf("enabled skills = %+v", skills)
	}

	action("disable")
	if got := dispatchForTest(t, srv, req); len(got) != 0 || endpoint.count() != 1 {
		t.Fatalf("disabled extension's hook was delivered: %+v, calls %d", got, endpoint.count())
	}
	if skills := listSkills(""); len(skills) != 0 {
		t.Fatalf("disabled extension's skills listed: %+v", skills)
	}
	if skills := listSkills("?include_disabled=true"); len(skills) != 1 || skills[0].Status != "disabled" {
		t.Fatalf("include_disabled skills = %+v", skills)
	}
	res := httptest.NewRecorder()
	srv.handleSkillByID(res, httptest.NewRequest(http.MethodGet, "/v1/skills/ext/summarise", nil))
	var skill Skill
	_ = json.Unmarshal(res.Body.Bytes(), &skill)
	if res.Code != http.StatusOK || skill.Status != "disabled" {
		t.Fatalf("disabled skill by id = %d %+v", res.Code, skill)
	}
	res = httptest.NewRecorder()
	srv.handleHooks(res, httptest.NewRequest(http.MethodGet, "/v1/hooks", nil))
	var hooks []map[string]any
	_ = json.Unmarshal(res.Body.Bytes(), &hooks)
	if len(hooks) != 1 || hooks[0]["active"] != false || hooks[0]["type"] != "synchronous" || hooks[0]["retryable"] != false {
		t.Fatalf("hook listing = %+v", hooks)
	}

	action("enable")
	if got := dispatchForTest(t, srv, req); len(got) != 1 || endpoint.count() != 2 {
		t.Fatalf("re-enabled dispatch = %+v, calls %d", got, endpoint.count())
	}

	// An extension skill cannot be deleted on its own or forged by POST.
	res = httptest.NewRecorder()
	srv.handleSkillByID(res, httptest.NewRequest(http.MethodDelete, "/v1/skills/ext/summarise", nil))
	if res.Code != http.StatusConflict {
		t.Fatalf("delete extension skill = %d", res.Code)
	}
	res = httptest.NewRecorder()
	srv.handleSkills(res, httptest.NewRequest(http.MethodPost, "/v1/skills", strings.NewReader(`{"name":"x","extension_id":"ext"}`)))
	if res.Code != http.StatusBadRequest {
		t.Fatalf("forged extension skill = %d", res.Code)
	}
}

func TestHookSemanticsDescribeTheDispatcher(t *testing.T) {
	key := withSigningKey(t)
	srv := newExtServer()
	m := hookManifest("ext", "1.0.0", "https://ext.example/hook")
	m.HooksSubscribed = []string{"on_turn_start", "on_checkpoint"}
	m.HookEndpoints["on_checkpoint"] = "https://ext.example/checkpoint"
	m.HookFallbackPolicies = map[string]string{"on_checkpoint": "fail_closed"}
	if res := installForTest(t, srv, sealManifest(t, m, key)); res.Code != http.StatusOK {
		t.Fatalf("install = %d %s", res.Code, res.Body.String())
	}
	normal, blocking := srv.hooks["on_turn_start"][0], srv.hooks["on_checkpoint"][0]
	if normal.Type != "synchronous" || normal.Retryable || normal.TimeoutMs != hookTimeoutMs || normal.Version != "1.0.0" {
		t.Fatalf("continue hook = %+v", normal)
	}
	if blocking.Type != "blocking" || blocking.Retryable {
		t.Fatalf("fail_closed hook = %+v", blocking)
	}
}

// TestCanaryRoutesHookDispatchToItsCohort: callers in the canary scope are
// delivered the candidate version's hook, everyone else the stable one's.
func TestCanaryRoutesHookDispatchToItsCohort(t *testing.T) {
	key := withSigningKey(t)
	srv := newExtServer()
	stable, candidate := newCountingEndpoint(t), newCountingEndpoint(t)
	if res := installForTest(t, srv, sealManifest(t, hookManifest("ext", "1.0.0", stable.URL), key)); res.Code != http.StatusOK {
		t.Fatalf("install = %d %s", res.Code, res.Body.String())
	}
	body := mustJSON(t, map[string]any{
		"manifest": sealManifest(t, hookManifest("ext", "2.0.0", candidate.URL), key),
		"scope":    CanaryScope{AgentIDs: []string{"agent-in"}},
	})
	res := httptest.NewRecorder()
	srv.handleExtensionByID(res, httptest.NewRequest(http.MethodPost, "/v1/extensions/ext/canary", bytes.NewReader(body)))
	if res.Code != http.StatusCreated {
		t.Fatalf("start canary = %d %s", res.Code, res.Body.String())
	}

	dispatch := func(claims auth.Claims, body string) (int, []hookDispatchResult) {
		r := httptest.NewRequest(http.MethodPost, "/v1/hooks/dispatch", strings.NewReader(body))
		r = r.WithContext(auth.ContextWithClaims(r.Context(), claims))
		w := httptest.NewRecorder()
		srv.handleHookDispatch(w, r)
		var out struct {
			Results []hookDispatchResult `json:"results"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out.Results
	}
	code, got := dispatch(auth.Claims{TenantID: "t", PrincipalID: "p", AgentID: "agent-in"}, `{"event":"on_turn_start"}`)
	if code != http.StatusOK || len(got) != 1 || got[0].Channel != "canary" || got[0].Version != "2.0.0" || candidate.count() != 1 || stable.count() != 0 {
		t.Fatalf("in-cohort dispatch = %d %+v (stable %d, canary %d)", code, got, stable.count(), candidate.count())
	}
	code, got = dispatch(auth.Claims{TenantID: "t", PrincipalID: "p", AgentID: "agent-out"}, `{"event":"on_turn_start"}`)
	if code != http.StatusOK || len(got) != 1 || got[0].Channel != "stable" || got[0].Version != "1.0.0" || stable.count() != 1 || candidate.count() != 1 {
		t.Fatalf("out-of-cohort dispatch = %d %+v (stable %d, canary %d)", code, got, stable.count(), candidate.count())
	}
	// An agent cannot join a cohort by naming another agent.
	if code, _ := dispatch(auth.Claims{TenantID: "t", PrincipalID: "p", AgentID: "agent-out"}, `{"event":"on_turn_start","agent_id":"agent-in"}`); code != http.StatusForbidden {
		t.Fatalf("spoofed cohort dispatch = %d", code)
	}

	// Rolling the canary back returns every caller to stable.
	res = httptest.NewRecorder()
	srv.handleExtensionByID(res, httptest.NewRequest(http.MethodPost, "/v1/extensions/ext/canary/rollback", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("canary rollback = %d", res.Code)
	}
	if got := dispatchForTest(t, srv, hookDispatchRequest{Event: "on_turn_start", AgentID: "agent-in"}); len(got) != 1 || got[0].Channel != "stable" {
		t.Fatalf("dispatch after canary rollback = %+v", got)
	}
}

// TestLoadNormalizesLegacyState covers state written before these checks:
// skills left behind by an uninstalled extension are dropped, surviving
// extension skills record their owner, hooks report the dispatcher's real
// semantics, and an enabled extension whose stored manifest fails its
// content hash or isolation check is disabled with the reason recorded.
func TestLoadNormalizesLegacyState(t *testing.T) {
	key := withSigningKey(t)
	good := sealManifest(t, hookManifest("good", "1.0.0", "https://good.example/hook"), key)
	legacy := hookManifest("legacy", "1.0.0", "https://legacy.example/hook")
	legacy.Isolation = "separate_process"
	legacy.ContentHash = "hash"
	state := persistedExtensionState{
		Extensions: map[string]*Extension{
			"good":   {Manifest: good, Status: "enabled", Isolation: "separate_process"},
			"legacy": {Manifest: legacy, Status: "enabled", Isolation: "separate_process"},
		},
		Skills: map[string]*Skill{
			"good/summarise": {ID: "good/summarise", Name: "summarise", Status: "active"},
			"gone/summarise": {ID: "gone/summarise", Name: "summarise", Status: "active"},
			"skill-3":        {ID: "skill-3", Name: "manual", Status: "active"},
		},
		Hooks: map[string][]*Hook{"on_turn_start": {
			{Event: "on_turn_start", ExtensionID: "good", Type: "asynchronous", Retryable: true, TimeoutMs: 30000, Endpoint: "https://good.example/hook", FallbackPolicy: "continue"},
			{Event: "on_turn_start", ExtensionID: "gone", Type: "asynchronous", Retryable: true, Endpoint: "https://gone.example/hook", FallbackPolicy: "continue"},
		}},
	}
	path := filepath.Join(t.TempDir(), "extension-state.json")
	if err := os.WriteFile(path, mustJSON(t, state), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := newExtServerAt(path)
	if srv.skills["gone/summarise"] != nil || srv.skills["skill-3"] == nil || srv.skills["good/summarise"].ExtensionID != "good" {
		t.Fatalf("skills after load = %+v", srv.skills)
	}
	hooks := srv.hooks["on_turn_start"]
	if len(hooks) != 1 || hooks[0].ExtensionID != "good" || hooks[0].Type != "synchronous" || hooks[0].Retryable {
		t.Fatalf("hooks after load = %+v", hooks)
	}
	if ext := srv.extensions["good"]; ext.Status != "enabled" || ext.Isolation != isolationRemoteEndpoint {
		t.Fatalf("good extension after load = %+v", ext)
	}
	if ext := srv.extensions["legacy"]; ext.Status != "disabled" || !strings.Contains(ext.StatusReason, "content_hash") || ext.Isolation != isolationRemoteEndpoint {
		t.Fatalf("legacy extension after load = %+v", ext)
	}
}
