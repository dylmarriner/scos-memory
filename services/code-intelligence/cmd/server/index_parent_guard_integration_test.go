package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// A Git delta cannot safely omit source if the chosen snapshot cannot
// reuse the exact same file from a compatible active parent. Failing the
// transaction must leave the old active snapshot and evidence untouched.
func TestPGDeltaReuseRequiresCompatibleParent(t *testing.T) {
	scenarios := []struct {
		name, parentExtractor, nextExtractor, parentConfig, nextConfig string
		nextPath, nextLanguage, expectedParent                         string
		changeHash, metadataOnly                                       bool
	}{
		{name: "different extractor", parentExtractor: "legacy-v1", nextExtractor: "go/parser-v1"},
		{name: "different configuration", parentExtractor: "go/parser-v1", nextExtractor: "go/parser-v1", parentConfig: "old-config", nextConfig: "new-config"},
		{name: "missing path", parentExtractor: "go/parser-v1", nextExtractor: "go/parser-v1", nextPath: "other.go"},
		{name: "different hash", parentExtractor: "go/parser-v1", nextExtractor: "go/parser-v1", changeHash: true},
		{name: "different language", parentExtractor: "go/parser-v1", nextExtractor: "go/parser-v1", nextLanguage: "python"},
		{name: "wrong parent commit", parentExtractor: "go/parser-v1", nextExtractor: "go/parser-v1", expectedParent: "unrelated"},
		{name: "metadata-only parent", parentExtractor: "go/parser-v1", nextExtractor: "go/parser-v1", metadataOnly: true},
	}
	for _, tc := range scenarios {
		t.Run(tc.name, func(t *testing.T) {
			store, db := testPGStore(t)
			ctx := context.Background()
			tenant := testTenant(t, db)
			repo, err := store.EnsureRepository(ctx, tenant, "/p3-guard/"+t.Name())
			if err != nil {
				t.Fatal(err)
			}
			content := "package demo\nfunc existing(){}\n"
			parent, err := store.CreateSnapshot(ctx, repo, tenant, "main", "before", tc.parentExtractor, tc.parentConfig)
			if err != nil {
				t.Fatal(err)
			}
			parentContent := content
			if tc.metadataOnly {
				parentContent = ""
			}
			if err := store.IndexSnapshot(ctx, parent, repo, []PGFileInput{{
				Path: "stable.go", Language: "go", Content: parentContent, ContentHash: hashContent(content),
			}}); err != nil {
				t.Fatal(err)
			}
			next, err := store.CreateSnapshot(ctx, repo, tenant, "main", "after", tc.nextExtractor, tc.nextConfig)
			if err != nil {
				t.Fatal(err)
			}
			path := tc.nextPath
			if path == "" {
				path = "stable.go"
			}
			language := tc.nextLanguage
			if language == "" {
				language = "go"
			}
			hash := hashContent(content)
			if tc.changeHash {
				hash = hashContent("different")
			}
			expected := tc.expectedParent
			if expected == "" {
				expected = "before"
			}
			err = store.IndexSnapshot(ctx, next, repo, []PGFileInput{{
				Path: path, Language: language, ContentHash: hash, ReuseParent: true, ExpectedParentCommit: expected,
			}})
			if !errors.Is(err, errSnapshotConflict) || !strings.Contains(err.Error(), "parent") {
				t.Fatalf("unsafe delta must fail closed, got %v", err)
			}
			var active string
			if err := db.QueryRowContext(ctx, "SELECT id::text FROM code_repository_snapshots WHERE repository_id=$1 AND branch='main' AND status='active'", repo).Scan(&active); err != nil {
				t.Fatal(err)
			}
			if active != parent {
				t.Fatalf("failed delta activated: got %s want %s", active, parent)
			}
			var partial int
			if err := db.QueryRowContext(ctx, "SELECT count(*) FROM code_file_versions WHERE snapshot_id=$1", next).Scan(&partial); err != nil {
				t.Fatal(err)
			}
			if partial != 0 {
				t.Fatalf("failed delta persisted %d file versions", partial)
			}
		})
	}
}

