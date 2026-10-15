package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

// End-to-end tests for the v2 HTTP routes (pghandlers.go), through the
// actual handler functions rather than pgCodeStore directly, against the
// same live database as pgstore_integration_test.go (see that file's doc
// comment for how to set SCOS_CODE_STORE_TEST_DSN).

func pgRequest(t *testing.T, srv *server, method, path, tenant string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return pgRequestAs(t, srv, method, path, tenant, "p", body)
}

func pgRequestAs(t *testing.T, srv *server, method, path, tenant, principal string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	} else {
		reader = bytes.NewReader(nil)
	}
	r := httptest.NewRequest(method, path, reader)
	r = r.WithContext(auth.ContextWithClaims(r.Context(), auth.Claims{TenantID: tenant, PrincipalID: principal}))
	w := httptest.NewRecorder()
	switch path {
	case "/v2/code/repos":
		srv.handlePGRepos(w, r)
	case "/v2/code/index":
		srv.handlePGIndex(w, r)
	case "/v2/code/query":
		srv.handlePGQuery(w, r)
	case "/v2/code/snapshots/diff":
		srv.handlePGSnapshotDiff(w, r)
	case "/v2/code/settings":
		srv.handlePGSettings(w, r)
	case "/v2/code/impact":
		srv.handlePGImpact(w, r)
	case "/v2/code/index/scip":
		srv.handlePGIndexSCIP(w, r)
	default:
		t.Fatalf("unhandled path %s", path)
	}
	return w
}

func TestHandlePGRoutesReturnUnavailableWithoutStoreConfigured(t *testing.T) {
	srv := &server{}
	for _, path := range []string{"/v2/code/repos", "/v2/code/index", "/v2/code/query", "/v2/code/snapshots/diff", "/v2/code/settings", "/v2/code/impact", "/v2/code/index/scip"} {
		w := pgRequest(t, srv, http.MethodPost, path, "tenant-a", map[string]any{})
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: expected 503 without a configured store, got %d: %s", path, w.Code, w.Body.String())
		}
	}
}

func TestHandlePGEndToEndIndexAndQuery(t *testing.T) {
	_, db := testPGStore(t)
	srv := &server{pgStore: newPGCodeStore(db)}
	tenant := testTenant(t, db)

	reposResp := pgRequest(t, srv, http.MethodPost, "/v2/code/repos", tenant, map[string]any{
		"canonical_path": "/repo/" + t.Name(),
	})
	if reposResp.Code != http.StatusOK {
		t.Fatalf("create repo: %d: %s", reposResp.Code, reposResp.Body.String())
	}
	var reposOut struct {
		RepositoryID string `json:"repository_id"`
	}
	if err := json.Unmarshal(reposResp.Body.Bytes(), &reposOut); err != nil {
		t.Fatal(err)
	}

	indexResp := pgRequest(t, srv, http.MethodPost, "/v2/code/index", tenant, map[string]any{
		"repository_id": reposOut.RepositoryID,
		"branch":        "main",
		"commit_id":     "commit-1",
		"files": []map[string]any{
			{"path": "pkg/handler.go", "language": "go", "content_hash": hashContent("handler-v1")},
		},
	})
	if indexResp.Code != http.StatusOK {
		t.Fatalf("index: %d: %s", indexResp.Code, indexResp.Body.String())
	}

	queryResp := pgRequest(t, srv, http.MethodPost, "/v2/code/query", tenant, map[string]any{
		"repository_id": reposOut.RepositoryID,
		"branch":        "main",
		"query":         "pkg/handler.go",
	})
	if queryResp.Code != http.StatusOK {
		t.Fatalf("query: %d: %s", queryResp.Code, queryResp.Body.String())
	}
	var queryOut struct {
		Results []PGSymbolVersion `json:"results"`
	}
	if err := json.Unmarshal(queryResp.Body.Bytes(), &queryOut); err != nil {
		t.Fatal(err)
	}
	if len(queryOut.Results) != 1 {
		t.Fatalf("expected exactly one result, got %d: %+v", len(queryOut.Results), queryOut.Results)
	}
}

// A repository_id that exists but belongs to a different tenant must
// behave exactly as if it didn't exist.
func TestHandlePGIndexRejectsCrossTenantRepository(t *testing.T) {
	_, db := testPGStore(t)
	srv := &server{pgStore: newPGCodeStore(db)}
	ownerTenant := testTenant(t, db)
	attackerTenant := testTenant(t, db)

	reposResp := pgRequest(t, srv, http.MethodPost, "/v2/code/repos", ownerTenant, map[string]any{
		"canonical_path": "/repo/" + t.Name(),
	})
	var reposOut struct {
		RepositoryID string `json:"repository_id"`
	}
	if err := json.Unmarshal(reposResp.Body.Bytes(), &reposOut); err != nil {
		t.Fatal(err)
	}

	indexResp := pgRequest(t, srv, http.MethodPost, "/v2/code/index", attackerTenant, map[string]any{
		"repository_id": reposOut.RepositoryID,
		"branch":        "main",
		"commit_id":     "commit-1",
		"files": []map[string]any{
			{"path": "pkg/handler.go", "content_hash": hashContent("x")},
		},
	})
	if indexResp.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for another tenant's repository, got %d: %s", indexResp.Code, indexResp.Body.String())
	}

	queryResp := pgRequest(t, srv, http.MethodPost, "/v2/code/query", attackerTenant, map[string]any{
		"repository_id": reposOut.RepositoryID,
		"branch":        "main",
		"query":         "pkg/handler.go",
	})
	if queryResp.Code != http.StatusOK {
		t.Fatalf("query: %d: %s", queryResp.Code, queryResp.Body.String())
	}
	var queryOut struct {
		Results []PGSymbolVersion `json:"results"`
	}
	if err := json.Unmarshal(queryResp.Body.Bytes(), &queryOut); err != nil {
		t.Fatal(err)
	}
	if len(queryOut.Results) != 0 {
		t.Fatalf("expected no results querying another tenant's repository, got %+v", queryOut.Results)
	}
}

