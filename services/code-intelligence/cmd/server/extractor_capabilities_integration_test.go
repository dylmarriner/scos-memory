package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Verifies the real workspace scan -> immutable PostgreSQL snapshot pipeline,
// not just the language detector or standalone Tree-sitter parser.
func TestPGP2ModuleSuffixesPersistSyntaxProvenance(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	root := t.TempDir()
	samples := []struct{ path, language, source string }{
		{"service.cjs", "javascript", "function ready() { return true; }\n"},
		{"service.mts", "typescript", "function ready(): boolean { return true; }\n"},
		{"service.cts", "typescript", "function ready(): boolean { return true; }\n"},
	}
	for _, sample := range samples {
		if err := os.WriteFile(filepath.Join(root, sample.path), []byte(sample.source), 0600); err != nil {
			t.Fatal(err)
		}
	}
	files, skipped, err := scanRepository(root)
	if err != nil || len(skipped) != 0 || len(files) != len(samples) {
		t.Fatalf("scan: %v, files=%d skipped=%v", err, len(files), skipped)
	}
	tenant := testTenant(t, db)
	repo, err := store.EnsureRepository(ctx, tenant, "/p2-capabilities/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.CreateSnapshot(ctx, repo, tenant, "main", "p2-capabilities", "p2-fixture-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, snapshot, repo, files); err != nil {
		t.Fatal(err)
	}
	for _, sample := range samples {
		var extractor, confidence string
		err := db.QueryRowContext(ctx, `SELECT extractor, confidence_class
            FROM code_symbol_versions
            WHERE snapshot_id=$1 AND path=$2 AND kind='function'
              AND fully_qualified_name='service.ready'`, snapshot, sample.path).
			Scan(&extractor, &confidence)
		if err != nil {
			t.Errorf("%s: missing persisted function: %v", sample.path, err)
			continue
		}
		if extractor != "tree-sitter:"+sample.language || confidence != "syntax" {
			t.Errorf("%s: got extractor=%q confidence=%q", sample.path, extractor, confidence)
		}
	}
}
