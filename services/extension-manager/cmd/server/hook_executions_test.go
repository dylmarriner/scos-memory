package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHookExecutionsAreRecordedPerTenant(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer ok.Close()
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) }))
	defer failing.Close()

	srv := newExtServer()
	enableForTest(srv, "good", "bad")
	srv.hooks["turn.completed"] = []*Hook{
		{Event: "turn.completed", ExtensionID: "good", Endpoint: ok.URL, TimeoutMs: 1000, FallbackPolicy: "continue"},
		{Event: "turn.completed", ExtensionID: "bad", Endpoint: failing.URL, TimeoutMs: 1000, FallbackPolicy: "continue"},
	}
	dispatch := func(tenant string) {
		req := httptest.NewRequest(http.MethodPost, "/v1/hooks/dispatch", bytes.NewBufferString(`{"event":"turn.completed","payload":{}}`))
		req.Header.Set("X-SCOS-Tenant-ID", tenant)
		req.Header.Set("X-SCOS-Principal-ID", "p")
		res := httptest.NewRecorder()
		srv.handleHookDispatch(res, req)
		if res.Code != http.StatusOK {
			t.Fatalf("dispatch status %d: %s", res.Code, res.Body.String())
		}
	}
	dispatch("tenant-a")
	dispatch("tenant-b")

	list := func(tenant, query string) []HookExecution {
		req := httptest.NewRequest(http.MethodGet, "/v1/hooks/executions"+query, nil)
		req.Header.Set("X-SCOS-Tenant-ID", tenant)
		req.Header.Set("X-SCOS-Principal-ID", "p")
		res := httptest.NewRecorder()
		srv.handleHookExecutions(res, req)
		if res.Code != http.StatusOK {
			t.Fatalf("list status %d: %s", res.Code, res.Body.String())
		}
		var out []HookExecution
		if err := json.Unmarshal(res.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	got := list("tenant-a", "")
	if len(got) != 2 {
		t.Fatalf("tenant-a sees %d executions, want 2 (its own only)", len(got))
	}
	statuses := map[string]string{}
	for _, e := range got {
		if e.TenantID != "tenant-a" || e.ID == "" || e.StartedAt == "" {
			t.Fatalf("bad execution %+v", e)
		}
		statuses[e.ExtensionID] = e.Status
	}
	if statuses["good"] != "succeeded" || statuses["bad"] != "failed" {
		t.Fatalf("statuses = %v", statuses)
	}
	if n := len(list("tenant-a", "?event=other")); n != 0 {
		t.Fatalf("event filter returned %d", n)
	}
	if n := len(list("tenant-b", "?limit=1")); n != 1 {
		t.Fatalf("limit returned %d", n)
	}
}
