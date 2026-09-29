package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dylmarriner/scos-memory/packages/auth"
	hooksdk "github.com/dylmarriner/scos-memory/packages/hook-sdk"
	"github.com/dylmarriner/scos-memory/packages/telemetry"
)

// enableExtensions registers enabled extensions so hooks set directly on the
// server are deliverable.
func enableExtensions(srv *extServer, ids ...string) {
	for _, id := range ids {
		srv.extensions[id] = &Extension{Manifest: ExtensionManifest{PackageID: id}, Status: "enabled"}
	}
}

func hookDecl(typ, fallback string) HookDeclaration {
	yes := true
	return HookDeclaration{Type: typ, Retryable: &yes, Idempotent: &yes, TimeoutMs: 1000, FallbackPolicy: fallback}
}

// signedHookManifest is a valid, signed manifest subscribing every event to
// one endpoint with one declaration.
func signedHookManifest(t *testing.T, key ed25519.PrivateKey, id, version, endpoint string, d HookDeclaration, events ...string) ExtensionManifest {
	t.Helper()
	m := validManifest(id)
	m.Version = version
	m.Permissions = []string{"memory.read"}
	m.HooksSubscribed = events
	m.HookEndpoints = map[string]string{}
	m.HookDeclarations = map[string]HookDeclaration{}
	for _, event := range events {
		m.HookEndpoints[event] = endpoint
		m.HookDeclarations[event] = d
	}
	m.Signature = signManifest(t, m, key)
	return m
}

type recordedHookCall struct {
	Event, Tenant, Principal string
	Payload                  map[string]any
}

// hookRecorder is an httptest hook endpoint answering with status and
// recording every call it receives.
func hookRecorder(t *testing.T, status int) (*httptest.Server, func() []recordedHookCall) {
	t.Helper()
	var mu sync.Mutex
	var calls []recordedHookCall
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Event   string         `json:"event"`
			Payload map[string]any `json:"payload"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		calls = append(calls, recordedHookCall{body.Event, r.Header.Get("X-SCOS-Tenant-ID"), r.Header.Get("X-SCOS-Principal-ID"), body.Payload})
		mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(server.Close)
	return server, func() []recordedHookCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]recordedHookCall(nil), calls...)
	}
}

// asTenant attaches verified claims, as auth.AuthenticatedHandler does.
func asTenant(r *http.Request) *http.Request {
	return r.WithContext(auth.ContextWithClaims(r.Context(), auth.Claims{TenantID: "tenant-l", PrincipalID: "principal-l"}))
}

func installAs(t *testing.T, srv *extServer, m ExtensionManifest) *httptest.ResponseRecorder {
	t.Helper()
	res := httptest.NewRecorder()
	srv.handleInstall(res, asTenant(httptest.NewRequest(http.MethodPost, "/v1/install", bytes.NewReader(mustJSON(t, m)))))
	return res
}

func callAs(srv *extServer, method, path string, body []byte) *httptest.ResponseRecorder {
	res := httptest.NewRecorder()
	srv.handleExtensionByID(res, asTenant(httptest.NewRequest(method, path, bytes.NewReader(body))))
	return res
}

func decodeLifecycle(t *testing.T, res *httptest.ResponseRecorder) []lifecycleReport {
	t.Helper()
	var body struct {
		LifecycleHooks []lifecycleReport `json:"lifecycle_hooks"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", res.Body.String(), err)
	}
	return body.LifecycleHooks
}

