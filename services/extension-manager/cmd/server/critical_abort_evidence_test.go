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
	if len(srv.executions) != 3 {
		t.Fatalf("critical abort lost earlier per-hook evidence: %+v", srv.executions)
	}
	want := []struct{ extension, status, reason string }{
		{"first", "succeeded", ""},
		{"invalid", "failed", "invalid_endpoint"},
		{"", "aborted", "critical hook critical failed"},
	}
	for i, w := range want {
		got := srv.executions[i]
		if got.ExtensionID != w.extension || got.Status != w.status || got.Reason != w.reason || got.TenantID != "tenant-a" || got.ID == "" {
			t.Fatalf("execution[%d] = %+v, want %+v", i, got, w)
		}
	}
}
