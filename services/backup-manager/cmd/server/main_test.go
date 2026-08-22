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

const backupTestSecret = "backup-manager-test-secret"

func backupBearer(t *testing.T, principal, tenant string, scopes ...string) string {
	t.Helper()
	token, err := serviceauth.Issue(serviceauth.Identity{
		Principal: principal,
		Tenant:    tenant,
		Scopes:    scopes,
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}, []byte(backupTestSecret))
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	return token
}

func backupRequest(t *testing.T, mux http.Handler, method, path, bearer string, body any) *httptest.ResponseRecorder {
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

// TestBackupMutationsRequireAdminScope proves that an authenticated caller
// without operator privileges cannot fabricate a "completed"/"passed" backup
// or restore-test record. Before this fix, any bearer token could call
// /v1/backups/complete or /v1/restore-tests/complete with an arbitrary
// status, falsifying verification metadata without ever running a backup.
func TestBackupMutationsRequireAdminScope(t *testing.T) {
	t.Setenv("SCOS_AUTH_ISSUER_SECRET", backupTestSecret)
	s := newBackupServer()
	mux := serviceauth.Require(newMux(s))

	unprivileged := backupBearer(t, "low-priv-agent", "tenant-a")
	operator := backupBearer(t, "sre", "tenant-a", "backups:admin")

	if rec := backupRequest(t, mux, http.MethodPost, "/v1/backups/start", unprivileged, map[string]any{"type": "base_backup"}); rec.Code != http.StatusForbidden {
		t.Fatalf("unprivileged backup start status = %d, want 403, body=%s", rec.Code, rec.Body.String())
	}

	rec := backupRequest(t, mux, http.MethodPost, "/v1/backups/start", operator, map[string]any{"type": "base_backup"})
	if rec.Code != http.StatusOK {
		t.Fatalf("operator backup start status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var b BackupRecord
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("decode backup: %v", err)
	}

	if rec := backupRequest(t, mux, http.MethodPost, "/v1/backups/complete", unprivileged, map[string]any{"backup_id": b.ID, "status": "completed"}); rec.Code != http.StatusForbidden {
		t.Fatalf("unprivileged backup complete status = %d, want 403, body=%s", rec.Code, rec.Body.String())
	}

	rec = backupRequest(t, mux, http.MethodPost, "/v1/backups/complete", operator, map[string]any{"backup_id": b.ID, "status": "completed"})
	if rec.Code != http.StatusOK {
		t.Fatalf("operator backup complete status = %d, body=%s", rec.Code, rec.Body.String())
	}

	if rec := backupRequest(t, mux, http.MethodPost, "/v1/restore-tests/start", unprivileged, map[string]any{"test_type": "monthly_restore", "backup_id": b.ID}); rec.Code != http.StatusForbidden {
		t.Fatalf("unprivileged restore-test start status = %d, want 403, body=%s", rec.Code, rec.Body.String())
	}

	rec = backupRequest(t, mux, http.MethodPost, "/v1/restore-tests/start", operator, map[string]any{"test_type": "monthly_restore", "backup_id": b.ID})
	if rec.Code != http.StatusOK {
		t.Fatalf("operator restore-test start status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var rt RestoreTestResult
	if err := json.Unmarshal(rec.Body.Bytes(), &rt); err != nil {
		t.Fatalf("decode restore test: %v", err)
	}

	if rec := backupRequest(t, mux, http.MethodPost, "/v1/restore-tests/complete", unprivileged, map[string]any{"test_id": rt.ID, "status": "passed"}); rec.Code != http.StatusForbidden {
		t.Fatalf("unprivileged restore-test complete status = %d, want 403, body=%s", rec.Code, rec.Body.String())
	}
}

func TestNewBackupServer(t *testing.T) {
	s := newBackupServer()
	if s == nil {
		t.Fatal("expected non-nil server")
	}
}

func TestDefaultBackupPlan(t *testing.T) {
	s := newBackupServer()
	if !s.plan.PostgreSQLWAL {
		t.Fatal("expected WAL archiving enabled")
	}
	if !s.plan.BaseBackups {
		t.Fatal("expected base backups enabled")
	}
	if !s.plan.EncryptedOffHost {
		t.Fatal("expected encrypted off-host enabled")
	}
}

func TestDefaultRetention(t *testing.T) {
	s := newBackupServer()
	if s.retention.WALAndDaily != 35 {
		t.Fatal("expected 35 days WAL/daily retention")
	}
	if s.retention.Weekly != 12 {
		t.Fatal("expected 12 weeks weekly retention")
	}
	if s.retention.Monthly != 12 {
		t.Fatal("expected 12 months monthly retention")
	}
	if !s.retention.OKFIndefinite {
		t.Fatal("expected OKF indefinite retention")
	}
}

func TestBackupLifecycle(t *testing.T) {
	s := newBackupServer()
	s.mu.Lock()
	b := &BackupRecord{
		ID: "backup-1", Type: "base_backup",
		Status: "running", Encrypted: true,
	}
	s.backups["backup-1"] = b
	s.mu.Unlock()

	if b.Status != "running" {
		t.Fatal("expected running status")
	}

	s.mu.Lock()
	b.Status = "completed"
	b.SizeBytes = 1048576
	b.Location = "s3://backups/base_backup_2026.tar.gz"
	s.mu.Unlock()

	if b.Status != "completed" {
		t.Fatal("expected completed status")
	}
	if b.SizeBytes != 1048576 {
		t.Fatal("size mismatch")
	}
}

func TestBackupFailed(t *testing.T) {
	s := newBackupServer()
	s.mu.Lock()
	s.backups["backup-2"] = &BackupRecord{
		ID: "backup-2", Type: "wal_archive",
		Status: "failed",
	}
	s.mu.Unlock()

	if s.backups["backup-2"].Status != "failed" {
		t.Fatal("expected failed status")
	}
}

func TestRestoreTestLifecycle(t *testing.T) {
	s := newBackupServer()
	s.mu.Lock()
	test := &RestoreTestResult{
		ID: "restore-test-1", TestType: "monthly_restore",
		BackupID: "backup-1", Status: "running",
	}
	s.restoreTests["restore-test-1"] = test
	s.mu.Unlock()

	if test.Status != "running" {
		t.Fatal("expected running status")
	}

	s.mu.Lock()
	test.Status = "passed"
	test.Detail = "All data verified"
	s.mu.Unlock()

	if test.Status != "passed" {
		t.Fatal("expected passed status")
	}
}

func TestRestoreTestFailed(t *testing.T) {
	s := newBackupServer()
	s.mu.Lock()
	s.restoreTests["restore-test-2"] = &RestoreTestResult{
		ID: "restore-test-2", TestType: "automated_integrity",
		Status: "failed", Detail: "checksum mismatch",
	}
	s.mu.Unlock()

	rt := s.restoreTests["restore-test-2"]
	if rt.Status != "failed" {
		t.Fatal("expected failed status")
	}
}

func TestBackupTypes(t *testing.T) {
	s := newBackupServer()
	s.mu.Lock()
	s.backups["b1"] = &BackupRecord{ID: "b1", Type: "base_backup", Status: "completed"}
	s.backups["b2"] = &BackupRecord{ID: "b2", Type: "wal_archive", Status: "completed"}
	s.backups["b3"] = &BackupRecord{ID: "b3", Type: "schema_export", Status: "completed"}
	s.backups["b4"] = &BackupRecord{ID: "b4", Type: "config_backup", Status: "completed"}
	s.mu.Unlock()

	if len(s.backups) != 4 {
		t.Fatal("expected 4 backups")
	}
}

func TestEmptyBackupsList(t *testing.T) {
	s := newBackupServer()
	s.mu.RLock()
	count := len(s.backups)
	s.mu.RUnlock()
	if count != 0 {
		t.Fatal("expected empty backups")
	}
}

func TestEmptyRestoreTestsList(t *testing.T) {
	s := newBackupServer()
	s.mu.RLock()
	count := len(s.restoreTests)
	s.mu.RUnlock()
	if count != 0 {
		t.Fatal("expected empty restore tests")
	}
}
