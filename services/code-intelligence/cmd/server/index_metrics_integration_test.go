package main

import (
	"context"
	"encoding/json"
	"testing"
)

func TestPGIndexSnapshotPersistsAtomicWorkTelemetry(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenant := testTenant(t, db)
	repo, err := store.EnsureRepository(ctx, tenant, "/metrics/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	const a = "package telemetry\nfunc caller(){target()}\n"
	const b1 = "package telemetry\nfunc target(){}\n"
	const b2 = "package telemetry\nfunc target(){println(\"change\")}\n"
	first, err := store.CreateSnapshot(ctx, repo, tenant, "main", "one", "go/parser-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, first, repo, []PGFileInput{
		{Path: "a.go", Language: "go", Content: a, ContentHash: hashContent(a)},
		{Path: "b.go", Language: "go", Content: b1, ContentHash: hashContent(b1)},
	}); err != nil {
		t.Fatal(err)
	}
	next, err := store.CreateSnapshot(ctx, repo, tenant, "main", "two", "go/parser-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, next, repo, []PGFileInput{
		{Path: "a.go", Language: "go", Content: a, ContentHash: hashContent(a)},
		{Path: "b.go", Language: "go", Content: b2, ContentHash: hashContent(b2)},
	}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id                                                            string
		total, parsed, reused, unparsed, parsedSymbols, reusedSymbols int
		state                                                         string
	}{
		{first, 2, 2, 0, 0, 2, 0, "superseded"},
		{next, 2, 1, 1, 0, 1, 1, "active"},
	} {
		var total, parsed, reused, unparsed, parsedSymbols, reusedSymbols int
		var cacheRate float64
		var elapsed int64
		var raw []byte
		var state string
		err := db.QueryRowContext(ctx, "SELECT file_count,parsed_file_count,reused_file_count,unparsed_file_count,parsed_declaration_count,reused_declaration_count,cache_hit_ratio,index_elapsed_ms,extractor_tier_counts,status::text FROM code_repository_snapshots WHERE id=$1",
			tc.id).Scan(&total, &parsed, &reused, &unparsed, &parsedSymbols, &reusedSymbols,
			&cacheRate, &elapsed, &raw, &state)
		if err != nil {
			t.Fatal(err)
		}
		if total != tc.total || parsed != tc.parsed || reused != tc.reused || unparsed != tc.unparsed ||
			parsedSymbols != tc.parsedSymbols || reusedSymbols != tc.reusedSymbols {
			t.Errorf("%s incorrect counts: files=%d parsed=%d reused=%d unparsed=%d new_decls=%d reused_decls=%d",
				tc.id, total, parsed, reused, unparsed, parsedSymbols, reusedSymbols)
		}
		if cacheRate != float64(tc.reused)/float64(tc.total) || elapsed < 0 {
			t.Errorf("incorrect cache ratio or negative duration: %.3f %d", cacheRate, elapsed)
		}
		var tiers map[string]int
		if err := json.Unmarshal(raw, &tiers); err != nil {
			t.Fatal(err)
		}
		if tiers["go/parser"] != parsedSymbols+reusedSymbols || state != tc.state {
			t.Errorf("extractor tier and activation provenance incorrect: %+v, %s", tiers, state)
		}
	}
	failed, err := store.CreateSnapshot(ctx, repo, tenant, "main", "fails", "go/parser-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	err = store.IndexSnapshot(ctx, failed, repo, []PGFileInput{
		{Path: "duplicate.go", Language: "go", Content: a, ContentHash: hashContent(a)},
		{Path: "duplicate.go", Language: "go", Content: b1, ContentHash: hashContent(b1)},
	})
	if err == nil {
		t.Fatal("invalid duplicate file must roll back indexing")
	}
	var metrics int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM code_repository_snapshots WHERE id=$1 AND index_elapsed_ms IS NOT NULL", failed).Scan(&metrics); err != nil {
		t.Fatal(err)
	}
	if metrics != 0 {
		t.Fatalf("rolled-back snapshot gained committed telemetry: %d", metrics)
	}
}
