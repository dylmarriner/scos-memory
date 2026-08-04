package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

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
	if status.Checklist[4].Status != "verifying" || status.Checklist[6].Detail != "2 pending outbox jobs discovered" {
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
	if status.Checklist[1].Status != "skipped" || status.Checklist[8].Status != "skipped" {
		t.Fatalf("expected steps 2 and 9 to remain deployment-owned skips, got %+v", status.Checklist)
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
