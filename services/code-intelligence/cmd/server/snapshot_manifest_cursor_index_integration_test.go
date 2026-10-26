package main

import (
	"context"
	"strings"
	"testing"
)

func TestPGSnapshotManifestCursorHasCollationCompatibleIndex(t *testing.T) {
	_, db := testPGStore(t)
	var definition string
	var valid bool
	err := db.QueryRowContext(context.Background(), `
        SELECT i.indisvalid, pg_get_indexdef(i.indexrelid)
        FROM pg_index i
        JOIN pg_class c ON c.oid = i.indexrelid
        JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE n.nspname='public'
          AND c.relname='code_file_versions_snapshot_path_c_idx'
    `).Scan(&valid, &definition)
	if err != nil {
		t.Fatalf("C-collated keyset index missing: %v", err)
	}
	if !valid {
		t.Fatal("C-collated keyset index is invalid (concurrent build may have been interrupted)")
	}
	if !strings.Contains(definition, "snapshot_id") ||
		!strings.Contains(definition, `path COLLATE "C"`) {
		t.Fatalf("cursor index does not match C-collated keyset query: %s", definition)
	}
}
