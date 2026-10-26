package main

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"sort"
	"testing"
)

// A change in one Go package cannot alter unqualified call resolution in a
// different package. Qualified callers are intentionally invalidated when
// any target package changes, including when their old edge disappears.
func TestPGPartialCallGraphReusesOnlyUnaffectedScopes(t *testing.T) {
	cases := []struct {
		name         string
		bSource      string
		wantCopied   int
		wantResolved int
		wantEdges    []string
	}{
		{
			name:       "unqualified_independent_package",
			bSource:    "package b\nfunc C(){D()}\nfunc D(){}\n",
			wantCopied: 1, wantResolved: 1,
			wantEdges: []string{"a.A -> a.E", "b.C -> b.D"},
		},
		{
			name:       "qualified_cross_package_caller",
			bSource:    "package b\nimport \"example.com/project/pkg/a\"\nfunc C(){a.B()}\nfunc D(){}\n",
			wantCopied: 0, wantResolved: 1,
			wantEdges: []string{"a.A -> a.E"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, db := testPGStore(t)
			ctx := context.Background()
			tenant := testTenant(t, db)
			repo, err := store.EnsureRepository(ctx, tenant, "/p3-partial/"+t.Name())
			if err != nil {
				t.Fatal(err)
			}
			files := []PGFileInput{
				{Path: "pkg/a/a.go", Language: "go", Content: "package a\nfunc A(){B()}\nfunc B(){}\n"},
				{Path: "pkg/b/b.go", Language: "go", Content: tc.bSource},
			}
			for i := range files {
				files[i].ContentHash = hashContent(files[i].Content)
			}
			parent, err := store.CreateSnapshot(ctx, repo, tenant, "main", "parent", "go/parser-v1", "")
			if err != nil {
				t.Fatal(err)
			}
			if err := store.IndexSnapshot(ctx, parent, repo, files); err != nil {
				t.Fatal(err)
			}
			changed := append([]PGFileInput(nil), files...)
			changed[0].Content = "package a\nfunc A(){E()}\nfunc E(){}\n"
			changed[0].ContentHash = hashContent(changed[0].Content)
			changed[1].Content = ""
			changed[1].ReuseParent = true
			changed[1].ExpectedParentCommit = "parent"
			next, err := store.CreateSnapshot(ctx, repo, tenant, "main", "next", "go/parser-v1", "")
			if err != nil {
				t.Fatal(err)
			}
			if err := store.IndexSnapshot(ctx, next, repo, changed); err != nil {
				t.Fatal(err)
			}
			var copied, resolved int
			if err := db.QueryRowContext(ctx, "SELECT copied_call_edge_count,resolved_call_edge_count FROM code_repository_snapshots WHERE id=$1", next).Scan(&copied, &resolved); err != nil {
				t.Fatal(err)
			}
			if copied != tc.wantCopied || resolved != tc.wantResolved {
				t.Fatalf("copied=%d resolved=%d; want %d,%d", copied, resolved, tc.wantCopied, tc.wantResolved)
			}
			rows, err := db.QueryContext(ctx, "SELECT src.fully_qualified_name,dst.fully_qualified_name FROM code_edges e JOIN code_symbol_versions src ON src.id=e.source_symbol_version_id JOIN code_symbol_versions dst ON dst.id=e.target_symbol_version_id WHERE e.snapshot_id=$1 AND e.relation='CALLS'", next)
			if err != nil {
				t.Fatal(err)
			}
			var edges []string
			for rows.Next() {
				var src, dst string
				if err := rows.Scan(&src, &dst); err != nil {
					rows.Close()
					t.Fatal(err)
				}
				edges = append(edges, src+" -> "+dst)
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			rows.Close()
			sort.Strings(edges)
			if !reflect.DeepEqual(edges, tc.wantEdges) {
				t.Fatalf("edges=%v want %v", edges, tc.wantEdges)
			}
		})
	}
}

func TestPGPartialCallGraphRejectsUntrustedParent(t *testing.T) {
	for _, kind := range []string{"legacy_resolver", "non_native_edge"} {
		t.Run(kind, func(t *testing.T) {
			store, db := testPGStore(t)
			ctx := context.Background()
			tenant := testTenant(t, db)
			repo, err := store.EnsureRepository(ctx, tenant, "/p3-partial-guard/"+t.Name())
			if err != nil {
				t.Fatal(err)
			}
			files := []PGFileInput{
				{Path: "a.go", Language: "go", Content: "package a\nfunc A(){B()}\nfunc B(){}\n"},
				{Path: "b.go", Language: "go", Content: "package b\nfunc C(){D()}\nfunc D(){}\n"},
			}
			for i := range files {
				files[i].ContentHash = hashContent(files[i].Content)
			}
			parent, err := store.CreateSnapshot(ctx, repo, tenant, "main", "parent", "go/parser-v1", "")
			if err != nil {
				t.Fatal(err)
			}
			if err := store.IndexSnapshot(ctx, parent, repo, files); err != nil {
				t.Fatal(err)
			}
			if kind == "legacy_resolver" {
				_, err = db.ExecContext(ctx, "UPDATE code_repository_snapshots SET call_resolver_version='legacy' WHERE id=$1", parent)
			} else {
				_, err = db.ExecContext(ctx, "INSERT INTO code_edges(snapshot_id,source_symbol_version_id,target_symbol_version_id,relation,confidence_class) SELECT snapshot_id,source_symbol_version_id,target_symbol_version_id,'REFERENCES','deterministic' FROM code_edges WHERE snapshot_id=$1 LIMIT 1", parent)
			}
			if err != nil {
				t.Fatal(err)
			}
			files[0].Content = "package a\nfunc A(){E()}\nfunc E(){}\n"
			files[0].ContentHash = hashContent(files[0].Content)
			files[1].Content = ""
			files[1].ReuseParent = true
			files[1].ExpectedParentCommit = "parent"
			next, err := store.CreateSnapshot(ctx, repo, tenant, "main", "next", "go/parser-v1", "")
			if err != nil {
				t.Fatal(err)
			}
			if err := store.IndexSnapshot(ctx, next, repo, files); err != nil {
				t.Fatal(err)
			}
			var copied, resolved int
			if err := db.QueryRowContext(ctx, "SELECT copied_call_edge_count,resolved_call_edge_count FROM code_repository_snapshots WHERE id=$1", next).Scan(&copied, &resolved); err != nil {
				t.Fatal(err)
			}
			if copied != 0 || resolved != 2 {
				t.Fatalf("untrusted parent copied=%d resolved=%d", copied, resolved)
			}
		})
	}
}

func TestPGPartialCallGraphDeletesOldScopeWithoutLeakingEdges(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenant := testTenant(t, db)
	repo, err := store.EnsureRepository(ctx, tenant, "/p3-partial-deletion/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	files := []PGFileInput{
		{Path: "a.go", Language: "go", Content: "package a\nfunc A(){B()}\nfunc B(){}\n"},
		{Path: "b.go", Language: "go", Content: "package b\nfunc C(){D()}\nfunc D(){}\n"},
	}
	for i := range files {
		files[i].ContentHash = hashContent(files[i].Content)
	}
	parent, err := store.CreateSnapshot(ctx, repo, tenant, "main", "parent", "go/parser-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, parent, repo, files); err != nil {
		t.Fatal(err)
	}
	keep := files[0]
	keep.Content = ""
	keep.ReuseParent = true
	keep.ExpectedParentCommit = "parent"
	next, err := store.CreateSnapshot(ctx, repo, tenant, "main", "next", "go/parser-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, next, repo, []PGFileInput{keep}); err != nil {
		t.Fatal(err)
	}
	var copied, resolved, edges int
	if err := db.QueryRowContext(ctx, "SELECT copied_call_edge_count,resolved_call_edge_count FROM code_repository_snapshots WHERE id=$1", next).Scan(&copied, &resolved); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM code_edges WHERE snapshot_id=$1", next).Scan(&edges); err != nil {
		t.Fatal(err)
	}
	if copied != 1 || resolved != 0 || edges != 1 {
		t.Fatalf("deleted scope leaked: copied=%d resolved=%d edges=%d", copied, resolved, edges)
	}
}