func TestInstallValidatesHookDeclarations(t *testing.T) {
	srv, key := metricsFixture(t)
	good := hookDecl(hookTypeSynchronous, fallbackContinue)
	cases := map[string]func(m *ExtensionManifest){
		"unknown event": func(m *ExtensionManifest) {
			m.HooksSubscribed = []string{"turn.completed"}
			m.HookEndpoints = map[string]string{"turn.completed": "https://hooks.example/h"}
			m.HookDeclarations = map[string]HookDeclaration{"turn.completed": good}
		},
		"missing declaration": func(m *ExtensionManifest) { delete(m.HookDeclarations, hooksdk.OnTurnStart) },
		"invalid type": func(m *ExtensionManifest) {
			d := good
			d.Type = "eventually"
			m.HookDeclarations[hooksdk.OnTurnStart] = d
		},
		"missing retryable":  func(m *ExtensionManifest) { d := good; d.Retryable = nil; m.HookDeclarations[hooksdk.OnTurnStart] = d },
		"missing idempotent": func(m *ExtensionManifest) { d := good; d.Idempotent = nil; m.HookDeclarations[hooksdk.OnTurnStart] = d },
		"zero timeout":       func(m *ExtensionManifest) { d := good; d.TimeoutMs = 0; m.HookDeclarations[hooksdk.OnTurnStart] = d },
		"excessive timeout": func(m *ExtensionManifest) {
			d := good
			d.TimeoutMs = 60000
			m.HookDeclarations[hooksdk.OnTurnStart] = d
		},
		"invalid fallback": func(m *ExtensionManifest) {
			d := good
			d.FallbackPolicy = "maybe"
			m.HookDeclarations[hooksdk.OnTurnStart] = d
		},
		"async fail_closed": func(m *ExtensionManifest) {
			m.HookDeclarations[hooksdk.OnTurnStart] = hookDecl(hookTypeAsynchronous, fallbackFailClosed)
		},
		"duplicate subscription": func(m *ExtensionManifest) {
			m.HooksSubscribed = []string{hooksdk.OnTurnStart, hooksdk.OnTurnStart}
		},
		"declaration without subscription": func(m *ExtensionManifest) { m.HookDeclarations[hooksdk.OnRecovery] = good },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			m := signedHookManifest(t, key, "decl.ext", "1.0.0", "https://hooks.example/h", good, hooksdk.OnTurnStart)
			mutate(&m)
			m.Signature = signManifest(t, m, key)
			if res := install(t, srv, m); res.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", res.Code, res.Body.String())
			}
			if srv.extensions["decl.ext"] != nil {
				t.Fatal("rejected manifest was installed")
			}
		})
	}

	// The control plane's turn stages are accepted alongside §22.6.
	m := signedHookManifest(t, key, "stage.ext", "1.0.0", "https://hooks.example/h", good,
		string(hooksdk.StagePreTurn), string(hooksdk.StagePostTurn), hooksdk.OnUserModelChange)
	if res := install(t, srv, m); res.Code != http.StatusOK {
		t.Fatalf("control-plane stages rejected: %d %s", res.Code, res.Body.String())
	}
	if h := srv.hooks[string(hooksdk.StagePreTurn)][0]; h.Type != hookTypeSynchronous || !h.Retryable || !h.Idempotent || h.TimeoutMs != 1000 || h.FallbackPolicy != fallbackContinue {
		t.Fatalf("declared properties not registered: %#v", h)
	}
}

func TestInstallRaisesOnInstallWithRequestIdentity(t *testing.T) {
	collect := captureMetrics(t)
	srv, key := metricsFixture(t)
	endpoint, calls := hookRecorder(t, http.StatusNoContent)

	m := signedHookManifest(t, key, "self.ext", "1.2.0", endpoint.URL, hookDecl(hookTypeSynchronous, fallbackContinue), hooksdk.OnInstall)
	res := installAs(t, srv, m)
	if res.Code != http.StatusOK {
		t.Fatalf("install returned %d: %s", res.Code, res.Body.String())
	}
	got := calls()
	if len(got) != 1 {
		t.Fatalf("on_install calls = %#v", got)
	}
	c := got[0]
	if c.Event != hooksdk.OnInstall || c.Tenant != "tenant-l" || c.Principal != "principal-l" {
		t.Fatalf("hook call = %#v", c)
	}
	if c.Payload["extension_id"] != "self.ext" || c.Payload["version"] != "1.2.0" || c.Payload["status"] != "enabled" {
		t.Fatalf("payload = %#v", c.Payload)
	}
	if perms, _ := c.Payload["permissions"].([]any); len(perms) != 1 || perms[0] != "memory.read" {
		t.Fatalf("payload permissions = %#v", c.Payload["permissions"])
	}
	reports := decodeLifecycle(t, res)
	if len(reports) != 1 || reports[0].Event != hooksdk.OnInstall || reports[0].Results[0].Status != "succeeded" {
		t.Fatalf("lifecycle report = %#v", reports)
	}
	latency := pointsNamed(collect(), telemetry.MetricHookLatency)
	if len(latency) != 1 || latency[0].Labels["event"] != hooksdk.OnInstall || latency[0].Tenant != "tenant-l" {
		t.Fatalf("hook latency points = %#v", latency)
	}
}

