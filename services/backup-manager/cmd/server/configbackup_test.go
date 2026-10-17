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

func writeConfigTree(t *testing.T) (dir, envFile string) {
	t.Helper()
	dir = t.TempDir()
	for name, body := range map[string]string{"tls/tls.crt": "CERT", "tls/tls.key": "KEY", "compose.override.yml": "services: {}\n"} {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	envFile = filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(envFile, []byte("SCOS_SECRET=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, envFile
}

func TestConfigArchiveRoundTripAndTamperDetection(t *testing.T) {
	dir, envFile := writeConfigTree(t)
	archive, files, err := archiveConfig([]string{dir, envFile})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 4 {
		t.Fatalf("files = %+v", files)
	}
	got, err := unpackConfig(archive, files)
	if err != nil {
		t.Fatal(err)
	}
	if string(got[filepath.Join(dir, "tls/tls.key")]) != "KEY" || string(got[envFile]) != "SCOS_SECRET=x\n" {
		t.Fatalf("unpacked = %v", got)
	}
	files[0].SHA256 = strings.Repeat("0", 64)
	if _, err := unpackConfig(archive, files); err == nil {
		t.Fatal("a digest mismatch must be detected")
	}
	if _, _, err := archiveConfig([]string{"relative/path"}); err == nil {
		t.Fatal("a relative configuration path must be refused")
	}
}

func TestConfigurationBackupTakeCoverAndRestore(t *testing.T) {
	fakePostgresTools(t)
	dir, envFile := writeConfigTree(t)
	s := newTestServer(t, nil)
	ctx, now := context.Background(), time.Now()

	// Without configured paths the type is unavailable and not covered.
	rec := httptest.NewRecorder()
	newBackupMux(s).ServeHTTP(rec, adminRequest(http.MethodPost, "/v1/backups/start", `{"type":"configuration_backup"}`))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no paths: %d %s", rec.Code, rec.Body.String())
	}
	if c := s.configCoverage(ctx, true, now); c.Covered || !strings.Contains(c.Reason, "SCOS_CONFIG_BACKUP_PATHS") {
		t.Fatalf("no paths coverage: %+v", c)
	}

	t.Setenv("SCOS_CONFIG_BACKUP_PATHS", dir+","+envFile)
	if !s.configBackupDue(ctx, now) {
		t.Fatal("with paths and no configuration backup, one is due")
	}
	rec = httptest.NewRecorder()
	newBackupMux(s).ServeHTTP(rec, adminRequest(http.MethodPost, "/v1/backups/start", `{"type":"configuration_backup"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("configuration backup: %d %s", rec.Code, rec.Body.String())
	}
	var resp BackupRecord
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Type != backupTypeConfig || resp.Status != "completed" {
		t.Fatalf("record = %+v", resp)
	}
	if s.configBackupDue(ctx, now) {
		t.Fatal("a fresh configuration backup satisfies the interval")
	}
	c := s.configCoverage(ctx, true, now)
	if !c.Covered || !strings.Contains(c.Evidence, "4 files") {
		t.Fatalf("coverage = %+v", c)
	}
	// It does not stand in for a database backup.
	if _, err := s.newestVerified(ctx); err == nil {
		t.Fatal("a configuration backup must not count as a pg_dump backup")
	}

	target := t.TempDir()
	n, err := s.restoreConfig(ctx, resp.ID, target)
	if err != nil || n != 4 {
		t.Fatalf("restore = %d, %v", n, err)
	}
	if b, err := os.ReadFile(filepath.Join(target, envFile)); err != nil || string(b) != "SCOS_SECRET=x\n" {
		t.Fatalf("restored .env = %q, %v", b, err)
	}

	// A configured path missing from the newest backup is reported.
	extra := filepath.Join(t.TempDir(), "extra.yml")
	if err := os.WriteFile(extra, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCOS_CONFIG_BACKUP_PATHS", dir+","+envFile+","+extra)
	if c := s.configCoverage(ctx, true, now); c.Covered || !strings.Contains(c.Reason, extra) {
		t.Fatalf("missing path coverage: %+v", c)
	}
}
