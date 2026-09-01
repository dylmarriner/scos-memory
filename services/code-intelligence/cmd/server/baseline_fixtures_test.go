package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

// Phase 0 ground truth (SCOS_MEMORY_TECHNICAL_BLUEPRINT_AND_ENGINEERING_ROADMAP.md
// §31): capture the current heuristic/JSON-map code-intelligence store's actual
// behavior on the fixtures the blueprint names -- exact symbol resolution,
// renamed symbol, deleted file, branch divergence -- BEFORE it is replaced by
// the durable snapshot store the same document calls for. Each test's name
// says which side of "does this already work" its assertion documents: a
// "Baseline" test that currently asserts a gap is expected to have its
// assertion flipped (not deleted) once the corresponding rewrite phase lands,
// so this file also serves as the completion gate that rewrite has to clear.
//
// (The other two fixtures the blueprint names -- stale Graphify cache and
// tenant isolation -- already have dedicated regression suites:
// services/graphify-provider/cmd/server/workdir_test.go and the
// tenant_isolation_test.go files across the fleet.)
//
// Both gaps below are now fixed in pgstore.go's durable, snapshot-based
// store, proven by pgstore_integration_test.go's
// TestPGRenamePreservesSymbolIdentity and TestPGBranchesCoexistIndependently
// against a real PostgreSQL database. The assertions here are intentionally
// NOT flipped: they document the in-memory codeStore's actual behavior on
// the /v1 API, which is genuinely unchanged -- the durable store is wired
// in additively as /v2/code/* (pghandlers.go), opt-in via SCOS_DATABASE_URL,
// specifically because the /v1 wire protocol (bare path strings, no content
// hash) cannot supply what rename detection needs. These assertions should
// flip only once /v1 itself is migrated onto the durable store.

func indexRequest(t *testing.T, srv *server, tenant, repoID, commitID string, files []string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"repository_id": repoID,
		"commit_id":     commitID,
		"files":         files,
	})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/index", bytes.NewReader(body))
	r = r.WithContext(auth.ContextWithClaims(r.Context(), auth.Claims{TenantID: tenant, PrincipalID: "p"}))
	w := httptest.NewRecorder()
	srv.handleIndex(w, r)
	return w
}