// Empty files have a provable zero-byte SHA-256 even without a persisted
// source chunk. Nonempty whitespace-only files must not be treated as empty.
func TestPGDeltaReuseEmptyFileWithoutChunk(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		allowReuse    bool
	}{
		{name: "empty", content: "", allowReuse: true},
		{name: "whitespace", content: "  \n\t", allowReuse: false},
		{name: "unicode whitespace", content: "\u00a0\u2003", allowReuse: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, db := testPGStore(t)
			ctx := context.Background()
			tenant := testTenant(t, db)
			repo, err := store.EnsureRepository(ctx, tenant, "/p3-empty/"+t.Name())
			if err != nil {
				t.Fatal(err)
			}
			parent, err := store.CreateSnapshot(ctx, repo, tenant, "main", "before", "go/parser-v1", "same-config")
			if err != nil {
				t.Fatal(err)
			}
			input := PGFileInput{
				Path: "package.py", Language: "python",
				Content: tc.content, ContentHash: hashContent(tc.content),
			}
			if err := store.IndexSnapshot(ctx, parent, repo, []PGFileInput{input}); err != nil {
				t.Fatal(err)
			}
			next, err := store.CreateSnapshot(ctx, repo, tenant, "main", "after", "go/parser-v1", "same-config")
			if err != nil {
				t.Fatal(err)
			}
			input.Content = ""
			input.ReuseParent = true
			input.ExpectedParentCommit = "before"
			err = store.IndexSnapshot(ctx, next, repo, []PGFileInput{input})
			if tc.allowReuse && err != nil {
				t.Fatalf("empty source should reuse safely: %v", err)
			}
			if !tc.allowReuse && !errors.Is(err, errSnapshotConflict) {
				t.Fatalf("nonempty source without persisted evidence must conflict: %v", err)
			}
			var active string
			if err := db.QueryRowContext(ctx,
				"SELECT commit_id FROM code_repository_snapshots WHERE repository_id=$1 AND branch='main' AND status='active'",
				repo).Scan(&active); err != nil {
				t.Fatal(err)
			}
			expected := "before"
			if tc.allowReuse {
				expected = "after"
			}
			if active != expected {
				t.Fatalf("active commit=%s want %s", active, expected)
			}
		})
	}
}

func TestPGDeltaReuseAcceptsMatchingParent(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenant := testTenant(t, db)
	repo, err := store.EnsureRepository(ctx, tenant, "/p3-guard/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	content := "package demo\nfunc existing(){}\n"
	parent, err := store.CreateSnapshot(ctx, repo, tenant, "main", "before", "go/parser-v1", "same-config")
	if err != nil {
		t.Fatal(err)
	}
	input := PGFileInput{Path: "stable.go", Language: "go", ContentHash: hashContent(content), Content: content}
	if err := store.IndexSnapshot(ctx, parent, repo, []PGFileInput{input}); err != nil {
		t.Fatal(err)
	}
	next, err := store.CreateSnapshot(ctx, repo, tenant, "main", "after", "go/parser-v1", "same-config")
	if err != nil {
		t.Fatal(err)
	}
	input.Content = ""
	input.ReuseParent = true
	input.ExpectedParentCommit = "before"
	if err := store.IndexSnapshot(ctx, next, repo, []PGFileInput{input}); err != nil {
		t.Fatal(err)
	}
	var symbols, sourceChunks int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM code_symbol_versions WHERE snapshot_id=$1", next).Scan(&symbols); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM code_chunks cc JOIN code_symbol_versions sv ON sv.id=cc.symbol_version_id WHERE sv.snapshot_id=$1 AND cc.content=$2", next, strings.TrimSpace(content)).Scan(&sourceChunks); err != nil {
		t.Fatal(err)
	}
	if symbols < 2 || sourceChunks == 0 {
		t.Fatalf("reused snapshot missing declarations or source evidence: symbols=%d chunks=%d", symbols, sourceChunks)
	}
}

