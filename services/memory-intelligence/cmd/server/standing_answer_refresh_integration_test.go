//go:build integration

package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// The refresh path must derive the answer from a currently readable fact,
// never from caller-supplied prose, and leave dependency invalidation intact.
func TestP7DeterministicStandingAnswerRefreshUsesGovernedFactEvidence(t *testing.T) {
	f := newBehaviourFixture(t)
	fact := f.scalar(`INSERT INTO memory_items (
        tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,
        authority_level,confidence_score,sensitivity)
        VALUES ($1,$2,$3,'semantic','active','  Where is the archive?  ',
                'In the audited store.',80,0.91,'internal') RETURNING id::text`,
		f.tenant, f.ns, f.owner)
	path := "/v1/standing-answers/refresh"
	payload := fmt.Sprintf(`{"fact_id":%q}`, fact)
	code, result, _ := f.call(f.srv.handleStandingAnswerRefresh, http.MethodPost, path, payload)
	if code != http.StatusOK || result["answer"] != "In the audited store." ||
		result["question_key"] != "where is the archive?" {
		t.Fatalf("refresh status=%d response=%v", code, result)
	}
	code, uppercase, _ := f.call(f.srv.handleStandingAnswerRefresh, http.MethodPost, path,
		fmt.Sprintf(`{"fact_id":%q}`, strings.ToUpper(fact)))
	if code != http.StatusOK || uppercase["id"] != result["id"] || uppercase["source_fact_id"] != fact {
		t.Fatalf("uppercase spelling of the same UUID changed source identity: %d %v", code, uppercase)
	}
	if result["source_fact_id"] != fact {
		t.Fatalf("refresh did not identify its exact source: %v", result)
	}
	readPath := "/v1/standing-answers?namespace_id=" + f.ns + "&question_key=" + url.QueryEscape("WHERE IS THE ARCHIVE?")
	code, answer, _ := f.call(f.srv.handleStandingAnswerRead, http.MethodGet, readPath, "")
	if code != http.StatusOK || answer["answer"] != "In the audited store." {
		t.Fatalf("refreshed answer is not available: %d %v", code, answer)
	}
	evidence, ok := answer["evidence_fact_ids"].([]any)
	if !ok || len(evidence) != 1 || evidence[0] != fact {
		t.Fatalf("refreshed answer has no verified evidence: %v", answer)
	}
	// The same refresh is idempotent and cannot multiply dependencies.
	code, again, _ := f.call(f.srv.handleStandingAnswerRefresh, http.MethodPost, path, payload)
	if code != http.StatusOK || again["id"] != result["id"] {
		t.Fatalf("refresh was not idempotent: %d %v", code, again)
	}
	var dependencies int
	if err := f.admin.QueryRowContext(context.Background(),
		"SELECT count(*) FROM memory_standing_answer_dependencies WHERE standing_answer_id=$1", result["id"]).Scan(&dependencies); err != nil || dependencies != 1 {
		t.Fatalf("dependency count=%d err=%v", dependencies, err)
	}
	if _, err := f.admin.ExecContext(context.Background(), "UPDATE memory_items SET status='revoked' WHERE id=$1", fact); err != nil {
		t.Fatal(err)
	}
	code, _, _ = f.call(f.srv.handleStandingAnswerRead, http.MethodGet, readPath, "")
	if code != http.StatusNotFound {
		t.Fatalf("revoked evidence still served: %d", code)
	}
	code, _, _ = f.call(f.srv.handleStandingAnswerRefresh, http.MethodPost, path, payload)
	if code != http.StatusNotFound {
		t.Fatalf("revoked evidence could refresh: %d", code)
	}
}

func TestP7StandingAnswerRefreshCannotForgeOrCrossTenantWrite(t *testing.T) {
	f := newBehaviourFixture(t)
	fact := f.scalar(`INSERT INTO memory_items (
        tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,
        authority_level,confidence_score,sensitivity)
        VALUES ($1,$2,$3,'semantic','active','Archive location',
                'The verified location',80,0.8,'internal') RETURNING id::text`,
		f.tenant, f.ns, f.owner)
	path := "/v1/standing-answers/refresh"
	payload := fmt.Sprintf(`{"fact_id":%q}`, fact)
	code, _, _ := f.callAs(f.otherTenant, f.other, f.srv.handleStandingAnswerRefresh, http.MethodPost, path, payload)
	if code != http.StatusNotFound {
		t.Fatalf("foreign tenant can identify or refresh fact: %d", code)
	}
	for _, invalid := range []string{
		fmt.Sprintf(`{"fact_id":%q,"answer":"forged"}`, fact),
		`{"fact_id":"invalid"}`, `{"fact_id":null}`, `{}`,
		fmt.Sprintf(`{"fact_id":%q} {}`, fact),
	} {
		code, _, _ = f.call(f.srv.handleStandingAnswerRefresh, http.MethodPost, path, invalid)
		if code != http.StatusBadRequest {
			t.Fatalf("accepted malformed or forged refresh %q: %d", invalid, code)
		}
	}
	if _, err := f.admin.ExecContext(context.Background(),
		"UPDATE namespace_memberships SET can_write_candidates=false WHERE namespace_id=$1 AND principal_id=$2", f.ns, f.owner); err != nil {
		t.Fatal(err)
	}
	code, _, _ = f.call(f.srv.handleStandingAnswerRefresh, http.MethodPost, path, payload)
	if code != http.StatusForbidden {
		t.Fatalf("read-only principal can refresh answer: %d", code)
	}
}