func queryRequest(t *testing.T, srv *server, tenant string, q CodeQuery) []CodeQueryResult {
	t.Helper()
	body, err := json.Marshal(q)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/query", bytes.NewReader(body))
	r = r.WithContext(auth.ContextWithClaims(r.Context(), auth.Claims{TenantID: tenant, PrincipalID: "p"}))
	w := httptest.NewRecorder()
	srv.handleQuery(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("query failed: %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Results []CodeQueryResult `json:"results"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp.Results
}

func newBaselineServer(repoID, tenant string) *server {
	store := newCodeStore()
	store.repos[repoID] = &Repository{ID: repoID, TenantID: tenant, CanonicalPath: "/repo"}
	return &server{store: store}
}

// Baseline: exact symbol resolution already works for the file-level (tier
// 0) symbol every indexed file gets, since its ID and FQN are derived
// directly from the path.
func TestBaselineExactSymbolResolutionWorks(t *testing.T) {
	srv := newBaselineServer("repo-1", "tenant-a")
	if w := indexRequest(t, srv, "tenant-a", "repo-1", "c1", []string{"pkg/handler.go"}); w.Code != http.StatusOK {
		t.Fatalf("index failed: %d: %s", w.Code, w.Body.String())
	}

	results := queryRequest(t, srv, "tenant-a", CodeQuery{Intent: "exact_lookup", Query: "pkg/handler.go", MaxResults: 10})
	if len(results) == 0 {
		t.Fatal("expected the file-level symbol to resolve by its exact path/FQN")
	}
}

// Baseline gap: a renamed file is indistinguishable from a deleted file plus
// an unrelated new one. Symbol IDs and content hashes are derived from
// symbolScope(repoID, path) -- a hash of the *path*, not the file's actual
// content -- so a rename produces a brand-new symbol ID with zero lineage to
// the old one, even though nothing about the underlying code changed. This
// is precisely the gap blueprint §5.2 (stable conceptual identity vs.
// snapshot occurrence) and §35 (rename/move lineage) exist to close.
func TestBaselineRenameLosesSymbolLineage(t *testing.T) {
	srv := newBaselineServer("repo-1", "tenant-a")
	indexRequest(t, srv, "tenant-a", "repo-1", "c1", []string{"pkg/old_name.go"})
	before := queryRequest(t, srv, "tenant-a", CodeQuery{Intent: "exact_lookup", Query: "pkg/old_name.go", MaxResults: 10})
	if len(before) == 0 {
		t.Fatal("expected the pre-rename symbol to resolve")
	}
	beforeID := before[0].Symbol.ID

	// Same file, same content, renamed path -- the only real-world signal a
	// rename detector would need is that the content is unchanged.
	indexRequest(t, srv, "tenant-a", "repo-1", "c2", []string{"pkg/new_name.go"})
	after := queryRequest(t, srv, "tenant-a", CodeQuery{Intent: "exact_lookup", Query: "pkg/new_name.go", MaxResults: 10})
	if len(after) == 0 {
		t.Fatal("expected the post-rename symbol to resolve under its new path")
	}

	if after[0].Symbol.ID == beforeID {
		t.Fatal("rename lineage is preserved now -- flip this assertion and remove this comment: " +
			"the gap this test documented (blueprint §35) has been closed")
	}
	// Document what "no lineage" concretely means: the old path is no longer
	// resolvable at all after the rename, with nothing connecting the two.
	stale := queryRequest(t, srv, "tenant-a", CodeQuery{Intent: "exact_lookup", Query: "pkg/old_name.go", MaxResults: 10})
	if len(stale) != 0 {
		t.Fatal("expected the pre-rename path to no longer resolve post-rename")
	}
}

// Baseline: this part already works correctly. Re-indexing with a file
// dropped from the file list does remove that file's symbols -- handleIndex
// builds each generation as a complete replacement for the repo rather than
// an accumulation (§19.3-§19.4 in the code, matching blueprint §7's "no
// partially built snapshot" principle applied to deletion too).
func TestBaselineDeletedFileRemovesItsSymbols(t *testing.T) {
	srv := newBaselineServer("repo-1", "tenant-a")
	indexRequest(t, srv, "tenant-a", "repo-1", "c1", []string{"pkg/a.go", "pkg/b.go"})
	if got := queryRequest(t, srv, "tenant-a", CodeQuery{Intent: "exact_lookup", Query: "pkg/b.go", MaxResults: 10}); len(got) == 0 {
		t.Fatal("expected pkg/b.go to resolve before it's removed from the file list")
	}

	indexRequest(t, srv, "tenant-a", "repo-1", "c2", []string{"pkg/a.go"})
	if got := queryRequest(t, srv, "tenant-a", CodeQuery{Intent: "exact_lookup", Query: "pkg/b.go", MaxResults: 10}); len(got) != 0 {
		t.Fatalf("expected pkg/b.go's symbols to be gone after it dropped out of the file list, got %d result(s)", len(got))
	}
	if got := queryRequest(t, srv, "tenant-a", CodeQuery{Intent: "exact_lookup", Query: "pkg/a.go", MaxResults: 10}); len(got) == 0 {
		t.Fatal("expected pkg/a.go to remain resolvable")
	}
}

// Baseline gap: there is no snapshot/branch isolation at all. A Repository
// carries one mutable CommitID and one mutable IndexGeneration; indexing a
// second commit for the same repository_id overwrites the first in place,
// so the two commits' symbol sets can never coexist. This is blueprint §8's
// "an active snapshot is immutable; a new commit produces another snapshot"
// -- the current store does the opposite of that by construction, which is
// also why two branches sharing overlapping paths cannot both be queried.
func TestBaselineReindexDestroysPriorCommitState(t *testing.T) {
	srv := newBaselineServer("repo-1", "tenant-a")
	indexRequest(t, srv, "tenant-a", "repo-1", "commit-a", []string{"pkg/only_in_a.go"})
	if got := queryRequest(t, srv, "tenant-a", CodeQuery{Intent: "exact_lookup", Query: "pkg/only_in_a.go", MaxResults: 10}); len(got) == 0 {
		t.Fatal("expected commit-a's symbol to resolve while commit-a is indexed")
	}

	// A second commit for the *same* repository_id -- what indexing a new
	// branch or a later commit on the same branch both look like today, since
	// there is no separate snapshot identity to index into.
	indexRequest(t, srv, "tenant-a", "repo-1", "commit-b", []string{"pkg/only_in_b.go"})

	stillA := queryRequest(t, srv, "tenant-a", CodeQuery{Intent: "exact_lookup", Query: "pkg/only_in_a.go", MaxResults: 10})
	if len(stillA) != 0 {
		t.Fatal("commit-a's state survived indexing commit-b -- flip this assertion and remove this comment: " +
			"the gap this test documented (blueprint §8/§33 snapshot model) has been closed")
	}
	if got := queryRequest(t, srv, "tenant-a", CodeQuery{Intent: "exact_lookup", Query: "pkg/only_in_b.go", MaxResults: 10}); len(got) == 0 {
		t.Fatal("expected commit-b's symbol to resolve while commit-b is indexed")
	}
}
