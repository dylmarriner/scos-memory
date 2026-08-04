package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewBackupServer(t *testing.T) {
	s := newBackupServer()
	if s == nil {
		t.Fatal("expected non-nil server")
	}
}

func TestDefaultBackupPlan(t *testing.T) {
	t.Setenv("SCOS_BACKUP_PROVIDER", "local")
	t.Setenv("SCOS_BACKUP_PROVIDER_VERIFIED", "false")
	t.Setenv("SCOS_BACKUP_OFF_HOST_VERIFIED", "false")
	s := newBackupServer()
	if s.plan.PostgreSQLWAL {
		t.Fatal("WAL archiving must not be advertised without a configured provider")
	}
	if !s.plan.BaseBackups {
		t.Fatal("expected base backups enabled")
	}
	if s.plan.EncryptedOffHost {
		t.Fatal("off-host copies must not be advertised without a configured provider")
	}
	if s.plan.PITRConfigured || s.plan.RemoteVerified {
		t.Fatal("PITR and remote coverage must not be advertised without verification")
	}
}

func TestTLSDatabaseURLIsRequired(t *testing.T) {
	for _, raw := range []string{
		"postgres://user:pass@db/scos?sslmode=disable",
		"postgres://user:pass@db/scos",
	} {
		if err := requireTLSDatabaseURL(raw); err == nil {
			t.Fatalf("expected TLS validation failure for %q", raw)
		}
	}
	if err := requireTLSDatabaseURL("postgres://user:pass@db/scos?sslmode=require"); err != nil {
		t.Fatalf("expected TLS URL to pass: %v", err)
	}
}

func TestProductionReadinessRequiresProviderVerificationAndPITR(t *testing.T) {
	t.Setenv("SCOS_BACKUP_PROVIDER", "filesystem")
	t.Setenv("SCOS_BACKUP_DESTINATION", t.TempDir())
	t.Setenv("SCOS_BACKUP_DATABASE_URL", "postgres://user:pass@db/scos?sslmode=require")
	t.Setenv("SCOS_BACKUP_ENCRYPTION_KEY", strings.Repeat("k", 32))
	t.Setenv("SCOS_SECRET_MANAGER", "vault")
	t.Setenv("SCOS_BACKUP_ENCRYPTION_KEY_REF", "secret/scos/backup-key")
	s := newBackupServerWithDir(t.TempDir())
	if err := s.productionConfigError(); err == nil || !strings.Contains(err.Error(), "PROVIDER_VERIFIED") {
		t.Fatalf("expected provider verification failure, got %v", err)
	}
	t.Setenv("SCOS_BACKUP_PROVIDER_VERIFIED", "true")
	t.Setenv("SCOS_BACKUP_OFF_HOST_VERIFIED", "true")
	s.config = loadBackupConfig()
	if err := s.productionConfigError(); err == nil || !strings.Contains(err.Error(), "PITR") {
		t.Fatalf("expected PITR failure, got %v", err)
	}
	t.Setenv("SCOS_WAL_ARCHIVE_DESTINATION", "s3://scos-wal-archive")
	t.Setenv("SCOS_PITR_ENABLED", "true")
	s.config = loadBackupConfig()
	if err := s.productionConfigError(); err == nil || !strings.Contains(err.Error(), "GIT_MIRROR") {
		t.Fatalf("expected git mirror failure, got %v", err)
	}
	t.Setenv("SCOS_OKF_GIT_MIRROR_REMOTE", "git@example.com:scos/okf-mirror.git")
	t.Setenv("SCOS_OKF_GIT_MIRROR_VERIFIED", "true")
	s.config = loadBackupConfig()
	if err := s.productionConfigError(); err != nil && strings.Contains(err.Error(), "GIT_MIRROR") {
		t.Fatalf("expected git mirror requirement satisfied, got %v", err)
	}
}

func TestGitMirrorPlanRequiresRemoteAndVerification(t *testing.T) {
	t.Setenv("SCOS_BACKUP_PROVIDER", "local")
	s := newBackupServer()
	if s.plan.GitMirrors {
		t.Fatal("git mirrors must not be advertised without a configured remote")
	}
	t.Setenv("SCOS_OKF_GIT_MIRROR_REMOTE", "git@example.com:scos/okf-mirror.git")
	s.config = loadBackupConfig()
	s.plan = backupPlan(s.config)
	if s.plan.GitMirrors {
		t.Fatal("git mirrors must not be advertised without verification")
	}
	t.Setenv("SCOS_OKF_GIT_MIRROR_VERIFIED", "true")
	s.config = loadBackupConfig()
	s.plan = backupPlan(s.config)
	if !s.plan.GitMirrors {
		t.Fatal("expected git mirrors advertised once remote is configured and verified")
	}
}

