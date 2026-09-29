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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dylmarriner/scos-memory/packages/telemetry"
)

// captureMetrics points serviceMetrics at an in-test collector and returns a
// function that flushes and yields every point received so far.
func captureMetrics(t *testing.T) func() []telemetry.MetricPoint {
	t.Helper()
	var mu sync.Mutex
	var received []telemetry.MetricPoint
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/metrics/ingest" {
			t.Errorf("unexpected collector request %s %s", r.Method, r.URL.Path)
		}
		var points []telemetry.MetricPoint
		if err := json.NewDecoder(r.Body).Decode(&points); err != nil {
			t.Errorf("decode ingest: %v", err)
		}
		mu.Lock()
		received = append(received, points...)
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	emitter := telemetry.New("extension-manager", collector.URL, nil)
	previous := serviceMetrics
	serviceMetrics = emitter
	t.Cleanup(func() {
		serviceMetrics = previous
		emitter.Close()
		collector.Close()
	})
	return func() []telemetry.MetricPoint {
		if err := emitter.Flush(context.Background()); err != nil {
			t.Fatalf("flush: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		return append([]telemetry.MetricPoint(nil), received...)
	}
}

func pointsNamed(points []telemetry.MetricPoint, name string) []telemetry.MetricPoint {
	var out []telemetry.MetricPoint
	for _, p := range points {
		if p.Name == name {
			out = append(out, p)
		}
	}
	return out
}

func metricsFixture(t *testing.T) (*extServer, ed25519.PrivateKey) {
	t.Helper()
	publicKey, privateKey, _ := ed25519.GenerateKey(nil)
	t.Setenv("SCOS_EXTENSION_SIGNING_PUBLIC_KEY", base64.StdEncoding.EncodeToString(publicKey))
	return newExtServerAt(filepath.Join(t.TempDir(), "extension-state.json")), privateKey
}

func install(t *testing.T, srv *extServer, m ExtensionManifest) *httptest.ResponseRecorder {
	t.Helper()
	res := httptest.NewRecorder()
	srv.handleInstall(res, httptest.NewRequest(http.MethodPost, "/v1/install", bytes.NewReader(mustJSON(t, m))))
	return res
}

func TestSignatureStateEmittedOnVerification(t *testing.T) {
	collect := captureMetrics(t)
	srv, key := metricsFixture(t)

	signed := validManifest("sig.good")
	signed.Signature = signManifest(t, signed, key)
	if res := install(t, srv, signed); res.Code != http.StatusOK {
		t.Fatalf("install signed returned %d: %s", res.Code, res.Body.String())
	}
	forged := validManifest("sig.bad")
	forged.Signature = base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))
	if res := install(t, srv, forged); res.Code != http.StatusForbidden {
		t.Fatalf("install forged returned %d", res.Code)
	}

	got := map[string]telemetry.MetricPoint{}
	for _, p := range pointsNamed(collect(), telemetry.MetricSignatureState) {
		got[p.Labels["extension"]] = p
	}
	if p := got["sig.good"]; p.Value != 1 || p.Labels["state"] != signatureValid || p.Labels["version"] != "1.0.0" {
		t.Fatalf("valid signature point = %#v", p)
	}
	if p := got["sig.bad"]; p.Value != 0 || p.Labels["state"] != signatureInvalid {
		t.Fatalf("invalid signature point = %#v", p)
	}
}

func TestPermissionsEmittedOnInstallAndLifecycle(t *testing.T) {
	collect := captureMetrics(t)
	srv, key := metricsFixture(t)

	m := validManifest("perm.ext")
	m.Permissions = []string{"memory.write", "memory.read"}
	m.Signature = signManifest(t, m, key)
	if res := install(t, srv, m); res.Code != http.StatusOK {
		t.Fatalf("install returned %d: %s", res.Code, res.Body.String())
	}
	for _, action := range []string{"disable", "enable"} {
		res := httptest.NewRecorder()
		srv.handleExtensionByID(res, httptest.NewRequest(http.MethodPost, "/v1/extensions/perm.ext/"+action, nil))
		if res.Code != http.StatusOK {
			t.Fatalf("%s returned %d", action, res.Code)
		}
	}

	points := pointsNamed(collect(), telemetry.MetricExtensionPermissions)
	want := []struct {
		action string
		value  float64
	}{{"install", 2}, {"disable", 0}, {"enable", 2}}
	if len(points) != len(want) {
		t.Fatalf("permission points = %#v", points)
	}
	for i, w := range want {
		p := points[i]
		if p.Labels["action"] != w.action || p.Value != w.value || p.Labels["extension"] != "perm.ext" ||
			p.Labels["permissions"] != "memory.read,memory.write" {
			t.Fatalf("point %d = %#v, want action %s value %v", i, p, w.action, w.value)
		}
	}
}

