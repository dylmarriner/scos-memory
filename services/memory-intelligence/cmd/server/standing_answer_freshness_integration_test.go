//go:build integration

package main

import (
	"context"
	"fmt"
	"testing"
)

func TestP7StandingAnswerEvidenceInvalidation(t *testing.T) {
	f := newBehaviourFixture(t)
	ctx := context.Background()
	makeFact := func(content string) string {
		return f.scalar(`INSERT INTO memory_items(
    tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,
    authority_level,confidence_score,sensitivity)
    VALUES($1,$2,$3,'semantic','active',$4,$4,80,0.9,'internal')
    RETURNING id::text`, f.tenant, f.ns, f.owner, content)
	}
	source := makeFact("source evidence")
	untouched := makeFact("unrelated evidence")
	cache := func(key string, factID string) string {
		t.Helper()
		raw := fmt.Sprintf("[%q]", factID)
		result, err := withPrincipalContext(ctx, f.srv.db, requestIdentity{TenantID: f.tenant, PrincipalID: f.owner}, func(tx querier) (string, error) {
			var id string
			err := tx.QueryRowContext(ctx, `INSERT INTO memory_standing_answers(
       tenant_id,namespace_id,question_key,answer,evidence_fact_ids)
       VALUES($1,$2,$3,'verified answer',$4) RETURNING id::text`,
				f.tenant, f.ns, key, raw).Scan(&id)
			return id, err
		})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	primary := cache("tracked", source)
	stable := cache("stable", untouched)
	check := func(id string, want bool) {
		t.Helper()
		val, err := withPrincipalContext(ctx, f.srv.db, requestIdentity{TenantID: f.tenant, PrincipalID: f.owner}, func(tx querier) (bool, error) {
			var stale bool
			e := tx.QueryRowContext(ctx, "SELECT stale_at IS NOT NULL FROM memory_standing_answers WHERE id=$1", id).Scan(&stale)
			return stale, e
		})
		if err != nil {
			t.Fatal(err)
		}
		if val != want {
			t.Fatalf("cache %s stale=%v want %v", id, val, want)
		}
	}
	check(primary, false)
	check(stable, false)
	if _, err := f.admin.ExecContext(ctx,
		"UPDATE memory_items SET metadata=jsonb_build_object('audit','changed') WHERE id=$1",
		source); err != nil {
		t.Fatal(err)
	}
	check(primary, false)
	if _, err := f.admin.ExecContext(ctx,
		"UPDATE memory_items SET status='superseded' WHERE id=$1", source); err != nil {
		t.Fatal(err)
	}
	check(primary, true)
	check(stable, false)
	// Trigger-owned dependencies must be absent from the runtime role's
	// direct table privileges, not just hidden by an empty RLS result.
	var count int
	if err := f.admin.QueryRowContext(ctx,
		"SELECT count(*) FROM memory_standing_answer_dependencies WHERE fact_id=$1", source).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("source dependency not stored: %d", count)
	}
	if err := f.srv.db.QueryRowContext(ctx,
		"SELECT count(*) FROM memory_standing_answer_dependencies").Scan(&count); err == nil {
		t.Fatal("ordinary app role queried trigger-owned dependency table")
	}
	// Stale answers may not be fraudulently declared fresh with a superseded
	// evidence row. They must be refreshed against currently valid evidence.
	_, err := withPrincipalContext(ctx, f.srv.db, requestIdentity{TenantID: f.tenant, PrincipalID: f.owner}, func(tx querier) (bool, error) {
		_, err := tx.ExecContext(ctx, "UPDATE memory_standing_answers SET stale_at=NULL WHERE id=$1", primary)
		return true, err
	})
	if err == nil {
		t.Fatal("superseded evidence was allowed to refresh stale cache")
	}
	// A new active fact and an explicit refresh can replace the evidence.
	replacement := makeFact("replacement evidence")
	_, err = withPrincipalContext(ctx, f.srv.db, requestIdentity{TenantID: f.tenant, PrincipalID: f.owner}, func(tx querier) (bool, error) {
		_, err := tx.ExecContext(ctx,
			"UPDATE memory_standing_answers SET evidence_fact_ids=$2,answer='refreshed',stale_at=NULL,last_refreshed_at=now() WHERE id=$1",
			primary, fmt.Sprintf("[%q]", replacement))
		return true, err
	})
	if err != nil {
		t.Fatalf("valid evidence refresh failed: %v", err)
	}
	check(primary, false)
	if _, err := f.admin.ExecContext(ctx, "UPDATE memory_items SET status='superseded' WHERE id=$1", replacement); err != nil {
		t.Fatal(err)
	}
	check(primary, true)
	if _, err := f.admin.ExecContext(ctx, "DELETE FROM memory_items WHERE id=$1", untouched); err != nil {
		t.Fatalf("delete supporting memory: %v", err)
	}
	check(stable, true)
}

// Verify an unrelated tenant cannot register another tenant's fact as
// cache evidence even when given the exact UUID.
func TestP7StandingAnswerRejectsForeignFact(t *testing.T) {
	f := newBehaviourFixture(t)
	ctx := context.Background()
	foreign := f.otherTenantFact()
	_, err := withPrincipalContext(ctx, f.srv.db, requestIdentity{TenantID: f.tenant, PrincipalID: f.owner}, func(tx querier) (bool, error) {
		_, e := tx.ExecContext(ctx,
			"INSERT INTO memory_standing_answers(tenant_id,namespace_id,question_key,answer,evidence_fact_ids) VALUES($1,$2,'foreign','no', $3)",
			f.tenant, f.ns, fmt.Sprintf("[%q]", foreign))
		return true, e
	})
	if err == nil {
		t.Fatal("accepted foreign-tenant fact as cache provenance")
	}
}

