//go:build integration

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/auth"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestDreamPipelineCountsEachStageUnderRLS seeds one tenant's turn, job,
// candidates (one of them a synthesis pattern), review and memory, then
// reads the Dream view as the runtime role: each stage and flow counts
// what was seeded, and another tenant's identical rows are not counted.
func TestDreamPipelineCountsEachStageUnderRLS(t *testing.T) {
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

	mustQuery := func(query string, dest *string, args ...any) {
		t.Helper()
		if err := admin.QueryRowContext(ctx, query, args...).Scan(dest); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	mustExec := func(query string, args ...any) {
		t.Helper()
		if _, err := admin.ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	seedTenant := func(name string) (tenant, owner, namespace string) {
		mustQuery(`INSERT INTO tenants (name) VALUES ($1) RETURNING id::text`, &tenant, name+"-"+t.Name())
		mustQuery(`INSERT INTO principals (tenant_id, kind, external_id, display_name) VALUES ($1, 'human', 'dream-'||gen_random_uuid(), 'Owner') RETURNING id::text`, &owner, tenant)
		mustQuery(`INSERT INTO namespaces (tenant_id, owner_principal_id, canonical_name, sensitivity_ceiling) VALUES ($1, $2, 'dream-ns', 'internal') RETURNING id::text`, &namespace, tenant, owner)
		mustExec(`INSERT INTO namespace_memberships (namespace_id, principal_id, can_read, can_write_candidates, can_promote, can_administer) VALUES ($1, $2, true, true, true, true)`, namespace, owner)

		var event string
		mustQuery(`INSERT INTO events (id, tenant_id, aggregate_id, aggregate_sequence, source, event_type, subject, occurred_at, data_schema, actor_id, data)
		           VALUES (gen_random_uuid(), $1, gen_random_uuid(), 1, 'it', 'turn.completed', 'turn/it', now(), 'scos.turn.completed/1', $2, '{}') RETURNING id::text`, &event, tenant, owner)
		mustExec(`INSERT INTO outbox_jobs (tenant_id, operation_id, job_type, payload, status) VALUES ($1, gen_random_uuid()::text, 'turn.completed', '{}', 'succeeded')`, tenant)
		mustExec(`INSERT INTO outbox_jobs (tenant_id, operation_id, job_type, payload, status) VALUES ($1, gen_random_uuid()::text, 'turn.completed', '{}', 'pending')`, tenant)
		candidate := func(status, assessment string) string {
			var id string
			mustQuery(`INSERT INTO memory_candidates (tenant_id, namespace_id, source_event_id, proposed_type, title, content, authority_level, confidence_score, sensitivity, status, conflict_assessment)
			           VALUES ($1, $2, $3, 'semantic', 't', 'c', 70, 0.8, 'internal', $4, $5::jsonb) RETURNING id::text`, &id, tenant, namespace, event, status, assessment)
			return id
		}
		candidate("promoted", `{}`)
		candidate("merged", `{}`)
		candidate("pending_review", `{"inferred": true}`)
		reviewed := candidate("promoted", `{}`)
		mustExec(`INSERT INTO memory_reviews (id, candidate_id, reviewer_id, decision, rationale) VALUES (gen_random_uuid(), $1, $2, 'approved', 'ok')`, reviewed, owner)

		var item, version string
		mustQuery(`INSERT INTO memory_items (tenant_id, namespace_id, owner_principal_id, type, status, title, canonical_summary, authority_level, confidence_score, sensitivity)
		           VALUES ($1, $2, $3, 'semantic', 'active', 't', 's', 70, 0.8, 'internal') RETURNING id::text`, &item, tenant, namespace, owner)
		mustQuery(`INSERT INTO memory_versions (memory_id, version_number, content, content_hash, change_reason, created_by)
		           VALUES ($1, 1, 'c', encode(sha256('c'::bytea), 'hex'), 'it', $2) RETURNING id::text`, &version, item, owner)
		mustExec(`UPDATE memory_items SET current_version_id = $1 WHERE id = $2`, version, item)
		mustExec(`INSERT INTO memory_synthesis_state (namespace_id, last_run_at, last_pattern_count) VALUES ($1, now(), 1)`, namespace)
		return tenant, owner, namespace
	}
	tenant, owner, _ := seedTenant("dream")
	seedTenant("dream-other")

	srv := &engineServer{db: app}
	r := httptest.NewRequest(http.MethodGet, "/v1/dashboard/dream?hours=6", nil)
	r = r.WithContext(auth.ContextWithClaims(r.Context(), auth.Claims{TenantID: tenant, PrincipalID: owner}))
	w := httptest.NewRecorder()
	srv.handleDashboardDream(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("dream = %d %s", w.Code, w.Body.String())
	}
	var view dreamView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	stage := map[string]int64{}
	for _, s := range view.Stages {
		stage[s.ID] = s.Count
	}
	want := map[string]int64{"turns": 1, "jobs": 1, "candidates": 3, "merged": 1, "review": 2, "promoted": 3, "memories": 1, "embedded": 0, "patterns": 1}
	for id, n := range want {
		if stage[id] != n {
			t.Errorf("stage %s = %d, want %d (all: %v)", id, stage[id], n, stage)
		}
	}
	if view.Backlog["jobs_waiting"] != 1 || view.Backlog["awaiting_review"] != 1 || view.Backlog["awaiting_embedding"] != 1 || view.Backlog["active_memories"] != 1 {
		t.Errorf("backlog = %v", view.Backlog)
	}
	if len(view.History) < 6 || len(view.History) > 8 {
		t.Errorf("history has %d hourly buckets for a 6-hour window", len(view.History))
	}
	var turns int64
	for _, b := range view.History {
		turns += b.Turns
	}
	if turns != 1 {
		t.Errorf("history counts %d turns, want 1", turns)
	}
	if len(view.Synthesis) != 1 || view.Synthesis[0].LastPatternCount != 1 || view.Synthesis[0].LastRunAt == nil {
		t.Errorf("synthesis = %+v, want the caller's one namespace", view.Synthesis)
	}

	r2 := httptest.NewRequest(http.MethodGet, "/v1/dashboard/dream?hours=500", nil)
	r2 = r2.WithContext(r.Context())
	bad := httptest.NewRecorder()
	srv.handleDashboardDream(bad, r2)
	if bad.Code != http.StatusBadRequest {
		t.Errorf("hours=500 = %d, want 400", bad.Code)
	}
}