// The v2 query route falls back to lexical search when no exact path/FQN
// match exists, so a caller of the HTTP API -- not just pgCodeStore
// directly -- benefits from migration 000026's FTS index.
func TestHandlePGQueryFallsBackToLexicalSearch(t *testing.T) {
	_, db := testPGStore(t)
	srv := &server{pgStore: newPGCodeStore(db)}
	tenant := testTenant(t, db)

	reposResp := pgRequest(t, srv, http.MethodPost, "/v2/code/repos", tenant, map[string]any{
		"canonical_path": "/repo/" + t.Name(),
	})
	var reposOut struct {
		RepositoryID string `json:"repository_id"`
	}
	if err := json.Unmarshal(reposResp.Body.Bytes(), &reposOut); err != nil {
		t.Fatal(err)
	}

	pgRequest(t, srv, http.MethodPost, "/v2/code/index", tenant, map[string]any{
		"repository_id": reposOut.RepositoryID,
		"branch":        "main",
		"commit_id":     "commit-1",
		"files": []map[string]any{
			{"path": "internal/auth/middleware.go", "content_hash": hashContent("auth-mw")},
		},
	})

	queryResp := pgRequest(t, srv, http.MethodPost, "/v2/code/query", tenant, map[string]any{
		"repository_id": reposOut.RepositoryID,
		"branch":        "main",
		"query":         "middleware",
	})
	if queryResp.Code != http.StatusOK {
		t.Fatalf("query: %d: %s", queryResp.Code, queryResp.Body.String())
	}
	var queryOut struct {
		Results []PGSymbolVersion `json:"results"`
	}
	if err := json.Unmarshal(queryResp.Body.Bytes(), &queryOut); err != nil {
		t.Fatal(err)
	}
	if len(queryOut.Results) != 1 || queryOut.Results[0].MatchType != "lexical" {
		t.Fatalf("expected exactly one lexical match, got %+v", queryOut.Results)
	}
}

// The index response's inline diff, and the standalone diff endpoint, must
// agree -- and a one-line change to one file (of several indexed) must
// show up as exactly one Modified entry, not a full re-index, through the
// actual HTTP surface a caller uses.
func TestHandlePGIndexReturnsDiffAndDiffEndpointAgrees(t *testing.T) {
	_, db := testPGStore(t)
	srv := &server{pgStore: newPGCodeStore(db)}
	tenant := testTenant(t, db)

	reposResp := pgRequest(t, srv, http.MethodPost, "/v2/code/repos", tenant, map[string]any{
		"canonical_path": "/repo/" + t.Name(),
	})
	var reposOut struct {
		RepositoryID string `json:"repository_id"`
	}
	if err := json.Unmarshal(reposResp.Body.Bytes(), &reposOut); err != nil {
		t.Fatal(err)
	}

	pgRequest(t, srv, http.MethodPost, "/v2/code/index", tenant, map[string]any{
		"repository_id": reposOut.RepositoryID,
		"branch":        "main",
		"commit_id":     "commit-1",
		"files": []map[string]any{
			{"path": "pkg/a.go", "content_hash": hashContent("a-v1")},
			{"path": "pkg/b.go", "content_hash": hashContent("b")},
			{"path": "pkg/c.go", "content_hash": hashContent("c")},
		},
	})

	indexResp := pgRequest(t, srv, http.MethodPost, "/v2/code/index", tenant, map[string]any{
		"repository_id": reposOut.RepositoryID,
		"branch":        "main",
		"commit_id":     "commit-2",
		"files": []map[string]any{
			{"path": "pkg/a.go", "content_hash": hashContent("a-v2")}, // one-line change
			{"path": "pkg/b.go", "content_hash": hashContent("b")},
			{"path": "pkg/c.go", "content_hash": hashContent("c")},
		},
	})
	if indexResp.Code != http.StatusOK {
		t.Fatalf("index: %d: %s", indexResp.Code, indexResp.Body.String())
	}
	var indexOut struct {
		SnapshotID string       `json:"snapshot_id"`
		Diff       SnapshotDiff `json:"diff"`
	}
	if err := json.Unmarshal(indexResp.Body.Bytes(), &indexOut); err != nil {
		t.Fatal(err)
	}
	if len(indexOut.Diff.Modified) != 1 || indexOut.Diff.Modified[0].Path != "pkg/a.go" {
		t.Fatalf("expected a single-file change to produce exactly one Modified entry for pkg/a.go, got %+v", indexOut.Diff)
	}
	if len(indexOut.Diff.Added) != 0 || len(indexOut.Diff.Removed) != 0 || len(indexOut.Diff.Renamed) != 0 {
		t.Fatalf("expected no other diff categories for an unrelated one-file change, got %+v", indexOut.Diff)
	}

	diffResp := pgRequest(t, srv, http.MethodPost, "/v2/code/snapshots/diff", tenant, map[string]any{
		"snapshot_id": indexOut.SnapshotID,
	})
	if diffResp.Code != http.StatusOK {
		t.Fatalf("diff endpoint: %d: %s", diffResp.Code, diffResp.Body.String())
	}
	var standaloneDiff SnapshotDiff
	if err := json.Unmarshal(diffResp.Body.Bytes(), &standaloneDiff); err != nil {
		t.Fatal(err)
	}
	if len(standaloneDiff.Modified) != 1 || standaloneDiff.Modified[0].Path != "pkg/a.go" {
		t.Fatalf("expected the standalone diff endpoint to agree with the inline diff, got %+v", standaloneDiff)
	}
}

