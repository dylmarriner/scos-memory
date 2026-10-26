package main

import (
	"context"
	"testing"
)

// Crosses the SQL bind-argument batching boundary against real PostgreSQL,
// and checks that no edge is dropped or duplicated at the split.
func TestPGBatchedCallEdgesAcrossChunkBoundary(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenant := testTenant(t, db)
	repo, err := store.EnsureRepository(ctx, tenant, "/batch-edges/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.CreateSnapshot(ctx, repo, tenant, "main", "batch-edges", "go/parser-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	files := scalingGoFiles(callEdgeBatchSize + 2)
	if err := store.IndexSnapshot(ctx, snapshot, repo, files); err != nil {
		t.Fatal(err)
	}
	var total, distinct, provenance int
	err = db.QueryRowContext(ctx, `SELECT count(*),
        count(DISTINCT (source_symbol_version_id, target_symbol_version_id)),
        count(*) FILTER (WHERE extractor='go/parser' AND confidence_class='static_analysis')
        FROM code_edges WHERE snapshot_id=$1 AND relation='CALLS'`, snapshot).
		Scan(&total, &distinct, &provenance)
	if err != nil {
		t.Fatal(err)
	}
	want := callEdgeBatchSize + 1
	if total != want || distinct != want || provenance != want {
		t.Fatalf("batched edges total=%d distinct=%d proven=%d, want %d",
			total, distinct, provenance, want)
	}
}