func TestInstallRolledBackWhenCriticalOnInstallHookFails(t *testing.T) {
	collect := captureMetrics(t)
	srv, key := metricsFixture(t)
	failing, _ := hookRecorder(t, http.StatusInternalServerError)
	ok, _ := hookRecorder(t, http.StatusNoContent)

	// A guard extension vetoes installs through a critical hook. It is
	// installed while its endpoint still answers.
	guard := signedHookManifest(t, key, "guard.ext", "1.0.0", ok.URL, hookDecl(hookTypeSynchronous, fallbackFailClosed), hooksdk.OnInstall)
	if res := installAs(t, srv, guard); res.Code != http.StatusOK {
		t.Fatalf("install guard returned %d: %s", res.Code, res.Body.String())
	}
	srv.hooks[hooksdk.OnInstall][0].Endpoint = failing.URL
	receipts := len(srv.receipts)

	candidate := signedHookManifest(t, key, "cand.ext", "1.0.0", ok.URL, hookDecl(hookTypeSynchronous, fallbackContinue), hooksdk.OnTurnStart)
	res := installAs(t, srv, candidate)
	if res.Code != http.StatusFailedDependency {
		t.Fatalf("install returned %d, want 424: %s", res.Code, res.Body.String())
	}
	if srv.extensions["cand.ext"] != nil || len(srv.hooks[hooksdk.OnTurnStart]) != 0 || len(srv.receipts) != receipts {
		t.Fatalf("rejected install left state behind: ext=%v hooks=%v receipts=%d", srv.extensions["cand.ext"], srv.hooks[hooksdk.OnTurnStart], len(srv.receipts))
	}
	reports := decodeLifecycle(t, res)
	if len(reports) != 1 || reports[0].Error == "" || reports[0].Results[0].Reason != fallbackFailClosed {
		t.Fatalf("lifecycle report = %#v", reports)
	}
	// The rollback is durable, not just in memory.
	if restarted := newExtServerAt(srv.storePath); restarted.extensions["cand.ext"] != nil {
		t.Fatal("rolled-back install survived restart")
	}

	// An upgrade vetoed the same way leaves the previous version in place.
	srv.hooks[hooksdk.OnInstall][0].Endpoint = ok.URL
	srv.hooks[hooksdk.OnInstall][0].ConsecutiveFailures, srv.hooks[hooksdk.OnInstall][0].CircuitOpenUntil = 0, ""
	if res := installAs(t, srv, candidate); res.Code != http.StatusOK {
		t.Fatalf("install v1 returned %d: %s", res.Code, res.Body.String())
	}
	srv.hooks[hooksdk.OnInstall][0].Endpoint = failing.URL
	upgrade := signedHookManifest(t, key, "cand.ext", "2.0.0", ok.URL, hookDecl(hookTypeSynchronous, fallbackContinue), hooksdk.OnRecovery)
	if res := installAs(t, srv, upgrade); res.Code != http.StatusFailedDependency {
		t.Fatalf("upgrade returned %d, want 424", res.Code)
	}
	if v := srv.extensions["cand.ext"].Manifest.Version; v != "1.0.0" || len(srv.hooks[hooksdk.OnTurnStart]) != 1 || len(srv.hooks[hooksdk.OnRecovery]) != 0 || len(srv.history["cand.ext"]) != 0 {
		t.Fatalf("vetoed upgrade not rolled back: version=%s hooks=%v history=%v", v, srv.hooks, srv.history["cand.ext"])
	}

	failures := pointsNamed(collect(), telemetry.MetricHookFailure)
	if len(failures) == 0 || failures[0].Labels["event"] != hooksdk.OnInstall || failures[0].Labels["reason"] != "dispatch_error" {
		t.Fatalf("hook failure points = %#v", failures)
	}
}

func TestInstallReportsNonCriticalHookFailure(t *testing.T) {
	srv, key := metricsFixture(t)
	failing, calls := hookRecorder(t, http.StatusBadGateway)
	m := signedHookManifest(t, key, "soft.ext", "1.0.0", failing.URL, hookDecl(hookTypeSynchronous, fallbackContinue), hooksdk.OnInstall)
	res := installAs(t, srv, m)
	if res.Code != http.StatusOK || srv.extensions["soft.ext"] == nil || len(calls()) != 1 {
		t.Fatalf("install returned %d: %s", res.Code, res.Body.String())
	}
	if r := decodeLifecycle(t, res); r[0].Results[0].Status != "failed" || r[0].Error != "" {
		t.Fatalf("lifecycle report = %#v", r)
	}
}

