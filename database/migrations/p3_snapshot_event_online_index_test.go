package migrations

import (
	"os"
	"strings"
	"testing"
)

// The migration runner applies each SQL file via psql -f without an outer
// transaction, so this index can be built online on an already-active ledger.
func TestP3SnapshotEventKeysetIndexBuildsConcurrently(t *testing.T) {
	contents, err := os.ReadFile("000073_snapshot_event_keyset_index.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := strings.ToUpper(string(contents))
	if !strings.Contains(sql, "CREATE INDEX CONCURRENTLY") ||
		strings.Contains(sql, "BEGIN;") || strings.Contains(sql, "COMMIT;") {
		t.Fatal("snapshot event keyset index must build concurrently outside a transaction")
	}
}
