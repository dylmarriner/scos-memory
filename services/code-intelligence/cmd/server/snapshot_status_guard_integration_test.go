package main

import "testing"

// A late worker error must not corrupt an immutable generation after another
// scan has already superseded it. Historical generations remain reactivatable.
func TestPGSetSnapshotStatusPreservesSupersededGeneration(t *testing.T) {
	store, db := testPGStore(t)
	ctx := t.Context()
	tenant := testTenant(t, db)
	repo, err := store.EnsureRepository(ctx, tenant, "/p1-status-guard/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.CreateSnapshot(ctx, repo, tenant, "main", "first", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, first, repo, nil); err != nil {
		t.Fatal(err)
	}
	second, err := store.CreateSnapshot(ctx, repo, tenant, "main", "second", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, second, repo, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSnapshotStatus(ctx, first, "failed", "late worker error"); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := db.QueryRowContext(ctx, "SELECT status::text FROM code_repository_snapshots WHERE id=$1", first).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "superseded" {
		t.Fatalf("late worker failure overwrote historical generation status: %s", status)
	}
	foreignTenant := testTenant(t, db)
	if err := store.ReactivateSnapshotScoped(ctx, foreignTenant, first); err == nil {
		t.Fatal("foreign tenant reactivated a superseded snapshot")
	}
	if err := store.ReactivateSnapshotScoped(ctx, tenant, first); err != nil {
		t.Fatalf("historical generation must remain reactivatable: %v", err)
	}
	var active string
	if err := db.QueryRowContext(ctx, "SELECT active_snapshot_id::text FROM code_repositories WHERE id=$1", repo).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != first {
		t.Fatalf("reactivated generation=%s, want %s", active, first)
	}
}

// Pending snapshots still need to transition to failed for retry and recovery.
func TestPGSetSnapshotStatusStillFailsPendingGeneration(t *testing.T) {
	store, db := testPGStore(t)
	ctx := t.Context()
	tenant := testTenant(t, db)
	repo, err := store.EnsureRepository(ctx, tenant, "/p1-pending-status/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	pending, err := store.CreateSnapshot(ctx, repo, tenant, "main", "pending", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetSnapshotStatus(ctx, pending, "failed", "interrupted"); err != nil {
		t.Fatal(err)
	}
	var status, failure string
	if err := db.QueryRowContext(ctx, "SELECT status::text, error FROM code_repository_snapshots WHERE id=$1", pending).Scan(&status, &failure); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || failure != "interrupted" {
		t.Fatalf("pending snapshot recovery lost: status=%s error=%q", status, failure)
	}
}
