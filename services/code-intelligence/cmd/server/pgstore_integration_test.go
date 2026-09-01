package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// These tests exercise pgCodeStore against a real PostgreSQL database with
// migration 000025_code_intelligence_persistence.sql applied. They are
// integration tests, not unit tests: nothing in this package mocks SQL
// execution (no other Postgres-backed service in this fleet does either),
// so the only way to know the schema and the queries against it actually
// agree is to run them against a real engine.
//
// Set SCOS_CODE_STORE_TEST_DSN to a postgres:// URL for a disposable
// database with the migrations already applied to run these; they skip
// otherwise. Example, against a local throwaway container:
//
//	docker run -d -e POSTGRES_PASSWORD=test -e POSTGRES_DB=scos_test \
//	  -p 15432:5432 pgvector/pgvector:pg16
//	for f in database/migrations/*.sql; do
//	  psql postgres://postgres:test@localhost:15432/scos_test -f "$f"
//	done
//	SCOS_CODE_STORE_TEST_DSN=postgres://postgres:test@localhost:15432/scos_test \
//	  go test ./services/code-intelligence/... -run TestPG -v

func testPGStore(t *testing.T) (*pgCodeStore, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("SCOS_CODE_STORE_TEST_DSN")
	if dsn == "" {
		t.Skip("SCOS_CODE_STORE_TEST_DSN not set; skipping pgCodeStore integration test")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(context.Background()); err != nil {
		t.Fatalf("ping test database: %v", err)
	}
	return newPGCodeStore(db), db
}

// testTenant inserts a throwaway tenant row (code_repositories.tenant_id
// has a NOT NULL FK to tenants(id)) and returns its id.
func testTenant(t *testing.T, db *sql.DB) string {
	t.Helper()
	var id string
	if err := db.QueryRowContext(context.Background(),
		`INSERT INTO tenants (name) VALUES ($1) RETURNING id`, t.Name()).Scan(&id); err != nil {
		t.Fatalf("create test tenant: %v", err)
	}
	return id
}

func hashContent(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func TestPGExactSymbolResolution(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenantID := testTenant(t, db)

	repoID, err := store.EnsureRepository(ctx, tenantID, "/repo/"+t.Name())
	if err != nil {
		t.Fatalf("ensure repository: %v", err)
	}
	snapshotID, err := store.CreateSnapshot(ctx, repoID, tenantID, "main", "commit-1", "heuristic-v1", "")
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}
	if err := store.IndexSnapshot(ctx, snapshotID, repoID, []PGFileInput{
		{Path: "pkg/handler.go", Language: "go", ContentHash: hashContent("package pkg\nfunc Handler() {}\n")},
	}); err != nil {
		t.Fatalf("index snapshot: %v", err)
	}

	results, err := store.QueryByPathOrFQN(ctx, repoID, "main", "pkg/handler.go")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected exactly one result, got %d: %+v", len(results), results)
	}
	if results[0].CommitID != "commit-1" {
		t.Fatalf("expected commit-1, got %q", results[0].CommitID)
	}
}

// TestPGRenamePreservesSymbolIdentity is the fix for
// TestBaselineRenameLosesSymbolLineage: a file's stable identity is keyed
// on its content hash, so renaming it without changing its content
// resolves to the *same* code_symbols.id under the new path, and the
// query result reports where it was renamed from.
func TestPGRenamePreservesSymbolIdentity(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenantID := testTenant(t, db)
	content := hashContent("package pkg\nfunc Unchanged() {}\n")

	repoID, err := store.EnsureRepository(ctx, tenantID, "/repo/"+t.Name())
	if err != nil {
		t.Fatalf("ensure repository: %v", err)
	}

	snap1, err := store.CreateSnapshot(ctx, repoID, tenantID, "main", "commit-1", "heuristic-v1", "")
	if err != nil {
		t.Fatalf("create snapshot 1: %v", err)
	}
	if err := store.IndexSnapshot(ctx, snap1, repoID, []PGFileInput{
		{Path: "pkg/old_name.go", Language: "go", ContentHash: content},
	}); err != nil {
		t.Fatalf("index snapshot 1: %v", err)
	}
	before, err := store.QueryByPathOrFQN(ctx, repoID, "main", "pkg/old_name.go")
	if err != nil || len(before) != 1 {
		t.Fatalf("expected the pre-rename symbol to resolve, got %v (err %v)", before, err)
	}
	beforeSymbolID := before[0].SymbolID

	snap2, err := store.CreateSnapshot(ctx, repoID, tenantID, "main", "commit-2", "heuristic-v1", "")
	if err != nil {
		t.Fatalf("create snapshot 2: %v", err)
	}
	if err := store.IndexSnapshot(ctx, snap2, repoID, []PGFileInput{
		{Path: "pkg/new_name.go", Language: "go", ContentHash: content},
	}); err != nil {
		t.Fatalf("index snapshot 2: %v", err)
	}

	after, err := store.QueryByPathOrFQN(ctx, repoID, "main", "pkg/new_name.go")
	if err != nil || len(after) != 1 {
		t.Fatalf("expected the post-rename symbol to resolve under its new path, got %v (err %v)", after, err)
	}
	if after[0].SymbolID != beforeSymbolID {
		t.Fatalf("expected the renamed file to keep its symbol identity: before=%s after=%s", beforeSymbolID, after[0].SymbolID)
	}
	if after[0].RenamedFromPath != "pkg/old_name.go" {
		t.Fatalf("expected renamed_from_path to report the previous path, got %q", after[0].RenamedFromPath)
	}

	stale, err := store.QueryByPathOrFQN(ctx, repoID, "main", "pkg/old_name.go")
	if err != nil {
		t.Fatalf("query stale path: %v", err)
	}
	if len(stale) != 0 {
		t.Fatalf("expected the pre-rename path to no longer resolve in the active snapshot, got %v", stale)
	}
}