// The HTTP contract must reject unproven omitted bytes, return a conflict
// for a stale parent, and then permit a retry with the exact active commit.
func TestHandlePGIndexDeltaRequiresExactParent(t *testing.T) {
	store, db := testPGStore(t)
	srv := &server{pgStore: store}
	ctx := context.Background()
	tenant := testTenant(t, db)
	repo, err := store.EnsureRepository(ctx, tenant, "/p3-http-guard/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	content := "package demo\nfunc existing(){}\n"
	base := map[string]any{
		"repository_id": repo, "branch": "main", "commit_id": "before",
		"files": []map[string]any{{"path": "stable.go", "language": "go", "content_hash": hashContent(content), "content": content}},
	}
	initial := pgRequest(t, srv, http.MethodPost, "/v2/code/index", tenant, base)
	if initial.Code != http.StatusOK {
		t.Fatalf("initial: %d %s", initial.Code, initial.Body.String())
	}
	next := map[string]any{
		"repository_id": repo, "branch": "main", "commit_id": "after",
		"files": []map[string]any{{"path": "stable.go", "language": "go", "content_hash": hashContent(content), "reuse_parent": true}},
	}
	missing := pgRequest(t, srv, http.MethodPost, "/v2/code/index", tenant, next)
	if missing.Code != http.StatusBadRequest {
		t.Fatalf("missing parent: %d %s", missing.Code, missing.Body.String())
	}
	next["parent_commit_id"] = "unrelated"
	stale := pgRequest(t, srv, http.MethodPost, "/v2/code/index", tenant, next)
	if stale.Code != http.StatusConflict {
		t.Fatalf("wrong parent: %d %s", stale.Code, stale.Body.String())
	}
	var active string
	if err := db.QueryRowContext(ctx, "SELECT commit_id FROM code_repository_snapshots WHERE repository_id=$1 AND branch='main' AND status='active'", repo).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != "before" {
		t.Fatalf("stale delta changed active commit: %s", active)
	}
	next["parent_commit_id"] = "before"
	success := pgRequest(t, srv, http.MethodPost, "/v2/code/index", tenant, next)
	if success.Code != http.StatusOK {
		t.Fatalf("valid delta: %d %s", success.Code, success.Body.String())
	}
	if err := db.QueryRowContext(ctx, "SELECT commit_id FROM code_repository_snapshots WHERE repository_id=$1 AND branch='main' AND status='active'", repo).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != "after" {
		t.Fatalf("valid delta did not activate: %s", active)
	}
}

func TestHandlePGIndexFullSourceFallbackAfterExtractorUpgrade(t *testing.T) {
	store, db := testPGStore(t)
	srv := &server{pgStore: store}
	ctx := context.Background()
	tenant := testTenant(t, db)
	repo, err := store.EnsureRepository(ctx, tenant, "/p3-upgrade/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	content := "package demo\nfunc existing(){}\n"
	parent, err := store.CreateSnapshot(ctx, repo, tenant, "main", "before", "old-extractor", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, parent, repo, []PGFileInput{{
		Path: "stable.go", Language: "go", Content: content, ContentHash: hashContent(content),
	}}); err != nil {
		t.Fatal(err)
	}
	delta := map[string]any{
		"repository_id": repo, "branch": "main", "commit_id": "after", "parent_commit_id": "before",
		"files": []map[string]any{{"path": "stable.go", "language": "go", "content_hash": hashContent(content), "reuse_parent": true}},
	}
	rejected := pgRequest(t, srv, http.MethodPost, "/v2/code/index", tenant, delta)
	if rejected.Code != http.StatusConflict {
		t.Fatalf("incompatible delta: %d %s", rejected.Code, rejected.Body.String())
	}
	delta["files"] = []map[string]any{{"path": "stable.go", "language": "go", "content_hash": hashContent(content), "content": content}}
	accepted := pgRequest(t, srv, http.MethodPost, "/v2/code/index", tenant, delta)
	if accepted.Code != http.StatusOK {
		t.Fatalf("full-source fallback: %d %s", accepted.Code, accepted.Body.String())
	}
	var active string
	if err := db.QueryRowContext(ctx, "SELECT id::text FROM code_repository_snapshots WHERE repository_id=$1 AND branch='main' AND status='active'", repo).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active == parent {
		t.Fatal("full-source fallback did not activate new extractor snapshot")
	}
	var declarations, chunks int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM code_symbol_versions WHERE snapshot_id=$1 AND kind='function'", active).Scan(&declarations); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM code_chunks cc JOIN code_symbol_versions sv ON sv.id=cc.symbol_version_id WHERE sv.snapshot_id=$1 AND sv.kind='file'", active).Scan(&chunks); err != nil {
		t.Fatal(err)
	}
	if declarations == 0 || chunks == 0 {
		t.Fatalf("full-source fallback missing declarations/evidence: %d/%d", declarations, chunks)
	}
}
