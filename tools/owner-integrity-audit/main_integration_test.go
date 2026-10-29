//go:build integration

package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/dylmarriner/scos-memory/packages/database"
	"github.com/jackc/pgx/v5"
)

func auditFixtureTx(t *testing.T) (context.Context, pgx.Tx) {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if err := database.RequireTLS(dsn); err != nil {
		t.Fatal(err)
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
