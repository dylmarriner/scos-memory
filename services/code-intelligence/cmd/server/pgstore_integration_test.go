package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"strings"
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

// TestPGLexicalSearchFindsSubstringMatches proves migration
// 000026_code_lexical_search.sql's FTS column and index actually work end
// to end: a query that is not an exact path/FQN (so QueryByPathOrFQN would
// return nothing) but shares a meaningful token with an indexed symbol's
// path/FQN is still found via full-text search.
func TestPGLexicalSearchFindsSubstringMatches(t *testing.T) {
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
		{Path: "internal/auth/middleware.go", Language: "go", ContentHash: hashContent("auth")},
		{Path: "internal/billing/invoice.go", Language: "go", ContentHash: hashContent("billing")},
	}); err != nil {
		t.Fatalf("index snapshot: %v", err)
	}

	exact, err := store.QueryByPathOrFQN(ctx, repoID, "main", "middleware")
	if err != nil {
		t.Fatalf("exact query: %v", err)
	}
	if len(exact) != 0 {
		t.Fatalf("expected no exact match for a bare substring, got %v", exact)
	}

	lexical, err := store.QueryLexical(ctx, repoID, "main", "middleware", 10)
	if err != nil {
		t.Fatalf("lexical query: %v", err)
	}
	if len(lexical) != 1 || lexical[0].Path != "internal/auth/middleware.go" {
		t.Fatalf("expected exactly the middleware file, got %+v", lexical)
	}
	if lexical[0].MatchType != "lexical" {
		t.Fatalf("expected MatchType lexical, got %q", lexical[0].MatchType)
	}

	noise, err := store.QueryLexical(ctx, repoID, "main", "nonexistent_token_xyz", 10)
	if err != nil {
		t.Fatalf("lexical query for no match: %v", err)
	}
	if len(noise) != 0 {
		t.Fatalf("expected no matches for an unrelated term, got %v", noise)
	}
}

func findDiffEntry(entries []SnapshotDiffEntry, path string) (SnapshotDiffEntry, bool) {
	for _, e := range entries {
		if e.Path == path {
			return e, true
		}
	}
	return SnapshotDiffEntry{}, false
}

// TestPGDiffSnapshotClassifiesEveryChangeKind exercises all four diff
// classifications (added, modified, removed, renamed) in one pair of
// snapshots, and confirms an unchanged file is reported in none of them --
// the set blueprint §35's "skip unchanged files" step would act on.
func TestPGDiffSnapshotClassifiesEveryChangeKind(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenantID := testTenant(t, db)

	repoID, err := store.EnsureRepository(ctx, tenantID, "/repo/"+t.Name())
	if err != nil {
		t.Fatalf("ensure repository: %v", err)
	}

	snap1, err := store.CreateSnapshot(ctx, repoID, tenantID, "main", "commit-1", "heuristic-v1", "")
	if err != nil {
		t.Fatalf("create snapshot 1: %v", err)
	}
	if err := store.IndexSnapshot(ctx, snap1, repoID, []PGFileInput{
		{Path: "pkg/unchanged.go", ContentHash: hashContent("unchanged")},
		{Path: "pkg/to_modify.go", ContentHash: hashContent("v1")},
		{Path: "pkg/old_name.go", ContentHash: hashContent("rename-me")},
		{Path: "pkg/to_remove.go", ContentHash: hashContent("goodbye")},
	}); err != nil {
		t.Fatalf("index snapshot 1: %v", err)
	}

	snap2, err := store.CreateSnapshot(ctx, repoID, tenantID, "main", "commit-2", "heuristic-v1", "")
	if err != nil {
		t.Fatalf("create snapshot 2: %v", err)
	}
	if err := store.IndexSnapshot(ctx, snap2, repoID, []PGFileInput{
		{Path: "pkg/unchanged.go", ContentHash: hashContent("unchanged")},
		{Path: "pkg/to_modify.go", ContentHash: hashContent("v2")},
		{Path: "pkg/new_name.go", ContentHash: hashContent("rename-me")},
		{Path: "pkg/newly_added.go", ContentHash: hashContent("brand new")},
	}); err != nil {
		t.Fatalf("index snapshot 2: %v", err)
	}

	diff, err := store.DiffSnapshot(ctx, snap2)
	if err != nil {
		t.Fatalf("diff snapshot: %v", err)
	}
	if diff.ParentSnapshotID != snap1 {
		t.Fatalf("expected parent snapshot to be snap1, got %q", diff.ParentSnapshotID)
	}

	if _, ok := findDiffEntry(diff.Added, "pkg/newly_added.go"); !ok {
		t.Fatalf("expected pkg/newly_added.go in Added, got %+v", diff.Added)
	}
	if len(diff.Added) != 1 {
		t.Fatalf("expected exactly one Added entry, got %+v", diff.Added)
	}

	if _, ok := findDiffEntry(diff.Modified, "pkg/to_modify.go"); !ok {
		t.Fatalf("expected pkg/to_modify.go in Modified, got %+v", diff.Modified)
	}
	if len(diff.Modified) != 1 {
		t.Fatalf("expected exactly one Modified entry, got %+v", diff.Modified)
	}

	if _, ok := findDiffEntry(diff.Removed, "pkg/to_remove.go"); !ok {
		t.Fatalf("expected pkg/to_remove.go in Removed, got %+v", diff.Removed)
	}
	if len(diff.Removed) != 1 {
		t.Fatalf("expected exactly one Removed entry, got %+v", diff.Removed)
	}

	renamed, ok := findDiffEntry(diff.Renamed, "pkg/new_name.go")
	if !ok {
		t.Fatalf("expected pkg/new_name.go in Renamed, got %+v", diff.Renamed)
	}
	if renamed.PreviousPath != "pkg/old_name.go" {
		t.Fatalf("expected renamed entry's previous path to be pkg/old_name.go, got %q", renamed.PreviousPath)
	}
	if len(diff.Renamed) != 1 {
		t.Fatalf("expected exactly one Renamed entry, got %+v", diff.Renamed)
	}

	for _, entries := range [][]SnapshotDiffEntry{diff.Added, diff.Modified, diff.Removed, diff.Renamed} {
		if _, ok := findDiffEntry(entries, "pkg/unchanged.go"); ok {
			t.Fatalf("expected pkg/unchanged.go to appear in no diff category, found in %+v", entries)
		}
	}
}

