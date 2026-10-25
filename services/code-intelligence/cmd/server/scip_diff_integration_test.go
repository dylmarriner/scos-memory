package main

import (
	"context"
	"testing"
)

// SCIP indexes persist declarations, not synthetic file symbol versions.
// Snapshot deltas must still cover their document paths, including multiple
// declarations in one document, and must not invent file-byte rename evidence.
func TestPGDiffSnapshotPreservesSCIPDocumentDeltas(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenant := testTenant(t, db)
	repo, err := store.EnsureRepository(ctx, tenant, "/scip-diff/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	index := func(commit string, changedDoc, moved bool) string {
		t.Helper()
		snapshot, err := store.CreateSnapshot(ctx, repo, tenant, "main", commit, "scip-python", "")
		if err != nil {
			t.Fatal(err)
		}
		fixture := sampleSCIPIndex()
		if changedDoc {
			fixture.Documents[0].Symbols[0].Documentation = []string{"Updated documentation"}
		}
		if moved {
			fixture.Documents[0].RelativePath = "pkg/renamed.py"
		}
		if _, err := store.IndexSnapshotFromSCIP(ctx, snapshot, repo, fixture); err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	first := index("scip-first", false, false)
	initial, err := store.DiffSnapshot(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	if len(initial.Added) != 1 || initial.Added[0].Path != "pkg/service.py" {
		t.Fatalf("SCIP first snapshot should add one document path, got %+v", initial)
	}
	unchanged := index("scip-unchanged", false, false)
	same, err := store.DiffSnapshot(ctx, unchanged)
	if err != nil {
		t.Fatal(err)
	}
	if len(same.Added)+len(same.Modified)+len(same.Removed)+len(same.Renamed)+len(same.Copied) != 0 {
		t.Fatalf("unchanged SCIP declarations must not produce a delta: %+v", same)
	}
	changed := index("scip-updated-doc", true, false)
	modified, err := store.DiffSnapshot(ctx, changed)
	if err != nil {
		t.Fatal(err)
	}
	if len(modified.Modified) != 1 || modified.Modified[0].Path != "pkg/service.py" ||
		len(modified.Added)+len(modified.Removed)+len(modified.Renamed)+len(modified.Copied) != 0 {
		t.Fatalf("changed SCIP declaration metadata must modify its document path: %+v", modified)
	}
	moved := index("scip-moved", true, true)
	moveDiff, err := store.DiffSnapshot(ctx, moved)
	if err != nil {
		t.Fatal(err)
	}
	if len(moveDiff.Added) != 1 || moveDiff.Added[0].Path != "pkg/renamed.py" ||
		len(moveDiff.Removed) != 1 || moveDiff.Removed[0].Path != "pkg/service.py" ||
		len(moveDiff.Modified)+len(moveDiff.Renamed)+len(moveDiff.Copied) != 0 {
		t.Fatalf("SCIP path move lacks source-byte provenance; use added+removed: %+v", moveDiff)
	}
}
