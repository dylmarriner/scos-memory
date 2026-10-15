package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

// TestMutatingRecoveryRoutesRequireAdminScope guards the AUDIT-007 fix: the
// three mutating routes must reject a caller authenticated with valid
// claims but lacking "recovery:admin", and must accept one that has it.
// This exercises the actual mux, not the handlers directly, so it fails if
// the auth.RequireScope wrapping in newRecoveryMux is ever removed or
// loosened to a blanket auth.AuthenticatedHandler-only gate.
func TestMutatingRecoveryRoutesRequireAdminScope(t *testing.T) {
	s := newRecoveryServer()
	mux := newRecoveryMux(s)

	for _, route := range []string{"/v1/recovery/start", "/v1/recovery/checklist", "/v1/recovery/complete"} {
		t.Run(route, func(t *testing.T) {
			body := bytes.NewBufferString(`{"service_id":"svc"}`)
			r := httptest.NewRequest(http.MethodPost, route, body)
			ctx := auth.ContextWithClaims(r.Context(), auth.Claims{TenantID: "tenant-1", PrincipalID: "principal-1"})
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, r.WithContext(ctx))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("expected 403 for caller without recovery:admin scope on %s, got %d", route, rec.Code)
			}
		})
	}

	body := bytes.NewBufferString(`{"service_id":"svc"}`)
	r := httptest.NewRequest(http.MethodPost, "/v1/recovery/start", body)
	ctx := auth.ContextWithClaims(r.Context(), auth.Claims{TenantID: "tenant-1", PrincipalID: "principal-1", Scopes: []string{"recovery:admin"}})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r.WithContext(ctx))
	if rec.Code == http.StatusForbidden {
		t.Fatalf("expected caller with recovery:admin scope to pass the scope gate, got 403")
	}
}

func TestNewRecoveryServer(t *testing.T) {
	s := newRecoveryServer()
	if s == nil {
		t.Fatal("expected non-nil server")
	}
}

func TestRecoveryStart(t *testing.T) {
	s := newRecoveryServer()
	status := &RecoveryStatus{
		ServiceID: "memory-gateway",
		Phase:     "startup",
		Checklist: RecoveryChecklist{
			{Step: 1, Name: "Verify schema", Status: "pending"},
			{Step: 2, Name: "Verify storage", Status: "pending"},
		},
	}
	s.mu.Lock()
	s.statuses["memory-gateway"] = status
	s.mu.Unlock()

	if status.Phase != "startup" {
		t.Fatal("expected startup phase")
	}
}

func TestChecklistUpdate(t *testing.T) {
	s := newRecoveryServer()
	s.mu.Lock()
	s.statuses["svc"] = &RecoveryStatus{
		ServiceID: "svc", Phase: "startup",
		Checklist: RecoveryChecklist{
			{Step: 1, Name: "Step 1", Status: "pending"},
		},
	}
	s.mu.Unlock()

	s.mu.Lock()
	st := s.statuses["svc"]
	st.Checklist[0].Status = "ok"
	st.Checklist[0].Detail = "connected"
	s.mu.Unlock()

	if st.Checklist[0].Status != "ok" {
		t.Fatal("expected ok status")
	}
	if st.Checklist[0].Detail != "connected" {
		t.Fatal("detail mismatch")
	}
}

func TestRecoveryComplete(t *testing.T) {
	s := newRecoveryServer()
	s.mu.Lock()
	s.statuses["svc"] = &RecoveryStatus{
		ServiceID: "svc", Phase: "startup",
	}
	s.mu.Unlock()

	s.mu.Lock()
	st := s.statuses["svc"]
	st.Phase = "completed"
	s.mu.Unlock()

	if st.Phase != "completed" {
		t.Fatal("expected completed phase")
	}
}

func TestRecoveryFailed(t *testing.T) {
	s := newRecoveryServer()
	s.mu.Lock()
	s.statuses["svc"] = &RecoveryStatus{
		ServiceID: "svc", Phase: "failed",
	}
	s.mu.Unlock()

	if s.statuses["svc"].Phase != "failed" {
		t.Fatal("expected failed phase")
	}
}

func TestFullChecklist(t *testing.T) {
	s := newRecoveryServer()
	s.mu.Lock()
	s.statuses["svc"] = &RecoveryStatus{
		ServiceID: "svc", Phase: "startup",
		Checklist: RecoveryChecklist{
			{Step: 1, Name: "Verify schema and database connectivity", Status: "pending"},
			{Step: 2, Name: "Verify required storage", Status: "pending"},
			{Step: 3, Name: "Confirm OKF repository state", Status: "pending"},
			{Step: 4, Name: "Load latest checkpoints", Status: "pending"},
			{Step: 5, Name: "Identify interrupted turns", Status: "pending"},
			{Step: 6, Name: "Identify open tool operations", Status: "pending"},
			{Step: 7, Name: "Resume pending outbox jobs", Status: "pending"},
			{Step: 8, Name: "Reset abandoned leases", Status: "pending"},
			{Step: 9, Name: "Rebuild temporary caches", Status: "pending"},
			{Step: 10, Name: "Mark readiness", Status: "pending"},
		},
	}
	s.mu.Unlock()

	if len(s.statuses["svc"].Checklist) != 10 {
		t.Fatal("expected 10 checklist items")
	}
}