// A snapshot belonging to another tenant must 404 from the diff endpoint,
// the same rule enforced for /v2/code/index and /v2/code/query.
func TestHandlePGSnapshotDiffRejectsCrossTenantSnapshot(t *testing.T) {
	_, db := testPGStore(t)
	srv := &server{pgStore: newPGCodeStore(db)}
	ownerTenant := testTenant(t, db)
	attackerTenant := testTenant(t, db)

	reposResp := pgRequest(t, srv, http.MethodPost, "/v2/code/repos", ownerTenant, map[string]any{
		"canonical_path": "/repo/" + t.Name(),
	})
	var reposOut struct {
		RepositoryID string `json:"repository_id"`
	}
	if err := json.Unmarshal(reposResp.Body.Bytes(), &reposOut); err != nil {
		t.Fatal(err)
	}
	indexResp := pgRequest(t, srv, http.MethodPost, "/v2/code/index", ownerTenant, map[string]any{
		"repository_id": reposOut.RepositoryID,
		"branch":        "main",
		"commit_id":     "commit-1",
		"files":         []map[string]any{{"path": "a.go", "content_hash": hashContent("a")}},
	})
	var indexOut struct {
		SnapshotID string `json:"snapshot_id"`
	}
	if err := json.Unmarshal(indexResp.Body.Bytes(), &indexOut); err != nil {
		t.Fatal(err)
	}

	diffResp := pgRequest(t, srv, http.MethodPost, "/v2/code/snapshots/diff", attackerTenant, map[string]any{
		"snapshot_id": indexOut.SnapshotID,
	})
	if diffResp.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for another tenant's snapshot, got %d: %s", diffResp.Code, diffResp.Body.String())
	}
}

