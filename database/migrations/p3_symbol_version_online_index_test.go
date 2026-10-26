package migrations

import (
	"os"
	"strings"
	"testing"
)

func TestP3SymbolVersionCopyIndexBuildsConcurrently(t *testing.T) {
	contents, err := os.ReadFile("000074_code_symbol_versions_file_version_idx.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := strings.ToUpper(string(contents))
	if !strings.Contains(sql, "CREATE INDEX CONCURRENTLY") ||
		strings.Contains(sql, "BEGIN;") || strings.Contains(sql, "COMMIT;") {
		t.Fatal("incremental copy index must build concurrently outside a transaction")
	}
}
