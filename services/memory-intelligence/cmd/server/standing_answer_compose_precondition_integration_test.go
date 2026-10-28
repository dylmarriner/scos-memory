//go:build integration

package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// The review-time precondition must be checked against the currently
// authorized snapshot and must never return a stale draft for publication.
func TestP7ComposeExpectedDraftFingerprintFailsClosedOnSourceDrift(t *testing.T) {
	f := newBehaviourFixture(t)
	makeFact := func(summary string) string {
		return f.scalar(`INSERT INTO memory_items
            (tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,authority_level,confidence_score,sensitivity)
            VALUES ($1,$2,$3,'semantic','active','Pinned draft',$4,80,0.8,'internal') RETURNING id::text`,
			f.tenant, f.ns, f.owner, summary)
	}
	a, b := makeFact("Alpha review source"), makeFact("Beta review source")
	body := func(expected string) string {
		suffix := ""
		if expected != "" {
			suffix = fmt.Sprintf(`,"expected_draft_fingerprint":%q`, expected)
		}
		return fmt.Sprintf(`{"namespace_id":%q,"fact_ids":[%q,%q]%s}`, f.ns, a, b, suffix)
	}
	call := func(expected string) (int, map[string]any) {
		code, result, _ := f.call(f.srv.handleStandingAnswerCompose, http.MethodPost,
			"/v1/standing-answers/compose", body(expected))
		return code, result
	}
	code, first := call("")
	if code != http.StatusOK {
		t.Fatalf("initial composition: %d %v", code, first)
	}
	expected, ok := first["draft_fingerprint"].(string)
	if !ok || len(expected) != 64 {
		t.Fatalf("missing initial fingerprint: %v", first)
	}

	code, pinned := call(expected)
	if code != http.StatusOK || pinned["draft_fingerprint"] != expected {
		t.Fatalf("unchanged pinned composition: %d %v", code, pinned)
	}
	if _, err := f.admin.ExecContext(context.Background(),
		"UPDATE memory_items SET canonical_summary='Beta changed after review' WHERE id=$1", b); err != nil {
		t.Fatal(err)
	}
	code, stale := call(expected)
	if code != http.StatusConflict || stale["draft_text"] != nil || stale["evidence"] != nil {
		t.Fatalf("stale fingerprint leaked or accepted a draft: %d %v", code, stale)
	}
	code, updated := call("")
	if code != http.StatusOK || updated["draft_fingerprint"] == expected {
		t.Fatalf("updated source failed to change fingerprint: %d %v", code, updated)
	}
	// Authorization is checked before fingerprint comparison, so callers
	// cannot use the precondition to distinguish inaccessible evidence.
	code, denied, _ := f.callAs(f.otherTenant, f.other, f.srv.handleStandingAnswerCompose,
		http.MethodPost, "/v1/standing-answers/compose", body(expected))
	if code != http.StatusNotFound || strings.Contains(fmt.Sprint(denied), "Beta changed") {
		t.Fatalf("cross-tenant precondition leaked evidence: %d %v", code, denied)
	}
	var count int
	if err := f.admin.QueryRowContext(context.Background(),
		"SELECT count(*) FROM memory_standing_answers WHERE tenant_id=$1", f.tenant).Scan(&count); err != nil || count != 0 {
		t.Fatalf("review precondition persisted unapproved answer: count=%d err=%v", count, err)
	}
}