func TestHandlePGSettingsGetDefaultsEnabledThenRoundTrips(t *testing.T) {
	_, db := testPGStore(t)
	srv := &server{pgStore: newPGCodeStore(db)}
	tenant := testTenant(t, db)

	getResp := pgRequest(t, srv, http.MethodGet, "/v2/code/settings", tenant, nil)
	if getResp.Code != http.StatusOK {
		t.Fatalf("get settings: %d: %s", getResp.Code, getResp.Body.String())
	}
	var out struct {
		DurableStoreEnabled bool `json:"durable_store_enabled"`
	}
	if err := json.Unmarshal(getResp.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.DurableStoreEnabled {
		t.Fatal("expected a user with no recorded preference to default to enabled")
	}

	putResp := pgRequest(t, srv, http.MethodPut, "/v2/code/settings", tenant, map[string]any{"durable_store_enabled": false})
	if putResp.Code != http.StatusOK {
		t.Fatalf("put settings: %d: %s", putResp.Code, putResp.Body.String())
	}

	getResp2 := pgRequest(t, srv, http.MethodGet, "/v2/code/settings", tenant, nil)
	if err := json.Unmarshal(getResp2.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.DurableStoreEnabled {
		t.Fatal("expected the preference to persist as disabled after PUT")
	}
}

// The core assertion this whole feature exists for: once a user disables
// the durable store for themselves, every v2 route rejects their calls --
// but another user in the same tenant, who never opted out, is unaffected.
func TestHandlePGRoutesRejectCallsFromUserWhoDisabledStore(t *testing.T) {
	_, db := testPGStore(t)
	srv := &server{pgStore: newPGCodeStore(db)}
	tenant := testTenant(t, db)

	putResp := pgRequestAs(t, srv, http.MethodPut, "/v2/code/settings", tenant, "opted-out-user", map[string]any{"durable_store_enabled": false})
	if putResp.Code != http.StatusOK {
		t.Fatalf("put settings: %d: %s", putResp.Code, putResp.Body.String())
	}

	reposResp := pgRequestAs(t, srv, http.MethodPost, "/v2/code/repos", tenant, "opted-out-user", map[string]any{
		"canonical_path": "/repo/" + t.Name(),
	})
	if reposResp.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a user who disabled the store, got %d: %s", reposResp.Code, reposResp.Body.String())
	}

	// The unaffected user in the same tenant can still use it normally.
	okResp := pgRequestAs(t, srv, http.MethodPost, "/v2/code/repos", tenant, "other-user", map[string]any{
		"canonical_path": "/repo/" + t.Name() + "-other",
	})
	if okResp.Code != http.StatusOK {
		t.Fatalf("expected a user who never opted out to be unaffected, got %d: %s", okResp.Code, okResp.Body.String())
	}
}

// A caller who submits real Go source as a file's "content" gets real
// declarations back through the actual HTTP index route, not just through
// pgCodeStore directly (see TestPGIndexSnapshotExtractsRealGoDeclarations).
func TestHandlePGIndexWithContentExtractsRealGoDeclarations(t *testing.T) {
	_, db := testPGStore(t)
	srv := &server{pgStore: newPGCodeStore(db)}
	tenant := testTenant(t, db)

	reposResp := pgRequest(t, srv, http.MethodPost, "/v2/code/repos", tenant, map[string]any{
		"canonical_path": "/repo/" + t.Name(),
	})
	var reposOut struct {
		RepositoryID string `json:"repository_id"`
	}
	if err := json.Unmarshal(reposResp.Body.Bytes(), &reposOut); err != nil {
		t.Fatal(err)
	}

	const source = "package sample\n\nfunc CreateUser(name string) error {\n\treturn nil\n}\n"
	indexResp := pgRequest(t, srv, http.MethodPost, "/v2/code/index", tenant, map[string]any{
		"repository_id": reposOut.RepositoryID,
		"branch":        "main",
		"commit_id":     "commit-1",
		"files": []map[string]any{
			{"path": "pkg/sample.go", "language": "go", "content_hash": hashContent(source), "content": source},
		},
	})
	if indexResp.Code != http.StatusOK {
		t.Fatalf("index: %d: %s", indexResp.Code, indexResp.Body.String())
	}

	queryResp := pgRequest(t, srv, http.MethodPost, "/v2/code/query", tenant, map[string]any{
		"repository_id": reposOut.RepositoryID,
		"branch":        "main",
		"query":         "sample.CreateUser",
	})
	if queryResp.Code != http.StatusOK {
		t.Fatalf("query: %d: %s", queryResp.Code, queryResp.Body.String())
	}
	var queryOut struct {
		Results []PGSymbolVersion `json:"results"`
	}
	if err := json.Unmarshal(queryResp.Body.Bytes(), &queryOut); err != nil {
		t.Fatal(err)
	}
	if len(queryOut.Results) != 1 || queryOut.Results[0].FullyQualifiedName != "sample.CreateUser" {
		t.Fatalf("expected exactly one result for sample.CreateUser, got %+v", queryOut.Results)
	}
	if queryOut.Results[0].MatchType != "exact" {
		t.Fatalf("expected an exact match, got %q", queryOut.Results[0].MatchType)
	}
}

const impactHandlerSource = `package sample

func leaf() int {
	return 1
}

func middle() int {
	return leaf()
}
`

func TestHandlePGImpactFindsDirectDependents(t *testing.T) {
	_, db := testPGStore(t)
	srv := &server{pgStore: newPGCodeStore(db)}
	tenant := testTenant(t, db)

	reposResp := pgRequest(t, srv, http.MethodPost, "/v2/code/repos", tenant, map[string]any{
		"canonical_path": "/repo/" + t.Name(),
	})
	var reposOut struct {
		RepositoryID string `json:"repository_id"`
	}
	if err := json.Unmarshal(reposResp.Body.Bytes(), &reposOut); err != nil {
		t.Fatal(err)
	}
	pgRequest(t, srv, http.MethodPost, "/v2/code/index", tenant, map[string]any{
		"repository_id": reposOut.RepositoryID,
		"branch":        "main",
		"commit_id":     "commit-1",
		"files": []map[string]any{
			{"path": "pkg/sample.go", "language": "go", "content_hash": hashContent(impactHandlerSource), "content": impactHandlerSource},
		},
	})

	impactResp := pgRequest(t, srv, http.MethodPost, "/v2/code/impact", tenant, map[string]any{
		"repository_id": reposOut.RepositoryID,
		"branch":        "main",
		"query":         "sample.leaf",
	})
	if impactResp.Code != http.StatusOK {
		t.Fatalf("impact: %d: %s", impactResp.Code, impactResp.Body.String())
	}
	var impact PGImpact
	if err := json.Unmarshal(impactResp.Body.Bytes(), &impact); err != nil {
		t.Fatal(err)
	}
	if !hasFQN(impact.DirectDependents, "sample.middle") {
		t.Fatalf("expected middle in DirectDependents, got %+v", impact.DirectDependents)
	}
}

func TestHandlePGImpactReturns404ForUnknownSymbol(t *testing.T) {
	_, db := testPGStore(t)
	srv := &server{pgStore: newPGCodeStore(db)}
	tenant := testTenant(t, db)

	reposResp := pgRequest(t, srv, http.MethodPost, "/v2/code/repos", tenant, map[string]any{
		"canonical_path": "/repo/" + t.Name(),
	})
	var reposOut struct {
		RepositoryID string `json:"repository_id"`
	}
	if err := json.Unmarshal(reposResp.Body.Bytes(), &reposOut); err != nil {
		t.Fatal(err)
	}

	impactResp := pgRequest(t, srv, http.MethodPost, "/v2/code/impact", tenant, map[string]any{
		"repository_id": reposOut.RepositoryID,
		"branch":        "main",
		"query":         "sample.doesNotExist",
	})
	if impactResp.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for an unknown symbol, got %d: %s", impactResp.Code, impactResp.Body.String())
	}
}

