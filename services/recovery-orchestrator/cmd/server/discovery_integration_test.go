//go:build integration

package main

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestDiscoveryThroughRestrictedRole runs discovery as the runtime role
// (SCOS_RECOVERY_APP_DATABASE_URL, e.g. scos_app) against state seeded by
// the owning role (DATABASE_URL): the restricted role must still see the
// interrupted fabric state and requeue a crashed worker's job.
func TestDiscoveryThroughRestrictedRole(t *testing.T) {
	adminURL, appURL := os.Getenv("DATABASE_URL"), os.Getenv("SCOS_RECOVERY_APP_DATABASE_URL")
	if adminURL == "" || appURL == "" {
		t.Fatal("DATABASE_URL (owner) and SCOS_RECOVERY_APP_DATABASE_URL (runtime role) must be set")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	app, err := pgxpool.New(ctx, appURL)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()

	tenant := "7a7a7a7a-0000-4000-8000-000000000001"
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := admin.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	exec(`INSERT INTO tenants (id, name) VALUES ($1, 'recovery-it') ON CONFLICT DO NOTHING`, tenant)
	exec(`DELETE FROM outbox_jobs WHERE tenant_id = $1`, tenant)
	exec(`INSERT INTO outbox_jobs (tenant_id, operation_id, job_type, payload, status, lease_owner, lease_expires_at)
		VALUES ($1, 'it-stale', 'consolidate', '{}', 'processing', 'crashed-worker', now() - interval '1 minute'),
		       ($1, 'it-pending', 'consolidate', '{}', 'pending', NULL, NULL)`, tenant)
	t.Cleanup(func() { _, _ = admin.Exec(ctx, `DELETE FROM outbox_jobs WHERE tenant_id = $1`, tenant) })

	var direct int
	if err := app.QueryRow(ctx, `SELECT count(*) FROM outbox_jobs`).Scan(&direct); err != nil {
		t.Fatal(err)
	}
	if direct != 0 {
		t.Fatalf("precondition: the runtime role should not see tenant rows directly, saw %d", direct)
	}

	findings, err := discoverRecoveryFindings(ctx, app, "recovery-it")
	if err != nil {
		t.Fatal(err)
	}
	if findings.staleRequeued < 1 || findings.pendingOutbox < 2 {
		t.Fatalf("discovery did not see or requeue fabric state: %+v", findings)
	}
	if !findings.storageOK {
		t.Fatalf("storage check failed: %s", findings.storageDetail)
	}
	var status, owner *string
	if err := admin.QueryRow(ctx, `SELECT status::text, lease_owner FROM outbox_jobs WHERE tenant_id=$1 AND operation_id='it-stale'`, tenant).Scan(&status, &owner); err != nil {
		t.Fatal(err)
	}
	if *status != "retryable" || owner != nil {
		t.Fatalf("stale job not requeued: status=%v owner=%v", *status, owner)
	}
}