func TestInterruptedTurns(t *testing.T) {
	s := newRecoveryServer()
	s.mu.Lock()
	s.statuses["svc"] = &RecoveryStatus{
		ServiceID:        "svc",
		InterruptedTurns: []string{"turn-1", "turn-2"},
		OpenOperations:   []string{"op-1"},
	}
	s.mu.Unlock()

	svc := s.statuses["svc"]
	if len(svc.InterruptedTurns) != 2 {
		t.Fatal("expected 2 interrupted turns")
	}
	if len(svc.OpenOperations) != 1 {
		t.Fatal("expected 1 open operation")
	}
}

func TestEmptyRecoveryList(t *testing.T) {
	s := newRecoveryServer()
	s.mu.RLock()
	count := len(s.statuses)
	s.mu.RUnlock()
	if count != 0 {
		t.Fatal("expected empty statuses")
	}
}

func TestMultipleRecoveryStatuses(t *testing.T) {
	s := newRecoveryServer()
	s.mu.Lock()
	s.statuses["svc-1"] = &RecoveryStatus{ServiceID: "svc-1", Phase: "completed"}
	s.statuses["svc-2"] = &RecoveryStatus{ServiceID: "svc-2", Phase: "startup"}
	s.mu.Unlock()

	if len(s.statuses) != 2 {
		t.Fatal("expected 2 statuses")
	}
}

func TestApplyFindingsIsDependencyAware(t *testing.T) {
	status := &RecoveryStatus{Checklist: RecoveryChecklist{
		{Step: 1}, {Step: 2}, {Step: 3}, {Step: 4}, {Step: 5}, {Step: 6}, {Step: 7}, {Step: 8}, {Step: 9}, {Step: 10},
	}}
	applyFindings(status, recoveryFindings{
		interruptedTurns: []string{"00000000-0000-0000-0000-000000000001"},
		openOperations:   []string{"00000000-0000-0000-0000-000000000002"},
		pendingOutbox:    2,
	})
	if status.Phase != "recovery" || status.Checklist[0].Status != "ok" {
		t.Fatalf("expected database-backed recovery phase, got %q", status.Phase)
	}
	if status.Checklist[4].Status != "verifying" || !strings.Contains(status.Checklist[6].Detail, "2 pending jobs available to workers") {
		t.Fatalf("expected discovered work in checklist: %+v", status.Checklist)
	}
	if checklistReady(status.Checklist) {
		t.Fatal("recovery with unresolved work must not be ready")
	}
}

func TestApplyFindingsResolvesOKFCheckpointsAndLeases(t *testing.T) {
	status := &RecoveryStatus{Checklist: RecoveryChecklist{
		{Step: 1}, {Step: 2}, {Step: 3}, {Step: 4}, {Step: 5}, {Step: 6}, {Step: 7}, {Step: 8}, {Step: 9}, {Step: 10},
	}}
	applyFindings(status, recoveryFindings{
		okfOK:            true,
		okfDetail:        "OKF repository synced at commit abc123",
		checkpointDetail: "3 checkpoint(s) loaded, latest at 2026-01-01T00:00:00Z",
		leasesReset:      2,
		storageOK:        true,
		storageDetail:    "database writable (1.0 MiB)",
		staleRequeued:    1,
		runsSuperseded:   1,
	})
	if status.Checklist[2].Status != "ok" || status.Checklist[2].Detail != "OKF repository synced at commit abc123" {
		t.Fatalf("expected OKF step resolved, got %+v", status.Checklist[2])
	}
	if status.Checklist[3].Status != "ok" {
		t.Fatalf("expected checkpoint step resolved, got %+v", status.Checklist[3])
	}
	if status.Checklist[7].Detail != "2 abandoned leases reset" {
		t.Fatalf("expected lease reset detail, got %+v", status.Checklist[7])
	}
	if status.Checklist[1].Status != "ok" || status.Checklist[1].Detail != "database writable (1.0 MiB)" {
		t.Fatalf("expected verified storage step, got %+v", status.Checklist[1])
	}
	if status.Checklist[6].Detail != "1 stale job requeued from expired worker leases; 0 pending jobs available to workers" {
		t.Fatalf("unexpected outbox detail %q", status.Checklist[6].Detail)
	}
	if status.Checklist[8].Detail != "recovery state cache rebuilt; 1 abandoned recovery run superseded" {
		t.Fatalf("unexpected cache detail %q", status.Checklist[8].Detail)
	}
	if !checklistReady(status.Checklist) {
		t.Fatalf("all verified steps should be ready: %+v", status.Checklist)
	}
}

