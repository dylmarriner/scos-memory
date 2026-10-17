//go:build integration

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestSyncDetectsAnIndexEditedWhileRunning: an edit made to okf_concepts
// after the synchroniser loaded it is reported as diverged on the next
// sync and is not overwritten by the sync's write-back.
// Requires SCOS_APP_TEST_DATABASE_URL (and DATABASE_URL to clean up).
func TestSyncDetectsAnIndexEditedWhileRunning(t *testing.T) {
	adminURL := os.Getenv("DATABASE_URL")
	appURL := os.Getenv("SCOS_APP_TEST_DATABASE_URL")
	if adminURL == "" || appURL == "" {
		t.Skip("DATABASE_URL and SCOS_APP_TEST_DATABASE_URL must both be set for this test")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	config, err := pgxpool.ParseConfig(appURL)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["scos.service_id"] = "okf-synchroniser"
	config.ConnConfig.RuntimeParams["scos.principal_id"] = "okf-synchroniser"
	app, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()

	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "okf-it-"+hex.EncodeToString(suffix))
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "policy.md")
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), `DELETE FROM okf_concepts WHERE path = $1`, path) })
	if err := os.WriteFile(path, []byte("# Policy\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &okfServer{db: app, documents: map[string]*OKFDocument{}, projections: map[string]*IndexProjection{},
		okfRoot: root, statePath: filepath.Join(t.TempDir(), "projections.json")}
	if first := s.synchronise(ctx); len(first.Added) != 1 || len(first.Errors) != 0 {
		t.Fatalf("first sync = %#v", first)
	}
	if _, err := admin.Exec(ctx, `UPDATE okf_concepts SET content = '# Policy edited in the index' WHERE path = $1`, path); err != nil {
		t.Fatal(err)
	}
	second := s.synchronise(ctx)
	if len(second.Diverged) != 1 || second.Diverged[0] != path {
		t.Fatalf("out-of-band index edit not reported diverged: %#v", second)
	}
	var content, status string
	if err := admin.QueryRow(ctx, `SELECT content, status FROM okf_concepts WHERE path = $1`, path).Scan(&content, &status); err != nil {
		t.Fatal(err)
	}
	if content != "# Policy edited in the index" || status != "diverged" {
		t.Fatalf("sync overwrote the edited index row: content %q status %q", content, status)
	}
}
