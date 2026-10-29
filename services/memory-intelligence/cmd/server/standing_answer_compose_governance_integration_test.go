//go:build integration

package main

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

func TestP7ComposePinnedDraftRejectsAuthorityAndValidityDrift(t *testing.T) {
	f := newBehaviourFixture(t)
	makeFact := func(summary string) string {
		return f.scalar(`INSERT INTO memory_items
            (tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,
             authority_level,confidence_score,sensitivity,valid_from,valid_until,expires_at)
            VALUES ($1,$2,$3,'semantic','active','Governance review',$4,80,0.8,'internal',
                    '2026-10-01T00:00:00Z','2026-12-01T00:00:00Z','2026-12-15T00:00:00Z')
            RETURNING id::text`, f.tenant, f.ns, f.owner, summary)
	}
	a, b := makeFact("Authority source"), makeFact("Other source")
	compose := func(expected string) (int, map[string]any) {
		t.Helper()
		precondition := ""
		if expected != "" {
			precondition = fmt.Sprintf(`,"expected_draft_fingerprint":%q`, expected)
		}
		body := fmt.Sprintf(`{"namespace_id":%q,"fact_ids":[%q,%q]%s}`, f.ns, a, b, precondition)
		code, result, _ := f.call(f.srv.handleStandingAnswerCompose, http.MethodPost,
			"/v1/standing-answers/compose", body)
		return code, result
	}
	code, first := compose("")
	if code != http.StatusOK {
		t.Fatalf("initial composition HTTP %d: %v", code, first)
	}
	expected, ok := first["draft_fingerprint"].(string)
	if !ok || len(expected) != 64 {
		t.Fatalf("missing initial digest: %v", first)
	}
	evidence := first["evidence"].([]any)[0].(map[string]any)
	if evidence["authority_level"] == nil || evidence["valid_from"] == nil ||
		evidence["valid_until"] == nil || evidence["expires_at"] == nil {
		t.Fatalf("reviewable evidence omits authority or validity metadata: %v", evidence)
	}
	updates := []struct{ name, query string }{
		{"authority downgrade", "UPDATE memory_items SET authority_level=5 WHERE id=$1"},
		{"valid-from change", "UPDATE memory_items SET valid_from='2026-10-02T00:00:00Z' WHERE id=$1"},
		{"valid-until change", "UPDATE memory_items SET valid_until='2026-11-30T00:00:00Z' WHERE id=$1"},
		{"expiry change", "UPDATE memory_items SET expires_at='2026-12-14T00:00:00Z' WHERE id=$1"},
	}
	for _, tc := range updates {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := f.admin.ExecContext(context.Background(), tc.query, a); err != nil {
				t.Fatal(err)
			}
			status, stale := compose(expected)
			if status != http.StatusConflict || stale["evidence"] != nil || stale["draft_text"] != nil {
				t.Fatalf("stale governance review accepted or disclosed evidence: HTTP %d %v", status, stale)
			}
			status, refreshed := compose("")
			if status != http.StatusOK || refreshed["draft_fingerprint"] == expected {
				t.Fatalf("governance change did not refresh digest: HTTP %d %v", status, refreshed)
			}
			expected = refreshed["draft_fingerprint"].(string)
		})
	}
	var count int
	if err := f.admin.QueryRowContext(context.Background(),
		"SELECT count(*) FROM memory_standing_answers WHERE tenant_id=$1", f.tenant).Scan(&count); err != nil || count != 0 {
		t.Fatalf("unapproved composition persisted: count=%d err=%v", count, err)
	}
}
