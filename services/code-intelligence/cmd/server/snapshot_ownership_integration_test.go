package main

import (
	"strconv"
	"testing"
)

// A caller-supplied repository ID must not be able to attach a different
// tenant's files/symbols to an otherwise valid immutable snapshot.
func TestPGIndexSnapshotRejectsForeignRepository(t *testing.T) {
	store, db := testPGStore(t)
	ctx := t.Context()
	owner := testTenant(t, db)
	other := testTenant(t, db)
	ownerRepo, err := store.EnsureRepository(ctx, owner, "/p1-owner/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	otherRepo, err := store.EnsureRepository(ctx, other, "/p1-other/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.CreateSnapshot(ctx, ownerRepo, owner, "main", "first", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}

	err = store.IndexSnapshot(ctx, snapshot, otherRepo, []PGFileInput{{
		Path: "foreign.go", Language: "go",
		Content:     "package foreign\nfunc Exfiltrate() {}\n",
		ContentHash: hashContent("package foreign\nfunc Exfiltrate() {}\n"),
	}})
	if err == nil {
		t.Fatal("foreign repository was allowed to index and activate another tenant's snapshot")
	}
	var files int
	var status string
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM code_file_versions WHERE snapshot_id=$1", snapshot).Scan(&files); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, "SELECT status::text FROM code_repository_snapshots WHERE id=$1", snapshot).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if files != 0 || status != "queued" {
		t.Fatalf("rejected foreign indexing left files=%d status=%s", files, status)
	}
}

// Repeating an index operation against an active immutable generation must
// not rewrite file counts or silently re-activate the old generation.
func TestPGIndexSnapshotRejectsActiveGeneration(t *testing.T) {
	store, db := testPGStore(t)
	ctx := t.Context()
	tenant := testTenant(t, db)
	repo, err := store.EnsureRepository(ctx, tenant, "/p1-active/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.CreateSnapshot(ctx, repo, tenant, "main", "first", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, snapshot, repo, []PGFileInput{{
		Path: "kept.txt", ContentHash: hashContent("kept"),
	}}); err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, snapshot, repo, nil); err == nil {
		t.Fatal("reindexing an active snapshot with no files falsely succeeded")
	}
	var status string
	var fileCount int
	if err := db.QueryRowContext(ctx, "SELECT status::text, file_count FROM code_repository_snapshots WHERE id=$1", snapshot).Scan(&status, &fileCount); err != nil {
		t.Fatal(err)
	}
	if status != "active" || fileCount != 1 {
		t.Fatalf("active immutable snapshot mutated: status=%s file_count=%d", status, fileCount)
	}
}

// Failed generations are not valid activation candidates even when the
// caller retries indexing without first deleting the failed generation.
func TestPGIndexSnapshotRejectsFailedGeneration(t *testing.T) {
	store, db := testPGStore(t)
	ctx := t.Context()
	tenant := testTenant(t, db)
	repo, err := store.EnsureRepository(ctx, tenant, "/p1-failed/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.CreateSnapshot(ctx, repo, tenant, "main", "first", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetSnapshotStatus(ctx, snapshot, "failed", "interrupted"); err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, snapshot, repo, nil); err == nil {
		t.Fatal("failed generation was indexed and activated")
	}
	var status string
	if err := db.QueryRowContext(ctx, "SELECT status::text FROM code_repository_snapshots WHERE id=$1", snapshot).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "failed" {
		t.Fatalf("failed snapshot became %s", status)
	}
}

// The snapshot's tenant must agree with the repository's authoritative
// tenant, rather than trusting a second caller-supplied tenant ID.
func TestPGCreateSnapshotRejectsMismatchedTenant(t *testing.T) {
	store, db := testPGStore(t)
	ctx := t.Context()
	owner := testTenant(t, db)
	foreign := testTenant(t, db)
	repo, err := store.EnsureRepository(ctx, owner, "/p1-tenant/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateSnapshot(ctx, repo, foreign, "main", "first", "heuristic-v1", ""); err == nil {
		t.Fatal("foreign tenant was allowed to create a snapshot in an owned repository")
	}
	var count int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM code_repository_snapshots WHERE repository_id=$1", repo).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("foreign tenant created %d snapshots", count)
	}
}

// Both callers may race to index one queued generation. Exactly one can
// commit; the second must observe the committed active status and fail.
func TestPGIndexSnapshotSerializesConcurrentAttempts(t *testing.T) {
	store, db := testPGStore(t)
	ctx := t.Context()
	tenant := testTenant(t, db)
	repo, err := store.EnsureRepository(ctx, tenant, "/p1-concurrent/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		snapshot, err := store.CreateSnapshot(ctx, repo, tenant, "main", "attempt-"+strconv.Itoa(i), "heuristic-v1", "")
		if err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		results := make(chan error, 2)
		for writer := 0; writer < 2; writer++ {
			go func() {
				<-start
				results <- store.IndexSnapshot(ctx, snapshot, repo, nil)
			}()
		}
		close(start)
		successes, rejections := 0, 0
		for writer := 0; writer < 2; writer++ {
			if err := <-results; err == nil {
				successes++
			} else {
				rejections++
			}
		}
		if successes != 1 || rejections != 1 {
			t.Fatalf("attempt %d: expected one successful index and one rejection, got %d/%d", i, successes, rejections)
		}
		var status string
		if err := db.QueryRowContext(ctx, "SELECT status::text FROM code_repository_snapshots WHERE id=$1", snapshot).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != "active" {
			t.Fatalf("attempt %d status=%s", i, status)
		}
	}
}
