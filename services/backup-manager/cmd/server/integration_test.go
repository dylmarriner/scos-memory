//go:build integration

package main

import (
	"context"
	"os"
	"testing"
)

// TestLocalBackupRoundTripAgainstPostgres takes a real pg_dump as the
// scos_backup role and restores it into a temporary database. It needs
// SCOS_BACKUP_DATABASE_URL (scos_backup), SCOS_BACKUP_RESTORE_DATABASE_URL
// (a CREATEDB role on the maintenance database), and, for a non-superuser
// restore role, SCOS_BACKUP_RESTORE_TEMPLATE.
func TestLocalBackupRoundTripAgainstPostgres(t *testing.T) {
	if os.Getenv("SCOS_BACKUP_DATABASE_URL") == "" || os.Getenv("SCOS_BACKUP_RESTORE_DATABASE_URL") == "" {
		t.Skip("SCOS_BACKUP_DATABASE_URL and SCOS_BACKUP_RESTORE_DATABASE_URL are required")
	}
	t.Setenv("SCOS_BACKUP_PROVIDER", "local")
	s := newBackupServerWithDir(t.TempDir())
	if err := localToolingError(); err != nil {
		t.Fatal(err)
	}
	b, err := s.createLocalBackup(context.Background(), "base_backup")
	if err != nil {
		t.Fatal(err)
	}
	if b.SizeBytes == 0 || b.Status != "completed" {
		t.Fatalf("backup = %+v", b)
	}
	for _, testType := range []string{restoreTestTypeIntegrity, restoreTestTypeMonthly} {
		if r := s.executeRestoreTest(testType, b.ID); r.Status != "passed" {
			t.Fatalf("%s drill = %+v", testType, r)
		}
	}
}
