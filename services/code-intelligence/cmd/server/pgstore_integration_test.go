package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
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
