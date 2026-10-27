package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestPGSettingsForcedRLSTenantScoped uses a restricted non-BYPASSRLS role
// and an isolated forced-RLS table, without altering production policy state.
func TestPGSettingsForcedRLSTenantScoped(t *testing.T) {
	adminDSN, appDSN := os.Getenv("SCOS_CODE_STORE_TEST_DSN"), os.Getenv("SCOS_APP_TEST_DATABASE_URL")
	if adminDSN == "" || appDSN == "" {
		t.Skip("requires disposable admin and restricted-role PostgreSQL DSNs")
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
	schema := fmt.Sprintf("p6_settings_scope_%d", time.Now().UnixNano())
	statements := []string{
		"CREATE SCHEMA " + schema,
		"CREATE TABLE " + schema + ".code_intelligence_user_settings (tenant_id uuid NOT NULL, principal_id text NOT NULL, durable_store_enabled boolean NOT NULL DEFAULT true, updated_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY (tenant_id, principal_id))",
		"GRANT USAGE ON SCHEMA " + schema + " TO scos_app",
		"GRANT SELECT, INSERT, UPDATE ON " + schema + ".code_intelligence_user_settings TO scos_app",
		"ALTER TABLE " + schema + ".code_intelligence_user_settings ENABLE ROW LEVEL SECURITY",
		"ALTER TABLE " + schema + ".code_intelligence_user_settings FORCE ROW LEVEL SECURITY",
		"CREATE POLICY settings_tenant_scope ON " + schema + ".code_intelligence_user_settings FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid) WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid)",
	}
	for _, stmt := range statements {
		if _, err := admin.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	defer func() {
		if _, err := admin.ExecContext(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	}()
	if _, err := app.ExecContext(ctx, "SET search_path TO "+schema+",public"); err != nil {
		t.Fatal(err)
	}
	var tenantA, tenantB string
	if err := admin.QueryRowContext(ctx, "SELECT gen_random_uuid()::text, gen_random_uuid()::text").Scan(&tenantA, &tenantB); err != nil {
		t.Fatal(err)
	}
	store := newPGCodeStore(app)
	if err := store.SetUserEnabled(ctx, tenantA, "shared-principal", false); err != nil {
		t.Fatalf("owner scoped settings write: %v", err)
	}
	enabled, err := store.IsUserEnabled(ctx, tenantA, "shared-principal")
	if err != nil || enabled {
		t.Fatalf("owner scoped settings read = %v, %v; want disabled", enabled, err)
	}
	enabled, err = store.IsUserEnabled(ctx, tenantB, "shared-principal")
	if err != nil || !enabled {
		t.Fatalf("foreign tenant settings = %v, %v; want default enabled", enabled, err)
	}
	var count int
	if err := app.QueryRowContext(ctx, "SELECT count(*) FROM code_intelligence_user_settings").Scan(&count); err != nil || count != 0 {
		t.Fatalf("unscoped restricted-role read = %d, %v; want 0", count, err)
	}
	if err := store.SetUserEnabled(ctx, tenantB, "shared-principal", false); err != nil {
		t.Fatalf("other tenant write: %v", err)
	}
	if err := store.SetUserEnabled(ctx, tenantA, "shared-principal", true); err != nil {
		t.Fatalf("owner update: %v", err)
	}
	enabled, err = store.IsUserEnabled(ctx, tenantB, "shared-principal")
	if err != nil || enabled {
		t.Fatalf("other tenant changed by owner update = %v, %v", enabled, err)
	}
	if err := store.SetUserEnabled(ctx, "invalid-tenant", "shared-principal", false); err == nil {
		t.Fatal("invalid tenant UUID accepted")
	}
	if err := app.QueryRowContext(ctx, "SELECT count(*) FROM code_intelligence_user_settings").Scan(&count); err != nil || count != 0 {
		t.Fatalf("tenant context leaked after failed operation: %d, %v", count, err)
	}
}

func TestPGSettingsProductionPolicyForced(t *testing.T) {
	_, db := testPGStore(t)
	var enabled, forced bool
	if err := db.QueryRowContext(t.Context(), `SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE oid='public.code_intelligence_user_settings'::regclass`).Scan(&enabled, &forced); err != nil {
		t.Fatal(err)
	}
	if !enabled || !forced {
		t.Fatalf("production settings table must have ENABLE/FORCE RLS; enabled=%v forced=%v", enabled, forced)
	}
	var count int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM pg_policies WHERE schemaname='public' AND tablename='code_intelligence_user_settings' AND cmd='ALL'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected one tenant-scoped settings policy, got %d", count)
	}
}
