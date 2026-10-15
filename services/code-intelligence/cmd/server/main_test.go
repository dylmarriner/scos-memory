package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

func TestNewCodeStore(t *testing.T) {
	store := newCodeStore()
	if store == nil {
		t.Fatal("expected non-nil store")
	}
	if len(store.repos) != 0 {
		t.Fatal("expected empty repos")
	}
}

func TestRegisterDefaultAdapters(t *testing.T) {
	store := newCodeStore()
	registerDefaultAdapters(store)
	if len(store.adapters) != 8 {
		t.Fatalf("expected 8 adapters, got %d", len(store.adapters))
	}
	if store.adapters["go"].Tier != 4 {
		t.Fatalf("expected Go tier 4, got %d", store.adapters["go"].Tier)
	}
}

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

func TestExtractSymbolsTier0(t *testing.T) {
	symbols, err := extractSymbols("/path/to/file.txt", "unknown", 0, "repo-1", nil)
	if err != nil || len(symbols) != 1 {
		t.Fatalf("expected 1 file symbol, got %d (%v)", len(symbols), err)
	}
	if symbols[0].Kind != "file" || symbols[0].ContentHash != "" {
		t.Fatalf("path-only file must not claim a content hash: %+v", symbols[0])
	}
}

func TestExtractSymbolsPathOnlyClaimsNothing(t *testing.T) {
	symbols, err := extractSymbols("/src/cmd/server/main.go", "go", 1, "repo-1", nil)
	if err != nil || len(symbols) != 1 || symbols[0].Kind != "file" {
		t.Fatalf("path-only Go file must yield only its file record, got %+v (%v)", symbols, err)
	}
}

func TestExtractGoSymbols(t *testing.T) {
	src := "package server\n\n// Handle serves a request.\nfunc Handle() { helper() }\n\nfunc helper() {}\n\ntype Config struct{ Addr string }\n"
	symbols, err := extractSymbols("/src/cmd/server/main.go", "go", 1, "repo-1", &src)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Symbol{}
	for _, s := range symbols {
		byName[s.FullyQualifiedName] = s
	}
	if byName["server"].Kind != "package" {
		t.Fatalf("expected parsed package symbol, got %+v", symbols)
	}
	handle := byName["server.Handle"]
	if handle.Kind != "function" || handle.DocComment != "Handle serves a request." || handle.Range != "4-4" || handle.Visibility != "public" || handle.ContentHash == "" {
		t.Fatalf("unexpected Handle symbol %+v", handle)
	}
	if byName["server.helper"].Visibility != "private" || byName["server.Config"].Kind != "type" {
		t.Fatalf("unexpected declarations %+v", symbols)
	}
	if symbols[0].ContentHash != sha256Hex([]byte(src)) {
		t.Fatal("file content hash must hash the file content")
	}
	changed := src + "\nvar X = 1\n"
	again, _ := extractSymbols("/src/cmd/server/main.go", "go", 1, "repo-1", &changed)
	for _, s := range again {
		if s.FullyQualifiedName == "server.Handle" && s.ContentHash != handle.ContentHash {
			t.Fatal("unchanged declaration must keep its span hash when another declaration changes")
		}
	}
	bad := "package server\nfunc {"
	if _, err := extractSymbols("/src/bad.go", "go", 1, "repo-1", &bad); err == nil {
		t.Fatal("unparseable Go must be an error")
	}
}

func TestExtractPythonSymbols(t *testing.T) {
	src := "class Model:\n    def save(self):\n        return persist(self)\n\ndef persist(x):\n    return x\n"
	symbols, err := extractSymbols("/src/models.py", "python", 1, "repo-1", &src)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	for _, s := range symbols {
		kinds[s.FullyQualifiedName] = s.Kind
	}
	if kinds["models.Model"] != "class" || kinds["models.Model.save"] != "method" || kinds["models.persist"] != "function" {
		t.Fatalf("unexpected python symbols %v", kinds)
	}
}

func TestExtractTSJSSymbols(t *testing.T) {
	src := "export function render(): string { return format(1) }\nfunction format(n: number) { return String(n) }\n"
	symbols, err := extractSymbols("/src/app.ts", "typescript", 1, "repo-1", &src)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range symbols {
		if s.FullyQualifiedName == "app.render" && s.Kind == "function" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected parsed TypeScript function, got %+v", symbols)
	}
}

