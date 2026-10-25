package main

import (
	"encoding/json"
	"net/http"
	"testing"
)

// Historical lookups must use the requested immutable snapshot, never the
// branch's currently active one. A caller may not borrow another tenant's
// repository or resolve an ambiguous commit into an arbitrary extractor.
func TestPGHistoricalCodeQueryUsesImmutableCommit(t *testing.T) {
	store, db := testPGStore(t)
	tenant := testTenant(t, db)
	otherTenant := testTenant(t, db)
	ctx := t.Context()
	repo, err := store.EnsureRepository(ctx, tenant, "/historical/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	old, err := store.CreateSnapshot(ctx, repo, tenant, "main", "commit-old", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, old, repo, []PGFileInput{{Path: "pkg/legacy.txt", Language: "text", ContentHash: hashContent("old")}}); err != nil {
		t.Fatal(err)
	}
	current, err := store.CreateSnapshot(ctx, repo, tenant, "main", "commit-new", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, current, repo, []PGFileInput{{Path: "pkg/current.txt", Language: "text", ContentHash: hashContent("new")}}); err != nil {
		t.Fatal(err)
	}
	srv := &server{pgStore: store}
	check := func(body map[string]any, who string, code int) map[string]any {
		t.Helper()
		resp := pgRequest(t, srv, http.MethodPost, "/v2/code/query", who, body)
		if resp.Code != code {
			t.Fatalf("query %v: status %d want %d body %s", body, resp.Code, code, resp.Body.String())
		}
		var result map[string]any
		if err := json.Unmarshal(resp.Body.Bytes(), &result); err == nil {
			return result
		}
		return nil
	}
	check(map[string]any{"repository_id": repo, "query": "pkg/legacy.txt", "as_of_commit": "commit-old"}, tenant, 400)
	historical := check(map[string]any{"repository_id": repo, "branch": "main", "query": "pkg/legacy.txt", "as_of_commit": "commit-old"}, tenant, 200)
	if historical["snapshot_id"] != old || historical["commit_id"] != "commit-old" {
		t.Fatalf("historical source not preserved: %+v", historical)
	}
	rows, ok := historical["results"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("expected 1 old symbol, got %+v", historical)
	}
	check(map[string]any{"repository_id": repo, "branch": "main", "query": "pkg/current.txt", "as_of_commit": "commit-old"}, tenant, 200)
	denied := check(map[string]any{"repository_id": repo, "branch": "main", "query": "pkg/legacy.txt", "as_of_commit": "commit-old"}, otherTenant, 200)
	if got, ok := denied["results"].([]any); !ok || len(got) != 0 {
		t.Fatalf("cross-tenant historical query exposed data: %+v", denied)
	}
	bySnapshot := check(map[string]any{"repository_id": repo, "branch": "main", "query": "pkg/legacy.txt", "snapshot_id": old}, tenant, 200)
	if bySnapshot["snapshot_id"] != old {
		t.Fatalf("explicit historical snapshot wrong: %+v", bySnapshot)
	}
	check(map[string]any{"repository_id": repo, "branch": "main", "query": "pkg/legacy.txt", "as_of_commit": "unknown"}, tenant, 404)
	check(map[string]any{"repository_id": repo, "branch": "main", "query": "pkg/legacy.txt", "snapshot_id": "not-a-uuid"}, tenant, 400)
	check(map[string]any{"repository_id": repo, "branch": "main", "query": "pkg/legacy.txt", "snapshot_id": old, "as_of_commit": "commit-new"}, tenant, 404)
	// Two immutable snapshots can describe the same commit under distinct
	// extraction configurations; require an explicit ID rather than guessing.
	duplicate, err := store.CreateSnapshot(ctx, repo, tenant, "main", "commit-old", "heuristic-v1", "another-config")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, duplicate, repo, []PGFileInput{{Path: "pkg/legacy.txt", Language: "text", ContentHash: hashContent("old")}}); err != nil {
		t.Fatal(err)
	}
	check(map[string]any{"repository_id": repo, "branch": "main", "query": "pkg/legacy.txt", "as_of_commit": "commit-old"}, tenant, 409)
	check(map[string]any{"repository_id": repo, "branch": "main", "query": "pkg/legacy.txt", "snapshot_id": old, "as_of_commit": "commit-old"}, tenant, 200)
}

func TestPGHistoricalCodeLexicalUsesEnglishStemming(t *testing.T) {
	store, db := testPGStore(t)
	tenant := testTenant(t, db)
	ctx := t.Context()
	repo, err := store.EnsureRepository(ctx, tenant, "/historical-lexical/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.CreateSnapshot(ctx, repo, tenant, "main", "old-doc", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	source := "package gateway\n\n// resolveIdentity authenticates a caller from its bearer token.\nfunc resolveIdentity(token string) string { return token }\n"
	if err := store.IndexSnapshot(ctx, before, repo, []PGFileInput{{Path: "gateway/identity.go", Language: "go", ContentHash: hashContent(source), Content: source}}); err != nil {
		t.Fatal(err)
	}
	after, err := store.CreateSnapshot(ctx, repo, tenant, "main", "current-doc", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, after, repo, []PGFileInput{{Path: "gateway/page.go", Language: "go", ContentHash: hashContent("package gateway\nfunc renderPage() {}\n"), Content: "package gateway\nfunc renderPage() {}\n"}}); err != nil {
		t.Fatal(err)
	}
	resp := pgRequest(t, &server{pgStore: store}, http.MethodPost, "/v2/code/query", tenant, map[string]any{
		"repository_id": repo, "branch": "main", "query": "bearer tokens", "as_of_commit": "old-doc",
	})
	if resp.Code != 200 {
		t.Fatalf("historical lexical status %d: %s", resp.Code, resp.Body.String())
	}
	var result struct {
		Results       []PGSymbolVersion `json:"results"`
		RetrievalMode string            `json:"retrieval_mode"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.RetrievalMode != "historical_lexical" {
		t.Fatalf("mode: %s", result.RetrievalMode)
	}
	found := false
	for _, symbol := range result.Results {
		if symbol.FullyQualifiedName == "gateway.resolveIdentity" {
			found = true
		}
	}
	if !found {
		t.Fatalf("historical lexical stemming failed: %+v", result.Results)
	}
}
