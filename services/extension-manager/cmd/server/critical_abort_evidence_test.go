package main

import (
	"bytes"
	"encoding/json"
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
	srv := newTestExtServer()
	enableForTest(srv, "first", "invalid", "critical")
	srv.hooks["on_checkpoint"] = []*Hook{
		{Event: "on_checkpoint", ExtensionID: "first", Endpoint: first.URL, FallbackPolicy: "continue"},
		{Event: "on_checkpoint", ExtensionID: "invalid", Endpoint: "http://example.com/unsafe", FallbackPolicy: "continue"},
		{Event: "on_checkpoint", ExtensionID: "critical", Version: "1.0.0", Endpoint: critical.URL, FallbackPolicy: "fail_closed"},
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
			srv := newTestExtServer()
			enableForTest(srv, "critical", "later")
			srv.hooks["on_checkpoint"] = []*Hook{
				{Event: "on_checkpoint", ExtensionID: "critical", Version: "1.0.0", Endpoint: critical.URL, FallbackPolicy: "fail_closed"},
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
			b, _ := json.Marshal(srv.executions[0])
			var record map[string]any
			if err := json.Unmarshal(b, &record); err != nil {
				t.Fatal(err)
			}
			if record["version"] != "1.0.0" || record["channel"] != "stable" {
				t.Fatalf("critical failure route missing: %v", record)
			}
			if e := srv.executions[1]; e.ExtensionID != "" || e.Status != "aborted" || e.TenantID != "tenant-a" {
				t.Fatalf("missing abort marker: %+v", e)
			}
		})
	}
}

// A blocked canary must be distinguishable from the stable hook in the
// tenant-scoped audit ledger, including through the readback API.
func TestCanaryCriticalPreflightPersistsSelectedRoute(t *testing.T) {
	var stableCalls atomic.Int32
	stable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		stableCalls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer stable.Close()
	srv := newTestExtServer()
	enableForTest(srv, "critical")
	srv.hooks["on_checkpoint"] = []*Hook{{
		Event: "on_checkpoint", ExtensionID: "critical", Version: "1.0.0",
		Endpoint: stable.URL, FallbackPolicy: "fail_closed",
	}}
	srv.canaries["critical"] = &CanaryDeployment{
		PackageID: "critical", Status: "active",
		Scope: CanaryScope{AgentIDs: []string{"agent-in"}},
		Hooks: map[string]*Hook{"on_checkpoint": {
			Event: "on_checkpoint", ExtensionID: "critical", Version: "2.0.0",
			Endpoint: "http://example.com/unsafe", FallbackPolicy: "fail_closed",
		}},
	}
	dispatch := func(agent string) int {
		body := []byte(`{"event":"on_checkpoint","agent_id":"` + agent + `"}`)
		req := httptest.NewRequest(http.MethodPost, "/v1/hooks/dispatch", bytes.NewReader(body))
		req.Header.Set("X-SCOS-Tenant-ID", "tenant-a")
		req.Header.Set("X-SCOS-Principal-ID", "principal-a")
		res := httptest.NewRecorder()
		srv.handleHookDispatch(res, req)
		return res.Code
	}
	if code := dispatch("agent-in"); code != http.StatusServiceUnavailable || stableCalls.Load() != 0 {
		t.Fatalf("canary preflight must fail before stable delivery: status=%d calls=%d", code, stableCalls.Load())
	}
	if code := dispatch("agent-out"); code != http.StatusOK || stableCalls.Load() != 1 {
		t.Fatalf("stable route must still work: status=%d calls=%d", code, stableCalls.Load())
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/hooks/executions?event=on_checkpoint", nil)
	req.Header.Set("X-SCOS-Tenant-ID", "tenant-a")
	req.Header.Set("X-SCOS-Principal-ID", "principal-a")
	res := httptest.NewRecorder()
	srv.handleHookExecutions(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("readback status=%d", res.Code)
	}
	var events []map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &events); err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("expected stable success, canary failure and abort: %+v", events)
	}
	if events[0]["status"] != "succeeded" || events[0]["version"] != "1.0.0" || events[0]["channel"] != "stable" {
		t.Fatalf("stable audit route missing: %+v", events[0])
	}
	if events[2]["status"] != "failed" || events[2]["extension_id"] != "critical" || events[2]["version"] != "2.0.0" || events[2]["channel"] != "canary" {
		t.Fatalf("canary audit route missing: %+v", events[2])
	}
	for _, event := range events {
		if event["tenant_id"] != "tenant-a" {
			t.Fatalf("cross-tenant audit row: %+v", event)
		}
	}
}