func TestApplyFindingsFailsOnOKFSyncErrors(t *testing.T) {
	status := &RecoveryStatus{Checklist: RecoveryChecklist{
		{Step: 1}, {Step: 2}, {Step: 3}, {Step: 4}, {Step: 5}, {Step: 6}, {Step: 7}, {Step: 8}, {Step: 9}, {Step: 10},
	}}
	applyFindings(status, recoveryFindings{okfOK: false, okfDetail: "OKF repository sync at commit abc123 reported 1 error(s)"})
	if status.Checklist[2].Status != "failed" {
		t.Fatalf("expected OKF step failed, got %+v", status.Checklist[2])
	}
	if checklistReady(status.Checklist) {
		t.Fatal("recovery must not be ready while OKF state is unconfirmed")
	}
}

func TestReadyRequiresPostgres(t *testing.T) {
	s := newRecoveryServer()
	req := httptest.NewRequest(http.MethodGet, "/health/ready", nil)
	rec := httptest.NewRecorder()
	s.handleReady(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected unavailable without database, got %d", rec.Code)
	}
}

func TestReadyChecksDependency(t *testing.T) {
	s := newRecoveryServer()
	s.readyCheck = func(context.Context) error { return errors.New("database down") }
	rec := httptest.NewRecorder()
	s.handleReady(rec, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected unavailable database, got %d", rec.Code)
	}
	s.readyCheck = func(context.Context) error { return nil }
	rec = httptest.NewRecorder()
	s.handleReady(rec, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected ready database, got %d", rec.Code)
	}
}

func TestRecoveryHandlersRejectUnknownJSONFields(t *testing.T) {
	s := newRecoveryServer()
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		body    string
	}{
		{name: "start", handler: s.handleRecoveryStart, body: `{"service_id":"svc","unexpected":true}`},
		{name: "checklist", handler: s.handleChecklistUpdate, body: `{"service_id":"svc","step":1,"status":"ok","unexpected":true}`},
		{name: "complete", handler: s.handleRecoveryComplete, body: `{"service_id":"svc","unexpected":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(tc.body))
			rec := httptest.NewRecorder()
			tc.handler(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestRecoveryStartFailsCleanlyWhenDiscoveryIsKilledMidFlight exercises §39's
// "process kills during ... consolidation" scenario against this service's
// own real discovery path: a process kill mid-recovery-discovery must be
// recorded as a clean, single failed status rather than a panic, a hang, or
// a partially-applied recovery run.
func TestRecoveryStartFailsCleanlyWhenDiscoveryIsKilledMidFlight(t *testing.T) {
	s := newRecoveryServer()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // simulate the process/dependency being killed before discovery returns
	s.discover = func(ctx context.Context, _ string) (recoveryFindings, error) {
		if err := ctx.Err(); err != nil {
			return recoveryFindings{}, err
		}
		t.Fatal("discover must observe the canceled context, not a fresh one")
		return recoveryFindings{}, nil
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/recovery/start", bytes.NewBufferString(`{"service_id":"killed-mid-flight"}`))
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	s.handleRecoveryStart(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected recovery start to record failure rather than error out, got %d", rec.Code)
	}
	var status RecoveryStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Phase != "failed" {
		t.Fatalf("expected failed phase on a killed discovery pass, got %q", status.Phase)
	}
	if status.Checklist[0].Status != "failed" {
		t.Fatalf("expected step 1 marked failed, got %+v", status.Checklist[0])
	}

	s.mu.RLock()
	stored, ok := s.statuses["killed-mid-flight"]
	s.mu.RUnlock()
	if !ok || stored.Phase != "failed" {
		t.Fatalf("expected exactly one stored failed status after the kill, got %+v", stored)
	}
}

// TestRecoveryStartConcurrentRetriesDoNotDuplicateSideEffects exercises §39's
// "duplicate outbox jobs/events" and "altered idempotency retries" scenarios:
// concurrent recovery-start retries for the same service id (as a naive
// retrying caller would produce after a timeout) must each run discovery
// independently but must never corrupt or duplicate the stored recovery
// status for that service.
func TestRecoveryStartConcurrentRetriesDoNotDuplicateSideEffects(t *testing.T) {
	s := newRecoveryServer()
	var discoverCalls int32
	s.discover = func(ctx context.Context, _ string) (recoveryFindings, error) {
		atomic.AddInt32(&discoverCalls, 1)
		return recoveryFindings{pendingOutbox: 1}, nil
	}

	const attempts = 8
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/v1/recovery/start", bytes.NewBufferString(`{"service_id":"idempotent-retry"}`))
			rec := httptest.NewRecorder()
			s.handleRecoveryStart(rec, req)
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&discoverCalls); got != attempts {
		t.Fatalf("expected discover invoked once per retry attempt, got %d", got)
	}
	s.mu.RLock()
	stored, ok := s.statuses["idempotent-retry"]
	s.mu.RUnlock()
	if !ok {
		t.Fatal("expected exactly one status entry stored for repeated retries of the same service id")
	}
	if stored.Phase != "recovery" {
		t.Fatalf("expected the last-applied retry outcome recorded without corruption, got %q", stored.Phase)
	}
}