func TestEnableDisableAndPatchRaiseLifecycleHooks(t *testing.T) {
	srv, key := metricsFixture(t)
	endpoint, calls := hookRecorder(t, http.StatusNoContent)
	observer := signedHookManifest(t, key, "observer.ext", "1.0.0", endpoint.URL, hookDecl(hookTypeSynchronous, fallbackContinue), hooksdk.OnEnable, hooksdk.OnDisable)
	if res := installAs(t, srv, observer); res.Code != http.StatusOK {
		t.Fatalf("install observer: %d %s", res.Code, res.Body.String())
	}
	target := validManifest("target.ext")
	target.Signature = signManifest(t, target, key)
	if res := installAs(t, srv, target); res.Code != http.StatusOK {
		t.Fatalf("install target: %d", res.Code)
	}

	steps := []struct {
		method, path, body string
		wantEvent, status  string
	}{
		{http.MethodPost, "/v1/extensions/target.ext/disable", "", hooksdk.OnDisable, "disabled"},
		{http.MethodPost, "/v1/extensions/target.ext/disable", "", "", ""}, // already disabled: no transition
		{http.MethodPost, "/v1/extensions/target.ext/enable", "", hooksdk.OnEnable, "enabled"},
		{http.MethodPatch, "/v1/extensions/target.ext", `{"status":"disabled"}`, hooksdk.OnDisable, "disabled"},
		{http.MethodPatch, "/v1/extensions/target.ext", `{"status":"enabled"}`, hooksdk.OnEnable, "enabled"},
		{http.MethodPatch, "/v1/extensions/target.ext", `{"config":{"k":"v"}}`, "", ""}, // no status change
	}
	for i, step := range steps {
		before := len(calls())
		res := callAs(srv, step.method, step.path, []byte(step.body))
		if res.Code != http.StatusOK {
			t.Fatalf("step %d returned %d: %s", i, res.Code, res.Body.String())
		}
		got := calls()[before:]
		if step.wantEvent == "" {
			if len(got) != 0 {
				t.Fatalf("step %d raised %#v, want nothing", i, got)
			}
			continue
		}
		if len(got) != 1 || got[0].Event != step.wantEvent || got[0].Payload["extension_id"] != "target.ext" || got[0].Payload["status"] != step.status || got[0].Tenant != "tenant-l" {
			t.Fatalf("step %d calls = %#v", i, got)
		}
		if r := decodeLifecycle(t, res); len(r) != 1 || r[0].Event != step.wantEvent {
			t.Fatalf("step %d report = %#v", i, r)
		}
	}
}

func TestCriticalOnEnableHookVetoesEnable(t *testing.T) {
	srv, key := metricsFixture(t)
	ok, _ := hookRecorder(t, http.StatusNoContent)
	failing, _ := hookRecorder(t, http.StatusInternalServerError)
	guard := signedHookManifest(t, key, "guard.ext", "1.0.0", ok.URL, hookDecl(hookTypeBlocking, fallbackFailClosed), hooksdk.OnEnable)
	if res := installAs(t, srv, guard); res.Code != http.StatusOK {
		t.Fatalf("install guard: %d", res.Code)
	}
	target := validManifest("target.ext")
	target.Signature = signManifest(t, target, key)
	installAs(t, srv, target)
	if res := callAs(srv, http.MethodPost, "/v1/extensions/target.ext/disable", nil); res.Code != http.StatusOK {
		t.Fatalf("disable: %d", res.Code)
	}
	srv.hooks[hooksdk.OnEnable][0].Endpoint = failing.URL

	for _, req := range []struct{ method, path, body string }{
		{http.MethodPost, "/v1/extensions/target.ext/enable", ""},
		{http.MethodPatch, "/v1/extensions/target.ext", `{"status":"enabled"}`},
	} {
		res := callAs(srv, req.method, req.path, []byte(req.body))
		if res.Code != http.StatusFailedDependency {
			t.Fatalf("%s %s returned %d, want 424: %s", req.method, req.path, res.Code, res.Body.String())
		}
		if s := srv.extensions["target.ext"].Status; s != "disabled" {
			t.Fatalf("vetoed enable left status %q", s)
		}
	}
	if restarted := newExtServerAt(srv.storePath); restarted.extensions["target.ext"].Status != "disabled" {
		t.Fatal("vetoed enable was persisted")
	}
}

func TestCriticalHookCannotBlockDisableOrUninstall(t *testing.T) {
	srv, key := metricsFixture(t)
	failing, calls := hookRecorder(t, http.StatusInternalServerError)
	guard := signedHookManifest(t, key, "guard.ext", "1.0.0", failing.URL, hookDecl(hookTypeSynchronous, fallbackFailClosed), hooksdk.OnDisable, hooksdk.OnUninstall)
	// Its own critical on_install is not subscribed, so installing succeeds.
	if res := installAs(t, srv, guard); res.Code != http.StatusOK {
		t.Fatalf("install guard: %d %s", res.Code, res.Body.String())
	}
	target := validManifest("target.ext")
	target.Signature = signManifest(t, target, key)
	installAs(t, srv, target)

	res := callAs(srv, http.MethodPost, "/v1/extensions/target.ext/disable", nil)
	if res.Code != http.StatusOK || srv.extensions["target.ext"].Status != "disabled" {
		t.Fatalf("disable returned %d: %s", res.Code, res.Body.String())
	}
	if r := decodeLifecycle(t, res); r[0].Error == "" || r[0].Results[0].Status != "failed" {
		t.Fatalf("disable must report the hook failure: %#v", r)
	}
	res = callAs(srv, http.MethodDelete, "/v1/extensions/target.ext", nil)
	if res.Code != http.StatusOK || srv.extensions["target.ext"] != nil {
		t.Fatalf("uninstall returned %d: %s", res.Code, res.Body.String())
	}
	if r := decodeLifecycle(t, res); r[0].Event != hooksdk.OnUninstall || r[0].Error == "" {
		t.Fatalf("uninstall must report the hook failure: %#v", r)
	}
	if len(calls()) != 2 {
		t.Fatalf("guard hook calls = %#v", calls())
	}
}

