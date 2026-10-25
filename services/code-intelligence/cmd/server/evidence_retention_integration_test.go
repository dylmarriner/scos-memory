package main

import (
	"context"
	"testing"
)

// A code version supporting an approved memory cannot be garbage-collected
// by cascading away its link. This is a database invariant, not a courtesy
// owed by a specific GC worker.
func TestPGLinkedHistoricalCodeEvidenceCannotBeDeleted(t *testing.T) {
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
	repo, err := store.EnsureRepository(ctx, tenant, "/linked-retention/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.CreateSnapshot(ctx, repo, tenant, "main", "first", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, first, repo, []PGFileInput{{
		Path: "pkg/retention.go", Language: "go", ContentHash: hashContent("retention source"),
	}}); err != nil {
		t.Fatal(err)
	}
	second, err := store.CreateSnapshot(ctx, repo, tenant, "main", "second", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, second, repo, []PGFileInput{{
		Path: "pkg/replacement.go", Language: "go", ContentHash: hashContent("different version"),
	}}); err != nil {
		t.Fatal(err)
	}
	var version, fact, link string
	if err := db.QueryRowContext(ctx,
		"SELECT id::text FROM code_symbol_versions WHERE snapshot_id=$1 AND path='pkg/retention.go' AND kind='file'",
		first).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx,
		`INSERT INTO memory_items(tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,authority_level,confidence_score,sensitivity)
    VALUES($1,$2,$3,'semantic','active','evidence','evidence',50,0.9,'internal') RETURNING id`,
		tenant, namespace, principal).Scan(&fact); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx,
		"INSERT INTO memory_code_links(tenant_id,fact_id,code_symbol_version_id,link_type,source) VALUES($1,$2,$3,'evidences','explicit') RETURNING id",
		tenant, fact, version).Scan(&link); err != nil {
		t.Fatal(err)
	}

	if _, err := db.ExecContext(ctx, "DELETE FROM code_symbol_versions WHERE id=$1", version); err == nil {
		t.Fatal("deleting a version cascaded away an authoritative memory-code link")
	}
	if _, err := db.ExecContext(ctx, "DELETE FROM code_repository_snapshots WHERE id=$1", first); err == nil {
		t.Fatal("GC deleted an immutable snapshot still cited by memory")
	}
	var retained int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM memory_code_links WHERE id=$1 AND code_symbol_version_id=$2", link, version).Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if retained != 1 {
		t.Fatalf("evidence relationship disappeared: %d", retained)
	}
	var stillExists int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM code_symbol_versions WHERE id=$1", version).Scan(&stillExists); err != nil {
		t.Fatal(err)
	}
	if stillExists != 1 {
		t.Fatalf("linked symbol version disappeared: %d", stillExists)
	}
	if _, err := db.ExecContext(ctx, "DELETE FROM memory_code_links WHERE id=$1", link); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "DELETE FROM code_symbol_versions WHERE id=$1", version); err != nil {
		t.Fatalf("unlinked evidence should be eligible for GC: %v", err)
	}
}
