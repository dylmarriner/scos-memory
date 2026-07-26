package main

import (
	"testing"
)

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
