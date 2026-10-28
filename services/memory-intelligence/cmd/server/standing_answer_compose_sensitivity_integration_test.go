//go:build integration

package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// A reviewer must not be allowed to reuse a pinned draft after a source's
// sensitivity changes within the same still-readable namespace.
func TestP7ComposeExpectedFingerprintRejectsReadableSensitivityDrift(t *testing.T) {
	f := newBehaviourFixture(t)
	makeFact := func(summary string) string {
		return f.scalar(`INSERT INTO memory_items
            (tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,
             authority_level,confidence_score,sensitivity)
            VALUES ($1,$2,$3,'semantic','active','Sensitivity draft',$4,80,0.8,'public')
            RETURNING id::text`, f.tenant, f.ns, f.owner, summary)
	}
	a, b := makeFact("First evidence"), makeFact("Second evidence")
	compose := func(expected string) (int, map[string]any) {
		suffix := ""
		if expected != "" {
			suffix = fmt.Sprintf(`,"expected_draft_fingerprint":%q`, expected)
		}
		body := fmt.Sprintf(`{"namespace_id":%q,"fact_ids":[%q,%q]%s}`, f.ns, a, b, suffix)
		status, result, _ := f.call(f.srv.handleStandingAnswerCompose,
			http.MethodPost, "/v1/standing-answers/compose", body)
		return status, result
	}
	status, first := compose("")
	if status != http.StatusOK {
		t.Fatalf("initial draft: HTTP %d %v", status, first)
	}
	pinned, ok := first["draft_fingerprint"].(string)
	if !ok || len(pinned) != 64 {
		t.Fatalf("missing draft fingerprint: %v", first)
	}
	status, unchanged := compose(pinned)
	if status != http.StatusOK || unchanged["draft_fingerprint"] != pinned {
		t.Fatalf("unchanged draft precondition: HTTP %d %v", status, unchanged)
	}
	if _, err := f.admin.ExecContext(context.Background(),
		"UPDATE memory_items SET sensitivity='internal' WHERE id=$1", b); err != nil {
		t.Fatal(err)
	}
	status, stale := compose(pinned)
	if status != http.StatusConflict || stale["draft_text"] != nil || stale["evidence"] != nil {
		t.Fatalf("classification drift accepted or leaked draft: HTTP %d %v", status, stale)
	}
	status, refreshed := compose("")
	if status != http.StatusOK || refreshed["draft_fingerprint"] == pinned {
		t.Fatalf("classification drift failed to change fingerprint: HTTP %d %v", status, refreshed)
	}
	evidence := refreshed["evidence"].([]any)
	classes := make(map[string]string)
	for _, raw := range evidence {
		item := raw.(map[string]any)
		classes[item["fact_id"].(string)] = item["sensitivity"].(string)
	}
	if classes[a] != "public" || classes[b] != "internal" {
		t.Fatalf("review evidence did not expose current classification: %v", classes)
	}
	if strings.Contains(fmt.Sprint(stale), "Second evidence") {
		t.Fatalf("stale precondition leaked source content: %v", stale)
	}
}
