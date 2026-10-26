package main

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestPGBulkCopyPreservesDeclarationProvenance(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenant := testTenant(t, db)
	repo, err := store.EnsureRepository(ctx, tenant, "/bulk-provenance/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	var source strings.Builder
	source.WriteString("package provenance\n")
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&source, "// Function%d is documented.\nfunc Function%d(){println(%d)}\n", i, i, i)
	}
	unchanged := source.String()
	caller := "package provenance\nfunc caller(){Function1()}\n"
	parent, err := store.CreateSnapshot(ctx, repo, tenant, "main", "bulk-parent", "go/parser-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, parent, repo, []PGFileInput{
		{Path: "multi.go", Language: "go", Content: unchanged, ContentHash: hashContent(unchanged)},
		{Path: "caller.go", Language: "go", Content: caller, ContentHash: hashContent(caller)},
	}); err != nil {
		t.Fatal(err)
	}
	changed := "package provenance\nfunc caller(){Function1();println(9)}\n"
	target, err := store.CreateSnapshot(ctx, repo, tenant, "main", "bulk-target", "go/parser-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, target, repo, []PGFileInput{
		{Path: "multi.go", Language: "go", ContentHash: hashContent(unchanged), ReuseParent: true, ExpectedParentCommit: "bulk-parent"},
		{Path: "caller.go", Language: "go", Content: changed, ContentHash: hashContent(changed)},
	}); err != nil {
		t.Fatal(err)
	}
	load := func(snapshot string) []string {
		t.Helper()
		rows, err := db.QueryContext(ctx, `
            SELECT sv.fully_qualified_name, sv.kind, COALESCE(sv.range,''),
                   COALESCE(sv.signature,''), sv.content_hash,
                   COALESCE(sv.doc_comment,''), sv.confidence_class,
                   COALESCE(sv.extractor,''), COALESCE(sv.scope,''),
                   COALESCE(sv.called_names,''), COALESCE(cc.content,''),
                   COALESCE(cc.content_hash,'')
            FROM code_symbol_versions sv
            LEFT JOIN code_chunks cc ON cc.symbol_version_id=sv.id
            WHERE sv.snapshot_id=$1 AND sv.path='multi.go'
            ORDER BY sv.fully_qualified_name,sv.kind,sv.range`, snapshot)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var result []string
		for rows.Next() {
			values := make([]string, 12)
			pointers := make([]any, len(values))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			result = append(result, strings.Join(values, "|"))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return result
	}
	original := load(parent)
	copied := load(target)
	if len(original) != 41 || !reflect.DeepEqual(original, copied) {
		t.Fatalf("bulk-copy provenance differs: original=%d copied=%d equal=%v", len(original), len(copied), reflect.DeepEqual(original, copied))
	}
}
