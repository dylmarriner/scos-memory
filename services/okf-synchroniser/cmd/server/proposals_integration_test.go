package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestPGPersistProposalTimestamps runs persistProposal against a real
// PostgreSQL with every migration applied (skips unless
// SCOS_OKF_STORE_TEST_DSN is set). reviewed_at/indexed_at are timestamptz
// and arrive as strings that are empty until the proposal is reviewed and
// indexed; both the empty (NULL) and set cases must persist.
func TestPGPersistProposalTimestamps(t *testing.T) {
	dsn := os.Getenv("SCOS_OKF_STORE_TEST_DSN")
	if dsn == "" {
		t.Skip("SCOS_OKF_STORE_TEST_DSN not set; skipping okf proposal persistence integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	s := &okfServer{db: pool}

	p := &OKFProposal{ID: newProposalID(), Path: "procedures/test.md", BaseCommit: "c0", BaseHash: "h0", Content: "x", Diff: "d", Proposer: "alice", CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	if err := s.persistProposal(ctx, p, "proposed", "alice", "", "proposed"); err != nil {
		t.Fatalf("persist proposed proposal with empty reviewed_at/indexed_at: %v", err)
	}
	var reviewedNull, indexedNull bool
	if err := pool.QueryRow(ctx, `SELECT reviewed_at IS NULL, indexed_at IS NULL FROM okf_proposals WHERE id=$1`, p.ID).Scan(&reviewedNull, &indexedNull); err != nil {
		t.Fatal(err)
	}
	if !reviewedNull || !indexedNull {
		t.Fatalf("empty timestamps must persist as NULL, got reviewed_null=%v indexed_null=%v", reviewedNull, indexedNull)
	}

	reviewed := time.Now().UTC().Truncate(time.Second)
	indexed := reviewed.Add(time.Minute)
	p.Reviewer, p.ReviewRole, p.Decision = "bob", "maintainer", "approve"
	p.ReviewedAt, p.IndexedAt, p.MergedCommit = reviewed.Format(time.RFC3339), indexed.Format(time.RFC3339), "c1"
	if err := s.persistProposal(ctx, p, "indexed", "bob", "maintainer", "approved"); err != nil {
		t.Fatalf("persist indexed proposal with timestamps: %v", err)
	}
	var gotReviewed, gotIndexed time.Time
	var transitions int
	if err := pool.QueryRow(ctx, `SELECT reviewed_at, indexed_at FROM okf_proposals WHERE id=$1`, p.ID).Scan(&gotReviewed, &gotIndexed); err != nil {
		t.Fatal(err)
	}
	if !gotReviewed.Equal(reviewed) || !gotIndexed.Equal(indexed) {
		t.Fatalf("timestamps = %s / %s, want %s / %s", gotReviewed, gotIndexed, reviewed, indexed)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM okf_proposal_transitions WHERE proposal_id=$1`, p.ID).Scan(&transitions); err != nil {
		t.Fatal(err)
	}
	if transitions != 2 {
		t.Fatalf("transitions = %d, want 2", transitions)
	}
}
