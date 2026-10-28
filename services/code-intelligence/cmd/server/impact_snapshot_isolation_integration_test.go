package main

import (
	"context"
	"testing"
)

// A code_edges row is not valid for a snapshot just because its endpoints
// happen to name symbol versions in that snapshot. The edge's own immutable
// snapshot_id must match too. Otherwise a corrupted/foreign-generation edge
// fabricates callers, impact evidence paths and knowledge dependencies.
func TestPGImpactIgnoresEdgesFromOtherSnapshots(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenant := testTenant(t, db)
	repo, err := store.EnsureRepository(ctx, tenant, "/impact-snapshot-isolation/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	seedSnapshot, err := store.CreateSnapshot(ctx, repo, tenant, "main", "seed-commit", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	seedContent := "package isolation\nfunc core() int { return 1 }\nfunc unrelated() int { return 2 }\n"
	if err := store.IndexSnapshot(ctx, seedSnapshot, repo, []PGFileInput{{
		Path: "isolation.go", Language: "go", ContentHash: hashContent(seedContent), Content: seedContent,
	}}); err != nil {
		t.Fatal(err)
	}
	otherSnapshot, err := store.CreateSnapshot(ctx, repo, tenant, "other", "other-commit", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	otherContent := "package other\nfunc unrelated() int { return 3 }\n"
	if err := store.IndexSnapshot(ctx, otherSnapshot, repo, []PGFileInput{{
		Path: "other.go", Language: "go", ContentHash: hashContent(otherContent), Content: otherContent,
	}}); err != nil {
		t.Fatal(err)
	}
	var core, unrelated string
	for name, target := range map[string]*string{"isolation.core": &core, "isolation.unrelated": &unrelated} {
		if err := db.QueryRowContext(ctx, "SELECT id::text FROM code_symbol_versions WHERE snapshot_id=$1 AND fully_qualified_name=$2",
			seedSnapshot, name).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	// Deliberately corrupt the graph: both endpoints are from seedSnapshot,
	// but the edge is attached to a different immutable generation.
	if _, err := db.ExecContext(ctx, `INSERT INTO code_edges
        (snapshot_id,source_symbol_version_id,target_symbol_version_id,relation)
        VALUES($1,$2,$3,'CALLS')`, otherSnapshot, unrelated, core); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func() (*PGImpact, error){
		"current": func() (*PGImpact, error) {
			return store.ChangeImpactScoped(ctx, tenant, repo, "main", "isolation.core", 2)
		},
		"historical": func() (*PGImpact, error) {
			return store.ChangeImpactHistorical(ctx, tenant, repo, "main", "seed-commit", "", "isolation.core", 2)
		},
	}
	adjacency, err := edgesAmongWithQuerier(ctx, db, []string{core, unrelated})
	if err != nil {
		t.Fatal(err)
	}
	if len(adjacency) != 0 {
		t.Fatalf("foreign edge entered ranking graph: %+v", adjacency)
	}
	for label, query := range cases {
		t.Run(label+"_foreign_edge", func(t *testing.T) {
			impact, err := query()
			if err != nil {
				t.Fatal(err)
			}
			if len(impact.MatchedSymbols) != 1 || len(impact.DirectDependents) != 0 ||
				len(impact.TransitiveDependents) != 0 || len(impact.CodeEvidencePaths) != 1 {
				t.Fatalf("foreign-generation edge contaminated %s impact: %+v", label, impact)
			}
		})
	}
	// Legitimate same-generation CALLS still produce an actual explainable hop.
	if _, err := db.ExecContext(ctx, `INSERT INTO code_edges
        (snapshot_id,source_symbol_version_id,target_symbol_version_id,relation)
        VALUES($1,$2,$3,'CALLS')`, seedSnapshot, unrelated, core); err != nil {
		t.Fatal(err)
	}
	adjacency, err = edgesAmongWithQuerier(ctx, db, []string{core, unrelated})
	if err != nil {
		t.Fatal(err)
	}
	if len(adjacency[unrelated]) != 1 || adjacency[unrelated][0] != core {
		t.Fatalf("ranking graph includes invalid edges: %+v", adjacency)
	}
	for label, query := range cases {
		t.Run(label+"_legitimate_edge", func(t *testing.T) {
			impact, err := query()
			if err != nil {
				t.Fatal(err)
			}
			if len(impact.DirectDependents) != 1 || impact.DirectDependents[0].SymbolVersionID != unrelated {
				t.Fatalf("lost legitimate %s caller: %+v", label, impact)
			}
			path := impact.CodeEvidencePaths[unrelated]
			if len(path) != 2 || path[0] != "code:"+core || path[1] != "code:"+unrelated {
				t.Fatalf("incorrect %s evidence path: %v", label, path)
			}
		})
	}
}
