package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

// A failed write after one file was inserted must roll the whole snapshot
// back, including activation. Retrying the same immutable key after marking
// the interrupted generation failed must produce a complete new generation.
func TestPGSnapshotIndexRollbackAndRetry(t *testing.T) {
	store, db := testPGStore(t)
	ctx := t.Context()
	tenant := testTenant(t, db)
	repo, err := store.EnsureRepository(ctx, tenant, "/p1-recovery/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	original, err := store.CreateSnapshot(ctx, repo, tenant, "main", "commit-1", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, original, repo, []PGFileInput{{Path: "original.txt", Language: "text", ContentHash: hashContent("original")}}); err != nil {
		t.Fatal(err)
	}

	interrupted, err := store.CreateSnapshot(ctx, repo, tenant, "main", "commit-2", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	duplicate := []PGFileInput{
		{Path: "same.txt", Language: "text", ContentHash: hashContent("first")},
		{Path: "same.txt", Language: "text", ContentHash: hashContent("second")},
	}
	if err := store.IndexSnapshot(ctx, interrupted, repo, duplicate); err == nil {
		t.Fatal("duplicate paths must fail indexing")
	}
	var active, status string
	if err := db.QueryRowContext(ctx, "SELECT active_snapshot_id::text FROM code_repositories WHERE id=$1", repo).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != original {
		t.Fatalf("failed indexing changed active snapshot to %s, want %s", active, original)
	}
	if err := db.QueryRowContext(ctx, "SELECT status::text FROM code_repository_snapshots WHERE id=$1", interrupted).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "queued" {
		t.Fatalf("partial snapshot became %s", status)
	}
	var files, symbols int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM code_file_versions WHERE snapshot_id=$1", interrupted).Scan(&files); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM code_symbol_versions WHERE snapshot_id=$1", interrupted).Scan(&symbols); err != nil {
		t.Fatal(err)
	}
	if files != 0 || symbols != 0 {
		t.Fatalf("partial rows persisted: files=%d symbols=%d", files, symbols)
	}

	if err := store.SetSnapshotStatus(ctx, interrupted, "failed", "interrupted during indexing"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteFailedSnapshot(ctx, interrupted); err != nil {
		t.Fatal(err)
	}
	retried, err := store.CreateSnapshot(ctx, repo, tenant, "main", "commit-2", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if retried == interrupted {
		t.Fatal("retry reused failed generation")
	}
	if err := store.IndexSnapshot(ctx, retried, repo, []PGFileInput{{Path: "complete.txt", Language: "text", ContentHash: hashContent("complete")}}); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, "SELECT active_snapshot_id::text FROM code_repositories WHERE id=$1", repo).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != retried {
		t.Fatalf("retry did not activate complete generation: %s", active)
	}
	if err := db.QueryRowContext(ctx, "SELECT status::text FROM code_repository_snapshots WHERE id=$1", original).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "superseded" {
		t.Fatalf("old active generation status = %s", status)
	}
}

// The internal reactivation operation must not pretend a queued, failed or
// nonexistent generation was successfully activated.
func TestPGReactivateRejectsIncompleteSnapshot(t *testing.T) {
	store, db := testPGStore(t)
	ctx := t.Context()
	tenant := testTenant(t, db)
	repo, err := store.EnsureRepository(ctx, tenant, "/p1-reactivate/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	active, err := store.CreateSnapshot(ctx, repo, tenant, "main", "active", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, active, repo, []PGFileInput{{Path: "active.txt", ContentHash: hashContent("active")}}); err != nil {
		t.Fatal(err)
	}
	queued, err := store.CreateSnapshot(ctx, repo, tenant, "main", "queued", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ReactivateSnapshot(ctx, queued); err == nil {
		t.Fatal("queued generation reactivation falsely succeeded")
	}
	if err := store.SetSnapshotStatus(ctx, queued, "failed", "interrupted"); err != nil {
		t.Fatal(err)
	}
	if err := store.ReactivateSnapshot(ctx, queued); err == nil {
		t.Fatal("failed generation reactivation falsely succeeded")
	}
	if err := store.ReactivateSnapshot(ctx, "00000000-0000-0000-0000-000000000001"); err == nil {
		t.Fatal("missing generation reactivation falsely succeeded")
	}
	if err := store.ReactivateSnapshot(ctx, active); err != nil {
		t.Fatalf("active generation must remain idempotent: %v", err)
	}
	var current string
	if err := db.QueryRowContext(ctx, "SELECT active_snapshot_id::text FROM code_repositories WHERE id=$1", repo).Scan(&current); err != nil {
		t.Fatal(err)
	}
	if current != active {
		t.Fatalf("invalid reactivation changed active snapshot: %s", current)
	}
}

// Retrying a workspace scan while its generation is queued/indexing must
// report in-progress rather than the success-shaped unchanged response.
func TestPGScanDoesNotReportIncompleteSnapshotAsUnchanged(t *testing.T) {
	store, db := testPGStore(t)
	ctx := t.Context()
	tenant := testTenant(t, db)
	root := t.TempDir()
	repoDir := filepath.Join(root, "repo")
	writeFile(t, filepath.Join(repoDir, "main.go"), "package main\nfunc main() {}\n")
	t.Setenv("SCOS_WORKSPACE_ROOT", root)
	t.Setenv("SCOS_WORKSPACE_HOST_PATH", "/host-workspace")
	repo, err := store.EnsureRepository(ctx, tenant, "/host-workspace/repo")
	if err != nil {
		t.Fatal(err)
	}
	files, _, err := scanRepository(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.CreateSnapshot(ctx, repo, tenant, "workspace", workingTreeID(files), extractorVersion, "")
	if err != nil {
		t.Fatal(err)
	}
	srv := &server{pgStore: store}
	check := func(expectedStatus string) {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"repository_id": repo})
		req := httptest.NewRequest(http.MethodPost, "/v2/code/scan", bytes.NewReader(body))
		req = req.WithContext(auth.ContextWithClaims(req.Context(), auth.Claims{TenantID: tenant, PrincipalID: "p"}))
		w := httptest.NewRecorder()
		srv.handlePGScan(w, req)
		if w.Code != http.StatusAccepted {
			t.Fatalf("%s scan returned %d instead of 202: %s", expectedStatus, w.Code, w.Body.String())
		}
		var response map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response["snapshot_id"] != snapshot || response["status"] != expectedStatus || response["unchanged"] == true {
			t.Fatalf("incomplete %s scan falsely completed: %v", expectedStatus, response)
		}
	}
	check("queued")
	if err := store.SetSnapshotStatus(ctx, snapshot, "indexing", ""); err != nil {
		t.Fatal(err)
	}
	check("indexing")
}

// Startup recovery only fails stale, incomplete generations; it must never
// affect the currently active snapshot or re-fail already failed rows.
func TestPGFailInterruptedSnapshotsPreservesActive(t *testing.T) {
	store, db := testPGStore(t)
	ctx := t.Context()
	tenant := testTenant(t, db)
	repo, err := store.EnsureRepository(ctx, tenant, "/p1-interrupted/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	active, err := store.CreateSnapshot(ctx, repo, tenant, "main", "active", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, active, repo, []PGFileInput{{Path: "active.txt", ContentHash: hashContent("active")}}); err != nil {
		t.Fatal(err)
	}
	pending, err := store.CreateSnapshot(ctx, repo, tenant, "main", "interrupted", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "UPDATE code_repository_snapshots SET created_at=now()-interval '2 minutes' WHERE id=$1", pending); err != nil {
		t.Fatal(err)
	}
	affected, err := store.FailInterruptedSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if affected < 1 {
		t.Fatal("stale queued snapshot was not marked failed")
	}
	var status, current string
	if err := db.QueryRowContext(ctx, "SELECT status::text FROM code_repository_snapshots WHERE id=$1", pending).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "failed" {
		t.Fatalf("interrupted status = %s", status)
	}
	if err := db.QueryRowContext(ctx, "SELECT active_snapshot_id::text FROM code_repositories WHERE id=$1", repo).Scan(&current); err != nil {
		t.Fatal(err)
	}
	if current != active {
		t.Fatalf("recovery disturbed active generation: %s", current)
	}
	again, err := store.FailInterruptedSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if again != 0 {
		t.Fatalf("recovery repeated %d updates", again)
	}
}

// A repository can have one active snapshot per branch even though its
// convenience active_snapshot_id points at the most recently activated
// branch. Idempotent reactivation must not reject the other active branch.
func TestPGReactivateIsIdempotentAcrossBranches(t *testing.T) {
	store, db := testPGStore(t)
	ctx := t.Context()
	tenant := testTenant(t, db)
	repo, err := store.EnsureRepository(ctx, tenant, "/p1-branches/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	main, err := store.CreateSnapshot(ctx, repo, tenant, "main", "main-1", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, main, repo, []PGFileInput{{Path: "main.txt", ContentHash: hashContent("main")}}); err != nil {
		t.Fatal(err)
	}
	feature, err := store.CreateSnapshot(ctx, repo, tenant, "feature", "feature-1", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, feature, repo, []PGFileInput{{Path: "feature.txt", ContentHash: hashContent("feature")}}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReactivateSnapshot(ctx, main); err != nil {
		t.Fatalf("active main branch rejected: %v", err)
	}
	if err := store.ReactivateSnapshot(ctx, feature); err != nil {
		t.Fatalf("active feature branch rejected: %v", err)
	}
}

// Reactivation of a genuinely superseded generation must restore its exact
// historical query surface without mutating the newer snapshot's symbols.
func TestPGReactivateSupersededSnapshotRestoresQueries(t *testing.T) {
	store, db := testPGStore(t)
	ctx := t.Context()
	tenant := testTenant(t, db)
	repo, err := store.EnsureRepository(ctx, tenant, "/p1-restore/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.CreateSnapshot(ctx, repo, tenant, "main", "first", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, first, repo, []PGFileInput{{Path: "first.txt", ContentHash: hashContent("first")}}); err != nil {
		t.Fatal(err)
	}
	second, err := store.CreateSnapshot(ctx, repo, tenant, "main", "second", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, second, repo, []PGFileInput{{Path: "second.txt", ContentHash: hashContent("second")}}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReactivateSnapshot(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := store.ReactivateSnapshot(ctx, first); err != nil {
		t.Fatalf("repeat reactivation: %v", err)
	}
	restored, err := store.QueryByPathOrFQN(ctx, repo, "main", "first.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(restored) != 1 || restored[0].SnapshotID != first {
		t.Fatalf("historical query not restored: %+v", restored)
	}
	hidden, err := store.QueryByPathOrFQN(ctx, repo, "main", "second.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(hidden) != 0 {
		t.Fatalf("superseded snapshot leaked into active queries: %+v", hidden)
	}
	var count int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM code_symbol_versions WHERE snapshot_id=$1", second).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("reactivation mutated immutable second snapshot: %d rows", count)
	}
}