func TestLocalBackupArtifactLifecycle(t *testing.T) {
	dir := t.TempDir()
	s := newBackupServerWithDir(dir)

	req := httptest.NewRequest(http.MethodPost, "/v1/backups/start", stringsReader(`{"type":"base_backup"}`))
	start := httptest.NewRecorder()
	s.handleBackupStart(start, req)
	if start.Code != http.StatusOK {
		t.Fatalf("start status = %d", start.Code)
	}
	var started BackupRecord
	if err := json.NewDecoder(start.Body).Decode(&started); err != nil {
		t.Fatal(err)
	}
	if started.Status != "running" || started.Location == "" {
		t.Fatalf("unexpected start record: %+v", started)
	}
	if _, err := os.Stat(started.Location); err != nil {
		t.Fatalf("artifact was not created: %v", err)
	}

	complete := httptest.NewRecorder()
	s.handleBackupComplete(complete, httptest.NewRequest(http.MethodPost, "/v1/backups/complete", stringsReader(`{"backup_id":"`+started.ID+`","status":"completed","size_bytes":999}`)))
	if complete.Code != http.StatusOK {
		t.Fatalf("complete status = %d: %s", complete.Code, complete.Body.String())
	}
	var completed BackupRecord
	if err := json.NewDecoder(complete.Body).Decode(&completed); err != nil {
		t.Fatal(err)
	}
	if completed.Status != "completed" || completed.SizeBytes == 999 || completed.VerifiedAt == "" {
		t.Fatalf("completion trusted caller metadata: %+v", completed)
	}
}

