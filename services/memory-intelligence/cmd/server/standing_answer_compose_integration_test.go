//go:build integration

package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestP7ComposeEvidenceDraftRequiresEveryCurrentAuthorizedFact(t *testing.T) {
	f := newBehaviourFixture(t)
	makeFact := func(summary string, confidence float64) string {
		return f.scalar(`INSERT INTO memory_items (
            tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,
            authority_level,confidence_score,sensitivity)
            VALUES ($1,$2,$3,'semantic','active','Evidence', $4,80,$5,'internal')
            RETURNING id::text`, f.tenant, f.ns, f.owner, summary, confidence)
	}
	first := makeFact("First verified statement.", 0.93)
	second := makeFact("Second verified statement.", 0.71)
	request := func(ids ...string) string {
		return fmt.Sprintf(`{"namespace_id":%q,"fact_ids":[%q,%q]}`, f.ns, ids[0], ids[1])
	}
	path := "/v1/standing-answers/compose"
	code, result, _ := f.call(f.srv.handleStandingAnswerCompose, http.MethodPost, path, request(first, second))
	if code != http.StatusOK || result["status"] != "draft_requires_review" ||
		result["confidence"] != 0.71 || !strings.Contains(result["draft_text"].(string), "First verified statement.") ||
		!strings.Contains(result["draft_text"].(string), "Second verified statement.") {
		t.Fatalf("composition HTTP %d %v", code, result)
	}
	evidence, ok := result["evidence"].([]any)
	if !ok || len(evidence) != 2 {
		t.Fatalf("missing exact source provenance: %v", result)
	}
	code, reverse, _ := f.call(f.srv.handleStandingAnswerCompose, http.MethodPost, path, request(second, first))
	if code != http.StatusOK || reverse["draft_text"] != result["draft_text"] {
		t.Fatalf("composition changed with caller ordering: %d %v", code, reverse)
	}
	var cacheCount int
	if err := f.admin.QueryRowContext(context.Background(), "SELECT count(*) FROM memory_standing_answers WHERE tenant_id=$1", f.tenant).Scan(&cacheCount); err != nil || cacheCount != 0 {
		t.Fatalf("draft persisted without governance: count=%d err=%v", cacheCount, err)
	}
	code, _, _ = f.callAs(f.otherTenant, f.other, f.srv.handleStandingAnswerCompose, http.MethodPost, path, request(first, second))
	if code != http.StatusNotFound {
		t.Fatalf("foreign tenant saw evidence: %d", code)
	}
	if _, err := f.admin.ExecContext(context.Background(), "UPDATE memory_items SET status='revoked' WHERE id=$1", second); err != nil {
		t.Fatal(err)
	}
	code, _, _ = f.call(f.srv.handleStandingAnswerCompose, http.MethodPost, path, request(first, second))
	if code != http.StatusNotFound {
		t.Fatalf("partial draft returned after revocation: %d", code)
	}
}

func TestP7ComposeRejectsExpiredAndRevalidationPendingEvidence(t *testing.T) {
	f := newBehaviourFixture(t)
	makeFact := func(title string) string {
		return f.scalar(`INSERT INTO memory_items (
            tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,
            authority_level,confidence_score,sensitivity)
            VALUES ($1,$2,$3,'semantic','active',$4,$4,80,0.85,'internal')
            RETURNING id::text`, f.tenant, f.ns, f.owner, title)
	}
	a, b := makeFact("A"), makeFact("B")
	body := fmt.Sprintf(`{"namespace_id":%q,"fact_ids":[%q,%q]}`, f.ns, a, b)
	call := func() int {
		status, _, _ := f.call(f.srv.handleStandingAnswerCompose, http.MethodPost, "/v1/standing-answers/compose", body)
		return status
	}
	if got := call(); got != http.StatusOK {
		t.Fatalf("initial draft HTTP %d", got)
	}
	if _, err := f.admin.ExecContext(context.Background(), "UPDATE memory_items SET expires_at=statement_timestamp()-interval '1 second' WHERE id=$1", b); err != nil {
		t.Fatal(err)
	}
	if got := call(); got != http.StatusNotFound {
		t.Fatalf("expired evidence HTTP %d", got)
	}
	if _, err := f.admin.ExecContext(context.Background(), "UPDATE memory_items SET expires_at=NULL WHERE id=$1", b); err != nil {
		t.Fatal(err)
	}
	if got := call(); got != http.StatusOK {
		t.Fatalf("recovered evidence HTTP %d", got)
	}
	if _, err := f.admin.ExecContext(context.Background(), "UPDATE namespaces SET sensitivity_ceiling='public' WHERE id=$1", f.ns); err != nil {
		t.Fatal(err)
	}
	if got := call(); got != http.StatusNotFound {
		t.Fatalf("restricted sensitivity ceiling HTTP %d", got)
	}
	if _, err := f.admin.ExecContext(context.Background(), "UPDATE namespaces SET sensitivity_ceiling='internal' WHERE id=$1", f.ns); err != nil {
		t.Fatal(err)
	}
	if got := call(); got != http.StatusOK {
		t.Fatalf("restored sensitivity ceiling HTTP %d", got)
	}
	if _, err := f.admin.ExecContext(context.Background(), "UPDATE namespace_memberships SET can_read=false WHERE namespace_id=$1 AND principal_id=$2", f.ns, f.owner); err != nil {
		t.Fatal(err)
	}
	if got := call(); got != http.StatusNotFound {
		t.Fatalf("revoked membership HTTP %d", got)
	}
}

