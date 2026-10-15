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
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

// ── fakes ────────────────────────────────────────────────────────────────

// fakeTOC is what the fake pg_restore prints for --list.
var fakeTOC string

// fakePostgresTools replaces the PostgreSQL client binaries with this test
// binary (TestHelperProcess) and records every invocation in the returned log.
func fakePostgresTools(t *testing.T) *[]string {
	t.Helper()
	var calls []string
	prevCmd, prevLook := commandContext, lookPath
	commandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		calls = append(calls, name+" "+strings.Join(args, " "))
		cmd := exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=TestHelperProcess", "--", name}, args...)...)
		cmd.Env = append(os.Environ(), "SCOS_BACKUP_HELPER_PROCESS=1", "SCOS_FAKE_TOC="+fakeTOC)
		return cmd
	}
	lookPath = func(name string) (string, error) { return "/usr/bin/" + name, nil }
	t.Cleanup(func() { commandContext, lookPath = prevCmd, prevLook })
	return &calls
}

// TestHelperProcess stands in for pg_dump, createdb, dropdb and pg_restore.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("SCOS_BACKUP_HELPER_PROCESS") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	args = args[1:]
	if args[0] == "pg_restore" {
		data, err := os.ReadFile(args[len(args)-1])
		if err != nil || !bytes.HasPrefix(data, []byte("PGDMP")) {
			fmt.Fprintln(os.Stderr, "pg_restore: input file does not appear to be a valid archive")
			os.Exit(1)
		}
		if args[1] == "--list" {
			fmt.Print(os.Getenv("SCOS_FAKE_TOC"))
		}
	}
	os.Exit(0)
}

func testFingerprint() dataFingerprint {
	return dataFingerprint{
		TakenAt: "2026-10-15T00:00:00Z",
		Tables: map[string]tableFingerprint{
			"tenants":      {Rows: 2, Checksum: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
			"turns":        {Rows: 7, Checksum: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
			"memory_items": {Rows: 3, Checksum: "cccccccccccccccccccccccccccccccc"},
			"audit_log":    {Rows: 9, Checksum: "dddddddddddddddddddddddddddddddd"},
		},
		Catalog: catalogFingerprint{Tables: 4, Columns: 20, Functions: 3, Policies: 2, Triggers: 1, Indexes: 5, Hash: "catalog-hash"},
	}
}

const fullTOC = `;
; Archive created at 2026-10-15 00:00:00 UTC
3000; 0 16400 TABLE DATA public tenants scos
3001; 0 16401 TABLE DATA public turns scos
3002; 0 16402 TABLE DATA public memory_items scos
3003; 0 16403 TABLE DATA public audit_log scos
2000; 1259 16400 TABLE public tenants scos
`

// newTestServer builds a server on a fresh state directory whose PostgreSQL
// reach (dump, restored-database fingerprint, archiver statistics) is
// faked; restoredFP is what a restored database fingerprints to.
func newTestServer(t *testing.T, restoredFP *dataFingerprint) *backupServer {
	t.Helper()
	t.Setenv("SCOS_BACKUP_DATABASE_URL", "postgres://scos@db/scos_memory?sslmode=require")
	t.Setenv("SCOS_BACKUP_RESTORE_DATABASE_URL", "postgres://scos@db/postgres?sslmode=require")
	s := newBackupServerWithDir(t.TempDir())
	s.dump = func(context.Context, string) ([]byte, dataFingerprint, error) {
		return []byte("PGDMP custom archive " + time.Now().String()), testFingerprint(), nil
	}
	s.fingerprintTarget = func(_ context.Context, _ string, tables []string, withCatalog bool) (dataFingerprint, error) {
		out := dataFingerprint{Tables: map[string]tableFingerprint{}}
		if withCatalog {
			out.Catalog = restoredFP.Catalog
			for name, fp := range restoredFP.Tables {
				out.Tables[name] = fp
			}
			return out, nil
		}
		for _, name := range tables {
			if fp, ok := restoredFP.Tables[name]; ok {
				out.Tables[name] = fp
			}
		}
		return out, nil
	}
	s.walState = func(context.Context) (walArchiverState, error) { return walArchiverState{ArchiveMode: "off"}, nil }
	return s
}

func adminRequest(method, path, body string) *http.Request {
	r := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	return r.WithContext(auth.ContextWithClaims(r.Context(), auth.Claims{TenantID: "tenant-1", PrincipalID: "operator-1", Scopes: []string{"backups:admin"}}))
}

func takeBackup(t *testing.T, s *backupServer) backupStartResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	newBackupMux(s).ServeHTTP(rec, adminRequest(http.MethodPost, "/v1/backups/start", `{}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("backup start: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		BackupRecord
		Retention *RetentionResult `json:"retention"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return backupStartResponse{BackupRecord: &resp.BackupRecord, Retention: resp.Retention}
}

// ── routes and scopes ────────────────────────────────────────────────────

// TestMutatingBackupRoutesRequireAdminScope: every route that takes,
// restores, drills, expires or mirrors must reject a caller without
// "backups:admin" -- including the per-backup restore route, which any
// authenticated caller could reach before.
func TestMutatingBackupRoutesRequireAdminScope(t *testing.T) {
	s := newBackupServerWithDir(t.TempDir())
	mux := newBackupMux(s)
	for _, route := range []string{"/v1/backups/start", "/v1/backups/complete", "/v1/backups/backup-1/restore", "/v1/restore-tests/start", "/v1/restore-tests/complete", "/v1/retention/apply", "/v1/git-mirror/push"} {
		t.Run(route, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, route, bytes.NewBufferString(`{}`))
			ctx := auth.ContextWithClaims(r.Context(), auth.Claims{TenantID: "tenant-1", PrincipalID: "principal-1"})
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, r.WithContext(ctx))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("expected 403 for caller without backups:admin scope on %s, got %d", route, rec.Code)
			}
		})
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, adminRequest(http.MethodPost, "/v1/backups/start", `{}`))
	if rec.Code == http.StatusForbidden {
		t.Fatalf("expected caller with backups:admin scope to pass the scope gate, got 403")
	}
}

