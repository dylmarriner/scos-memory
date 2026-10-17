package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newShippingServer is a test server whose provider encrypts, with a WAL
// archive directory to ship from.
func newShippingServer(t *testing.T) (*backupServer, string, string) {
	t.Helper()
	dest, walDir := t.TempDir(), t.TempDir()
	t.Setenv("SCOS_BACKUP_PROVIDER", "filesystem")
	t.Setenv("SCOS_BACKUP_DESTINATION", dest)
	t.Setenv("SCOS_BACKUP_ENCRYPTION_KEY", strings.Repeat("ab", 32))
	t.Setenv("SCOS_WAL_ARCHIVE_DESTINATION", "file://"+walDir)
	s := newTestServer(t, nil)
	if _, _, ok := s.walShipDir(); !ok {
		t.Fatal("a filesystem provider must ship WAL")
	}
	return s, dest, walDir
}

func TestShipWALEncryptsAndFetchRestores(t *testing.T) {
	s, dest, walDir := newShippingServer(t)
	segment := bytes.Repeat([]byte("WALDATA-"), 1024)
	for name, body := range map[string][]byte{"000000010000000000000003": segment, "00000002.history": []byte("1\t0/3000000\tno recovery target\n"), "000000010000000000000004.tmp.9": []byte("partial")} {
		if err := os.WriteFile(filepath.Join(walDir, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.shipWAL(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("shipped %d, %v; want the segment and the history file, not the temp copy", n, err)
	}
	enc, err := os.ReadFile(filepath.Join(dest, "wal", "000000010000000000000003"+walShipSuffix))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(enc, []byte("WALDATA")) {
		t.Fatal("the shipped copy must not hold WAL in the clear")
	}
	if n, _ := s.shipWAL(context.Background()); n != 0 {
		t.Fatalf("a second pass shipped %d already-shipped files", n)
	}
	out := filepath.Join(t.TempDir(), "RECOVERYXLOG")
	if err := s.fetchShippedWAL("000000010000000000000003", out); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(out); !bytes.Equal(got, segment) {
		t.Fatal("wal-fetch must return the original segment")
	}
	if err := s.fetchShippedWAL("000000010000000000000009", out); err == nil {
		t.Fatal("a segment never shipped must be not found (the end of recovery)")
	}
	if err := s.fetchShippedWAL("../escape", out); err == nil {
		t.Fatal("a name with a path separator must be refused")
	}
	// A tampered copy is detected.
	enc[len(enc)-1] ^= 0xff
	if err := os.WriteFile(filepath.Join(dest, "wal", "000000010000000000000003"+walShipSuffix), enc, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.shippedWALPresent("000000010000000000000003"); err == nil || !strings.Contains(err.Error(), "decrypt") {
		t.Fatalf("tampered copy: %v", err)
	}
}

func TestWALShipCoverage(t *testing.T) {
	s, _, walDir := newShippingServer(t)
	ctx := context.Background()
	now := time.Now()
	s.walState = func(context.Context) (walArchiverState, error) {
		return walArchiverState{ArchiveMode: "on", ArchivedCount: 1, LastArchivedWAL: "000000010000000000000003", LastArchivedTime: &now}, nil
	}
	walOK := Coverage{Covered: true}
	offHost := ProviderReport{OffHost: true, Detail: "fuse.sshfs mount of backup-host"}
	if c := s.walShipCoverage(ctx, walOK, ProviderReport{}); c.Covered || !strings.Contains(c.Reason, "on this host") {
		t.Fatalf("local provider: %+v", c)
	}
	if c := s.walShipCoverage(ctx, walOK, offHost); c.Covered || !strings.Contains(c.Reason, "not been shipped") {
		t.Fatalf("before shipping: %+v", c)
	}
	if err := os.WriteFile(filepath.Join(walDir, "000000010000000000000003"), []byte("wal"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.shipWAL(ctx); err != nil {
		t.Fatal(err)
	}
	if c := s.walShipCoverage(ctx, walOK, offHost); !c.Covered || !strings.Contains(c.Evidence, "000000010000000000000003") {
		t.Fatalf("after shipping: %+v", c)
	}
	if c := s.walShipCoverage(ctx, Coverage{Reason: "archive_mode is off"}, offHost); c.Covered {
		t.Fatalf("without WAL archiving: %+v", c)
	}
}

func TestShippedWALRetentionKeepsWhatAPhysicalBackupNeeds(t *testing.T) {
	s, dest, walDir := newShippingServer(t)
	old := time.Now().AddDate(0, 0, -60)
	for _, name := range []string{"000000010000000000000001", "000000010000000000000005"} {
		if err := os.WriteFile(filepath.Join(walDir, name), []byte("wal"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.shipWAL(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"000000010000000000000001", "000000010000000000000005"} {
		for _, p := range []string{filepath.Join(walDir, name), filepath.Join(dest, "wal", name+walShipSuffix)} {
			if err := os.Chtimes(p, old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	res := RetentionResult{}
	s.applyWALRetention("t", time.Now(), &res, "000000010000000000000005")
	for name, want := range map[string]bool{"000000010000000000000001": false, "000000010000000000000005": true} {
		_, err := os.Stat(filepath.Join(dest, "wal", name+walShipSuffix))
		if (err == nil) != want {
			t.Fatalf("shipped %s present=%v, want %v", name, err == nil, want)
		}
	}
}
