package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Simulate an audit write that committed to durable storage but whose acknowledgement
// could not be verified. Restart must not convert that unverified attempt into
// tenant-visible committed evidence.
func TestHookAuditUncertainAcknowledgementNeverBecomesCommittedOnRestart(t *testing.T) {
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer endpoint.Close()
	store := filepath.Join(t.TempDir(), "extension-state.json")
	srv := newExtServerAt(store)
	enableForTest(srv, "audit-ext")
	srv.hooks["on_checkpoint"] = []*Hook{{Event: "on_checkpoint", ExtensionID: "audit-ext", Endpoint: endpoint.URL, FallbackPolicy: "fail_closed"}}
	srv.persistHookExecutions = func(ctx context.Context) error {
		if err := srv.persist(ctx); err != nil {
			return err
		}
		return errPersistenceOutcomeUnknown // acknowledgement lost, readback unavailable
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/hooks/dispatch", bytes.NewBufferString(`{"event":"on_checkpoint"}`))
	req.Header.Set("X-SCOS-Tenant-ID", "tenant-a")
	req.Header.Set("X-SCOS-Principal-ID", "principal-a")
	res := httptest.NewRecorder()
	srv.handleHookDispatch(res, req)
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("unverified dispatch status=%d", res.Code)
	}
	raw, err := os.ReadFile(store)
	if err != nil {
		t.Fatal(err)
	}
	var legacy struct {
		Executions []json.RawMessage `json:"hook_executions"`
	}
	if err := json.Unmarshal(raw, &legacy); err != nil {
		t.Fatal(err)
	}
	if len(legacy.Executions) != 0 || !bytes.Contains(raw, []byte(`"pending_hook_executions"`)) {
		t.Fatalf("staged audit visible to legacy reader: %s", raw)
	}
	restarted := newExtServerAt(store)
	read := httptest.NewRequest(http.MethodGet, "/v1/hooks/executions", nil)
	read.Header.Set("X-SCOS-Tenant-ID", "tenant-a")
	read.Header.Set("X-SCOS-Principal-ID", "principal-a")
	got := httptest.NewRecorder()
	restarted.handleHookExecutions(got, read)
	if got.Code != http.StatusOK || bytes.TrimSpace(got.Body.Bytes()) == nil || string(bytes.TrimSpace(got.Body.Bytes())) != "[]" {
		t.Fatalf("unverified audit resurrected on restart: status=%d body=%s", got.Code, got.Body.String())
	}
}

