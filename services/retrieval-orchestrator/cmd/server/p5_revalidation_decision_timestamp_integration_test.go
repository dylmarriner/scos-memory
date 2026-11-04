//go:build integration

package main

import (
	"context"
	"testing"
	"time"
)

// Revalidation decisions are append-only audit evidence. The caller may not
// backdate or future-date the database's record timestamp. This timestamp is
// still not a transaction commit timestamp and does not enable as-of retrieval.
func TestP7RevalidationDecisionRecordTimeCannotBeForged(t *testing.T) {
	ctx := context.Background()
	pool := testRetrievalPool(t)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var tenant, principal, namespace, fact, repository string
	if err := tx.QueryRow(ctx, `INSERT INTO tenants(name) VALUES($1) RETURNING id::text`, t.Name()).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `INSERT INTO principals(tenant_id,kind,external_id,display_name)
		VALUES($1,'human',$2,$2) RETURNING id::text`, tenant, t.Name()).Scan(&principal); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `INSERT INTO namespaces(tenant_id,owner_principal_id,canonical_name,sensitivity_ceiling)
		VALUES($1,$2,$3,'internal') RETURNING id::text`, tenant, principal, t.Name()).Scan(&namespace); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO namespace_memberships(
		namespace_id,principal_id,can_read,can_write_candidates,can_promote,can_administer)
		VALUES($1,$2,true,true,true,true)`, namespace, principal); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `INSERT INTO memory_items(
		tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,
		authority_level,confidence_score,sensitivity)
		VALUES($1,$2,$3,'semantic','active','timestamp test','timestamp test',80,0.9,'internal')
		RETURNING id::text`, tenant, namespace, principal).Scan(&fact); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `INSERT INTO code_repositories(tenant_id,canonical_path)
		VALUES($1,$2) RETURNING id::text`, tenant, "/p7-timestamp-"+tenant).Scan(&repository); err != nil {
		t.Fatal(err)
	}
	var oldSnapshot, pastSnapshot, futureSnapshot, symbolVersion, link string
	if err := tx.QueryRow(ctx, `INSERT INTO code_repository_snapshots(
		tenant_id,repository_id,branch,commit_id,status)
		VALUES($1,$2,'main','timestamp-old','active') RETURNING id::text`, tenant, repository).Scan(&oldSnapshot); err != nil {
		t.Fatal(err)
	}
	for _, target := range []struct {
		commit string
		dest   *string
	}{
		{commit: "timestamp-past", dest: &pastSnapshot},
		{commit: "timestamp-future", dest: &futureSnapshot},
	} {
		if err := tx.QueryRow(ctx, `INSERT INTO code_repository_snapshots(
			tenant_id,repository_id,branch,commit_id,status,parent_snapshot_id)
			VALUES($1,$2,'main',$3,'ready',$4) RETURNING id::text`,
			tenant, repository, target.commit, oldSnapshot).Scan(target.dest); err != nil {
			t.Fatal(err)
		}
	}
	var symbol string
	if err := tx.QueryRow(ctx, `INSERT INTO code_symbols(repository_id,stable_key,kind,first_snapshot_id)
		VALUES($1,$2,'file',$3) RETURNING id::text`, repository, "timestamp-"+fact, oldSnapshot).Scan(&symbol); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `INSERT INTO code_symbol_versions(
		symbol_id,snapshot_id,fully_qualified_name,path,kind,content_hash)
		VALUES($1,$2,'timestamp.guard','src/timestamp.go','file','timestamp-hash')
		RETURNING id::text`, symbol, oldSnapshot).Scan(&symbolVersion); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `INSERT INTO memory_code_links(
		tenant_id,fact_id,code_symbol_version_id,link_type,source)
		VALUES($1,$2,$3,'evidences','explicit') RETURNING id::text`,
		tenant, fact, symbolVersion).Scan(&link); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name     string
		snapshot string
		forgedAt time.Time
	}{
		{name: "backdated", snapshot: pastSnapshot, forgedAt: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)},
		{name: "future-dated", snapshot: futureSnapshot, forgedAt: time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var event string
			if err := tx.QueryRow(ctx, `INSERT INTO memory_revalidation_events(
				tenant_id,fact_id,link_id,old_snapshot_id,new_snapshot_id,
				old_symbol_version_id,continuity,review_status)
				VALUES($1,$2,$3,$4,$5,$6,'changed','needs_review') RETURNING id::text`,
				tenant, fact, link, oldSnapshot, tc.snapshot, symbolVersion).Scan(&event); err != nil {
				t.Fatal(err)
			}

			var before, recorded, after time.Time
			if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&before); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, `SELECT set_config('scos.tenant_id',$1,true), set_config('scos.principal_id',$2,true)`, tenant, principal); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, `SET LOCAL ROLE scos_app`); err != nil {
				t.Fatal(err)
			}
			insertErr := tx.QueryRow(ctx, `INSERT INTO memory_revalidation_decisions(
				event_id,tenant_id,reviewer_id,outcome,reason,created_at)
				VALUES($1,$2,$3,'still_valid','Reviewer verified timestamp integrity evidence',$4)
				RETURNING created_at`, event, tenant, principal, tc.forgedAt).Scan(&recorded)
			if _, err := tx.Exec(ctx, `RESET ROLE`); err != nil {
				t.Fatal(err)
			}
			if insertErr != nil {
				t.Fatal(insertErr)
			}
			if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&after); err != nil {
				t.Fatal(err)
			}
			if recorded.Before(before) || recorded.After(after) {
				t.Fatalf("database did not replace caller-supplied created_at: got %s, insert window [%s,%s]",
					recorded.Format(time.RFC3339Nano), before.Format(time.RFC3339Nano), after.Format(time.RFC3339Nano))
			}
			if recorded.Equal(tc.forgedAt) {
				t.Fatalf("caller-controlled timestamp survived insert: %s", recorded.Format(time.RFC3339Nano))
			}
		})
	}
}