func TestP7ComposeDoesNotIncludeUnreviewedCodeLinkedEvidence(t *testing.T) {
	f := newBehaviourFixture(t)
	makeFact := func(title string) string {
		return f.scalar(`INSERT INTO memory_items(tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,authority_level,confidence_score,sensitivity)
            VALUES($1,$2,$3,'semantic','active',$4,$4,80,0.88,'internal') RETURNING id::text`, f.tenant, f.ns, f.owner, title)
	}
	a, b := makeFact("Approved evidence"), makeFact("Code-linked evidence")
	body := fmt.Sprintf(`{"namespace_id":%q,"fact_ids":[%q,%q]}`, f.ns, a, b)
	call := func() int {
		status, _, _ := f.call(f.srv.handleStandingAnswerCompose, http.MethodPost, "/v1/standing-answers/compose", body)
		return status
	}
	if got := call(); got != http.StatusOK {
		t.Fatalf("initial HTTP %d", got)
	}
	repo := f.scalar("INSERT INTO code_repositories(tenant_id,canonical_path) VALUES($1,$2) RETURNING id::text", f.tenant, "/p7-compose/"+b)
	old := f.scalar("INSERT INTO code_repository_snapshots(tenant_id,repository_id,branch,commit_id,status) VALUES($1,$2,'main','before','active') RETURNING id::text", f.tenant, repo)
	next := f.scalar("INSERT INTO code_repository_snapshots(tenant_id,repository_id,branch,commit_id,status) VALUES($1,$2,'main','after','ready') RETURNING id::text", f.tenant, repo)
	symbol := f.scalar("INSERT INTO code_symbols(repository_id,stable_key,kind,first_snapshot_id) VALUES($1,$2,'file',$3) RETURNING id::text", repo, "compose-"+b, old)
	version := f.scalar("INSERT INTO code_symbol_versions(symbol_id,snapshot_id,fully_qualified_name,path,kind,content_hash) VALUES($1,$2,'compose.evidence','src/compose.go','file','abc') RETURNING id::text", symbol, old)
	link := f.scalar("INSERT INTO memory_code_links(tenant_id,fact_id,code_symbol_version_id,link_type,source) VALUES($1,$2,$3,'evidences','explicit') RETURNING id::text", f.tenant, b, version)
	event := f.scalar(`INSERT INTO memory_revalidation_events(tenant_id,fact_id,link_id,old_snapshot_id,new_snapshot_id,old_symbol_version_id,continuity,review_status)
        VALUES($1,$2,$3,$4,$5,$6,'changed','needs_review') RETURNING id::text`, f.tenant, b, link, old, next, version)
	if got := call(); got != http.StatusNotFound {
		t.Fatalf("unreviewed code-linked evidence HTTP %d", got)
	}
	if _, err := f.admin.ExecContext(context.Background(),
		"INSERT INTO memory_revalidation_decisions(event_id,tenant_id,reviewer_id,outcome,reason) VALUES($1,$2,$3,'still_valid','Reviewer verified source continuity')",
		event, f.tenant, f.owner); err != nil {
		t.Fatal(err)
	}
	if got := call(); got != http.StatusOK {
		t.Fatalf("reviewed code-linked evidence HTTP %d", got)
	}
}