func TestOnlyImplementedProvidersExist(t *testing.T) {
	for provider, want := range map[string]string{"disabled": "disabled", "s3": "not implemented", "gcs": "not implemented"} {
		t.Setenv("SCOS_BACKUP_PROVIDER", provider)
		s := newBackupServerWithDir(t.TempDir())
		if s.provider != nil || s.providerErr == nil || !strings.Contains(s.providerErr.Error(), want) {
			t.Fatalf("%s: provider=%v err=%v", provider, s.provider, s.providerErr)
		}
		rec := httptest.NewRecorder()
		s.handleReady(rec, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: ready = %d", provider, rec.Code)
		}
		plan := s.evaluatePlan(context.Background(), time.Now())
		for key, item := range plan.Items {
			if item.Covered {
				t.Fatalf("%s: %s reported covered with no provider", provider, key)
			}
		}
	}
	t.Setenv("SCOS_BACKUP_PROVIDER", "filesystem")
	t.Setenv("SCOS_BACKUP_DESTINATION", "relative/path")
	if s := newBackupServerWithDir(t.TempDir()); s.provider != nil {
		t.Fatal("a relative filesystem destination must not configure a provider")
	}
}

func TestTLSDatabaseURLIsRequired(t *testing.T) {
	for _, raw := range []string{"postgres://user:pass@db/scos?sslmode=disable", "postgres://user:pass@db/scos"} {
		if err := requireTLSDatabaseURL(raw); err == nil {
			t.Fatalf("expected TLS validation failure for %q", raw)
		}
	}
	if err := requireTLSDatabaseURL("postgres://user:pass@db/scos?sslmode=require"); err != nil {
		t.Fatalf("expected TLS URL to pass: %v", err)
	}
}

