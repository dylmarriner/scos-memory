package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
// behave exactly as if it didn't exist -- the same rule
// TestHandleQueryPreventsCrossTenantLeakage enforces for the v1 store.
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
