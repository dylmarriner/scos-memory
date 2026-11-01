package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// A mandatory hook may fail after optional hooks ran. The aborted dispatch
// must retain earlier tenant-scoped per-hook evidence rather than losing it.
func TestCriticalAbortPreservesEarlierHookExecutionEvidence(t *testing.T) {
	var firstCalls, criticalCalls atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		firstCalls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer first.Close()
	critical := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		criticalCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer critical.Close()
	srv := newExtServer()
	enableForTest(srv, "first", "invalid", "critical")
	srv.hooks["on_checkpoint"] = []*Hook{
		{Event: "on_checkpoint", ExtensionID: "first", Endpoint: first.URL, FallbackPolicy: "continue"},
		{Event: "on_checkpoint", ExtensionID: "invalid", Endpoint: "http://example.com/unsafe", FallbackPolicy: "continue"},
		{Event: "on_checkpoint", ExtensionID: "critical", Endpoint: critical.URL, FallbackPolicy: "fail_closed"},
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/hooks/dispatch", bytes.NewBufferString(`{"event":"on_checkpoint"}`))
	req.Header.Set("X-SCOS-Tenant-ID", "tenant-a")
	req.Header.Set("X-SCOS-Principal-ID", "principal-a")
	res := httptest.NewRecorder()
	srv.handleHookDispatch(res, req)
	if res.Code != http.StatusServiceUnavailable || firstCalls.Load() != 1 || criticalCalls.Load() != 1 {
		t.Fatalf("critical failure contract changed: status=%d first=%d critical=%d", res.Code, firstCalls.Load(), criticalCalls.Load())
	}
	if len(srv.executions) != 4 {
		t.Fatalf("critical abort lost earlier per-hook evidence: %+v", srv.executions)
	}
	want := []struct{ extension, status, reason string }{
		{"first", "succeeded", ""},
		{"invalid", "failed", "invalid_endpoint"},
		{"critical", "failed", "fallback_fail_closed"},
		{"", "aborted", "critical hook critical failed"},
	}
	for i, w := range want {
		got := srv.executions[i]
		if got.ExtensionID != w.extension || got.Status != w.status || got.Reason != w.reason || got.TenantID != "tenant-a" || got.ID == "" {
			t.Fatalf("execution[%d] = %+v, want %+v", i, got, w)
		}
	}
}

// A failing mandatory hook must appear in the tenant audit ledger, not only
// as an anonymous aborted-dispatch marker. The failure must also prevent
// later hooks from running, including for a redirect response.
func TestCriticalHookFailureRecordsAttributableEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
	}{
		{"upstream_500", http.StatusInternalServerError},
		{"redirect_302", http.StatusFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var criticalCalls, laterCalls atomic.Int32
			critical := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				criticalCalls.Add(1)
				if tc.status == http.StatusFound {
					w.Header().Set("Location", "/other")
				}
				w.WriteHeader(tc.status)
			}))
			defer critical.Close()
			later := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				laterCalls.Add(1)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer later.Close()
			srv := newExtServer()
			enableForTest(srv, "critical", "later")
			srv.hooks["on_checkpoint"] = []*Hook{
				{Event: "on_checkpoint", ExtensionID: "critical", Endpoint: critical.URL, FallbackPolicy: "fail_closed"},
				{Event: "on_checkpoint", ExtensionID: "later", Endpoint: later.URL, FallbackPolicy: "continue"},
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/hooks/dispatch", bytes.NewBufferString(`{"event":"on_checkpoint"}`))
			req.Header.Set("X-SCOS-Tenant-ID", "tenant-a")
			req.Header.Set("X-SCOS-Principal-ID", "principal-a")
			res := httptest.NewRecorder()
			srv.handleHookDispatch(res, req)
			if res.Code != http.StatusServiceUnavailable || criticalCalls.Load() != 1 || laterCalls.Load() != 0 {
				t.Fatalf("fail-closed status=%d critical=%d later=%d", res.Code, criticalCalls.Load(), laterCalls.Load())
			}
			if len(srv.executions) != 2 {
				t.Fatalf("missing critical failure record: %+v", srv.executions)
			}
			if e := srv.executions[0]; e.ExtensionID != "critical" || e.Status != "failed" || e.Reason != "fallback_fail_closed" || e.TenantID != "tenant-a" || e.ID == "" {
				t.Fatalf("unattributed critical failure: %+v", e)
			}
			if e := srv.executions[1]; e.ExtensionID != "" || e.Status != "aborted" || e.TenantID != "tenant-a" {
				t.Fatalf("missing abort marker: %+v", e)
			}
		})
	}
}
