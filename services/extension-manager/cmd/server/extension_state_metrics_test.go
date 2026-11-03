package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/telemetry"
)

func extensionMetricCollector(t *testing.T) func() []telemetry.MetricPoint {
	t.Helper()
	var mu sync.Mutex
	var received []telemetry.MetricPoint
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var points []telemetry.MetricPoint
		if err := json.NewDecoder(r.Body).Decode(&points); err != nil {
			http.Error(w, "invalid metrics", http.StatusBadRequest)
			return
		}
		mu.Lock()
		received = append(received, points...)
		mu.Unlock()
	}))
	original := serviceMetrics
	serviceMetrics = telemetry.New("extension-manager", collector.URL, collector.Client())
	t.Cleanup(func() {
		serviceMetrics.Close()
		serviceMetrics = original
		collector.Close()
	})
	return func() []telemetry.MetricPoint {
		t.Helper()
		if err := serviceMetrics.Flush(context.Background()); err != nil {
			t.Fatalf("flush metrics: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		return append([]telemetry.MetricPoint(nil), received...)
	}
}

func filterExtensionMetrics(points []telemetry.MetricPoint, name string) []telemetry.MetricPoint {
	var filtered []telemetry.MetricPoint
	for _, point := range points {
		if point.Name == name {
			filtered = append(filtered, point)
		}
	}
	return filtered
}

func TestExtensionVerifiedSignatureMetricRequiresDurableInstallHTTP(t *testing.T) {
	srv, key := canaryFixture(t)
	metrics := extensionMetricCollector(t)

	install := func(version string) *httptest.ResponseRecorder {
		t.Helper()
		manifest := canaryManifest(t, key, version)
		res := httptest.NewRecorder()
		srv.handleInstall(res, operatorRequest(http.MethodPost, "/v1/install", bytes.NewReader(mustJSON(t, manifest))))
		return res
	}
	if res := install("2.0.0"); res.Code != http.StatusOK {
		t.Fatalf("valid signed install: %d %s", res.Code, res.Body.String())
	}
	signed := filterExtensionMetrics(metrics(), telemetry.MetricSignatureState)
	if len(signed) != 1 || signed[0].Value != 1 || signed[0].Labels["extension"] != "canary.ext" || signed[0].Labels["version"] != "2.0.0" || signed[0].Labels["state"] != "verified" {
		t.Fatalf("committed signature evidence missing or inaccurate: %+v", signed)
	}

	invalid := canaryManifest(t, key, "2.5.0")
	invalid.Signature = "not-a-valid-signature"
	rejected := httptest.NewRecorder()
	srv.handleInstall(rejected, operatorRequest(http.MethodPost, "/v1/install", bytes.NewReader(mustJSON(t, invalid))))
	if rejected.Code != http.StatusForbidden {
		t.Fatalf("invalid signature must be rejected: %d %s", rejected.Code, rejected.Body.String())
	}
	if got := filterExtensionMetrics(metrics(), telemetry.MetricSignatureState); len(got) != 1 {
		t.Fatalf("rejected signature fabricated verified metric: %+v", got)
	}

	srv.persistBeforeWrite = func(context.Context) error { return errors.New("injected durable failure") }
	if res := install("3.0.0"); res.Code != http.StatusServiceUnavailable {
		t.Fatalf("failed persistence must not be acknowledged: %d %s", res.Code, res.Body.String())
	}
	if got := filterExtensionMetrics(metrics(), telemetry.MetricSignatureState); len(got) != 1 {
		t.Fatalf("failed durable install fabricated verified signature metric: %+v", got)
	}
}

func TestExtensionHookHealthMetricsReflectActualHTTPDelivery(t *testing.T) {
	metrics := extensionMetricCollector(t)
	var fail atomic.Bool
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			http.Error(w, "hook failure", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer endpoint.Close()

	srv := newTestExtServer()
	enableForTest(srv, "metric-ext")
	srv.hooks["on_checkpoint"] = []*Hook{{Event: "on_checkpoint", ExtensionID: "metric-ext", Endpoint: endpoint.URL, TimeoutMs: 1000, FallbackPolicy: "continue"}}
	dispatch := func() {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/hooks/dispatch", bytes.NewBufferString(`{"event":"on_checkpoint","payload":{}}`))
		req.Header.Set("X-SCOS-Tenant-ID", "tenant-a")
		req.Header.Set("X-SCOS-Principal-ID", "principal-a")
		res := httptest.NewRecorder()
		srv.handleHookDispatch(res, req)
		if res.Code != http.StatusOK {
			t.Fatalf("hook dispatch: %d %s", res.Code, res.Body.String())
		}
	}
	dispatch()
	fail.Store(true)
	dispatch()
	health := filterExtensionMetrics(metrics(), telemetry.MetricPluginHealth)
	if len(health) != 2 || health[0].Value != 1 || health[0].Labels["health"] != "healthy" || health[1].Value != 0 || health[1].Labels["health"] != "failing" {
		t.Fatalf("observed hook health not emitted correctly: %+v", health)
	}
	for _, point := range health {
		if point.Labels["extension"] != "metric-ext" {
			t.Fatalf("health metric missing extension identity: %+v", point)
		}
	}
}
