package main

import (
	"context"
	"reflect"
	"testing"
)

// The performance fast-path is correct only if a one-file change produces
// exactly the same symbols, content chunks and call graph as indexing the
// entire target commit from scratch.
func TestPGIncrementalSnapshotEquivalentToCleanRebuild(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenant := testTenant(t, db)
	original := []PGFileInput{
		{Path: "src/caller.go", Language: "go", Content: "package compare\nfunc caller(){target()}\n"},
		{Path: "src/target.go", Language: "go", Content: "package compare\nfunc target(){helper()}\n"},
		{Path: "src/helper.go", Language: "go", Content: "package compare\nfunc helper(){}\n"},
		{Path: "src/unused.go", Language: "go", Content: "package compare\nfunc unused(){}\n"},
	}
	for i := range original {
		original[i].ContentHash = hashContent(original[i].Content)
	}
	changed := append([]PGFileInput(nil), original...)
	changed[1].Content = "package compare\nfunc target(){helper(); helper()}\n"
	changed[1].ContentHash = hashContent(changed[1].Content)
	build := func(path string, initial bool) (string, string) {
		t.Helper()
		repo, err := store.EnsureRepository(ctx, tenant, path+t.Name())
		if err != nil {
			t.Fatal(err)
		}
		if initial {
			parent, err := store.CreateSnapshot(ctx, repo, tenant, "main", "before", "go/parser-v1", "")
			if err != nil {
				t.Fatal(err)
			}
			if err := store.IndexSnapshot(ctx, parent, repo, original); err != nil {
				t.Fatal(err)
			}
		}
		head, err := store.CreateSnapshot(ctx, repo, tenant, "main", "target", "go/parser-v1", "")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.IndexSnapshot(ctx, head, repo, changed); err != nil {
			t.Fatal(err)
		}
		return repo, head
	}
	_, incremental := build("/incremental/", true)
	_, clean := build("/clean/", false)
	rowsFor := func(snapshot string) []string {
		t.Helper()
		rows, err := db.QueryContext(ctx, "SELECT sv.path,sv.fully_qualified_name,sv.kind,sv.content_hash,COALESCE(sv.scope,''),COALESCE(sv.extractor,''),COALESCE(sv.called_names,''),COALESCE(cc.content,'') FROM code_symbol_versions sv LEFT JOIN code_chunks cc ON cc.symbol_version_id=sv.id WHERE sv.snapshot_id=$1 ORDER BY sv.path,sv.fully_qualified_name,sv.kind", snapshot)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var path, fqn, kind, hash, scope, extractor, called, content string
			if err := rows.Scan(&path, &fqn, &kind, &hash, &scope, &extractor, &called, &content); err != nil {
				t.Fatal(err)
			}
			out = append(out, path+"|"+fqn+"|"+kind+"|"+hash+"|"+scope+"|"+extractor+"|"+called+"|"+content)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	edgesFor := func(snapshot string) []string {
		t.Helper()
		rows, err := db.QueryContext(ctx, "SELECT src.fully_qualified_name,dst.fully_qualified_name,e.relation,e.confidence_class,COALESCE(e.extractor,'') FROM code_edges e JOIN code_symbol_versions src ON src.id=e.source_symbol_version_id JOIN code_symbol_versions dst ON dst.id=e.target_symbol_version_id WHERE e.snapshot_id=$1 ORDER BY src.fully_qualified_name,dst.fully_qualified_name,e.relation", snapshot)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var source, dest, relation, confidence, extractor string
			if err := rows.Scan(&source, &dest, &relation, &confidence, &extractor); err != nil {
				t.Fatal(err)
			}
			out = append(out, source+"->"+dest+"|"+relation+"|"+confidence+"|"+extractor)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if want, got := rowsFor(clean), rowsFor(incremental); !reflect.DeepEqual(want, got) {
		t.Fatalf("incremental symbols/chunks differ from clean rebuild: clean=%v incremental=%v", want, got)
	}
	if want, got := edgesFor(clean), edgesFor(incremental); !reflect.DeepEqual(want, got) {
		t.Fatalf("incremental call graph differs from clean rebuild: clean=%v incremental=%v", want, got)
	}
	for _, tc := range []struct {
		id                     string
		wantParsed, wantReused int
	}{
		{incremental, 1, 3}, {clean, 4, 0},
	} {
		var parsed, reused int
		if err := db.QueryRowContext(ctx, "SELECT parsed_file_count,reused_file_count FROM code_repository_snapshots WHERE id=$1", tc.id).Scan(&parsed, &reused); err != nil {
			t.Fatal(err)
		}
		if parsed != tc.wantParsed || reused != tc.wantReused {
			t.Fatalf("wrong changed-content scaling for %s: parsed=%d reused=%d", tc.id, parsed, reused)
		}
	}
}