// The very first snapshot for a repository has no parent, so everything in
// it is Added -- there is nothing to diff against.
func TestPGDiffSnapshotWithNoParentReportsEverythingAdded(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenantID := testTenant(t, db)

	repoID, err := store.EnsureRepository(ctx, tenantID, "/repo/"+t.Name())
	if err != nil {
		t.Fatalf("ensure repository: %v", err)
	}
	snap, err := store.CreateSnapshot(ctx, repoID, tenantID, "main", "commit-1", "heuristic-v1", "")
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}
	if err := store.IndexSnapshot(ctx, snap, repoID, []PGFileInput{
		{Path: "pkg/a.go", ContentHash: hashContent("a")},
		{Path: "pkg/b.go", ContentHash: hashContent("b")},
	}); err != nil {
		t.Fatalf("index snapshot: %v", err)
	}

	diff, err := store.DiffSnapshot(ctx, snap)
	if err != nil {
		t.Fatalf("diff snapshot: %v", err)
	}
	if diff.ParentSnapshotID != "" {
		t.Fatalf("expected no parent snapshot, got %q", diff.ParentSnapshotID)
	}
	if len(diff.Added) != 2 || len(diff.Modified) != 0 || len(diff.Removed) != 0 || len(diff.Renamed) != 0 {
		t.Fatalf("expected everything Added and nothing else, got %+v", diff)
	}
}

