package main

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	scip "github.com/scip-code/scip/bindings/go/scip"
)

// The activation paths must acquire repository before snapshot locks.
// A second transaction holding the repository lock must be able to lock
// the target snapshot without deadlocking with a waiting activation.
func TestPGSnapshotActivationLockOrder(t *testing.T) {
	for _, tc := range []struct {
		name       string
		run        func(context.Context, *pgCodeStore, string, string) error
		reactivate bool
	}{
		{
			name: "go_index",
			run: func(ctx context.Context, s *pgCodeStore, snapshot, repo string) error {
				return s.IndexSnapshot(ctx, snapshot, repo, nil)
			},
		},
		{
			name: "scip_index",
			run: func(ctx context.Context, s *pgCodeStore, snapshot, repo string) error {
				_, err := s.IndexSnapshotFromSCIP(ctx, snapshot, repo, &scip.Index{})
				return err
			},
		},
		{
			name:       "historical_reactivation",
			reactivate: true,
			run: func(ctx context.Context, s *pgCodeStore, snapshot, repo string) error {
				return s.ReactivateSnapshot(ctx, snapshot)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, db := testPGStore(t)
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			tenant := testTenant(t, db)
			repo, err := store.EnsureRepository(ctx, tenant, "/p1-lock-order/"+t.Name())
			if err != nil {
				t.Fatal(err)
			}
			first, err := store.CreateSnapshot(ctx, repo, tenant, "main", "first", "heuristic-v1", "")
			if err != nil {
				t.Fatal(err)
			}
			if err := store.IndexSnapshot(ctx, first, repo, nil); err != nil {
				t.Fatal(err)
			}
			second, err := store.CreateSnapshot(ctx, repo, tenant, "main", "second", "heuristic-v1", "")
			if err != nil {
				t.Fatal(err)
			}
			target := second
			if tc.reactivate {
				if err := store.IndexSnapshot(ctx, second, repo, nil); err != nil {
					t.Fatal(err)
				}
				target = first
			}

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

			result := make(chan error, 1)
			go func() { result <- tc.run(ctx, store, target, repo) }()

			// Verify the worker has reached a real database lock wait.
			// Without this synchronization, a lucky schedule masks inversion.
			blocked := 0
			deadline := time.Now().Add(8 * time.Second)
			for time.Now().Before(deadline) {
				if err := db.QueryRowContext(ctx,
					"SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND wait_event_type='Lock' AND state='active'").Scan(&blocked); err != nil {
					t.Fatal(err)
				}
				if blocked >= 1 {
					break
				}
				select {
				case err := <-result:
					t.Fatalf("activation finished before repository contention: %v", err)
				default:
				}
				time.Sleep(20 * time.Millisecond)
			}
			if blocked < 1 {
				t.Fatal("activation never waited on repository lock")
			}

			// Under the old snapshot->repository order this statement waits
			// on the worker's snapshot lock, causing PostgreSQL 40P01.
			// Under repository->snapshot order it succeeds immediately.
			probeCtx, probeCancel := context.WithTimeout(ctx, 4*time.Second)
			defer probeCancel()
			var snapshotLocked string
			probeErr := blocker.QueryRowContext(probeCtx,
				"SELECT id::text FROM code_repository_snapshots WHERE id=$1 FOR UPDATE", target).Scan(&snapshotLocked)
			_ = blocker.Rollback()
			workerErr := <-result
			if probeErr != nil {
				t.Fatalf("repository->snapshot lock order deadlocked (%s): %v (worker: %v)", tc.name, probeErr, workerErr)
			}
			if workerErr != nil {
				t.Fatalf("activation failed after repository lock release: %v", workerErr)
			}
			var status string
			if err := db.QueryRowContext(ctx,
				"SELECT status::text FROM code_repository_snapshots WHERE id=$1", target).Scan(&status); err != nil {
				t.Fatal(err)
			}
			if status != "active" {
				t.Fatal(fmt.Sprintf("target status=%s, want active", status))
			}
		})
	}
}