// Stable identities retained by the unchanged-declaration copier may be reused
// even when duplicate-content input files arrive in a different order.
func TestPGPartialCallGraphKeepsStableDuplicateIdentities(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenant := testTenant(t, db)
	repo, err := store.EnsureRepository(ctx, tenant, "/p3-partial-identity/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	files := []PGFileInput{
		{Path: "a.go", Language: "go", Content: "package a\nfunc A(){B()}\nfunc B(){}\n"},
		{Path: "b1.go", Language: "go", Content: "package b\nfunc X(){}\n"},
		{Path: "b2.go", Language: "go", Content: "package b\nfunc X(){}\n"},
		{Path: "bc.go", Language: "go", Content: "package b\nfunc C(){D()}\nfunc D(){}\n"},
	}
	for i := range files {
		files[i].ContentHash = hashContent(files[i].Content)
	}
	parent, err := store.CreateSnapshot(ctx, repo, tenant, "main", "parent", "go/parser-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, parent, repo, files); err != nil {
		t.Fatal(err)
	}
	nextFiles := append([]PGFileInput(nil), files...)
	nextFiles[0].Content = "package a\nfunc A(){E()}\nfunc E(){}\n"
	nextFiles[0].ContentHash = hashContent(nextFiles[0].Content)
	nextFiles[1], nextFiles[2] = nextFiles[2], nextFiles[1]
	for i := 1; i < len(nextFiles); i++ {
		nextFiles[i].Content = ""
		nextFiles[i].ReuseParent = true
		nextFiles[i].ExpectedParentCommit = "parent"
	}
	next, err := store.CreateSnapshot(ctx, repo, tenant, "main", "next", "go/parser-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, next, repo, nextFiles); err != nil {
		t.Fatal(err)
	}
	var copied, resolved int
	if err := db.QueryRowContext(ctx, "SELECT copied_call_edge_count,resolved_call_edge_count FROM code_repository_snapshots WHERE id=$1", next).Scan(&copied, &resolved); err != nil {
		t.Fatal(err)
	}
	if copied != 1 || resolved != 1 {
		t.Fatalf("stable duplicate identity graph not reused correctly: copied=%d resolved=%d", copied, resolved)
	}
}

