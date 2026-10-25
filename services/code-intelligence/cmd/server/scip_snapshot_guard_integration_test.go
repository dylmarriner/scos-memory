package main

import (
	"testing"

	scip "github.com/scip-code/scip/bindings/go/scip"
)

// SCIP indexing must enforce the same immutable snapshot and repository
// ownership boundaries as the file-based indexing path.
func TestPGSCIPSnapshotOwnershipAndImmutability(t *testing.T) {
	store, db := testPGStore(t)
	ctx := t.Context()
	owner := testTenant(t, db)
	foreign := testTenant(t, db)
	ownerRepo, err := store.EnsureRepository(ctx, owner, "/p1-scip-owner/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	foreignRepo, err := store.EnsureRepository(ctx, foreign, "/p1-scip-foreign/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.CreateSnapshot(ctx, ownerRepo, owner, "main", "scip-first", "scip-v1", "")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := store.IndexSnapshotFromSCIP(ctx, snapshot, foreignRepo, sampleSCIPIndex()); err == nil {
		t.Fatal("SCIP indexing accepted a foreign repository")
	}
	var versions int
	var status string
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM code_symbol_versions WHERE snapshot_id=$1", snapshot).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, "SELECT status::text FROM code_repository_snapshots WHERE id=$1", snapshot).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if versions != 0 || status != "queued" {
		t.Fatalf("rejected foreign SCIP index changed snapshot: versions=%d status=%s", versions, status)
	}

	if _, err := store.IndexSnapshotFromSCIP(ctx, snapshot, ownerRepo, sampleSCIPIndex()); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM code_symbol_versions WHERE snapshot_id=$1", snapshot).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions == 0 {
		t.Fatal("valid SCIP index inserted no symbol versions")
	}
	if _, err := store.IndexSnapshotFromSCIP(ctx, snapshot, ownerRepo, &scip.Index{}); err == nil {
		t.Fatal("SCIP reindex silently mutated an already active snapshot")
	}
	var after int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM code_symbol_versions WHERE snapshot_id=$1", snapshot).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, "SELECT status::text FROM code_repository_snapshots WHERE id=$1", snapshot).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if after != versions || status != "active" {
		t.Fatalf("immutable SCIP snapshot mutated: versions %d -> %d, status=%s", versions, after, status)
	}
}
