package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeSegments(t *testing.T, dir string, size int, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestContiguousArchivedWALCrossesALogBoundary(t *testing.T) {
	dir := t.TempDir()
	const size = 16 << 20 // 256 segments per log
	writeSegments(t, dir, size,
		"0000000100000001000000FE", "0000000100000001000000FF",
		"000000010000000200000000", "000000010000000200000001",
		// Before the start and on another timeline: ignored.
		"0000000100000001000000FD", "000000020000000200000002")
	through, n, err := contiguousArchivedWAL(dir, "0000000100000001000000FE")
	if err != nil || through != "000000010000000200000001" || n != 4 {
		t.Fatalf("got (%s, %d, %v), want (000000010000000200000001, 4, nil)", through, n, err)
	}
}

func TestContiguousArchivedWALRefusesAGap(t *testing.T) {
	dir := t.TempDir()
	writeSegments(t, dir, 16<<20, "000000010000000400000010", "000000010000000400000011", "000000010000000400000013")
	if _, _, err := contiguousArchivedWAL(dir, "000000010000000400000010"); err == nil || !strings.Contains(err.Error(), "gap before 000000010000000400000013") {
		t.Fatalf("gap not refused: %v", err)
	}
	if _, _, err := contiguousArchivedWAL(dir, "00000001000000040000000F"); err == nil || !strings.Contains(err.Error(), "where the backup starts") {
		t.Fatalf("missing start not refused: %v", err)
	}
}

// storePhysicalArchive stores a physical backup whose archive holds a real
// base.tar.gz, as createPhysicalBackup does.
func storePhysicalArchive(t *testing.T, s *backupServer, id, walStart string, at time.Time, files map[string]string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "base.tar.gz"), gzipTar(t, files), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "backup_manifest"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive, err := tarFiles(dir, "base.tar.gz", "backup_manifest")
	if err != nil {
		t.Fatal(err)
	}
	m := backupManifest{ID: id, Type: backupTypePhysical, Format: formatPhysical, ContentHash: hashBytes(archive), SizeBytes: int64(len(archive)),
		CreatedAt: at.UTC().Format(time.RFC3339), WALTimeline: 1, WALStart: walStart, WALEnd: walStart}
	if _, err := s.provider.Store(context.Background(), m, archive); err != nil {
		t.Fatal(err)
	}
}

func TestPITRPrepareUsesTheNewestBackupBeforeTheTarget(t *testing.T) {
	walDir := t.TempDir()
	t.Setenv("SCOS_WAL_ARCHIVE_DESTINATION", "file://"+walDir)
	s := newTestServer(t, nil)
	target := time.Date(2026, 10, 21, 9, 33, 50, 0, time.UTC)
	storePhysicalArchive(t, s, "backup-physical-old", "000000010000000400000001", target.Add(-48*time.Hour), map[string]string{"PG_VERSION": "17\n", "which": "old"})
	storePhysicalArchive(t, s, "backup-physical-chosen", "000000010000000400000005", target.Add(-2*time.Hour), map[string]string{"PG_VERSION": "17\n", "which": "chosen"})
	// Taken after the target: recovery cannot start from it.
	storePhysicalArchive(t, s, "backup-physical-after", "000000010000000400000009", target.Add(time.Hour), map[string]string{"PG_VERSION": "17\n", "which": "after"})
	writeSegments(t, walDir, 16<<20, "000000010000000400000005", "000000010000000400000006", "000000010000000400000007")

	data := filepath.Join(t.TempDir(), "restore")
	p, err := s.pitrPrepare(context.Background(), target, data, "/var/lib/scos/wal-archive")
	if err != nil {
		t.Fatal(err)
	}
	if p.BackupID != "backup-physical-chosen" || p.Segments != 3 || p.ArchivedThrough != "000000010000000400000007" {
		t.Fatalf("preparation = %+v", p)
	}
	if got, _ := os.ReadFile(filepath.Join(data, "which")); string(got) != "chosen" {
		t.Fatalf("restored the wrong backup: %q", got)
	}
	if _, err := os.Stat(filepath.Join(data, "recovery.signal")); err != nil {
		t.Fatal("recovery.signal missing")
	}
	conf, _ := os.ReadFile(filepath.Join(data, "postgresql.auto.conf"))
	for _, want := range []string{
		`restore_command = 'cp ''/var/lib/scos/wal-archive/%f'' ''%p'''`,
		`recovery_target_time = '2026-10-21 09:33:50.000000+00'`,
		`recovery_target_action = 'promote'`,
	} {
		if !strings.Contains(string(conf), want) {
			t.Fatalf("postgresql.auto.conf lacks %s:\n%s", want, conf)
		}
	}
	// A non-empty directory is never overwritten.
	if _, err := s.pitrPrepare(context.Background(), target, data, ""); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("non-empty directory was not refused: %v", err)
	}
	// A gap in the archive refuses before anything is written.
	if err := os.Remove(filepath.Join(walDir, "000000010000000400000006")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pitrPrepare(context.Background(), target, filepath.Join(t.TempDir(), "r2"), ""); err == nil || !strings.Contains(err.Error(), "gap") {
		t.Fatalf("gap was not refused: %v", err)
	}
}