// CreateSnapshot must link parent_snapshot_id to whatever was active for
// the branch at creation time, and must leave that link alone on a repeat
// call for the same (branch, commit, extractor, config) identity rather
// than resetting it.
func TestPGCreateSnapshotLinksParent(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenantID := testTenant(t, db)

	repoID, err := store.EnsureRepository(ctx, tenantID, "/repo/"+t.Name())
	if err != nil {
		t.Fatalf("ensure repository: %v", err)
	}
	snap1, err := store.CreateSnapshot(ctx, repoID, tenantID, "main", "commit-1", "heuristic-v1", "")
	if err != nil {
		t.Fatalf("create snapshot 1: %v", err)
	}
	if err := store.IndexSnapshot(ctx, snap1, repoID, []PGFileInput{{Path: "a.go", ContentHash: hashContent("a")}}); err != nil {
		t.Fatalf("index snapshot 1: %v", err)
	}

	snap2, err := store.CreateSnapshot(ctx, repoID, tenantID, "main", "commit-2", "heuristic-v1", "")
	if err != nil {
		t.Fatalf("create snapshot 2: %v", err)
	}
	var parent sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT parent_snapshot_id FROM code_repository_snapshots WHERE id = $1`, snap2).Scan(&parent); err != nil {
		t.Fatalf("load parent: %v", err)
	}
	if !parent.Valid || parent.String != snap1 {
		t.Fatalf("expected snap2's parent to be snap1, got %+v", parent)
	}
}

// A user with no recorded preference is enabled by default -- the store
// always runs, and this table only ever records an explicit opt-out.
func TestPGIsUserEnabledDefaultsTrueWithNoRow(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenantID := testTenant(t, db)

	enabled, err := store.IsUserEnabled(ctx, tenantID, "principal-with-no-row")
	if err != nil {
		t.Fatalf("is user enabled: %v", err)
	}
	if !enabled {
		t.Fatal("expected a user with no recorded preference to be enabled by default")
	}
}

func TestPGSetUserEnabledRoundTrips(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenantID := testTenant(t, db)
	principalID := "principal-a"

	if err := store.SetUserEnabled(ctx, tenantID, principalID, false); err != nil {
		t.Fatalf("set disabled: %v", err)
	}
	enabled, err := store.IsUserEnabled(ctx, tenantID, principalID)
	if err != nil {
		t.Fatalf("is user enabled: %v", err)
	}
	if enabled {
		t.Fatal("expected the user to be disabled after SetUserEnabled(false)")
	}

	if err := store.SetUserEnabled(ctx, tenantID, principalID, true); err != nil {
		t.Fatalf("set enabled: %v", err)
	}
	enabled, err = store.IsUserEnabled(ctx, tenantID, principalID)
	if err != nil {
		t.Fatalf("is user enabled: %v", err)
	}
	if !enabled {
		t.Fatal("expected the user to be enabled again after SetUserEnabled(true)")
	}
}

// One user's preference must never affect another's, or another tenant's
// user with the same principal_id string.
func TestPGUserSettingsAreIsolatedPerTenantAndPrincipal(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenantA := testTenant(t, db)
	tenantB := testTenant(t, db)

	if err := store.SetUserEnabled(ctx, tenantA, "shared-principal-id", false); err != nil {
		t.Fatalf("disable for tenant A: %v", err)
	}

	enabledOtherPrincipalSameTenant, err := store.IsUserEnabled(ctx, tenantA, "different-principal")
	if err != nil {
		t.Fatalf("check other principal: %v", err)
	}
	if !enabledOtherPrincipalSameTenant {
		t.Fatal("expected a different principal in the same tenant to be unaffected")
	}

	enabledSamePrincipalOtherTenant, err := store.IsUserEnabled(ctx, tenantB, "shared-principal-id")
	if err != nil {
		t.Fatalf("check same principal id in other tenant: %v", err)
	}
	if !enabledSamePrincipalOtherTenant {
		t.Fatal("expected the same principal_id string in a different tenant to be unaffected")
	}
}

const pgTestGoSource = `package sample

// CreateUser creates a user.
func CreateUser(name string) error {
	return nil
}

type UserService struct{}

func (s *UserService) Close() error {
	return nil
}
`

// TestPGIndexSnapshotExtractsRealGoDeclarations proves the whole path end
// to end: a Go file indexed with its actual source produces real Tier 1
// symbols (goextract.go) queryable by their fully-qualified name, in
// addition to -- not instead of -- the Tier 0 file-level symbol every
// language gets.
func TestPGIndexSnapshotExtractsRealGoDeclarations(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenantID := testTenant(t, db)

	repoID, err := store.EnsureRepository(ctx, tenantID, "/repo/"+t.Name())
	if err != nil {
		t.Fatalf("ensure repository: %v", err)
	}
	snap, err := store.CreateSnapshot(ctx, repoID, tenantID, "main", "commit-1", "heuristic-v1", "")
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}
	if err := store.IndexSnapshot(ctx, snap, repoID, []PGFileInput{
		{Path: "pkg/sample.go", Language: "go", ContentHash: hashContent(pgTestGoSource), Content: pgTestGoSource},
	}); err != nil {
		t.Fatalf("index snapshot: %v", err)
	}

	// An exact-path query now matches every symbol anchored at that path --
	// the Tier 0 file-level symbol plus every Tier 1 declaration in it, all
	// legitimately "at" pkg/sample.go.
	byPath, err := store.QueryByPathOrFQN(ctx, repoID, "main", "pkg/sample.go")
	if err != nil {
		t.Fatalf("query by path: %v", err)
	}
	fileLevelCount := 0
	for _, r := range byPath {
		if r.FullyQualifiedName == "pkg/sample.go" {
			fileLevelCount++
		}
	}
	if fileLevelCount != 1 {
		t.Fatalf("expected exactly one Tier 0 file-level symbol among path matches, got %+v", byPath)
	}
	if len(byPath) != 4 {
		t.Fatalf("expected the file symbol plus 3 declarations (CreateUser, UserService, Close), got %d: %+v", len(byPath), byPath)
	}

	fn, err := store.QueryByPathOrFQN(ctx, repoID, "main", "sample.CreateUser")
	if err != nil {
		t.Fatalf("query function: %v", err)
	}
	if len(fn) != 1 {
		t.Fatalf("expected exactly one result for sample.CreateUser, got %+v", fn)
	}

	method, err := store.QueryByPathOrFQN(ctx, repoID, "main", "sample.(*UserService).Close")
	if err != nil {
		t.Fatalf("query method: %v", err)
	}
	if len(method) != 1 {
		t.Fatalf("expected exactly one result for the Close method, got %+v", method)
	}

	var docComment sql.NullString
	if err := db.QueryRowContext(ctx, `
		SELECT doc_comment FROM code_symbol_versions WHERE snapshot_id = $1 AND symbol_id = $2`,
		fn[0].SnapshotID, fn[0].SymbolID).Scan(&docComment); err != nil {
		t.Fatalf("load doc comment: %v", err)
	}
	if !docComment.Valid || docComment.String != "CreateUser creates a user." {
		t.Fatalf("expected CreateUser's doc comment to be stored, got %+v", docComment)
	}
}

// A one-line edit to a single function's body must change only that
// function's identity in the durable store -- not the type, not the other
// method, not the file-level symbol -- carrying goextract_test.go's
// fine-grained-identity guarantee through actual indexing.
func TestPGIndexSnapshotIsolatesGoFunctionEditToThatSymbol(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenantID := testTenant(t, db)

	repoID, err := store.EnsureRepository(ctx, tenantID, "/repo/"+t.Name())
	if err != nil {
		t.Fatalf("ensure repository: %v", err)
	}
	snap1, err := store.CreateSnapshot(ctx, repoID, tenantID, "main", "commit-1", "heuristic-v1", "")
	if err != nil {
		t.Fatalf("create snapshot 1: %v", err)
	}
	if err := store.IndexSnapshot(ctx, snap1, repoID, []PGFileInput{
		{Path: "pkg/sample.go", Language: "go", ContentHash: hashContent(pgTestGoSource), Content: pgTestGoSource},
	}); err != nil {
		t.Fatalf("index snapshot 1: %v", err)
	}
	before, err := store.QueryByPathOrFQN(ctx, repoID, "main", "sample.(*UserService).Close")
	if err != nil || len(before) != 1 {
		t.Fatalf("expected Close to resolve before the edit: %v (err %v)", before, err)
	}
	closeSymbolBefore := before[0].SymbolID
	beforeType, err := store.QueryByPathOrFQN(ctx, repoID, "main", "sample.UserService")
	if err != nil || len(beforeType) != 1 {
		t.Fatalf("expected UserService to resolve before the edit: %v (err %v)", beforeType, err)
	}
	typeSymbolBefore := beforeType[0].SymbolID

	editedSource := strings.Replace(pgTestGoSource,
		"func (s *UserService) Close() error {\n\treturn nil\n}",
		"func (s *UserService) Close() error {\n\treturn nil // now with a comment\n}", 1)
	snap2, err := store.CreateSnapshot(ctx, repoID, tenantID, "main", "commit-2", "heuristic-v1", "")
	if err != nil {
		t.Fatalf("create snapshot 2: %v", err)
	}
	if err := store.IndexSnapshot(ctx, snap2, repoID, []PGFileInput{
		{Path: "pkg/sample.go", Language: "go", ContentHash: hashContent(editedSource), Content: editedSource},
	}); err != nil {
		t.Fatalf("index snapshot 2: %v", err)
	}

	afterClose, err := store.QueryByPathOrFQN(ctx, repoID, "main", "sample.(*UserService).Close")
	if err != nil || len(afterClose) != 1 {
		t.Fatalf("expected Close to still resolve after the edit: %v (err %v)", afterClose, err)
	}
	if afterClose[0].SymbolID == closeSymbolBefore {
		t.Fatal("expected editing Close's body to change its symbol identity")
	}

	afterType, err := store.QueryByPathOrFQN(ctx, repoID, "main", "sample.UserService")
	if err != nil || len(afterType) != 1 {
		t.Fatalf("expected UserService to still resolve after the edit: %v (err %v)", afterType, err)
	}
	if afterType[0].SymbolID != typeSymbolBefore {
		t.Fatal("expected UserService's identity to be unaffected by an edit inside Close")
	}
}

func countEdges(t *testing.T, db *sql.DB, snapshotID, sourceSymbolID, targetSymbolID string) int {
	t.Helper()
	var count int
	if err := db.QueryRowContext(context.Background(), `
		SELECT count(*) FROM code_edges
		WHERE snapshot_id = $1 AND source_symbol_version_id = $2 AND target_symbol_version_id = $3
		  AND relation = 'CALLS' AND confidence_class = 'static_analysis'`,
		snapshotID, sourceSymbolID, targetSymbolID).Scan(&count); err != nil {
		t.Fatalf("count edges: %v", err)
	}
	return count
}

const callEdgeSource = `package sample

func helper() int {
	return 1
}

func Run() int {
	return helper()
}
`

// TestPGIndexSnapshotCreatesUnambiguousCallEdges proves the real, if
// syntactic, call graph the audit's competitive matrix marked "Missing"
// for the heuristic extractor: Run calling helper() produces an actual
// code_edges row once both are indexed, not just two isolated symbols.
func TestPGIndexSnapshotCreatesUnambiguousCallEdges(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenantID := testTenant(t, db)

	repoID, err := store.EnsureRepository(ctx, tenantID, "/repo/"+t.Name())
	if err != nil {
		t.Fatalf("ensure repository: %v", err)
	}
	snap, err := store.CreateSnapshot(ctx, repoID, tenantID, "main", "commit-1", "heuristic-v1", "")
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}
	if err := store.IndexSnapshot(ctx, snap, repoID, []PGFileInput{
		{Path: "pkg/sample.go", Language: "go", ContentHash: hashContent(callEdgeSource), Content: callEdgeSource},
	}); err != nil {
		t.Fatalf("index snapshot: %v", err)
	}

	run, err := store.QueryByPathOrFQN(ctx, repoID, "main", "sample.Run")
	if err != nil || len(run) != 1 {
		t.Fatalf("expected Run to resolve: %v (err %v)", run, err)
	}
	helper, err := store.QueryByPathOrFQN(ctx, repoID, "main", "sample.helper")
	if err != nil || len(helper) != 1 {
		t.Fatalf("expected helper to resolve: %v (err %v)", helper, err)
	}

	if got := countEdges(t, db, snap, run[0].SymbolVersionID, helper[0].SymbolVersionID); got != 1 {
		t.Fatalf("expected exactly one CALLS edge from Run to helper, got %d", got)
	}
}

const ambiguousCallSource = `package sample

type A struct{}

func (a *A) Validate() bool { return true }

type B struct{}

func (b *B) Validate() bool { return false }

func Run(a *A) bool {
	return a.Validate()
}
`

// TestPGIndexSnapshotSkipsAmbiguousCallNames is the other half of the same
// guarantee: when a bare call name matches more than one same-named
// declaration in the package (two unrelated Validate methods), no edge is
// created for either -- guessing between A.Validate and B.Validate would
// be worse than reporting nothing, per the confidence-class philosophy
// insertGoCallEdgesLocked documents.
func TestPGIndexSnapshotSkipsAmbiguousCallNames(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenantID := testTenant(t, db)

	repoID, err := store.EnsureRepository(ctx, tenantID, "/repo/"+t.Name())
	if err != nil {
		t.Fatalf("ensure repository: %v", err)
	}
	snap, err := store.CreateSnapshot(ctx, repoID, tenantID, "main", "commit-1", "heuristic-v1", "")
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}
	if err := store.IndexSnapshot(ctx, snap, repoID, []PGFileInput{
		{Path: "pkg/sample.go", Language: "go", ContentHash: hashContent(ambiguousCallSource), Content: ambiguousCallSource},
	}); err != nil {
		t.Fatalf("index snapshot: %v", err)
	}

	var edgeCount int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM code_edges WHERE snapshot_id = $1 AND relation = 'CALLS'`, snap).Scan(&edgeCount); err != nil {
		t.Fatalf("count edges: %v", err)
	}
	if edgeCount != 0 {
		t.Fatalf("expected no CALLS edges for an ambiguous name, got %d", edgeCount)
	}
}

