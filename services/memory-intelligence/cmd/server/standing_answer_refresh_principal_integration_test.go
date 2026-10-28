//go:build integration

package main

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

// A still-valid token must not refresh a cache after the principal is disabled,
// even if its old namespace membership and write flags remain in the database.
func TestP7RefreshRejectsDisabledPrincipalWithRetainedWriteMembership(t *testing.T) {
	f := newBehaviourFixture(t)
	fact := f.scalar(`INSERT INTO memory_items (
        tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,
        authority_level,confidence_score,sensitivity)
        VALUES ($1,$2,$3,'semantic','active','Disabled refresh',
                'Governed summary',80,0.9,'internal') RETURNING id::text`,
		f.tenant, f.ns, f.owner)
	body := fmt.Sprintf(`{"fact_id":%q}`, fact)
	call := func() int {
		code, _, _ := f.call(f.srv.handleStandingAnswerRefresh, http.MethodPost,
			"/v1/standing-answers/refresh", body)
		return code
	}
	if got := call(); got != http.StatusOK {
		t.Fatalf("enabled principal baseline HTTP %d", got)
	}
	if _, err := f.admin.ExecContext(context.Background(),
		"UPDATE principals SET disabled_at=statement_timestamp() WHERE id=$1", f.owner); err != nil {
		t.Fatal(err)
	}
	if got := call(); got != http.StatusNotFound {
		t.Fatalf("disabled principal with retained write membership refreshed cache: HTTP %d", got)
	}
}

// A namespace membership row alone is not proof that the authenticated
// principal belongs to the asserted tenant. Refuse mismatched claim pairs.
func TestP7RefreshRejectsCrossTenantPrincipalWithForgedMembership(t *testing.T) {
	f := newBehaviourFixture(t)
	fact := f.scalar(`INSERT INTO memory_items (
        tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,
        authority_level,confidence_score,sensitivity)
        VALUES ($1,$2,$3,'semantic','active','Cross tenant refresh',
                'Governed summary',80,0.9,'internal') RETURNING id::text`,
		f.tenant, f.ns, f.owner)
	if _, err := f.admin.ExecContext(context.Background(),
		`INSERT INTO namespace_memberships(namespace_id,principal_id,can_read,can_write_candidates)
          VALUES ($1,$2,true,true)`, f.ns, f.other); err != nil {
		t.Fatal(err)
	}
	code, _, _ := f.callAs(f.tenant, f.other, f.srv.handleStandingAnswerRefresh,
		http.MethodPost, "/v1/standing-answers/refresh", fmt.Sprintf(`{"fact_id":%q}`, fact))
	if code != http.StatusNotFound {
		t.Fatalf("foreign-tenant principal with forged membership refreshed cache: HTTP %d", code)
	}
}

// Previously materialized answers must also disappear when a token outlives
// account suspension. RLS membership alone does not encode principal status.
func TestP7ReadRejectsDisabledPrincipalWithRetainedMembership(t *testing.T) {
	f := newBehaviourFixture(t)
	fact := f.scalar(`INSERT INTO memory_items (
        tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,
        authority_level,confidence_score,sensitivity)
        VALUES ($1,$2,$3,'semantic','active','Read access',
                'Current governed summary',80,0.9,'internal') RETURNING id::text`,
		f.tenant, f.ns, f.owner)
	f.scalar(`INSERT INTO memory_standing_answers (
        tenant_id,namespace_id,question_key,answer,evidence_fact_ids)
        VALUES ($1,$2,'read access','Current governed summary',$3) RETURNING id::text`,
		f.tenant, f.ns, fmt.Sprintf("[%q]", fact))
	path := "/v1/standing-answers?namespace_id=" + f.ns + "&question_key=read%20access"
	code, _, _ := f.call(f.srv.handleStandingAnswerRead, http.MethodGet, path, "")
	if code != http.StatusOK {
		t.Fatalf("enabled principal baseline HTTP %d", code)
	}
	if _, err := f.admin.ExecContext(context.Background(),
		"UPDATE principals SET disabled_at=statement_timestamp() WHERE id=$1", f.owner); err != nil {
		t.Fatal(err)
	}
	code, _, _ = f.call(f.srv.handleStandingAnswerRead, http.MethodGet, path, "")
	if code != http.StatusNotFound {
		t.Fatalf("disabled principal with retained membership read cached answer: HTTP %d", code)
	}
}

func TestP7ReadRejectsCrossTenantPrincipalWithForgedMembership(t *testing.T) {
	f := newBehaviourFixture(t)
	fact := f.scalar(`INSERT INTO memory_items (
        tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,
        authority_level,confidence_score,sensitivity)
        VALUES ($1,$2,$3,'semantic','active','Read access',
                'Current governed summary',80,0.9,'internal') RETURNING id::text`,
		f.tenant, f.ns, f.owner)
	f.scalar(`INSERT INTO memory_standing_answers (
        tenant_id,namespace_id,question_key,answer,evidence_fact_ids)
        VALUES ($1,$2,'read access','Current governed summary',$3) RETURNING id::text`,
		f.tenant, f.ns, fmt.Sprintf("[%q]", fact))
	if _, err := f.admin.ExecContext(context.Background(),
		`INSERT INTO namespace_memberships(namespace_id,principal_id,can_read)
          VALUES ($1,$2,true)`, f.ns, f.other); err != nil {
		t.Fatal(err)
	}
	path := "/v1/standing-answers?namespace_id=" + f.ns + "&question_key=read%20access"
	code, _, _ := f.callAs(f.tenant, f.other, f.srv.handleStandingAnswerRead,
		http.MethodGet, path, "")
	if code != http.StatusNotFound {
		t.Fatalf("foreign-tenant principal with forged membership read cached answer: HTTP %d", code)
	}
}
