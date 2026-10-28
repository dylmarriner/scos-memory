//go:build integration

package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// The reviewable composition must be pinned to the exact ordered evidence set
// and displayed draft, not just independent per-fact hashes. This is not an
// approval or publication operation.
func TestP7ComposeDraftManifestTracksEvidenceSetAndSourceChanges(t *testing.T) {
	f := newBehaviourFixture(t)
	makeFact := func(summary string) string {
		return f.scalar(`INSERT INTO memory_items (
            tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,
            authority_level,confidence_score,sensitivity)
            VALUES ($1,$2,$3,'semantic','active','Manifest evidence',$4,80,0.8,'internal')
            RETURNING id::text`, f.tenant, f.ns, f.owner, summary)
	}
	a, b, c := makeFact("Alpha evidence"), makeFact("Beta evidence"), makeFact("Gamma evidence")
	compose := func(ids ...string) (string, map[string]any) {
		t.Helper()
		quoted := make([]string, 0, len(ids))
		for _, id := range ids {
			quoted = append(quoted, fmt.Sprintf("%q", id))
		}
		body := fmt.Sprintf(`{"namespace_id":%q,"fact_ids":[%s]}`, f.ns, strings.Join(quoted, ","))
		code, result, _ := f.call(f.srv.handleStandingAnswerCompose, http.MethodPost,
			"/v1/standing-answers/compose", body)
		if code != http.StatusOK || result["status"] != "draft_requires_review" {
			t.Fatalf("compose status=%d result=%v", code, result)
		}
		digest, ok := result["draft_fingerprint"].(string)
		if !ok || len(digest) != 64 {
			t.Fatalf("missing SHA-256 draft manifest: %v", result)
		}
		return digest, result
	}
	original, originalResult := compose(a, b)
	reversed, _ := compose(b, a)
	if reversed != original {
		t.Fatalf("same evidence in different request order changed manifest: %s vs %s", original, reversed)
	}
	// UUID claims are case-insensitive identities in PostgreSQL. Their text
	// spelling must not change a review manifest for identical evidence.
	body := fmt.Sprintf(`{"namespace_id":%q,"fact_ids":[%q,%q]}`, f.ns, a, b)
	code, upper, _ := f.callAs(strings.ToUpper(f.tenant), strings.ToUpper(f.owner),
		f.srv.handleStandingAnswerCompose, http.MethodPost, "/v1/standing-answers/compose", body)
	if code != http.StatusOK || upper["draft_fingerprint"] != original {
		t.Fatalf("same tenant/principal UUID with uppercase claims changed manifest: HTTP %d original=%s upper=%v", code, original, upper)
	}
	withThird, _ := compose(a, b, c)
	if withThird == original {
		t.Fatal("adding a source did not invalidate the draft manifest")
	}
	if _, err := f.admin.ExecContext(context.Background(),
		"UPDATE memory_items SET canonical_summary='Changed beta evidence' WHERE id=$1", b); err != nil {
		t.Fatal(err)
	}
	revised, revisedResult := compose(a, b)
	if revised == original || revisedResult["draft_text"] == originalResult["draft_text"] {
		t.Fatalf("source mutation did not invalidate review manifest: old=%s new=%s", original, revised)
	}
	var cacheCount int
	if err := f.admin.QueryRowContext(context.Background(),
		"SELECT count(*) FROM memory_standing_answers WHERE tenant_id=$1", f.tenant).Scan(&cacheCount); err != nil || cacheCount != 0 {
		t.Fatalf("unapproved composition persisted: count=%d err=%v", cacheCount, err)
	}
}
