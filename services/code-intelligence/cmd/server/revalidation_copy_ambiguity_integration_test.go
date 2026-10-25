package main

import (
	"context"
	"testing"
)

// Copying a linked file while removing its original path is not a safe rename:
// two possible successors carry the same bytes, so continuity is ambiguous.
// This must create a review event rather than silently marking the fact valid.
func TestPGRevalidationCopiedSuccessorIsAmbiguous(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenant := testTenant(t, db)

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
	repo, err := store.EnsureRepository(ctx, tenant, "/p7-copy-ambiguity/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.CreateSnapshot(ctx, repo, tenant, "main", "before", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	contentHash := hashContent("same exact bytes")
	if err := store.IndexSnapshot(ctx, before, repo, []PGFileInput{
		{Path: "original.txt", Language: "text", ContentHash: contentHash},
	}); err != nil {
		t.Fatal(err)
	}
	var oldVersion, fact string
	if err := db.QueryRowContext(ctx,
		"SELECT id FROM code_symbol_versions WHERE snapshot_id=$1 AND path='original.txt' AND kind='file'",
		before).Scan(&oldVersion); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx,
		"INSERT INTO memory_items(tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,authority_level,confidence_score,sensitivity) VALUES($1,$2,$3,'semantic','active','original','original',50,0.9,'internal') RETURNING id",
		tenant, namespace, principal).Scan(&fact); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO memory_code_links(tenant_id,fact_id,code_symbol_version_id,link_type,source) VALUES($1,$2,$3,'evidences','explicit')",
		tenant, fact, oldVersion); err != nil {
		t.Fatal(err)
	}

	after, err := store.CreateSnapshot(ctx, repo, tenant, "main", "after", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, after, repo, []PGFileInput{
		{Path: "copied-a.txt", Language: "text", ContentHash: contentHash},
		{Path: "copied-b.txt", Language: "text", ContentHash: contentHash},
	}); err != nil {
		t.Fatal(err)
	}
	var possibleSuccessors int
	if err := db.QueryRowContext(ctx,
		"SELECT count(*) FROM code_symbol_versions WHERE snapshot_id=$1 AND kind='file' AND content_hash=$2",
		after, contentHash).Scan(&possibleSuccessors); err != nil {
		t.Fatal(err)
	}
	if possibleSuccessors != 2 {
		t.Fatalf("fixture has %d successor candidates; need exactly two", possibleSuccessors)
	}

	var continuity, reviewStatus string
	if err := db.QueryRowContext(ctx,
		"SELECT continuity, review_status FROM memory_revalidation_events WHERE new_snapshot_id=$1 AND fact_id=$2",
		after, fact).Scan(&continuity, &reviewStatus); err != nil {
		t.Fatalf("missing review event for ambiguous copy: %v", err)
	}
	if continuity != "ambiguous" || reviewStatus != "needs_review" {
		t.Fatalf("ambiguous copied successor was treated as %s:%s; want ambiguous:needs_review", continuity, reviewStatus)
	}
}
