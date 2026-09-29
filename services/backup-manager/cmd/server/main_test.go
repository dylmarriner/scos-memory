package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

// TestMutatingBackupRoutesRequireAdminScope guards the AUDIT-007 fix: the
// four mutating routes must reject a caller authenticated with valid claims
// but lacking "backups:admin", and must accept one that has it. This
// exercises the actual mux, not the handlers directly, so it fails if the
// auth.RequireScope wrapping in newBackupMux is ever removed or loosened to
// a blanket auth.AuthenticatedHandler-only gate.
func TestMutatingBackupRoutesRequireAdminScope(t *testing.T) {
	s := newBackupServer()
	mux := newBackupMux(s)

	for _, route := range []string{"/v1/backups/start", "/v1/backups/complete", "/v1/restore-tests/start", "/v1/restore-tests/complete"} {
		t.Run(route, func(t *testing.T) {
			body := bytes.NewBufferString(`{}`)
			r := httptest.NewRequest(http.MethodPost, route, body)
			ctx := auth.ContextWithClaims(r.Context(), auth.Claims{TenantID: "tenant-1", PrincipalID: "principal-1"})
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, r.WithContext(ctx))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("expected 403 for caller without backups:admin scope on %s, got %d", route, rec.Code)
			}
		})
	}

	body := bytes.NewBufferString(`{}`)
	r := httptest.NewRequest(http.MethodPost, "/v1/backups/start", body)
	ctx := auth.ContextWithClaims(r.Context(), auth.Claims{TenantID: "tenant-1", PrincipalID: "principal-1", Scopes: []string{"backups:admin"}})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r.WithContext(ctx))
	if rec.Code == http.StatusForbidden {
		t.Fatalf("expected caller with backups:admin scope to pass the scope gate, got 403")
	}
}

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

// fakePostgresTools replaces commandContext so pg_dump writes a small archive
// to its --file target and every other PostgreSQL client tool succeeds,
// recording each invocation.
func fakePostgresTools(t *testing.T) *[]string {
	t.Helper()
	var calls []string
	orig := commandContext
	commandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		calls = append(calls, name+" "+strings.Join(args, " "))
		if name == "pg_dump" {
			for i, a := range args {
				if a == "--file" {
					return exec.CommandContext(ctx, "sh", "-c", `printf 'PGDMP-archive' > "$1"`, "sh", args[i+1])
				}
			}
		}
		return exec.CommandContext(ctx, "true")
	}
	t.Cleanup(func() { commandContext = orig })
	return &calls
}

