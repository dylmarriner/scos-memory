package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dylmarriner/scos-memory/packages/auth"
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
		"CREATE TABLE " + schema + ".code_repository_snapshots (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), repository_id uuid NOT NULL, tenant_id uuid NOT NULL, branch text NOT NULL, commit_id text NOT NULL, extractor_version text NOT NULL, configuration_hash text NOT NULL, parent_snapshot_id uuid, status text NOT NULL DEFAULT 'queued', error text, symbol_count integer NOT NULL DEFAULT 0, created_at timestamptz NOT NULL DEFAULT now(), activated_at timestamptz, UNIQUE(repository_id,branch,commit_id,extractor_version,configuration_hash))",
		"CREATE TABLE " + schema + ".code_symbols (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), repository_id uuid NOT NULL, stable_key text NOT NULL, kind text NOT NULL, first_snapshot_id uuid NOT NULL)",
		"CREATE TABLE " + schema + ".code_file_versions (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), snapshot_id uuid NOT NULL, path text NOT NULL, content_hash text NOT NULL)",
		"CREATE TABLE " + schema + ".code_symbol_versions (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), symbol_id uuid NOT NULL, snapshot_id uuid NOT NULL, fully_qualified_name text NOT NULL, path text NOT NULL, kind text NOT NULL DEFAULT 'file', content_hash text NOT NULL, status text NOT NULL DEFAULT 'active', search_vector tsvector, extractor text NOT NULL DEFAULT '', signature text, doc_comment text)",
		"CREATE TABLE " + schema + ".code_edges (snapshot_id uuid NOT NULL, source_symbol_version_id uuid NOT NULL, target_symbol_version_id uuid NOT NULL, relation text NOT NULL)",
		"GRANT USAGE ON SCHEMA " + schema + " TO scos_app",
		"GRANT SELECT,INSERT,UPDATE,DELETE ON " + schema + ".code_repositories," + schema + ".code_repository_snapshots," + schema + ".code_symbols," + schema + ".code_symbol_versions TO scos_app",
		"GRANT SELECT ON " + schema + ".code_file_versions, " + schema + ".code_edges TO scos_app",
		"ALTER TABLE " + schema + ".code_edges ENABLE ROW LEVEL SECURITY",
		"ALTER TABLE " + schema + ".code_edges FORCE ROW LEVEL SECURITY",
		"CREATE POLICY edge_scope ON " + schema + ".code_edges USING (EXISTS (SELECT 1 FROM " + schema + ".code_repository_snapshots WHERE id=snapshot_id))",
		"ALTER TABLE " + schema + ".code_repositories ENABLE ROW LEVEL SECURITY",
		"ALTER TABLE " + schema + ".code_repositories FORCE ROW LEVEL SECURITY",
		"ALTER TABLE " + schema + ".code_repository_snapshots ENABLE ROW LEVEL SECURITY",
		"ALTER TABLE " + schema + ".code_repository_snapshots FORCE ROW LEVEL SECURITY",
		"ALTER TABLE " + schema + ".code_symbols ENABLE ROW LEVEL SECURITY",
		"ALTER TABLE " + schema + ".code_symbols FORCE ROW LEVEL SECURITY",
		"ALTER TABLE " + schema + ".code_file_versions ENABLE ROW LEVEL SECURITY",
		"ALTER TABLE " + schema + ".code_file_versions FORCE ROW LEVEL SECURITY",
		"ALTER TABLE " + schema + ".code_symbol_versions ENABLE ROW LEVEL SECURITY",
		"ALTER TABLE " + schema + ".code_symbol_versions FORCE ROW LEVEL SECURITY",
		"CREATE POLICY symbol_scope ON " + schema + ".code_symbols USING (EXISTS (SELECT 1 FROM " + schema + ".code_repositories WHERE id=repository_id)) WITH CHECK (EXISTS (SELECT 1 FROM " + schema + ".code_repositories WHERE id=repository_id))",
		"CREATE POLICY version_scope ON " + schema + ".code_symbol_versions USING (EXISTS (SELECT 1 FROM " + schema + ".code_repository_snapshots WHERE id=snapshot_id)) WITH CHECK (EXISTS (SELECT 1 FROM " + schema + ".code_repository_snapshots WHERE id=snapshot_id))",
		"CREATE POLICY file_scope ON " + schema + ".code_file_versions USING (EXISTS (SELECT 1 FROM " + schema + ".code_repository_snapshots WHERE id=snapshot_id))",
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
	// Status transitions must be scoped at the database boundary, not
	// merely protected by the HTTP repository ownership check.
	if err := store.SetSnapshotStatusScoped(ctx, tenantB, snap, "indexing", ""); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("foreign tenant changed snapshot status: %v", err)
	}
	if err := store.SetSnapshotStatusScoped(ctx, tenantA, snap, "indexing", ""); err != nil {
		t.Fatalf("own tenant status update under forced RLS: %v", err)
	}
	var scopedStatus string
	if err := admin.QueryRowContext(ctx, "SELECT status FROM "+schema+".code_repository_snapshots WHERE id=$1", snap).Scan(&scopedStatus); err != nil || scopedStatus != "indexing" {
		t.Fatalf("status after scoped update = %q, %v", scopedStatus, err)
	}
	if err := store.SetSnapshotStatusScoped(ctx, "not-a-uuid", snap, "failed", ""); err == nil {
		t.Fatal("invalid tenant accepted for snapshot status transition")
	}
	if _, err := admin.ExecContext(ctx, "INSERT INTO "+schema+".code_file_versions(snapshot_id,path,content_hash) VALUES($1,'scope.go','abc')", snap); err != nil {
		t.Fatal(err)
	}
	if same, err := store.SnapshotHoldsFilesScoped(ctx, tenantA, snap, []PGFileInput{{Path: "scope.go", ContentHash: "abc"}}); err != nil || !same {
		t.Fatalf("own tenant snapshot files = %v, %v", same, err)
	}
	if same, err := store.SnapshotHoldsFilesScoped(ctx, tenantB, snap, []PGFileInput{{Path: "scope.go", ContentHash: "abc"}}); !errors.Is(err, sql.ErrNoRows) || same {
		t.Fatalf("foreign tenant snapshot files = %v, %v", same, err)
	}
	if same, err := store.SnapshotHoldsFilesScoped(ctx, tenantA, snap, []PGFileInput{{Path: "scope.go", ContentHash: "different"}}); err != nil || same {
		t.Fatalf("mismatched snapshot manifest = %v, %v", same, err)
	}
	building, err := store.IndexingSnapshot(ctx, repo, tenantA)
	if err != nil || building != snap {
		t.Fatalf("tenant-scoped indexing snapshot = %q, %v; want %q", building, err, snap)
	}
	if _, err := admin.ExecContext(ctx, "UPDATE "+schema+".code_repository_snapshots SET status='active',activated_at=now() WHERE id=$1", snap); err != nil {
		t.Fatal(err)
	}
	// Reactivation must use the same transaction-local tenant scope as the
	// snapshot lock. An already-active snapshot avoids the activation function
	// while still exercising the restricted-role ownership boundary.
	if err := store.ReactivateSnapshotScoped(ctx, tenantA, snap); err != nil {
		t.Fatalf("own tenant reactivation under forced RLS: %v", err)
	}
	if err := store.ReactivateSnapshotScoped(ctx, tenantB, snap); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("foreign tenant reactivated snapshot: %v", err)
	}
	if err := store.ReactivateSnapshotScoped(ctx, "not-a-uuid", snap); err == nil {
		t.Fatal("invalid tenant accepted for reactivation")
	}
	var symbolID, versionID string
	if err := admin.QueryRowContext(ctx, "INSERT INTO "+schema+".code_symbols(repository_id,stable_key,kind,first_snapshot_id) VALUES($1,'scope-file','file',$2) RETURNING id::text", repo, snap).Scan(&symbolID); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRowContext(ctx, "INSERT INTO "+schema+".code_symbol_versions(symbol_id,snapshot_id,fully_qualified_name,path,content_hash) VALUES($1,$2,'pkg/scope.go','pkg/scope.go','test-hash') RETURNING id::text", symbolID, snap).Scan(&versionID); err != nil {
		t.Fatal(err)
	}

	// A retry of an already-indexed snapshot must read its symbol count
	// with the tenant context, not via a pooled connection. The old
	// implementation fails under FORCE RLS with "no rows in result set".
	if _, err := admin.ExecContext(ctx, "UPDATE "+schema+".code_repository_snapshots SET symbol_count=1 WHERE id=$1", snap); err != nil {
		t.Fatal(err)
	}
	retryResult, err := (&server{pgStore: store}).indexDurableSnapshot(ctx, tenantA, repo,
		"main", "commit1", "test-v1", "", []PGFileInput{{Path: "scope.go", ContentHash: "abc"}})
	if err != nil {
		t.Fatalf("own-tenant idempotent index retry under forced RLS: %v", err)
	}
	if retryResult["unchanged"] != true || retryResult["symbol_count"] != 1 {
		t.Fatalf("idempotent index retry lost symbol count: %+v", retryResult)
	}

	if count, err := store.SnapshotSymbolCountScoped(ctx, tenantA, snap); err != nil || count != 1 {
		t.Fatalf("own-tenant snapshot count = %d, %v", count, err)
	}
	if _, err := store.SnapshotSymbolCountScoped(ctx, tenantB, snap); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("foreign tenant read symbol count: %v", err)
	}
	if _, err := store.SnapshotSymbolCountScoped(ctx, "not-a-uuid", snap); err == nil {
		t.Fatal("invalid tenant accepted for symbol count")
	}

	// A snapshot diff must read the snapshot, parent and symbol versions
	// under one tenant-local transaction. Under forced RLS the old handler's
	// context-free identity query returned 404 even to the owning tenant.
	diffServer := &server{pgStore: store}
	diffPath := "/v2/code/snapshots/diff"
	for _, tc := range []struct {
		tenant string
		id     string
		want   int
	}{
		{tenantA, snap, http.StatusOK},
		{tenantB, snap, http.StatusNotFound},
		{tenantA, "00000000-0000-4000-8000-000000000001", http.StatusNotFound},
		{tenantA, "not-a-uuid", http.StatusBadRequest},
	} {
		resp := pgRequest(t, diffServer, http.MethodPost, diffPath, tc.tenant,
			map[string]any{"snapshot_id": tc.id})
		if resp.Code != tc.want {
			t.Errorf("forced-RLS diff tenant %s snapshot %s: got HTTP %d, want %d: %s",
				tc.tenant, tc.id, resp.Code, tc.want, resp.Body.String())
		}
		if tc.want == http.StatusOK && resp.Code == http.StatusOK {
			var result SnapshotDiff
			if err := json.Unmarshal(resp.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.SnapshotID != snap || len(result.Added) != 1 || result.Added[0].Path != "pkg/scope.go" {
				t.Errorf("own-tenant snapshot diff: %+v", result)
			}
		}
	}
	// A second JSON object must not be silently ignored by the diff route.
	raw := httptest.NewRequest(http.MethodPost, diffPath, strings.NewReader(
		fmt.Sprintf("{%q:%q}{%q:%q}", "snapshot_id", snap, "snapshot_id", snap)))
	raw = raw.WithContext(auth.ContextWithClaims(raw.Context(),
		auth.Claims{TenantID: tenantA, PrincipalID: "p"}))
	rawResult := httptest.NewRecorder()
	diffServer.handlePGSnapshotDiff(rawResult, raw)
	if rawResult.Code != http.StatusBadRequest {
		t.Fatalf("diff accepted multiple JSON objects: HTTP %d: %s", rawResult.Code, rawResult.Body.String())
	}

	// A second snapshot must read its parent in the same scoped transaction.
	next, err := store.CreateSnapshot(ctx, repo, tenantA, "main", "commit2", "test-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(ctx, "INSERT INTO "+schema+".code_symbol_versions(symbol_id,snapshot_id,fully_qualified_name,path,content_hash) VALUES($1,$2,'pkg/scope.go','pkg/scope.go','changed-hash')", symbolID, next); err != nil {
		t.Fatal(err)
	}
	resp := pgRequest(t, diffServer, http.MethodPost, diffPath, tenantA,
		map[string]any{"snapshot_id": next})
	if resp.Code != http.StatusOK {
		t.Fatalf("parent diff under forced RLS: HTTP %d: %s", resp.Code, resp.Body.String())
	}
	var parentDiff SnapshotDiff
	if err := json.Unmarshal(resp.Body.Bytes(), &parentDiff); err != nil {
		t.Fatal(err)
	}
	if parentDiff.ParentSnapshotID != snap || len(parentDiff.Modified) != 1 ||
		parentDiff.Modified[0].Path != "pkg/scope.go" {
		t.Fatalf("parent snapshot diff lost lineage: %+v", parentDiff)
	}

	// Failed snapshot cleanup must not delete another tenant's generation.
	failed, err := store.CreateSnapshot(ctx, repo, tenantA, "main", "failed-commit", "test-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetSnapshotStatusScoped(ctx, tenantA, failed, "failed", "synthetic"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteFailedSnapshotScoped(ctx, tenantB, failed); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("foreign tenant deleted failed snapshot: %v", err)
	}
	if err := store.DeleteFailedSnapshotScoped(ctx, tenantA, failed); err != nil {
		t.Fatalf("own tenant failed snapshot cleanup: %v", err)
	}
	var remaining int
	if err := admin.QueryRowContext(ctx, "SELECT count(*) FROM "+schema+".code_repository_snapshots WHERE id=$1", failed).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("failed snapshot cleanup left %d rows, %v", remaining, err)
	}
	// The event API must resolve snapshot identity under the same tenant
	// transaction as its ledger read. Without this, forced RLS hides an
	// authorized snapshot during the initial pooled-connection lookup.
	eventServer := &server{pgStore: store}
	for _, tc := range []struct {
		tenant string
		want   int
	}{
		{tenantA, http.StatusOK},
		{tenantB, http.StatusNotFound},
	} {
		resp := pgRequest(t, eventServer, http.MethodPost,
			"/v2/code/snapshots/events", tc.tenant,
			map[string]any{"snapshot_id": snap})
		if resp.Code != tc.want {
			t.Fatalf("forced-RLS event read tenant %s: got HTTP %d, want %d: %s",
				tc.tenant, resp.Code, tc.want, resp.Body.String())
		}
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
	// The impact walk must keep the tenant identity across seed lookup,
	// reverse-CALLS traversal, and graph ranking. Previously only the
	// preliminary repository check was scoped; forced RLS hid the seeds.
	var callerSymbol, callerVersion string
	if err := admin.QueryRowContext(ctx, "INSERT INTO "+schema+".code_symbols(repository_id,stable_key,kind,first_snapshot_id) VALUES($1,'scope-caller','function',$2) RETURNING id::text", repo, snap).Scan(&callerSymbol); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRowContext(ctx, "INSERT INTO "+schema+".code_symbol_versions(symbol_id,snapshot_id,fully_qualified_name,path,content_hash,kind) VALUES($1,$2,'pkg.Caller','pkg/caller.go','caller-hash','function') RETURNING id::text", callerSymbol, snap).Scan(&callerVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(ctx, "INSERT INTO "+schema+".code_edges(snapshot_id,source_symbol_version_id,target_symbol_version_id,relation) VALUES($1,$2,$3,'CALLS')", snap, callerVersion, versionID); err != nil {
		t.Fatal(err)
	}
	impact, err := store.ChangeImpactScoped(ctx, tenantA, repo, "main", "pkg/scope.go", 2)
	if err != nil {
		t.Fatalf("own-tenant forced-RLS impact failed: %v", err)
	}
	if len(impact.DirectDependents) != 1 || impact.DirectDependents[0].SymbolVersionID != callerVersion ||
		len(impact.CodeEvidencePaths[callerVersion]) != 2 {
		t.Fatalf("own-tenant forced-RLS impact: %+v, %v", impact, err)
	}
	if _, err := store.ChangeImpactScoped(ctx, tenantB, repo, "main", "pkg/scope.go", 2); !errors.Is(err, ErrSymbolNotFound) {
		t.Fatalf("foreign tenant impact should not find symbols: %v", err)
	}
	historicalImpact, err := store.ChangeImpactHistorical(ctx, tenantA, repo, "main", "commit1", "", "pkg/scope.go", 2)
	if err != nil {
		t.Fatalf("own-tenant historical forced-RLS impact failed: %v", err)
	}
	if len(historicalImpact.DirectDependents) != 1 || historicalImpact.DirectDependents[0].SymbolVersionID != callerVersion {
		t.Fatalf("own-tenant historical forced-RLS impact: %+v, %v", historicalImpact, err)
	}
	if _, err := store.ChangeImpactHistorical(ctx, tenantB, repo, "main", "commit1", "", "pkg/scope.go", 2); !errors.Is(err, errHistoricalNotFound) {
		t.Fatalf("foreign tenant historical impact: %v", err)
	}
	if _, err := store.ChangeImpactScoped(ctx, "not-a-uuid", repo, "main", "pkg/scope.go", 2); err == nil {
		t.Fatal("invalid tenant accepted for impact")
	}
	if _, err := store.ChangeImpactHistorical(ctx, tenantA, repo, "main", "", "", "pkg/scope.go", 2); err == nil {
		t.Fatal("historical impact accepted missing immutable snapshot selector")
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

// TestPGScopedIndexingAsRestrictedRole exercises real file and SCIP indexing
// using the non-BYPASSRLS runtime role, including foreign-tenant denial.
// Unlike the shadow-schema read tests, this covers the full migrated schema.
func TestPGScopedIndexingAsRestrictedRole(t *testing.T) {
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
	store := newPGCodeStore(app)
	tenantA := testTenant(t, admin)
	tenantB := testTenant(t, admin)
	repo, err := store.EnsureRepository(ctx, tenantA, "/p6-scoped-index/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	snap, err := store.CreateSnapshot(ctx, repo, tenantA, "main", "file-index", "go-parser", "")
	if err != nil {
		t.Fatal(err)
	}
	source := "package pkg\nfunc ScopedIndex() {}\n"
	files := []PGFileInput{{Path: "pkg/index.go", Language: "go", ContentHash: hashContent(source), Content: source}}
	if err := store.IndexSnapshotScoped(ctx, tenantB, snap, repo, files); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("foreign tenant indexed file snapshot: %v", err)
	}
	if err := store.IndexSnapshotScoped(ctx, tenantA, snap, repo, files); err != nil {
		t.Fatalf("own tenant file indexing: %v", err)
	}
	results, err := store.QueryByPathOrFQNScoped(ctx, tenantA, repo, "main", "pkg/index.go")
	if err != nil || len(results) == 0 {
		t.Fatalf("file indexing lost owner-visible evidence: %+v, %v", results, err)
	}
	// Transaction-local context must not survive on the pooled connection.
	var leaked sql.NullString
	if err := app.QueryRowContext(ctx, "SELECT current_setting('scos.tenant_id',true)").Scan(&leaked); err != nil {
		t.Fatal(err)
	}
	if leaked.Valid && leaked.String != "" {
		t.Fatalf("tenant identity leaked outside indexing transaction")
	}
	scipSnap, err := store.CreateSnapshot(ctx, repo, tenantA, "main", "scip-index", "scip", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.IndexSnapshotFromSCIPScoped(ctx, tenantB, scipSnap, repo, sampleSCIPIndex()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("foreign tenant indexed SCIP snapshot: %v", err)
	}
	count, err := store.IndexSnapshotFromSCIPScoped(ctx, tenantA, scipSnap, repo, sampleSCIPIndex())
	if err != nil || count == 0 {
		t.Fatalf("own tenant SCIP indexing: count=%d err=%v", count, err)
	}
}

// TestPGCodeProvenanceForcedRLSQualification is an opt-in release gate for a
// disposable database with the candidate four-table policies installed.
// It is intentionally not enabled by ordinary CI until the coordinated
// production policy migration is ready for review.
func TestPGCodeProvenanceForcedRLSQualification(t *testing.T) {
	if os.Getenv("SCOS_EXPECT_CODE_PROVENANCE_RLS") != "1" {
		t.Skip("candidate code-provenance RLS policies not yet in production migrations")
	}
	adminDSN := os.Getenv("SCOS_CODE_STORE_TEST_DSN")
	appDSN := os.Getenv("SCOS_APP_TEST_DATABASE_URL")
	if adminDSN == "" || appDSN == "" {
		t.Fatal("forced-RLS qualification requires both owner and restricted-role DSNs")
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
	owner := newPGCodeStore(admin)
	tenant := testTenant(t, admin)
	repo, err := owner.EnsureRepository(ctx, tenant, "/p6-forced-rls/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	snap, err := owner.CreateSnapshot(ctx, repo, tenant, "main", "rls-qualification", "go-parser", "")
	if err != nil {
		t.Fatal(err)
	}
	source := "package pkg\nfunc Provenance() {}\n"
	if err := owner.IndexSnapshot(ctx, snap, repo, []PGFileInput{{Path: "pkg/rls.go", Language: "go", ContentHash: hashContent(source), Content: source}}); err != nil {
		t.Fatal(err)
	}
	tables := []string{"code_repositories", "code_repository_snapshots", "code_symbols", "code_symbol_versions"}
	for _, table := range tables {
		var enabled, forced bool
		if err := admin.QueryRowContext(ctx, "SELECT relrowsecurity,relforcerowsecurity FROM pg_class WHERE oid=$1::regclass", table).Scan(&enabled, &forced); err != nil {
			t.Fatal(err)
		}
		if !enabled || !forced {
			t.Fatalf("%s missing enforced RLS", table)
		}
		var visible int
		if err := app.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&visible); err != nil {
			t.Fatal(err)
		}
		if visible != 0 {
			t.Fatalf("unscoped scos_app can read %d %s rows", visible, table)
		}
	}
	store := newPGCodeStore(app)
	if err := store.withCodeTenantContext(ctx, tenant, func(tx *sql.Tx) error {
		for _, table := range tables {
			var visible int
			if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&visible); err != nil {
				return err
			}
			if visible == 0 {
				return fmt.Errorf("owner cannot see %s under forced RLS", table)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