func TestTamperedArtifactFailsRestore(t *testing.T) {
	s := newBackupServerWithDir(t.TempDir())
	artifact := backupArtifact{Manifest: backupManifest{ID: "backup-tampered", Type: "base_backup", Format: "scos-local-v1", ContentHash: hashString("original"), SizeBytes: 8}, Payload: "tampered"}
	data, _ := json.Marshal(artifact)
	if err := os.WriteFile(filepath.Join(s.backupDir, "backup-tampered.backup.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.handleRestore(rec, httptest.NewRequest(http.MethodPost, "/v1/backups/backup-tampered/restore", nil), "backup-tampered")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("restore status = %d", rec.Code)
	}
}

func TestReadyChecksBackupStorage(t *testing.T) {
	s := newBackupServerWithDir(filepath.Join(t.TempDir(), "missing"))
	rec := httptest.NewRecorder()
	s.handleReady(rec, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("ready status = %d", rec.Code)
	}
}

func TestRetentionCleanupRemovesExpiredArtifacts(t *testing.T) {
	dir := t.TempDir()
	s := newBackupServerWithDir(dir)
	s.retention.WALAndDaily = 1
	artifact := backupArtifact{Manifest: backupManifest{ID: "backup-old", Type: "base_backup", Format: "scos-local-v1", ContentHash: hashString("old"), SizeBytes: 3}, Payload: "old"}
	data, _ := json.Marshal(artifact)
	path := filepath.Join(dir, "backup-old.backup.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	removed, err := s.cleanupRetention(time.Now())
	if err != nil || removed != 1 {
		t.Fatalf("cleanup removed=%d err=%v", removed, err)
	}
}

func TestUnsupportedProviderIsUnavailable(t *testing.T) {
	s := newBackupServerWithDir(t.TempDir())
	s.provider = "s3"
	rec := httptest.NewRecorder()
	s.handleBackupStart(rec, httptest.NewRequest(http.MethodPost, "/v1/backups/start", stringsReader(`{}`)))
	if rec.Code != http.StatusServiceUnavailable || !contains(rec.Body.String(), "unavailable") {
		t.Fatalf("unexpected unsupported provider response: %d %s", rec.Code, rec.Body.String())
	}
}

func stringsReader(v string) *strings.Reader { return strings.NewReader(v) }
func contains(s, v string) bool              { return strings.Contains(s, v) }

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

func TestRestoreCadenceStatusReportsOverdueWithNoHistory(t *testing.T) {
	statuses := restoreCadenceStatus(map[string]*RestoreTestResult{}, time.Now().UTC())
	if len(statuses) != 3 {
		t.Fatalf("expected 3 cadence entries, got %d", len(statuses))
	}
	for _, c := range statuses {
		if !c.Overdue {
			t.Fatalf("cadence %s with no history must be overdue", c.TestType)
		}
		if c.LastPassedAt != "" {
			t.Fatalf("cadence %s with no history must have no last_passed_at", c.TestType)
		}
	}
}

func TestRestoreCadenceStatusHonoursRecentPass(t *testing.T) {
	now := time.Now().UTC()
	tests := map[string]*RestoreTestResult{
		"t1": {ID: "t1", TestType: restoreTestTypeMonthly, Status: "passed", CompletedAt: now.Add(-2 * 24 * time.Hour).Format(time.RFC3339)},
		"t2": {ID: "t2", TestType: restoreTestTypeQuarterly, Status: "passed", CompletedAt: now.Add(-120 * 24 * time.Hour).Format(time.RFC3339)},
	}
	statuses := restoreCadenceStatus(tests, now)
	byType := make(map[string]CadenceStatus, len(statuses))
	for _, c := range statuses {
		byType[c.TestType] = c
	}
	if byType[restoreTestTypeMonthly].Overdue {
		t.Fatal("monthly restore test passed 2 days ago must not be overdue")
	}
	if !byType[restoreTestTypeQuarterly].Overdue {
		t.Fatal("quarterly drill passed 120 days ago must be overdue (interval is 93 days)")
	}
	if byType[restoreTestTypeIntegrity].LastPassedAt != "" {
		t.Fatal("integrity cadence has no recorded pass and must report none")
	}
}

func TestRestoreTestPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	s := newBackupServerWithDir(dir)
	artifact := backupArtifact{Manifest: backupManifest{ID: "backup-1", Type: "base_backup", Format: "scos-local-v1", ContentHash: hashString("data"), SizeBytes: 4}, Payload: "data"}
	data, _ := json.Marshal(artifact)
	if err := os.WriteFile(filepath.Join(dir, "backup-1.backup.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	result := s.executeRestoreTest(restoreTestTypeMonthly, "backup-1")
	if result.Status != "failed" {
		// Local synthetic artifacts never satisfy a restore drill by design;
		// the point here is that the *result* is durable, not its verdict.
		t.Fatalf("unexpected status: %s", result.Status)
	}

	reopened := newBackupServerWithDir(dir)
	reopened.mu.RLock()
	_, ok := reopened.restoreTests[result.ID]
	reopened.mu.RUnlock()
	if !ok {
		t.Fatal("expected restore test result to survive server restart")
	}
}

func TestRestoreSchedulerIntervalDefaultsAndParses(t *testing.T) {
	if got := restoreSchedulerInterval(); got != time.Hour {
		t.Fatalf("expected default 1h interval, got %s", got)
	}
	t.Setenv("SCOS_BACKUP_RESTORE_SCHEDULER_INTERVAL", "15m")
	if got := restoreSchedulerInterval(); got != 15*time.Minute {
		t.Fatalf("expected 15m interval, got %s", got)
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

func TestProductionEncryptionRoundTrip(t *testing.T) {
	key := strings.Repeat("k", 32)
	ciphertext, err := encryptBackup([]byte(key), []byte("postgres dump"))
	if err != nil {
		t.Fatal(err)
	}
	if string(ciphertext) == "postgres dump" {
		t.Fatal("backup payload must not be stored in plaintext")
	}
	plaintext, err := decryptBackup([]byte(key), ciphertext)
	if err != nil || string(plaintext) != "postgres dump" {
		t.Fatalf("decrypt result = %q, err = %v", plaintext, err)
	}
	ciphertext[len(ciphertext)-1] ^= 1
	if _, err := decryptBackup([]byte(key), ciphertext); err == nil {
		t.Fatal("tampered encrypted backup was accepted")
	}
}

func TestProductionRestoreDrillRequiresTemporaryDatabase(t *testing.T) {
	t.Setenv("SCOS_BACKUP_PROVIDER", "filesystem")
	t.Setenv("SCOS_BACKUP_DESTINATION", t.TempDir())
	t.Setenv("SCOS_BACKUP_ENCRYPTION_KEY", strings.Repeat("k", 32))
	t.Setenv("SCOS_BACKUP_DATABASE_URL", "postgres://example/db")
	s := newBackupServerWithDir(t.TempDir())
	if err := s.restoreProduction("missing"); err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Fatalf("expected unavailable restore source, got %v", err)
	}
}
