package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/telemetry"
)

func TestHookMetricsCarryValidatedTraceAndSpan(t *testing.T) {
	metrics := extensionMetricCollector(t)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "injected hook failure", http.StatusInternalServerError)
	}))
	defer endpoint.Close()

	srv := newTestExtServer()
	enableForTest(srv, "trace-ext")
	srv.hooks["on_checkpoint"] = []*Hook{{
		Event: "on_checkpoint", ExtensionID: "trace-ext", Endpoint: endpoint.URL,
		TimeoutMs: 1000, FallbackPolicy: "continue",
	}}

	dispatch := func(header string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/hooks/dispatch",
			bytes.NewBufferString(`{"event":"on_checkpoint","agent_id":"agent-a","payload":{}}`))
		req.Header.Set("X-SCOS-Tenant-ID", "tenant-a")
		req.Header.Set("X-SCOS-Principal-ID", "principal-a")
		req.Header.Set("traceparent", header)
		res := httptest.NewRecorder()
		srv.handleHookDispatch(res, req)
		if res.Code != http.StatusOK {
			t.Fatalf("dispatch: %d %s", res.Code, res.Body.String())
		}
	}
	dispatch("00-4bf92f3577b34da6a3ce929d0e0e4730-00f067aa0ba902b7-01")
	dispatch("00-zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz-00f067aa0ba902b7-01")
	for _, metric := range []string{telemetry.MetricHookLatency, telemetry.MetricHookFailure} {
		points := filterExtensionMetrics(metrics(), metric)
		if len(points) != 2 {
			t.Fatalf("%s: expected 2 points, got %+v", metric, points)
		}
		if points[0].TraceID != "4bf92f3577b34da6a3ce929d0e0e4730" || points[0].SpanID != "00f067aa0ba902b7" {
			t.Errorf("%s: valid trace missing: %+v", metric, points[0])
		}
		if points[1].TraceID != "" || points[1].SpanID != "" {
			t.Errorf("%s: invalid trace accepted: %+v", metric, points[1])
		}
		for _, point := range points {
			if point.Tenant != "tenant-a" || point.AgentID != "agent-a" {
				t.Errorf("%s: caller correlation lost: %+v", metric, point)
			}
		}
	}
}
