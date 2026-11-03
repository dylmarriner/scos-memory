package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/telemetry"
)

// A hook failure must remain attributable to the initiating tenant and agent
// in the collector, without relying on high-cardinality metric labels.
func TestHookLatencyAndFailureMetricsCarryCallerCorrelation(t *testing.T) {
	metrics := extensionMetricCollector(t)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "deliberate hook failure", http.StatusInternalServerError)
	}))
	defer endpoint.Close()

	srv := newExtServer()
	enableForTest(srv, "correlation-ext")
	srv.hooks["on_checkpoint"] = []*Hook{{
		Event: "on_checkpoint", ExtensionID: "correlation-ext",
		Endpoint: endpoint.URL, TimeoutMs: 1000, FallbackPolicy: "continue",
	}}

	req := httptest.NewRequest(http.MethodPost, "/v1/hooks/dispatch",
		bytes.NewBufferString(`{"event":"on_checkpoint","agent_id":"agent-a","payload":{}}`))
	req.Header.Set("X-SCOS-Tenant-ID", "tenant-a")
	req.Header.Set("X-SCOS-Principal-ID", "principal-a")
	res := httptest.NewRecorder()
	srv.handleHookDispatch(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("hook dispatch: %d %s", res.Code, res.Body.String())
	}

	points := metrics()
	for _, metric := range []string{telemetry.MetricHookLatency, telemetry.MetricHookFailure} {
		found := filterExtensionMetrics(points, metric)
		if len(found) != 1 {
			t.Fatalf("%s: expected one metric, got %+v", metric, found)
		}
		if found[0].Tenant != "tenant-a" || found[0].AgentID != "agent-a" {
			t.Fatalf("%s missing caller correlation: %+v", metric, found[0])
		}
	}
	// Persistence failure is a dispatch-level failure with the same caller.
	srv.persistHookExecutions = func(context.Context) error {
		return errors.New("injected persistence failure")
	}
	req = httptest.NewRequest(http.MethodPost, "/v1/hooks/dispatch",
		bytes.NewBufferString(`{"event":"on_checkpoint","agent_id":"agent-a","payload":{}}`))
	req.Header.Set("X-SCOS-Tenant-ID", "tenant-a")
	req.Header.Set("X-SCOS-Principal-ID", "principal-a")
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4730-00f067aa0ba902b7-01")
	res = httptest.NewRecorder()
	srv.handleHookDispatch(res, req)
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("failed persistence should fail dispatch: %d %s", res.Code, res.Body.String())
	}
	var dispatchErrors int
	for _, point := range filterExtensionMetrics(metrics(), telemetry.MetricHookFailure) {
		if point.Labels["reason"] == "dispatch_error" {
			dispatchErrors++
			if point.Tenant != "tenant-a" || point.AgentID != "agent-a" {
				t.Fatalf("dispatch error missing caller correlation: %+v", point)
			}
			if point.TraceID != "4bf92f3577b34da6a3ce929d0e0e4730" || point.SpanID != "00f067aa0ba902b7" {
				t.Fatalf("dispatch error missing trace correlation: %+v", point)
			}
		}
	}
	if dispatchErrors != 1 {
		t.Fatalf("expected one dispatch_error metric, got %d", dispatchErrors)
	}

}
