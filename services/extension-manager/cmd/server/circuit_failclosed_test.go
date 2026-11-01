package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A critical hook whose breaker is open must never be reported as a successful
// dispatch, and later hooks must not run after that mandatory gate is blocked.
func TestCriticalHookOpenCircuitBlocksDispatchAndFollowingHooks(t *testing.T) {
	var calls atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer endpoint.Close()
	srv := newExtServer()
	enableForTest(srv, "critical-ext", "later-ext")
	srv.hooks["on_checkpoint"] = []*Hook{
		{Event: "on_checkpoint", ExtensionID: "critical-ext", Endpoint: endpoint.URL, TimeoutMs: 1000, FallbackPolicy: "fail_closed", CircuitOpenUntil: time.Now().Add(time.Minute).UTC().Format(time.RFC3339)},
		{Event: "on_checkpoint", ExtensionID: "later-ext", Endpoint: endpoint.URL, TimeoutMs: 1000, FallbackPolicy: "continue"},
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/hooks/dispatch", bytes.NewBufferString(`{"event":"on_checkpoint","payload":{}}`))
	req.Header.Set("X-SCOS-Tenant-ID", "tenant-a")
	req.Header.Set("X-SCOS-Principal-ID", "principal-a")
	res := httptest.NewRecorder()
	srv.handleHookDispatch(res, req)
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("open critical breaker must block dispatch: status=%d body=%s", res.Code, res.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatalf("critical or subsequent hook called %d times", calls.Load())
	}
	if len(srv.executions) != 1 || srv.executions[0].Status != "aborted" {
		t.Fatalf("expected durable aborted execution, got %+v", srv.executions)
	}
}

func TestCriticalHookInvalidCircuitStateFailsClosed(t *testing.T) {
	var calls atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(http.StatusNoContent) }))
	defer endpoint.Close()
	srv := newExtServer()
	enableForTest(srv, "critical-ext")
	srv.extensions["critical-ext"].Health = "healthy"
	srv.hooks["on_checkpoint"] = []*Hook{{Event: "on_checkpoint", ExtensionID: "critical-ext", Endpoint: endpoint.URL, TimeoutMs: 1000, FallbackPolicy: "fail_closed", CircuitOpenUntil: "not-a-timestamp"}}
	req := httptest.NewRequest(http.MethodPost, "/v1/hooks/dispatch", bytes.NewBufferString(`{"event":"on_checkpoint"}`))
	req.Header.Set("X-SCOS-Tenant-ID", "tenant-a")
	req.Header.Set("X-SCOS-Principal-ID", "principal-a")
	res := httptest.NewRecorder()
	srv.handleHookDispatch(res, req)
	if res.Code != http.StatusServiceUnavailable || calls.Load() != 0 {
		t.Fatalf("invalid critical breaker must fail closed: status=%d calls=%d", res.Code, calls.Load())
	}
	if strings.Contains(res.Body.String(), "not-a-timestamp") {
		t.Fatal("internal breaker state leaked to caller")
	}
	if ext := srv.extensions["critical-ext"]; ext.Health != "failing" || ext.HealthCheckedAt == "" {
		t.Fatalf("critical breaker corruption must invalidate previously healthy status: %+v", ext)
	}
}

func TestCriticalHookExpiredCircuitCanRecover(t *testing.T) {
	var calls atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(http.StatusNoContent) }))
	defer endpoint.Close()
	srv := newExtServer()
	enableForTest(srv, "critical-ext")
	hook := &Hook{Event: "on_checkpoint", ExtensionID: "critical-ext", Endpoint: endpoint.URL, TimeoutMs: 1000, FallbackPolicy: "fail_closed", CircuitOpenUntil: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)}
	srv.hooks["on_checkpoint"] = []*Hook{hook}
	req := httptest.NewRequest(http.MethodPost, "/v1/hooks/dispatch", bytes.NewBufferString(`{"event":"on_checkpoint"}`))
	req.Header.Set("X-SCOS-Tenant-ID", "tenant-a")
	req.Header.Set("X-SCOS-Principal-ID", "principal-a")
	res := httptest.NewRecorder()
	srv.handleHookDispatch(res, req)
	if res.Code != http.StatusOK || calls.Load() != 1 || hook.CircuitOpenUntil != "" {
		t.Fatalf("expired breaker did not recover: status=%d calls=%d breaker=%q", res.Code, calls.Load(), hook.CircuitOpenUntil)
	}
}

// An optional hook explicitly configured to continue must remain callable
// even when its persisted breaker timestamp is malformed.
func TestOptionalHookInvalidCircuitStateContinues(t *testing.T) {
	var calls atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer endpoint.Close()
	srv := newExtServer()
	enableForTest(srv, "optional-ext")
	srv.hooks["on_after_tool"] = []*Hook{{
		Event: "on_after_tool", ExtensionID: "optional-ext", Endpoint: endpoint.URL,
		TimeoutMs: 1000, FallbackPolicy: "continue", CircuitOpenUntil: "not-a-timestamp",
	}}
	req := httptest.NewRequest(http.MethodPost, "/v1/hooks/dispatch", bytes.NewBufferString(`{"event":"on_after_tool"}`))
	req.Header.Set("X-SCOS-Tenant-ID", "tenant-a")
	req.Header.Set("X-SCOS-Principal-ID", "principal-a")
	res := httptest.NewRecorder()
	srv.handleHookDispatch(res, req)
	if res.Code != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("optional hook should retain continue policy: status=%d calls=%d body=%s", res.Code, calls.Load(), res.Body.String())
	}
	if len(srv.executions) != 1 || srv.executions[0].Status != "succeeded" {
		t.Fatalf("optional hook outcome not recorded: %+v", srv.executions)
	}
}

// A later mandatory breaker must be checked before any earlier hook can
// produce irreversible external side effects in the same dispatch.
func TestLaterCriticalBreakerPreflightPreventsEarlierDeliveries(t *testing.T) {
	for _, tc := range []struct{ name, state, health string }{
		{name: "invalid", state: "not-a-timestamp", health: "failing"},
		{name: "open", state: time.Now().Add(time.Minute).UTC().Format(time.RFC3339), health: "circuit_open"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(http.StatusNoContent) }))
			defer endpoint.Close()
			srv := newExtServer()
			enableForTest(srv, "earlier-ext", "critical-ext")
			srv.extensions["critical-ext"].Health = "healthy"
			srv.hooks["on_checkpoint"] = []*Hook{
				{Event: "on_checkpoint", ExtensionID: "earlier-ext", Endpoint: endpoint.URL, TimeoutMs: 1000, FallbackPolicy: "continue"},
				{Event: "on_checkpoint", ExtensionID: "critical-ext", Endpoint: endpoint.URL, TimeoutMs: 1000, FallbackPolicy: "fail_closed", CircuitOpenUntil: tc.state},
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/hooks/dispatch", bytes.NewBufferString(`{"event":"on_checkpoint"}`))
			req.Header.Set("X-SCOS-Tenant-ID", "tenant-a")
			req.Header.Set("X-SCOS-Principal-ID", "principal-a")
			res := httptest.NewRecorder()
			srv.handleHookDispatch(res, req)
			if res.Code != http.StatusServiceUnavailable || calls.Load() != 0 {
				t.Fatalf("preflight failed: status=%d outbound_calls=%d", res.Code, calls.Load())
			}
			if ext := srv.extensions["critical-ext"]; ext.Health != tc.health || ext.HealthCheckedAt == "" {
				t.Fatalf("breaker health = %+v, want %s", ext, tc.health)
			}
		})
	}
}