func TestHandlePGImpactRejectsCrossTenantRepository(t *testing.T) {
	_, db := testPGStore(t)
	srv := &server{pgStore: newPGCodeStore(db)}
	ownerTenant := testTenant(t, db)
	attackerTenant := testTenant(t, db)

	reposResp := pgRequest(t, srv, http.MethodPost, "/v2/code/repos", ownerTenant, map[string]any{
		"canonical_path": "/repo/" + t.Name(),
	})
	var reposOut struct {
		RepositoryID string `json:"repository_id"`
	}
	if err := json.Unmarshal(reposResp.Body.Bytes(), &reposOut); err != nil {
		t.Fatal(err)
	}

	impactResp := pgRequest(t, srv, http.MethodPost, "/v2/code/impact", attackerTenant, map[string]any{
		"repository_id": reposOut.RepositoryID,
		"branch":        "main",
		"query":         "sample.leaf",
	})
	if impactResp.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for another tenant's repository, got %d: %s", impactResp.Code, impactResp.Body.String())
	}
}

func TestHandlePGQueryExactBypassesCodeModelRuntime(t *testing.T) {
	store, db := testPGStore(t)
	tenant := testTenant(t, db)
	repoID, err := store.EnsureRepository(t.Context(), tenant, "/repo/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	snapshotID, err := store.CreateSnapshot(t.Context(), repoID, tenant, "main", "commit-1", "test", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(t.Context(), snapshotID, repoID, []PGFileInput{{Path: "pkg/exact.go", Language: "go", ContentHash: hashContent("package pkg\nfunc Exact() {}\n"), Content: "package pkg\nfunc Exact() {}\n"}}); err != nil {
		t.Fatal(err)
	}

	calls := 0
	runtime := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, "exact queries must not reach the model runtime", http.StatusInternalServerError)
	}))
	defer runtime.Close()
	t.Setenv("SCOS_MODEL_RUNTIME_URL", runtime.URL)
	t.Setenv("SCOS_INTERNAL_SERVICE_TOKEN", "service-token")
	t.Setenv("SCOS_CONTROL_PLANE_TOKEN", "control-token")

	resp := pgRequest(t, &server{pgStore: store}, http.MethodPost, "/v2/code/query", tenant, map[string]any{
		"repository_id": repoID,
		"branch":        "main",
		"query":         "pkg/exact.go",
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("query: %d: %s", resp.Code, resp.Body.String())
	}
	if calls != 0 {
		t.Fatalf("exact query made %d model-runtime calls", calls)
	}
	var out struct {
		RetrievalMode string            `json:"retrieval_mode"`
		Degraded      []string          `json:"degraded_models"`
		Results       []PGSymbolVersion `json:"results"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.RetrievalMode != "exact" || len(out.Degraded) != 0 || len(out.Results) == 0 {
		t.Fatalf("unexpected exact response: %+v", out)
	}
	for _, result := range out.Results {
		if result.MatchType != "exact" || result.Path != "pkg/exact.go" {
			t.Fatalf("exact path query returned non-exact result: %+v", result)
		}
	}
}

func TestHandlePGQueryDegradesToLexicalWhenCodeRuntimeFails(t *testing.T) {
	store, db := testPGStore(t)
	tenant := testTenant(t, db)
	repoID, err := store.EnsureRepository(t.Context(), tenant, "/repo/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	snapshotID, err := store.CreateSnapshot(t.Context(), repoID, tenant, "main", "commit-1", "test", "")
	if err != nil {
		t.Fatal(err)
	}
	content := "package auth\nfunc Middleware() {}\n"
	if err := store.IndexSnapshot(t.Context(), snapshotID, repoID, []PGFileInput{{Path: "internal/auth/middleware.go", Language: "go", ContentHash: hashContent(content), Content: content}}); err != nil {
		t.Fatal(err)
	}

	runtime := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model unavailable", http.StatusServiceUnavailable)
	}))
	defer runtime.Close()
	t.Setenv("SCOS_MODEL_RUNTIME_URL", runtime.URL)
	t.Setenv("SCOS_INTERNAL_SERVICE_TOKEN", "service-token")
	t.Setenv("SCOS_CONTROL_PLANE_TOKEN", "control-token")
	t.Setenv("SCOS_MODEL_CODE_MODEL", "BAAI/bge-code-v1")
	t.Setenv("SCOS_MODEL_CODE_REVISION", "rev-test")
	t.Setenv("SCOS_MODEL_CODE_DIMENSIONS", "3")
	t.Setenv("SCOS_MODEL_CODE_GENERATION", "1")

	resp := pgRequest(t, &server{pgStore: store}, http.MethodPost, "/v2/code/query", tenant, map[string]any{
		"repository_id": repoID,
		"branch":        "main",
		"query":         "middleware",
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("query: %d: %s", resp.Code, resp.Body.String())
	}
	var out struct {
		RetrievalMode string             `json:"retrieval_mode"`
		Degraded      []string           `json:"degraded_models"`
		Results       []codeHybridResult `json:"results"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.RetrievalMode != "lexical" || len(out.Degraded) != 1 || out.Degraded[0] != "code_embedding" {
		t.Fatalf("expected explicit lexical degradation, got %+v", out)
	}
	if len(out.Results) == 0 || out.Results[0].MatchType != "lexical" {
		t.Fatalf("expected lexical results during model outage, got %+v", out.Results)
	}
}

func TestHandlePGQueryUsesHybridBGECodeRetrieval(t *testing.T) {
	store, db := testPGStore(t)
	tenant := testTenant(t, db)
	repoID, err := store.EnsureRepository(t.Context(), tenant, "/repo/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	snapshotID, err := store.CreateSnapshot(t.Context(), repoID, tenant, "main", "commit-1", "test", "")
	if err != nil {
		t.Fatal(err)
	}
	content := "package auth\n\n// Middleware validates a request.\nfunc Middleware() {}\n"
	if err := store.IndexSnapshot(t.Context(), snapshotID, repoID, []PGFileInput{{Path: "internal/auth/middleware.go", Language: "go", ContentHash: hashContent(content), Content: content}}); err != nil {
		t.Fatal(err)
	}

	calls := 0
	runtime := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/embed/code" {
			t.Fatalf("unexpected runtime path %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer service-token" || r.Header.Get("X-SCOS-Control-Plane-Token") != "control-token" {
			t.Fatalf("missing governed model-runtime auth")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"embedding":      []float64{1, 0, 0},
			"model":          "BAAI/bge-code-v1",
			"model_revision": "rev-test",
			"dimensions":     3,
			"role":           "code",
		})
	}))
	defer runtime.Close()
	t.Setenv("SCOS_MODEL_RUNTIME_URL", runtime.URL)
	t.Setenv("SCOS_INTERNAL_SERVICE_TOKEN", "service-token")
	t.Setenv("SCOS_CONTROL_PLANE_TOKEN", "control-token")
	t.Setenv("SCOS_MODEL_CODE_MODEL", "BAAI/bge-code-v1")
	t.Setenv("SCOS_MODEL_CODE_REVISION", "rev-test")
	t.Setenv("SCOS_MODEL_CODE_DIMENSIONS", "3")
	t.Setenv("SCOS_MODEL_CODE_GENERATION", "1")

	resp := pgRequest(t, &server{pgStore: store}, http.MethodPost, "/v2/code/query", tenant, map[string]any{
		"repository_id": repoID,
		"branch":        "main",
		"query":         "middleware",
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("query: %d: %s", resp.Code, resp.Body.String())
	}
	var out struct {
		RetrievalMode string             `json:"retrieval_mode"`
		Degraded      []string           `json:"degraded_models"`
		Results       []codeHybridResult `json:"results"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.RetrievalMode != "hybrid" || len(out.Degraded) != 0 {
		t.Fatalf("unexpected hybrid metadata: %+v", out)
	}
	if calls < 2 {
		t.Fatalf("expected snapshot backfill plus query embedding calls, got %d", calls)
	}
	if len(out.Results) == 0 {
		t.Fatal("expected hybrid results")
	}
	foundHybrid := false
	for _, result := range out.Results {
		if result.MatchType == "hybrid" && result.RawLexicalScore > 0 && result.RawDenseScore > 0 && result.RRFScore > 0 {
			foundHybrid = true
			break
		}
	}
	if !foundHybrid {
		t.Fatalf("expected a result with preserved lexical+dense+RRF scores: %+v", out.Results)
	}
}

func TestHandlePGQueryFusesStructuralAndPageRankSignals(t *testing.T) {
	store, db := testPGStore(t)
	tenant := testTenant(t, db)
	repoID, err := store.EnsureRepository(t.Context(), tenant, "/repo/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	snapshotID, err := store.CreateSnapshot(t.Context(), repoID, tenant, "main", "commit-1", "test", "")
	if err != nil {
		t.Fatal(err)
	}
	content := "package graph\nfunc Target() {}\nfunc Caller(){ Target() }\n"
	if err := store.IndexSnapshot(t.Context(), snapshotID, repoID, []PGFileInput{{Path: "graph.go", Language: "go", ContentHash: hashContent(content), Content: content}}); err != nil {
		t.Fatal(err)
	}

	runtime := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"embedding": []float64{1, 0, 0}, "model": "BAAI/bge-code-v1", "model_revision": "rev-test", "dimensions": 3, "role": "code"})
	}))
	defer runtime.Close()
	t.Setenv("SCOS_MODEL_RUNTIME_URL", runtime.URL)
	t.Setenv("SCOS_INTERNAL_SERVICE_TOKEN", "service-token")
	t.Setenv("SCOS_CONTROL_PLANE_TOKEN", "control-token")
	t.Setenv("SCOS_MODEL_CODE_MODEL", "BAAI/bge-code-v1")
	t.Setenv("SCOS_MODEL_CODE_REVISION", "rev-test")
	t.Setenv("SCOS_MODEL_CODE_DIMENSIONS", "3")
	t.Setenv("SCOS_MODEL_CODE_GENERATION", "1")

	resp := pgRequest(t, &server{pgStore: store}, http.MethodPost, "/v2/code/query", tenant, map[string]any{"repository_id": repoID, "branch": "main", "query": "graph target"})
	if resp.Code != http.StatusOK {
		t.Fatalf("query: %d: %s", resp.Code, resp.Body.String())
	}
	var out struct {
		Signals []string           `json:"signals"`
		Results []codeHybridResult `json:"results"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(out.Signals, ",")
	if !strings.Contains(joined, "structural") || !strings.Contains(joined, "pagerank") {
		t.Fatalf("missing graph signals: %+v", out.Signals)
	}
	found := false
	for _, r := range out.Results {
		if r.RawStructuralScore > 0 && r.Centrality > 0 && r.FusedScore >= r.RRFScore {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected structural/PageRank provenance in results: %+v", out.Results)
	}
}

func TestHandlePGIndexRefusesAContentHashThatDoesNotMatchTheContent(t *testing.T) {
	store, db := testPGStore(t)
	tenant := testTenant(t, db)
	repoID, err := store.EnsureRepository(t.Context(), tenant, "/repo/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	content := "package a\nfunc A() {}\n"
	resp := pgRequest(t, &server{pgStore: store}, http.MethodPost, "/v2/code/index", tenant, map[string]any{
		"repository_id": repoID, "branch": "main", "commit_id": "c1",
		"files": []map[string]any{{"path": "a.go", "language": "go", "content": content, "content_hash": hashContent("something else")}},
	})
	if resp.Code != http.StatusBadRequest || !strings.Contains(resp.Body.String(), "does not match") {
		t.Fatalf("mismatched hash must be refused, got %d %s", resp.Code, resp.Body.String())
	}
	resp = pgRequest(t, &server{pgStore: store}, http.MethodPost, "/v2/code/index", tenant, map[string]any{
		"repository_id": repoID, "branch": "main", "commit_id": "c1",
		"files": []map[string]any{{"path": "a.go", "language": "go", "content": content, "content_hash": hashContent(content)}},
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("matching hash must be indexed, got %d %s", resp.Code, resp.Body.String())
	}
}

// Re-sending the same tree under the same snapshot key (a client retry)
// returns the snapshot already built; different files under that key are
// refused with 409 instead of failing on duplicate rows.
func TestHandlePGIndexSameCommitIsIdempotent(t *testing.T) {
	_, db := testPGStore(t)
	srv := &server{pgStore: newPGCodeStore(db)}
	tenant := testTenant(t, db)

	reposResp := pgRequest(t, srv, http.MethodPost, "/v2/code/repos", tenant, map[string]any{"canonical_path": "/repo/" + t.Name()})
	var repo struct {
		RepositoryID string `json:"repository_id"`
	}
	if err := json.Unmarshal(reposResp.Body.Bytes(), &repo); err != nil {
		t.Fatal(err)
	}
	body := func(content string) map[string]any {
		return map[string]any{
			"repository_id": repo.RepositoryID, "branch": "main", "commit_id": "commit-1",
			"files": []map[string]any{{"path": "pkg/a.go", "language": "go", "content_hash": hashContent(content), "content": content}},
		}
	}
	type indexOut struct {
		SnapshotID  string `json:"snapshot_id"`
		Unchanged   bool   `json:"unchanged"`
		SymbolCount int    `json:"symbol_count"`
	}
	decode := func(w *httptest.ResponseRecorder) indexOut {
		t.Helper()
		var out indexOut
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode %s: %v", w.Body.String(), err)
		}
		return out
	}
	src := "package pkg\n\nfunc A() {}\n"

	first := pgRequest(t, srv, http.MethodPost, "/v2/code/index", tenant, body(src))
	if first.Code != http.StatusOK {
		t.Fatalf("first index: %d: %s", first.Code, first.Body.String())
	}
	again := pgRequest(t, srv, http.MethodPost, "/v2/code/index", tenant, body(src))
	if again.Code != http.StatusOK {
		t.Fatalf("retry: %d: %s", again.Code, again.Body.String())
	}
	a, b := decode(first), decode(again)
	if b.SnapshotID != a.SnapshotID || !b.Unchanged || b.SymbolCount != a.SymbolCount || a.Unchanged {
		t.Fatalf("retry returned %+v, first %+v", b, a)
	}

	changed := pgRequest(t, srv, http.MethodPost, "/v2/code/index", tenant, body("package pkg\n\nfunc B() {}\n"))
	if changed.Code != http.StatusConflict || !strings.Contains(changed.Body.String(), a.SnapshotID) {
		t.Fatalf("different files under the same commit: %d: %s", changed.Code, changed.Body.String())
	}
}

// A declaration named the way people write it -- bare, qualified by its
// class, with a language keyword, or as a Go Type.Method -- anchors as an
// exact match on the declaration the indexer recorded.
func TestHandlePGQueryAnchorsDeclarationNames(t *testing.T) {
	_, db := testPGStore(t)
	srv := &server{pgStore: newPGCodeStore(db)}
	tenant := testTenant(t, db)

	reposResp := pgRequest(t, srv, http.MethodPost, "/v2/code/repos", tenant, map[string]any{"canonical_path": "/repo/" + t.Name()})
	var repo struct {
		RepositoryID string `json:"repository_id"`
	}
	if err := json.Unmarshal(reposResp.Body.Bytes(), &repo); err != nil {
		t.Fatal(err)
	}
	py := "class OutputReducer:\n    def reduce_output(self, text):\n        return text\n"
	gosrc := "package proxy\n\ntype server struct{}\n\nfunc (s *server) handleRun() {}\n"
	index := pgRequest(t, srv, http.MethodPost, "/v2/code/index", tenant, map[string]any{
		"repository_id": repo.RepositoryID, "branch": "main", "commit_id": "c1",
		"files": []map[string]any{
			{"path": "tools/reducer.py", "language": "python", "content_hash": hashContent(py), "content": py},
			{"path": "proxy/run.go", "language": "go", "content_hash": hashContent(gosrc), "content": gosrc},
		},
	})
	if index.Code != http.StatusOK {
		t.Fatalf("index: %d: %s", index.Code, index.Body.String())
	}

	for query, wantSuffix := range map[string]string{
		"OutputReducer":               ".OutputReducer",
		"class OutputReducer":         ".OutputReducer",
		"OutputReducer.reduce_output": ".OutputReducer.reduce_output",
		"def reduce_output":           ".OutputReducer.reduce_output",
		"server.handleRun":            ".(*server).handleRun",
		"func handleRun()":            ".(*server).handleRun",
	} {
		resp := pgRequest(t, srv, http.MethodPost, "/v2/code/query", tenant, map[string]any{
			"repository_id": repo.RepositoryID, "branch": "main", "query": query,
		})
		var out struct {
			Results       []PGSymbolVersion `json:"results"`
			RetrievalMode string            `json:"retrieval_mode"`
		}
		if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
			t.Fatalf("%q: %v: %s", query, err, resp.Body.String())
		}
		if out.RetrievalMode != "exact" || len(out.Results) != 1 || !strings.HasSuffix(out.Results[0].FullyQualifiedName, wantSuffix) {
			t.Errorf("%q: mode %q results %+v, want one exact match ending %q", query, out.RetrievalMode, out.Results, wantSuffix)
		}
	}
}

// Indexing a tree that an earlier snapshot already holds -- an edit was
// reverted, a branch moved back -- makes that snapshot the served one
// again; the snapshot that replaced it is never left serving.
func TestHandlePGIndexReactivatesARevertedTree(t *testing.T) {
	_, db := testPGStore(t)
	srv := &server{pgStore: newPGCodeStore(db)}
	tenant := testTenant(t, db)
	reposResp := pgRequest(t, srv, http.MethodPost, "/v2/code/repos", tenant, map[string]any{"canonical_path": "/repo/" + t.Name()})
	var repo struct {
		RepositoryID string `json:"repository_id"`
	}
	if err := json.Unmarshal(reposResp.Body.Bytes(), &repo); err != nil {
		t.Fatal(err)
	}
	index := func(commit, path, src string) string {
		t.Helper()
		w := pgRequest(t, srv, http.MethodPost, "/v2/code/index", tenant, map[string]any{
			"repository_id": repo.RepositoryID, "branch": "main", "commit_id": commit,
			"files": []map[string]any{{"path": path, "language": "go", "content_hash": hashContent(src), "content": src}},
		})
		if w.Code != http.StatusOK {
			t.Fatalf("index %s: %d: %s", commit, w.Code, w.Body.String())
		}
		var out struct {
			SnapshotID string `json:"snapshot_id"`
			Status     string `json:"status"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.SnapshotID
	}
	served := func(query string) []PGSymbolVersion {
		t.Helper()
		w := pgRequest(t, srv, http.MethodPost, "/v2/code/query", tenant, map[string]any{"repository_id": repo.RepositoryID, "branch": "main", "query": query})
		var out struct {
			Results []PGSymbolVersion `json:"results"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Results
	}

	original := index("tree-a", "pkg/a.go", "package pkg\n\nfunc Original() {}\n")
	edited := index("tree-b", "pkg/b.go", "package pkg\n\nfunc Edited() {}\n")
	if got := served("Edited"); len(got) != 1 || got[0].SnapshotID != edited {
		t.Fatalf("edit not served: %+v", got)
	}
	if again := index("tree-a", "pkg/a.go", "package pkg\n\nfunc Original() {}\n"); again != original {
		t.Fatalf("reverted tree got snapshot %s, want the original %s", again, original)
	}
	if got := served("Original"); len(got) != 1 || got[0].SnapshotID != original {
		t.Fatalf("reverted tree not served: %+v", got)
	}
	if got := served("Edited"); len(got) != 0 {
		t.Fatalf("the reverted edit is still served: %+v", got)
	}
}
