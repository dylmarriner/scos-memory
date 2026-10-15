package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

func TestDetectLanguage(t *testing.T) {
	tests := []struct {
		path     string
		expected string
	}{
		{"main.go", "go"},
		{"app.py", "python"},
		{"component.tsx", "typescript"},
		{"util.js", "javascript"},
		{"main.rs", "rust"},
		{"schema.sql", "sql"},
		{"config.yaml", "yaml"},
		{"config.json", "configuration"},
		{"readme.md", "unknown"},
	}
	for _, tc := range tests {
		lang := detectLanguage(tc.path)
		if lang != tc.expected {
			t.Errorf("detectLanguage(%q) = %q, want %q", tc.path, lang, tc.expected)
		}
	}
}

// Two repositories declaring the same name in the same file must never get
// the same symbol ID.
func TestTreeSitterSymbolIDsAreRepoScoped(t *testing.T) {
	src := "def handler():\n    return 1\n"
	symbolsA, err := extractTreeSitterSymbols("/src/app.py", "python", src, "repo-a")
	if err != nil {
		t.Fatal(err)
	}
	symbolsB, err := extractTreeSitterSymbols("/src/app.py", "python", src, "repo-b")
	if err != nil {
		t.Fatal(err)
	}
	if len(symbolsA) == 0 || len(symbolsA) != len(symbolsB) {
		t.Fatalf("expected identical non-empty symbol sets, got %d vs %d", len(symbolsA), len(symbolsB))
	}
	for i := range symbolsA {
		if symbolsA[i].ID == symbolsB[i].ID {
			t.Fatalf("symbol ID collision across repos for identical source: %s", symbolsA[i].ID)
		}
		if symbolsA[i].RepoID != "repo-a" || symbolsB[i].RepoID != "repo-b" {
			t.Fatalf("expected RepoID to be the owning repository, got %q and %q", symbolsA[i].RepoID, symbolsB[i].RepoID)
		}
	}
}

func postRepo(t *testing.T, srv *server, tenant, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/repos", bytes.NewBufferString(body))
	r.Header.Set("X-SCOS-Tenant-ID", tenant)
	w := httptest.NewRecorder()
	srv.handleRepos(w, r)
	return w
}

func TestHandleReposCreateListAndGet(t *testing.T) {
	srv := &server{store: newRepoStore()}

	created := postRepo(t, srv, "tenant-1", `{"canonical_path":"/test/repo","branch_or_worktree":"main","tenant_id":"someone-else"}`)
	if created.Code != http.StatusOK {
		t.Fatalf("create: expected 200, got %d: %s", created.Code, created.Body.String())
	}
	var repo Repository
	if err := json.Unmarshal(created.Body.Bytes(), &repo); err != nil {
		t.Fatal(err)
	}
	if repo.ID != "repo-1" || repo.Branch != "main" || repo.CreatedAt == "" {
		t.Fatalf("unexpected created repository %+v", repo)
	}
	if repo.TenantID != "tenant-1" {
		t.Fatalf("owning tenant must come from the caller, not the body: got %q", repo.TenantID)
	}

	list := httptest.NewRequest(http.MethodGet, "/v1/repos", nil)
	list.Header.Set("X-SCOS-Tenant-ID", "tenant-1")
	listResp := httptest.NewRecorder()
	srv.handleRepos(listResp, list)
	var repos []*Repository
	if err := json.Unmarshal(listResp.Body.Bytes(), &repos); err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 || repos[0].ID != "repo-1" {
		t.Fatalf("expected the created repository in the list, got %+v", repos)
	}

	get := httptest.NewRequest(http.MethodGet, "/v1/repos/repo-1", nil)
	get.Header.Set("X-SCOS-Tenant-ID", "tenant-1")
	getResp := httptest.NewRecorder()
	srv.handleRepoByID(getResp, get)
	if getResp.Code != http.StatusOK {
		t.Fatalf("get: expected 200, got %d", getResp.Code)
	}
	var fetched Repository
	if err := json.Unmarshal(getResp.Body.Bytes(), &fetched); err != nil {
		t.Fatal(err)
	}
	if fetched.CanonicalPath != "/test/repo" {
		t.Fatalf("unexpected fetched repository %+v", fetched)
	}

	if missing := postRepo(t, srv, "tenant-1", `{"branch_or_worktree":"main"}`); missing.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without canonical_path, got %d", missing.Code)
	}
}

