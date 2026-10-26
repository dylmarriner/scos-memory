package main

import (
	"context"
	"strings"
	"testing"
)

func TestPGSnapshotManifestCursorHasCollationCompatibleIndex(t *testing.T) {
	_, db := testPGStore(t)
	var definition string
	err := db.QueryRowContext(context.Background(), `
        SELECT indexdef FROM pg_indexes
        WHERE schemaname='public'
          AND indexname='code_file_versions_snapshot_path_c_idx'
    `).Scan(&definition)
	if err != nil {
		t.Fatalf("C-collated keyset index missing: %v", err)
	}
	if !strings.Contains(definition, "snapshot_id") ||
		!strings.Contains(definition, `path COLLATE "C"`) {
		t.Fatalf("cursor index does not match C-collated keyset query: %s", definition)
	}
}
