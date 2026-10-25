package main

import (
	"context"
	"os"
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

	// Block the current active generation only. The child must enter
	// activate_code_snapshot before it can supersede that generation.
	appName := "scos_p1_reactivate_kill_" + strings.ReplaceAll(historical[:8], "-", "")
	killWorkerBlockedInActivation(t, ctx, db, current, appName, "TestPGSnapshotReactivationKillChild",
		"SCOS_P1_REACTIVATION_KILL_CHILD=1",
		"SCOS_P1_REACTIVATE_SNAPSHOT="+historical,
	)

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
