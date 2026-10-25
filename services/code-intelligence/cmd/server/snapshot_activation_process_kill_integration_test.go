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

// Killing the actual indexing process while activate_code_snapshot is blocked
// must roll back its ready status, immutable versions, and activation changes.
// This is deliberately different from killing a worker during file ingestion:
// the worker has completed its file writes and entered the activation function.
func TestPGSnapshotActivationProcessTerminationRollbackAndRecovery(t *testing.T) {
	store, db := testPGStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()

	tenant := testTenant(t, db)
	repo, err := store.EnsureRepository(ctx, tenant, "/p1-activation-kill/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	original, err := store.CreateSnapshot(ctx, repo, tenant, "main", "before-activation-kill", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, original, repo, []PGFileInput{
		{Path: "original.txt", Language: "text", ContentHash: hashContent("original")},
	}); err != nil {
		t.Fatal(err)
	}
	pending, err := store.CreateSnapshot(ctx, repo, tenant, "main", "after-activation-kill", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}

	// Only the old active snapshot is locked. The worker can acquire the
	// repository and pending-snapshot locks, insert all its files, and mark
	// pending ready. It then blocks INSIDE activate_code_snapshot while
	// attempting to supersede the old active generation.
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
		"SELECT id::text FROM code_repository_snapshots WHERE id=$1 FOR UPDATE", original).Scan(&locked); err != nil {
		t.Fatal(err)
	}

	dsn, err := url.Parse(os.Getenv("SCOS_CODE_STORE_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	appName := "scos_p1_activate_kill_" + strings.ReplaceAll(pending[:8], "-", "")
	query := dsn.Query()
	query.Set("application_name", appName)
	dsn.RawQuery = query.Encode()

	// Reuse the real IndexSnapshot subprocess from the ingestion-kill test.
	worker := exec.Command(os.Args[0], "-test.run=^TestPGSnapshotProcessKillChild$", "-test.v")
	worker.Env = append(os.Environ(),
		"SCOS_P1_INDEX_KILL_CHILD=1",
		"SCOS_P1_KILL_SNAPSHOT="+pending,
		"SCOS_P1_KILL_REPOSITORY="+repo,
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

	// Confirm the exact PostgreSQL backend is waiting on OUR snapshot row
	// from within the activation function, rather than assuming a timing
	// window. This proves the worker reached activation after file writes.
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
		t.Fatalf("worker did not reach the blocked activation function: %s", output.String())
	}

	if err := worker.Process.Kill(); err != nil {
		t.Fatalf("kill worker inside activation: %v", err)
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

	var status string
	var activated bool
	var fileCount int
	if err := db.QueryRowContext(ctx, `
		SELECT status::text, activated_at IS NOT NULL, file_count
		  FROM code_repository_snapshots WHERE id=$1`, pending).
		Scan(&status, &activated, &fileCount); err != nil {
		t.Fatal(err)
	}
	if status != "queued" || activated || fileCount != 0 {
		t.Fatalf("killed activation persisted pending state: status=%s activated=%t file_count=%d",
			status, activated, fileCount)
	}
	var activeID string
	var activeCount int
	if err := db.QueryRowContext(ctx,
		"SELECT active_snapshot_id::text FROM code_repositories WHERE id=$1", repo).Scan(&activeID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM code_repository_snapshots
		 WHERE repository_id=$1 AND branch='main' AND status='active'`, repo).Scan(&activeCount); err != nil {
		t.Fatal(err)
	}
	if activeID != original || activeCount != 1 {
		t.Fatalf("killed activation changed active generation: pointer=%s count=%d want=%s",
			activeID, activeCount, original)
	}
	for _, table := range []string{"code_file_versions", "code_symbol_versions"} {
		var count int
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM "+table+" WHERE snapshot_id=$1", pending).
			Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("killed activation left %d uncommitted rows in %s", count, table)
		}
	}

	// Startup recovery must reject the abandoned generation without
	// changing the still-active historical snapshot, then allow a retry.
	if _, err := db.ExecContext(ctx,
		"UPDATE code_repository_snapshots SET created_at=now()-interval '2 minutes' WHERE id=$1", pending); err != nil {
		t.Fatal(err)
	}
	recovered, err := store.FailInterruptedSnapshots(ctx)
	if err != nil || recovered < 1 {
		t.Fatalf("startup recovery: affected=%d err=%v", recovered, err)
	}
	if err := db.QueryRowContext(ctx,
		"SELECT status::text FROM code_repository_snapshots WHERE id=$1", pending).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "failed" {
		t.Fatalf("abandoned activation status=%s, want failed", status)
	}
	if err := store.DeleteFailedSnapshot(ctx, pending); err != nil {
		t.Fatal(err)
	}
	retry, err := store.CreateSnapshot(ctx, repo, tenant, "main", "after-activation-kill", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if retry == pending {
		t.Fatal("retry reused the killed generation")
	}
	if err := store.IndexSnapshot(ctx, retry, repo, []PGFileInput{
		{Path: "partial.txt", Language: "text", ContentHash: hashContent("partial")},
		{Path: "blocked.txt", Language: "text", ContentHash: hashContent("shared")},
	}); err != nil {
		t.Fatalf("index replacement generation: %v", err)
	}
	if err := db.QueryRowContext(ctx,
		"SELECT active_snapshot_id::text FROM code_repositories WHERE id=$1", repo).Scan(&activeID); err != nil {
		t.Fatal(err)
	}
	if activeID != retry {
		t.Fatalf("recovery activated %s, want %s", activeID, retry)
	}
	if err := db.QueryRowContext(ctx,
		"SELECT status::text FROM code_repository_snapshots WHERE id=$1", original).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "superseded" {
		t.Fatalf("recovery lost original immutable generation: status=%s", status)
	}
}