// TestPGBranchesCoexistIndependently is the fix for
// TestBaselineReindexDestroysPriorCommitState: indexing a second commit on
// a different branch does not disturb the first branch's active snapshot.
func TestPGBranchesCoexistIndependently(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenantID := testTenant(t, db)

	repoID, err := store.EnsureRepository(ctx, tenantID, "/repo/"+t.Name())
	if err != nil {
		t.Fatalf("ensure repository: %v", err)
	}

	mainSnap, err := store.CreateSnapshot(ctx, repoID, tenantID, "main", "commit-main", "heuristic-v1", "")
	if err != nil {
		t.Fatalf("create main snapshot: %v", err)
	}
	if err := store.IndexSnapshot(ctx, mainSnap, repoID, []PGFileInput{
		{Path: "pkg/only_in_main.go", Language: "go", ContentHash: hashContent("main")},
	}); err != nil {
		t.Fatalf("index main: %v", err)
	}

	featureSnap, err := store.CreateSnapshot(ctx, repoID, tenantID, "feature-x", "commit-feature", "heuristic-v1", "")
	if err != nil {
		t.Fatalf("create feature snapshot: %v", err)
	}
	if err := store.IndexSnapshot(ctx, featureSnap, repoID, []PGFileInput{
		{Path: "pkg/only_in_feature.go", Language: "go", ContentHash: hashContent("feature")},
	}); err != nil {
		t.Fatalf("index feature: %v", err)
	}

	mainResults, err := store.QueryByPathOrFQN(ctx, repoID, "main", "pkg/only_in_main.go")
	if err != nil || len(mainResults) != 1 {
		t.Fatalf("expected main's file to still resolve on main after indexing a different branch, got %v (err %v)", mainResults, err)
	}
	featureResults, err := store.QueryByPathOrFQN(ctx, repoID, "feature-x", "pkg/only_in_feature.go")
	if err != nil || len(featureResults) != 1 {
		t.Fatalf("expected feature-x's file to resolve on feature-x, got %v (err %v)", featureResults, err)
	}
	// Cross-branch isolation: feature-x's file must not leak into main's
	// active snapshot query.
	crossLeak, err := store.QueryByPathOrFQN(ctx, repoID, "main", "pkg/only_in_feature.go")
	if err != nil {
		t.Fatalf("cross-branch query: %v", err)
	}
	if len(crossLeak) != 0 {
		t.Fatalf("expected feature-x's file not to resolve on main, got %v", crossLeak)
	}
}

// A second commit on the *same* branch must supersede the first: this is
// the deliberate exception to "snapshots coexist" -- the active pointer
// only ever names the latest generation for a given branch, matching
// blueprint §8.
func TestPGSameBranchReindexSupersedesPriorCommit(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenantID := testTenant(t, db)

	repoID, err := store.EnsureRepository(ctx, tenantID, "/repo/"+t.Name())
	if err != nil {
		t.Fatalf("ensure repository: %v", err)
	}
	snapA, err := store.CreateSnapshot(ctx, repoID, tenantID, "main", "commit-a", "heuristic-v1", "")
	if err != nil {
		t.Fatalf("create snapshot a: %v", err)
	}
	if err := store.IndexSnapshot(ctx, snapA, repoID, []PGFileInput{
		{Path: "pkg/a.go", Language: "go", ContentHash: hashContent("a")},
	}); err != nil {
		t.Fatalf("index a: %v", err)
	}
	snapB, err := store.CreateSnapshot(ctx, repoID, tenantID, "main", "commit-b", "heuristic-v1", "")
	if err != nil {
		t.Fatalf("create snapshot b: %v", err)
	}
	if err := store.IndexSnapshot(ctx, snapB, repoID, []PGFileInput{
		{Path: "pkg/b.go", Language: "go", ContentHash: hashContent("b")},
	}); err != nil {
		t.Fatalf("index b: %v", err)
	}

	active, err := store.ActiveSnapshotID(ctx, repoID, "main")
	if err != nil {
		t.Fatalf("active snapshot: %v", err)
	}
	if active != snapB {
		t.Fatalf("expected commit-b's snapshot to be active, got %s want %s", active, snapB)
	}
	stale, err := store.QueryByPathOrFQN(ctx, repoID, "main", "pkg/a.go")
	if err != nil {
		t.Fatalf("query stale: %v", err)
	}
	if len(stale) != 0 {
		t.Fatalf("expected commit-a's file to no longer resolve once commit-b is active, got %v", stale)
	}
}

func TestPGCreateSnapshotIsIdempotentForSameIdentity(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenantID := testTenant(t, db)

	repoID, err := store.EnsureRepository(ctx, tenantID, "/repo/"+t.Name())
	if err != nil {
		t.Fatalf("ensure repository: %v", err)
	}
	id1, err := store.CreateSnapshot(ctx, repoID, tenantID, "main", "commit-1", "heuristic-v1", "cfg-hash")
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}
	id2, err := store.CreateSnapshot(ctx, repoID, tenantID, "main", "commit-1", "heuristic-v1", "cfg-hash")
	if err != nil {
		t.Fatalf("create snapshot again: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("expected identical (branch, commit, extractor, config) to return the same snapshot, got %s and %s", id1, id2)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM code_repository_snapshots WHERE id = $1`, id1).Scan(&count); err != nil {
		t.Fatalf("count snapshots: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly one snapshot row, got %d", count)
	}
}