func TestP7StandingAnswerStalesOnUnreviewedCodeChange(t *testing.T) {
	f := newBehaviourFixture(t)
	ctx := context.Background()
	fact := f.scalar(`INSERT INTO memory_items(
   tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,
   authority_level,confidence_score,sensitivity)
   VALUES($1,$2,$3,'semantic','active','code dependent','code dependent',80,0.9,'internal') RETURNING id::text`,
		f.tenant, f.ns, f.owner)
	answer, err := withPrincipalContext(ctx, f.srv.db, requestIdentity{TenantID: f.tenant, PrincipalID: f.owner}, func(tx querier) (string, error) {
		var id string
		e := tx.QueryRowContext(ctx, `INSERT INTO memory_standing_answers(
     tenant_id,namespace_id,question_key,answer,evidence_fact_ids)
     VALUES ($1,$2,'p7 code change','supported',$3) RETURNING id::text`,
			f.tenant, f.ns, fmt.Sprintf("[%q]", fact)).Scan(&id)
		return id, e
	})
	if err != nil {
		t.Fatal(err)
	}
	repo := f.scalar("INSERT INTO code_repositories(tenant_id,canonical_path) VALUES($1,$2) RETURNING id::text", f.tenant, "/standing-p7/"+fact)
	old := f.scalar("INSERT INTO code_repository_snapshots(tenant_id,repository_id,branch,commit_id,status) VALUES($1,$2,'main','before','active') RETURNING id::text", f.tenant, repo)
	newest := f.scalar("INSERT INTO code_repository_snapshots(tenant_id,repository_id,branch,commit_id,status) VALUES($1,$2,'main','after','ready') RETURNING id::text", f.tenant, repo)
	symbol := f.scalar("INSERT INTO code_symbols(repository_id,stable_key,kind,first_snapshot_id) VALUES($1,$2,'file',$3) RETURNING id::text", repo, "code-dependent-"+fact, old)
	version := f.scalar("INSERT INTO code_symbol_versions(symbol_id,snapshot_id,fully_qualified_name,path,kind,content_hash) VALUES($1,$2,'code.guard','src/guard.go','file','abc') RETURNING id::text", symbol, old)
	link := f.scalar("INSERT INTO memory_code_links(tenant_id,fact_id,code_symbol_version_id,link_type,source) VALUES($1,$2,$3,'evidences','explicit') RETURNING id::text", f.tenant, fact, version)
	if _, err := f.admin.ExecContext(ctx, `INSERT INTO memory_revalidation_events(
  tenant_id,fact_id,link_id,old_snapshot_id,new_snapshot_id,
  old_symbol_version_id,continuity,review_status)
  VALUES ($1,$2,$3,$4,$5,$6,'changed','needs_review')`,
		f.tenant, fact, link, old, newest, version); err != nil {
		t.Fatal(err)
	}
	var stale bool
	var reason string
	if err := f.admin.QueryRowContext(ctx,
		"SELECT stale_at IS NOT NULL,stale_reason FROM memory_standing_answers WHERE id=$1", answer).Scan(&stale, &reason); err != nil {
		t.Fatal(err)
	}
	if !stale || reason != "supporting_code_requires_review" {
		t.Fatalf("code-review event did not invalidate dependent standing answer: stale=%v reason=%q", stale, reason)
	}
	// The memory is not changed by this cache-invalidation signal.
	var status string
	if err := f.admin.QueryRowContext(ctx,
		"SELECT status::text FROM memory_items WHERE id=$1", fact).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "active" {
		t.Fatalf("code event mutated approved memory status: %s", status)
	}
	tryRefresh := func() error {
		_, err := withPrincipalContext(ctx, f.srv.db, requestIdentity{
			TenantID: f.tenant, PrincipalID: f.owner,
		}, func(tx querier) (bool, error) {
			_, e := tx.ExecContext(ctx,
				"UPDATE memory_standing_answers SET stale_at=NULL WHERE id=$1", answer)
			return true, e
		})
		return err
	}
	if err := tryRefresh(); err == nil {
		t.Fatal("pending code-review evidence allowed stale-answer refresh")
	}
	event := f.scalar("SELECT id::text FROM memory_revalidation_events WHERE tenant_id=$1 AND fact_id=$2 AND new_snapshot_id=$3", f.tenant, fact, newest)
	if _, err := f.admin.ExecContext(ctx,
		"INSERT INTO memory_revalidation_decisions(event_id,tenant_id,reviewer_id,outcome,reason) VALUES($1,$2,$3,'still_valid','Reviewer checked historical code continuity and approved evidence')",
		event, f.tenant, f.owner); err != nil {
		t.Fatal(err)
	}
	if err := tryRefresh(); err != nil {
		t.Fatalf("reviewed evidence cannot refresh: %v", err)
	}
	if err := f.admin.QueryRowContext(ctx,
		"SELECT stale_at IS NOT NULL FROM memory_standing_answers WHERE id=$1", answer).Scan(&stale); err != nil {
		t.Fatal(err)
	}
	if stale {
		t.Fatal("approved evidence remains incorrectly stale")
	}

}