const treeSitterPythonSource = `class User:
    def save(self):
        return True

def load_user(identifier):
    return User()
`

func TestPGIndexSnapshotUsesTreeSitterForPython(t *testing.T) {
	store, _ := testPGStore(t)
	ctx := context.Background()
	tenantID := testTenant(t, store.db)
	repoID, err := store.EnsureRepository(ctx, tenantID, "/repo/"+t.Name())
	if err != nil {
		t.Fatalf("ensure repository: %v", err)
	}
	snap, err := store.CreateSnapshot(ctx, repoID, tenantID, "main", "commit-python-1", "tree-sitter-v1", "")
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}
	if err := store.IndexSnapshot(ctx, snap, repoID, []PGFileInput{{
		Path: "models.py", Language: "python", ContentHash: hashContent(treeSitterPythonSource), Content: treeSitterPythonSource,
	}}); err != nil {
		t.Fatalf("index snapshot: %v", err)
	}
	for _, name := range []string{"models.User", "models.User.save", "models.load_user"} {
		results, err := store.QueryByPathOrFQN(ctx, repoID, "main", name)
		if err != nil || len(results) != 1 {
			t.Fatalf("expected Tree-sitter symbol %s, got %v (err %v)", name, results, err)
		}
	}
}

const treeSitterCallEdgeSource = `def helper():
    return 1

def run():
    return helper()
`

// TestPGIndexSnapshotCreatesTreeSitterCallEdges is the Tree-sitter-fallback
// counterpart to TestPGIndexSnapshotCreatesUnambiguousCallEdges: a Python
// file indexed via Tree-sitter (no Go parser involved) still produces a
// real CALLS edge for an unambiguous syntactic call, with 'tree-sitter:
// python' recorded as the edge's extractor.
func TestPGIndexSnapshotCreatesTreeSitterCallEdges(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenantID := testTenant(t, db)

	repoID, err := store.EnsureRepository(ctx, tenantID, "/repo/"+t.Name())
	if err != nil {
		t.Fatalf("ensure repository: %v", err)
	}
	snap, err := store.CreateSnapshot(ctx, repoID, tenantID, "main", "commit-1", "tree-sitter-v1", "")
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}
	if err := store.IndexSnapshot(ctx, snap, repoID, []PGFileInput{
		{Path: "pkg/sample.py", Language: "python", ContentHash: hashContent(treeSitterCallEdgeSource), Content: treeSitterCallEdgeSource},
	}); err != nil {
		t.Fatalf("index snapshot: %v", err)
	}

	run, err := store.QueryByPathOrFQN(ctx, repoID, "main", "sample.run")
	if err != nil || len(run) != 1 {
		t.Fatalf("expected run to resolve: %v (err %v)", run, err)
	}
	helper, err := store.QueryByPathOrFQN(ctx, repoID, "main", "sample.helper")
	if err != nil || len(helper) != 1 {
		t.Fatalf("expected helper to resolve: %v (err %v)", helper, err)
	}
	if got := countEdges(t, db, snap, run[0].SymbolVersionID, helper[0].SymbolVersionID); got != 1 {
		t.Fatalf("expected exactly one CALLS edge from run to helper, got %d", got)
	}

	var extractor string
	if err := db.QueryRowContext(ctx, `
		SELECT extractor FROM code_edges WHERE snapshot_id = $1 AND relation = 'CALLS'`, snap).Scan(&extractor); err != nil {
		t.Fatalf("load edge extractor: %v", err)
	}
	if extractor != "tree-sitter:python" {
		t.Fatalf("expected extractor tree-sitter:python, got %q", extractor)
	}
}

func hasFQN(symbols []PGSymbolVersion, fqn string) bool {
	for _, s := range symbols {
		if s.FullyQualifiedName == fqn {
			return true
		}
	}
	return false
}

// leaf <- middle <- top: a three-level real call chain, so ChangeImpact
// can be tested for both direct (one hop) and transitive (two hops)
// dependents in the same fixture.
const impactChainSource = `package sample

func leaf() int {
	return 1
}

func middle() int {
	return leaf()
}

func top() int {
	return middle()
}
`

func TestPGChangeImpactFindsDirectAndTransitiveDependents(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenantID := testTenant(t, db)

	repoID, err := store.EnsureRepository(ctx, tenantID, "/repo/"+t.Name())
	if err != nil {
		t.Fatalf("ensure repository: %v", err)
	}
	snap, err := store.CreateSnapshot(ctx, repoID, tenantID, "main", "commit-1", "heuristic-v1", "")
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}
	if err := store.IndexSnapshot(ctx, snap, repoID, []PGFileInput{
		{Path: "pkg/sample.go", Language: "go", ContentHash: hashContent(impactChainSource), Content: impactChainSource},
	}); err != nil {
		t.Fatalf("index snapshot: %v", err)
	}

	impact, err := store.ChangeImpact(ctx, repoID, "main", "sample.leaf", 3)
	if err != nil {
		t.Fatalf("change impact: %v", err)
	}
	if !hasFQN(impact.DirectDependents, "sample.middle") {
		t.Fatalf("expected middle (calls leaf directly) in DirectDependents, got %+v", impact.DirectDependents)
	}
	if hasFQN(impact.DirectDependents, "sample.top") {
		t.Fatalf("expected top NOT in DirectDependents (it calls middle, not leaf directly), got %+v", impact.DirectDependents)
	}
	if !hasFQN(impact.TransitiveDependents, "sample.top") {
		t.Fatalf("expected top (calls middle, which calls leaf) in TransitiveDependents, got %+v", impact.TransitiveDependents)
	}
	if hasFQN(impact.TransitiveDependents, "sample.middle") {
		t.Fatalf("expected middle NOT to also appear in TransitiveDependents (it's already Direct), got %+v", impact.TransitiveDependents)
	}
}