func scopedGoFiles(groups, filesPerGroup int) []PGFileInput {
	var files []PGFileInput
	for group := 0; group < groups; group++ {
		for i := 0; i < filesPerGroup; i++ {
			body := ""
			if i+1 < filesPerGroup {
				body = fmt.Sprintf("f%d()", i+1)
			}
			content := fmt.Sprintf("package pkg%d\nfunc f%d(){%s}\n", group, i, body)
			files = append(files, PGFileInput{
				Path:     fmt.Sprintf("pkg%d/file_%03d.go", group, i),
				Language: "go", Content: content, ContentHash: hashContent(content),
			})
		}
	}
	return files
}

func snapshotCallLabels(t *testing.T, db *sql.DB, snapshot string) []string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), "SELECT src.fully_qualified_name,dst.fully_qualified_name FROM code_edges e JOIN code_symbol_versions src ON src.id=e.source_symbol_version_id JOIN code_symbol_versions dst ON dst.id=e.target_symbol_version_id WHERE e.snapshot_id=$1 AND e.relation='CALLS'", snapshot)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var labels []string
	for rows.Next() {
		var src, dst string
		if err := rows.Scan(&src, &dst); err != nil {
			t.Fatal(err)
		}
		labels = append(labels, src+" -> "+dst)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(labels)
	return labels
}

