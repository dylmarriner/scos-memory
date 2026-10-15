//go:build integration

package main

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// TestBackupAndDrillsAgainstPostgreSQL takes a real encrypted backup of the
// database at DATABASE_URL (the owner connection pg_dump needs) with the
// filesystem provider, then runs each drill type against it with the real
// PostgreSQL client tools: the archive's table of contents, a restore with
// key-table validation, and a full restore with every table and the
// schema catalog compared against the manifest. The plan must then report
// base backups covered and nothing it cannot evidence.
func TestBackupAndDrillsAgainstPostgreSQL(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Fatal("DATABASE_URL (owner connection) must be set")
	}
	for _, tool := range []string{"pg_dump", "pg_restore", "createdb", "dropdb"} {
		if _, err := lookPath(tool); err != nil {
			t.Fatalf("%s is required: %v", tool, err)
		}
	}
	u, err := url.Parse(dbURL)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/postgres"
	t.Setenv("SCOS_BACKUP_DATABASE_URL", dbURL)
	t.Setenv("SCOS_BACKUP_RESTORE_DATABASE_URL", u.String())
	t.Setenv("SCOS_BACKUP_PROVIDER", "filesystem")
	t.Setenv("SCOS_BACKUP_DESTINATION", t.TempDir())
	t.Setenv("SCOS_BACKUP_ENCRYPTION_KEY", strings.Repeat("i", 32))
	t.Setenv("SCOS_WAL_ARCHIVE_DESTINATION", "file://"+t.TempDir())
	s := newBackupServerWithDir(t.TempDir())
	if s.provider == nil {
		t.Fatal(s.providerErr)
	}
	ctx := context.Background()

	s.opMu.Lock()
	resp, err := s.createBackup(ctx, "integration-test")
	s.opMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Encrypted || resp.TablesFingerprinted < 50 || resp.Retention == nil {
		t.Fatalf("backup = %+v retention=%+v", resp.BackupRecord, resp.Retention)
	}
	m, _, _, err := s.provider.Load(ctx, resp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Fingerprint.Tables["tenants"]; !ok || m.Fingerprint.Catalog.Hash == "" || m.Fingerprint.Catalog.Policies == 0 {
		t.Fatalf("manifest fingerprint = %+v", m.Fingerprint.Catalog)
	}

	for _, drill := range []string{restoreTestTypeIntegrity, restoreTestTypeMonthly, restoreTestTypeQuarterly} {
		start := time.Now()
		r := s.executeRestoreTest(ctx, drill, resp.ID, "integration-test")
		if r.Status != "passed" {
			t.Fatalf("%s: %s\n%s", drill, r.Detail, strings.Join(r.Validations, "\n"))
		}
		t.Logf("%s passed in %s: %s", drill, time.Since(start).Round(time.Millisecond), r.Validations[len(r.Validations)-1])
	}

	plan := s.evaluatePlan(ctx, time.Now())
	if !plan.BaseBackups || !plan.Items["schema_exports"].Covered {
		t.Fatalf("base backups = %+v", plan.Items["base_backups"])
	}
	// The test database archives no WAL, and a temp directory is not
	// another host: both must say so.
	if plan.PostgreSQLWAL || plan.Items["postgresql_wal_archiving"].Reason == "" {
		t.Fatalf("WAL = %+v", plan.Items["postgresql_wal_archiving"])
	}
	if plan.EncryptedOffHost || plan.PITRConfigured {
		t.Fatalf("plan claims coverage it has no evidence for: %+v", plan.Items)
	}
	t.Logf("WAL: %s", plan.Items["postgresql_wal_archiving"].Reason)
}
