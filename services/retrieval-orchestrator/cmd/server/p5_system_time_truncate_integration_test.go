package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// A row-level DELETE trigger cannot protect an append-only ledger against
// TRUNCATE, including a cascading TRUNCATE initiated on another table.
// Rollback protects the disposable integration fixture in the RED case.
func TestP5MemorySystemHistoryRejectsTruncate(t *testing.T) {
	ctx := context.Background()
	admin := testRetrievalPool(t)
	for _, tc := range []struct{ name, statement string }{
		{"memory_system_events", "TRUNCATE TABLE memory_system_events"},
		{"memory_system_event_purge_audit", "TRUNCATE TABLE memory_system_event_purge_audit"},
		{"tenant_cascade", "TRUNCATE TABLE tenants CASCADE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := admin.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			_, err = tx.Exec(ctx, tc.statement)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "55006" || !strings.Contains(pgErr.Message, "memory system history is append-only") {
				t.Fatalf("%s must fail via history guard with 55006, got %v", tc.statement, err)
			}
		})
	}
}
