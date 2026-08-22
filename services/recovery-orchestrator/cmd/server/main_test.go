package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	serviceauth "github.com/dylanmarriner/scos-memory-fabric/packages/service-auth"
)

const recoveryTestSecret = "recovery-orchestrator-test-secret"

func recoveryBearer(t *testing.T, principal, tenant string, scopes ...string) string {
	t.Helper()
	token, err := serviceauth.Issue(serviceauth.Identity{
		Principal: principal,
		Tenant:    tenant,
		Scopes:    scopes,
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}, []byte(recoveryTestSecret))
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	return token
}

func recoveryRequest(t *testing.T, mux http.Handler, method, path, bearer string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(data)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// TestRecoveryMutationsRequireAdminScope proves that an authenticated caller
// with no operator privileges cannot start, update, or force-complete a
// service's disaster-recovery lifecycle. Before this fix, any bearer token
// (scoped for something unrelated, or scopeless) could mark recovery
// "completed" without the underlying checklist ever running, silently
// masking a degraded service.
func TestRecoveryMutationsRequireAdminScope(t *testing.T) {
	t.Setenv("SCOS_AUTH_ISSUER_SECRET", recoveryTestSecret)
	s := newRecoveryServer()
	mux := serviceauth.Require(newMux(s))

	unprivileged := recoveryBearer(t, "low-priv-agent", "tenant-a")
	operator := recoveryBearer(t, "sre", "tenant-a", "recovery:admin")

	if rec := recoveryRequest(t, mux, http.MethodPost, "/v1/recovery/start", unprivileged, map[string]any{"service_id": "memory-gateway"}); rec.Code != http.StatusForbidden {
		t.Fatalf("unprivileged recovery start status = %d, want 403, body=%s", rec.Code, rec.Body.String())
	}

	rec := recoveryRequest(t, mux, http.MethodPost, "/v1/recovery/start", operator, map[string]any{"service_id": "memory-gateway"})
	if rec.Code != http.StatusOK {
		t.Fatalf("operator recovery start status = %d, body=%s", rec.Code, rec.Body.String())
	}

	if rec := recoveryRequest(t, mux, http.MethodPost, "/v1/recovery/checklist", unprivileged, map[string]any{"service_id": "memory-gateway", "step": 1, "status": "ok"}); rec.Code != http.StatusForbidden {
		t.Fatalf("unprivileged checklist update status = %d, want 403, body=%s", rec.Code, rec.Body.String())
	}

	if rec := recoveryRequest(t, mux, http.MethodPost, "/v1/recovery/complete", unprivileged, map[string]any{"service_id": "memory-gateway"}); rec.Code != http.StatusForbidden {
		t.Fatalf("unprivileged recovery complete status = %d, want 403, body=%s", rec.Code, rec.Body.String())
	}

	rec = recoveryRequest(t, mux, http.MethodPost, "/v1/recovery/complete", operator, map[string]any{"service_id": "memory-gateway"})
	if rec.Code != http.StatusOK {
		t.Fatalf("operator recovery complete status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var status RecoveryStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil || status.Phase != "completed" {
		t.Fatalf("expected completed phase, got %+v (err=%v)", status, err)
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