// maxDepth=1 must surface middle (one hop from leaf) but never reach top
// (two hops) -- the depth bound actually bounds something.
func TestPGChangeImpactRespectsMaxDepth(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenantID := testTenant(t, db)

	repoID, err := store.EnsureRepository(ctx, tenantID, "/repo/"+t.Name())
	if err != nil {
		t.Fatalf("ensure repository: %v", err)
	}
	snap, err := store.CreateSnapshot(ctx, repoID, tenantID, "main", "commit-1", "heuristic-v1", "")
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}
	if err := store.IndexSnapshot(ctx, snap, repoID, []PGFileInput{
		{Path: "pkg/sample.go", Language: "go", ContentHash: hashContent(impactChainSource), Content: impactChainSource},
	}); err != nil {
		t.Fatalf("index snapshot: %v", err)
	}

	impact, err := store.ChangeImpact(ctx, repoID, "main", "sample.leaf", 1)
	if err != nil {
		t.Fatalf("change impact: %v", err)
	}
	if !hasFQN(impact.DirectDependents, "sample.middle") {
		t.Fatalf("expected middle in DirectDependents even at maxDepth=1, got %+v", impact.DirectDependents)
	}
	if hasFQN(impact.TransitiveDependents, "sample.top") || len(impact.TransitiveDependents) != 0 {
		t.Fatalf("expected no transitive dependents at maxDepth=1, got %+v", impact.TransitiveDependents)
	}
}

// A leaf function with no callers at all must return an empty impact, not
// an error -- "nothing depends on this" is itself a meaningful answer.
func TestPGChangeImpactReturnsEmptyForUncalledSymbol(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenantID := testTenant(t, db)

	repoID, err := store.EnsureRepository(ctx, tenantID, "/repo/"+t.Name())
	if err != nil {
		t.Fatalf("ensure repository: %v", err)
	}
	snap, err := store.CreateSnapshot(ctx, repoID, tenantID, "main", "commit-1", "heuristic-v1", "")
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}
	if err := store.IndexSnapshot(ctx, snap, repoID, []PGFileInput{
		{Path: "pkg/sample.go", Language: "go", ContentHash: hashContent(impactChainSource), Content: impactChainSource},
	}); err != nil {
		t.Fatalf("index snapshot: %v", err)
	}

	impact, err := store.ChangeImpact(ctx, repoID, "main", "sample.top", 3)
	if err != nil {
		t.Fatalf("change impact: %v", err)
	}
	if len(impact.DirectDependents) != 0 || len(impact.TransitiveDependents) != 0 {
		t.Fatalf("expected no dependents for top (nothing calls it), got %+v", impact)
	}
}

// fanInSource gives hub two independent callers (heavyCaller and
// lightCaller) plus a caller-of-a-caller (indirect -> heavyCaller), so
// ChangeImpact("hub") has a DirectDependents set where centrality should
// distinguish heavyCaller (also transitively depended on by indirect) from
// lightCaller (a dead end) even though both are one hop away.
const fanInSource = `package sample

func hub() int {
	return 1
}

func heavyCaller() int {
	return hub()
}

func lightCaller() int {
	return hub()
}

func indirect() int {
	return heavyCaller()
}
`

// P0-A: ChangeImpact ranks DirectDependents/TransitiveDependents by
// personalized-PageRank centrality within the visited subgraph, not by
// arbitrary BFS order. heavyCaller is itself called by indirect, so it
// should rank above lightCaller (a dead end with no further callers) even
// though both are direct dependents of hub.
func TestPGChangeImpactRanksDependentsByCentrality(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenantID := testTenant(t, db)

	repoID, err := store.EnsureRepository(ctx, tenantID, "/repo/"+t.Name())
	if err != nil {
		t.Fatalf("ensure repository: %v", err)
	}
	snap, err := store.CreateSnapshot(ctx, repoID, tenantID, "main", "commit-1", "heuristic-v1", "")
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}
	if err := store.IndexSnapshot(ctx, snap, repoID, []PGFileInput{
		{Path: "pkg/sample.go", Language: "go", ContentHash: hashContent(fanInSource), Content: fanInSource},
	}); err != nil {
		t.Fatalf("index snapshot: %v", err)
	}

	impact, err := store.ChangeImpact(ctx, repoID, "main", "sample.hub", 3)
	if err != nil {
		t.Fatalf("change impact: %v", err)
	}
	if len(impact.DirectDependents) != 2 {
		t.Fatalf("expected 2 direct dependents (heavyCaller, lightCaller), got %+v", impact.DirectDependents)
	}
	if impact.DirectDependents[0].FullyQualifiedName != "sample.heavyCaller" {
		t.Fatalf("expected heavyCaller ranked first by centrality, got order %+v", impact.DirectDependents)
	}
	if impact.DirectDependents[0].Centrality <= impact.DirectDependents[1].Centrality {
		t.Fatalf("expected heavyCaller's centrality (%v) to exceed lightCaller's (%v)",
			impact.DirectDependents[0].Centrality, impact.DirectDependents[1].Centrality)
	}
	if !hasFQN(impact.TransitiveDependents, "sample.indirect") {
		t.Fatalf("expected indirect (calls heavyCaller) in TransitiveDependents, got %+v", impact.TransitiveDependents)
	}
}

