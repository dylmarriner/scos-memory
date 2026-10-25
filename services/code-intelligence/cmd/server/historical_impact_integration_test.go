package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// A historical impact query must traverse the exact immutable generation
// requested by the caller, not the current branch graph. Knowledge links are
// evaluated using CURRENT memory authorization/validity, not time-travelled.
func TestPGHistoricalImpactSnapshotSelection(t *testing.T) {
	store, db := testPGStore(t)
	ctx := t.Context()
	tenant := testTenant(t, db)
	otherTenant := testTenant(t, db)
	var principal, namespace string
	if err := db.QueryRowContext(ctx,
		"INSERT INTO principals(tenant_id,kind,external_id,display_name) VALUES($1,'human',$2,$2) RETURNING id",
		tenant, t.Name()).Scan(&principal); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx,
		"INSERT INTO namespaces(tenant_id,owner_principal_id,canonical_name) VALUES($1,$2,$3) RETURNING id",
		tenant, principal, t.Name()).Scan(&namespace); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO namespace_memberships(namespace_id,principal_id,can_read) VALUES($1,$2,true)",
		namespace, principal); err != nil {
		t.Fatal(err)
	}

	repo, err := store.EnsureRepository(ctx, tenant, "/historical-impact/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	old, err := store.CreateSnapshot(ctx, repo, tenant, "main", "before", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	oldSource := "package history\nfunc core() int { return 1 }\nfunc caller() int { return core() }\n"
	if err := store.IndexSnapshot(ctx, old, repo, []PGFileInput{{
		Path: "history.go", Language: "go", ContentHash: hashContent(oldSource), Content: oldSource,
	}}); err != nil {
		t.Fatal(err)
	}
	var oldCore, fact string
	if err := db.QueryRowContext(ctx,
		"SELECT id::text FROM code_symbol_versions WHERE snapshot_id=$1 AND fully_qualified_name='history.core'",
		old).Scan(&oldCore); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx,
		"INSERT INTO memory_items(tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,authority_level,confidence_score,sensitivity) VALUES($1,$2,$3,'semantic','active','historical link','historical link',50,0.9,'internal') RETURNING id::text",
		tenant, namespace, principal).Scan(&fact); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO memory_code_links(tenant_id,fact_id,code_symbol_version_id,link_type,source) VALUES($1,$2,$3,'evidences','explicit')",
		tenant, fact, oldCore); err != nil {
		t.Fatal(err)
	}

	current, err := store.CreateSnapshot(ctx, repo, tenant, "main", "after", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	newSource := "package history\nfunc core() int { return 2 }\n"
	if err := store.IndexSnapshot(ctx, current, repo, []PGFileInput{{
		Path: "history.go", Language: "go", ContentHash: hashContent(newSource), Content: newSource,
	}}); err != nil {
		t.Fatal(err)
	}

	// Cross-generation edges must not mix callers from different snapshots.
	var currentCore, oldCaller string
	if err := db.QueryRowContext(ctx,
		"SELECT id::text FROM code_symbol_versions WHERE snapshot_id=$1 AND fully_qualified_name='history.core'",
		current).Scan(&currentCore); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx,
		"SELECT id::text FROM code_symbol_versions WHERE snapshot_id=$1 AND fully_qualified_name='history.caller'",
		old).Scan(&oldCaller); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO code_edges(snapshot_id,source_symbol_version_id,target_symbol_version_id,relation) VALUES($1,$2,$3,'CALLS')",
		old, oldCaller, currentCore); err != nil {
		t.Fatal(err)
	}
	srv := &server{pgStore: store}
	query := func(who string, body map[string]any, want int) *PGImpact {
		t.Helper()
		resp := pgRequestAs(t, srv, http.MethodPost, "/v2/code/impact", who, principal, body)
		if resp.Code != want {
			t.Fatalf("impact %+v: status=%d want=%d body=%s", body, resp.Code, want, resp.Body.String())
		}
		if want != http.StatusOK {
			return nil
		}
		var impact PGImpact
		if err := json.Unmarshal(resp.Body.Bytes(), &impact); err != nil {
			t.Fatal(err)
		}
		return &impact
	}
	base := map[string]any{"repository_id": repo, "branch": "main", "query": "history.core", "max_depth": 3}
	active := query(tenant, base, http.StatusOK)
	if len(active.MatchedSymbols) != 1 || active.MatchedSymbols[0].SnapshotID != current ||
		len(active.DirectDependents) != 0 {
		t.Fatalf("current impact leaked historical caller: %+v", active)
	}
	byCommit := map[string]any{"repository_id": repo, "branch": "main", "query": "history.core", "as_of_commit": "before"}
	// A caller-controlled traversal depth must have a hard upper bound for
	// both current and historical impact; a small graph must not mask the
	// unbounded-work defect by finishing quickly.
	if _, err := store.ChangeImpact(ctx, repo, "main", "history.core", 9); !errors.Is(err, errImpactDepthTooLarge) {
		t.Fatalf("direct current impact must reject excessive depth, got %v", err)
	}
	if _, err := store.ChangeImpactHistorical(ctx, tenant, repo, "main", "before", "", "history.core", 9); !errors.Is(err, errImpactDepthTooLarge) {
		t.Fatalf("direct historical impact must reject excessive depth, got %v", err)
	}
	for _, selector := range []map[string]any{
		{"repository_id": repo, "branch": "main", "query": "history.core", "max_depth": 9},
		{"repository_id": repo, "branch": "main", "query": "history.core", "as_of_commit": "before", "max_depth": 9},
	} {
		query(tenant, selector, http.StatusUnprocessableEntity)
	}
	// The documented maximum remains usable, including for historical data.
	query(tenant, map[string]any{"repository_id": repo, "branch": "main", "query": "history.core", "max_depth": 8}, http.StatusOK)
	query(tenant, map[string]any{"repository_id": repo, "branch": "main", "query": "history.core", "as_of_commit": "before", "max_depth": 8}, http.StatusOK)
	oldImpact := query(tenant, byCommit, http.StatusOK)
	if !oldImpact.Historical || oldImpact.SnapshotID != old || oldImpact.CommitID != "before" ||
		len(oldImpact.MatchedSymbols) != 1 || oldImpact.MatchedSymbols[0].SnapshotID != old ||
		len(oldImpact.DirectDependents) != 1 || oldImpact.DirectDependents[0].SnapshotID != old ||
		oldImpact.DirectDependents[0].FullyQualifiedName != "history.caller" {
		t.Fatalf("historical graph lost immutable provenance: %+v", oldImpact)
	}
	if oldImpact.KnowledgeScope != "current_visible_facts" ||
		len(oldImpact.KnowledgeDependents) != 1 || oldImpact.KnowledgeDependents[0].FactID != fact ||
		oldImpact.KnowledgeDependents[0].SymbolVersionID != oldCore {
		t.Fatalf("historical impact lost governed knowledge provenance: %+v", oldImpact)
	}
	// A principal with no namespace membership may inspect tenant-owned code,
	// but cannot borrow the owner's historical linked memory evidence.
	var outsider string
	if err := db.QueryRowContext(ctx,
		"INSERT INTO principals(tenant_id,kind,external_id,display_name) VALUES($1,'human',$2,$2) RETURNING id",
		tenant, "outsider-"+t.Name()).Scan(&outsider); err != nil {
		t.Fatal(err)
	}
	noAccess := pgRequestAs(t, srv, http.MethodPost, "/v2/code/impact", tenant, outsider, byCommit)
	if noAccess.Code != http.StatusOK {
		t.Fatalf("outsider impact status %d: %s", noAccess.Code, noAccess.Body.String())
	}
	var outsiderImpact PGImpact
	if err := json.Unmarshal(noAccess.Body.Bytes(), &outsiderImpact); err != nil {
		t.Fatal(err)
	}
	if len(outsiderImpact.KnowledgeDependents) != 0 {
		t.Fatalf("historical impact leaked another principal's memory: %+v", outsiderImpact.KnowledgeDependents)
	}
	byID := map[string]any{"repository_id": repo, "branch": "main", "query": "history.core", "snapshot_id": old}
	if selected := query(tenant, byID, http.StatusOK); selected.SnapshotID != old {
		t.Fatalf("wrong snapshot: %+v", selected)
	}
	query(otherTenant, byID, http.StatusNotFound)
	query(tenant, map[string]any{"repository_id": repo, "branch": "other", "query": "history.core", "snapshot_id": old}, http.StatusNotFound)
	query(tenant, map[string]any{"repository_id": repo, "branch": "main", "query": "history.core", "as_of_commit": "missing"}, http.StatusNotFound)
	query(tenant, map[string]any{"repository_id": repo, "branch": "main", "query": "history.core", "snapshot_id": old, "as_of_commit": "after"}, http.StatusNotFound)
	query(tenant, map[string]any{"repository_id": repo, "query": "history.core", "snapshot_id": old}, http.StatusBadRequest)
	query(tenant, map[string]any{"repository_id": repo, "branch": "main", "query": "history.core", "snapshot_id": "invalid"}, http.StatusBadRequest)

	queued, err := store.CreateSnapshot(ctx, repo, tenant, "main", "queued", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	query(tenant, map[string]any{"repository_id": repo, "branch": "main", "query": "history.core", "snapshot_id": queued}, http.StatusNotFound)

	duplicate, err := store.CreateSnapshot(ctx, repo, tenant, "main", "before", "heuristic-v1", "alternate")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, duplicate, repo, []PGFileInput{{
		Path: "history.go", Language: "go", ContentHash: hashContent(oldSource), Content: oldSource,
	}}); err != nil {
		t.Fatal(err)
	}
	query(tenant, byCommit, http.StatusConflict)
	if selected := query(tenant, byID, http.StatusOK); selected.SnapshotID != old {
		t.Fatalf("ambiguous commit should still allow exact snapshot: %+v", selected)
	}
}

// A file path may match hundreds of declarations. Never silently compute
// a partial blast radius from the first 100 historical query results.
func TestPGHistoricalImpactRejectsTruncatedSymbolSeeds(t *testing.T) {
	store, db := testPGStore(t)
	ctx := t.Context()
	tenant := testTenant(t, db)
	repo, err := store.EnsureRepository(ctx, tenant, "/impact-breadth/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.CreateSnapshot(ctx, repo, tenant, "main", "many-functions", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	var source strings.Builder
	source.WriteString("package many\n")
	for i := 0; i < 110; i++ {
		fmt.Fprintf(&source, "func function%d() int { return %d }\n", i, i)
	}
	content := source.String()
	if err := store.IndexSnapshot(ctx, snapshot, repo, []PGFileInput{{
		Path: "many.go", Language: "go", ContentHash: hashContent(content), Content: content,
	}}); err != nil {
		t.Fatal(err)
	}
	_, err = store.ChangeImpactHistorical(ctx, tenant, repo, "main", "many-functions", "", "many.go", 3)
	if err == nil || !strings.Contains(err.Error(), "too many") {
		t.Fatalf("expected explicit incomplete-impact error, got %v", err)
	}
}
