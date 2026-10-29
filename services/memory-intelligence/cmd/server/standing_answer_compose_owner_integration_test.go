//go:build integration

package main

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

// A reviewer must not be allowed to reuse an old draft fingerprint after
// the authority owning a source changes, even if its summary and version stay
// byte-for-byte identical. This tests the real restricted-role HTTP path.
func TestP7ComposeReviewFingerprintInvalidatesOnSourceOwnerTransfer(t *testing.T) {
	f := newBehaviourFixture(t)
	makeFact := func(summary string) string {
		return f.scalar(`INSERT INTO memory_items
            (tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,
             authority_level,confidence_score,sensitivity)
            VALUES ($1,$2,$3,'semantic','active','Ownership evidence',$4,80,0.8,'internal')
            RETURNING id::text`, f.tenant, f.ns, f.owner, summary)
	}
	a, b := makeFact("Source A stays unchanged"), makeFact("Source B stays unchanged")
	body := func(expected string) string {
		suffix := ""
		if expected != "" {
			suffix = fmt.Sprintf(`,"expected_draft_fingerprint":%q`, expected)
		}
		return fmt.Sprintf(`{"namespace_id":%q,"fact_ids":[%q,%q]%s}`, f.ns, a, b, suffix)
	}
	compose := func(expected string) (int, map[string]any) {
		t.Helper()
		status, result, _ := f.call(f.srv.handleStandingAnswerCompose, http.MethodPost,
			"/v1/standing-answers/compose", body(expected))
		return status, result
	}
	code, before := compose("")
	if code != http.StatusOK {
		t.Fatalf("initial compose: HTTP %d %v", code, before)
	}
	original, ok := before["draft_fingerprint"].(string)
	if !ok || len(original) != 64 {
		t.Fatalf("missing initial review fingerprint: %v", before)
	}
	replacementOwner := f.scalar(`INSERT INTO principals(tenant_id,kind,external_id,display_name)
        VALUES ($1,'human','p7-owner-'||gen_random_uuid(),'Replacement owner') RETURNING id::text`, f.tenant)
	if _, err := f.admin.ExecContext(context.Background(),
		`UPDATE memory_items SET owner_principal_id=$1 WHERE id=$2`, replacementOwner, a); err != nil {
		t.Fatal(err)
	}
	code, after := compose("")
	if code != http.StatusOK {
		t.Fatalf("compose after owner transfer: HTTP %d %v", code, after)
	}
	if after["draft_fingerprint"] == original {
		t.Fatal("owner transfer did not invalidate the human-review draft fingerprint")
	}
	if after["draft_text"] != before["draft_text"] {
		t.Fatalf("test requires unchanged review prose: before=%v after=%v", before["draft_text"], after["draft_text"])
	}
	evidence, ok := after["evidence"].([]any)
	if !ok || len(evidence) != 2 {
		t.Fatalf("missing composed evidence: %v", after)
	}
	for _, raw := range evidence {
		source := raw.(map[string]any)
		if source["fact_id"] == a && source["owner_principal_id"] != replacementOwner {
			t.Fatalf("transferred source does not identify current owner: %v", source)
		}
	}
	code, stale := compose(original)
	if code != http.StatusConflict || stale["draft_text"] != nil || stale["evidence"] != nil {
		t.Fatalf("stale review precondition accepted or leaked evidence: HTTP %d %v", code, stale)
	}
	var count int
	if err := f.admin.QueryRowContext(context.Background(),
		`SELECT count(*) FROM memory_standing_answers WHERE tenant_id=$1`, f.tenant).Scan(&count); err != nil || count != 0 {
		t.Fatalf("draft persisted without approval: count=%d err=%v", count, err)
	}
}
