//go:build integration

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jackc/pgx/v5"
)

func auditFixtureTx(t *testing.T) (context.Context, pgx.Tx) {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Fatal("DATABASE_URL is required for PostgreSQL owner-integrity integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(ctx) })
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tx.Rollback(ctx) })
	return ctx, tx
}

func TestOwnerTenantAuditDetectsLegacyViolationsWithoutMutatingData(t *testing.T) {
	ctx, tx := auditFixtureTx(t)
	schema := "scos_owner_audit_fixture_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	for _, sql := range []string{
		"CREATE SCHEMA " + schema,
		"CREATE TABLE " + schema + ".principals (id uuid PRIMARY KEY, tenant_id uuid NOT NULL, UNIQUE (tenant_id,id))",
		"CREATE TABLE " + schema + ".memory_items (id uuid PRIMARY KEY, tenant_id uuid NOT NULL, owner_principal_id uuid NOT NULL)",
		"INSERT INTO " + schema + ".principals VALUES ('00000000-0000-4000-8000-000000000001','10000000-0000-4000-8000-000000000001'), ('00000000-0000-4000-8000-000000000002','10000000-0000-4000-8000-000000000002')",
		"INSERT INTO " + schema + ".memory_items VALUES ('20000000-0000-4000-8000-000000000001','10000000-0000-4000-8000-000000000001','00000000-0000-4000-8000-000000000001'), ('20000000-0000-4000-8000-000000000002','10000000-0000-4000-8000-000000000001','00000000-0000-4000-8000-000000000002'), ('20000000-0000-4000-8000-000000000003','10000000-0000-4000-8000-000000000001','00000000-0000-4000-8000-000000000003')",
		"ALTER TABLE " + schema + ".memory_items ADD CONSTRAINT memory_items_owner_tenant_fk FOREIGN KEY (tenant_id, owner_principal_id) REFERENCES " + schema + ".principals(tenant_id,id) NOT VALID",
	} {
		if _, err := tx.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	report, err := auditOwnerTenant(ctx, tx, schema)
	if err != nil {
		t.Fatal(err)
	}
	if report.TotalRows != 3 || report.CrossTenantOwners != 1 || report.MissingOwners != 1 || !report.ConstraintPresent || report.ConstraintValidated {
		t.Fatalf("legacy violations missed: %+v", report)
	}
	var owner string
	if err := tx.QueryRow(ctx, "SELECT owner_principal_id::text FROM "+schema+".memory_items WHERE id='20000000-0000-4000-8000-000000000002'").Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != "00000000-0000-4000-8000-000000000002" {
		t.Fatalf("audit changed legacy owner: %s", owner)
	}
	// Only the test repairs its synthetic rows; the audit is read-only.
	if _, err := tx.Exec(ctx, "UPDATE "+schema+".memory_items SET owner_principal_id='00000000-0000-4000-8000-000000000001' WHERE id IN ('20000000-0000-4000-8000-000000000002','20000000-0000-4000-8000-000000000003')"); err != nil {
		t.Fatal(err)
	}
	report, err = auditOwnerTenant(ctx, tx, schema)
	if err != nil {
		t.Fatal(err)
	}
	if report.TotalRows != 3 || report.CrossTenantOwners != 0 || report.MissingOwners != 0 || report.ConstraintValidated {
		t.Fatalf("remediated but unvalidated constraint misreported: %+v", report)
	}
	if _, err := tx.Exec(ctx, "ALTER TABLE "+schema+".memory_items VALIDATE CONSTRAINT memory_items_owner_tenant_fk"); err != nil {
		t.Fatal(err)
	}
	report, err = auditOwnerTenant(ctx, tx, schema)
	if err != nil {
		t.Fatal(err)
	}
	if !report.ConstraintValidated || report.CrossTenantOwners != 0 || report.MissingOwners != 0 {
		t.Fatalf("validated constraint misreported: %+v", report)
	}
}

