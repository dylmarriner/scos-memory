//go:build integration

package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"
)

func TestP7StandingAnswerHTTPReadFailsClosedOnRevocationAndExpiry(t *testing.T) {
	f := newBehaviourFixture(t)
	ctx := context.Background()
	fact := f.scalar(`INSERT INTO memory_items(
        tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,
        authority_level,confidence_score,sensitivity)
        VALUES($1,$2,$3,'semantic','active','answer evidence','answer evidence',80,0.9,'internal')
        RETURNING id::text`, f.tenant, f.ns, f.owner)
	_, err := withPrincipalContext(ctx, f.srv.db, requestIdentity{TenantID: f.tenant, PrincipalID: f.owner}, func(tx querier) (bool, error) {
		_, e := tx.ExecContext(ctx, `INSERT INTO memory_standing_answers(
            tenant_id,namespace_id,question_key,answer,evidence_fact_ids)
            VALUES($1,$2,'where is the evidence?','verified answer',$3)`, f.tenant, f.ns, fmt.Sprintf("[%q]", fact))
		return true, e
	})
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/standing-answers?namespace_id=" + f.ns + "&question_key=" + url.QueryEscape("  WHERE IS THE EVIDENCE? ")
	code, body, _ := f.call(f.srv.handleStandingAnswerRead, http.MethodGet, path, "")
	if code != http.StatusOK || body["answer"] != "verified answer" || body["question_key"] != "where is the evidence?" {
		t.Fatalf("fresh answer: %d %v", code, body)
	}
	if evidence, ok := body["evidence_fact_ids"].([]any); !ok || len(evidence) != 1 || evidence[0] != fact {
		t.Fatalf("missing or wrong provenance: %v", body)
	}
	code, _, _ = f.callAs(f.otherTenant, f.other, f.srv.handleStandingAnswerRead, http.MethodGet, path, "")
	if code != http.StatusNotFound {
		t.Fatalf("foreign tenant answer exposed: %d", code)
	}
	if _, err := f.admin.ExecContext(ctx, "UPDATE namespace_memberships SET can_read=false WHERE namespace_id=$1 AND principal_id=$2", f.ns, f.owner); err != nil {
		t.Fatal(err)
	}
	code, _, _ = f.call(f.srv.handleStandingAnswerRead, http.MethodGet, path, "")
	if code != http.StatusNotFound {
		t.Fatalf("revoked member answer exposed: %d", code)
	}
	if _, err := f.admin.ExecContext(ctx, "UPDATE namespace_memberships SET can_read=true WHERE namespace_id=$1 AND principal_id=$2", f.ns, f.owner); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.ExecContext(ctx, "UPDATE memory_items SET status='revoked' WHERE id=$1", fact); err != nil {
		t.Fatal(err)
	}
	code, _, _ = f.call(f.srv.handleStandingAnswerRead, http.MethodGet, path, "")
	if code != http.StatusNotFound {
		t.Fatalf("revoked fact answer exposed: %d", code)
	}

	expires := time.Now().UTC().Add(5 * time.Second)
	expiring := f.scalar(`INSERT INTO memory_items(
        tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,
        authority_level,confidence_score,sensitivity,expires_at)
        VALUES($1,$2,$3,'semantic','active','expiring evidence','expiring evidence',80,0.9,'internal',$4)
        RETURNING id::text`, f.tenant, f.ns, f.owner, expires)
	_, err = withPrincipalContext(ctx, f.srv.db, requestIdentity{TenantID: f.tenant, PrincipalID: f.owner}, func(tx querier) (bool, error) {
		_, e := tx.ExecContext(ctx, `INSERT INTO memory_standing_answers(
            tenant_id,namespace_id,question_key,answer,evidence_fact_ids)
            VALUES($1,$2,'expiring question','temporary answer',$3)`, f.tenant, f.ns, fmt.Sprintf("[%q]", expiring))
		return true, e
	})
	if err != nil {
		t.Fatal(err)
	}
	expiryPath := "/v1/standing-answers?namespace_id=" + f.ns + "&question_key=expiring%20question"
	code, _, _ = f.call(f.srv.handleStandingAnswerRead, http.MethodGet, expiryPath, "")
	if code != http.StatusOK {
		t.Fatalf("unexpired answer unavailable: %d", code)
	}
	if remaining := time.Until(expires.Add(200 * time.Millisecond)); remaining > 0 {
		time.Sleep(remaining)
	}
	code, _, _ = f.call(f.srv.handleStandingAnswerRead, http.MethodGet, expiryPath, "")
	if code != http.StatusNotFound {
		t.Fatalf("clock-expired answer exposed: %d", code)
	}
}
