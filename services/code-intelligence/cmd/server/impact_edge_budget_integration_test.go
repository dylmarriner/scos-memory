package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// A dense dependency graph can stay below both the depth and unique-node
// limits while exhausting the ranking query's edge and CPU budgets.
func TestPGImpactEdgeBudget(t *testing.T) {
	store, db := testPGStore(t)
	ctx := t.Context()
	tenant := testTenant(t, db)
	repo, err := store.EnsureRepository(ctx, tenant, "/impact-edge-budget/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.CreateSnapshot(ctx, repo, tenant, "main", "dense-commit", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}

	var source strings.Builder
	source.WriteString("package dense\nfunc boundaryCore() int { return 1 }\nfunc overflowCore() int { return 2 }\n")
	for _, group := range []struct {
		prefix string
		core   string
		count  int
	}{
		{"boundaryCaller", "boundaryCore", 64},
		{"overflowCaller", "overflowCore", 65},
	} {
		for i := 0; i < group.count; i++ {
			fmt.Fprintf(&source, "func %s%d() int { return %s() }\n", group.prefix, i, group.core)
		}
	}
	body := source.String()
	if err := store.IndexSnapshot(ctx, snapshot, repo, []PGFileInput{{
		Path: "dense.go", Language: "go", ContentHash: hashContent(body), Content: body,
	}}); err != nil {
		t.Fatal(err)
	}

	// The fixture deliberately adds complete CALLS subgraphs within each
	// caller group. Every edge belongs to this test's immutable snapshot.
	for _, prefix := range []string{"boundaryCaller", "overflowCaller"} {
		_, err := db.ExecContext(ctx, `
			INSERT INTO code_edges (snapshot_id, source_symbol_version_id, target_symbol_version_id, relation)
			SELECT $1, source.id, target.id, 'CALLS'
			FROM code_symbol_versions source
			CROSS JOIN code_symbol_versions target
			WHERE source.snapshot_id = $1 AND target.snapshot_id = $1
			  AND source.fully_qualified_name LIKE $2
			  AND target.fully_qualified_name LIKE $2
			  AND source.id <> target.id
			ON CONFLICT DO NOTHING`, snapshot, "dense."+prefix+"%")
		if err != nil {
			t.Fatalf("insert %s clique: %v", prefix, err)
		}
	}

	// Verify the fixture really reaches the intended edge boundary. Without
	// this assertion, an extractor or schema change could make the test pass
	// without exercising the cap.
	for _, fixture := range []struct {
		prefix string
		core   string
		want   int
	}{
		{"boundaryCaller", "dense.boundaryCore", 4096},
		{"overflowCaller", "dense.overflowCore", 4225},
	} {
		var count int
		err := db.QueryRowContext(ctx, `
			SELECT count(*) FROM code_edges e
			JOIN code_symbol_versions source ON source.id = e.source_symbol_version_id
			JOIN code_symbol_versions target ON target.id = e.target_symbol_version_id
			WHERE e.snapshot_id = $1 AND e.relation = 'CALLS'
			  AND source.fully_qualified_name LIKE $2
			  AND (target.fully_qualified_name LIKE $2 OR target.fully_qualified_name = $3)`,
			snapshot, "dense."+fixture.prefix+"%", fixture.core).Scan(&count)
		if err != nil || count != fixture.want {
			t.Fatalf("fixture %s has %d edges, want %d: %v", fixture.prefix, count, fixture.want, err)
		}
	}

	for name, query := range map[string]func() (*PGImpact, error){
		"current_boundary": func() (*PGImpact, error) {
			return store.ChangeImpact(ctx, repo, "main", "dense.boundaryCore", 1)
		},
		"historical_boundary": func() (*PGImpact, error) {
			return store.ChangeImpactHistorical(ctx, tenant, repo, "main", "dense-commit", "", "dense.boundaryCore", 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			impact, err := query()
			if err != nil {
				t.Fatalf("exact 4096-edge boundary must succeed: %v", err)
			}
			if len(impact.MatchedSymbols) != 1 || len(impact.DirectDependents) != 64 {
				t.Fatalf("incomplete boundary impact: matched=%d direct=%d", len(impact.MatchedSymbols), len(impact.DirectDependents))
			}
		})
	}

	for name, query := range map[string]func() (*PGImpact, error){
		"current_overflow": func() (*PGImpact, error) {
			return store.ChangeImpact(ctx, repo, "main", "dense.overflowCore", 1)
		},
		"historical_overflow": func() (*PGImpact, error) {
			return store.ChangeImpactHistorical(ctx, tenant, repo, "main", "dense-commit", "", "dense.overflowCore", 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			impact, err := query()
			if !errors.Is(err, errImpactEdgeBudgetExceeded) {
				t.Fatalf("expected edge-budget rejection, got err=%v (impact nonnil=%t)", err, impact != nil)
			}
			if impact != nil {
				t.Fatal("over-budget graph returned a partial impact")
			}
		})
	}

	srv := &server{pgStore: store}
	for _, request := range []map[string]any{
		{"repository_id": repo, "branch": "main", "query": "dense.overflowCore", "max_depth": 1},
		{"repository_id": repo, "branch": "main", "query": "dense.overflowCore", "max_depth": 1, "as_of_commit": "dense-commit"},
	} {
		response := pgRequestAs(t, srv, http.MethodPost, "/v2/code/impact", tenant, "test-principal", request)
		if response.Code != http.StatusUnprocessableEntity {
			t.Fatalf("over-budget impact must fail with 422, got %d: %s", response.Code, response.Body.String())
		}
	}
}
