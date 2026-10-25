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

// TestPGSnapshotProcessKillChild is invoked in a separate OS process by the
// parent test. It executes the real IndexSnapshot path, not hand-written SQL.
// The parent kills this process while PostgreSQL is waiting on a symbol row
// that the parent locked after the first file has already been inserted.
func TestPGSnapshotProcessKillChild(t *testing.T) {
	if os.Getenv("SCOS_P1_INDEX_KILL_CHILD") != "1" {
		t.Skip("only executed by the process-termination integration test")
	}
	store, _ := testPGStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	err := store.IndexSnapshot(ctx, os.Getenv("SCOS_P1_KILL_SNAPSHOT"), os.Getenv("SCOS_P1_KILL_REPOSITORY"), []PGFileInput{
		{Path: "partial.txt", Language: "text", ContentHash: hashContent("partial")},
		{Path: "blocked.txt", Language: "text", ContentHash: hashContent("shared")},
	})
	t.Fatalf("index worker unexpectedly returned instead of being killed: %v", err)
}

// A real SIGKILL-equivalent process termination during the *application's*
// indexing transaction must not leave partial file/symbol rows, promote an
// incomplete snapshot, or prevent startup recovery and a clean retry.
// PostgreSQL's lock wait makes the interruption deterministic, not a sleep
// hoping to catch a short-lived transaction.
func TestPGSnapshotProcessTerminationRollbackAndRecovery(t *testing.T) {
	store, db := testPGStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	tenant := testTenant(t, db)
	repo, err := store.EnsureRepository(ctx, tenant, "/p1-process-kill/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	original, err := store.CreateSnapshot(ctx, repo, tenant, "main", "original", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, original, repo, []PGFileInput{
		{Path: "blocked.txt", Language: "text", ContentHash: hashContent("shared")},
	}); err != nil {
		t.Fatal(err)
	}
	pending, err := store.CreateSnapshot(ctx, repo, tenant, "main", "after-kill", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}

	// The second file's symbol UPSERT must wait for this existing identity.
	// The first file has already been inserted within the same transaction.
	blocker, err := db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blocker.Rollback() })
	var blockerPID int
	if err := blocker.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&blockerPID); err != nil {
		t.Fatal(err)
	}
	var lockedSymbol string
	if err := blocker.QueryRowContext(ctx,
		"SELECT id::text FROM code_symbols WHERE repository_id=$1 AND stable_key=$2 FOR UPDATE",
		repo, hashContent("shared")).Scan(&lockedSymbol); err != nil {
		t.Fatal(err)
	}

	dsn, err := url.Parse(os.Getenv("SCOS_CODE_STORE_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	appName := "scos_p1_kill_" + strings.ReplaceAll(pending[:8], "-", "")
	query := dsn.Query()
	query.Set("application_name", appName)
	dsn.RawQuery = query.Encode()

	worker := exec.Command(os.Args[0], "-test.run=^TestPGSnapshotProcessKillChild$", "-test.v")
	worker.Env = append(os.Environ(),
		"SCOS_P1_INDEX_KILL_CHILD=1",
		"SCOS_P1_KILL_SNAPSHOT="+pending,
		"SCOS_P1_KILL_REPOSITORY="+repo,
		"SCOS_CODE_STORE_TEST_DSN="+dsn.String(),
	)
	var workerOutput bytes.Buffer
	worker.Stdout = &workerOutput
	worker.Stderr = &workerOutput
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

	// pg_blocking_pids proves that the actual worker is blocked by our
	// specific row lock inside claimSymbolLocked, after partial writes.
	observed := false
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		err := db.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity a
				 WHERE a.application_name=$1
				   AND a.wait_event_type='Lock'
				   AND a.query ILIKE '%INSERT INTO code_symbols%'
				   AND $2=ANY(pg_blocking_pids(a.pid))
			)`, appName, blockerPID).Scan(&observed)
		if err != nil {
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
		t.Fatalf("worker never reached the post-insert symbol lock; output: %s", workerOutput.String())
	}

	if err := worker.Process.Kill(); err != nil {
		t.Fatalf("kill indexing process: %v", err)
	}
	waitErr := worker.Wait()
	waited = true
	if waitErr == nil {
		t.Fatal("worker was not terminated")
	}
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) {
		t.Fatalf("unexpected worker termination result: %v", waitErr)
	}
	if err := blocker.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		t.Fatal(err)
	}

	var status, active string
	if err := db.QueryRowContext(ctx, "SELECT status::text FROM code_repository_snapshots WHERE id=$1", pending).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "queued" {
		t.Fatalf("killed transaction persisted snapshot status %q, want queued", status)
	}
	if err := db.QueryRowContext(ctx, "SELECT active_snapshot_id::text FROM code_repositories WHERE id=$1", repo).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != original {
		t.Fatalf("killed worker promoted %s, want original %s", active, original)
	}
	for _, table := range []string{"code_file_versions", "code_symbol_versions"} {
		var count int
		// Table names are hard-coded, never supplied by a caller.
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM "+table+" WHERE snapshot_id=$1", pending).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("killed worker left %d partial rows in %s", count, table)
		}
	}

	// Simulate restart after the stale-generation threshold. Recovery must
	// fail only the incomplete generation, then permit a fresh replacement.
	if _, err := db.ExecContext(ctx,
		"UPDATE code_repository_snapshots SET created_at=now()-interval '2 minutes' WHERE id=$1", pending); err != nil {
		t.Fatal(err)
	}
	affected, err := store.FailInterruptedSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if affected < 1 {
		t.Fatal("startup recovery did not fail the killed generation")
	}
	if err := db.QueryRowContext(ctx, "SELECT status::text FROM code_repository_snapshots WHERE id=$1", pending).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "failed" {
		t.Fatalf("recovery status=%s, want failed", status)
	}
	if err := store.DeleteFailedSnapshot(ctx, pending); err != nil {
		t.Fatal(err)
	}
	retry, err := store.CreateSnapshot(ctx, repo, tenant, "main", "after-kill", "heuristic-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if retry == pending {
		t.Fatal("retry reused terminated generation")
	}
	if err := store.IndexSnapshot(ctx, retry, repo, []PGFileInput{
		{Path: "partial.txt", Language: "text", ContentHash: hashContent("partial")},
		{Path: "blocked.txt", Language: "text", ContentHash: hashContent("shared")},
	}); err != nil {
		t.Fatalf("reindex after process termination: %v", err)
	}
	if err := db.QueryRowContext(ctx, "SELECT active_snapshot_id::text FROM code_repositories WHERE id=$1", repo).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != retry {
		t.Fatalf("recovery did not activate new generation: %s, want %s", active, retry)
	}
	var fileCount int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM code_file_versions WHERE snapshot_id=$1", retry).Scan(&fileCount); err != nil {
		t.Fatal(err)
	}
	if fileCount != 2 {
		t.Fatalf("recovered generation has %d files, want 2", fileCount)
	}
}
