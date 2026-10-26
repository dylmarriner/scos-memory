package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// A one-hop impact query can be wider than a depth-eight query. Refuse an
// incomplete blast radius instead of returning the first arbitrary callers.
func TestPGImpactRejectsUnboundedBreadth(t *testing.T) {
	store, db := testPGStore(t)
	ctx := t.Context()
	tenant := testTenant(t, db)
	repo, err := store.EnsureRepository(ctx, tenant, "/impact-node-budget/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.CreateSnapshot(ctx, repo, tenant, "main", "wide-commit", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}

	var source strings.Builder
	source.WriteString("package fanout\nfunc core() int { return 1 }\n")
	for i := 0; i < 520; i++ {
		fmt.Fprintf(&source, "func caller%d() int { return core() }\n", i)
	}
	// The exact 512-node boundary (one seed plus 511 callers) must work.
	source.WriteString("func narrow() int { return 2 }\n")
	for i := 0; i < 511; i++ {
		fmt.Fprintf(&source, "func narrowCaller%d() int { return narrow() }\n", i)
	}
	// Each individual BFS layer is below budget, but the combined two-hop
	// graph exceeds it. Limiting only per-query rows is insufficient.
	source.WriteString("func twoHop() int { return 3 }\n")
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&source, "func layer1_%d() int { return twoHop() }\n", i)
		fmt.Fprintf(&source, "func layer2_%d() int { return layer1_%d() }\n", i, i)
	}
	content := source.String()
	if err := store.IndexSnapshot(ctx, snapshot, repo, []PGFileInput{{
		Path: "fanout.go", Language: "go", ContentHash: hashContent(content), Content: content,
	}}); err != nil {
		t.Fatal(err)
	}

	for name, query := range map[string]func() (*PGImpact, error){
		"current": func() (*PGImpact, error) {
			return store.ChangeImpact(ctx, repo, "main", "fanout.core", 1)
		},
		"historical": func() (*PGImpact, error) {
			return store.ChangeImpactHistorical(ctx, tenant, repo, "main", "wide-commit", "", "fanout.core", 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			impact, err := query()
			if err == nil || !strings.Contains(err.Error(), "node budget") {
				t.Fatalf("expected explicit node budget failure, got err=%v", err)
			}
			if impact != nil {
				t.Fatal("over-budget query returned a partial impact result")
			}
		})
	}

	for name, query := range map[string]func() (*PGImpact, error){
		"current_two_hop": func() (*PGImpact, error) {
			return store.ChangeImpact(ctx, repo, "main", "fanout.twoHop", 2)
		},
		"historical_two_hop": func() (*PGImpact, error) {
			return store.ChangeImpactHistorical(ctx, tenant, repo, "main", "wide-commit", "", "fanout.twoHop", 2)
		},
		"current_seed_budget": func() (*PGImpact, error) {
			return store.ChangeImpact(ctx, repo, "main", "fanout.go", 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := query()
			if err == nil || !strings.Contains(err.Error(), "node budget") {
				t.Fatalf("expected node budget failure, got %v", err)
			}
		})
	}
	for name, query := range map[string]func() (*PGImpact, error){
		"current_at_boundary": func() (*PGImpact, error) {
			return store.ChangeImpact(ctx, repo, "main", "fanout.narrow", 1)
		},
		"historical_at_boundary": func() (*PGImpact, error) {
			return store.ChangeImpactHistorical(ctx, tenant, repo, "main", "wide-commit", "", "fanout.narrow", 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			impact, err := query()
			if err != nil {
				t.Fatal(err)
			}
			if len(impact.MatchedSymbols) != 1 || len(impact.DirectDependents) != 511 {
				t.Fatalf("boundary must return all 512 nodes: matched=%d direct=%d", len(impact.MatchedSymbols), len(impact.DirectDependents))
			}
		})
	}

	srv := &server{pgStore: store}
	for name, request := range map[string]map[string]any{
		"current":    {"repository_id": repo, "branch": "main", "query": "fanout.core", "max_depth": 1},
		"historical": {"repository_id": repo, "branch": "main", "query": "fanout.core", "max_depth": 1, "as_of_commit": "wide-commit"},
		"two_hop":    {"repository_id": repo, "branch": "main", "query": "fanout.twoHop", "max_depth": 2},
		"wide_seed":  {"repository_id": repo, "branch": "main", "query": "fanout.go", "max_depth": 1},
	} {
		t.Run("http_"+name, func(t *testing.T) {
			response := pgRequestAs(t, srv, http.MethodPost, "/v2/code/impact", tenant, "test-principal", request)
			if response.Code != http.StatusUnprocessableEntity {
				t.Fatalf("wide impact must fail closed with 422, got %d: %s", response.Code, response.Body.String())
			}
		})
	}
}
