//go:build integration

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	servicestate "github.com/dylmarriner/scos-memory/packages/service-state"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestCheckpointIsAtomicAndRestartReplaysTheJournal checkpoints through the
// runtime role, acknowledges more records, "crashes", and restores from what
// PostgreSQL and the journal hold: the snapshot and rollups come back with
// their journal position and every later record is replayed exactly once.
func TestCheckpointIsAtomicAndRestartReplaysTheJournal(t *testing.T) {
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
	app, err := servicestate.New(ctx, appURL)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()

	// Preserve whatever snapshot this database already holds; the owner
	// role bypasses RLS, so it reads and restores the row directly.
	var original []byte
	if err := admin.QueryRow(ctx, `SELECT state FROM service_state WHERE service=$1`, observabilityServiceID).Scan(&original); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal(err)
	}
	metricName := "scos.test.checkpoint." + time.Now().UTC().Format("20060102T150405.000000000")
	t.Cleanup(func() {
		_, _ = admin.Exec(ctx, `DELETE FROM observability_rollups WHERE name=$1`, metricName)
		if original != nil {
			_, _ = admin.Exec(ctx, `UPDATE service_state SET state=$2 WHERE service=$1`, observabilityServiceID, original)
		} else {
			_, _ = admin.Exec(ctx, `DELETE FROM service_state WHERE service=$1`, observabilityServiceID)
		}
	})

	point := func(v float64) MetricPoint {
		return MetricPoint{Name: metricName, Value: v, Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
			Labels: map[string]string{"service": "checkpoint-it"}}
	}
	path := filepath.Join(t.TempDir(), "observability.jsonl")
	history := &pgHistoryStore{db: app.DB}
	s, err := newObservabilityServerAt(path)
	if err != nil {
		t.Fatal(err)
	}
	s.history = history
	ingest(t, s, []MetricPoint{point(1)})
	if err := s.checkpoint(ctx); err != nil {
		t.Fatalf("checkpoint as the runtime role: %v", err)
	}
	ingest(t, s, []MetricPoint{point(2)})
	_ = s.store.close()

	var state persistedObservabilityState
	if err := app.Load(ctx, observabilityServiceID, &state); err != nil {
		t.Fatal(err)
	}
	if state.JournalSeq != 1 || state.JournalID == "" {
		t.Fatalf("stored snapshot position = (%q, %d), want this journal at 1", state.JournalID, state.JournalSeq)
	}

	recovered, err := newObservabilityServerAt(path)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.store.close()
	recovered.history = history
	if err := recovered.restoreDurableState(ctx, state); err != nil {
		t.Fatal(err)
	}
	var values []float64
	for _, m := range recovered.metrics {
		if m.Name == metricName {
			values = append(values, m.Value)
		}
	}
	if len(values) != 2 || values[0] != 1 || values[1] != 2 {
		t.Fatalf("restored points %v, want [1 2]", values)
	}
	entries := recovered.rollups.catalog(time.Now().Add(-time.Hour), resolutionMinute)
	var count int64
	for _, e := range entries {
		if e.Name == metricName {
			count += e.Samples
		}
	}
	if count != 2 {
		t.Fatalf("restored rollups hold %d samples of %s, want 2", count, metricName)
	}
}