func TestExecuteExactLookup(t *testing.T) {
	store := newCodeStore()
	store.symbols["sym1"] = &Symbol{
		ID: "sym1", FullyQualifiedName: "myFunction", Kind: "function",
		Path: "/src/main.go", Language: "go",
	}
	store.chunks["sym1"] = &Chunk{SymbolID: "sym1", Content: "func myFunction()", Path: "/src/main.go"}

	srv := &server{store: store}
	q := CodeQuery{Intent: "exact_lookup", Query: "myFunction", MaxResults: 10}
	results := srv.executeQuery(q)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Plane != "exact" {
		t.Fatalf("expected exact plane, got %s", results[0].Plane)
	}
}

func TestExecuteSymbolLookup(t *testing.T) {
	store := newCodeStore()
	store.symbols["sym1"] = &Symbol{
		ID: "sym1", FullyQualifiedName: "com.example.MyClass", Kind: "class",
		Path: "/src/MyClass.java", Language: "java",
	}
	store.chunks["sym1"] = &Chunk{SymbolID: "sym1", Content: "class MyClass", Path: "/src/MyClass.java"}

	srv := &server{store: store}
	q := CodeQuery{Intent: "symbol_lookup", Query: "MyClass", MaxResults: 10}
	results := srv.executeQuery(q)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Plane != "symbol" {
		t.Fatalf("expected symbol plane, got %s", results[0].Plane)
	}
}

func TestExecuteImplementationSearch(t *testing.T) {
	store := newCodeStore()
	store.symbols["sym1"] = &Symbol{
		ID: "sym1", FullyQualifiedName: "MyClass", Kind: "class",
		Path: "/src/app.go", Signature: "type MyClass struct{}",
	}
	store.chunks["sym1"] = &Chunk{SymbolID: "sym1", Content: "type MyClass struct{}", Path: "/src/app.go"}

	srv := &server{store: store}
	q := CodeQuery{Intent: "implementation_search", Query: "MyClass", MaxResults: 10}
	results := srv.executeQuery(q)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Plane != "structural" {
		t.Fatalf("expected structural plane, got %s", results[0].Plane)
	}
}

func TestExecuteTestDiscovery(t *testing.T) {
	store := newCodeStore()
	store.symbols["sym1"] = &Symbol{
		ID: "sym1", FullyQualifiedName: "TestMyFunction", Kind: "function",
		Path: "/src/my_test.go", Language: "go",
	}
	store.chunks["sym1"] = &Chunk{SymbolID: "sym1", Content: "func TestMyFunction", Path: "/src/my_test.go"}

	srv := &server{store: store}
	q := CodeQuery{Intent: "test_discovery", Query: "MyFunction", MaxResults: 10}
	results := srv.executeQuery(q)
	if len(results) == 0 {
		t.Fatalf("expected at least 1 result")
	}
	if results[0].Plane != "test_relation" {
		t.Fatalf("expected test_relation plane, got %s", results[0].Plane)
	}
}

func TestExecuteArchitectureQuestion(t *testing.T) {
	store := newCodeStore()
	store.symbols["sym1"] = &Symbol{
		ID: "sym1", FullyQualifiedName: "com.app.PaymentService", Kind: "interface",
		Path: "/src/payment.go", Language: "go",
		DocComment: "PaymentService handles payments",
	}
	store.chunks["sym1"] = &Chunk{SymbolID: "sym1", Content: "type PaymentService interface{}", Path: "/src/payment.go"}

	srv := &server{store: store}
	q := CodeQuery{Intent: "architecture_question", Query: "PaymentService", MaxResults: 10}
	results := srv.executeQuery(q)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Plane != "relation" {
		t.Fatalf("expected relation plane, got %s", results[0].Plane)
	}
}

func TestExecuteConfigurationLookup(t *testing.T) {
	store := newCodeStore()
	store.symbols["sym1"] = &Symbol{
		ID: "sym1", FullyQualifiedName: "config.yaml", Kind: "configuration",
		Path: "/etc/config.yaml", Language: "yaml",
	}
	store.chunks["sym1"] = &Chunk{SymbolID: "sym1", Content: "database:\n  host: localhost", Path: "/etc/config.yaml"}

	srv := &server{store: store}
	q := CodeQuery{Intent: "configuration_lookup", Query: "config.yaml", MaxResults: 10}
	results := srv.executeQuery(q)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
}

func TestExecuteQueryEmptyResults(t *testing.T) {
	store := newCodeStore()
	srv := &server{store: store}
	q := CodeQuery{Intent: "exact_lookup", Query: "nonexistent", MaxResults: 10}
	results := srv.executeQuery(q)
	if len(results) != 0 {
		t.Fatalf("expected 0 results, got %d", len(results))
	}
}

