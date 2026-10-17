package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBaseBackupCarriesAMigrationExport(t *testing.T) {
	fakePostgresTools(t)
	migrations, policies := t.TempDir(), t.TempDir()
	for dir, names := range map[string][]string{migrations: {"000001_initial", "000002_tables"}, policies: {"000001_rls"}} {
		for _, n := range names {
			if err := os.WriteFile(filepath.Join(dir, n+".sql"), []byte("-- "+n+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	restored := testFingerprint()
	s := newTestServer(t, &restored)
	ctx := context.Background()
	applied := []string{"000001_initial", "000001_rls", "000002_tables"}
	s.applied = func(context.Context, string) ([]string, error) { return applied, nil }

	// Without export paths a backup is still taken; the item says why it is
	// not covered.
	takeBackup(t, s)
	plan := s.evaluatePlan(ctx, time.Now())
	if c := plan.Items["schema_and_migration_exports"]; c.Covered || !strings.Contains(c.Reason, "SCOS_MIGRATION_EXPORT_PATHS") {
		t.Fatalf("without paths: %+v", c)
	}

	t.Setenv("SCOS_MIGRATION_EXPORT_PATHS", migrations+","+policies)
	rec := httptest.NewRecorder()
	newBackupMux(s).ServeHTTP(rec, adminRequest(http.MethodPost, "/v1/backups/start", `{}`))
	var resp backupStartResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("backup: %d %s", rec.Code, rec.Body.String())
	}
	if resp.MigrationExport == "" || resp.MigrationExportError != "" {
		t.Fatalf("migration export = %q, error %q", resp.MigrationExport, resp.MigrationExportError)
	}
	c := s.evaluatePlan(ctx, time.Now()).Items["schema_and_migration_exports"]
	if !c.Covered || !strings.Contains(c.Evidence, "3 migration and policy files") {
		t.Fatalf("with an export: %+v", c)
	}
	// It restores like a configuration backup.
	if n, err := s.restoreConfig(ctx, resp.MigrationExport, t.TempDir()); err != nil || n != 3 {
		t.Fatalf("restore export = %d, %v", n, err)
	}
	// The export does not count as the base backup.
	if m, _ := s.newestVerified(ctx); m.Type != backupTypeLogical {
		t.Fatalf("newest pg_dump backup has type %q", m.Type)
	}

	// A migration applied with no file to export is reported, not hidden.
	applied = append(applied, "000003_missing")
	rec = httptest.NewRecorder()
	newBackupMux(s).ServeHTTP(rec, adminRequest(http.MethodPost, "/v1/backups/start", `{}`))
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !strings.Contains(resp.MigrationExportError, "000003_missing") {
		t.Fatalf("missing file not reported: %+v", resp)
	}
	if c := s.evaluatePlan(ctx, time.Now()).Items["schema_and_migration_exports"]; c.Covered || !strings.Contains(c.Reason, "000003_missing") {
		t.Fatalf("missing file coverage: %+v", c)
	}
}
