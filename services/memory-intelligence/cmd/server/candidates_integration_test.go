//go:build integration

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/auth"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestReviewQueueLifecycleThroughRestrictedRole seeds held-back candidates as
// the owner role, then lists, approves (including a duplicate) and rejects
// them through the handlers running as the runtime role, under RLS.
func TestReviewQueueLifecycleThroughRestrictedRole(t *testing.T) {
	adminURL, appURL := os.Getenv("DATABASE_URL"), os.Getenv("SCOS_APP_TEST_DATABASE_URL")
	if adminURL == "" || appURL == "" {
		t.Fatal("DATABASE_URL (owner) and SCOS_APP_TEST_DATABASE_URL (runtime role) must be set")
	}
	ctx := context.Background()
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	app, err := sql.Open("pgx", appURL)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()

	var tenant, owner, outsider, namespace string
	mustQuery := func(query string, dest *string, args ...any) {
		t.Helper()
		if err := admin.QueryRowContext(ctx, query, args...).Scan(dest); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	mustQuery(`INSERT INTO tenants (name) VALUES ('review-it') RETURNING id::text`, &tenant)
	mustQuery(`INSERT INTO principals (tenant_id, kind, external_id, display_name) VALUES ($1, 'human', 'review-owner-'||gen_random_uuid(), 'Owner') RETURNING id::text`, &owner, tenant)
	mustQuery(`INSERT INTO principals (tenant_id, kind, external_id, display_name) VALUES ($1, 'human', 'review-outsider-'||gen_random_uuid(), 'Outsider') RETURNING id::text`, &outsider, tenant)
	mustQuery(`INSERT INTO namespaces (tenant_id, owner_principal_id, canonical_name, sensitivity_ceiling) VALUES ($1, $2, 'review-it', 'internal') RETURNING id::text`, &namespace, tenant, owner)
	if _, err := admin.ExecContext(ctx, `INSERT INTO namespace_memberships (namespace_id, principal_id, can_read, can_write_candidates, can_promote, can_administer) VALUES ($1, $2, true, true, true, true)`, namespace, owner); err != nil {
		t.Fatal(err)
	}
	seed := func(content string) string {
		t.Helper()
		var event, candidate string
		mustQuery(`INSERT INTO events (id, tenant_id, aggregate_id, aggregate_sequence, source, event_type, subject, occurred_at, data_schema, actor_id, data)
		           VALUES (gen_random_uuid(), $1, gen_random_uuid(), 1, 'it', 'turn.completed', 'turn/it', now(), 'scos.turn.completed/1', $2, '{}') RETURNING id::text`, &event, tenant, owner)
		mustQuery(`INSERT INTO memory_candidates (tenant_id, namespace_id, source_event_id, proposed_type, title, content, authority_level, confidence_score, sensitivity, status)
		           VALUES ($1, $2, $3, 'semantic', $4, $4, 70, 0.75, 'internal', 'pending_review') RETURNING id::text`, &candidate, tenant, namespace, event, content)
		return candidate
	}
	first := seed("The production deploy window is Tuesday 14:00 UTC.")
	duplicate := seed("The production deploy window is Tuesday 14:00 UTC.")
	unwanted := seed("Production credentials rotate whenever someone feels like it.")

	srv := &engineServer{db: app}
	call := func(principal, method, path, body string) (int, map[string]any) {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r = r.WithContext(auth.ContextWithClaims(r.Context(), auth.Claims{TenantID: tenant, PrincipalID: principal}))
		w := httptest.NewRecorder()
		if strings.HasPrefix(path, "/v1/candidates/") {
			srv.handleCandidateDecision(w, r)
		} else {
			srv.handleCandidates(w, r)
		}
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}

	code, listed := call(owner, http.MethodGet, "/v1/candidates", "")
	if code != http.StatusOK || len(listed["candidates"].([]any)) != 3 {
		t.Fatalf("list = %d %v", code, listed)
	}
	if code, out := call(outsider, http.MethodGet, "/v1/candidates", ""); code != http.StatusOK || len(out["candidates"].([]any)) != 0 {
		t.Fatalf("a non-member must see no candidates, got %d %v", code, out)
	}
	if code, _ := call(outsider, http.MethodPost, "/v1/candidates/"+first+"/review", `{"decision":"approve"}`); code != http.StatusNotFound {
		t.Fatalf("a non-member must not find the candidate, got %d", code)
	}

	code, approved := call(owner, http.MethodPost, "/v1/candidates/"+first+"/review", `{"decision":"approve","rationale":"confirmed"}`)
	if code != http.StatusOK || approved["decision"] != "approved" {
		t.Fatalf("approve = %d %v", code, approved)
	}
	factID := approved["fact"].(map[string]any)["id"].(string)
	var active int
	if err := admin.QueryRowContext(ctx, `SELECT count(*) FROM memory_items WHERE tenant_id = $1 AND status = 'active'`, tenant).Scan(&active); err != nil || active != 1 {
		t.Fatalf("approved memory not active: %d %v", active, err)
	}

	code, merged := call(owner, http.MethodPost, "/v1/candidates/"+duplicate+"/review", `{"decision":"approve"}`)
	if code != http.StatusOK || merged["merged"] != true || merged["canonical_fact_id"] != factID {
		t.Fatalf("approving a duplicate must merge into %s, got %d %v", factID, code, merged)
	}
	if err := admin.QueryRowContext(ctx, `SELECT count(*) FROM memory_items WHERE tenant_id = $1 AND status = 'active'`, tenant).Scan(&active); err != nil || active != 1 {
		t.Fatalf("a duplicate approval created a second memory: %d %v", active, err)
	}

	if code, _ := call(owner, http.MethodPost, "/v1/candidates/"+unwanted+"/review", `{"decision":"reject"}`); code != http.StatusBadRequest {
		t.Fatalf("reject without rationale must be refused, got %d", code)
	}
	if code, out := call(owner, http.MethodPost, "/v1/candidates/"+unwanted+"/review", `{"decision":"reject","rationale":"not a policy"}`); code != http.StatusOK || out["decision"] != "rejected" {
		t.Fatalf("reject = %d %v", code, out)
	}
	if code, _ := call(owner, http.MethodPost, "/v1/candidates/"+unwanted+"/review", `{"decision":"approve"}`); code != http.StatusConflict {
		t.Fatalf("a decided candidate must not be decided again, got %d", code)
	}

	var statuses, reviews string
	if err := admin.QueryRowContext(ctx, `SELECT string_agg(status, ',' ORDER BY status) FROM memory_candidates WHERE tenant_id = $1`, tenant).Scan(&statuses); err != nil || statuses != "merged,promoted,rejected" {
		t.Fatalf("candidate statuses = %q %v", statuses, err)
	}
	if err := admin.QueryRowContext(ctx, `SELECT string_agg(r.decision || ':' || r.reviewer_id::text, ',' ORDER BY r.decision) FROM memory_reviews r JOIN memory_candidates c ON c.id = r.candidate_id WHERE c.tenant_id = $1`, tenant).Scan(&reviews); err != nil ||
		reviews != "approved:"+owner+",approved:"+owner+",rejected:"+owner {
		t.Fatalf("reviews = %q %v", reviews, err)
	}
	if code, out := call(owner, http.MethodGet, "/v1/candidates", ""); code != http.StatusOK || len(out["candidates"].([]any)) != 0 {
		t.Fatalf("queue must be empty after decisions, got %v", out)
	}
}