func TestExecuteReferenceSearch(t *testing.T) {
	store := newCodeStore()
	store.symbols["sym1"] = &Symbol{
		ID: "sym1", FullyQualifiedName: "app.go", Kind: "file",
		Path: "/src/app.go", Imports: []string{"fmt", "net/http"},
	}
	store.chunks["sym1"] = &Chunk{SymbolID: "sym1", Content: "package main", Path: "/src/app.go"}

	srv := &server{store: store}
	q := CodeQuery{Intent: "reference_search", Query: "fmt", MaxResults: 10}
	results := srv.executeQuery(q)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
}

func TestPlaneReporting(t *testing.T) {
	if planeForIntent("exact_lookup") != "exact" || planeForIntent("bug_localisation") != "lexical" {
		t.Fatal("unexpected plane mapping")
	}
	used := planesUsed([]CodeQueryResult{{Plane: "symbol"}, {Plane: "symbol"}, {Plane: "exact"}})
	if len(used) != 2 || used[0] != "symbol" || used[1] != "exact" {
		t.Fatalf("planesUsed = %v", used)
	}
	if got := planesUsed(nil); len(got) != 0 {
		t.Fatalf("no results must report no planes used, got %v", got)
	}
}

func TestLexicalScoreOrdersMatches(t *testing.T) {
	q := "reduce"
	exact := lexicalScore("reduce", q)
	segment := lexicalScore("pkg.OutputReducer.reduce", q)
	prefix := lexicalScore("reducer_factory", q)
	inner := lexicalScore("outputreducer", q)
	if !(exact == 1 && exact > segment && segment > prefix && prefix > inner && inner > 0) {
		t.Fatalf("scores exact=%v segment=%v prefix=%v inner=%v", exact, segment, prefix, inner)
	}
	if lexicalScore("unrelated", q) != 0 || lexicalScore("", q) != 0 {
		t.Fatal("non-matches must score zero")
	}
}

func TestHandleReposCRUD(t *testing.T) {
	store := newCodeStore()

	// Create a repo
	repo := Repository{CanonicalPath: "/test/repo", Branch: "main"}
	// Simulate POST
	store.mu.Lock()
	repo.ID = "repo-1"
	repo.CreatedAt = "now"
	repo.UpdatedAt = "now"
	store.repos[repo.ID] = &repo
	store.mu.Unlock()

	if len(store.repos) != 1 {
		t.Fatalf("expected 1 repo, got %d", len(store.repos))
	}
	if store.repos["repo-1"].Branch != "main" {
		t.Fatal("branch mismatch")
	}
}

