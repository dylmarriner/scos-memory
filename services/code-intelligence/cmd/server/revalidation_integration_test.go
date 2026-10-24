package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"

	"github.com/dylmarriner/scos-memory/packages/auth"
	"testing"
)

func TestPGMemoryCodeRevalidation(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenant := testTenant(t, db)
	var principal, namespace string
	if err := db.QueryRowContext(ctx, "INSERT INTO principals(tenant_id,kind,external_id,display_name) VALUES($1,'human',$2,$2) RETURNING id", tenant, t.Name()).Scan(&principal); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, "INSERT INTO namespaces(tenant_id,owner_principal_id,canonical_name) VALUES($1,$2,$3) RETURNING id", tenant, principal, t.Name()).Scan(&namespace); err != nil {
		t.Fatal(err)
	}
	repo, err := store.EnsureRepository(ctx, tenant, "/p7/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	old, err := store.CreateSnapshot(ctx, repo, tenant, "main", "old", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	oldFiles := []PGFileInput{
		{Path: "changed.txt", Language: "text", ContentHash: hashContent("before")},
		{Path: "deleted.txt", Language: "text", ContentHash: hashContent("deleted")},
		{Path: "renamed.txt", Language: "text", ContentHash: hashContent("rename-only")},
		{Path: "unchanged.txt", Language: "text", ContentHash: hashContent("stable")},
	}
	if err := store.IndexSnapshot(ctx, old, repo, oldFiles); err != nil {
		t.Fatal(err)
	}
	facts := map[string]string{}
	for _, file := range oldFiles {
		var version, fact string
		if err := db.QueryRowContext(ctx, "SELECT id FROM code_symbol_versions WHERE snapshot_id=$1 AND path=$2 AND kind='file'", old, file.Path).Scan(&version); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRowContext(ctx, "INSERT INTO memory_items(tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,authority_level,confidence_score,sensitivity) VALUES($1,$2,$3,'semantic','active',$4,$4,50,0.9,'internal') RETURNING id", tenant, namespace, principal, file.Path).Scan(&fact); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, "INSERT INTO memory_code_links(tenant_id,fact_id,code_symbol_version_id,link_type,source) VALUES($1,$2,$3,'evidences','explicit')", tenant, fact, version); err != nil {
			t.Fatal(err)
		}
		facts[file.Path] = fact
	}
	next, err := store.CreateSnapshot(ctx, repo, tenant, "main", "next", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, next, repo, []PGFileInput{
		{Path: "changed.txt", Language: "text", ContentHash: hashContent("after")},
		{Path: "renamed-next.txt", Language: "text", ContentHash: hashContent("rename-only")},
		{Path: "unchanged.txt", Language: "text", ContentHash: hashContent("stable")},
	}); err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	rows, err := db.QueryContext(ctx, "SELECT fact_id,continuity,review_status FROM memory_revalidation_events WHERE new_snapshot_id=$1", next)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, continuity, status string
		if err := rows.Scan(&id, &continuity, &status); err != nil {
			t.Fatal(err)
		}
		seen[id] = continuity + ":" + status
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{facts["changed.txt"]: "changed:needs_review", facts["deleted.txt"]: "deleted:needs_review", facts["renamed.txt"]: "renamed:still_valid"}
	if len(seen) != len(want) {
		t.Fatalf("events=%v want=%v", seen, want)
	}
	for id, expect := range want {
		if seen[id] != expect {
			t.Errorf("fact %s: %q want %q", id, seen[id], expect)
		}
	}
	if _, ok := seen[facts["unchanged.txt"]]; ok {
		t.Fatal("unchanged fact got revalidation")
	}
	if err := store.ReactivateSnapshot(ctx, next); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM memory_revalidation_events WHERE new_snapshot_id=$1", next).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("duplicate events: %d", count)
	}
	for _, name := range []string{"changed.txt", "deleted.txt", "renamed.txt"} {
		var status string
		if err := db.QueryRowContext(ctx, "SELECT status::text FROM memory_items WHERE id=$1", facts[name]).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != "active" {
			t.Fatalf("%s mutated memory status to %q", name, status)
		}
	}

	if _, err := db.ExecContext(ctx, "INSERT INTO namespace_memberships(namespace_id,principal_id,can_read) VALUES ($1,$2,true)", namespace, principal); err != nil {
		t.Fatal(err)
	}
	srv := &server{pgStore: store}
	req := httptest.NewRequest(http.MethodGet, "/v2/code/revalidations?repository_id="+repo, nil)
	req = req.WithContext(auth.ContextWithClaims(req.Context(), auth.Claims{TenantID: tenant, PrincipalID: principal}))
	resp := httptest.NewRecorder()
	srv.handlePGRevalidations(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("list http %d: %s", resp.Code, resp.Body.String())
	}
	var listed []map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 3 {
		t.Fatalf("returned %d events: %s", len(listed), resp.Body.String())
	}
	for _, event := range listed {
		if event["old_commit"] != "old" || event["new_commit"] != "next" {
			t.Errorf("missing commit provenance: %+v", event)
		}
	}
	var outsider string
	if err := db.QueryRowContext(ctx, "INSERT INTO principals(tenant_id,kind,external_id,display_name) VALUES($1,'human',$2,$2) RETURNING id", tenant, "outsider-"+t.Name()).Scan(&outsider); err != nil {
		t.Fatal(err)
	}
	denied := httptest.NewRequest(http.MethodGet, "/v2/code/revalidations?repository_id="+repo, nil)
	denied = denied.WithContext(auth.ContextWithClaims(denied.Context(), auth.Claims{TenantID: tenant, PrincipalID: outsider}))
	hidden := httptest.NewRecorder()
	srv.handlePGRevalidations(hidden, denied)
	if hidden.Code != http.StatusOK || hidden.Body.String() != "[]\n" {
		t.Fatalf("outsider saw data: %d %s", hidden.Code, hidden.Body.String())
	}

}