func TestUninstallNotifiesDepartingExtensionAndKeepsOthersHooks(t *testing.T) {
	srv, key := metricsFixture(t)
	endpoint, calls := hookRecorder(t, http.StatusNoContent)
	decl := hookDecl(hookTypeSynchronous, fallbackContinue)
	for _, id := range []string{"leaving.ext", "staying.ext"} {
		m := signedHookManifest(t, key, id, "1.0.0", endpoint.URL, decl, hooksdk.OnUninstall, hooksdk.OnTurnStart)
		if res := installAs(t, srv, m); res.Code != http.StatusOK {
			t.Fatalf("install %s: %d", id, res.Code)
		}
	}
	res := callAs(srv, http.MethodDelete, "/v1/extensions/leaving.ext", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("uninstall returned %d: %s", res.Code, res.Body.String())
	}
	got := calls()
	if len(got) != 2 || got[0].Payload["status"] != "uninstalled" || got[0].Payload["extension_id"] != "leaving.ext" {
		t.Fatalf("on_uninstall calls = %#v", got)
	}
	r := decodeLifecycle(t, res)
	if len(r[0].Results) != 2 || r[0].Results[0].ExtensionID != "leaving.ext" || r[0].Results[1].ExtensionID != "staying.ext" {
		t.Fatalf("both the departing extension and the observer must be notified: %#v", r)
	}
	// Uninstall used to delete every extension's hooks for the departing
	// extension's events.
	for _, event := range []string{hooksdk.OnUninstall, hooksdk.OnTurnStart} {
		if hooks := srv.hooks[event]; len(hooks) != 1 || hooks[0].ExtensionID != "staying.ext" {
			t.Fatalf("%s hooks after uninstall = %#v", event, hooks)
		}
	}
}

func TestLifecycleHooksNeverInventIdentity(t *testing.T) {
	srv, key := metricsFixture(t)
	endpoint, calls := hookRecorder(t, http.StatusNoContent)
	observer := signedHookManifest(t, key, "observer.ext", "1.0.0", endpoint.URL, hookDecl(hookTypeSynchronous, fallbackContinue), hooksdk.OnInstall)
	if res := install(t, srv, observer); res.Code != http.StatusOK {
		t.Fatalf("install without identity: %d %s", res.Code, res.Body.String())
	}
	if len(calls()) != 0 {
		t.Fatalf("hook dispatched without a request identity: %#v", calls())
	}
	var body struct {
		LifecycleHooks []lifecycleReport `json:"lifecycle_hooks"`
	}
	res := install(t, srv, func() ExtensionManifest {
		m := validManifest("plain.ext")
		m.Signature = signManifest(t, m, key)
		return m
	}())
	_ = json.Unmarshal(res.Body.Bytes(), &body)
	if res.Code != http.StatusOK || body.LifecycleHooks[0].Results[0].Status != "skipped" {
		t.Fatalf("expected a visible skip, got %d %s", res.Code, res.Body.String())
	}

	// A critical hook cannot be skipped: the install is refused instead.
	srv.hooks[hooksdk.OnInstall][0].FallbackPolicy = fallbackFailClosed
	m := validManifest("refused.ext")
	m.Signature = signManifest(t, m, key)
	if res := install(t, srv, m); res.Code != http.StatusFailedDependency || srv.extensions["refused.ext"] != nil {
		t.Fatalf("critical hook without identity: %d %s", res.Code, res.Body.String())
	}
}

func TestCanaryPromoteAndRollbackRaiseLifecycleHooks(t *testing.T) {
	srv, key := canaryFixture(t)
	endpoint, calls := hookRecorder(t, http.StatusNoContent)
	observer := signedHookManifest(t, key, "observer.ext", "1.0.0", endpoint.URL, hookDecl(hookTypeSynchronous, fallbackContinue), hooksdk.OnEnable, hooksdk.OnDisable)
	if res := installAs(t, srv, observer); res.Code != http.StatusOK {
		t.Fatalf("install observer: %d", res.Code)
	}

	startCanary(t, srv, canaryManifest(t, key, "2.0.0"), CanaryScope{Organisation: true})
	if res := callAs(srv, http.MethodPost, "/v1/extensions/canary.ext/canary/rollback", nil); res.Code != http.StatusOK {
		t.Fatalf("canary rollback: %d %s", res.Code, res.Body.String())
	}
	startCanary(t, srv, canaryManifest(t, key, "3.0.0"), CanaryScope{Organisation: true})
	if res := callAs(srv, http.MethodPost, "/v1/extensions/canary.ext/canary/promote", nil); res.Code != http.StatusOK {
		t.Fatalf("canary promote: %d %s", res.Code, res.Body.String())
	}
	got := calls()
	if len(got) != 2 ||
		got[0].Event != hooksdk.OnDisable || got[0].Payload["version"] != "2.0.0" || got[0].Payload["status"] != "rolled_back" ||
		got[1].Event != hooksdk.OnEnable || got[1].Payload["version"] != "3.0.0" || got[1].Payload["status"] != "enabled" {
		t.Fatalf("canary lifecycle calls = %#v", got)
	}
}

