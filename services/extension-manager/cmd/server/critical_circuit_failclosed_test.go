package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A fail-closed hook is a mandatory gate even when its circuit is already
// open. The circuit must suppress network calls, not silently authorize work.
func TestCriticalHookOpenCircuitAbortsDispatch(t *testing.T) {
	delivered := 0
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		delivered++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer endpoint.Close()

	srv := newExtServer()
	enableForTest(srv, "critical", "later")
	future := time.Now().UTC().Add(5 * time.Minute).Format(time.RFC3339)
	srv.hooks["on_before_tool"] = []*Hook{
		{Event: "on_before_tool", ExtensionID: "critical", Endpoint: endpoint.URL, FallbackPolicy: "fail_closed", CircuitOpenUntil: future, TimeoutMs: 1000},
		{Event: "on_before_tool", ExtensionID: "later", Endpoint: endpoint.URL, FallbackPolicy: "continue", TimeoutMs: 1000},
	}
	results, err := srv.dispatchHooks(context.Background(), hookDispatchRequest{Event: "on_before_tool", TenantID: "t", PrincipalID: "p"})
	if err == nil || !strings.Contains(err.Error(), "critical") || !strings.Contains(err.Error(), "circuit") {
		t.Fatalf("open mandatory circuit must abort dispatch, got results=%+v error=%v", results, err)
	}
	if delivered != 0 {
		t.Fatalf("open circuit must not call any hook endpoint: %d deliveries", delivered)
	}
	if srv.extensions["critical"].Health != "circuit_open" {
		t.Fatalf("critical health not recorded: %+v", srv.extensions["critical"])
	}
}

// Optional hooks retain their documented continue behavior on an open
// circuit; a mandatory hook must be an HTTP 503, not an HTTP 200.
func TestCriticalOpenCircuitReturnsHTTPServiceUnavailable(t *testing.T) {
	srv := newExtServer()
	enableForTest(srv, "mandatory")
	srv.hooks["on_before_tool"] = []*Hook{{
		Event: "on_before_tool", ExtensionID: "mandatory",
		Endpoint:         "http://127.0.0.1:1/never-call",
		FallbackPolicy:   "fail_closed",
		CircuitOpenUntil: time.Now().UTC().Add(time.Minute).Format(time.RFC3339),
	}}
	req := httptest.NewRequest(http.MethodPost, "/v1/hooks/dispatch", bytes.NewBufferString(`{"event":"on_before_tool","payload":{}}`))
	req.Header.Set("X-SCOS-Tenant-ID", "t")
	req.Header.Set("X-SCOS-Principal-ID", "p")
	res := httptest.NewRecorder()
	srv.handleHookDispatch(res, req)
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("mandatory open circuit returned %d, want 503: %s", res.Code, res.Body.String())
	}
	if len(srv.executions) != 1 || srv.executions[0].Status != "aborted" {
		t.Fatalf("mandatory failure must be auditable: %+v", srv.executions)
	}
}

func TestOptionalHookOpenCircuitContinuesWithRecordedSkip(t *testing.T) {
	srv := newExtServer()
	enableForTest(srv, "optional")
	srv.hooks["on_before_tool"] = []*Hook{{
		Event: "on_before_tool", ExtensionID: "optional",
		Endpoint:         "http://127.0.0.1:1/never-call",
		FallbackPolicy:   "continue",
		CircuitOpenUntil: time.Now().UTC().Add(time.Minute).Format(time.RFC3339),
	}}
	results, err := srv.dispatchHooks(context.Background(), hookDispatchRequest{Event: "on_before_tool", TenantID: "t", PrincipalID: "p"})
	if err != nil || len(results) != 1 || results[0].Status != "circuit_open" {
		t.Fatalf("optional circuit should be recorded and skipped: %+v %v", results, err)
	}
}