// writeLocalDump stores a verifiable local backup (dump + manifest).
func writeLocalDump(t *testing.T, s *backupServer, id, backupType, content string) {
	t.Helper()
	if err := os.WriteFile(s.dumpPath(id), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	m := backupManifest{ID: id, Type: backupType, Format: localDumpFormat, ContentHash: hashString(content), SizeBytes: int64(len(content)), CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	data, _ := json.Marshal(backupArtifact{Manifest: m})
	if err := os.WriteFile(s.artifactPath(id), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestLocalBackupArtifactLifecycle(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SCOS_BACKUP_DATABASE_URL", "postgres://scos@db/scos?sslmode=require")
	calls := fakePostgresTools(t)
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
	if started.Status != "completed" || started.Location != s.dumpPath(started.ID) || started.SizeBytes != int64(len("PGDMP-archive")) {
		t.Fatalf("unexpected start record: %+v", started)
	}
	if got, _ := os.ReadFile(started.Location); string(got) != "PGDMP-archive" {
		t.Fatalf("dump content = %q", got)
	}
	if len(*calls) != 1 || !strings.HasPrefix((*calls)[0], "pg_dump --format=custom --no-owner --enable-row-security") || !strings.HasSuffix((*calls)[0], "postgres://scos@db/scos?sslmode=require") {
		t.Fatalf("pg_dump invocation = %v", *calls)
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
	writeLocalDump(t, s, "backup-old", "base_backup", "old")
	path := filepath.Join(dir, "backup-old.backup.json")
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	removed, err := s.cleanupRetention(time.Now())
	if err != nil || removed != 1 {
		t.Fatalf("cleanup removed=%d err=%v", removed, err)
	}
	if _, err := os.Stat(s.dumpPath("backup-old")); !os.IsNotExist(err) {
		t.Fatalf("expired dump not removed: %v", err)
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
	writeLocalDump(t, s, "backup-1", "base_backup", "data")
	// No restore database configured, so the monthly drill fails; the point
	// here is that the *result* is durable, not its verdict.
	result := s.executeRestoreTest(restoreTestTypeMonthly, "backup-1")
	if result.Status != "failed" {
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

func TestLocalRestoreDrills(t *testing.T) {
	calls := fakePostgresTools(t)
	s := newBackupServerWithDir(t.TempDir())
	writeLocalDump(t, s, "backup-1", "base_backup", "PGDMP")

	if r := s.executeRestoreTest(restoreTestTypeIntegrity, "backup-1"); r.Status != "passed" {
		t.Fatalf("integrity drill = %+v", r)
	}
	if r := s.executeRestoreTest(restoreTestTypeMonthly, "backup-1"); r.Status != "failed" || !strings.Contains(r.Detail, "SCOS_BACKUP_RESTORE_DATABASE_URL") {
		t.Fatalf("monthly drill without restore database = %+v", r)
	}
	t.Setenv("SCOS_BACKUP_RESTORE_DATABASE_URL", "postgres://scos@db/postgres?sslmode=require")
	if _, err := exec.LookPath("createdb"); err != nil {
		t.Skip("createdb not on PATH; temporary-database restore path needs it")
	}
	if r := s.executeRestoreTest(restoreTestTypeMonthly, "backup-1"); r.Status != "passed" {
		t.Fatalf("monthly drill with restore database = %+v", r)
	}
	joined := strings.Join(*calls, "\n")
	if !strings.Contains(joined, "createdb") || !strings.Contains(joined, "createdb --maintenance-db") || !strings.Contains(joined, "pg_restore --exit-on-error --no-owner --dbname") || !strings.Contains(joined, "dropdb --if-exists --maintenance-db") {
		t.Fatalf("restore did not create, restore and drop a temporary database: %s", joined)
	}
}

func TestLocalBackupRejectsTamperedDumpAndSyntheticArtifacts(t *testing.T) {
	s := newBackupServerWithDir(t.TempDir())
	writeLocalDump(t, s, "backup-1", "base_backup", "PGDMP")
	if err := os.WriteFile(s.dumpPath("backup-1"), []byte("PGDMX"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.verifyBackup("backup-1"); err == nil {
		t.Fatal("tampered dump verified")
	}
	synthetic, _ := json.Marshal(backupArtifact{Manifest: backupManifest{ID: "backup-2", Format: "scos-local-test-v1", ContentHash: hashString("x"), SizeBytes: 1}, Payload: "x"})
	if err := os.WriteFile(s.artifactPath("backup-2"), synthetic, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.verifyBackup("backup-2"); err == nil || !strings.Contains(err.Error(), "not a PostgreSQL dump") {
		t.Fatalf("synthetic artifact verify err = %v", err)
	}
}

func TestBackupSchedulerTakesBackupOnlyWhenDue(t *testing.T) {
	t.Setenv("SCOS_BACKUP_DATABASE_URL", "postgres://scos@db/scos?sslmode=require")
	calls := fakePostgresTools(t)
	s := newBackupServerWithDir(t.TempDir())
	now := time.Now().UTC()
	s.backupIfDue(context.Background(), time.Hour, now)
	if len(*calls) != 1 {
		t.Fatalf("first run should back up, calls = %v", *calls)
	}
	s.backupIfDue(context.Background(), time.Hour, now.Add(30*time.Minute))
	if len(*calls) != 1 {
		t.Fatalf("backup taken before interval elapsed: %v", *calls)
	}
	s.backupIfDue(context.Background(), time.Hour, now.Add(2*time.Hour))
	if len(*calls) != 2 {
		t.Fatalf("overdue backup not taken: %v", *calls)
	}
	t.Setenv("SCOS_BACKUP_SCHEDULE_INTERVAL", "")
	if backupScheduleInterval() != 0 {
		t.Fatal("empty schedule interval should disable the scheduler")
	}
	t.Setenv("SCOS_BACKUP_SCHEDULE_INTERVAL", "24h")
	if backupScheduleInterval() != 24*time.Hour {
		t.Fatal("schedule interval not parsed")
	}
}

func TestSkipExtensionEntries(t *testing.T) {
	toc := ";\n; Archive created at ...\n2; 3079 16386 EXTENSION - vector \n5504; 0 0 COMMENT - EXTENSION vector \n230; 1259 16400 TABLE public tenants scos\n"
	got := skipExtensionEntries(toc)
	for _, want := range []string{";2; 3079 16386 EXTENSION - vector ", ";5504; 0 0 COMMENT - EXTENSION vector ", "\n230; 1259 16400 TABLE public tenants scos"} {
		if !strings.Contains(got, want) {
			t.Fatalf("skipExtensionEntries missing %q in:\n%s", want, got)
		}
	}
}