// TestRequireTenantPrefersClaimsOverSpoofedHeader guards against a
// regression of the cross-tenant IDOR the authorization auditor confirmed:
// requireTenant() previously trusted X-SCOS-Tenant-ID unconditionally, so a
// caller authenticated with valid claims for one tenant could still read
// another tenant's data simply by setting the header to the victim's ID.
// With verified claims present, the header must be ignored entirely.
func TestRequireTenantPrefersClaimsOverSpoofedHeader(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/v1/repos", nil)
	r.Header.Set("X-SCOS-Tenant-ID", "victim-tenant")
	ctx := auth.ContextWithClaims(r.Context(), auth.Claims{TenantID: "attacker-tenant", PrincipalID: "attacker"})
	r = r.WithContext(ctx)

	tenant, ok := requireTenant(r)
	if !ok {
		t.Fatalf("expected requireTenant to succeed with valid claims")
	}
	if tenant != "attacker-tenant" {
		t.Fatalf("expected claims tenant %q to win over spoofed header, got %q", "attacker-tenant", tenant)
	}
}

// TestHandleReposIgnoresSpoofedHeaderWhenClaimsPresent exercises the same
// regression end-to-end: an attacker authenticated as tenant-a sets
// X-SCOS-Tenant-ID to tenant-b (the victim) and must still only see
// tenant-a's (empty) repository list, never tenant-b's.
func TestHandleReposIgnoresSpoofedHeaderWhenClaimsPresent(t *testing.T) {
	store := newRepoStore()
	store.repos["repo-b"] = &Repository{ID: "repo-b", TenantID: "tenant-b", CanonicalPath: "/repo"}
	srv := &server{store: store}

	r := httptest.NewRequest(http.MethodGet, "/v1/repos", nil)
	r.Header.Set("X-SCOS-Tenant-ID", "tenant-b") // spoofed victim header
	r = r.WithContext(auth.ContextWithClaims(r.Context(), auth.Claims{TenantID: "tenant-a", PrincipalID: "attacker"}))
	w := httptest.NewRecorder()
	srv.handleRepos(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var repos []*Repository
	if err := json.Unmarshal(w.Body.Bytes(), &repos); err != nil {
		t.Fatal(err)
	}
	if len(repos) != 0 {
		t.Fatalf("expected no cross-tenant repositories via spoofed header, got %+v", repos)
	}
}

func TestHandleReposRequiresTenant(t *testing.T) {
	srv := &server{store: newRepoStore()}
	r := httptest.NewRequest(http.MethodGet, "/v1/repos", nil)
	w := httptest.NewRecorder()
	srv.handleRepos(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without a tenant header, got %d", w.Code)
	}
	byID := httptest.NewRequest(http.MethodGet, "/v1/repos/repo-1", nil)
	byIDResp := httptest.NewRecorder()
	srv.handleRepoByID(byIDResp, byID)
	if byIDResp.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for repository lookup without a tenant header, got %d", byIDResp.Code)
	}
}

func TestHandleReposScopedToTenant(t *testing.T) {
	store := newRepoStore()
	store.repos["repo-a"] = &Repository{ID: "repo-a", TenantID: "tenant-a", CanonicalPath: "/a"}
	store.repos["repo-b"] = &Repository{ID: "repo-b", TenantID: "tenant-b", CanonicalPath: "/b"}
	srv := &server{store: store}

	r := httptest.NewRequest(http.MethodGet, "/v1/repos", nil)
	r.Header.Set("X-SCOS-Tenant-ID", "tenant-a")
	w := httptest.NewRecorder()
	srv.handleRepos(w, r)
	var repos []*Repository
	if err := json.Unmarshal(w.Body.Bytes(), &repos); err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 || repos[0].ID != "repo-a" {
		t.Fatalf("expected only tenant-a's repository, got %+v", repos)
	}

	// tenant-b must not be able to fetch tenant-a's repository by ID.
	byID := httptest.NewRequest(http.MethodGet, "/v1/repos/repo-a", nil)
	byID.Header.Set("X-SCOS-Tenant-ID", "tenant-b")
	byIDResp := httptest.NewRecorder()
	srv.handleRepoByID(byIDResp, byID)
	if byIDResp.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for cross-tenant repository lookup, got %d", byIDResp.Code)
	}
}

func TestRepoStoreRestoresRepositoriesAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "code-intelligence.json")
	srv := &server{store: newRepoStoreAt(path)}
	if w := postRepo(t, srv, "tenant-1", `{"canonical_path":"/repo","branch_or_worktree":"main"}`); w.Code != http.StatusOK {
		t.Fatalf("create repository: expected 200, got %d", w.Code)
	}

	restarted := newRepoStoreAt(path)
	if err := restarted.load(); err != nil {
		t.Fatalf("load after restart: %v", err)
	}
	repo := restarted.repos["repo-1"]
	if repo == nil || repo.TenantID != "tenant-1" || repo.CanonicalPath != "/repo" {
		t.Fatalf("expected repo-1 restored after restart, got %+v", repo)
	}

	// IDs continue from the restored registry instead of reusing repo-1.
	again := &server{store: restarted, nextID: nextRepositoryID(restarted.repos)}
	w := postRepo(t, again, "tenant-1", `{"canonical_path":"/other"}`)
	var second Repository
	if err := json.Unmarshal(w.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if second.ID != "repo-2" {
		t.Fatalf("expected repo-2 after restart, got %q", second.ID)
	}
}

// A store file written while the in-memory symbol index still existed
// carries symbols, chunks, generations and relations alongside the
// repositories. It must still load, and only the repositories survive.
func TestRepoStoreLoadsFileWrittenWithSymbolIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "code-intelligence.json")
	legacy := `{"version":1,"repositories":{"repo-3":{"repository_id":"repo-3","tenant_id":"tenant-1","canonical_path":"/repo","index_generation":2}},` +
		`"symbols":{"abc":{"symbol_id":"abc"}},"chunks":{},"generations":{"repo-3":{"generation":2,"status":"active"}},"relations":{},` +
		`"reconciliations":[{"repository_id":"repo-3","commit_id":"c1"}]}`
	if err := os.WriteFile(path, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	store := newRepoStoreAt(path)
	if err := store.load(); err != nil {
		t.Fatalf("load legacy store: %v", err)
	}
	if repo := store.repos["repo-3"]; repo == nil || repo.TenantID != "tenant-1" {
		t.Fatalf("expected repo-3 restored from legacy store, got %+v", store.repos)
	}
	if got := nextRepositoryID(store.repos); got != 3 {
		t.Fatalf("expected next repository number 3, got %d", got)
	}
}

func TestRepoStoreRejectsUnknownVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "code-intelligence.json")
	if err := os.WriteFile(path, []byte(`{"version":2,"repositories":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := newRepoStoreAt(path).load(); err == nil {
		t.Fatal("expected an unsupported store version to fail loading")
	}
}

func TestCreateRepoDoesNotPublishWhenAtomicStoreCommitFails(t *testing.T) {
	dir := t.TempDir()
	blockedParent := filepath.Join(dir, "blocked")
	if err := os.WriteFile(blockedParent, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	store := newRepoStoreAt(filepath.Join(blockedParent, "store.json"))
	srv := &server{store: store}

	response := postRepo(t, srv, "tenant-1", `{"canonical_path":"/repo"}`)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("expected failed commit to return 500, got %d", response.Code)
	}
	if len(store.repos) != 0 {
		t.Fatal("failed atomic commit published the repository")
	}
	if srv.nextID != 0 {
		t.Fatalf("failed atomic commit consumed a repository ID: nextID=%d", srv.nextID)
	}
}
