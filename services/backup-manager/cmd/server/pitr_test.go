package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWALFileNameMatchesPostgres(t *testing.T) {
	const seg16MB = 16 << 20
	for lsn, want := range map[string]string{
		"0/2000028":  "000000010000000000000002",
		"0/FF000000": "0000000100000000000000FF",
		"1/A0000060": "0000000100000001000000A0",
	} {
		v, err := parseLSN(lsn)
		if err != nil {
			t.Fatal(err)
		}
		got, err := walFileName(1, v, seg16MB)
		if err != nil || got != want {
			t.Fatalf("walFileName(%s) = %q, %v; want %q", lsn, got, err, want)
		}
	}
	if _, err := parseLSN("2000028"); err == nil {
		t.Fatal("an LSN without a slash must be rejected")
	}
}

func TestBackupWALRangeReadsTheManifest(t *testing.T) {
	wr, err := backupWALRange([]byte(`{"PostgreSQL-Backup-Manifest-Version":2,"WAL-Ranges":[{"Timeline":1,"Start-LSN":"0/5000028","End-LSN":"0/5000138"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if wr.Timeline != 1 || wr.StartLSN != "0/5000028" || wr.EndLSN != "0/5000138" {
		t.Fatalf("range = %+v", wr)
	}
	if _, err := backupWALRange([]byte(`{"WAL-Ranges":[]}`)); err == nil {
		t.Fatal("a manifest without a WAL range must be rejected")
	}
}

func gzipTar(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractDataDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if err := extractDataDirectory(gzipTar(t, map[string]string{"PG_VERSION": "17\n", "base/1/1259": "page"}), dir); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "PG_VERSION")); err != nil || string(b) != "17\n" {
		t.Fatalf("PG_VERSION = %q, %v", b, err)
	}
	if info, err := os.Stat(filepath.Join(dir, "pg_wal", "archive_status")); err != nil || !info.IsDir() {
		t.Fatal("pg_wal/archive_status must exist for recovery")
	}
	if info, _ := os.Stat(dir); info.Mode().Perm() != 0o700 {
		t.Fatalf("data directory mode %v, PostgreSQL requires 0700", info.Mode().Perm())
	}
	if err := extractDataDirectory(gzipTar(t, map[string]string{"../escape": "x"}), filepath.Join(t.TempDir(), "d")); err == nil {
		t.Fatal("an entry escaping the data directory must be rejected")
	}
}

func TestTarFilesRoundTrip(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{"base.tar.gz": "gz", "backup_manifest": "{}"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	archive, err := tarFiles(dir, "base.tar.gz", "backup_manifest")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := fileFromTar(archive, "backup_manifest"); err != nil || string(got) != "{}" {
		t.Fatalf("backup_manifest = %q, %v", got, err)
	}
	if _, err := tarFiles(dir, "missing"); err == nil {
		t.Fatal("a missing pg_basebackup output file must be an error")
	}
}

// storePhysical stores a physical base backup manifest the way
// createPhysicalBackup does.
func storePhysical(t *testing.T, s *backupServer, id, walStart string, at time.Time) {
	t.Helper()
	archive := []byte("tar " + id)
	m := backupManifest{ID: id, Type: backupTypePhysical, Format: formatPhysical, ContentHash: hashBytes(archive), SizeBytes: int64(len(archive)),
		CreatedAt: at.UTC().Format(time.RFC3339), WALTimeline: 1, WALStart: walStart, WALEnd: walStart}
	if _, err := s.provider.Store(context.Background(), m, archive); err != nil {
		t.Fatal(err)
	}
}

func TestPITRCoverageNeedsEvidenceAtEveryStep(t *testing.T) {
	walDir := t.TempDir()
	t.Setenv("SCOS_WAL_ARCHIVE_DESTINATION", "file://"+walDir)
	s := newTestServer(t, nil)
	ctx, now := context.Background(), time.Now()
	walOK := Coverage{Covered: true}

	if c := s.pitrCoverage(ctx, Coverage{Reason: "archive_mode is off"}, now); c.Covered || !strings.Contains(c.Reason, "WAL archiving") {
		t.Fatalf("without WAL archiving: %+v", c)
	}
	if c := s.pitrCoverage(ctx, walOK, now); c.Covered || !strings.Contains(c.Reason, "physical base backup") {
		t.Fatalf("without a physical backup: %+v", c)
	}
	start := "000000010000000000000007"
	storePhysical(t, s, "backup-1", start, now.Add(-time.Hour))
	if c := s.pitrCoverage(ctx, walOK, now); c.Covered || !strings.Contains(c.Reason, start) {
		t.Fatalf("with the starting segment missing from the archive: %+v", c)
	}
	if err := os.WriteFile(filepath.Join(walDir, start), []byte("wal"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c := s.pitrCoverage(ctx, walOK, now); c.Covered || !strings.Contains(c.Reason, "pitr_recovery") {
		t.Fatalf("without a passing drill: %+v", c)
	}
	s.restoreTests["old"] = &RestoreTestResult{ID: "old", TestType: restoreTestTypePITR, Status: "passed", CompletedAt: now.Add(-8 * 24 * time.Hour).Format(time.RFC3339)}
	if c := s.pitrCoverage(ctx, walOK, now); c.Covered || !strings.Contains(c.Reason, "cadence") {
		t.Fatalf("with only a drill outside its cadence: %+v", c)
	}
	s.restoreTests["new"] = &RestoreTestResult{ID: "new", TestType: restoreTestTypePITR, Status: "passed", CompletedAt: now.Add(-time.Hour).Format(time.RFC3339)}
	c := s.pitrCoverage(ctx, walOK, now)
	if !c.Covered || !strings.Contains(c.Evidence, "backup-1") || c.LastSuccessAt == "" {
		t.Fatalf("with every piece of evidence: %+v", c)
	}
}

func TestPhysicalAndLogicalBackupsStayApart(t *testing.T) {
	fakePostgresTools(t)
	restored := testFingerprint()
	s := newTestServer(t, &restored)
	logical := takeBackup(t, s)
	storePhysical(t, s, "backup-physical", "000000010000000000000003", time.Now().Add(time.Minute))

	m, err := s.newestVerified(context.Background())
	if err != nil || m.ID != logical.ID {
		t.Fatalf("newest pg_dump backup = %q, %v; want %q (the newer physical one must not count)", m.ID, err, logical.ID)
	}
	pm, err := s.newestVerifiedOfType(context.Background(), backupTypePhysical)
	if err != nil || pm.ID != "backup-physical" {
		t.Fatalf("newest physical backup = %q, %v", pm.ID, err)
	}
	if _, err := s.runDrill(context.Background(), restoreTestTypeMonthly, pm, nil); err == nil || !strings.Contains(err.Error(), "physical") {
		t.Fatalf("a pg_restore drill against a physical backup must refuse, got %v", err)
	}
	if _, err := s.pitrDrill(context.Background(), m, nil); err == nil || !strings.Contains(err.Error(), backupTypePhysical) {
		t.Fatalf("a PITR drill against a pg_dump backup must refuse, got %v", err)
	}
	// Taken the same day, neither type's retention expires the other's.
	res, err := s.applyRetention(context.Background(), "t", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Deleted) != 0 || len(res.Kept) != 2 {
		t.Fatalf("retention kept %v, deleted %v", res.Kept, res.Deleted)
	}
}

func TestWALRetentionKeepsWhatAPhysicalBackupNeeds(t *testing.T) {
	fakePostgresTools(t)
	walDir := t.TempDir()
	t.Setenv("SCOS_WAL_ARCHIVE_DESTINATION", "file://"+walDir)
	s := newTestServer(t, nil)
	old := time.Now().AddDate(0, 0, -60)
	for _, name := range []string{"000000010000000000000001", "000000010000000000000005", "000000010000000000000006"} {
		path := filepath.Join(walDir, name)
		if err := os.WriteFile(path, []byte("wal"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	// A physical backup kept by the monthly tier starts at segment 5.
	storePhysical(t, s, "backup-physical", "000000010000000000000005", time.Now().AddDate(0, 0, -50))
	if _, err := s.applyRetention(context.Background(), "t", time.Now()); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"000000010000000000000001": false, "000000010000000000000005": true, "000000010000000000000006": true} {
		_, err := os.Stat(filepath.Join(walDir, name))
		if (err == nil) != want {
			t.Fatalf("%s present=%v, want %v", name, err == nil, want)
		}
	}
}

func TestBackupStartAcceptsOnlyKnownTypes(t *testing.T) {
	fakePostgresTools(t)
	s := newTestServer(t, nil)
	rec := httptest.NewRecorder()
	newBackupMux(s).ServeHTTP(rec, adminRequest(http.MethodPost, "/v1/backups/start", `{"type":"incremental"}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown type: %d", rec.Code)
	}
	// The test server's archiver reports archive_mode off, so a physical
	// backup is refused as unrecoverable rather than taken.
	rec = httptest.NewRecorder()
	newBackupMux(s).ServeHTTP(rec, adminRequest(http.MethodPost, "/v1/backups/start", `{"type":"physical_base_backup"}`))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "WAL archiving") {
		t.Fatalf("physical backup without WAL archiving: %d %s", rec.Code, rec.Body.String())
	}
}
