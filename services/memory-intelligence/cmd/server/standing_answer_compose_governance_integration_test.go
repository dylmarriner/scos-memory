//go:build integration

package main

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestP7ComposePinnedDraftRejectsAuthorityAndValidityDrift(t *testing.T) {
	f := newBehaviourFixture(t)
	// Use one database clock anchor so this regression cannot expire.
	var anchor time.Time
	if err := f.admin.QueryRowContext(context.Background(), "SELECT statement_timestamp()").Scan(&anchor); err != nil {
		t.Fatal(err)
	}
	makeFact := func(summary string) string {
		return f.scalar(`INSERT INTO memory_items
            (tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,
             authority_level,confidence_score,sensitivity,valid_from,valid_until,expires_at)
            VALUES ($1,$2,$3,'semantic','active','Governance review',$4,80,0.8,'internal',
                    $5,$6,$7) RETURNING id::text`,
			f.tenant, f.ns, f.owner, summary,
			anchor.Add(-30*24*time.Hour), anchor.Add(60*24*time.Hour), anchor.Add(90*24*time.Hour))
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
	updates := []struct {
		name, query string
		value       any
	}{
		{"authority downgrade", "UPDATE memory_items SET authority_level=5 WHERE id=$1", nil},
		{"valid-from change", "UPDATE memory_items SET valid_from=$2 WHERE id=$1", anchor.Add(-29 * 24 * time.Hour)},
		{"valid-until change", "UPDATE memory_items SET valid_until=$2 WHERE id=$1", anchor.Add(59 * 24 * time.Hour)},
		{"expiry change", "UPDATE memory_items SET expires_at=$2 WHERE id=$1", anchor.Add(89 * 24 * time.Hour)},
	}
	for _, tc := range updates {
		t.Run(tc.name, func(t *testing.T) {
			args := []any{a}
			if tc.value != nil {
				args = append(args, tc.value)
			}
			if _, err := f.admin.ExecContext(context.Background(), tc.query, args...); err != nil {
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
	// An expired source is not eligible for review. Authorization and
	// validity filtering take precedence over fingerprint comparison (404).
	if _, err := f.admin.ExecContext(context.Background(),
		"UPDATE memory_items SET valid_until=$2 WHERE id=$1", a, anchor.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	status, unavailable := compose(expected)
	if status != http.StatusNotFound || unavailable["evidence"] != nil || unavailable["draft_text"] != nil {
		t.Fatalf("expired source disclosed review evidence: HTTP %d %v", status, unavailable)
	}
	var count int
	if err := f.admin.QueryRowContext(context.Background(),
		"SELECT count(*) FROM memory_standing_answers WHERE tenant_id=$1", f.tenant).Scan(&count); err != nil || count != 0 {
		t.Fatalf("unapproved composition persisted: count=%d err=%v", count, err)
	}
}

func TestP7ComposeSupportsPostgresInfinityValidity(t *testing.T) {
	f := newBehaviourFixture(t)
	makeFact := func(summary string) string {
		return f.scalar(`INSERT INTO memory_items
            (tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,
             authority_level,confidence_score,sensitivity,valid_from,valid_until,expires_at)
            VALUES ($1,$2,$3,'semantic','active','Infinite window',$4,80,0.8,'internal',
                    '-infinity'::timestamptz,'infinity'::timestamptz,'infinity'::timestamptz)
            RETURNING id::text`, f.tenant, f.ns, f.owner, summary)
	}
	a, b := makeFact("Infinite window A"), makeFact("Infinite window B")
	compose := func(expected string) (int, map[string]any) {
		suffix := ""
		if expected != "" {
			suffix = fmt.Sprintf(`,"expected_draft_fingerprint":%q`, expected)
		}
		body := fmt.Sprintf(`{"namespace_id":%q,"fact_ids":[%q,%q]%s}`, f.ns, a, b, suffix)
		status, result, _ := f.call(f.srv.handleStandingAnswerCompose, http.MethodPost,
			"/v1/standing-answers/compose", body)
		return status, result
	}
	status, first := compose("")
	if status != http.StatusOK {
		t.Fatalf("infinity source rejected: HTTP %d %v", status, first)
	}
	evidence := first["evidence"].([]any)[0].(map[string]any)
	if evidence["valid_from"] != "-infinity" || evidence["valid_until"] != "infinity" || evidence["expires_at"] != "infinity" {
		t.Fatalf("infinity bounds lost in review metadata: %v", evidence)
	}
	pinned := first["draft_fingerprint"].(string)
	if _, err := f.admin.ExecContext(context.Background(),
		"UPDATE memory_items SET expires_at=statement_timestamp()+interval '90 days' WHERE id=$1", a); err != nil {
		t.Fatal(err)
	}
	status, stale := compose(pinned)
	if status != http.StatusConflict || stale["draft_text"] != nil || stale["evidence"] != nil {
		t.Fatalf("infinity-to-finite change did not invalidate review: HTTP %d %v", status, stale)
	}
}
