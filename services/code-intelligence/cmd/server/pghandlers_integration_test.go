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
	r = r.WithContext(auth.ContextWithClaims(r.Context(), auth.Claims{TenantID: tenant, PrincipalID: "p"}))
	w := httptest.NewRecorder()
	switch path {
	case "/v2/code/repos":
		srv.handlePGRepos(w, r)
	case "/v2/code/index":
		srv.handlePGIndex(w, r)
	case "/v2/code/query":
		srv.handlePGQuery(w, r)
	default:
		t.Fatalf("unhandled path %s", path)
	}
	return w
}

func TestHandlePGRoutesReturnUnavailableWithoutStoreConfigured(t *testing.T) {
	srv := &server{}
	for _, path := range []string{"/v2/code/repos", "/v2/code/index", "/v2/code/query"} {
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