func TestOwnerTenantAuditFailsClosedOnForcedRLS(t *testing.T) {
	ctx, tx := auditFixtureTx(t)
	schema := "scos_owner_audit_rls_fixture_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	role := "scos_owner_audit_reader_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	for _, sql := range []string{
		"CREATE SCHEMA " + schema,
		"CREATE TABLE " + schema + ".principals (id uuid PRIMARY KEY, tenant_id uuid NOT NULL, UNIQUE(tenant_id,id))",
		"CREATE TABLE " + schema + ".memory_items (id uuid PRIMARY KEY, tenant_id uuid NOT NULL, owner_principal_id uuid NOT NULL)",
		"ALTER TABLE " + schema + ".memory_items ENABLE ROW LEVEL SECURITY",
		"ALTER TABLE " + schema + ".memory_items FORCE ROW LEVEL SECURITY",
		"CREATE POLICY hide_rows ON " + schema + ".memory_items FOR SELECT USING (false)",
		"CREATE ROLE " + role + " NOLOGIN",
		"GRANT USAGE ON SCHEMA " + schema + " TO " + role,
		"GRANT SELECT ON " + schema + ".memory_items, " + schema + ".principals TO " + role,
		"SET LOCAL ROLE " + role,
		"SET LOCAL row_security=off",
	} {
		if _, err := tx.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := auditOwnerTenant(ctx, tx, schema); err == nil {
		t.Fatal("audit silently accepted RLS-filtered inventory")
	}
}

func TestOwnerTenantAuditRejectsForgedConstraintName(t *testing.T) {
	ctx, tx := auditFixtureTx(t)
	schema := "scos_owner_audit_wrong_fk_fixture_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	for _, sql := range []string{
		"CREATE SCHEMA " + schema,
		"CREATE TABLE " + schema + ".principals (id uuid PRIMARY KEY, tenant_id uuid NOT NULL, UNIQUE(tenant_id,id))",
		"CREATE TABLE " + schema + ".memory_items (id uuid PRIMARY KEY, tenant_id uuid NOT NULL, owner_principal_id uuid NOT NULL)",
		"ALTER TABLE " + schema + ".memory_items ADD CONSTRAINT memory_items_owner_tenant_fk FOREIGN KEY (owner_principal_id) REFERENCES " + schema + ".principals(id) NOT VALID",
	} {
		if _, err := tx.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := auditOwnerTenant(ctx, tx, schema); err == nil {
		t.Fatal("audit trusted a same-named FK with the wrong columns")
	}
}

func TestOwnerTenantAuditRejectsAlteredReferentialAction(t *testing.T) {
	ctx, tx := auditFixtureTx(t)
	schema := "scos_owner_audit_action_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	for _, statement := range []string{
		"CREATE SCHEMA " + schema,
		"CREATE TABLE " + schema + ".principals (id uuid PRIMARY KEY, tenant_id uuid NOT NULL, UNIQUE(tenant_id,id))",
		"CREATE TABLE " + schema + ".memory_items (id uuid PRIMARY KEY, tenant_id uuid NOT NULL, owner_principal_id uuid NOT NULL)",
		"ALTER TABLE " + schema + ".memory_items ADD CONSTRAINT memory_items_owner_tenant_fk FOREIGN KEY (tenant_id, owner_principal_id) REFERENCES " + schema + ".principals(tenant_id,id) ON DELETE CASCADE NOT VALID",
	} {
		if _, err := tx.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := auditOwnerTenant(ctx, tx, schema); err == nil {
		t.Fatal("audit accepted a same-named FK with ON DELETE CASCADE")
	}
}

func TestBeginAuditTxIsReadOnlyRepeatableRead(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Fatal("DATABASE_URL required")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	tx, err := beginAuditTx(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var readOnly, isolation string
	if err := tx.QueryRow(ctx, "SELECT current_setting('transaction_read_only'), current_setting('transaction_isolation')").Scan(&readOnly, &isolation); err != nil {
		t.Fatal(err)
	}
	if readOnly != "on" || isolation != "repeatable read" {
		t.Fatalf("unsafe audit transaction: read_only=%q isolation=%q", readOnly, isolation)
	}
	if _, err := tx.Exec(ctx, "CREATE TEMP TABLE scos_audit_must_not_write(id integer)"); err == nil {
		t.Fatal("read-only transaction allowed CREATE TEMP TABLE")
	}
}

// The subprocess uses a committed public-schema fixture in an explicitly
// disposable TLS-enabled database, never a shared or live database.
func TestAuditExecutableExitStatuses(t *testing.T) {
	dsn := os.Getenv("SCOS_OWNER_AUDIT_TLS_TEST_DSN")
	if dsn == "" {
		t.Skip("TLS disposable database not configured")
	}
	if os.Getenv("SCOS_OWNER_AUDIT_DISPOSABLE") != "1" {
		t.Fatal("explicit disposable database opt-in required")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(ctx) })
	var dbname, ssl string
	if err := conn.QueryRow(ctx, "SELECT current_database(), (SELECT ssl::text FROM pg_stat_ssl WHERE pid=pg_backend_pid())").Scan(&dbname, &ssl); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dbname, "scos_owner_audit_test_") || ssl != "true" {
		t.Fatalf("refusing non-disposable or non-TLS database %q (TLS %q)", dbname, ssl)
	}
	var existing int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM pg_catalog.pg_class WHERE oid IN (to_regclass('public.memory_items'), to_regclass('public.principals'))").Scan(&existing); err != nil {
		t.Fatal(err)
	}
	if existing != 0 {
		t.Fatal("refusing to overwrite pre-existing tables")
	}
	t.Cleanup(func() {
		conn.Exec(ctx, "DROP TABLE IF EXISTS public.memory_items")
		conn.Exec(ctx, "DROP TABLE IF EXISTS public.principals")
	})
	for _, stmt := range []string{
		"CREATE TABLE public.principals (id uuid PRIMARY KEY, tenant_id uuid NOT NULL, UNIQUE(tenant_id,id))",
		"CREATE TABLE public.memory_items (id uuid PRIMARY KEY, tenant_id uuid NOT NULL, owner_principal_id uuid NOT NULL)",
		"INSERT INTO public.principals VALUES ('00000000-0000-4000-8000-000000000001','10000000-0000-4000-8000-000000000001'), ('00000000-0000-4000-8000-000000000002','10000000-0000-4000-8000-000000000002')",
		"INSERT INTO public.memory_items VALUES ('20000000-0000-4000-8000-000000000001','10000000-0000-4000-8000-000000000001','00000000-0000-4000-8000-000000000001'), ('20000000-0000-4000-8000-000000000002','10000000-0000-4000-8000-000000000001','00000000-0000-4000-8000-000000000002')",
		"ALTER TABLE public.memory_items ADD CONSTRAINT memory_items_owner_tenant_fk FOREIGN KEY (tenant_id, owner_principal_id) REFERENCES public.principals(tenant_id,id) NOT VALID",
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	binary := filepath.Join(t.TempDir(), "owner-audit")
	cmd := exec.Command("go", "build", "-o", binary, ".")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build audit: %v: %s", err, output)
	}
	check := func(want int, crossTenant int64) {
		t.Helper()
		cmd := exec.Command(binary)
		cmd.Env = append(os.Environ(), "DATABASE_URL="+dsn)
		output, err := cmd.CombinedOutput()
		got := 0
		if err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatal(err)
			}
			got = exit.ExitCode()
		}
		if got != want {
			t.Fatalf("exit %d want %d: %s", got, want, output)
		}
		var report ownerTenantAudit
		if err := json.Unmarshal(output, &report); err != nil {
			t.Fatalf("bad report: %v: %s", err, output)
		}
		if report.CrossTenantOwners != crossTenant {
			t.Fatalf("cross-tenant owners %d want %d", report.CrossTenantOwners, crossTenant)
		}
	}
	check(2, 1)
	if _, err := conn.Exec(ctx, "DELETE FROM public.memory_items WHERE id='20000000-0000-4000-8000-000000000002'"); err != nil {
		t.Fatal(err)
	}
	check(0, 0)
}