// TestPGIndexSnapshotSkipsUnchangedFileAndPreservesCrossFileEdges is P1-D's
// core correctness guarantee: a changed file (helper.go's target function
// gets renamed to force a real content change) that calls into an
// UNCHANGED file (caller.go, byte-identical between snapshots) must still
// resolve that CALLS edge in the new snapshot -- caller.go's declarations
// were copied forward from the parent snapshot, not re-parsed, and this
// proves the copy still contributes correctly to this run's call-edge
// resolution rather than silently dropping the edge.
func TestPGIndexSnapshotSkipsUnchangedFileAndPreservesCrossFileEdges(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenantID := testTenant(t, db)

	repoID, err := store.EnsureRepository(ctx, tenantID, "/repo/"+t.Name())
	if err != nil {
		t.Fatalf("ensure repository: %v", err)
	}

	const callerSource = `package sample

func caller() {
	target()
}
`
	const helperSourceV1 = `package sample

func target() {}
`
	const helperSourceV2 = `package sample

func target() { _ = 1 }
`

	snap1, err := store.CreateSnapshot(ctx, repoID, tenantID, "main", "commit-1", "heuristic-v1", "")
	if err != nil {
		t.Fatalf("create snapshot 1: %v", err)
	}
	if err := store.IndexSnapshot(ctx, snap1, repoID, []PGFileInput{
		{Path: "pkg/caller.go", Language: "go", ContentHash: hashContent(callerSource), Content: callerSource},
		{Path: "pkg/helper.go", Language: "go", ContentHash: hashContent(helperSourceV1), Content: helperSourceV1},
	}); err != nil {
		t.Fatalf("index snapshot 1: %v", err)
	}

	// Sanity check on the baseline snapshot: the edge exists before any skip
	// logic is involved.
	impact1, err := store.ChangeImpact(ctx, repoID, "main", "sample.target", 3)
	if err != nil {
		t.Fatalf("change impact (snapshot 1): %v", err)
	}
	if !hasFQN(impact1.DirectDependents, "sample.caller") {
		t.Fatalf("expected sample.caller as a direct dependent in snapshot 1, got %+v", impact1.DirectDependents)
	}

	// Snapshot 2: caller.go is byte-identical (must be skipped/copied, not
	// re-parsed); helper.go's target() body changed (forces re-parse).
	snap2, err := store.CreateSnapshot(ctx, repoID, tenantID, "main", "commit-2", "heuristic-v1", "")
	if err != nil {
		t.Fatalf("create snapshot 2: %v", err)
	}
	if err := store.IndexSnapshot(ctx, snap2, repoID, []PGFileInput{
		{Path: "pkg/caller.go", Language: "go", ContentHash: hashContent(callerSource), Content: callerSource},
		{Path: "pkg/helper.go", Language: "go", ContentHash: hashContent(helperSourceV2), Content: helperSourceV2},
	}); err != nil {
		t.Fatalf("index snapshot 2: %v", err)
	}

	impact2, err := store.ChangeImpact(ctx, repoID, "main", "sample.target", 3)
	if err != nil {
		t.Fatalf("change impact (snapshot 2): %v", err)
	}
	if !hasFQN(impact2.DirectDependents, "sample.caller") {
		t.Fatalf("expected sample.caller as a direct dependent in snapshot 2 (edge must survive caller.go's skip), got %+v", impact2.DirectDependents)
	}

	// Confirm the skip actually happened (caller.go's symbol_version row in
	// snapshot 2 was copied, not re-inserted from a fresh parse) by checking
	// its declaration count matches snapshot 1's -- a re-parse of identical
	// source would produce the same count too, so this alone doesn't prove
	// the skip path ran, but combined with the edge surviving above it
	// confirms the copy path is at minimum not corrupting anything.
	var count1, count2 int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM code_symbol_versions sv
		JOIN code_file_versions fv ON fv.id = sv.file_version_id
		WHERE fv.snapshot_id = $1 AND fv.path = 'pkg/caller.go' AND sv.kind != 'file'`, snap1).Scan(&count1); err != nil {
		t.Fatalf("count snapshot 1 caller declarations: %v", err)
	}
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM code_symbol_versions sv
		JOIN code_file_versions fv ON fv.id = sv.file_version_id
		WHERE fv.snapshot_id = $1 AND fv.path = 'pkg/caller.go' AND sv.kind != 'file'`, snap2).Scan(&count2); err != nil {
		t.Fatalf("count snapshot 2 caller declarations: %v", err)
	}
	if count1 != count2 || count1 == 0 {
		t.Fatalf("expected matching non-zero declaration counts for caller.go across snapshots, got %d vs %d", count1, count2)
	}
}

func TestPGChangeImpactReturnsErrSymbolNotFoundForUnknownQuery(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenantID := testTenant(t, db)

	repoID, err := store.EnsureRepository(ctx, tenantID, "/repo/"+t.Name())
	if err != nil {
		t.Fatalf("ensure repository: %v", err)
	}
	if _, err := store.ChangeImpact(ctx, repoID, "main", "sample.doesNotExist", 3); !errors.Is(err, ErrSymbolNotFound) {
		t.Fatalf("expected ErrSymbolNotFound, got %v", err)
	}
}

