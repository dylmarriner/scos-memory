package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"testing"
)

const p8ImpactFixture = "package p8\nfunc core() int { return 1 }\nfunc caller() int { return core() }\n"

// Graph expansion is read-only: code blast radius and knowledge impact are
// distinct; every memory result must prove a permitted namespace membership
// and carry its exact immutable linked symbol version.
func TestPGKnowledgeImpactHonoursScopeAndCodeProvenance(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenant := testTenant(t, db)
	foreign := testTenant(t, db)
	mkPrincipal := func(tid, name string) string {
		var id string
		if err := db.QueryRowContext(ctx,
			"INSERT INTO principals(tenant_id,kind,external_id,display_name) VALUES($1,'human',$2,$2) RETURNING id",
			tid, name).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	owner := mkPrincipal(tenant, "owner-"+t.Name())
	outsider := mkPrincipal(tenant, "outsider-"+t.Name())
	foreignOwner := mkPrincipal(foreign, "foreign-"+t.Name())
	mkNamespace := func(tid, principal, name string) string {
		var id string
		if err := db.QueryRowContext(ctx,
			"INSERT INTO namespaces(tenant_id,owner_principal_id,canonical_name) VALUES($1,$2,$3) RETURNING id",
			tid, principal, name).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx,
			"INSERT INTO namespace_memberships(namespace_id,principal_id,can_read) VALUES($1,$2,true)", id, principal); err != nil {
			t.Fatal(err)
		}
		return id
	}
	namespace := mkNamespace(tenant, owner, "owner-"+t.Name())
	private := mkNamespace(tenant, outsider, "private-"+t.Name())
	foreignNS := mkNamespace(foreign, foreignOwner, "foreign-"+t.Name())
	repo, err := store.EnsureRepository(ctx, tenant, "/p8/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.CreateSnapshot(ctx, repo, tenant, "main", "p8-commit", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, snapshot, repo, []PGFileInput{{
		Path: "pkg/p8.go", Language: "go", ContentHash: hashContent(p8ImpactFixture), Content: p8ImpactFixture,
	}}); err != nil {
		t.Fatal(err)
	}
	var core, caller string
	for name, dest := range map[string]*string{"p8.core": &core, "p8.caller": &caller} {
		if err := db.QueryRowContext(ctx, "SELECT id::text FROM code_symbol_versions WHERE snapshot_id=$1 AND fully_qualified_name=$2", snapshot, name).Scan(dest); err != nil {
			t.Fatal(err)
		}
	}
	fact := func(tid, ns, principal, title, version string) string {
		var id string
		if err := db.QueryRowContext(ctx, `INSERT INTO memory_items(tenant_id,namespace_id,owner_principal_id,
    type,status,title,canonical_summary,authority_level,confidence_score,sensitivity)
    VALUES($1,$2,$3,'semantic','active',$4,$4,50,0.9,'internal') RETURNING id::text`,
			tid, ns, principal, title).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, "INSERT INTO memory_code_links(tenant_id,fact_id,code_symbol_version_id,link_type,source) VALUES($1,$2,$3,'evidences','explicit')", tid, id, version); err != nil {
			t.Fatal(err)
		}
		return id
	}
	visibleFact := fact(tenant, namespace, owner, "visible core", core)
	futureFact := fact(tenant, namespace, owner, "not yet effective", core)
	if _, err := db.ExecContext(ctx, "UPDATE memory_items SET valid_from=now()+interval '2 days' WHERE id=$1", futureFact); err != nil {
		t.Fatal(err)
	}

	privateFact := fact(tenant, private, outsider, "private caller", caller)
	// A legitimate foreign-tenant evidence graph must not be visible when
	// a caller tries to reuse this repository's affected-symbol IDs.
	foreignRepo, err := store.EnsureRepository(ctx, foreign, "/p8-foreign/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	foreignSnapshot, err := store.CreateSnapshot(ctx, foreignRepo, foreign, "main", "foreign-commit", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, foreignSnapshot, foreignRepo, []PGFileInput{{
		Path: "foreign.go", Language: "go", ContentHash: hashContent("package foreign\\nfunc isolated() {}\\n"),
		Content: "package foreign\\nfunc isolated() {}\\n",
	}}); err != nil {
		t.Fatal(err)
	}
	var foreignVersion string
	if err := db.QueryRowContext(ctx, "SELECT id::text FROM code_symbol_versions WHERE snapshot_id=$1 AND kind='file'", foreignSnapshot).Scan(&foreignVersion); err != nil {
		t.Fatal(err)
	}
	_ = fact(foreign, foreignNS, foreignOwner, "foreign evidence", foreignVersion)
	impact, err := store.ChangeImpact(ctx, repo, "main", "p8.core", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(impact.DirectDependents) == 0 {
		t.Fatalf("missing code graph dependents %+v", impact)
	}
	view, err := store.KnowledgeImpact(ctx, tenant, owner, repo, impact)
	if err != nil {
		t.Fatal(err)
	}
	if len(view) != 1 {
		t.Fatalf("owner saw %d facts instead of 1: %+v", len(view), view)
	}
	v := view[0]
	if v.FactID != visibleFact || v.SymbolVersionID != core || v.CommitID != "p8-commit" || v.Scope != "matched" || v.LinkID == "" || v.Source != "explicit" {
		t.Fatalf("lost source provenance or scope: %+v", v)
	}
	if len(v.EvidencePath) != 2 || v.EvidencePath[0] != "code:"+core || v.EvidencePath[1] != "memory:"+visibleFact {
		t.Fatalf("missing explainable direct link %+v", v.EvidencePath)
	}
	otherView, err := store.KnowledgeImpact(ctx, tenant, outsider, repo, impact)
	if err != nil {
		t.Fatal(err)
	}
	if len(otherView) != 1 || otherView[0].FactID != privateFact || otherView[0].Scope != "direct_caller" {
		t.Fatalf("outsider saw unauthorised evidence: %+v", otherView)
	}
	foreignView, err := store.KnowledgeImpact(ctx, foreign, foreignOwner, repo, impact)
	if err != nil {
		t.Fatal(err)
	}
	if len(foreignView) != 0 {
		t.Fatalf("foreign tenant read code graph: %+v", foreignView)
	}

	srv := &server{pgStore: store}
	response := pgRequestAs(t, srv, http.MethodPost, "/v2/code/impact", tenant, owner, map[string]any{
		"repository_id": repo, "branch": "main", "query": "p8.core", "max_depth": 3,
	})
	if response.Code != http.StatusOK {
		t.Fatalf("HTTP impact status=%d body=%s", response.Code, response.Body.String())
	}
	var published PGImpact
	if err := json.Unmarshal(response.Body.Bytes(), &published); err != nil {
		t.Fatal(err)
	}
	if len(published.KnowledgeDependents) != 1 || published.KnowledgeDependents[0].FactID != visibleFact {
		t.Fatalf("HTTP impact escaped namespace or dropped memory evidence: %+v", published.KnowledgeDependents)
	}
	forbidden := pgRequestAs(t, srv, http.MethodPost, "/v2/code/impact", tenant, outsider, map[string]any{
		"repository_id": repo, "branch": "main", "query": "p8.core", "max_depth": 3,
	})
	if forbidden.Code != http.StatusOK {
		t.Fatalf("outsider HTTP response %d: %s", forbidden.Code, forbidden.Body.String())
	}
	if err := json.Unmarshal(forbidden.Body.Bytes(), &published); err != nil {
		t.Fatal(err)
	}
	if len(published.KnowledgeDependents) != 1 || published.KnowledgeDependents[0].FactID != privateFact {
		t.Fatalf("HTTP impact leaks someone else's memory: %+v", published.KnowledgeDependents)
	}

	// When CI supplies a restricted runtime role, repeat the core
	// authorization checks under forced RLS rather than relying on the
	// migration owner's unrestricted SQL connection.
	if appDSN := os.Getenv("SCOS_APP_TEST_DATABASE_URL"); appDSN != "" {
		app, err := sql.Open("pgx", appDSN)
		if err != nil {
			t.Fatal(err)
		}
		defer app.Close()
		restricted := newPGCodeStore(app)
		readable, err := restricted.KnowledgeImpact(ctx, tenant, owner, repo, impact)
		if err != nil {
			t.Fatalf("restricted role lookup: %v", err)
		}
		if len(readable) != 1 || readable[0].FactID != visibleFact {
			t.Fatalf("restricted role failed owner scope: %+v", readable)
		}
		hidden, err := restricted.KnowledgeImpact(ctx, tenant, outsider, repo, impact)
		if err != nil {
			t.Fatalf("restricted role outsider: %v", err)
		}
		if len(hidden) != 1 || hidden[0].FactID != privateFact {
			t.Fatalf("restricted role escaped namespace scope: %+v", hidden)
		}
	}

}
