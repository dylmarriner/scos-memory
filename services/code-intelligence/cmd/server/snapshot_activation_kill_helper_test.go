package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"testing"
	"time"
)

// killWorkerBlockedInActivation deterministically terminates a real Go test
// subprocess while its PostgreSQL backend is inside activate_code_snapshot.
// The blocker locks only the previously active generation, allowing all
// earlier indexing or reactivation work to reach the activation boundary.
// It waits for the orphaned backend to disappear before returning, so a
// subsequent recovery or retry cannot race the killed transaction's cleanup.
func killWorkerBlockedInActivation(
	t *testing.T, ctx context.Context, db *sql.DB,
	lockedSnapshotID, appName, childTest string, childEnv ...string,
) {
	t.Helper()
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
		"SELECT id::text FROM code_repository_snapshots WHERE id=$1 FOR UPDATE", lockedSnapshotID).
		Scan(&locked); err != nil {
		t.Fatal(err)
	}

	dsn, err := url.Parse(os.Getenv("SCOS_CODE_STORE_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	query := dsn.Query()
	query.Set("application_name", appName)
	dsn.RawQuery = query.Encode()

	worker := exec.Command(os.Args[0], "-test.run=^"+childTest+"$", "-test.v")
	worker.Env = append(append(os.Environ(), childEnv...), "SCOS_CODE_STORE_TEST_DSN="+dsn.String())
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

	// Verify this worker, not an unrelated session, is waiting on the
	// snapshot row held by this test INSIDE the activation SQL function.
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
		t.Fatalf("worker did not reach blocked activation SQL: %s", output.String())
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

	// Killing the Go process does not instantly guarantee that PostgreSQL
	// has detected the disconnected client. Wait until all backends using
	// the child-specific application_name are gone before asserting rollback
	// or starting recovery/retry transactions.
	gone := false
	for end := time.Now().Add(15 * time.Second); time.Now().Before(end); {
		if err := db.QueryRowContext(ctx,
			"SELECT NOT EXISTS (SELECT 1 FROM pg_stat_activity WHERE application_name=$1)", appName).
			Scan(&gone); err != nil {
			t.Fatal(err)
		}
		if gone {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !gone {
		t.Fatalf("killed worker PostgreSQL backend did not terminate: %s", output.String())
	}
}
