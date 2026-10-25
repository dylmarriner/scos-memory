package main

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// Two independent generations of the same branch must never both become
// active. Hold the repository row while both index transactions reach the
// activation boundary; this reproduces a real READ COMMITTED write race
// without relying on scheduler timing.
func TestPGConcurrentGenerationsHaveSingleActiveSnapshot(t *testing.T) {
	store, db := testPGStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()

	tenant := testTenant(t, db)
	repo, err := store.EnsureRepository(ctx, tenant, "/p1-cross-generation/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}

	ids := make([]string, 2)
	for i, commit := range []string{"first", "second"} {
		ids[i], err = store.CreateSnapshot(ctx, repo, tenant, "main", commit, "heuristic-v1", "")
		if err != nil {
			t.Fatal(err)
		}
	}

	// Hold the final repository pointer update. On the unfixed schema both
	// transactions mark their distinct snapshot active before this lock.
	blocker, err := db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback() }()
	var locked string
	if err := blocker.QueryRowContext(ctx,
		"SELECT id::text FROM code_repositories WHERE id=$1 FOR UPDATE", repo).Scan(&locked); err != nil {
		t.Fatal(err)
	}

	results := make(chan error, 2)
	for _, id := range ids {
		go func(snapshot string) {
			results <- store.IndexSnapshot(ctx, snapshot, repo, nil)
		}(id)
	}

	// Wait until *both* indexing transactions are actually blocked inside
	// activate_code_snapshot, so a lucky sequential execution cannot pass.
	deadline := time.Now().Add(10 * time.Second)
	blocked := 0
	for time.Now().Before(deadline) {
		if err := db.QueryRowContext(ctx, `
            SELECT count(*) FROM pg_stat_activity
             WHERE datname = current_database()
               AND pid <> pg_backend_pid()
               AND wait_event_type = 'Lock'
               AND (query ILIKE '%activate_code_snapshot%'
                    OR query ILIKE '%code_repositories%')`).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked == 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if blocked != 2 {
		t.Fatalf("could not establish simultaneous activation contention: %d blocked workers", blocked)
	}
	if err := blocker.Commit(); err != nil {
		t.Fatal(err)
	}

	for range ids {
		if err := <-results; err != nil {
			t.Fatalf("concurrent generation indexing failed: %v", err)
		}
	}

	var activeCount int
	var activeID string
	if err := db.QueryRowContext(ctx,
		"SELECT count(*) FROM code_repository_snapshots WHERE repository_id=$1 AND branch='main' AND status='active'", repo).
		Scan(&activeCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx,
		"SELECT active_snapshot_id::text FROM code_repositories WHERE id=$1", repo).
		Scan(&activeID); err != nil {
		t.Fatal(err)
	}
	if activeCount != 1 {
		t.Fatalf("concurrent generations left %d active snapshots (repository pointer %s); want exactly one", activeCount, activeID)
	}
	var pointedActive bool
	if err := db.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM code_repository_snapshots WHERE id=$1 AND status='active')", activeID).
		Scan(&pointedActive); err != nil {
		t.Fatal(err)
	}
	if !pointedActive {
		t.Fatalf("repository pointer %s does not identify an active snapshot", activeID)
	}

	// The loser remains a recoverable historical generation, not a second
	// active snapshot or a failed generation. Reactivating it must restore
	// the single-active invariant and update the repository pointer.
	var historical string
	if err := db.QueryRowContext(ctx,
		"SELECT id::text FROM code_repository_snapshots WHERE repository_id=$1 AND branch='main' AND status='superseded'", repo).
		Scan(&historical); err != nil {
		t.Fatalf("historical generation missing: %v", err)
	}
	if err := store.ReactivateSnapshot(ctx, historical); err != nil {
		t.Fatalf("reactivate concurrent loser: %v", err)
	}
	var restoredActiveCount int
	var restoredPointer string
	if err := db.QueryRowContext(ctx,
		"SELECT count(*) FROM code_repository_snapshots WHERE repository_id=$1 AND branch='main' AND status='active'", repo).
		Scan(&restoredActiveCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx,
		"SELECT active_snapshot_id::text FROM code_repositories WHERE id=$1", repo).
		Scan(&restoredPointer); err != nil {
		t.Fatal(err)
	}
	if restoredActiveCount != 1 || restoredPointer != historical {
		t.Fatalf("historical reactivation violated invariant: active=%d pointer=%s want=%s",
			restoredActiveCount, restoredPointer, historical)
	}
}
