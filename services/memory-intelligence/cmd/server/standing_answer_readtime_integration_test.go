//go:build integration

package main

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// The projection must reject evidence that expires solely through passage of
// time, even when no UPDATE trigger has had an opportunity to mark it stale.
// All reads are made through the restricted scos_app role with forced RLS.
func TestP7StandingAnswerCurrentViewRequiresLiveEvidence(t *testing.T) {
	f := newBehaviourFixture(t)
	ctx := context.Background()
	expires := time.Now().UTC().Add(4 * time.Second)
	fact := f.scalar(`INSERT INTO memory_items(
		tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,
		authority_level,confidence_score,sensitivity,expires_at)
		VALUES($1,$2,$3,'semantic','active','expiring evidence','expiring evidence',
		80,0.9,'internal',$4) RETURNING id::text`,
		f.tenant, f.ns, f.owner, expires)
	var answer string
	err := f.admin.QueryRowContext(ctx, `INSERT INTO memory_standing_answers(
			tenant_id,namespace_id,question_key,answer,evidence_fact_ids)
			VALUES($1,$2,'readtime expiry','supported',$3) RETURNING id::text`,
		f.tenant, f.ns, fmt.Sprintf("[%q]", fact)).Scan(&answer)
	if err != nil {
		t.Fatal(err)
	}
	countAs := func(tenant, principal string) int {
		t.Helper()
		count, err := withPrincipalContext(ctx, f.srv.db, requestIdentity{
			TenantID: tenant, PrincipalID: principal,
		}, func(tx querier) (int, error) {
			var n int
			err := tx.QueryRowContext(ctx,
				"SELECT count(*) FROM memory_standing_answers_current WHERE id=$1",
				answer).Scan(&n)
			return n, err
		})
		if err != nil {
			t.Fatal(err)
		}
		return count
	}
	if got := countAs(f.tenant, f.owner); got != 1 {
		t.Fatalf("fresh evidence should be readable, got %d", got)
	}
	if got := countAs(f.otherTenant, f.other); got != 0 {
		t.Fatalf("cross-tenant standing answer exposed: %d", got)
	}
	// The caller may begin a transaction before expiry and issue the SELECT
	// after expiry. now() is frozen at transaction start and is unsafe for
	// this read-time contract; statement_timestamp() is not.
	longTransactionCount, err := withPrincipalContext(ctx, f.srv.db, requestIdentity{
		TenantID: f.tenant, PrincipalID: f.owner,
	}, func(tx querier) (int, error) {
		if remaining := time.Until(expires.Add(200 * time.Millisecond)); remaining > 0 {
			time.Sleep(remaining)
		}
		var n int
		err := tx.QueryRowContext(ctx,
			"SELECT count(*) FROM memory_standing_answers_current WHERE id=$1",
			answer).Scan(&n)
		return n, err
	})
	if err != nil {
		t.Fatal(err)
	}
	if longTransactionCount != 0 {
		t.Fatalf("expired answer visible in long-running transaction: %d", longTransactionCount)
	}
	// A wall-clock transition does not invoke the mutation triggers.
	var stillMarkedFresh bool
	if err := f.admin.QueryRowContext(ctx,
		"SELECT stale_at IS NULL FROM memory_standing_answers WHERE id=$1",
		answer).Scan(&stillMarkedFresh); err != nil {
		t.Fatal(err)
	}
	if !stillMarkedFresh {
		t.Fatal("test precondition: expiry unexpectedly mutated the cache row")
	}
	if got := countAs(f.tenant, f.owner); got != 0 {
		t.Fatalf("expired evidence was served as fresh: %d", got)
	}
}

func TestP7StandingAnswerCurrentViewRequiresMembership(t *testing.T) {
	f := newBehaviourFixture(t)
	ctx := context.Background()
	fact := f.scalar(`INSERT INTO memory_items(
		tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,
		authority_level,confidence_score,sensitivity)
		VALUES($1,$2,$3,'semantic','active','membership evidence','membership evidence',
		80,0.9,'internal') RETURNING id::text`, f.tenant, f.ns, f.owner)
	_, err := f.admin.ExecContext(ctx, `INSERT INTO memory_standing_answers(
			tenant_id,namespace_id,question_key,answer,evidence_fact_ids)
			VALUES($1,$2,'membership read','supported',$3)`,
		f.tenant, f.ns, fmt.Sprintf("[%q]", fact))
	if err != nil {
		t.Fatal(err)
	}
	count := func() int {
		t.Helper()
		n, err := withPrincipalContext(ctx, f.srv.db, requestIdentity{
			TenantID: f.tenant, PrincipalID: f.owner,
		}, func(tx querier) (int, error) {
			var count int
			err := tx.QueryRowContext(ctx,
				"SELECT count(*) FROM memory_standing_answers_current WHERE question_key='membership read'").Scan(&count)
			return count, err
		})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if got := count(); got != 1 {
		t.Fatalf("authorized cache not visible: %d", got)
	}
	if _, err := f.admin.ExecContext(ctx,
		"UPDATE namespace_memberships SET can_read=false WHERE namespace_id=$1 AND principal_id=$2",
		f.ns, f.owner); err != nil {
		t.Fatal(err)
	}
	if got := count(); got != 0 {
		t.Fatalf("revoked reader can still see standing answer: %d", got)
	}
}