func TestP7StandingAnswerInvalidatesOnSourceTextChangeWithoutVersionBump(t *testing.T) {
	f := newBehaviourFixture(t)
	fact := f.scalar(`INSERT INTO memory_items (
        tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,
        authority_level,confidence_score,sensitivity)
        VALUES ($1,$2,$3,'semantic','active','Where is the report?',
                'Old verified location',80,0.8,'internal') RETURNING id::text`,
		f.tenant, f.ns, f.owner)
	code, _, _ := f.call(f.srv.handleStandingAnswerRefresh, http.MethodPost,
		"/v1/standing-answers/refresh", fmt.Sprintf(`{"fact_id":%q}`, fact))
	if code != http.StatusOK {
		t.Fatalf("initial refresh status %d", code)
	}
	readPath := "/v1/standing-answers?namespace_id=" + f.ns + "&question_key=where%20is%20the%20report%3F"
	code, _, _ = f.call(f.srv.handleStandingAnswerRead, http.MethodGet, readPath, "")
	if code != http.StatusOK {
		t.Fatalf("initial read status %d", code)
	}
	// Direct SQL writers can change the summary without changing the
	// current_version_id. A cache must still be invalidated atomically.
	if _, err := f.admin.ExecContext(context.Background(),
		"UPDATE memory_items SET canonical_summary='New verified location' WHERE id=$1", fact); err != nil {
		t.Fatal(err)
	}
	code, _, _ = f.call(f.srv.handleStandingAnswerRead, http.MethodGet, readPath, "")
	if code != http.StatusNotFound {
		t.Fatalf("old answer still served after source summary change: HTTP %d", code)
	}
	code, _, _ = f.call(f.srv.handleStandingAnswerRefresh, http.MethodPost,
		"/v1/standing-answers/refresh", fmt.Sprintf(`{"fact_id":%q}`, fact))
	if code != http.StatusOK {
		t.Fatalf("refresh after summary change status %d", code)
	}
	if _, err := f.admin.ExecContext(context.Background(),
		"UPDATE memory_items SET title='Where is the revised report?' WHERE id=$1", fact); err != nil {
		t.Fatal(err)
	}
	code, _, _ = f.call(f.srv.handleStandingAnswerRead, http.MethodGet, readPath, "")
	if code != http.StatusNotFound {
		t.Fatalf("old title answer still served after source title change: HTTP %d", code)
	}
}

func TestP7StandingAnswerRefreshCannotOverwriteCuratedOrDifferentSource(t *testing.T) {
	f := newBehaviourFixture(t)
	first := f.scalar(`INSERT INTO memory_items (
        tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,
        authority_level,confidence_score,sensitivity)
        VALUES ($1,$2,$3,'semantic','active','Shared title',
                'first source',80,0.8,'internal') RETURNING id::text`,
		f.tenant, f.ns, f.owner)
	second := f.scalar(`INSERT INTO memory_items (
        tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,
        authority_level,confidence_score,sensitivity)
        VALUES ($1,$2,$3,'semantic','active','Shared title',
                'second source',80,0.8,'internal') RETURNING id::text`,
		f.tenant, f.ns, f.owner)
	path := "/v1/standing-answers/refresh"
	code, _, _ := f.call(f.srv.handleStandingAnswerRefresh, http.MethodPost, path,
		fmt.Sprintf(`{"fact_id":%q}`, first))
	if code != http.StatusOK {
		t.Fatalf("first source refresh %d", code)
	}
	code, _, _ = f.call(f.srv.handleStandingAnswerRefresh, http.MethodPost, path,
		fmt.Sprintf(`{"fact_id":%q}`, second))
	if code != http.StatusConflict {
		t.Fatalf("different fact overwrote answer: %d", code)
	}
	readPath := "/v1/standing-answers?namespace_id=" + f.ns + "&question_key=shared%20title"
	code, answer, _ := f.call(f.srv.handleStandingAnswerRead, http.MethodGet, readPath, "")
	if code != http.StatusOK || answer["answer"] != "first source" {
		t.Fatalf("different source changed standing answer: %d %v", code, answer)
	}
	if _, err := f.admin.ExecContext(context.Background(),
		"UPDATE memory_standing_answers SET refresh_reason='curated' WHERE tenant_id=$1 AND question_key='shared title'", f.tenant); err != nil {
		t.Fatal(err)
	}
	code, _, _ = f.call(f.srv.handleStandingAnswerRefresh, http.MethodPost, path,
		fmt.Sprintf(`{"fact_id":%q}`, first))
	if code != http.StatusConflict {
		t.Fatalf("curated answer overwritten: %d", code)
	}
}
