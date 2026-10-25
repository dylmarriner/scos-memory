package main

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
)

// P8: Every affected memory item must have an explainable, snapshot-scoped
// reverse-CALLS path from a matched symbol to its linked symbol version.
// A two-element code->memory link is not enough for a transitive dependent.
func TestPGImpactExplainsTransitiveMemoryEvidence(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenant := testTenant(t, db)
	repo, err := store.EnsureRepository(ctx, tenant, "/impact-paths/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.CreateSnapshot(ctx, repo, tenant, "main", "path-commit", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	source := "package paths\nfunc core() int { return 1 }\nfunc direct() int { return core() }\nfunc middle() int { return direct() }\nfunc top() int { return middle() }\nfunc unrelated() int { return 9 }\n"
	if err := store.IndexSnapshot(ctx, snapshot, repo, []PGFileInput{{
		Path: "paths.go", Language: "go", ContentHash: hashContent(source), Content: source,
	}}); err != nil {
		t.Fatal(err)
	}

	ids := map[string]string{}
	for _, name := range []string{"paths.core", "paths.direct", "paths.middle", "paths.top", "paths.unrelated"} {
		var id string
		if err := db.QueryRowContext(ctx, "SELECT id::text FROM code_symbol_versions WHERE snapshot_id=$1 AND fully_qualified_name=$2", snapshot, name).Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids[name] = id
	}
	expected := []string{"code:" + ids["paths.core"], "code:" + ids["paths.direct"], "code:" + ids["paths.middle"], "code:" + ids["paths.top"]}

	for label, get := range map[string]func() (*PGImpact, error){
		"current": func() (*PGImpact, error) { return store.ChangeImpact(ctx, repo, "main", "paths.core", 3) },
		"historical": func() (*PGImpact, error) {
			return store.ChangeImpactHistorical(ctx, tenant, repo, "main", "path-commit", "", "paths.core", 3)
		},
	} {
		t.Run(label, func(t *testing.T) {
			impact, err := get()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(impact.CodeEvidencePaths[ids["paths.top"]], expected) {
				t.Fatalf("transitive path is not the actual three-hop reverse-CALLS chain: got %v, want %v", impact.CodeEvidencePaths[ids["paths.top"]], expected)
			}
			if !reflect.DeepEqual(impact.CodeEvidencePaths[ids["paths.core"]], expected[:1]) {
				t.Fatalf("seed path is not self: %v", impact.CodeEvidencePaths[ids["paths.core"]])
			}
			if _, ok := impact.CodeEvidencePaths[ids["paths.unrelated"]]; ok {
				t.Fatal("unreachable symbol received fabricated evidence path")
			}
		})
	}

	var principal, namespace, fact string
	if err := db.QueryRowContext(ctx,
		"INSERT INTO principals(tenant_id,kind,external_id,display_name) VALUES($1,'human',$2,$2) RETURNING id",
		tenant, "impact-paths-"+t.Name()).Scan(&principal); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx,
		"INSERT INTO namespaces(tenant_id,owner_principal_id,canonical_name) VALUES($1,$2,$3) RETURNING id",
		tenant, principal, "impact-paths-"+t.Name()).Scan(&namespace); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO namespace_memberships(namespace_id,principal_id,can_read) VALUES($1,$2,true)",
		namespace, principal); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx,
		`INSERT INTO memory_items(tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,authority_level,confidence_score,sensitivity)
        VALUES($1,$2,$3,'semantic','active','top dependency','top dependency',50,0.9,'internal') RETURNING id::text`,
		tenant, namespace, principal).Scan(&fact); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO memory_code_links(tenant_id,fact_id,code_symbol_version_id,link_type,source) VALUES($1,$2,$3,'evidences','explicit')",
		tenant, fact, ids["paths.top"]); err != nil {
		t.Fatal(err)
	}

	impact, err := store.ChangeImpact(ctx, repo, "main", "paths.core", 3)
	if err != nil {
		t.Fatal(err)
	}
	links, err := store.KnowledgeImpact(ctx, tenant, principal, repo, impact)
	if err != nil {
		t.Fatal(err)
	}
	want := append(append([]string{}, expected...), "memory:"+fact)
	if len(links) != 1 || links[0].Scope != "transitive_caller" || !reflect.DeepEqual(links[0].EvidencePath, want) {
		t.Fatalf("missing actual reverse-CALLS chain to memory link: links=%+v want=%v", links, want)
	}

	for _, historical := range []bool{false, true} {
		request := map[string]any{"repository_id": repo, "branch": "main", "query": "paths.core", "max_depth": 3}
		if historical {
			request["as_of_commit"] = "path-commit"
		}
		response := pgRequestAs(t, &server{pgStore: store}, http.MethodPost, "/v2/code/impact", tenant, principal, request)
		if response.Code != http.StatusOK {
			t.Fatalf("historical=%t HTTP status %d: %s", historical, response.Code, response.Body.String())
		}
		var published PGImpact
		if err := json.Unmarshal(response.Body.Bytes(), &published); err != nil {
			t.Fatal(err)
		}
		if published.Historical != historical || !reflect.DeepEqual(published.CodeEvidencePaths[ids["paths.top"]], expected) {
			t.Fatalf("historical=%t lost code path/provenance over HTTP: %+v", historical, published)
		}
		if len(published.KnowledgeDependents) != 1 || !reflect.DeepEqual(published.KnowledgeDependents[0].EvidencePath, want) {
			t.Fatalf("historical=%t lost governed memory path over HTTP: %+v", historical, published.KnowledgeDependents)
		}
	}
}
