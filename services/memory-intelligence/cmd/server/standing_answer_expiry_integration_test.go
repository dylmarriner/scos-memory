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

// A generated answer can become unreadable solely because the source clock
// expires, without a row UPDATE to set stale_at. A verified successor must
// replace that dead cache key without permitting live-source or curated
// answer replacement.
func TestP7RefreshReplacesClockExpiredSourceWithoutMutation(t *testing.T) {
	for _, expiryColumn := range []string{"expires_at", "valid_until"} {
		t.Run(expiryColumn, func(t *testing.T) {
			f := newBehaviourFixture(t)
			first := f.scalar(fmt.Sprintf(`INSERT INTO memory_items (
                tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,
                authority_level,confidence_score,sensitivity,%s)
                VALUES ($1,$2,$3,'semantic','active','Clock expiry successor',
                        'original answer',80,0.8,'internal',statement_timestamp()+interval '3 seconds')
                RETURNING id::text`, expiryColumn), f.tenant, f.ns, f.owner)
			second := f.scalar(`INSERT INTO memory_items (
                tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,
                authority_level,confidence_score,sensitivity)
                VALUES ($1,$2,$3,'semantic','active','Clock expiry successor',
                        'replacement answer',80,0.8,'internal')
                RETURNING id::text`, f.tenant, f.ns, f.owner)
			refresh := func(id string) (int, map[string]any) {
				returnCode, out, _ := f.call(f.srv.handleStandingAnswerRefresh, http.MethodPost,
					"/v1/standing-answers/refresh", fmt.Sprintf(`{"fact_id":%q}`, id))
				return returnCode, out
			}
			code, initial := refresh(first)
			if code != http.StatusOK {
				t.Fatalf("initial refresh: HTTP %d %v", code, initial)
			}
			if code, _ = refresh(second); code != http.StatusConflict {
				t.Fatalf("live source was overwritten: HTTP %d", code)
			}
			// Wait for the DB clock, not a direct UPDATE; the invalidation
			// trigger cannot mark this row stale when no data is mutated.
			var expiry time.Time
			if err := f.admin.QueryRowContext(context.Background(),
				fmt.Sprintf("SELECT %s FROM memory_items WHERE id=$1", expiryColumn), first).Scan(&expiry); err != nil {
				t.Fatal(err)
			}
			if delay := time.Until(expiry.Add(150 * time.Millisecond)); delay > 0 {
				time.Sleep(delay)
			}
			readPath := "/v1/standing-answers?namespace_id=" + f.ns +
				"&question_key=" + url.QueryEscape("Clock expiry successor")
			if code, _, _ := f.call(f.srv.handleStandingAnswerRead, http.MethodGet, readPath, ""); code != http.StatusNotFound {
				t.Fatalf("expired source still served: HTTP %d", code)
			}
			var staleAt *time.Time
			if err := f.admin.QueryRowContext(context.Background(),
				"SELECT stale_at FROM memory_standing_answers WHERE id=$1", initial["id"]).Scan(&staleAt); err != nil {
				t.Fatal(err)
			}
			if staleAt != nil {
				t.Fatalf("precondition failed: clock expiry unexpectedly set stale_at=%v", *staleAt)
			}
			code, replacement := refresh(second)
			if code != http.StatusOK || replacement["id"] != initial["id"] {
				t.Fatalf("expired generated answer not replaced: HTTP %d %v", code, replacement)
			}
			if code, answer, _ := f.call(f.srv.handleStandingAnswerRead, http.MethodGet, readPath, ""); code != http.StatusOK || answer["answer"] != "replacement answer" {
				t.Fatalf("replacement unavailable: HTTP %d %v", code, answer)
			}
		})
	}
}

// Losing permission to read the original source is not objective expiry.
// A writer must not seize another source's cache key merely because the
// original evidence is no longer visible through the security-invoker view.
func TestP7RefreshDoesNotReplaceRestrictedUnexpiredSource(t *testing.T) {
	f := newBehaviourFixture(t)
	if _, err := f.admin.ExecContext(context.Background(),
		"UPDATE namespaces SET sensitivity_ceiling='confidential' WHERE id=$1", f.ns); err != nil {
		t.Fatal(err)
	}
	first := f.scalar(`INSERT INTO memory_items (
         tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,
         authority_level,confidence_score,sensitivity)
         VALUES ($1,$2,$3,'semantic','active','Protected source title',
                 'confidential answer',80,0.8,'confidential') RETURNING id::text`,
		f.tenant, f.ns, f.owner)
	second := f.scalar(`INSERT INTO memory_items (
         tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,
         authority_level,confidence_score,sensitivity)
         VALUES ($1,$2,$3,'semantic','active','Protected source title',
                 'internal replacement',80,0.8,'internal') RETURNING id::text`,
		f.tenant, f.ns, f.owner)
	refresh := func(id string) int {
		status, _, _ := f.call(f.srv.handleStandingAnswerRefresh, http.MethodPost,
			"/v1/standing-answers/refresh", fmt.Sprintf(`{"fact_id":%q}`, id))
		return status
	}
	if status := refresh(first); status != http.StatusOK {
		t.Fatalf("initial refresh HTTP %d", status)
	}
	if _, err := f.admin.ExecContext(context.Background(),
		"UPDATE namespaces SET sensitivity_ceiling='internal' WHERE id=$1", f.ns); err != nil {
		t.Fatal(err)
	}
	path := "/v1/standing-answers?namespace_id=" + f.ns +
		"&question_key=" + url.QueryEscape("Protected source title")
	if status, _, _ := f.call(f.srv.handleStandingAnswerRead, http.MethodGet, path, ""); status != http.StatusNotFound {
		t.Fatalf("restricted evidence still readable: HTTP %d", status)
	}
	if status := refresh(second); status != http.StatusConflict {
		t.Fatalf("invisible but unexpired source was replaced: HTTP %d", status)
	}
}
