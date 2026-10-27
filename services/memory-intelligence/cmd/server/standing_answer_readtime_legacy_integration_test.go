//go:build integration

package main

import (
	"context"
	"testing"
)

// Legacy free-text evidence is intentionally untrusted. A stale legacy row
// with malformed JSON must neither be served nor crash the read projection.
func TestP7StandingAnswerCurrentViewIgnoresMalformedLegacyEvidence(t *testing.T) {
	f := newBehaviourFixture(t)
	ctx := context.Background()
	_, err := withPrincipalContext(ctx, f.srv.db, requestIdentity{
		TenantID: f.tenant, PrincipalID: f.owner,
	}, func(tx querier) (bool, error) {
		_, err := tx.ExecContext(ctx, `INSERT INTO memory_standing_answers(
			tenant_id,namespace_id,question_key,answer,evidence_fact_ids,stale_at,stale_reason)
			VALUES($1,$2,'malformed legacy','unverified','not valid json',now(),'unverified_legacy_evidence')`,
			f.tenant, f.ns)
		return true, err
	})
	if err != nil {
		t.Fatal(err)
	}
	n, err := withPrincipalContext(ctx, f.srv.db, requestIdentity{
		TenantID: f.tenant, PrincipalID: f.owner,
	}, func(tx querier) (int, error) {
		var count int
		err := tx.QueryRowContext(ctx,
			"SELECT count(*) FROM memory_standing_answers_current WHERE question_key='malformed legacy'").Scan(&count)
		return count, err
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("unverified legacy answer was served: %d", n)
	}
}
