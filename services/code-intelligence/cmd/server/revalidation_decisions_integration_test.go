package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

// A reviewer may record an assessment without silently mutating the memory
// fact or its immutable revalidation event. Namespace write authority is
// required; read-only and unrelated principals cannot record decisions.
func TestPGRevalidationGovernedDecision(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenant := testTenant(t, db)
	var reviewer, reader, outsider, blindPromoter, namespace string
	for _, entry := range []struct {
		name string
		dst  *string
	}{
		{"reviewer", &reviewer}, {"reader", &reader}, {"outsider", &outsider}, {"blind-promoter", &blindPromoter},
	} {
		if err := db.QueryRowContext(ctx, "INSERT INTO principals(tenant_id,kind,external_id,display_name) VALUES($1,'human',$2,$2) RETURNING id", tenant, t.Name()+entry.name).Scan(entry.dst); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.QueryRowContext(ctx, "INSERT INTO namespaces(tenant_id,owner_principal_id,canonical_name) VALUES($1,$2,$3) RETURNING id", tenant, reviewer, t.Name()).Scan(&namespace); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO namespace_memberships(namespace_id,principal_id,can_read,can_promote) VALUES ($1,$2,true,true),($1,$3,true,false),($1,$4,false,true)", namespace, reviewer, reader, blindPromoter); err != nil {
		t.Fatal(err)
	}
	repo, err := store.EnsureRepository(ctx, tenant, "/p7-decision/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	old, err := store.CreateSnapshot(ctx, repo, tenant, "main", "before", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, old, repo, []PGFileInput{{Path: "a.go", Language: "go", ContentHash: hashContent("old")}}); err != nil {
		t.Fatal(err)
	}
	var version, fact string
	if err := db.QueryRowContext(ctx, "SELECT id FROM code_symbol_versions WHERE snapshot_id=$1 AND path='a.go' AND kind='file'", old).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, "INSERT INTO memory_items(tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,authority_level,confidence_score,sensitivity) VALUES($1,$2,$3,'semantic','active','fact','fact',50,0.9,'internal') RETURNING id", tenant, namespace, reviewer).Scan(&fact); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO memory_code_links(tenant_id,fact_id,code_symbol_version_id,link_type,source) VALUES($1,$2,$3,'evidences','explicit')", tenant, fact, version); err != nil {
		t.Fatal(err)
	}
	next, err := store.CreateSnapshot(ctx, repo, tenant, "main", "after", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, next, repo, []PGFileInput{{Path: "a.go", Language: "go", ContentHash: hashContent("changed")}}); err != nil {
		t.Fatal(err)
	}
	var event string
	if err := db.QueryRowContext(ctx, "SELECT id FROM memory_revalidation_events WHERE fact_id=$1 AND new_snapshot_id=$2", fact, next).Scan(&event); err != nil {
		t.Fatal(err)
	}

	srv := &server{pgStore: store}
	post := func(actor, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v2/code/revalidations", strings.NewReader(body))
		req = req.WithContext(auth.ContextWithClaims(req.Context(), auth.Claims{TenantID: tenant, PrincipalID: actor}))
		resp := httptest.NewRecorder()
		srv.handlePGRevalidations(resp, req)
		return resp
	}
	valid := `{"event_id":"` + event + `","outcome":"still_valid","reason":"Reviewed source and tests; semantic claim remains accurate."}`
	for label, actor := range map[string]string{"read-only": reader, "outsider": outsider, "blind promoter": blindPromoter} {
		if resp := post(actor, valid); resp.Code != http.StatusNotFound && resp.Code != http.StatusForbidden {
			t.Fatalf("%s decision status=%d body=%s", label, resp.Code, resp.Body.String())
		}
	}
	if resp := post(reviewer, `{"event_id":"`+event+`","outcome":"still_valid","reason":"   "}`); resp.Code != http.StatusBadRequest {
		t.Fatalf("blank rationale accepted: %d %s", resp.Code, resp.Body.String())
	}
	if resp := post(reviewer, `{"event_id":"`+event+`","outcome":"erase","reason":"invalid"}`); resp.Code != http.StatusBadRequest {
		t.Fatalf("unsupported outcome accepted: %d %s", resp.Code, resp.Body.String())
	}
	otherTenant := testTenant(t, db)
	if _, err := store.RecordRevalidationDecision(ctx, otherTenant, reviewer, event, "still_valid", "Cross-tenant claims must not authorize this."); !errors.Is(err, errReviewEventNotFound) {
		t.Fatalf("cross-tenant decision should be denied: %v", err)
	}
	for label, body := range map[string]string{
		"unknown-field": `{"event_id":"` + event + `","outcome":"still_valid","reason":"Reviewed evidence is sufficient.","bypass":true}`,
		"two-objects":   valid + valid,
	} {
		if resp := post(reviewer, body); resp.Code != http.StatusBadRequest {
			t.Fatalf("%s JSON accepted: %d %s", label, resp.Code, resp.Body.String())
		}
	}
	// Valid 2000-character non-BMP rationale, encoded using JSON surrogate
	// escapes (24 KiB), must not be rejected by the byte-size guard.
	unicodeValid := `{"event_id":"` + event + `","outcome":"still_valid","reason":"` +
		strings.Repeat(`\ud83d\ude80`, 2000) + `"}`
	if resp := post(reviewer, `{"event_id":"`+event+`","outcome":"still_valid","reason":"`+strings.Repeat("x", 33<<10)+`"}`); resp.Code != http.StatusBadRequest {
		t.Fatalf("oversized review body accepted: %d", resp.Code)
	}
	accepted := post(reviewer, unicodeValid)
	if accepted.Code != http.StatusCreated {
		t.Fatalf("review status=%d body=%s", accepted.Code, accepted.Body.String())
	}
	if resp := post(reviewer, valid); resp.Code != http.StatusConflict {
		t.Fatalf("duplicate decision status=%d body=%s", resp.Code, resp.Body.String())
	}
	var count int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM memory_revalidation_decisions WHERE event_id=$1 AND reviewer_id=$2 AND outcome='still_valid'", event, reviewer).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("decision count=%d", count)
	}
	var eventStatus, memoryStatus string
	if err := db.QueryRowContext(ctx, "SELECT review_status FROM memory_revalidation_events WHERE id=$1", event).Scan(&eventStatus); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, "SELECT status FROM memory_items WHERE id=$1", fact).Scan(&memoryStatus); err != nil {
		t.Fatal(err)
	}
	if eventStatus != "needs_review" || memoryStatus != "active" {
		t.Fatalf("authoritative state mutated: event=%s fact=%s", eventStatus, memoryStatus)
	}
	get := httptest.NewRequest(http.MethodGet, "/v2/code/revalidations?repository_id="+repo, nil)
	get = get.WithContext(auth.ContextWithClaims(get.Context(), auth.Claims{TenantID: tenant, PrincipalID: reviewer}))
	listed := httptest.NewRecorder()
	srv.handlePGRevalidations(listed, get)
	if listed.Code != http.StatusOK {
		t.Fatalf("list=%d %s", listed.Code, listed.Body.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(listed.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	// Exercise the actual restricted runtime role with forced RLS, not only
	// the superuser migration connection. CI requires this role to exist.
	appDSN := os.Getenv("SCOS_APP_TEST_DATABASE_URL")
	if appDSN == "" && os.Getenv("SCOS_REQUIRE_APP_RLS_TESTS") == "1" {
		t.Fatal("restricted role DSN required for P7 decision integration")
	}
	if appDSN != "" {
		app, err := sql.Open("pgx", appDSN)
		if err != nil {
			t.Fatal(err)
		}
		defer app.Close()
		restricted := newPGCodeStore(app)
		visible, err := restricted.ListRevalidationEvents(ctx, tenant, reviewer, repo, 10)
		if err != nil {
			t.Fatalf("restricted list: %v", err)
		}
		if len(visible) != 1 || visible[0]["review_outcome"] != "still_valid" {
			t.Fatalf("restricted review visibility: %+v", visible)
		}
		hidden, err := restricted.ListRevalidationEvents(ctx, tenant, outsider, repo, 10)
		if err != nil {
			t.Fatalf("restricted outsider list: %v", err)
		}
		if len(hidden) != 0 {
			t.Fatalf("restricted outsider leaked decisions: %+v", hidden)
		}
		if _, err := restricted.RecordRevalidationDecision(ctx, tenant, reader, event, "still_valid", "Read only user must be denied."); !errors.Is(err, errReviewEventNotFound) {
			t.Fatalf("restricted read-only decision should be denied: %v", err)
		}
		// A fresh activation creates an undecided event so we can prove the
		// runtime role can insert under RLS, not just read an owner-written row.
		third, err := store.CreateSnapshot(ctx, repo, tenant, "main", "third", "heuristic-v1", "")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.IndexSnapshot(ctx, third, repo, []PGFileInput{{Path: "a.go", Language: "go", ContentHash: hashContent("third")}}); err != nil {
			t.Fatal(err)
		}
		var pending string
		if err := db.QueryRowContext(ctx, "SELECT id FROM memory_revalidation_events WHERE fact_id=$1 AND new_snapshot_id=$2", fact, third).Scan(&pending); err != nil {
			t.Fatal(err)
		}
		if _, err := restricted.RecordRevalidationDecision(ctx, tenant, reviewer, pending, "unresolved", "Requires a deeper source review."); err != nil {
			t.Fatalf("restricted reviewer insert: %v", err)
		}
		// Even a privileged namespace reviewer cannot mutate the immutable
		// decision evidence through the restricted database role.
		tx, err := app.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, "SELECT set_config('scos.tenant_id',$1,true)", tenant); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, "SELECT set_config('scos.principal_id',$1,true)", reviewer); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, "UPDATE memory_revalidation_decisions SET reason='tampered evidence' WHERE event_id=$1", pending); err == nil {
			t.Fatal("restricted role unexpectedly updated immutable decision")
		}
	}
	if len(rows) != 1 || rows[0]["review_outcome"] != "still_valid" || rows[0]["reviewer_id"] != reviewer {
		t.Fatalf("decision not projected with evidence: %+v", rows)
	}
}
