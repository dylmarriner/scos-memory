package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The parent invokes this test in an independent process, then kills it
// after PostgreSQL confirms it is blocked within activate_code_snapshot.
func TestPGSnapshotReactivationKillChild(t *testing.T) {
	if os.Getenv("SCOS_P1_REACTIVATION_KILL_CHILD") != "1" {
		t.Skip("only executed by the reactivation process-termination test")
	}
	store, _ := testPGStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	err := store.ReactivateSnapshot(ctx, os.Getenv("SCOS_P1_REACTIVATE_SNAPSHOT"))
	t.Fatalf("reactivation worker returned instead of being killed: %v", err)
}

// Historical reactivation must be atomic when the real Go process dies
// during the SQL activation function. Neither snapshot status nor the
// repository's active pointer may be left halfway through the transition.
func TestPGSnapshotReactivationProcessTerminationPreservesHistory(t *testing.T) {
	store, db := testPGStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	tenant := testTenant(t, db)
	repo, err := store.EnsureRepository(ctx, tenant, "/p1-reactivation-kill/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	historical, err := store.CreateSnapshot(ctx, repo, tenant, "main", "historical", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, historical, repo, []PGFileInput{
		{Path: "old.txt", Language: "text", ContentHash: hashContent("old")},
	}); err != nil {
		t.Fatal(err)
	}
	current, err := store.CreateSnapshot(ctx, repo, tenant, "main", "current", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, current, repo, []PGFileInput{
		{Path: "new.txt", Language: "text", ContentHash: hashContent("new")},
	}); err != nil {
		t.Fatal(err)
	}

	// Reactivation first locks the repository and historical snapshot, then
	// attempts to supersede the current active snapshot. Block only that
	// final row, so the process must enter the activation SQL to wait.
	blocker, err := db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blocker.Rollback() })
	var blockerPID int
	if err := blocker.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&blockerPID); err != nil {
		t.Fatal(err)
	}
	var locked string
	if err := blocker.QueryRowContext(ctx,
		"SELECT id::text FROM code_repository_snapshots WHERE id=$1 FOR UPDATE", current).Scan(&locked); err != nil {
		t.Fatal(err)
	}

	dsn, err := url.Parse(os.Getenv("SCOS_CODE_STORE_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	appName := "scos_p1_reactivate_kill_" + strings.ReplaceAll(historical[:8], "-", "")
	query := dsn.Query()
	query.Set("application_name", appName)
	dsn.RawQuery = query.Encode()

	worker := exec.Command(os.Args[0], "-test.run=^TestPGSnapshotReactivationKillChild$", "-test.v")
	worker.Env = append(os.Environ(),
		"SCOS_P1_REACTIVATION_KILL_CHILD=1",
		"SCOS_P1_REACTIVATE_SNAPSHOT="+historical,
		"SCOS_CODE_STORE_TEST_DSN="+dsn.String(),
	)
	var output bytes.Buffer
	worker.Stdout = &output
	worker.Stderr = &output
	if err := worker.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	t.Cleanup(func() {
		if !waited {
			_ = worker.Process.Kill()
			_ = worker.Wait()
		}
	})

	observed := false
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if err := db.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity a
				 WHERE a.application_name=$1
				   AND a.wait_event_type='Lock'
				   AND a.query ILIKE '%activate_code_snapshot%'
				   AND $2=ANY(pg_blocking_pids(a.pid))
			)`, appName, blockerPID).Scan(&observed); err != nil {
			t.Fatal(err)
		}
		if observed {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !observed {
		_ = worker.Process.Kill()
		_ = worker.Wait()
		waited = true
		t.Fatalf("reactivation worker did not reach activation SQL: %s", output.String())
	}
	if err := worker.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	waitErr := worker.Wait()
	waited = true
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) {
		t.Fatalf("worker was not forcibly terminated: %v", waitErr)
	}
	if err := blocker.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		t.Fatal(err)
	}

	var activeID string
	var activeCount int
	var historicalStatus, currentStatus string
	if err := db.QueryRowContext(ctx,
		"SELECT active_snapshot_id::text FROM code_repositories WHERE id=$1", repo).Scan(&activeID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM code_repository_snapshots
		 WHERE repository_id=$1 AND branch='main' AND status='active'`, repo).Scan(&activeCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx,
		"SELECT status::text FROM code_repository_snapshots WHERE id=$1", historical).Scan(&historicalStatus); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx,
		"SELECT status::text FROM code_repository_snapshots WHERE id=$1", current).Scan(&currentStatus); err != nil {
		t.Fatal(err)
	}
	if activeID != current || activeCount != 1 || historicalStatus != "superseded" || currentStatus != "active" {
		t.Fatalf("killed reactivation changed history: pointer=%s active_count=%d historical=%s current=%s",
			activeID, activeCount, historicalStatus, currentStatus)
	}

	// Once the crashed connection is gone, reactivation must still work
	// without reindexing or corrupting immutable historical versions.
	if err := store.ReactivateSnapshot(ctx, historical); err != nil {
		t.Fatalf("retry historical reactivation: %v", err)
	}
	if err := db.QueryRowContext(ctx,
		"SELECT active_snapshot_id::text FROM code_repositories WHERE id=$1", repo).Scan(&activeID); err != nil {
		t.Fatal(err)
	}
	if activeID != historical {
		t.Fatalf("retry activated %s, want historical %s", activeID, historical)
	}
	var historicalFiles int
	if err := db.QueryRowContext(ctx,
		"SELECT count(*) FROM code_file_versions WHERE snapshot_id=$1", historical).Scan(&historicalFiles); err != nil {
		t.Fatal(err)
	}
	if historicalFiles != 1 {
		t.Fatalf("historical reactivation changed immutable files: %d", historicalFiles)
	}
}