// A memory linked to S0 must be reconsidered after S2 even when S1 left the
// symbol unchanged; otherwise an unrelated intermediate commit hides damage.
func TestPGRevalidationTraversesHistoricalSnapshotLinks(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenant := testTenant(t, db)
	var principal, namespace string
	if err := db.QueryRowContext(ctx, "INSERT INTO principals(tenant_id,kind,external_id,display_name) VALUES ($1,'human',$2,$2) RETURNING id", tenant, t.Name()).Scan(&principal); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, "INSERT INTO namespaces(tenant_id,owner_principal_id,canonical_name) VALUES ($1,$2,$3) RETURNING id", tenant, principal, t.Name()).Scan(&namespace); err != nil {
		t.Fatal(err)
	}
	repo, err := store.EnsureRepository(ctx, tenant, "/p7-history/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.CreateSnapshot(ctx, repo, tenant, "main", "first", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	content := hashContent("first bytes")
	if err := store.IndexSnapshot(ctx, first, repo, []PGFileInput{{Path: "stable.txt", Language: "text", ContentHash: content}}); err != nil {
		t.Fatal(err)
	}
	var version, fact string
	if err := db.QueryRowContext(ctx, "SELECT id FROM code_symbol_versions WHERE snapshot_id=$1 AND path='stable.txt' AND kind='file'", first).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, "INSERT INTO memory_items(tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,authority_level,confidence_score,sensitivity) VALUES($1,$2,$3,'semantic','active','first','first',50,0.9,'internal') RETURNING id", tenant, namespace, principal).Scan(&fact); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO memory_code_links(tenant_id,fact_id,code_symbol_version_id,link_type,source) VALUES($1,$2,$3,'evidences','explicit')", tenant, fact, version); err != nil {
		t.Fatal(err)
	}
	middle, err := store.CreateSnapshot(ctx, repo, tenant, "main", "middle", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, middle, repo, []PGFileInput{{Path: "stable.txt", Language: "text", ContentHash: content}}); err != nil {
		t.Fatal(err)
	}
	latest, err := store.CreateSnapshot(ctx, repo, tenant, "main", "latest", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, latest, repo, []PGFileInput{{Path: "stable.txt", Language: "text", ContentHash: hashContent("second bytes")}}); err != nil {
		t.Fatal(err)
	}
	var continuity, status string
	if err := db.QueryRowContext(ctx, "SELECT continuity,review_status FROM memory_revalidation_events WHERE new_snapshot_id=$1 AND fact_id=$2", latest, fact).Scan(&continuity, &status); err != nil {
		t.Fatalf("missing historical-link revalidation: %v", err)
	}
	if continuity != "changed" || status != "needs_review" {
		t.Fatalf("historical link = %s:%s", continuity, status)
	}
}

func TestPGDuplicateContentDoesNotMasqueradeAsSafeRename(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenant := testTenant(t, db)
	var principal, namespace string
	if err := db.QueryRowContext(ctx, "INSERT INTO principals(tenant_id,kind,external_id,display_name) VALUES($1,'human',$2,$2) RETURNING id", tenant, t.Name()).Scan(&principal); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, "INSERT INTO namespaces(tenant_id,owner_principal_id,canonical_name) VALUES($1,$2,$3) RETURNING id", tenant, principal, t.Name()).Scan(&namespace); err != nil {
		t.Fatal(err)
	}
	repo, err := store.EnsureRepository(ctx, tenant, "/p7-duplicate/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	old, err := store.CreateSnapshot(ctx, repo, tenant, "main", "before", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	identical := hashContent("identical")
	if err := store.IndexSnapshot(ctx, old, repo, []PGFileInput{
		{Path: "a.txt", Language: "text", ContentHash: identical},
		{Path: "b.txt", Language: "text", ContentHash: identical},
	}); err != nil {
		t.Fatal(err)
	}
	var version, fact string
	if err := db.QueryRowContext(ctx, "SELECT id FROM code_symbol_versions WHERE snapshot_id=$1 AND path='a.txt' AND kind='file'", old).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, "INSERT INTO memory_items(tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,authority_level,confidence_score,sensitivity) VALUES($1,$2,$3,'semantic','active','a','a',50,0.9,'internal') RETURNING id", tenant, namespace, principal).Scan(&fact); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO memory_code_links(tenant_id,fact_id,code_symbol_version_id,link_type,source) VALUES($1,$2,$3,'evidences','explicit')", tenant, fact, version); err != nil {
		t.Fatal(err)
	}
	latest, err := store.CreateSnapshot(ctx, repo, tenant, "main", "after", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, latest, repo, []PGFileInput{
		{Path: "a.txt", Language: "text", ContentHash: hashContent("changed")},
		{Path: "b.txt", Language: "text", ContentHash: identical},
	}); err != nil {
		t.Fatal(err)
	}
	var continuity, status string
	if err := db.QueryRowContext(ctx, "SELECT continuity,review_status FROM memory_revalidation_events WHERE new_snapshot_id=$1 AND fact_id=$2", latest, fact).Scan(&continuity, &status); err != nil {
		t.Fatal(err)
	}
	if continuity == "renamed" || status != "needs_review" {
		t.Fatalf("a.txt modification incorrectly cleared as %s:%s", continuity, status)
	}
}