func TestPGPartialCallGraph128FilesMatchesCleanRebuild(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenant := testTenant(t, db)
	repo, err := store.EnsureRepository(ctx, tenant, "/p3-partial-scale/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	initial := scopedGoFiles(4, 32)
	parent, err := store.CreateSnapshot(ctx, repo, tenant, "main", "parent", "go/parser-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, parent, repo, initial); err != nil {
		t.Fatal(err)
	}
	changed := append([]PGFileInput(nil), initial...)
	changed[0].Content = "package pkg0\nfunc f0(){f1();println(42)}\n"
	changed[0].ContentHash = hashContent(changed[0].Content)
	cleanFiles := append([]PGFileInput(nil), changed...)
	for i := 1; i < len(changed); i++ {
		changed[i].Content = ""
		changed[i].ReuseParent = true
		changed[i].ExpectedParentCommit = "parent"
	}
	next, err := store.CreateSnapshot(ctx, repo, tenant, "main", "next", "go/parser-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, next, repo, changed); err != nil {
		t.Fatal(err)
	}
	var parsed, reused, copied, resolved int
	if err := db.QueryRowContext(ctx, "SELECT parsed_file_count,reused_file_count,copied_call_edge_count,resolved_call_edge_count FROM code_repository_snapshots WHERE id=$1", next).Scan(&parsed, &reused, &copied, &resolved); err != nil {
		t.Fatal(err)
	}
	if parsed != 1 || reused != 127 || copied != 93 || resolved != 31 {
		t.Fatalf("128-file delta parsed=%d reused=%d copied=%d resolved=%d, want 1,127,93,31", parsed, reused, copied, resolved)
	}
	cleanRepo, err := store.EnsureRepository(ctx, tenant, "/p3-partial-scale-clean/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	clean, err := store.CreateSnapshot(ctx, cleanRepo, tenant, "main", "clean", "go/parser-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, clean, cleanRepo, cleanFiles); err != nil {
		t.Fatal(err)
	}
	got, want := snapshotCallLabels(t, db, next), snapshotCallLabels(t, db, clean)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("partial graph not equivalent to clean: got %d edges, want %d", len(got), len(want))
	}
}

func TestPGPartialCallGraphRejectsModifiedCopiedScopeFingerprint(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenant := testTenant(t, db)
	repo, err := store.EnsureRepository(ctx, tenant, "/p3-partial-fingerprint/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	files := []PGFileInput{
		{Path: "a.go", Language: "go", Content: "package a\nfunc A(){B()}\nfunc B(){}\n"},
		{Path: "b.go", Language: "go", Content: "package b\nfunc C(){D()}\nfunc D(){}\n"},
	}
	for i := range files {
		files[i].ContentHash = hashContent(files[i].Content)
	}
	parent, err := store.CreateSnapshot(ctx, repo, tenant, "main", "parent", "go/parser-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, parent, repo, files); err != nil {
		t.Fatal(err)
	}
	nextFiles := append([]PGFileInput(nil), files...)
	nextFiles[0].Content = "package a\nfunc A(){E()}\nfunc E(){}\n"
	nextFiles[0].ContentHash = hashContent(nextFiles[0].Content)
	nextFiles[1].Content = ""
	nextFiles[1].ReuseParent = true
	nextFiles[1].ExpectedParentCommit = "parent"
	next, err := store.CreateSnapshot(ctx, repo, tenant, "main", "next", "go/parser-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, next, repo, nextFiles); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	// Deliberately tamper with a supposedly reusable declaration in the
	// disposable database. The fast path must refuse to trust this mapping.
	if _, err := tx.ExecContext(ctx, "UPDATE code_symbol_versions SET signature='tampered' WHERE snapshot_id=$1 AND path='b.go' AND kind='function'", next); err != nil {
		t.Fatal(err)
	}
	oldManifest := map[string]snapshotFileFingerprint{}
	newManifest := map[string]snapshotFileFingerprint{}
	for _, f := range files {
		oldManifest[f.Path] = snapshotFileFingerprint{Hash: f.ContentHash, Language: f.Language}
	}
	newManifest["a.go"] = snapshotFileFingerprint{Hash: nextFiles[0].ContentHash, Language: "go"}
	newManifest["b.go"] = oldManifest["b.go"]
	copied, count, _, err := copyUnaffectedParentCallEdgesLocked(ctx, tx, parent, next, oldManifest, newManifest, []indexedDecl{{Scope: "b", Kind: "function", Name: "C"}})
	if err != nil {
		t.Fatal(err)
	}
	if copied || count != 0 {
		t.Fatalf("tampered target declaration reused: copied=%v edges=%d", copied, count)
	}
}
