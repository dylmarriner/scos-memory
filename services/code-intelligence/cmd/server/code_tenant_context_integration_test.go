package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestPGCodeRepositoryWritesRequireTransactionLocalTenant verifies the
// restricted role cannot read or write code provenance without a scoped
// transaction. A private test schema shadows the two production tables so
// this test never changes production policies or other integration fixtures.
func TestPGCodeRepositoryWritesRequireTransactionLocalTenant(t *testing.T) {
	adminDSN := os.Getenv("SCOS_CODE_STORE_TEST_DSN")
	appDSN := os.Getenv("SCOS_APP_TEST_DATABASE_URL")
	if adminDSN == "" || appDSN == "" {
		t.Skip("requires owner and restricted-role disposable PostgreSQL DSNs")
	}
	ctx := context.Background()
	admin, err := sql.Open("pgx", adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	app, err := sql.Open("pgx", appDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	app.SetMaxOpenConns(1)
	schema := fmt.Sprintf("p6_code_scope_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.ExecContext(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("clean isolated scope schema: %v", err)
		}
	}()
	statements := []string{
		"CREATE TABLE " + schema + ".code_repositories (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), tenant_id uuid NOT NULL, canonical_path text NOT NULL, updated_at timestamptz NOT NULL DEFAULT now(), UNIQUE(tenant_id,canonical_path))",
		"CREATE TABLE " + schema + ".code_repository_snapshots (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), repository_id uuid NOT NULL, tenant_id uuid NOT NULL, branch text NOT NULL, commit_id text NOT NULL, extractor_version text NOT NULL, configuration_hash text NOT NULL, parent_snapshot_id uuid, status text NOT NULL DEFAULT 'queued', created_at timestamptz NOT NULL DEFAULT now(), activated_at timestamptz, UNIQUE(repository_id,branch,commit_id,extractor_version,configuration_hash))",
		"CREATE TABLE " + schema + ".code_symbols (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), repository_id uuid NOT NULL, stable_key text NOT NULL, kind text NOT NULL, first_snapshot_id uuid NOT NULL)",
		"CREATE TABLE " + schema + ".code_symbol_versions (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), symbol_id uuid NOT NULL, snapshot_id uuid NOT NULL, fully_qualified_name text NOT NULL, path text NOT NULL, kind text NOT NULL DEFAULT 'file', content_hash text NOT NULL, status text NOT NULL DEFAULT 'active', search_vector tsvector)",
		"GRANT USAGE ON SCHEMA " + schema + " TO scos_app",
		"GRANT SELECT,INSERT,UPDATE ON " + schema + ".code_repositories," + schema + ".code_repository_snapshots," + schema + ".code_symbols," + schema + ".code_symbol_versions TO scos_app",
		"ALTER TABLE " + schema + ".code_repositories ENABLE ROW LEVEL SECURITY",
		"ALTER TABLE " + schema + ".code_repositories FORCE ROW LEVEL SECURITY",
		"ALTER TABLE " + schema + ".code_repository_snapshots ENABLE ROW LEVEL SECURITY",
		"ALTER TABLE " + schema + ".code_repository_snapshots FORCE ROW LEVEL SECURITY",
		"ALTER TABLE " + schema + ".code_symbols ENABLE ROW LEVEL SECURITY",
		"ALTER TABLE " + schema + ".code_symbols FORCE ROW LEVEL SECURITY",
		"ALTER TABLE " + schema + ".code_symbol_versions ENABLE ROW LEVEL SECURITY",
		"ALTER TABLE " + schema + ".code_symbol_versions FORCE ROW LEVEL SECURITY",
		"CREATE POLICY symbol_scope ON " + schema + ".code_symbols USING (EXISTS (SELECT 1 FROM " + schema + ".code_repositories WHERE id=repository_id)) WITH CHECK (EXISTS (SELECT 1 FROM " + schema + ".code_repositories WHERE id=repository_id))",
		"CREATE POLICY version_scope ON " + schema + ".code_symbol_versions USING (EXISTS (SELECT 1 FROM " + schema + ".code_repository_snapshots WHERE id=snapshot_id)) WITH CHECK (EXISTS (SELECT 1 FROM " + schema + ".code_repository_snapshots WHERE id=snapshot_id))",
		"CREATE POLICY repo_scope ON " + schema + ".code_repositories USING (tenant_id = NULLIF(current_setting('scos.tenant_id',true),'')::uuid) WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id',true),'')::uuid)",
		"CREATE POLICY snap_scope ON " + schema + ".code_repository_snapshots USING (tenant_id = NULLIF(current_setting('scos.tenant_id',true),'')::uuid) WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id',true),'')::uuid)",
	}
	for _, stmt := range statements {
		if _, err := admin.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	// A dedicated, single-connection pool pins the test-only search_path.
	if _, err := app.ExecContext(ctx, "SET search_path TO "+schema+",public"); err != nil {
		t.Fatal(err)
	}
	var tenantA, tenantB string
	if err := admin.QueryRowContext(ctx, "SELECT gen_random_uuid()::text,gen_random_uuid()::text").Scan(&tenantA, &tenantB); err != nil {
		t.Fatal(err)
	}
	store := newPGCodeStore(app)
	repo, err := store.EnsureRepository(ctx, tenantA, "/scope-test")
	if err != nil {
		t.Fatalf("restricted role must register repository with transaction-local tenant: %v", err)
	}
	snap, err := store.CreateSnapshot(ctx, repo, tenantA, "main", "commit1", "test-v1", "")
	if err != nil {
		t.Fatalf("restricted role must create scoped snapshot: %v", err)
	}
	if repo == "" || snap == "" {
		t.Fatalf("missing IDs: repo=%q snap=%q", repo, snap)
	}
	building, err := store.IndexingSnapshot(ctx, repo, tenantA)
	if err != nil || building != snap {
		t.Fatalf("tenant-scoped indexing snapshot = %q, %v; want %q", building, err, snap)
	}
	if _, err := admin.ExecContext(ctx, "UPDATE "+schema+".code_repository_snapshots SET status='active',activated_at=now() WHERE id=$1", snap); err != nil {
		t.Fatal(err)
	}
	var symbolID, versionID string
	if err := admin.QueryRowContext(ctx, "INSERT INTO "+schema+".code_symbols(repository_id,stable_key,kind,first_snapshot_id) VALUES($1,'scope-file','file',$2) RETURNING id::text", repo, snap).Scan(&symbolID); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRowContext(ctx, "INSERT INTO "+schema+".code_symbol_versions(symbol_id,snapshot_id,fully_qualified_name,path,content_hash) VALUES($1,$2,'pkg/scope.go','pkg/scope.go','test-hash') RETURNING id::text", symbolID, snap).Scan(&versionID); err != nil {
		t.Fatal(err)
	}
	// Historical resolution and its symbol read must share a tenant-scoped
	// transaction. This test is RED if either query uses a bare pool connection.
	history, err := store.QueryHistoricalCode(ctx, tenantA, repo, "main", "commit1", "", "pkg/scope.go", 10)
	if err != nil {
		t.Fatalf("historical query under forced RLS: %v", err)
	}
	hits, ok := history["results"].([]PGSymbolVersion)
	if !ok || len(hits) != 1 || hits[0].SymbolVersionID != versionID {
		t.Fatalf("historical readback under forced RLS: %+v", history)
	}
	if _, err := store.QueryHistoricalCode(ctx, tenantB, repo, "main", "commit1", "", "pkg/scope.go", 10); err != errHistoricalNotFound {
		t.Fatalf("foreign tenant historical query should not resolve: %v", err)
	}
	// Current exact and lexical reads must share transaction-local tenant context.
	if _, err := admin.ExecContext(ctx, "UPDATE "+schema+".code_symbol_versions SET search_vector=to_tsvector('simple','pkg scope go') WHERE id=$1", versionID); err != nil {
		t.Fatal(err)
	}
	exact, err := store.QueryByPathOrFQNScoped(ctx, tenantA, repo, "main", "pkg/scope.go")
	if err != nil || len(exact) != 1 || exact[0].SymbolVersionID != versionID {
		t.Fatalf("own-tenant exact readback: %+v, %v", exact, err)
	}
	exact, err = store.QueryByPathOrFQNScoped(ctx, tenantB, repo, "main", "pkg/scope.go")
	if err != nil || len(exact) != 0 {
		t.Fatalf("foreign-tenant exact readback: %+v, %v", exact, err)
	}
	lexical, err := store.QueryLexicalScoped(ctx, tenantA, repo, "main", "scope", 10)
	if err != nil || len(lexical) != 1 || lexical[0].SymbolVersionID != versionID {
		t.Fatalf("own-tenant lexical readback: %+v, %v", lexical, err)
	}
	lexical, err = store.QueryLexicalScoped(ctx, tenantB, repo, "main", "scope", 10)
	if err != nil || len(lexical) != 0 {
		t.Fatalf("foreign-tenant lexical readback: %+v, %v", lexical, err)
	}
	branch, err := store.LatestIndexedBranch(ctx, repo, tenantA)
	if err != nil || branch != "main" {
		t.Fatalf("tenant-scoped latest branch = %q, %v; want main", branch, err)
	}
	// These are the HTTP handler's repository-ownership and idempotent
	// snapshot lookups. They must work with forced RLS, and foreign
	// repositories must be indistinguishable from missing ones.
	path, err := store.RepositoryPathForTenant(ctx, tenantA, repo)
	if err != nil || path != "/scope-test" {
		t.Fatalf("scoped repository path = %q, %v", path, err)
	}
	if _, err := store.RepositoryPathForTenant(ctx, tenantB, repo); err != sql.ErrNoRows {
		t.Fatalf("foreign repository path should be invisible: %v", err)
	}
	id, status, err := store.SnapshotByKey(ctx, tenantA, repo, "main", "commit1", "test-v1", "")
	if err != nil || id != snap || status != "active" {
		t.Fatalf("scoped snapshot lookup = %q, %q, %v", id, status, err)
	}
	id, status, err = store.SnapshotByKey(ctx, tenantB, repo, "main", "commit1", "test-v1", "")
	if err != nil || id != "" || status != "" {
		t.Fatalf("foreign snapshot lookup leaked: %q, %q, %v", id, status, err)
	}
	if _, err := store.RepositoryPathForTenant(ctx, "not-a-uuid", repo); err == nil {
		t.Fatal("repository lookup accepted invalid tenant")
	}
	if branch, err := store.LatestIndexedBranch(ctx, repo, tenantB); err != nil || branch != "" {
		t.Fatalf("foreign tenant latest branch = %q, %v; want empty", branch, err)
	}
	var visible int
	if err := app.QueryRowContext(ctx, "SELECT count(*) FROM code_repositories").Scan(&visible); err != nil {
		t.Fatal(err)
	}
	if visible != 0 {
		t.Fatalf("tenant context leaked outside transaction: %d repositories visible", visible)
	}
	if err := app.QueryRowContext(ctx, "SELECT count(*) FROM code_repository_snapshots").Scan(&visible); err != nil {
		t.Fatal(err)
	}
	if visible != 0 {
		t.Fatalf("tenant context leaked outside transaction: %d snapshots visible", visible)
	}
	for _, table := range []string{"code_symbols", "code_symbol_versions"} {
		if err := app.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&visible); err != nil {
			t.Fatal(err)
		}
		if visible != 0 {
			t.Fatalf("tenant context leaked outside transaction: %d %s visible", visible, table)
		}
	}
	if _, err := store.CreateSnapshot(ctx, repo, tenantB, "main", "foreign", "test-v1", ""); err == nil {
		t.Fatal("foreign tenant must not create snapshot in another tenant's repository")
	}
	if err := store.withCodeTenantContext(ctx, tenantA, func(tx *sql.Tx) error {
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM code_repositories WHERE id=$1", repo).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			return fmt.Errorf("own tenant saw %d repositories", count)
		}
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM code_repository_snapshots WHERE id=$1", snap).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			return fmt.Errorf("own tenant saw %d snapshots", count)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.withCodeTenantContext(ctx, tenantB, func(tx *sql.Tx) error {
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM code_repositories WHERE id=$1", repo).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("foreign tenant saw %d repositories", count)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.withCodeTenantContext(ctx, "not-a-uuid", func(*sql.Tx) error { t.Fatal("invalid tenant callback ran"); return nil }); err == nil || !strings.Contains(err.Error(), "tenant") {
		t.Fatalf("invalid tenant ID should be rejected before DB work: %v", err)
	}
}