func TestCriticalHookVetoesCanaryPromotion(t *testing.T) {
	srv, key := canaryFixture(t)
	ok, _ := hookRecorder(t, http.StatusNoContent)
	failing, _ := hookRecorder(t, http.StatusServiceUnavailable)
	guard := signedHookManifest(t, key, "guard.ext", "1.0.0", ok.URL, hookDecl(hookTypeSynchronous, fallbackFailClosed), hooksdk.OnEnable)
	if res := installAs(t, srv, guard); res.Code != http.StatusOK {
		t.Fatalf("install guard: %d", res.Code)
	}
	srv.hooks[hooksdk.OnEnable][0].Endpoint = failing.URL
	candidate := canaryManifest(t, key, "2.0.0")
	candidate.SkillsExported = []string{"new-skill"}
	candidate.Signature = signManifest(t, candidate, key)
	startCanary(t, srv, candidate, CanaryScope{Organisation: true})

	res := callAs(srv, http.MethodPost, "/v1/extensions/canary.ext/canary/promote", nil)
	if res.Code != http.StatusFailedDependency {
		t.Fatalf("promote returned %d, want 424: %s", res.Code, res.Body.String())
	}
	if v := srv.extensions["canary.ext"].Manifest.Version; v != "1.0.0" || srv.canaries["canary.ext"].Status != "active" ||
		len(srv.history["canary.ext"]) != 0 || srv.skills["canary.ext/new-skill"] != nil {
		t.Fatalf("vetoed promotion not reverted: version=%s canary=%s history=%v skills=%v", v, srv.canaries["canary.ext"].Status, srv.history["canary.ext"], srv.skills)
	}
}