func TestReadyChecksBackupStorage(t *testing.T) {
	fakePostgresTools(t)
	t.Setenv("SCOS_BACKUP_DATABASE_URL", "postgres://scos@db/scos?sslmode=require")
	base := t.TempDir()
	blocker := filepath.Join(base, "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newBackupServerWithDir(filepath.Join(blocker, "backups"))
	rec := httptest.NewRecorder()
	s.handleReady(rec, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("ready status = %d", rec.Code)
	}
	s = newBackupServerWithDir(t.TempDir())
	rec = httptest.NewRecorder()
	s.handleReady(rec, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("ready with writable storage and tools = %d %s", rec.Code, rec.Body.String())
	}
}

// ── backups ──────────────────────────────────────────────────────────────

func TestLocalBackupIsTakenFingerprintedAndVerified(t *testing.T) {
	fakePostgresTools(t)
	s := newTestServer(t, nil)
	resp := takeBackup(t, s)
	if resp.Status != "completed" || resp.Production || resp.Encrypted || resp.TablesFingerprinted != 4 || resp.VerifiedAt == "" {
		t.Fatalf("unexpected backup record: %+v", resp.BackupRecord)
	}
	if resp.Retention == nil || len(resp.Retention.Kept) != 1 {
		t.Fatalf("retention was not applied after the backup: %+v", resp.Retention)
	}
	m, archive, _, err := s.provider.Load(context.Background(), resp.ID)
	if err != nil || !bytes.HasPrefix(archive, []byte("PGDMP")) || m.Fingerprint == nil || m.Fingerprint.Tables["turns"].Rows != 7 {
		t.Fatalf("stored backup = %+v %q %v", m, archive, err)
	}
	rec := httptest.NewRecorder()
	newBackupMux(s).ServeHTTP(rec, adminRequest(http.MethodPost, "/v1/backups/start", `{"type":"weekly"}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a caller-chosen backup label must be refused, got %d", rec.Code)
	}
}

func TestLocalBackupRequiresDatabase(t *testing.T) {
	fakePostgresTools(t)
	t.Setenv("SCOS_BACKUP_DATABASE_URL", "")
	t.Setenv("SCOS_DATABASE_URL", "")
	s := newBackupServerWithDir(t.TempDir())
	rec := httptest.NewRecorder()
	newBackupMux(s).ServeHTTP(rec, adminRequest(http.MethodPost, "/v1/backups/start", `{}`))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "SCOS_DATABASE_URL") {
		t.Fatalf("expected an unavailable backup without a database, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestTamperedArtifactFailsVerificationAndDrills(t *testing.T) {
	fakePostgresTools(t)
	fp := testFingerprint()
	s := newTestServer(t, &fp)
	resp := takeBackup(t, s)
	if err := os.WriteFile(filepath.Join(s.backupDir, resp.ID+".dump"), []byte("PGDMP tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.provider.Load(context.Background(), resp.ID); err == nil {
		t.Fatal("a tampered archive verified")
	}
	result := s.executeRestoreTest(context.Background(), restoreTestTypeIntegrity, resp.ID, "t")
	if result.Status != "failed" || !strings.Contains(result.Detail, "does not verify") {
		t.Fatalf("drill on a tampered archive = %+v", result)
	}
}

func TestFilesystemProviderEncryptsAndAuthenticates(t *testing.T) {
	fakePostgresTools(t)
	t.Setenv("SCOS_BACKUP_PROVIDER", "filesystem")
	dest := t.TempDir()
	t.Setenv("SCOS_BACKUP_DESTINATION", "file://"+dest)
	t.Setenv("SCOS_BACKUP_ENCRYPTION_KEY", strings.Repeat("k", 32))
	s := newTestServer(t, nil)
	resp := takeBackup(t, s)
	if !resp.Encrypted || !resp.Production {
		t.Fatalf("filesystem backup = %+v", resp.BackupRecord)
	}
	raw, err := os.ReadFile(filepath.Join(dest, resp.ID+".backup"))
	if err != nil || bytes.Contains(raw, []byte("PGDMP")) {
		t.Fatalf("archive at rest is not encrypted: %v", err)
	}
	manifestPath := filepath.Join(dest, resp.ID+".backup.manifest.json")
	data, _ := os.ReadFile(manifestPath)
	var m backupManifest
	_ = json.Unmarshal(data, &m)
	m.Fingerprint.Tables["turns"] = tableFingerprint{Rows: 999, Checksum: "forged"}
	forged, _ := json.Marshal(m)
	if err := os.WriteFile(manifestPath, forged, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.provider.Load(context.Background(), resp.ID); err == nil || !strings.Contains(err.Error(), "verification failed") {
		t.Fatalf("a forged manifest fingerprint was accepted: %v", err)
	}
}

func TestProductionEncryptionRoundTrip(t *testing.T) {
	key := strings.Repeat("k", 32)
	ciphertext, err := encryptBackup([]byte(key), []byte("postgres dump"))
	if err != nil {
		t.Fatal(err)
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

func TestNewestVerifiedBackupAgeMeasuresFromWhenTheBackupWasTaken(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	verifiedNow := now.Format(time.RFC3339)
	backups := []*BackupRecord{
		{Status: "completed", CompletedAt: now.Add(-72 * time.Hour).Format(time.RFC3339), VerifiedAt: verifiedNow},
		{Status: "completed", CompletedAt: now.Add(-26 * time.Hour).Format(time.RFC3339), VerifiedAt: verifiedNow},
		{Status: "completed", CompletedAt: now.Add(-1 * time.Hour).Format(time.RFC3339)},
	}
	age, ok := newestVerifiedBackupAge(backups, now)
	if !ok || age != 26*time.Hour {
		t.Fatalf("age = %v, %v; a backup re-verified just now but taken 26h ago is 26h old", age, ok)
	}
	if _, ok := newestVerifiedBackupAge(nil, now); ok {
		t.Fatal("no verified backup must report no age")
	}
}

// ── plan coverage ────────────────────────────────────────────────────────

func TestPlanReportsOnlyWhatTheDeploymentDoes(t *testing.T) {
	fakePostgresTools(t)
	s := newTestServer(t, nil)
	plan := s.evaluatePlan(context.Background(), time.Now())
	if plan.BaseBackups || plan.Items["base_backups"].Reason == "" {
		t.Fatalf("base backups covered with no backup taken: %+v", plan.Items["base_backups"])
	}
	if !plan.Provider.Working || plan.Provider.OffHost {
		t.Fatalf("local provider report = %+v", plan.Provider)
	}

	takeBackup(t, s)
	plan = s.evaluatePlan(context.Background(), time.Now())
	if !plan.BaseBackups || !plan.Items["schema_exports"].Covered || plan.Items["base_backups"].LastSuccessAt == "" {
		t.Fatalf("a fresh verified backup must cover base backups and schema: %+v", plan.Items)
	}
	for _, key := range []string{"encrypted_off_host_copies", "remote_provider_verified", "object_store_snapshots", "postgresql_wal_archiving", "pitr_configured",
		"remote_git_mirrors", "schema_and_migration_exports", "secret_manager_recovery", "configuration_backups"} {
		if item := plan.Items[key]; item.Covered || item.Reason == "" {
			t.Fatalf("%s must be not covered, with a reason, on a local deployment: %+v", key, item)
		}
	}
	if plan.EncryptedOffHost || plan.PostgreSQLWAL || plan.PITRConfigured || plan.GitMirrors || plan.SecretRecovery || plan.ConfigBackups || plan.ObjectStoreSnaps || plan.RemoteVerified || plan.SchemaExports {
		t.Fatalf("legacy flags disagree with the items: %+v", plan)
	}

	// A backup older than the freshness bound no longer covers.
	s.config.coverageMaxAge = time.Minute
	plan = s.evaluatePlan(context.Background(), time.Now().Add(time.Hour))
	if plan.BaseBackups || !strings.Contains(plan.Items["base_backups"].Reason, "freshness bound") {
		t.Fatalf("a stale backup covered: %+v", plan.Items["base_backups"])
	}
}

func TestOffHostIsReadFromTheMountTable(t *testing.T) {
	dest := t.TempDir()
	mountinfo := filepath.Join(t.TempDir(), "mountinfo")
	write := func(lines ...string) {
		if err := os.WriteFile(mountinfo, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	prev := mountInfoPath
	mountInfoPath = mountinfo
	t.Cleanup(func() { mountInfoPath = prev })

	write(`22 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw`)
	if off, why := networkMount(dest); off || !strings.Contains(why, "local ext4") {
		t.Fatalf("an ext4 destination is not off-host: %v %q", off, why)
	}
	write(`22 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw`,
		fmt.Sprintf(`40 22 0:50 / %s rw,relatime shared:9 - nfs4 backup-host:/exports/scos rw,vers=4.2`, strings.ReplaceAll(dest, " ", `\040`)))
	if off, why := networkMount(dest); !off || !strings.Contains(why, "nfs4 mount of backup-host:/exports/scos") {
		t.Fatalf("an nfs4 destination is off-host: %v %q", off, why)
	}
	if off, _ := networkMount(filepath.Dir(dest)); off {
		t.Fatal("the parent of a network mount is not on it")
	}

	// A working encrypted provider on that mount with a fresh backup covers
	// encrypted off-host copies.
	fakePostgresTools(t)
	t.Setenv("SCOS_BACKUP_PROVIDER", "filesystem")
	t.Setenv("SCOS_BACKUP_DESTINATION", dest)
	t.Setenv("SCOS_BACKUP_ENCRYPTION_KEY", strings.Repeat("k", 32))
	s := newTestServer(t, nil)
	takeBackup(t, s)
	plan := s.evaluatePlan(context.Background(), time.Now())
	if !plan.EncryptedOffHost || !plan.RemoteVerified || !strings.Contains(plan.Items["encrypted_off_host_copies"].Evidence, "nfs4") {
		t.Fatalf("encrypted off-host coverage = %+v", plan.Items["encrypted_off_host_copies"])
	}
}

func TestWALCoverageNeedsTheArchivedSegmentAtTheDestination(t *testing.T) {
	dest := t.TempDir()
	at := time.Now().UTC()
	st := walArchiverState{ArchiveMode: "on", ArchivedCount: 12, LastArchivedWAL: "00000001000000000000000A", LastArchivedTime: &at,
		CurrentWAL: "00000001000000000000000B", SegmentSize: 16 << 20}
	if c := walCoverage(walArchiverState{ArchiveMode: "off"}, "file://"+dest, 2); c.Covered || !strings.Contains(c.Reason, "archive_mode") {
		t.Fatalf("archive_mode off: %+v", c)
	}
	// The server says it archived the segment, but nothing arrived: the
	// archive_command exited 0 without copying.
	if c := walCoverage(st, "file://"+dest, 2); c.Covered || !strings.Contains(c.Reason, "without copying") {
		t.Fatalf("missing segment: %+v", c)
	}
	if err := os.WriteFile(filepath.Join(dest, st.LastArchivedWAL), make([]byte, 16<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	if c := walCoverage(st, "file://"+dest, 2); !c.Covered || !strings.Contains(c.Evidence, "present at") {
		t.Fatalf("archived and present: %+v", c)
	}
	failed := at.Add(time.Minute)
	failing := st
	failing.LastFailedWAL, failing.LastFailedTime = "00000001000000000000000B", &failed
	if c := walCoverage(failing, "file://"+dest, 2); c.Covered || !strings.Contains(c.Reason, "failing") {
		t.Fatalf("failing archiver: %+v", c)
	}
	behind := st
	behind.CurrentWAL = "000000010000000100000002"
	if c := walCoverage(behind, "file://"+dest, 2); c.Covered || !strings.Contains(c.Reason, "segments behind") {
		t.Fatalf("lagging archiver: %+v", c)
	}
	if n, _ := walSegmentNumber("000000010000000100000002", 16<<20); n != 0x102 {
		t.Fatalf("segment number = %x", n)
	}
	if c := walCoverage(st, "s3://bucket/wal", 2); c.Covered {
		t.Fatalf("an unreachable destination cannot be confirmed: %+v", c)
	}
}

// ── retention ────────────────────────────────────────────────────────────

func TestRetentionClassificationIsDerivedFromWhenBackupsWereTaken(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	policy := RetentionPolicy{WALAndDaily: 35, Weekly: 12, Monthly: 12}
	var backups []retentionBackup
	// One backup a day for 500 days.
	for d := 0; d < 500; d++ {
		backups = append(backups, retentionBackup{ID: fmt.Sprintf("backup-%03d", d), Taken: now.AddDate(0, 0, -d)})
	}
	kept := map[string]string{}
	for _, b := range classifyRetention(policy, backups, now) {
		if !b.Expired {
			kept[b.ID] = b.Reason
		}
	}
	for d := 0; d <= 35; d++ {
		if _, ok := kept[fmt.Sprintf("backup-%03d", d)]; !ok {
			t.Fatalf("day %d inside the daily window was expired", d)
		}
	}
	weekly, monthly := 0, 0
	for id, reason := range kept {
		switch {
		case strings.Contains(reason, "of week"):
			weekly++
		case strings.Contains(reason, "of month"):
			monthly++
		}
		var d int
		fmt.Sscanf(id, "backup-%d", &d)
		if d > 366+31 {
			t.Fatalf("%s (%d days old) kept beyond every window: %s", id, d, reason)
		}
	}
	if weekly < 6 || weekly > 8 || monthly < 8 || monthly > 11 {
		t.Fatalf("weekly=%d monthly=%d kept beyond the daily window", weekly, monthly)
	}
	if len(kept) > 36+8+11 {
		t.Fatalf("kept %d backups", len(kept))
	}
	// The newest backup is kept however old it is.
	old := classifyRetention(policy, []retentionBackup{{ID: "backup-old", Taken: now.AddDate(-3, 0, 0)}}, now)
	if old[0].Expired {
		t.Fatal("the only backup was expired")
	}
}

func TestRetentionIsAppliedToProductionBackupsAndAudited(t *testing.T) {
	fakePostgresTools(t)
	t.Setenv("SCOS_BACKUP_PROVIDER", "filesystem")
	dest := t.TempDir()
	t.Setenv("SCOS_BACKUP_DESTINATION", dest)
	t.Setenv("SCOS_BACKUP_ENCRYPTION_KEY", strings.Repeat("k", 32))
	s := newTestServer(t, nil)
	ctx := context.Background()
	now := time.Now().UTC()
	store := func(id string, taken time.Time) {
		fp := testFingerprint()
		archive := []byte("PGDMP " + id)
		m := backupManifest{ID: id, Type: "base_backup", Format: "postgresql-custom-v1", ContentHash: hashBytes(archive), SizeBytes: int64(len(archive)), Production: true, CreatedAt: taken.Format(time.RFC3339), Fingerprint: &fp}
		if _, err := s.provider.Store(ctx, m, archive); err != nil {
			t.Fatal(err)
		}
	}
	store("backup-1000", now.AddDate(0, 0, -400))
	store("backup-2000", now.AddDate(0, 0, -100))
	store("backup-2001", now.AddDate(0, 0, -99))
	store("backup-3000", now.AddDate(0, 0, -1))
	// An archive that no longer verifies is never deleted automatically.
	if err := os.WriteFile(filepath.Join(dest, "backup-0500.backup"), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	newBackupMux(s).ServeHTTP(rec, adminRequest(http.MethodPost, "/v1/retention/apply", ``))
	if rec.Code != http.StatusOK {
		t.Fatalf("apply: %d %s", rec.Code, rec.Body.String())
	}
	var res RetentionResult
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	deleted := strings.Join(res.Deleted, ",")
	if !strings.Contains(deleted, "backup-1000") || !strings.Contains(deleted, "backup-2000") || strings.Contains(deleted, "backup-2001") || strings.Contains(deleted, "backup-3000") {
		t.Fatalf("deleted = %v (kept %v)", res.Deleted, res.Kept)
	}
	if len(res.Unverifiable) != 1 || res.Unverifiable[0] != "backup-0500" {
		t.Fatalf("unverifiable = %v", res.Unverifiable)
	}
	if _, err := os.Stat(filepath.Join(dest, "backup-1000.backup")); !os.IsNotExist(err) {
		t.Fatalf("expired archive still present: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "backup-0500.backup")); err != nil {
		t.Fatalf("unverifiable archive was deleted: %v", err)
	}
	rec = httptest.NewRecorder()
	newBackupMux(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/retention/audit", nil))
	var audit []retentionAuditRecord
	_ = json.Unmarshal(rec.Body.Bytes(), &audit)
	if len(audit) != 2 || audit[0].Actor != "operator-1" || audit[0].Action != "backup_deleted" || audit[0].Reason == "" || audit[0].TakenAt == "" {
		t.Fatalf("audit = %+v", audit)
	}
}

func TestRetentionDoesNotDeleteWhatItCannotAudit(t *testing.T) {
	fakePostgresTools(t)
	s := newTestServer(t, nil)
	ctx := context.Background()
	now := time.Now().UTC()
	for i, age := range []int{400, 1} {
		fp := testFingerprint()
		archive := []byte(fmt.Sprintf("PGDMP %d", i))
		id := fmt.Sprintf("backup-%d", i)
		m := backupManifest{ID: id, Type: "base_backup", ContentHash: hashBytes(archive), SizeBytes: int64(len(archive)), CreatedAt: now.AddDate(0, 0, -age).Format(time.RFC3339), Fingerprint: &fp}
		if _, err := s.provider.Store(ctx, m, archive); err != nil {
			t.Fatal(err)
		}
	}
	// The audit log path is a directory: no record can be appended.
	if err := os.MkdirAll(s.retentionAuditPath(), 0o750); err != nil {
		t.Fatal(err)
	}
	res, err := s.applyRetention(ctx, "t", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Deleted) != 0 || len(res.Errors) == 0 {
		t.Fatalf("deleted without an audit record: %+v", res)
	}
	if _, _, _, err := s.provider.Load(ctx, "backup-0"); err != nil {
		t.Fatalf("unaudited expiry removed the backup: %v", err)
	}
}

func TestWALRetentionExpiresOldSegmentsOnly(t *testing.T) {
	fakePostgresTools(t)
	walDir := t.TempDir()
	t.Setenv("SCOS_WAL_ARCHIVE_DESTINATION", "file://"+walDir)
	s := newTestServer(t, nil)
	old := time.Now().AddDate(0, 0, -40)
	for _, name := range []string{"000000010000000000000001", "00000002.history", "000000010000000000000002"} {
		path := filepath.Join(walDir, name)
		if err := os.WriteFile(path, []byte("wal"), 0o600); err != nil {
			t.Fatal(err)
		}
		if name != "000000010000000000000002" {
			if err := os.Chtimes(path, old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	res := RetentionResult{}
	n, _ := s.applyWALRetention("t", time.Now(), &res)
	if n != 1 {
		t.Fatalf("deleted %d WAL files", n)
	}
	for name, want := range map[string]bool{"000000010000000000000001": false, "00000002.history": true, "000000010000000000000002": true} {
		_, err := os.Stat(filepath.Join(walDir, name))
		if (err == nil) != want {
			t.Fatalf("%s present=%v, want %v", name, err == nil, want)
		}
	}
}

func TestDefaultRetention(t *testing.T) {
	s := newBackupServerWithDir(t.TempDir())
	if s.retention.WALAndDaily != 35 || s.retention.Weekly != 12 || s.retention.Monthly != 12 || !s.retention.OKFIndefinite {
		t.Fatalf("retention = %+v", s.retention)
	}
}

// ── restore drills ───────────────────────────────────────────────────────

func TestEachDrillTypeDoesWhatItNames(t *testing.T) {
	calls := fakePostgresTools(t)
	fakeTOC = fullTOC
	t.Cleanup(func() { fakeTOC = "" })
	restored := testFingerprint()
	s := newTestServer(t, &restored)
	s.config.keyTables = []string{"tenants", "turns", "memory_items", "not_a_table"}
	backup := takeBackup(t, s)
	ctx := context.Background()

	*calls = nil
	integrity := s.executeRestoreTest(ctx, restoreTestTypeIntegrity, backup.ID, "t")
	if integrity.Status != "passed" || !strings.Contains(strings.Join(integrity.Validations, "|"), "all 4 fingerprinted tables have data entries") {
		t.Fatalf("integrity = %+v", integrity)
	}
	for _, c := range *calls {
		if strings.HasPrefix(c, "createdb") {
			t.Fatal("an integrity check must not restore")
		}
	}

	*calls = nil
	monthly := s.executeRestoreTest(ctx, restoreTestTypeMonthly, backup.ID, "t")
	joined := strings.Join(monthly.Validations, "|")
	if monthly.Status != "passed" || !strings.Contains(joined, "turns: 7 rows") || !strings.Contains(joined, "not_a_table: not in this backup") || strings.Contains(joined, "audit_log") {
		t.Fatalf("monthly = %+v", monthly)
	}
	if !strings.Contains(strings.Join(*calls, "\n"), "createdb") || !strings.Contains(strings.Join(*calls, "\n"), "dropdb --if-exists") {
		t.Fatalf("monthly restore did not use a temporary database: %v", *calls)
	}

	quarterly := s.executeRestoreTest(ctx, restoreTestTypeQuarterly, backup.ID, "t")
	joined = strings.Join(quarterly.Validations, "|")
	if quarterly.Status != "passed" || !strings.Contains(joined, "audit_log: 9 rows") || !strings.Contains(joined, "schema catalog matches") {
		t.Fatalf("quarterly = %+v", quarterly)
	}
}

func TestDrillsFailWhenRestoredDataDiffers(t *testing.T) {
	fakePostgresTools(t)
	restored := testFingerprint()
	s := newTestServer(t, &restored)
	backup := takeBackup(t, s)
	ctx := context.Background()

	restored.Tables["turns"] = tableFingerprint{Rows: 6, Checksum: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"}
	if r := s.executeRestoreTest(ctx, restoreTestTypeMonthly, backup.ID, "t"); r.Status != "failed" || !strings.Contains(r.Detail, "turns: 6 rows restored, manifest has 7") {
		t.Fatalf("row loss not detected: %+v", r)
	}
	restored = testFingerprint()
	restored.Tables["audit_log"] = tableFingerprint{Rows: 9, Checksum: "ffffffffffffffffffffffffffffffff"}
	if r := s.executeRestoreTest(ctx, restoreTestTypeMonthly, backup.ID, "t"); r.Status != "passed" {
		t.Fatalf("monthly validates key tables only: %+v", r)
	}
	if r := s.executeRestoreTest(ctx, restoreTestTypeQuarterly, backup.ID, "t"); r.Status != "failed" || !strings.Contains(r.Detail, "audit_log: checksum") {
		t.Fatalf("full recovery must validate every table: %+v", r)
	}
	restored = testFingerprint()
	restored.Catalog.Hash = "other"
	if r := s.executeRestoreTest(ctx, restoreTestTypeQuarterly, backup.ID, "t"); r.Status != "failed" || !strings.Contains(r.Detail, "schema catalog differs") {
		t.Fatalf("catalog drift not detected: %+v", r)
	}
	restored = testFingerprint()
	restored.Tables["unexpected"] = tableFingerprint{Rows: 1, Checksum: "x"}
	if r := s.executeRestoreTest(ctx, restoreTestTypeQuarterly, backup.ID, "t"); r.Status != "failed" || !strings.Contains(r.Detail, "not in the backup") {
		t.Fatalf("an extra restored table not detected: %+v", r)
	}
	fakeTOC = "3000; 0 16400 TABLE DATA public tenants scos\n"
	t.Cleanup(func() { fakeTOC = "" })
	if r := s.executeRestoreTest(ctx, restoreTestTypeIntegrity, backup.ID, "t"); r.Status != "failed" || !strings.Contains(r.Detail, "no data entry") {
		t.Fatalf("missing table data in the archive not detected: %+v", r)
	}
}

func TestDrillRequiresAFingerprintedBackup(t *testing.T) {
	fakePostgresTools(t)
	s := newTestServer(t, nil)
	archive := []byte("PGDMP legacy")
	m := backupManifest{ID: "backup-legacy", Type: "base_backup", ContentHash: hashBytes(archive), SizeBytes: int64(len(archive)), CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	if _, err := s.provider.Store(context.Background(), m, archive); err != nil {
		t.Fatal(err)
	}
	if r := s.executeRestoreTest(context.Background(), restoreTestTypeMonthly, "backup-legacy", "t"); r.Status != "failed" || !strings.Contains(r.Detail, "no data fingerprint") {
		t.Fatalf("a drill passed with nothing to validate against: %+v", r)
	}
}

func TestRestoreTestStartValidatesTypeAndRecordsActor(t *testing.T) {
	fakePostgresTools(t)
	restored := testFingerprint()
	s := newTestServer(t, &restored)
	takeBackup(t, s)
	mux := newBackupMux(s)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, adminRequest(http.MethodPost, "/v1/restore-tests/start", `{"test_type":"whatever"}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("an unknown drill type was accepted: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, adminRequest(http.MethodPost, "/v1/restore-tests/start", `{"test_type":"monthly_restore"}`))
	var result RestoreTestResult
	_ = json.Unmarshal(rec.Body.Bytes(), &result)
	if rec.Code != http.StatusOK || result.Status != "passed" || result.Actor != "operator-1" || result.BackupID == "" {
		t.Fatalf("start = %d %+v", rec.Code, result)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, adminRequest(http.MethodPost, "/v1/backups/"+result.BackupID+"/restore", ``))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"monthly_restore"`) {
		t.Fatalf("restore route = %d %s", rec.Code, rec.Body.String())
	}
}

func TestCadenceIsTrackedPerDrillType(t *testing.T) {
	fakePostgresTools(t)
	restored := testFingerprint()
	s := newTestServer(t, &restored)
	takeBackup(t, s)
	s.executeRestoreTest(context.Background(), restoreTestTypeMonthly, "", "t")
	byType := map[string]CadenceStatus{}
	for _, c := range restoreCadenceStatus(s.restoreTestSnapshot(), time.Now().UTC()) {
		byType[c.TestType] = c
	}
	if byType[restoreTestTypeMonthly].Overdue || byType[restoreTestTypeMonthly].LastBackupID == "" {
		t.Fatalf("monthly cadence = %+v", byType[restoreTestTypeMonthly])
	}
	if !byType[restoreTestTypeQuarterly].Overdue || !byType[restoreTestTypeIntegrity].Overdue {
		t.Fatalf("a monthly restore satisfied another cadence: %+v", byType)
	}
	// The scheduler runs only the overdue types, each its own drill.
	fakeTOC = fullTOC
	t.Cleanup(func() { fakeTOC = "" })
	s.runDueRestoreTests(context.Background())
	counts := map[string]int{}
	for _, r := range s.restoreTestSnapshot() {
		if r.Status == "passed" {
			counts[r.TestType]++
		}
	}
	if counts[restoreTestTypeMonthly] != 1 || counts[restoreTestTypeQuarterly] != 1 || counts[restoreTestTypeIntegrity] != 1 {
		t.Fatalf("scheduled drills = %v", counts)
	}
}

func TestRestoreCadenceStatusHonoursRecentPass(t *testing.T) {
	now := time.Now().UTC()
	tests := map[string]*RestoreTestResult{
		"t1": {ID: "t1", TestType: restoreTestTypeMonthly, Status: "passed", CompletedAt: now.Add(-2 * 24 * time.Hour).Format(time.RFC3339)},
		"t2": {ID: "t2", TestType: restoreTestTypeQuarterly, Status: "passed", CompletedAt: now.Add(-120 * 24 * time.Hour).Format(time.RFC3339)},
	}
	byType := map[string]CadenceStatus{}
	for _, c := range restoreCadenceStatus(tests, now) {
		byType[c.TestType] = c
	}
	if byType[restoreTestTypeMonthly].Overdue || !byType[restoreTestTypeQuarterly].Overdue || byType[restoreTestTypeIntegrity].LastPassedAt != "" {
		t.Fatalf("cadence = %+v", byType)
	}
}

func TestRestoreTestPersistsAcrossRestart(t *testing.T) {
	fakePostgresTools(t)
	s := newTestServer(t, nil)
	t.Setenv("SCOS_BACKUP_RESTORE_DATABASE_URL", "")
	takeBackup(t, s)
	result := s.executeRestoreTest(context.Background(), restoreTestTypeMonthly, "", "t")
	if result.Status != "failed" || !strings.Contains(result.Detail, "SCOS_BACKUP_RESTORE_DATABASE_URL") {
		t.Fatalf("unexpected result: %+v", result)
	}
	reopened := newBackupServerWithDir(s.backupDir)
	if _, ok := reopened.restoreTestSnapshot()[result.ID]; !ok {
		t.Fatal("expected restore test result to survive server restart")
	}
}

func TestRestoreTestCompleteReturnsTheRecordedResult(t *testing.T) {
	s := newBackupServerWithDir(t.TempDir())
	s.restoreTests["restore-test-1"] = &RestoreTestResult{ID: "restore-test-1", TestType: restoreTestTypeMonthly, Status: "failed", Detail: "checksum mismatch"}
	rec := httptest.NewRecorder()
	newBackupMux(s).ServeHTTP(rec, adminRequest(http.MethodPost, "/v1/restore-tests/complete", `{"test_id":"restore-test-1"}`))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "checksum mismatch") {
		t.Fatalf("complete = %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	newBackupMux(s).ServeHTTP(rec, adminRequest(http.MethodPost, "/v1/restore-tests/complete", `{"test_id":"missing"}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("complete missing = %d", rec.Code)
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

func TestParseTOC(t *testing.T) {
	entries, tables := parseTOC(fullTOC)
	if entries != 5 || !tables["turns"] || tables["scos"] || len(tables) != 4 {
		t.Fatalf("entries=%d tables=%v", entries, tables)
	}
}

// ── OKF git mirror ───────────────────────────────────────────────────────

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func TestGitMirrorPushesAndVerifiesEveryRef(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	repo, remote := t.TempDir(), t.TempDir()
	runGit(t, repo, "init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "policy.md"), []byte("# policy\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "policy.md")
	runGit(t, repo, "-c", "user.name=T", "-c", "user.email=t@example.com", "commit", "-qm", "initial")
	runGit(t, repo, "tag", "v1")
	runGit(t, remote, "init", "-q", "--bare")
	t.Setenv("SCOS_OKF_GIT_REPO", repo)
	t.Setenv("SCOS_OKF_GIT_MIRROR_REMOTE", remote)
	s := newBackupServerWithDir(t.TempDir())
	rec := httptest.NewRecorder()
	newBackupMux(s).ServeHTTP(rec, adminRequest(http.MethodPost, "/v1/git-mirror/push", ``))
	var run GitMirrorRun
	_ = json.Unmarshal(rec.Body.Bytes(), &run)
	if rec.Code != http.StatusOK || run.Status != "verified" || run.Refs != 2 || run.Head == "" || run.Actor != "operator-1" {
		t.Fatalf("mirror run = %d %+v", rec.Code, run)
	}
	// A remote that is a path on this host is not an off-host mirror,
	// however well the push verified.
	plan := s.evaluatePlan(context.Background(), time.Now())
	if plan.GitMirrors || !strings.Contains(plan.Items["remote_git_mirrors"].Reason, "path on this host") {
		t.Fatalf("local-path mirror covered: %+v", plan.Items["remote_git_mirrors"])
	}

	// A broken remote is recorded as a failed run.
	s.config.gitMirrorRemote = filepath.Join(t.TempDir(), "missing.git")
	if run, err := s.runGitMirror(context.Background(), "t"); err != nil || run.Status != "failed" || !strings.Contains(run.Detail, "push --mirror failed") {
		t.Fatalf("broken remote run = %+v %v", run, err)
	}
}

func TestGitMirrorCoverageNeedsAFreshVerifiedOffHostRun(t *testing.T) {
	t.Setenv("SCOS_OKF_GIT_REPO", "/var/lib/scos/okf")
	t.Setenv("SCOS_OKF_GIT_MIRROR_REMOTE", "git@mirror.example.com:scos/okf.git")
	s := newBackupServerWithDir(t.TempDir())
	record := func(run GitMirrorRun) {
		data, _ := json.Marshal(run)
		if err := os.MkdirAll(filepath.Dir(s.gitMirrorStatePath()), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(s.gitMirrorStatePath(), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mirror := func(now time.Time) Coverage {
		return s.evaluatePlan(context.Background(), now).Items["remote_git_mirrors"]
	}
	if c := mirror(time.Now()); c.Covered || !strings.Contains(c.Reason, "no mirror run") {
		t.Fatalf("no run: %+v", c)
	}
	now := time.Now().UTC()
	base := GitMirrorRun{CompletedAt: now.Format(time.RFC3339), Repository: "/var/lib/scos/okf", Remote: "git@mirror.example.com:scos/okf.git", Refs: 3, Head: "abc", Detail: "pushed and verified 3 refs on the remote"}
	failed := base
	failed.Status = "failed"
	record(failed)
	if c := mirror(now); c.Covered {
		t.Fatalf("a failed run covered: %+v", c)
	}
	verified := base
	verified.Status = "verified"
	record(verified)
	if c := mirror(now); !c.Covered || c.LastSuccessAt == "" {
		t.Fatalf("a fresh verified off-host run: %+v", c)
	}
	if c := mirror(now.Add(72 * time.Hour)); c.Covered {
		t.Fatalf("a stale run covered: %+v", c)
	}
	for remote, want := range map[string]bool{"https://git.example.com/okf.git": true, "ssh://git@h/okf": true, "git@h:okf.git": true, "/srv/okf.git": false, "file:///srv/okf.git": false, "": false} {
		if got, _ := remoteIsOffHost(remote); got != want {
			t.Fatalf("remoteIsOffHost(%q) = %v", remote, got)
		}
	}
}

func TestBackupIDsCannotEscapeTheDestination(t *testing.T) {
	for _, id := range []string{"../etc/passwd", "backup-1/../../x", "backup-1.json", "other"} {
		if err := validBackupID(id); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%q accepted", id)
		}
	}
}
