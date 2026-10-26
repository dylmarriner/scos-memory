package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"

	"github.com/dylmarriner/scos-memory/packages/auth"
	"testing"
)

func TestPGSnapshotEventsAPIAuthIsolationAndPagination(t *testing.T) {
	store, db := testPGStore(t)
	srv := &server{pgStore: store}
	ctx := context.Background()
	owner := testTenant(t, db)
	other := testTenant(t, db)
	repo, err := store.EnsureRepository(ctx, owner, "/events-api/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	inputs := []PGFileInput{
		{Path: "a.go", Language: "go", Content: "package a\nfunc A(){}\n"},
		{Path: "b.go", Language: "go", Content: "package a\nfunc B(){}\n"},
		{Path: "c.go", Language: "go", Content: "package a\nfunc C(){}\n"},
	}
	for i := range inputs {
		inputs[i].ContentHash = hashContent(inputs[i].Content)
	}
	snap, err := store.CreateSnapshot(ctx, repo, owner, "main", "events-api-1", "go/parser-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, snap, repo, inputs); err != nil {
		t.Fatal(err)
	}
	path := "/v2/code/snapshots/events"
	request := func(tenant string, body any) *snapshotEventsPage {
		t.Helper()
		resp := pgRequest(t, srv, http.MethodPost, path, tenant, body)
		if resp.Code != http.StatusOK {
			t.Fatalf("events HTTP %d: %s", resp.Code, resp.Body.String())
		}
		var page snapshotEventsPage
		if err := json.Unmarshal(resp.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		return &page
	}
	first := request(owner, map[string]any{"snapshot_id": snap, "limit": 2})
	if len(first.Events) != 2 || first.Events[0].Path != "a.go" || first.Events[1].Path != "b.go" || first.NextAfterPath != "b.go" {
		t.Fatalf("unexpected first page: %+v", first)
	}
	second := request(owner, map[string]any{"snapshot_id": snap, "limit": 2, "after_path": first.NextAfterPath})
	if len(second.Events) != 1 || second.Events[0].Path != "c.go" || second.NextAfterPath != "" {
		t.Fatalf("unexpected second page: %+v", second)
	}
	for _, bad := range []struct {
		tenant string
		body   any
		status int
	}{
		{other, map[string]any{"snapshot_id": snap}, http.StatusNotFound},
		{owner, map[string]any{"snapshot_id": "not-a-uuid"}, http.StatusBadRequest},
		{owner, map[string]any{"snapshot_id": snap, "limit": 1001}, http.StatusBadRequest},
		{owner, map[string]any{"snapshot_id": snap, "unknown": true}, http.StatusBadRequest},
	} {
		resp := pgRequest(t, srv, http.MethodPost, path, bad.tenant, bad.body)
		if resp.Code != bad.status {
			t.Errorf("expected %d got %d: %s", bad.status, resp.Code, resp.Body.String())
		}
	}
	// Requests without verified identity or trusted gateway headers are denied.
	unauth := httptest.NewRequest(http.MethodPost, path,
		strings.NewReader(`{"snapshot_id":"`+snap+`"}`))
	unauthResult := httptest.NewRecorder()
	srv.handlePGSnapshotEvents(unauthResult, unauth)
	if unauthResult.Code != http.StatusUnauthorized {
		t.Fatalf("forged headers returned %d", unauthResult.Code)
	}
	// Tenant-only claims without a verified principal must also fail.
	noPrincipal := httptest.NewRequest(http.MethodPost, path,
		strings.NewReader(`{"snapshot_id":"`+snap+`"}`))
	noPrincipal = noPrincipal.WithContext(auth.ContextWithClaims(noPrincipal.Context(),
		auth.Claims{TenantID: owner}))
	noPrincipalResult := httptest.NewRecorder()
	srv.handlePGSnapshotEvents(noPrincipalResult, noPrincipal)
	if noPrincipalResult.Code != http.StatusUnauthorized {
		t.Fatalf("missing principal returned %d", noPrincipalResult.Code)
	}
	// A superseded snapshot remains readable, while the target ledger
	// reports deletion and modification without resurrecting old paths.
	changed := []PGFileInput{
		inputs[0],
		{Path: "b.go", Language: "go", Content: "package a\nfunc B(){println(1)}\n"},
	}
	changed[1].ContentHash = hashContent(changed[1].Content)
	target, err := store.CreateSnapshot(ctx, repo, owner, "main", "events-api-2", "go/parser-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, target, repo, changed); err != nil {
		t.Fatal(err)
	}
	changes := request(owner, map[string]any{"snapshot_id": target})
	if len(changes.Events) != 2 ||
		changes.Events[0].Path != "b.go" || changes.Events[0].Kind != "modify" ||
		changes.Events[1].Path != "c.go" || changes.Events[1].Kind != "delete" {
		t.Fatalf("incorrect target change ledger: %+v", changes)
	}
	if superseded := request(owner, map[string]any{"snapshot_id": snap}); len(superseded.Events) != 3 {
		t.Fatalf("historical snapshot ledger disappeared: %+v", superseded)
	}
	resp := pgRequest(t, srv, http.MethodGet, path, owner, nil)
	if resp.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET returned %d", resp.Code)
	}
	incomplete, err := store.CreateSnapshot(ctx, repo, owner, "main", "events-api-unindexed", "go/parser-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	resp = pgRequest(t, srv, http.MethodPost, path, owner, map[string]any{"snapshot_id": incomplete})
	if resp.Code != http.StatusNotFound {
		t.Fatalf("incomplete snapshot returned %d: %s", resp.Code, resp.Body.String())
	}
}

// The runtime uses a non-owner, NOBYPASSRLS role. The event table forces
// tenant RLS even when SQL includes explicit tenant predicates.
func TestPGSnapshotEventsAPIReadsWithRestrictedRuntimeRole(t *testing.T) {
	dsn := os.Getenv("SCOS_APP_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SCOS_APP_TEST_DATABASE_URL not configured")
	}
	store, ownerDB := testPGStore(t)
	tenant := testTenant(t, ownerDB)
	ctx := context.Background()
	repo, err := store.EnsureRepository(ctx, tenant, "/events-api-rls/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	snap, err := store.CreateSnapshot(ctx, repo, tenant, "main", "rls-api-1", "go/parser-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	source := "package demo\nfunc Visible(){}\n"
	if err := store.IndexSnapshot(ctx, snap, repo, []PGFileInput{
		{Path: "visible.go", Language: "go", Content: source, ContentHash: hashContent(source)},
	}); err != nil {
		t.Fatal(err)
	}
	appDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer appDB.Close()
	if err := appDB.PingContext(ctx); err != nil {
		t.Fatal(err)
	}
	// Without transaction-local scos.tenant_id, forced RLS hides this row.
	var invisible int
	if err := appDB.QueryRowContext(ctx,
		"SELECT count(*) FROM code_snapshot_change_events WHERE snapshot_id=$1", snap).Scan(&invisible); err != nil {
		t.Fatal(err)
	}
	if invisible != 0 {
		t.Fatalf("unscoped restricted role saw %d rows", invisible)
	}
	srv := &server{pgStore: newPGCodeStore(appDB)}
	resp := pgRequest(t, srv, http.MethodPost, "/v2/code/snapshots/events", tenant, map[string]any{"snapshot_id": snap})
	if resp.Code != http.StatusOK {
		t.Fatalf("restricted HTTP %d: %s", resp.Code, resp.Body.String())
	}
	var page snapshotEventsPage
	if err := json.Unmarshal(resp.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || page.Events[0].Path != "visible.go" || page.Events[0].Kind != "add" {
		t.Fatalf("restricted role failed to retrieve tenant evidence: %+v", page)
	}
}
