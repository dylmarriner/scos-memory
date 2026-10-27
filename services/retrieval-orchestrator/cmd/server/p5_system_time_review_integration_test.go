package main

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Purging must be an explicit privileged operation. A failed offboarding
// transaction must not discard either history or its audit trail.
func TestP5MemorySystemEventPurgePrivilegeAndRollback(t *testing.T) {
	ctx := context.Background()
	admin := testRetrievalPool(t)
	appDSN := os.Getenv("SCOS_APP_TEST_DATABASE_URL")
	if appDSN == "" {
		t.Fatal("SCOS_APP_TEST_DATABASE_URL required for P5 purge acceptance")
	}
	app, err := pgxpool.New(ctx, appDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	if err := app.Ping(ctx); err != nil {
		t.Fatal(err)
	}

	var tenant string
	if err := admin.QueryRow(ctx, `INSERT INTO tenants(name) VALUES($1) RETURNING id`, t.Name()).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `INSERT INTO memory_system_events
        (tenant_id,memory_id,namespace_id,owner_principal_id,memory_type,status,
         authority_level,confidence_score,sensitivity,operation)
        VALUES ($1,gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),
                'semantic','candidate',50,0.9,'internal','baseline')`, tenant); err != nil {
		t.Fatal(err)
	}

	if _, err := app.Exec(ctx, `SELECT purge_memory_system_events_for_tenant($1,$2)`,
		tenant, "untrusted runtime request for purge"); err == nil {
		t.Fatal("runtime principal executed privileged event purge")
	}
	if _, err := app.Exec(ctx, `DELETE FROM memory_system_events WHERE tenant_id=$1`, tenant); err == nil {
		t.Fatal("runtime principal directly deleted authoritative event history")
	}

	tx, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := tx.QueryRow(ctx, `SELECT purge_memory_system_events_for_tenant($1,$2)`,
		tenant, "aborted offboarding must restore events").Scan(&count); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if count != 1 {
		_ = tx.Rollback(ctx)
		t.Fatalf("purged %d events, expected 1", count)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	var eventCount, auditCount int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM memory_system_events WHERE tenant_id=$1`, tenant).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM memory_system_event_purge_audit WHERE tenant_id=$1`, tenant).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 1 || auditCount != 0 {
		t.Fatalf("rollback did not restore event/audit state: events=%d audits=%d", eventCount, auditCount)
	}

	tx, err = admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `SELECT purge_memory_system_events_for_tenant($1,$2)`,
		tenant, "approved offboarding with full audit").Scan(&count); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM tenants WHERE id=$1`, tenant); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM memory_system_event_purge_audit
        WHERE tenant_id=$1 AND event_count=1 AND operator_name <> '' AND reason <> ''
        AND transaction_id > 0`, tenant).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if auditCount != 1 {
		t.Fatalf("durable audit after tenant deletion: %d", auditCount)
	}
	if _, err := admin.Exec(ctx, `DELETE FROM memory_system_event_purge_audit WHERE tenant_id=$1`, tenant); err == nil {
		t.Fatal("privileged operator deleted immutable purge audit")
	}
}

// PostgreSQL itself must prove that the migration's lock mode conflicts with
// concurrent memory writes, not just a source-text ordering assertion.
func TestP5MemorySystemEventBaselineLockBlocksConcurrentInsert(t *testing.T) {
	ctx := context.Background()
	admin := testRetrievalPool(t)
	var tenant, principal, ns string
	if err := admin.QueryRow(ctx, `INSERT INTO tenants(name) VALUES($1) RETURNING id`, t.Name()).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(ctx, `INSERT INTO principals(tenant_id,kind,external_id,display_name)
        VALUES($1,'human',$2,$2) RETURNING id`, tenant, t.Name()).Scan(&principal); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(ctx, `INSERT INTO namespaces(tenant_id,owner_principal_id,canonical_name,sensitivity_ceiling)
        VALUES($1,$2,$3,'internal') RETURNING id`, tenant, principal, t.Name()).Scan(&ns); err != nil {
		t.Fatal(err)
	}

	lockTx, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lockTx.Rollback(context.Background()) }()
	if _, err := lockTx.Exec(ctx, `LOCK TABLE memory_items IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}

	insertSQL := `INSERT INTO memory_items
        (tenant_id,namespace_id,owner_principal_id,type,status,title,
         canonical_summary,authority_level,confidence_score,sensitivity)
        VALUES($1,$2,$3,'semantic','candidate','concurrent','concurrent',50,0.9,'internal')`
	blockedCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	start := time.Now()
	_, err = admin.Exec(blockedCtx, insertSQL, tenant, ns, principal)
	elapsed := time.Since(start)
	cancel()
	if err == nil || elapsed < 150*time.Millisecond {
		t.Fatalf("write did not block behind migration lock: error=%v elapsed=%v", err, elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(blockedCtx.Err(), context.DeadlineExceeded) {
		t.Fatalf("write failed for a reason other than lock timeout: %v", err)
	}
	if err := lockTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, insertSQL, tenant, ns, principal); err != nil {
		t.Fatalf("write did not resume after migration lock release: %v", err)
	}
	var events int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM memory_system_events WHERE tenant_id=$1 AND operation='insert'`, tenant).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Fatalf("untracked post-lock memory insertion: %d events", events)
	}
}

// Earlier ordered migrations provide owner-only FORCE-RLS access needed by
// 000082. Losing those policies would silently omit baseline rows when the
// migration owner is not a superuser, or block its tenant FOR UPDATE lock.
func TestP5MemorySystemEventOwnerRLSPrerequisites(t *testing.T) {
	ctx := context.Background()
	admin := testRetrievalPool(t)
	rows, err := admin.Query(ctx, `SELECT tablename, policyname, cmd
        FROM pg_policies
        WHERE schemaname = current_schema()
          AND current_user = ANY(roles)
          AND policyname IN (
            'revalidation_owner_read_facts',
            'revalidation_purge_owner_tenant_read',
            'revalidation_purge_owner_tenant_lock')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	expected := map[string]bool{
		"memory_items/revalidation_owner_read_facts/SELECT":   false,
		"tenants/revalidation_purge_owner_tenant_read/SELECT": false,
		"tenants/revalidation_purge_owner_tenant_lock/UPDATE": false,
	}
	for rows.Next() {
		var table, policy, command string
		if err := rows.Scan(&table, &policy, &command); err != nil {
			t.Fatal(err)
		}
		key := table + "/" + policy + "/" + command
		if _, ok := expected[key]; !ok {
			t.Fatalf("unexpected owner RLS policy: %s", key)
		}
		expected[key] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for key, present := range expected {
		if !present {
			t.Fatalf("missing required owner FORCE-RLS prerequisite: %s", key)
		}
	}
	for _, table := range []string{"memory_items", "tenants"} {
		var forced bool
		if err := admin.QueryRow(ctx, `SELECT c.relforcerowsecurity FROM pg_class c
            JOIN pg_namespace n ON n.oid=c.relnamespace
            WHERE n.nspname=current_schema() AND c.relname=$1`, table).Scan(&forced); err != nil {
			t.Fatal(err)
		}
		if !forced {
			t.Fatalf("%s must FORCE RLS", table)
		}
	}
}