// Failure to promote a staged execution must leave restart-loaded audit
// history unchanged, even if the first staging write was durable.
func TestHookAuditPromotionFailureDoesNotExposeStagedExecutionAfterRestart(t *testing.T) {
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer endpoint.Close()
	store := filepath.Join(t.TempDir(), "extension-state.json")
	srv := newExtServerAt(store)
	enableForTest(srv, "audit-ext")
	srv.hooks["on_checkpoint"] = []*Hook{{Event: "on_checkpoint", ExtensionID: "audit-ext", Endpoint: endpoint.URL, FallbackPolicy: "fail_closed"}}
	calls := 0
	srv.persistHookExecutions = func(ctx context.Context) error {
		calls++
		if calls == 1 {
			return srv.persist(ctx)
		}
		return errors.New("confirmation write failed")
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/hooks/dispatch", bytes.NewBufferString(`{"event":"on_checkpoint"}`))
	req.Header.Set("X-SCOS-Tenant-ID", "tenant-a")
	req.Header.Set("X-SCOS-Principal-ID", "principal-a")
	res := httptest.NewRecorder()
	srv.handleHookDispatch(res, req)
	if res.Code != http.StatusServiceUnavailable || calls != 2 {
		t.Fatalf("confirmation failure not rejected: status=%d writes=%d", res.Code, calls)
	}
	restarted := newExtServerAt(store)
	read := httptest.NewRequest(http.MethodGet, "/v1/hooks/executions", nil)
	read.Header.Set("X-SCOS-Tenant-ID", "tenant-a")
	read.Header.Set("X-SCOS-Principal-ID", "principal-a")
	got := httptest.NewRecorder()
	restarted.handleHookExecutions(got, read)
	if got.Code != http.StatusOK || string(bytes.TrimSpace(got.Body.Bytes())) != "[]" {
		t.Fatalf("unconfirmed audit exposed after restart: status=%d body=%s", got.Code, got.Body.String())
	}
}

// Staging an unverified audit append must not evict any of the existing
// committed bounded history, including across a process restart.
func TestHookAuditUncertainStagingPreservesFullCommittedHistory(t *testing.T) {
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer endpoint.Close()
	store := filepath.Join(t.TempDir(), "extension-state.json")
	srv := newExtServerAt(store)
	enableForTest(srv, "audit-ext")
	srv.hooks["on_checkpoint"] = []*Hook{{Event: "on_checkpoint", ExtensionID: "audit-ext", Endpoint: endpoint.URL, FallbackPolicy: "fail_closed"}}
	for i := 0; i < maxHookExecutions; i++ {
		srv.executions = append(srv.executions, &HookExecution{
			ID: fmt.Sprintf("prior-%d", i), Event: "on_checkpoint", TenantID: "tenant-a", Status: "succeeded",
		})
	}
	if err := srv.persist(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv.persistHookExecutions = func(ctx context.Context) error {
		if err := srv.persist(ctx); err != nil {
			return err
		}
		return errPersistenceOutcomeUnknown
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/hooks/dispatch", bytes.NewBufferString(`{"event":"on_checkpoint"}`))
	req.Header.Set("X-SCOS-Tenant-ID", "tenant-a")
	req.Header.Set("X-SCOS-Principal-ID", "principal-a")
	res := httptest.NewRecorder()
	srv.handleHookDispatch(res, req)
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("unverified status=%d", res.Code)
	}
	restarted := newExtServerAt(store)
	read := httptest.NewRequest(http.MethodGet, "/v1/hooks/executions?limit=2000", nil)
	read.Header.Set("X-SCOS-Tenant-ID", "tenant-a")
	read.Header.Set("X-SCOS-Principal-ID", "principal-a")
	got := httptest.NewRecorder()
	restarted.handleHookExecutions(got, read)
	if got.Code != http.StatusOK {
		t.Fatalf("read status=%d", got.Code)
	}
	var records []HookExecution
	if err := json.Unmarshal(got.Body.Bytes(), &records); err != nil {
		t.Fatal(err)
	}
	if len(records) != maxHookExecutions {
		t.Fatalf("staged audit changed durable history count=%d", len(records))
	}
	if records[0].ID != fmt.Sprintf("prior-%d", maxHookExecutions-1) || records[len(records)-1].ID != "prior-0" {
		t.Fatalf("staged audit evicted durable history: count=%d first=%+v last=%+v", len(records), records[0], records[len(records)-1])
	}
}

// PostgreSQL integration: the INSERT can commit while both the acknowledgement
// and its follow-up readback are lost. A fresh process loading that committed
// row must still treat the staged execution as unverified. Only a dedicated
// loopback test DB is accepted by extensionOperatorTestDB.
func TestHookAuditCommittedUnknownDBWriteStaysPendingAfterRestart(t *testing.T) {
	pool, dsn := extensionOperatorTestDB(t)
	verifier, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer verifier.Close()
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer endpoint.Close()
	srv := newExtServerAt("")
	srv.db = pool
	enableForTest(srv, "audit-ext")
	srv.hooks["on_checkpoint"] = []*Hook{{Event: "on_checkpoint", ExtensionID: "audit-ext", Endpoint: endpoint.URL, FallbackPolicy: "fail_closed"}}
	srv.persistAfterDBWrite = func(context.Context) error {
		srv.mu.RLock()
		pending := len(srv.executions) > 0 && srv.executions[len(srv.executions)-1].AuditPending
		srv.mu.RUnlock()
		if pending {
			pool.Close() // only the test pool; independent verifier remains online
			return errors.New("lost acknowledgement and database readback")
		}
		return nil
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/hooks/dispatch", bytes.NewBufferString(`{"event":"on_checkpoint"}`))
	req.Header.Set("X-SCOS-Tenant-ID", "tenant-a")
	req.Header.Set("X-SCOS-Principal-ID", "principal-a")
	res := httptest.NewRecorder()
	srv.handleHookDispatch(res, req)
	if res.Code != http.StatusServiceUnavailable || !srv.persistenceUncertain.Load() {
		t.Fatalf("unverified DB commit was acknowledged: status=%d uncertain=%t", res.Code, srv.persistenceUncertain.Load())
	}
	restarted := newExtServerAt("")
	restarted.db = verifier
	if err := restarted.loadState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(restarted.executions) != 1 || !restarted.executions[0].AuditPending {
		t.Fatalf("committed unknown write lost its durable pending marker: %+v", restarted.executions)
	}
	read := httptest.NewRequest(http.MethodGet, "/v1/hooks/executions", nil)
	read.Header.Set("X-SCOS-Tenant-ID", "tenant-a")
	read.Header.Set("X-SCOS-Principal-ID", "principal-a")
	got := httptest.NewRecorder()
	restarted.handleHookExecutions(got, read)
	if got.Code != http.StatusOK || string(bytes.TrimSpace(got.Body.Bytes())) != "[]" {
		t.Fatalf("restart exposed unverified DB execution: status=%d body=%s", got.Code, got.Body.String())
	}
}
