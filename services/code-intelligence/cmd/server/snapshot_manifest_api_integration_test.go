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
)

func TestPGSnapshotManifestReadbackTenantIsolationAndPagination(t *testing.T) {
	store, db := testPGStore(t)
	srv := &server{pgStore: store}
	ctx := context.Background()
	owner, foreign := testTenant(t, db), testTenant(t, db)
	repo, err := store.EnsureRepository(ctx, owner, "/manifest-readback/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	inputs := []PGFileInput{
		{Path: "a.go", Language: "go", Content: "package demo\nfunc A(){}\n"},
		{Path: "b.go", Language: "go", Content: "package demo\nfunc B(){}\n"},
		{Path: "c.go", Language: "go", Content: "package demo\nfunc C(){}\n"},
	}
	for i := range inputs {
		inputs[i].ContentHash = hashContent(inputs[i].Content)
	}
	first, err := store.CreateSnapshot(ctx, repo, owner, "main", "git-commit-parent", "go/parser-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, first, repo, inputs); err != nil {
		t.Fatal(err)
	}
	path := "/v2/code/snapshots/manifest"
	request := func(tenant string, body any) snapshotManifestPage {
		t.Helper()
		resp := pgRequest(t, srv, http.MethodPost, path, tenant, body)
		if resp.Code != http.StatusOK {
			t.Fatalf("manifest HTTP %d: %s", resp.Code, resp.Body.String())
		}
		var page snapshotManifestPage
		if err := json.Unmarshal(resp.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		return page
	}
	page := request(owner, map[string]any{"snapshot_id": first, "limit": 2})
	if page.SnapshotID != first || page.RepositoryID != repo || page.Branch != "main" ||
		page.CommitID != "git-commit-parent" || page.ParentCommitID != "" {
		t.Fatalf("incorrect provenance: %+v", page)
	}
	if len(page.Files) != 2 || page.Files[0].Path != "a.go" || page.Files[1].Path != "b.go" ||
		page.Files[0].ContentHash != inputs[0].ContentHash || page.NextAfterPath != "b.go" {
		t.Fatalf("incorrect first manifest page: %+v", page)
	}
	last := request(owner, map[string]any{"snapshot_id": first, "limit": 2, "after_path": page.NextAfterPath})
	if len(last.Files) != 1 || last.Files[0].Path != "c.go" || last.NextAfterPath != "" ||
		last.CommitID != page.CommitID || last.RepositoryID != page.RepositoryID {
		t.Fatalf("incorrect final page: %+v", last)
	}
	for _, bad := range []struct {
		tenant string
		body   any
		code   int
	}{
		{foreign, map[string]any{"snapshot_id": first}, 404},
		{owner, map[string]any{"snapshot_id": "bad"}, 400},
		{owner, map[string]any{"snapshot_id": first, "limit": 1001}, 400},
		{owner, map[string]any{"snapshot_id": first, "unknown": true}, 400},
		{owner, map[string]any{"snapshot_id": first, "after_path": "a\u0000"}, 400},
	} {
		resp := pgRequest(t, srv, http.MethodPost, path, bad.tenant, bad.body)
		if resp.Code != bad.code {
			t.Errorf("want %d got %d: %s", bad.code, resp.Code, resp.Body.String())
		}
	}
	unauth := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"snapshot_id":"`+first+`"}`))
	recorder := httptest.NewRecorder()
	srv.handlePGSnapshotManifest(recorder, unauth)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated request returned %d", recorder.Code)
	}
	incomplete, err := store.CreateSnapshot(ctx, repo, owner, "main", "unindexed", "go/parser-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if resp := pgRequest(t, srv, http.MethodPost, path, owner, map[string]any{"snapshot_id": incomplete}); resp.Code != http.StatusNotFound {
		t.Fatalf("incomplete snapshot was exposed: %d", resp.Code)
	}
	target, err := store.CreateSnapshot(ctx, repo, owner, "main", "git-commit-target", "go/parser-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, target, repo, inputs[:2]); err != nil {
		t.Fatal(err)
	}
	history := request(owner, map[string]any{"snapshot_id": first})
	current := request(owner, map[string]any{"snapshot_id": target})
	if len(history.Files) != 3 || len(current.Files) != 2 ||
		current.ParentCommitID != "git-commit-parent" {
		t.Fatalf("historical readback or deleted-file isolation broken: history=%+v current=%+v", history, current)
	}
}

// A real non-owner role must be able to read the scoped manifest without
// bypassing the tenant check. No table owner or superuser connection is used
// for the HTTP request.
func TestPGSnapshotManifestRestrictedRole(t *testing.T) {
	dsn := os.Getenv("SCOS_APP_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SCOS_APP_TEST_DATABASE_URL not configured")
	}
	store, ownerDB := testPGStore(t)
	ctx := context.Background()
	tenant, other := testTenant(t, ownerDB), testTenant(t, ownerDB)
	repo, err := store.EnsureRepository(ctx, tenant, "/manifest-restricted/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	snap, err := store.CreateSnapshot(ctx, repo, tenant, "main", "restricted-test", "go/parser-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	content := "package demo\nfunc A(){}\n"
	if err := store.IndexSnapshot(ctx, snap, repo, []PGFileInput{{Path: "a.go", Language: "go", Content: content, ContentHash: hashContent(content)}}); err != nil {
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
	srv := &server{pgStore: newPGCodeStore(appDB)}
	resp := pgRequest(t, srv, http.MethodPost, "/v2/code/snapshots/manifest", tenant, map[string]any{"snapshot_id": snap})
	if resp.Code != http.StatusOK {
		t.Fatalf("restricted role HTTP %d: %s", resp.Code, resp.Body.String())
	}
	var page snapshotManifestPage
	if err := json.Unmarshal(resp.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Files) != 1 || page.Files[0].ContentHash != hashContent(content) {
		t.Fatalf("restricted role did not read stored hash: %+v", page)
	}
	foreign := pgRequest(t, srv, http.MethodPost, "/v2/code/snapshots/manifest", other, map[string]any{"snapshot_id": snap})
	if foreign.Code != http.StatusNotFound {
		t.Fatalf("restricted role cross-tenant access returned %d", foreign.Code)
	}
}