// TestHookOrderStableAcrossReload covers §22.6 deterministic ordering: the
// dispatch order is a function of the registered set, not of install order,
// re-installs or restarts.
func TestHookOrderStableAcrossReload(t *testing.T) {
	srv, key := metricsFixture(t)
	endpoint, _ := hookRecorder(t, http.StatusNoContent)
	decl := hookDecl(hookTypeSynchronous, fallbackContinue)
	for _, id := range []string{"zeta.ext", "alpha.ext", "mid.ext"} {
		if res := install(t, srv, signedHookManifest(t, key, id, "1.0.0", endpoint.URL, decl, hooksdk.OnTurnStart)); res.Code != http.StatusOK {
			t.Fatalf("install %s: %d", id, res.Code)
		}
	}
	// Upgrading alpha re-registers it; it must not move to the back.
	if res := install(t, srv, signedHookManifest(t, key, "alpha.ext", "1.1.0", endpoint.URL, decl, hooksdk.OnTurnStart)); res.Code != http.StatusOK {
		t.Fatalf("upgrade alpha: %d", res.Code)
	}
	want := "alpha.ext,mid.ext,zeta.ext"
	order := func(s *extServer) string {
		results, err := s.dispatchHooks(context.Background(), hookDispatchRequest{Event: hooksdk.OnTurnStart, TenantID: "t", PrincipalID: "p"})
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]string, len(results))
		for i, r := range results {
			ids[i] = r.ExtensionID
		}
		return strings.Join(ids, ",")
	}
	if got := order(srv); got != want {
		t.Fatalf("order = %s, want %s", got, want)
	}
	if got := order(newExtServerAt(srv.storePath)); got != want {
		t.Fatalf("order after reload = %s, want %s", got, want)
	}

	// State persisted before ordering was enforced is normalised on load.
	path := filepath.Join(t.TempDir(), "legacy.json")
	legacy := persistedExtensionState{Hooks: map[string][]*Hook{hooksdk.OnTurnStart: {
		{Event: hooksdk.OnTurnStart, ExtensionID: "zeta.ext", Endpoint: endpoint.URL, TimeoutMs: 1000},
		{Event: hooksdk.OnTurnStart, ExtensionID: "alpha.ext", Endpoint: endpoint.URL, TimeoutMs: 1000},
		{Event: hooksdk.OnTurnStart, ExtensionID: "mid.ext", Endpoint: endpoint.URL, TimeoutMs: 1000},
	}}}
	if err := os.WriteFile(path, mustJSON(t, legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := order(newExtServerAt(path)); got != want {
		t.Fatalf("legacy order after reload = %s, want %s", got, want)
	}
}

func TestAsynchronousHookDoesNotBlockDispatch(t *testing.T) {
	release := make(chan struct{})
	called := make(chan struct{}, 1)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer slow.Close()
	syncEndpoint, syncCalls := hookRecorder(t, http.StatusNoContent)

	srv := newExtServer()
	srv.extensions["async.ext"] = &Extension{Manifest: ExtensionManifest{PackageID: "async.ext"}, Status: "enabled", Health: "unknown"}
	enableExtensions(srv, "sync.ext")
	srv.hooks[hooksdk.OnAfterModel] = []*Hook{
		{Event: hooksdk.OnAfterModel, ExtensionID: "async.ext", Type: hookTypeAsynchronous, Endpoint: slow.URL, TimeoutMs: 5000, FallbackPolicy: fallbackContinue},
		{Event: hooksdk.OnAfterModel, ExtensionID: "sync.ext", Type: hookTypeSynchronous, Endpoint: syncEndpoint.URL, TimeoutMs: 1000, FallbackPolicy: fallbackContinue},
	}
	start := time.Now()
	results, err := srv.dispatchHooks(context.Background(), hookDispatchRequest{Event: hooksdk.OnAfterModel, TenantID: "t", PrincipalID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("dispatch blocked on an asynchronous hook for %v", elapsed)
	}
	if len(results) != 2 || results[0].Status != "queued" || results[1].Status != "succeeded" || len(syncCalls()) != 1 {
		t.Fatalf("results = %#v", results)
	}
	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("asynchronous hook was never invoked")
	}
	close(release)
	srv.asyncHooks.Wait()
	srv.mu.RLock()
	health := srv.extensions["async.ext"].Health
	srv.mu.RUnlock()
	if health != "healthy" {
		t.Fatalf("async success not recorded in health: %q", health)
	}
}

func TestAsynchronousHookFailureIsVisible(t *testing.T) {
	collect := captureMetrics(t)
	failing, calls := hookRecorder(t, http.StatusInternalServerError)
	srv := newExtServer()
	srv.extensions["async.ext"] = &Extension{Manifest: ExtensionManifest{PackageID: "async.ext"}, Status: "enabled", Health: "unknown"}
	srv.hooks[hooksdk.OnMemoryPromoted] = []*Hook{{
		Event: hooksdk.OnMemoryPromoted, ExtensionID: "async.ext", Type: hookTypeAsynchronous, Endpoint: failing.URL, TimeoutMs: 1000, FallbackPolicy: fallbackContinue,
	}}
	body := mustJSON(t, map[string]any{"event": hooksdk.OnMemoryPromoted, "payload": map[string]any{}})
	res := httptest.NewRecorder()
	srv.handleHookDispatch(res, asTenant(httptest.NewRequest(http.MethodPost, "/v1/hooks/dispatch", bytes.NewReader(body))))
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"queued"`) {
		t.Fatalf("dispatch returned %d: %s", res.Code, res.Body.String())
	}
	srv.asyncHooks.Wait()
	if len(calls()) != 1 {
		t.Fatalf("async hook calls = %d", len(calls()))
	}
	srv.mu.RLock()
	health, failures := srv.extensions["async.ext"].Health, srv.hooks[hooksdk.OnMemoryPromoted][0].ConsecutiveFailures
	srv.mu.RUnlock()
	if health != "degraded" || failures != 1 {
		t.Fatalf("async failure not reflected in health/breaker: health=%q failures=%d", health, failures)
	}
	var asyncFailure bool
	for _, p := range pointsNamed(collect(), telemetry.MetricHookFailure) {
		if p.Labels["reason"] == "async_failed" && p.Labels["extension"] == "async.ext" && p.Tenant == "tenant-l" {
			asyncFailure = true
		}
	}
	if !asyncFailure {
		t.Fatal("async hook failure metric was not emitted")
	}
}

// A fail_closed hook persisted as asynchronous (the pre-declaration default)
// is still awaited: detaching it would silently drop its veto.
func TestFailClosedHookIsNeverDetached(t *testing.T) {
	failing, _ := hookRecorder(t, http.StatusInternalServerError)
	srv := newExtServer()
	enableExtensions(srv, "legacy.ext")
	srv.hooks[hooksdk.OnCheckpoint] = []*Hook{{
		Event: hooksdk.OnCheckpoint, ExtensionID: "legacy.ext", Type: hookTypeAsynchronous, Endpoint: failing.URL, TimeoutMs: 1000, FallbackPolicy: fallbackFailClosed,
	}}
	results, err := srv.dispatchHooks(context.Background(), hookDispatchRequest{Event: hooksdk.OnCheckpoint, TenantID: "t", PrincipalID: "p"})
	if err == nil || len(results) != 1 || results[0].Status != "failed" {
		t.Fatalf("fail_closed async hook was not enforced: results=%#v err=%v", results, err)
	}
}

// TestNonEnabledExtensionHooksAreSkipped: a hook belonging to an extension
// that is not enabled is never invoked -- for /v1/hooks/dispatch or any
// lifecycle event -- and is reported skipped; re-enabling restores delivery.
func TestNonEnabledExtensionHooksAreSkipped(t *testing.T) {
	srv, key := metricsFixture(t)
	endpoint, calls := hookRecorder(t, http.StatusNoContent)
	events := []string{hooksdk.OnTurnStart, hooksdk.OnInstall, hooksdk.OnEnable, hooksdk.OnDisable, hooksdk.OnUninstall}
	// Critical, so a skip also proves a disabled extension cannot veto.
	sleeper := signedHookManifest(t, key, "sleeper.ext", "1.0.0", endpoint.URL, hookDecl(hookTypeSynchronous, fallbackFailClosed), events...)
	if res := installAs(t, srv, sleeper); res.Code != http.StatusOK {
		t.Fatalf("install sleeper: %d %s", res.Code, res.Body.String())
	}
	target := validManifest("target.ext")
	target.Signature = signManifest(t, target, key)
	if res := installAs(t, srv, target); res.Code != http.StatusOK {
		t.Fatalf("install target: %d", res.Code)
	}
	// The sleeper receives its own on_disable -- the intentional exception.
	before := len(calls())
	if res := callAs(srv, http.MethodPost, "/v1/extensions/sleeper.ext/disable", nil); res.Code != http.StatusOK {
		t.Fatalf("disable sleeper: %d", res.Code)
	}
	if got := calls()[before:]; len(got) != 1 || got[0].Event != hooksdk.OnDisable || got[0].Payload["extension_id"] != "sleeper.ext" {
		t.Fatalf("sleeper did not receive its own on_disable: %#v", got)
	}

	wantSkipped := func(step string, reports []lifecycleReport) {
		t.Helper()
		if len(reports) != 1 || len(reports[0].Results) != 1 || reports[0].Results[0].Status != "skipped" || reports[0].Results[0].Reason != "extension disabled" {
			t.Fatalf("%s: expected a visible skip, got %#v", step, reports)
		}
	}
	before = len(calls())
	dispatch := func() []hookDispatchResult {
		body := mustJSON(t, map[string]any{"event": hooksdk.OnTurnStart, "payload": map[string]any{}})
		res := httptest.NewRecorder()
		srv.handleHookDispatch(res, asTenant(httptest.NewRequest(http.MethodPost, "/v1/hooks/dispatch", bytes.NewReader(body))))
		if res.Code != http.StatusOK {
			t.Fatalf("dispatch returned %d: %s", res.Code, res.Body.String())
		}
		var out struct {
			Results []hookDispatchResult `json:"results"`
		}
		_ = json.Unmarshal(res.Body.Bytes(), &out)
		return out.Results
	}
	if r := dispatch(); len(r) != 1 || r[0].Status != "skipped" || r[0].Reason != "extension disabled" {
		t.Fatalf("dispatch to disabled extension = %#v", r)
	}
	for _, step := range []struct{ method, path string }{
		{http.MethodPost, "/v1/extensions/target.ext/disable"},
		{http.MethodPost, "/v1/extensions/target.ext/enable"},
		{http.MethodDelete, "/v1/extensions/target.ext"},
	} {
		res := callAs(srv, step.method, step.path, nil)
		if res.Code != http.StatusOK {
			t.Fatalf("%s %s: %d %s", step.method, step.path, res.Code, res.Body.String())
		}
		wantSkipped(step.path, decodeLifecycle(t, res))
	}
	other := validManifest("other.ext")
	other.Signature = signManifest(t, other, key)
	res := installAs(t, srv, other)
	if res.Code != http.StatusOK {
		t.Fatalf("install while critical sleeper disabled: %d %s", res.Code, res.Body.String())
	}
	wantSkipped("install", decodeLifecycle(t, res))
	if got := calls()[before:]; len(got) != 0 {
		t.Fatalf("disabled extension's hooks were invoked: %#v", got)
	}

	// Re-enabling restores delivery (it also receives its own on_enable).
	if res := callAs(srv, http.MethodPatch, "/v1/extensions/sleeper.ext", []byte(`{"status":"enabled"}`)); res.Code != http.StatusOK {
		t.Fatalf("re-enable: %d %s", res.Code, res.Body.String())
	}
	if r := dispatch(); len(r) != 1 || r[0].Status != "succeeded" {
		t.Fatalf("dispatch after re-enable = %#v", r)
	}
	if got := calls()[before:]; len(got) != 2 || got[0].Event != hooksdk.OnEnable || got[1].Event != hooksdk.OnTurnStart {
		t.Fatalf("delivery not restored: %#v", got)
	}

	// Any other non-enabled status is skipped the same way.
	srv.extensions["sleeper.ext"].Status = "quarantined"
	if r := dispatch(); len(r) != 1 || r[0].Reason != "extension quarantined" {
		t.Fatalf("quarantined dispatch = %#v", r)
	}
}
