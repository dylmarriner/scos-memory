package main

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// This is a database-level P5 prerequisite, not an as_of_system_time API.
// The event timestamp records a transition observation, not commit ordering.
func TestP5MemorySystemEventsAreAppendOnlyAndTenantIsolated(t *testing.T) {
	ctx := context.Background()
	admin := testRetrievalPool(t)
	dsn := os.Getenv("SCOS_APP_TEST_DATABASE_URL")
	if dsn == "" {
		t.Fatal("SCOS_APP_TEST_DATABASE_URL required for P5 ledger RLS acceptance")
	}
	app, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	if err := app.Ping(ctx); err != nil {
		t.Fatal(err)
	}

	seed := func(name string) (tenant, principal, ns, memory string) {
		t.Helper()
		if err := admin.QueryRow(ctx, `INSERT INTO tenants(name) VALUES($1) RETURNING id`, name).Scan(&tenant); err != nil {
			t.Fatal(err)
		}
		if err := admin.QueryRow(ctx, `INSERT INTO principals(tenant_id,kind,external_id,display_name)
            VALUES($1,'human',$2,$2) RETURNING id`, tenant, name).Scan(&principal); err != nil {
			t.Fatal(err)
		}
		if err := admin.QueryRow(ctx, `INSERT INTO namespaces(tenant_id,owner_principal_id,canonical_name,sensitivity_ceiling)
            VALUES($1,$2,$3,'internal') RETURNING id`, tenant, principal, name).Scan(&ns); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.Exec(ctx, `INSERT INTO namespace_memberships(namespace_id,principal_id,can_read,can_write_candidates)
            VALUES($1,$2,true,true)`, ns, principal); err != nil {
			t.Fatal(err)
		}
		if err := admin.QueryRow(ctx, `INSERT INTO memory_items(tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,authority_level,confidence_score,sensitivity)
            VALUES($1,$2,$3,'semantic','candidate','fact','fact',50,0.9,'internal') RETURNING id`, tenant, ns, principal).Scan(&memory); err != nil {
			t.Fatal(err)
		}
		return
	}
	tenant, principal, ns, memory := seed(t.Name() + "-a")
	otherTenant, otherPrincipal, _, otherMemory := seed(t.Name() + "-b")

	begin := func(tenant, principal string) pgx.Tx {
		t.Helper()
		tx, err := app.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
		for k, v := range map[string]string{"scos.tenant_id": tenant, "scos.principal_id": principal} {
			if _, err := tx.Exec(ctx, `SELECT set_config($1,$2,true)`, k, v); err != nil {
				t.Fatal(err)
			}
		}
		return tx
	}
	assertCount := func(tx pgx.Tx, query string, want int, args ...any) {
		t.Helper()
		var got int
		if err := tx.QueryRow(ctx, query, args...).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("query=%s count=%d want=%d", query, got, want)
		}
	}
	tx := begin(tenant, principal)
	assertCount(tx, `SELECT count(*) FROM memory_system_events WHERE memory_id=$1 AND operation='insert' AND status='candidate'`, 1, memory)
	assertCount(tx, `SELECT count(*) FROM memory_system_events WHERE memory_id=$1`, 0, otherMemory)
	_, err = tx.Exec(ctx, `INSERT INTO memory_system_events(
        tenant_id,memory_id,namespace_id,owner_principal_id,memory_type,status,
        authority_level,confidence_score,sensitivity,operation)
        VALUES($1,$2,$3,$4,'semantic','active',50,0.9,'internal','update')`,
		tenant, memory, ns, principal)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("forged event not denied by privilege/RLS (42501): %v", err)
	}
	_ = tx.Rollback(ctx)

	tx = begin(tenant, principal)
	if _, err := tx.Exec(ctx, `UPDATE memory_items SET status='active' WHERE id=$1`, memory); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	tx = begin(tenant, principal)
	assertCount(tx, `SELECT count(*) FROM memory_system_events WHERE memory_id=$1 AND operation='update' AND status='active'`, 1, memory)
	assertCount(tx, `SELECT count(*) FROM memory_system_events WHERE memory_id=$1`, 2, memory)
	if tag, err := tx.Exec(ctx, `UPDATE memory_system_events SET status='revoked' WHERE memory_id=$1`, memory); err == nil && tag.RowsAffected() != 0 {
		t.Fatal("untrusted role modified historical event")
	}
	_ = tx.Rollback(ctx)

	tx = begin(otherTenant, otherPrincipal)
	assertCount(tx, `SELECT count(*) FROM memory_system_events WHERE memory_id=$1`, 0, memory)
	_ = tx.Rollback(ctx)
	if _, err := admin.Exec(ctx, `UPDATE memory_items SET metadata='{"unrelated":true}'::jsonb WHERE id=$1`, memory); err != nil {
		t.Fatal(err)
	}
	tx = begin(tenant, principal)
	assertCount(tx, `SELECT count(*) FROM memory_system_events WHERE memory_id=$1`, 2, memory)
	_ = tx.Rollback(ctx)

	var count int
	if err := app.QueryRow(ctx, `SELECT count(*) FROM memory_system_events`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("context leaked across transactions: %d events", count)
	}
}

// The event FK cascade must not make tenant offboarding impossible, nor may
// ordinary callers silently delete immutable events without an audited purge.
func TestP5MemorySystemEventTenantPurgeRequiresAudit(t *testing.T) {
	ctx := context.Background()
	admin := testRetrievalPool(t)
	var tenant string
	if err := admin.QueryRow(ctx, `INSERT INTO tenants(name) VALUES($1) RETURNING id`, t.Name()).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `INSERT INTO memory_system_events (
        tenant_id,memory_id,namespace_id,owner_principal_id,memory_type,status,
        authority_level,confidence_score,sensitivity,operation)
        VALUES ($1,gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),
        'semantic','candidate',50,0.9,'internal','baseline')`, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `DELETE FROM tenants WHERE id=$1`, tenant); err == nil {
		t.Fatal("tenant cascade bypassed append-only event guard")
	}
	var purged int64
	if err := admin.QueryRow(ctx,
		`SELECT purge_memory_system_events_for_tenant($1,$2)`,
		tenant, "authorized offboarding with recorded reason").Scan(&purged); err != nil {
		t.Fatal(err)
	}
	if purged != 1 {
		t.Fatalf("purged %d, want 1", purged)
	}
	var count int
	if err := admin.QueryRow(ctx,
		`SELECT count(*) FROM memory_system_event_purge_audit
         WHERE tenant_id=$1 AND event_count=1`, tenant).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("missing durable purge audit: %d", count)
	}
	if _, err := admin.Exec(ctx, `DELETE FROM tenants WHERE id=$1`, tenant); err != nil {
		t.Fatalf("audited tenant offboarding failed: %v", err)
	}
}
