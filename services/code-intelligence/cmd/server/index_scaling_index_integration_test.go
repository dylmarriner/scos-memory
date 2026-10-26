package main

import (
	"context"
	"strings"
	"testing"
)

func TestPGIncrementalCopyHasFileVersionLookupIndex(t *testing.T) {
	_, db := testPGStore(t)
	var definition string
	err := db.QueryRowContext(context.Background(), `
        SELECT indexdef FROM pg_indexes WHERE schemaname='public'
          AND indexname='code_symbol_versions_file_version_idx'`).Scan(&definition)
	if err != nil {
		t.Fatalf("incremental declaration lookup index missing: %v", err)
	}
	if !strings.Contains(definition, "(file_version_id)") {
		t.Fatalf("wrong incremental declaration lookup index: %s", definition)
	}
}
