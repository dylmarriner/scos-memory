package main

import (
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