func TestPGCodeEmbeddingRLSIsolatesTenants(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenantA := testTenant(t, db)
	tenantB := testTenant(t, db)

	makeSymbol := func(tenant, repoPath, filePath string) string {
		repoID, err := store.EnsureRepository(ctx, tenant, repoPath)
		if err != nil {
			t.Fatal(err)
		}
		snapshotID, err := store.CreateSnapshot(ctx, repoID, tenant, "main", "commit-1", "test", "")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.IndexSnapshot(ctx, snapshotID, repoID, []PGFileInput{{Path: filePath, Language: "unknown", ContentHash: hashContent(filePath)}}); err != nil {
			t.Fatal(err)
		}
		results, err := store.QueryByPathOrFQN(ctx, repoID, "main", filePath)
		if err != nil || len(results) != 1 {
			t.Fatalf("resolve symbol: %v %+v", err, results)
		}
		return results[0].SymbolVersionID
	}

	symbolA := makeSymbol(tenantA, "/repo/rls-a-"+t.Name(), "a.txt")
	symbolB := makeSymbol(tenantB, "/repo/rls-b-"+t.Name(), "b.txt")
	spaceID, err := store.EnsureCodeEmbeddingSpace(ctx, codeEmbeddingSpace{Role: "code", Provider: "test", Model: "test-code", Revision: "r1", Dimensions: 3, Generation: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertCodeEmbedding(ctx, tenantA, symbolA, spaceID, hashContent("a"), []float64{1, 0, 0}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertCodeEmbedding(ctx, tenantB, symbolB, spaceID, hashContent("b"), []float64{0, 1, 0}); err != nil {
		t.Fatal(err)
	}

	role := "scos_p4_rls_" + hashContent(t.Name())[:12]
	if _, err := db.ExecContext(ctx, `CREATE ROLE `+role+` NOLOGIN`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`REVOKE SELECT ON code_embeddings FROM ` + role)
		_, _ = db.Exec(`DROP ROLE IF EXISTS ` + role)
	})
	if _, err := db.ExecContext(ctx, `GRANT SELECT ON code_embeddings TO `+role); err != nil {
		t.Fatal(err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SET LOCAL ROLE `+role); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('scos.tenant_id', $1, true)`, tenantA); err != nil {
		t.Fatal(err)
	}
	var visible, leaked int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM code_embeddings`).Scan(&visible); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM code_embeddings WHERE tenant_id = $1`, tenantB).Scan(&leaked); err != nil {
		t.Fatal(err)
	}
	if visible != 1 || leaked != 0 {
		t.Fatalf("RLS visible=%d leaked_other_tenant=%d", visible, leaked)
	}
}

func TestPGDenseQueryFiltersExactEmbeddingSpace(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenant := testTenant(t, db)
	repoID, err := store.EnsureRepository(ctx, tenant, "/repo/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	snapshotID, err := store.CreateSnapshot(ctx, repoID, tenant, "main", "commit-1", "test", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, snapshotID, repoID, []PGFileInput{
		{Path: "three.txt", Language: "unknown", ContentHash: hashContent("three")},
		{Path: "two.txt", Language: "unknown", ContentHash: hashContent("two")},
	}); err != nil {
		t.Fatal(err)
	}
	three, _ := store.QueryByPathOrFQN(ctx, repoID, "main", "three.txt")
	two, _ := store.QueryByPathOrFQN(ctx, repoID, "main", "two.txt")
	space3, err := store.EnsureCodeEmbeddingSpace(ctx, codeEmbeddingSpace{Role: "code", Provider: "test", Model: "code-3", Revision: "r1", Dimensions: 3, Generation: 1})
	if err != nil {
		t.Fatal(err)
	}
	space2, err := store.EnsureCodeEmbeddingSpace(ctx, codeEmbeddingSpace{Role: "code", Provider: "test", Model: "code-2", Revision: "r1", Dimensions: 2, Generation: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertCodeEmbedding(ctx, tenant, three[0].SymbolVersionID, space3, hashContent("three"), []float64{1, 0, 0}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertCodeEmbedding(ctx, tenant, two[0].SymbolVersionID, space2, hashContent("two"), []float64{1, 0}); err != nil {
		t.Fatal(err)
	}
	results, err := store.QueryDense(ctx, tenant, repoID, "main", []float64{1, 0, 0}, space3, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Path != "three.txt" {
		t.Fatalf("cross-space dense result leaked: %+v", results)
	}
}

func TestPGDenseQueryUsesActiveSnapshotOnly(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenant := testTenant(t, db)
	repoID, err := store.EnsureRepository(ctx, tenant, "/repo/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	spaceID, err := store.EnsureCodeEmbeddingSpace(ctx, codeEmbeddingSpace{Role: "code", Provider: "test", Model: "active-only", Revision: "r1", Dimensions: 3, Generation: 1})
	if err != nil {
		t.Fatal(err)
	}

	snap1, _ := store.CreateSnapshot(ctx, repoID, tenant, "main", "commit-1", "test", "")
	if err := store.IndexSnapshot(ctx, snap1, repoID, []PGFileInput{{Path: "old.txt", Language: "unknown", ContentHash: hashContent("old")}}); err != nil {
		t.Fatal(err)
	}
	old, _ := store.QueryByPathOrFQN(ctx, repoID, "main", "old.txt")
	if err := store.UpsertCodeEmbedding(ctx, tenant, old[0].SymbolVersionID, spaceID, hashContent("old"), []float64{1, 0, 0}); err != nil {
		t.Fatal(err)
	}

	snap2, _ := store.CreateSnapshot(ctx, repoID, tenant, "main", "commit-2", "test", "")
	if err := store.IndexSnapshot(ctx, snap2, repoID, []PGFileInput{{Path: "new.txt", Language: "unknown", ContentHash: hashContent("new")}}); err != nil {
		t.Fatal(err)
	}
	current, _ := store.QueryByPathOrFQN(ctx, repoID, "main", "new.txt")
	if err := store.UpsertCodeEmbedding(ctx, tenant, current[0].SymbolVersionID, spaceID, hashContent("new"), []float64{0.9, 0.1, 0}); err != nil {
		t.Fatal(err)
	}

	results, err := store.QueryDense(ctx, tenant, repoID, "main", []float64{1, 0, 0}, spaceID, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Path != "new.txt" || results[0].SnapshotID != snap2 {
		t.Fatalf("superseded snapshot leaked into dense query: %+v", results)
	}
}

func TestPGIndexPersistsSourceChunksAndCopiesThemForward(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenant := testTenant(t, db)
	repoID, err := store.EnsureRepository(ctx, tenant, "/repo/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	content := "package sample\n\n// Work does useful work.\nfunc Work() {\n\tprintln(\"useful\")\n}\n"
	snap1, _ := store.CreateSnapshot(ctx, repoID, tenant, "main", "commit-1", "test", "")
	if err := store.IndexSnapshot(ctx, snap1, repoID, []PGFileInput{{Path: "work.go", Language: "go", ContentHash: hashContent(content), Content: content}}); err != nil {
		t.Fatal(err)
	}
	var declarationChunk string
	if err := db.QueryRowContext(ctx, `
		SELECT cc.content FROM code_chunks cc
		JOIN code_symbol_versions sv ON sv.id = cc.symbol_version_id
		WHERE sv.snapshot_id = $1 AND sv.fully_qualified_name = 'sample.Work'`, snap1).Scan(&declarationChunk); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(declarationChunk, `println("useful")`) {
		t.Fatalf("declaration chunk did not retain source: %q", declarationChunk)
	}

	snap2, _ := store.CreateSnapshot(ctx, repoID, tenant, "main", "commit-2", "test", "")
	if err := store.IndexSnapshot(ctx, snap2, repoID, []PGFileInput{{Path: "work.go", Language: "go", ContentHash: hashContent(content), Content: content}}); err != nil {
		t.Fatal(err)
	}
	var copied string
	if err := db.QueryRowContext(ctx, `
		SELECT cc.content FROM code_chunks cc
		JOIN code_symbol_versions sv ON sv.id = cc.symbol_version_id
		WHERE sv.snapshot_id = $1 AND sv.fully_qualified_name = 'sample.Work'`, snap2).Scan(&copied); err != nil {
		t.Fatal(err)
	}
	if copied != declarationChunk {
		t.Fatalf("unchanged declaration chunk was not copied forward: %q != %q", copied, declarationChunk)
	}
}

func TestPGStructuralRetrievalUsesActiveSnapshotReferenceEdges(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenantID := testTenant(t, db)
	repoID, err := store.EnsureRepository(ctx, tenantID, "/repo/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	snapshotID, err := store.CreateSnapshot(ctx, repoID, tenantID, "main", "commit-1", "test", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, snapshotID, repoID, []PGFileInput{
		{Path: "a.go", Language: "go", ContentHash: hashContent("package p\nfunc A(){}\n"), Content: "package p\nfunc A(){}\n"},
		{Path: "b.go", Language: "go", ContentHash: hashContent("package p\nfunc B(){}\n"), Content: "package p\nfunc B(){}\n"},
	}); err != nil {
		t.Fatal(err)
	}
	var aID, bID string
	if err := db.QueryRow(`SELECT id FROM code_symbol_versions WHERE snapshot_id=$1 AND fully_qualified_name='p.A'`, snapshotID).Scan(&aID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT id FROM code_symbol_versions WHERE snapshot_id=$1 AND fully_qualified_name='p.B'`, snapshotID).Scan(&bID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO code_edges (snapshot_id,source_symbol_version_id,target_symbol_version_id,relation,confidence_class,confidence_score,extractor) VALUES ($1,$2,$3,'REFERENCES','deterministic',1.0,'scip:go')`, snapshotID, bID, aID); err != nil {
		t.Fatal(err)
	}
	neighbors, ranks, err := store.QueryStructuralCandidates(ctx, repoID, "main", []string{aID}, 2, 20)
	if err != nil {
		t.Fatal(err)
	}
	foundB := false
	for _, neighbor := range neighbors {
		if neighbor.SymbolVersionID == bID && neighbor.RawStructuralScore > 0 {
			foundB = true
			break
		}
	}
	if !foundB {
		t.Fatalf("expected B as structural evidence for A, got %#v", neighbors)
	}
	if ranks[aID] <= 0 || ranks[bID] <= 0 {
		t.Fatalf("expected PageRank for both visited nodes: %#v", ranks)
	}
}

// Byte-identical files (duplicated config, copied helpers) and identical
// declarations used to collide on one content-keyed symbol identity and
// abort the whole snapshot with a unique-constraint error. Each occurrence
// must index; re-indexing the same tree must reuse the same identities.
func TestPGIndexSnapshotIndexesDuplicateFilesAndDeclarations(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenantID := testTenant(t, db)
	repoID, err := store.EnsureRepository(ctx, tenantID, "/repo/"+t.Name())
	if err != nil {
		t.Fatalf("ensure repository: %v", err)
	}
	const config = `{"mcpServers":{}}`
	const helper = `package sample

func noop() {}
`
	files := []PGFileInput{
		{Path: "a/.mcp.json", Language: "configuration", ContentHash: hashContent(config), Content: config},
		{Path: "b/.mcp.json", Language: "configuration", ContentHash: hashContent(config), Content: config},
		{Path: "x/helper.go", Language: "go", ContentHash: hashContent(helper), Content: helper},
		{Path: "y/helper.go", Language: "go", ContentHash: hashContent(helper), Content: helper},
	}
	index := func(commit string) string {
		snap, err := store.CreateSnapshot(ctx, repoID, tenantID, "main", commit, "heuristic-v1", "")
		if err != nil {
			t.Fatalf("create snapshot: %v", err)
		}
		if err := store.IndexSnapshot(ctx, snap, repoID, files); err != nil {
			t.Fatalf("index snapshot with duplicates: %v", err)
		}
		return snap
	}
	snap1 := index("commit-1")
	var versions, distinct, symbolCount int
	if err := db.QueryRowContext(ctx, `SELECT count(*), count(DISTINCT symbol_id) FROM code_symbol_versions WHERE snapshot_id = $1`, snap1).Scan(&versions, &distinct); err != nil {
		t.Fatal(err)
	}
	// 4 file symbols + 2 noop declarations, each its own identity.
	if versions != 6 || distinct != 6 {
		t.Fatalf("snapshot holds %d versions over %d identities, want 6/6", versions, distinct)
	}
	if err := db.QueryRowContext(ctx, `SELECT symbol_count FROM code_repository_snapshots WHERE id = $1`, snap1).Scan(&symbolCount); err != nil || symbolCount != 6 {
		t.Fatalf("snapshot symbol_count = %d (%v), want the real symbol count 6", symbolCount, err)
	}

	snap2 := index("commit-2")
	var reused int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM code_symbol_versions v2
		  JOIN code_symbol_versions v1 ON v1.symbol_id = v2.symbol_id AND v1.path = v2.path AND v1.snapshot_id = $1
		 WHERE v2.snapshot_id = $2`, snap1, snap2).Scan(&reused); err != nil {
		t.Fatal(err)
	}
	if reused != 6 {
		t.Fatalf("re-indexing an unchanged tree reused %d of 6 identities", reused)
	}
}

// A call through an imported package ("auth.Sign()") must become a CALLS
// edge to that package's function, so impact analysis of an exported
// function finds its callers in other packages. A same-named function in a
// different package must not capture the edge.
func TestPGIndexSnapshotResolvesCallsAcrossPackages(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenantID := testTenant(t, db)
	repoID, err := store.EnsureRepository(ctx, tenantID, "/repo/"+t.Name())
	if err != nil {
		t.Fatalf("ensure repository: %v", err)
	}
	const auth = "package auth\n\nfunc Sign() string { return \"t\" }\n"
	const decoy = "package other\n\nfunc Sign() string { return \"decoy\" }\n"
	const gateway = "package main\n\nimport (\n\t\"example.com/svc/packages/auth\"\n\tx \"example.com/svc/packages/other\"\n)\n\nfunc handle() string { return auth.Sign() }\n\nfunc unrelated() string { return x.Sign() }\n"
	snap, err := store.CreateSnapshot(ctx, repoID, tenantID, "main", "c1", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, snap, repoID, []PGFileInput{
		{Path: "packages/auth/sign.go", Language: "go", ContentHash: hashContent(auth), Content: auth},
		{Path: "packages/other/sign.go", Language: "go", ContentHash: hashContent(decoy), Content: decoy},
		{Path: "cmd/gateway/main.go", Language: "go", ContentHash: hashContent(gateway), Content: gateway},
	}); err != nil {
		t.Fatalf("index: %v", err)
	}
	impact, err := store.ChangeImpact(ctx, repoID, "main", "auth.Sign", 3)
	if err != nil {
		t.Fatalf("impact: %v", err)
	}
	var callers []string
	for _, d := range impact.DirectDependents {
		callers = append(callers, d.FullyQualifiedName)
	}
	if len(callers) != 1 || callers[0] != "main.handle" {
		t.Fatalf("auth.Sign callers = %v, want [main.handle]", callers)
	}

	// A bare name matches every declaration of it, each listed.
	byName, err := store.ChangeImpact(ctx, repoID, "main", "Sign", 3)
	if err != nil {
		t.Fatalf("impact by bare name: %v", err)
	}
	var matched []string
	for _, m := range byName.MatchedSymbols {
		matched = append(matched, m.FullyQualifiedName)
	}
	if strings.Join(matched, ",") != "auth.Sign,other.Sign" || len(byName.DirectDependents) != 2 {
		t.Fatalf("bare-name impact matched %v with %d dependants", matched, len(byName.DirectDependents))
	}
	if _, err := store.ChangeImpact(ctx, repoID, "main", "Sig_", 3); err == nil {
		t.Fatal("LIKE wildcards in a bare name must match literally")
	}
}

// A question in prose must find a symbol whose name, signature or doc
// comment carries its words (camelCase split, English stemming), and name
// matches must still outrank mentions in prose.
func TestPGQueryLexicalAnswersNaturalLanguageQuestions(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenantID := testTenant(t, db)
	repoID, err := store.EnsureRepository(ctx, tenantID, "/repo/"+t.Name())
	if err != nil {
		t.Fatalf("ensure repository: %v", err)
	}
	const src = `package gateway

// resolveIdentity authenticates the caller from its bearer token.
func resolveIdentity(token string) string { return token }

// renderPage writes HTML.
func renderPage() string { return "" }
`
	snap, err := store.CreateSnapshot(ctx, repoID, tenantID, "main", "c1", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, snap, repoID, []PGFileInput{
		{Path: "gateway/identity.go", Language: "go", ContentHash: hashContent(src), Content: src},
	}); err != nil {
		t.Fatalf("index: %v", err)
	}
	for query, want := range map[string]string{
		"how does the gateway authenticate callers": "gateway.resolveIdentity",
		"resolve identity":                          "gateway.resolveIdentity",
		"resolveIdentity":                           "gateway.resolveIdentity",
		"bearer tokens":                             "gateway.resolveIdentity",
	} {
		results, err := store.QueryLexical(ctx, repoID, "main", query, 5)
		if err != nil {
			t.Fatalf("%q: %v", query, err)
		}
		if len(results) == 0 || results[0].FullyQualifiedName != want {
			var got []string
			for _, r := range results {
				got = append(got, r.FullyQualifiedName)
			}
			t.Fatalf("%q ranked %v first, want %s", query, got, want)
		}
	}
	if results, err := store.QueryLexical(ctx, repoID, "main", "the of & | !(", 5); err != nil || len(results) != 0 {
		t.Fatalf("stop words and operators must yield no query and no error, got %v %v", results, err)
	}
}