func TestPluginHealthTracksHookOutcomes(t *testing.T) {
	collect := captureMetrics(t)
	var failing atomic.Bool
	hookServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if failing.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer hookServer.Close()

	srv := newExtServerAt(filepath.Join(t.TempDir(), "extension-state.json"))
	srv.extensions["health.ext"] = &Extension{Manifest: ExtensionManifest{PackageID: "health.ext"}, Status: "enabled", Health: "unknown"}
	srv.hooks["turn.completed"] = []*Hook{{
		Event: "turn.completed", ExtensionID: "health.ext", Endpoint: hookServer.URL,
		TimeoutMs: 1000, FallbackPolicy: "continue",
	}}
	dispatch := func() {
		t.Helper()
		body := mustJSON(t, map[string]any{"event": "turn.completed"})
		req := httptest.NewRequest(http.MethodPost, "/v1/hooks/dispatch", bytes.NewReader(body))
		req.Header.Set("X-SCOS-Tenant-ID", "tenant-h")
		req.Header.Set("X-SCOS-Principal-ID", "principal-h")
		res := httptest.NewRecorder()
		srv.handleHookDispatch(res, req)
		if res.Code != http.StatusOK {
			t.Fatalf("dispatch returned %d: %s", res.Code, res.Body.String())
		}
	}

	dispatch() // unknown -> healthy
	failing.Store(true)
	dispatch() // healthy -> degraded
	dispatch() // degraded, unchanged: no point
	dispatch() // third consecutive failure opens the circuit -> unhealthy

	points := pointsNamed(collect(), telemetry.MetricPluginHealth)
	want := []struct {
		state    string
		value    float64
		failures string
	}{{"healthy", 1, "0"}, {"degraded", 0, "1"}, {"unhealthy", 0, "3"}}
	if len(points) != len(want) {
		t.Fatalf("health points = %#v", points)
	}
	for i, w := range want {
		p := points[i]
		if p.Labels["state"] != w.state || p.Value != w.value || p.Labels["consecutive_failures"] != w.failures ||
			p.Labels["extension"] != "health.ext" || p.Tenant != "tenant-h" {
			t.Fatalf("point %d = %#v, want %+v", i, p, w)
		}
	}
	if got := srv.extensions["health.ext"].Health; got != "unhealthy" {
		t.Fatalf("extension health = %q, want unhealthy", got)
	}
}

func TestResourceConsumptionMeasuresHookCalls(t *testing.T) {
	collect := captureMetrics(t)
	const delay = 25 * time.Millisecond
	hookServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(delay)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer hookServer.Close()

	srv := newExtServerAt(filepath.Join(t.TempDir(), "extension-state.json"))
	srv.extensions["res.ext"] = &Extension{Manifest: ExtensionManifest{PackageID: "res.ext"}, Status: "enabled"}
	srv.hooks["turn.completed"] = []*Hook{{
		Event: "turn.completed", ExtensionID: "res.ext", Endpoint: hookServer.URL,
		TimeoutMs: 1000, FallbackPolicy: "continue",
	}}
	req := hookDispatchRequest{
		Event: "turn.completed", TenantID: "tenant-r", PrincipalID: "principal-r",
		Payload: map[string]any{"message_id": "m-42"},
	}
	if _, err := srv.dispatchHooks(context.Background(), req); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	sent, _ := json.Marshal(map[string]any{"event": req.Event, "tenant_id": req.TenantID, "principal_id": req.PrincipalID, "payload": req.Payload})

	byResource := map[string]telemetry.MetricPoint{}
	for _, p := range pointsNamed(collect(), telemetry.MetricResourceConsumption) {
		byResource[p.Labels["resource"]] = p
	}
	callTime := byResource["hook_call_time"]
	if callTime.Unit != "ms" || callTime.Value < float64(delay.Milliseconds()) || callTime.Labels["extension"] != "res.ext" || callTime.Tenant != "tenant-r" {
		t.Fatalf("hook_call_time point = %#v", callTime)
	}
	payload := byResource["hook_payload_bytes"]
	if payload.Unit != "bytes" || payload.Value != float64(len(sent)) || payload.Labels["event"] != "turn.completed" {
		t.Fatalf("hook_payload_bytes point = %#v, want %d bytes", payload, len(sent))
	}
}

