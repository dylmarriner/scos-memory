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
		"GRANT USAGE ON SCHEMA " + schema + " TO scos_app",
		"GRANT SELECT,INSERT,UPDATE ON " + schema + ".code_repositories," + schema + ".code_repository_snapshots TO scos_app",
		"ALTER TABLE " + schema + ".code_repositories ENABLE ROW LEVEL SECURITY",
		"ALTER TABLE " + schema + ".code_repositories FORCE ROW LEVEL SECURITY",
		"ALTER TABLE " + schema + ".code_repository_snapshots ENABLE ROW LEVEL SECURITY",
		"ALTER TABLE " + schema + ".code_repository_snapshots FORCE ROW LEVEL SECURITY",
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
