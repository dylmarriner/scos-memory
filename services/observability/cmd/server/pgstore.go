package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pgHistoryStore persists rollups and benchmark runs in the tables created by
// database/migrations/000038_observability_history.sql. Every transaction
// identifies as the observability service so the tables' RLS policies admit
// it under the restricted scos_app role.
type pgHistoryStore struct{ db *pgxpool.Pool }

const observabilityServiceID = "observability"

func (p *pgHistoryStore) tx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := p.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT set_config('scos.service_id', $1, true)`, observabilityServiceID); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (p *pgHistoryStore) UpsertRollups(ctx context.Context, rows []rollupRow) error {
	if len(rows) == 0 {
		return nil
	}
	return p.tx(ctx, func(tx pgx.Tx) error {
		batch := &pgx.Batch{}
		for _, row := range rows {
			hist := row.Agg.Histogram
			if hist == nil {
				hist = []int64{}
			}
			batch.Queue(`
				INSERT INTO observability_rollups
					(resolution, bucket_start, name, service, caller, route, unit,
					 sample_count, value_sum, value_min, value_max, value_last, error_count, histogram, updated_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, now())
				ON CONFLICT (resolution, bucket_start, name, service, caller, route) DO UPDATE SET
					unit = EXCLUDED.unit,
					sample_count = EXCLUDED.sample_count,
					value_sum = EXCLUDED.value_sum,
					value_min = EXCLUDED.value_min,
					value_max = EXCLUDED.value_max,
					value_last = EXCLUDED.value_last,
					error_count = EXCLUDED.error_count,
					histogram = EXCLUDED.histogram,
					updated_at = now()`,
				row.Key.Resolution, time.Unix(row.Key.Bucket, 0).UTC(), row.Key.Name, row.Key.Service, row.Key.Caller, row.Key.Route, row.Agg.Unit,
				row.Agg.Count, row.Agg.Sum, row.Agg.Min, row.Agg.Max, row.Agg.Last, row.Agg.Errors, hist)
		}
		results := tx.SendBatch(ctx, batch)
		for range rows {
			if _, err := results.Exec(); err != nil {
				results.Close()
				return fmt.Errorf("upsert rollup: %w", err)
			}
		}
		return results.Close()
	})
}

func (p *pgHistoryStore) LoadRollups(ctx context.Context, minuteSince, hourSince time.Time) ([]rollupRow, error) {
	var out []rollupRow
	err := p.tx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT resolution, bucket_start, name, service, caller, route, unit,
			       sample_count, value_sum, value_min, value_max, value_last, error_count, histogram
			FROM observability_rollups
			WHERE (resolution = '1m' AND bucket_start >= $1) OR (resolution = '1h' AND bucket_start >= $2)`,
			minuteSince.UTC(), hourSince.UTC())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var row rollupRow
			var bucket time.Time
			if err := rows.Scan(&row.Key.Resolution, &bucket, &row.Key.Name, &row.Key.Service, &row.Key.Caller, &row.Key.Route, &row.Agg.Unit,
				&row.Agg.Count, &row.Agg.Sum, &row.Agg.Min, &row.Agg.Max, &row.Agg.Last, &row.Agg.Errors, &row.Agg.Histogram); err != nil {
				return err
			}
			row.Key.Bucket = bucket.Unix()
			if len(row.Agg.Histogram) == 0 {
				row.Agg.Histogram = nil
			}
			out = append(out, row)
		}
		return rows.Err()
	})
	return out, err
}

func (p *pgHistoryStore) PruneRollups(ctx context.Context, minuteBefore, hourBefore time.Time) error {
	return p.tx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			DELETE FROM observability_rollups
			WHERE (resolution = '1m' AND bucket_start < $1) OR (resolution = '1h' AND bucket_start < $2)`,
			minuteBefore.UTC(), hourBefore.UTC())
		return err
	})
}

func (p *pgHistoryStore) SaveBenchmarkRun(ctx context.Context, run BenchmarkRun) error {
	raw, err := json.Marshal(run)
	if err != nil {
		return err
	}
	recorded, err := time.Parse(time.RFC3339Nano, run.RecordedAt)
	if err != nil {
		return err
	}
	return p.tx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO observability_benchmark_runs (run_id, suite, recorded_at, run)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (run_id) DO NOTHING`, run.RunID, run.Suite, recorded.UTC(), raw)
		return err
	})
}

func (p *pgHistoryStore) LoadBenchmarkRuns(ctx context.Context, limit int) ([]BenchmarkRun, error) {
	var out []BenchmarkRun
	err := p.tx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT run FROM observability_benchmark_runs ORDER BY recorded_at DESC LIMIT $1`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var raw []byte
			if err := rows.Scan(&raw); err != nil {
				return err
			}
			var run BenchmarkRun
			if err := json.Unmarshal(raw, &run); err != nil {
				return err
			}
			out = append(out, run)
		}
		return rows.Err()
	})
	// Stored newest-first; the in-memory history is oldest-first.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, err
}
