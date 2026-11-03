package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// A failed execution-ledger write must remain available to the process for
// operator forensics, but must not be represented to tenants as durable audit
// evidence, even after an unrelated later write succeeds.
func TestFailedHookExecutionNeverBecomesDurableAuditOnLaterWrite(t *testing.T) {
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer endpoint.Close()

	store := filepath.Join(t.TempDir(), "state.json")
	srv := newExtServerAt(store)
	enableForTest(srv, "audit-ext")
	srv.hooks["on_checkpoint"] = []*Hook{{
		Event: "on_checkpoint", ExtensionID: "audit-ext",
		Endpoint: endpoint.URL, FallbackPolicy: "fail_closed",
	}}
	dispatch := func() int {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/hooks/dispatch", bytes.NewBufferString(`{"event":"on_checkpoint"}`))
		req.Header.Set("X-SCOS-Tenant-ID", "tenant-a")
		req.Header.Set("X-SCOS-Principal-ID", "principal-a")
		res := httptest.NewRecorder()
		srv.handleHookDispatch(res, req)
		return res.Code
	}
	list := func(s *extServer) []HookExecution {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/v1/hooks/executions", nil)
		req.Header.Set("X-SCOS-Tenant-ID", "tenant-a")
		req.Header.Set("X-SCOS-Principal-ID", "principal-a")
		res := httptest.NewRecorder()
		s.handleHookExecutions(res, req)
		if res.Code != http.StatusOK {
			t.Fatalf("audit GET status=%d body=%s", res.Code, res.Body.String())
		}
		var out []HookExecution
		if err := json.Unmarshal(res.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	srv.persistHookExecutions = func(context.Context) error { return errors.New("injected failed audit commit") }
	if status := dispatch(); status != http.StatusServiceUnavailable {
		t.Fatalf("failed audit commit status=%d", status)
	}
	if len(srv.executions) != 1 {
		t.Fatalf("in-memory side effect forensic record lost: %+v", srv.executions)
	}
	failedID := srv.executions[0].ID
	if got := list(srv); len(got) != 0 {
		t.Fatalf("uncommitted execution exposed as durable: %+v", got)
	}

	srv.persistHookExecutions = nil
	if status := dispatch(); status != http.StatusOK {
		t.Fatalf("second dispatch status=%d", status)
	}
	got := list(srv)
	if len(got) != 1 || got[0].ID == failedID || got[0].Status != "succeeded" {
		t.Fatalf("audit must contain only later committed execution, got %+v", got)
	}
	restarted := newExtServerAt(store)
	restored := list(restarted)
	if len(restored) != 1 || restored[0].ID != got[0].ID {
		t.Fatalf("restart resurrected failed execution: %+v", restored)
	}
}

// A failed new record must not evict previously committed audit history when
// the bounded ledger is full. Otherwise a later successful persistence would
// permanently discard a valid audit record.
func TestFailedHookExecutionDoesNotEvictCommittedAuditHistory(t *testing.T) {
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer endpoint.Close()
	store := filepath.Join(t.TempDir(), "state.json")
	srv := newExtServerAt(store)
	enableForTest(srv, "audit-ext")
	srv.hooks["on_checkpoint"] = []*Hook{{Event: "on_checkpoint", ExtensionID: "audit-ext", Endpoint: endpoint.URL, FallbackPolicy: "fail_closed"}}
	for i := 0; i < maxHookExecutions; i++ {
		srv.executions = append(srv.executions, &HookExecution{ID: fmt.Sprintf("prior-%d", i), TenantID: "tenant-a", Event: "on_checkpoint", Status: "succeeded"})
	}
	if err := srv.persist(context.Background()); err != nil {
		t.Fatalf("seed durable audit history: %v", err)
	}
	srv.persistHookExecutions = func(context.Context) error { return errors.New("injected failed audit commit") }
	req := httptest.NewRequest(http.MethodPost, "/v1/hooks/dispatch", bytes.NewBufferString(`{"event":"on_checkpoint"}`))
	req.Header.Set("X-SCOS-Tenant-ID", "tenant-a")
	req.Header.Set("X-SCOS-Principal-ID", "principal-a")
	res := httptest.NewRecorder()
	srv.handleHookDispatch(res, req)
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("failed audit commit status=%d", res.Code)
	}
	read := httptest.NewRequest(http.MethodGet, "/v1/hooks/executions?limit=2000", nil)
	read.Header.Set("X-SCOS-Tenant-ID", "tenant-a")
	read.Header.Set("X-SCOS-Principal-ID", "principal-a")
	got := httptest.NewRecorder()
	srv.handleHookExecutions(got, read)
	var records []HookExecution
	if err := json.Unmarshal(got.Body.Bytes(), &records); err != nil {
		t.Fatal(err)
	}
	if len(records) != maxHookExecutions || records[len(records)-1].ID != "prior-0" {
		t.Fatalf("failed append evicted previously committed audit history: count=%d oldest=%+v", len(records), records[len(records)-1])
	}
	restarted := newExtServerAt(store)
	if len(restarted.executions) != maxHookExecutions || restarted.executions[0].ID != "prior-0" {
		t.Fatalf("failed append altered persisted audit history: count=%d", len(restarted.executions))
	}
}

func TestHookExecutionAuditFailsClosedWhenPersistenceOutcomeUnknown(t *testing.T) {
	srv := newTestExtServer()
	srv.executions = []*HookExecution{{ID: "hookexec-1", TenantID: "tenant-a", Status: "succeeded"}}
	srv.persistenceUncertain.Store(true)
	req := httptest.NewRequest(http.MethodGet, "/v1/hooks/executions", nil)
	req.Header.Set("X-SCOS-Tenant-ID", "tenant-a")
	req.Header.Set("X-SCOS-Principal-ID", "principal-a")
	res := httptest.NewRecorder()
	srv.handleHookExecutions(res, req)
	if res.Code != http.StatusServiceUnavailable || bytes.Contains(res.Body.Bytes(), []byte("hookexec-1")) {
		t.Fatalf("uncertain persistence exposed purportedly durable audit: status=%d body=%q", res.Code, res.Body.String())
	}
}
