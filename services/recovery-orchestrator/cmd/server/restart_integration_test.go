//go:build integration

package main

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestRecoveryStatusSurvivesARestart persists a recovery run through the
// runtime role, then starts a fresh orchestrator on the same database: it
// must serve the same run, phase, checklist and interrupted work.
func TestRecoveryStatusSurvivesARestart(t *testing.T) {
	adminURL, appURL := os.Getenv("DATABASE_URL"), os.Getenv("SCOS_APP_TEST_DATABASE_URL")
	if adminURL == "" || appURL == "" {
		t.Fatal("DATABASE_URL (owner) and SCOS_APP_TEST_DATABASE_URL (runtime role) must be set")
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
	const service = "restart-it-service"
	cleanup := func() { _, _ = admin.Exec(ctx, `DELETE FROM recovery_runs WHERE service_id=$1`, service) }
	cleanup()
	t.Cleanup(cleanup)

	before := &recoveryServer{statuses: map[string]*RecoveryStatus{}, db: app}
	status := &RecoveryStatus{
		ServiceID: service, Phase: "recovery",
		Checklist:        RecoveryChecklist{{Step: 1, Name: "storage", Status: "ok"}, {Step: 2, Name: "turns", Status: "pending"}},
		InterruptedTurns: []string{"turn-a", "turn-b"}, OpenOperations: []string{"op-1"}, PendingOutbox: 3,
	}
	if err := before.persistRecoveryStatus(ctx, status, "recovery_started"); err != nil {
		t.Fatal(err)
	}

	after := &recoveryServer{statuses: map[string]*RecoveryStatus{}, db: app}
	if err := after.loadPersistedStatuses(ctx); err != nil {
		t.Fatal(err)
	}
	got := after.statuses[service]
	if got == nil {
		t.Fatalf("restarted orchestrator lost the run; has %v", after.statuses)
	}
	if got.RecoveryRunID != status.RecoveryRunID || got.Phase != "recovery" || got.PendingOutbox != 3 ||
		len(got.Checklist) != 2 || got.Checklist[1].Status != "pending" ||
		len(got.InterruptedTurns) != 2 || len(got.OpenOperations) != 1 || got.RecoveryStarted == "" {
		t.Fatalf("restored status = %+v, want %+v", got, status)
	}
}