func TestIndexFiles(t *testing.T) {
	store := newCodeStore()
	store.repos["repo-1"] = &Repository{ID: "repo-1", CanonicalPath: "/test/repo"}

	// Simulate index
	store.mu.Lock()
	gen := &IndexGeneration{Generation: 1, RepoID: "repo-1", Status: "active"}
	for _, file := range []string{"/test/repo/main.go", "/test/repo/app.py"} {
		lang := detectLanguage(file)
		symbols, err := extractSymbols(file, lang, 1, "repo-1", nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, sym := range symbols {
			store.symbols[sym.ID] = &sym
			store.chunks[sym.ID] = &Chunk{SymbolID: sym.ID}
		}
	}
	gen.SymbolCount = len(store.symbols)
	gen.FileCount = 2
	store.generations["repo-1"] = gen
	store.repos["repo-1"].IndexGeneration = 1
	store.mu.Unlock()

	if store.repos["repo-1"].IndexGeneration != 1 {
		t.Fatal("generation mismatch")
	}
	if len(store.symbols) < 2 {
		t.Fatalf("expected at least 2 symbols, got %d", len(store.symbols))
	}
}

func TestExtractSymbolsRepoScoped(t *testing.T) {
	// Two repositories indexing the identical relative path must not collide
	// in the shared global symbol/chunk maps, and a scoped query must never
	// return the other repository's symbols.
	src := "package main\n\nfunc main() {}\n"
	symbolsA, _ := extractSymbols("/src/main.go", "go", 1, "repo-a", &src)
	symbolsB, _ := extractSymbols("/src/main.go", "go", 1, "repo-b", &src)
	if len(symbolsA) != len(symbolsB) {
		t.Fatalf("expected identical symbol counts, got %d vs %d", len(symbolsA), len(symbolsB))
	}
	for i := range symbolsA {
		if symbolsA[i].ID == symbolsB[i].ID {
			t.Fatalf("symbol ID collision across repos for identical path: %s", symbolsA[i].ID)
		}
		if symbolsA[i].RepoID != "repo-a" || symbolsB[i].RepoID != "repo-b" {
			t.Fatalf("expected RepoID to be set to the owning repository, got %q and %q", symbolsA[i].RepoID, symbolsB[i].RepoID)
		}
	}

	srv := &server{store: newCodeStore()}
	for _, sym := range symbolsA {
		sym.CommitID = "commit-a"
		srv.store.symbols[sym.ID] = &sym
	}
	for _, sym := range symbolsB {
		sym.CommitID = "commit-b"
		srv.store.symbols[sym.ID] = &sym
	}
	for _, sym := range srv.store.symbols {
		srv.store.chunks[sym.ID] = &Chunk{SymbolID: sym.ID, Path: sym.Path}
	}

	results := srv.executeQuery(CodeQuery{Intent: "exact_lookup", Query: "main.go", RepoID: "repo-a", MaxResults: 10})
	if len(results) != len(symbolsA) {
		t.Fatalf("expected %d scoped results for repo-a, got %d", len(symbolsA), len(results))
	}
	for _, r := range results {
		if r.Symbol.CommitID != "commit-a" {
			t.Fatalf("cross-repo leakage: repo-a query returned symbol from commit %q", r.Symbol.CommitID)
		}
	}
}

func TestHandleQueryPreventsCrossTenantLeakage(t *testing.T) {
	// Two tenants each register a repository with an overlapping path. A
	// query with no repository_id from tenant A must never surface tenant
	// B's symbols (§19.10).
	store := newCodeStore()
	store.repos["repo-a"] = &Repository{ID: "repo-a", TenantID: "tenant-a", CanonicalPath: "/repo"}
	store.repos["repo-b"] = &Repository{ID: "repo-b", TenantID: "tenant-b", CanonicalPath: "/repo"}
	store.symbols["sym-a"] = &Symbol{ID: "sym-a", RepoID: "repo-a", FullyQualifiedName: "shared.Handler", Kind: "function", Path: "/repo/handler.go"}
	store.symbols["sym-b"] = &Symbol{ID: "sym-b", RepoID: "repo-b", FullyQualifiedName: "shared.Handler", Kind: "function", Path: "/repo/handler.go"}
	store.chunks["sym-a"] = &Chunk{SymbolID: "sym-a", Path: "/repo/handler.go"}
	store.chunks["sym-b"] = &Chunk{SymbolID: "sym-b", Path: "/repo/handler.go"}
	srv := &server{store: store}

	body := `{"intent":"exact_lookup","query":"shared.Handler","max_results":10}`
	r := httptest.NewRequest(http.MethodPost, "/v1/code/query", bytes.NewBufferString(body))
	r.Header.Set("X-SCOS-Tenant-ID", "tenant-a")
	w := httptest.NewRecorder()
	srv.handleQuery(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Results []CodeQueryResult `json:"results"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 1 || resp.Results[0].Symbol.RepoID != "repo-a" {
		t.Fatalf("expected only tenant-a's symbol, got %+v", resp.Results)
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

// TestHandleQueryIgnoresSpoofedHeaderWhenClaimsPresent exercises the same
// regression end-to-end through handleQuery: an attacker authenticated as
// tenant-a sets X-SCOS-Tenant-ID to tenant-b (the victim) and must still
// only see tenant-a's (empty) results, never tenant-b's data.
func TestHandleQueryIgnoresSpoofedHeaderWhenClaimsPresent(t *testing.T) {
	store := newCodeStore()
	store.repos["repo-b"] = &Repository{ID: "repo-b", TenantID: "tenant-b", CanonicalPath: "/repo"}
	store.symbols["sym-b"] = &Symbol{ID: "sym-b", RepoID: "repo-b", FullyQualifiedName: "shared.Handler", Kind: "function", Path: "/repo/handler.go"}
	store.chunks["sym-b"] = &Chunk{SymbolID: "sym-b", Path: "/repo/handler.go"}
	srv := &server{store: store}

	body := `{"intent":"exact_lookup","query":"shared.Handler","max_results":10}`
	r := httptest.NewRequest(http.MethodPost, "/v1/code/query", bytes.NewBufferString(body))
	r.Header.Set("X-SCOS-Tenant-ID", "tenant-b") // spoofed victim header
	ctx := auth.ContextWithClaims(r.Context(), auth.Claims{TenantID: "tenant-a", PrincipalID: "attacker"})
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()
	srv.handleQuery(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Results []CodeQueryResult `json:"results"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 0 {
		t.Fatalf("expected no cross-tenant results via spoofed header, got %+v", resp.Results)
	}
}

func TestHandleQueryRequiresTenant(t *testing.T) {
	srv := &server{store: newCodeStore()}
	body := `{"intent":"exact_lookup","query":"x"}`
	r := httptest.NewRequest(http.MethodPost, "/v1/code/query", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	srv.handleQuery(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without a tenant header, got %d", w.Code)
	}
}

func TestHandleReposScopedToTenant(t *testing.T) {
	store := newCodeStore()
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

func TestHandleImpact(t *testing.T) {
	store := newCodeStore()
	store.symbols["sym-a"] = &Symbol{
		ID: "sym-a", FullyQualifiedName: "A", Kind: "function",
		OutboundRelations: []string{"B"},
		Visibility:        "public",
	}
	store.symbols["sym-b"] = &Symbol{
		ID: "sym-b", FullyQualifiedName: "B", Kind: "function",
		Tests: []string{"TestB"},
	}

	// Test impact detection directly
	store.mu.RLock()
	modifiedSet := map[string]bool{"b": true}
	var directDeps []string
	for _, sym := range store.symbols {
		for _, ref := range sym.OutboundRelations {
			if modifiedSet[strings.ToLower(ref)] {
				directDeps = append(directDeps, sym.FullyQualifiedName)
				break
			}
		}
	}
	store.mu.RUnlock()

	if len(directDeps) != 1 || directDeps[0] != "A" {
		t.Fatalf("expected A as direct dependant, got %v", directDeps)
	}
}

func TestHandleImpactReconcile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "code-intelligence.json")
	store := newCodeStoreAt(path)
	registerDefaultAdapters(store)
	store.repos["repo-1"] = &Repository{ID: "repo-1", TenantID: "tenant-1", CanonicalPath: "/repo"}
	srv := &server{store: store}

	body := `{"repository_id":"repo-1","commit_id":"abc","predicted_risk_score":0.1,"predicted_dependants":1,"actual_dependants":4,"build_passed":false,"tests_passed":false}`
	r := httptest.NewRequest(http.MethodPost, "/v1/impact/reconcile", bytes.NewBufferString(body))
	r.Header.Set("X-SCOS-Tenant-ID", "tenant-1")
	w := httptest.NewRecorder()
	srv.handleImpactReconcile(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var record ImpactReconciliation
	if err := json.Unmarshal(w.Body.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record.DependantCountError != 3 {
		t.Fatalf("expected dependant count error of 3, got %d", record.DependantCountError)
	}
	if !record.OutcomeMismatch {
		t.Fatal("expected an under-predicted risk score with a failed build to be flagged as a mismatch")
	}
	if len(store.reconciliations) != 1 {
		t.Fatalf("expected reconciliation to be recorded, got %d", len(store.reconciliations))
	}

	// The learning signal must survive a restart, like every other durable
	// index generation.
	restarted := newCodeStoreAt(path)
	registerDefaultAdapters(restarted)
	if err := restarted.load(); err != nil {
		t.Fatalf("load after restart: %v", err)
	}
	if len(restarted.reconciliations) != 1 {
		t.Fatalf("expected reconciliation to survive restart, got %d", len(restarted.reconciliations))
	}
}

func TestHandleImpactReconcileRequiresTenant(t *testing.T) {
	srv := &server{store: newCodeStore()}
	body := `{"repository_id":"repo-1","commit_id":"abc"}`
	r := httptest.NewRequest(http.MethodPost, "/v1/impact/reconcile", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	srv.handleImpactReconcile(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without a tenant header, got %d", w.Code)
	}
}

func TestContextAssembly(t *testing.T) {
	store := newCodeStore()
	store.symbols["sym-target"] = &Symbol{
		ID: "sym-target", FullyQualifiedName: "MyStruct", Kind: "type",
		Path: "/src/types.go",
	}
	store.chunks["sym-target"] = &Chunk{
		SymbolID: "sym-target", Content: "type MyStruct struct{}", Path: "/src/types.go",
	}

	// Test lookup
	store.mu.RLock()
	target, ok := store.symbols["sym-target"]
	store.mu.RUnlock()
	if !ok {
		t.Fatal("target symbol not found")
	}
	if target.FullyQualifiedName != "MyStruct" {
		t.Fatal("symbol name mismatch")
	}

	store.mu.RLock()
	chunk, ok := store.chunks["sym-target"]
	store.mu.RUnlock()
	if !ok {
		t.Fatal("chunk not found")
	}
	if chunk.Content != "type MyStruct struct{}" {
		t.Fatal("chunk content mismatch")
	}
}

func TestMinFloat(t *testing.T) {
	if minFloat(0.5, 1.0) != 0.5 {
		t.Fatal("minFloat should return smaller value")
	}
	if minFloat(1.0, 0.5) != 0.5 {
		t.Fatal("minFloat should return smaller value")
	}
}

func TestJSONRoundTripSymbol(t *testing.T) {
	sym := Symbol{
		ID: "sym1", FullyQualifiedName: "com.example.MyClass", Kind: "class",
		Path: "/src/MyClass.java", Language: "java",
		Imports: []string{"java.util.List"},
	}
	data, err := json.Marshal(sym)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Symbol
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.ID != "sym1" {
		t.Fatal("roundtrip failed")
	}
}

func TestJSONRoundTripGeneration(t *testing.T) {
	gen := IndexGeneration{
		Generation: 5, RepoID: "repo-1", CommitID: "abc123",
		SymbolCount: 100, FileCount: 20, Status: "active",
	}
	data, err := json.Marshal(gen)
	if err != nil {
		t.Fatal(err)
	}
	var decoded IndexGeneration
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.SymbolCount != 100 {
		t.Fatal("roundtrip failed")
	}
}

func TestHandleIndexViaCodeRoute(t *testing.T) {
	store := newCodeStore()
	store.repos["repo-1"] = &Repository{ID: "repo-1", TenantID: "tenant-1", CanonicalPath: "/repo"}
	srv := &server{store: store}
	r := httptest.NewRequest(http.MethodPost, "/v1/code/index", bytes.NewBufferString(`{"repository_id":"repo-1","commit_id":"abc","files":["main.go"]}`))
	r.Header.Set("X-SCOS-Tenant-ID", "tenant-1")
	w := httptest.NewRecorder()
	srv.handleIndex(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if store.generations["repo-1"].Status != "active" {
		t.Fatal("expected active index generation")
	}
}

func TestHandleIndexRejectsEmptyFilePath(t *testing.T) {
	store := newCodeStore()
	store.repos["repo-1"] = &Repository{ID: "repo-1", TenantID: "tenant-1"}
	srv := &server{store: store}
	r := httptest.NewRequest(http.MethodPost, "/v1/code/index", bytes.NewBufferString(`{"repository_id":"repo-1","files":[""]}`))
	r.Header.Set("X-SCOS-Tenant-ID", "tenant-1")
	w := httptest.NewRecorder()
	srv.handleIndex(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestCodeStoreRestoresRepositoriesAndGenerationsAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "code-intelligence.json")
	store := newCodeStoreAt(path)
	registerDefaultAdapters(store)
	srv := &server{store: store}

	create := httptest.NewRequest(http.MethodPost, "/v1/repos", bytes.NewBufferString(`{"canonical_path":"/repo","branch_or_worktree":"main"}`))
	create.Header.Set("X-SCOS-Tenant-ID", "tenant-1")
	createResponse := httptest.NewRecorder()
	srv.handleRepos(createResponse, create)
	if createResponse.Code != http.StatusOK {
		t.Fatalf("create repository: expected 200, got %d", createResponse.Code)
	}

	index := httptest.NewRequest(http.MethodPost, "/v1/index", bytes.NewBufferString(`{"repository_id":"repo-1","commit_id":"abc","files":["main.go"]}`))
	index.Header.Set("X-SCOS-Tenant-ID", "tenant-1")
	indexResponse := httptest.NewRecorder()
	srv.handleIndex(indexResponse, index)
	if indexResponse.Code != http.StatusOK {
		t.Fatalf("index repository: expected 200, got %d", indexResponse.Code)
	}

	restarted := newCodeStoreAt(path)
	registerDefaultAdapters(restarted)
	if err := restarted.load(); err != nil {
		t.Fatalf("load after restart: %v", err)
	}
	if restarted.repos["repo-1"].IndexGeneration != 1 {
		t.Fatalf("expected restored generation 1, got %d", restarted.repos["repo-1"].IndexGeneration)
	}
	if restarted.generations["repo-1"].Status != "active" {
		t.Fatalf("expected restored active generation")
	}
	if len(restarted.symbols) == 0 || len(restarted.chunks) == 0 {
		t.Fatal("expected indexed symbols and chunks after restart")
	}
}

func TestIndexDoesNotPublishWhenAtomicStoreCommitFails(t *testing.T) {
	dir := t.TempDir()
	blockedParent := filepath.Join(dir, "blocked")
	if err := os.WriteFile(blockedParent, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	store := newCodeStoreAt(filepath.Join(blockedParent, "store.json"))
	registerDefaultAdapters(store)
	store.repos["repo-1"] = &Repository{ID: "repo-1", TenantID: "tenant-1", CanonicalPath: "/repo"}
	srv := &server{store: store}

	request := httptest.NewRequest(http.MethodPost, "/v1/index", bytes.NewBufferString(`{"repository_id":"repo-1","commit_id":"abc","files":["main.go"]}`))
	request.Header.Set("X-SCOS-Tenant-ID", "tenant-1")
	response := httptest.NewRecorder()
	srv.handleIndex(response, request)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("expected failed commit to return 500, got %d", response.Code)
	}
	if len(store.symbols) != 0 || len(store.chunks) != 0 || len(store.generations) != 0 {
		t.Fatal("failed atomic commit published partial index state")
	}
	if store.repos["repo-1"].IndexGeneration != 0 {
		t.Fatal("failed atomic commit changed repository metadata")
	}
}

// seedTwoBranches indexes the same symbol name on two branches of two repos so
// branch isolation can be asserted.
func seedTwoBranches(t *testing.T) *server {
	t.Helper()
	s := &server{store: newCodeStore()}
	s.store.repos["repo-main"] = &Repository{
		ID: "repo-main", TenantID: "tenant-1", Branch: "main",
		CommitID: "commit-main", IndexGeneration: 1,
	}
	s.store.repos["repo-feature"] = &Repository{
		ID: "repo-feature", TenantID: "tenant-1", Branch: "feature-x",
		CommitID: "commit-feature", IndexGeneration: 1,
	}
	for id, repo := range map[string]string{"sym-main": "repo-main", "sym-feature": "repo-feature"} {
		s.store.symbols[id] = &Symbol{
			ID: id, RepoID: repo, FullyQualifiedName: "pkg.Target",
			Path: "pkg/target.go", Kind: "function",
		}
		s.store.chunks[id] = &Chunk{SymbolID: id, Content: "func Target() {}", Path: "pkg/target.go"}
	}
	return s
}

// TestQueryNeverCrossesBranches covers §19's prohibition on serving
// cross-branch code: a query scoped to one branch must not return symbols
// indexed from another.
func TestQueryNeverCrossesBranches(t *testing.T) {
	s := seedTwoBranches(t)

	results := s.withProvenance(s.executeQuery(CodeQuery{
		Intent: "symbol_lookup", Query: "pkg.Target", TenantID: "tenant-1", Branch: "main", MaxResults: 10,
	}))
	if len(results) == 0 {
		t.Fatal("branch-scoped query returned nothing")
	}
	for _, r := range results {
		if r.Branch != "main" {
			t.Errorf("query scoped to main returned a %q result", r.Branch)
		}
	}

	// The other branch resolves to its own symbol, not main's.
	other := s.withProvenance(s.executeQuery(CodeQuery{
		Intent: "symbol_lookup", Query: "pkg.Target", TenantID: "tenant-1", Branch: "feature-x", MaxResults: 10,
	}))
	for _, r := range other {
		if r.Branch != "feature-x" {
			t.Errorf("query scoped to feature-x returned a %q result", r.Branch)
		}
	}
}

// TestResultsCarryRepositoryBranchAndCommit covers §19's requirement that a
// result identify the exact repository, branch and commit it represents.
func TestResultsCarryRepositoryBranchAndCommit(t *testing.T) {
	s := seedTwoBranches(t)
	results := s.withProvenance(s.executeQuery(CodeQuery{
		Intent: "symbol_lookup", Query: "pkg.Target", TenantID: "tenant-1", Branch: "main", MaxResults: 10,
	}))
	if len(results) == 0 {
		t.Fatal("no results to check provenance on")
	}
	for _, r := range results {
		if r.Repo == "" || r.Branch == "" || r.CommitID == "" {
			t.Errorf("result is unattributable: repo=%q branch=%q commit=%q", r.Repo, r.Branch, r.CommitID)
		}
		if r.Chunk.Path == "" {
			t.Error("result carries no source path")
		}
	}
}

// TestUnknownBranchReturnsNothing guards against a mistyped branch silently
// falling back to the whole index.
func TestUnknownBranchReturnsNothing(t *testing.T) {
	s := seedTwoBranches(t)
	results := s.executeQuery(CodeQuery{
		Intent: "symbol_lookup", Query: "pkg.Target", TenantID: "tenant-1", Branch: "no-such-branch", MaxResults: 10,
	})
	if len(results) != 0 {
		t.Fatalf("unknown branch returned %d results", len(results))
	}
}

func TestReindexReplacesPreviousGenerationForRepository(t *testing.T) {
	store := newCodeStore()
	registerDefaultAdapters(store)
	store.repos["repo-1"] = &Repository{ID: "repo-1", TenantID: "tenant-1", CanonicalPath: "/repo", Branch: "main"}
	srv := &server{store: store}

	first := httptest.NewRequest(http.MethodPost, "/v1/index", bytes.NewBufferString(`{"repository_id":"repo-1","commit_id":"commit-1","files":["main.go","app.py"]}`))
	first.Header.Set("X-SCOS-Tenant-ID", "tenant-1")
	firstResp := httptest.NewRecorder()
	srv.handleIndex(firstResp, first)
	if firstResp.Code != http.StatusOK {
		t.Fatalf("first index: expected 200, got %d: %s", firstResp.Code, firstResp.Body.String())
	}
	if got := store.generations["repo-1"].FileCount; got != 2 {
		t.Fatalf("expected first generation file count 2, got %d", got)
	}

	second := httptest.NewRequest(http.MethodPost, "/v1/index", bytes.NewBufferString(`{"repository_id":"repo-1","commit_id":"commit-2","files":["main.go"]}`))
	second.Header.Set("X-SCOS-Tenant-ID", "tenant-1")
	secondResp := httptest.NewRecorder()
	srv.handleIndex(secondResp, second)
	if secondResp.Code != http.StatusOK {
		t.Fatalf("second index: expected 200, got %d: %s", secondResp.Code, secondResp.Body.String())
	}
	if got := store.generations["repo-1"].FileCount; got != 1 {
		t.Fatalf("expected replacement generation file count 1, got %d", got)
	}
	for _, sym := range store.symbols {
		if sym.RepoID == "repo-1" && sym.Language == "python" {
			t.Fatalf("stale symbol from previous generation survived reindex: %+v", sym)
		}
	}
}

func TestIndexCreatesConfigurationSymbols(t *testing.T) {
	store := newCodeStore()
	registerDefaultAdapters(store)
	store.repos["repo-1"] = &Repository{ID: "repo-1", TenantID: "tenant-1", CanonicalPath: "/repo", Branch: "main"}
	srv := &server{store: store}

	request := httptest.NewRequest(http.MethodPost, "/v1/index", bytes.NewBufferString(`{"repository_id":"repo-1","commit_id":"commit-1","files":["config/app.yaml","config/settings.json"]}`))
	request.Header.Set("X-SCOS-Tenant-ID", "tenant-1")
	response := httptest.NewRecorder()
	srv.handleIndex(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("index config files: expected 200, got %d: %s", response.Code, response.Body.String())
	}

	results := srv.executeQuery(CodeQuery{Intent: "configuration_lookup", Query: "settings.json", RepoID: "repo-1", MaxResults: 10})
	if len(results) == 0 {
		t.Fatal("expected configuration lookup to return indexed configuration symbol")
	}
	found := false
	for _, result := range results {
		if result.Symbol.Kind == "configuration" && result.Symbol.Path == "config/settings.json" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("configuration lookup did not surface the indexed config symbol: %+v", results)
	}
}

func TestIndexWithSourcesParsesDeclarations(t *testing.T) {
	store := newCodeStore()
	registerDefaultAdapters(store)
	store.repos["repo-1"] = &Repository{ID: "repo-1", TenantID: "tenant-1", CanonicalPath: "/repo", Branch: "main"}
	srv := &server{store: store}

	body := `{"repository_id":"repo-1","commit_id":"c1","files":["README.md"],"sources":[{"path":"svc/handler.go","content":"package svc\n\n// Serve answers.\nfunc Serve() string {\n\treturn \"ok\"\n}\n"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/index", bytes.NewBufferString(body))
	req.Header.Set("X-SCOS-Tenant-ID", "tenant-1")
	res := httptest.NewRecorder()
	srv.handleIndex(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("index: %d %s", res.Code, res.Body.String())
	}
	if store.generations["repo-1"].FileCount != 2 {
		t.Fatalf("file count = %d", store.generations["repo-1"].FileCount)
	}
	results := srv.executeQuery(CodeQuery{Intent: "symbol_lookup", Query: "Serve", RepoID: "repo-1", MaxResults: 5})
	if len(results) != 1 || results[0].Symbol.FullyQualifiedName != "svc.Serve" {
		t.Fatalf("symbol lookup = %+v", results)
	}
	if !strings.Contains(results[0].Chunk.Content, "return \"ok\"") || results[0].Chunk.LineStart != 4 || results[0].Chunk.LineEnd != 6 {
		t.Fatalf("chunk must be the declaration's source span: %+v", results[0].Chunk)
	}

	bad := httptest.NewRequest(http.MethodPost, "/v1/index", bytes.NewBufferString(`{"repository_id":"repo-1","sources":[{"path":"x.go","content":"package x\nfunc {"}]}`))
	bad.Header.Set("X-SCOS-Tenant-ID", "tenant-1")
	badRes := httptest.NewRecorder()
	srv.handleIndex(badRes, bad)
	if badRes.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unparseable source should be 422, got %d", badRes.Code)
	}
	if store.generations["repo-1"].CommitID != "c1" {
		t.Fatal("a failed index must not replace the committed generation")
	}
}