func TestSkillSelectionEmittedOnResolve(t *testing.T) {
	collect := captureMetrics(t)
	srv, key := metricsFixture(t)
	m := validManifest("skill.ext")
	m.SkillsExported = []string{"summarise"}
	m.Signature = signManifest(t, m, key)
	if res := install(t, srv, m); res.Code != http.StatusOK {
		t.Fatalf("install returned %d: %s", res.Code, res.Body.String())
	}

	for _, id := range []string{"skill.ext/summarise", "skill.ext/missing"} {
		req := httptest.NewRequest(http.MethodGet, "/v1/skills/"+id, nil)
		req.Header.Set("X-SCOS-Tenant-ID", "tenant-s")
		srv.handleSkillByID(httptest.NewRecorder(), req)
	}

	points := pointsNamed(collect(), telemetry.MetricSkillSelection)
	if len(points) != 2 {
		t.Fatalf("skill selection points = %#v", points)
	}
	hit, miss := points[0], points[1]
	if hit.Value != 1 || hit.Labels["outcome"] != "selected" || hit.Labels["name"] != "summarise" ||
		hit.Labels["extension"] != "skill.ext" || hit.Labels["version"] != "1.0.0" || hit.Tenant != "tenant-s" {
		t.Fatalf("selected point = %#v", hit)
	}
	if miss.Value != 1 || miss.Labels["outcome"] != "not_found" || miss.Labels["skill"] != "skill.ext/missing" {
		t.Fatalf("not_found point = %#v", miss)
	}
}

func TestPermissionsEmittedOnPatchStatusChange(t *testing.T) {
	srv, key := metricsFixture(t)
	m := validManifest("patch.ext")
	m.Permissions = []string{"memory.read", "tools.invoke", "memory.write"}
	m.Signature = signManifest(t, m, key)
	if res := install(t, srv, m); res.Code != http.StatusOK {
		t.Fatalf("install returned %d: %s", res.Code, res.Body.String())
	}
	collect := captureMetrics(t)
	patch := func(body string) {
		t.Helper()
		res := httptest.NewRecorder()
		srv.handleExtensionByID(res, httptest.NewRequest(http.MethodPatch, "/v1/extensions/patch.ext", bytes.NewReader([]byte(body))))
		if res.Code != http.StatusOK {
			t.Fatalf("patch %s returned %d: %s", body, res.Code, res.Body.String())
		}
	}
	patch(`{"status":"disabled"}`)
	patch(`{"status":"disabled"}`)      // unchanged status: no point
	patch(`{"config":{"threshold":3}}`) // no status change: no point
	patch(`{"status":"enabled"}`)

	points := pointsNamed(collect(), telemetry.MetricExtensionPermissions)
	want := []struct {
		action string
		value  float64
	}{{"status_disabled", 0}, {"status_enabled", 3}}
	if len(points) != len(want) {
		t.Fatalf("permission points = %#v", points)
	}
	for i, w := range want {
		p := points[i]
		if p.Labels["action"] != w.action || p.Value != w.value || p.Labels["extension"] != "patch.ext" ||
			p.Labels["permissions"] != "memory.read,memory.write,tools.invoke" {
			t.Fatalf("point %d = %#v, want action %s value %v", i, p, w.action, w.value)
		}
	}
}
