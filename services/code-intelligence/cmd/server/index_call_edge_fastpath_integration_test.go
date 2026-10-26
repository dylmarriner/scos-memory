package main

import (
	"context"
	"testing"
)

// Identical immutable manifests must reuse their parent's proven CALLS graph.
// Changing even one target file must recompute the graph and must never
// retain a deleted target's edges.
func TestPGIdenticalManifestCopiesCallGraphAndChangedManifestRecomputes(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenant := testTenant(t, db)
	repo, err := store.EnsureRepository(ctx, tenant, "/p3-call-edge-copy/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	files := []PGFileInput{
		{Path: "a.go", Language: "go", Content: "package demo\nfunc A(){B()}\n"},
		{Path: "b.go", Language: "go", Content: "package demo\nfunc B(){}\n"},
		{Path: "c.go", Language: "go", Content: "package demo\nfunc C(){B()}\n"},
	}
	for i := range files {
		files[i].ContentHash = hashContent(files[i].Content)
	}
	check := func(commit string, input []PGFileInput, wantEdges, wantCopied, wantResolved int) string {
		t.Helper()
		snap, err := store.CreateSnapshot(ctx, repo, tenant, "main", commit, "go/parser-v1", "")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.IndexSnapshot(ctx, snap, repo, input); err != nil {
			t.Fatal(err)
		}
		var edges, copied, resolved int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM code_edges WHERE snapshot_id=$1`, snap).Scan(&edges); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRowContext(ctx, `SELECT copied_call_edge_count,resolved_call_edge_count FROM code_repository_snapshots WHERE id=$1`, snap).Scan(&copied, &resolved); err != nil {
			t.Fatal(err)
		}
		if edges != wantEdges || copied != wantCopied || resolved != wantResolved {
			t.Fatalf("%s edges=%d copied=%d resolved=%d; want %d,%d,%d", commit, edges, copied, resolved, wantEdges, wantCopied, wantResolved)
		}
		return snap
	}
	first := check("parent", files, 2, 0, 2)
	identical := append([]PGFileInput(nil), files...)
	identical[0], identical[2] = identical[2], identical[0]
	for i := range identical {
		identical[i].Content = ""
		identical[i].ReuseParent = true
		identical[i].ExpectedParentCommit = "parent"
	}
	second := check("identical", identical, 2, 2, 0)
	changed := append([]PGFileInput(nil), files...)
	changed[1].Content = "package demo\nfunc D(){}\n"
	changed[1].ContentHash = hashContent(changed[1].Content)
	third := check("changed", changed, 0, 0, 0)
	fourth := check("restored", files, 2, 0, 2)
	for _, snap := range []string{first, second, third, fourth} {
		var status string
		if err := db.QueryRowContext(ctx, `SELECT status::text FROM code_repository_snapshots WHERE id=$1`, snap).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if snap == fourth && status != "active" {
			t.Fatalf("latest not active: %s", status)
		}
		if snap != fourth && status != "superseded" {
			t.Fatalf("historical snapshot not superseded: %s", status)
		}
	}
}

// Duplicate-content files can swap their conceptual symbol IDs when file
// order changes. The fast path must reject that mapping and recompute edges.
func TestPGCallGraphCopyRejectsSwappedDuplicateSymbolIdentity(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenant := testTenant(t, db)
	repo, err := store.EnsureRepository(ctx, tenant, "/p3-duplicate-copy/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	files := []PGFileInput{
		{Path: "dupe1.go", Language: "go", Content: "package demo\nfunc B(){}\n"},
		{Path: "dupe2.go", Language: "go", Content: "package demo\nfunc B(){}\n"},
		{Path: "caller.go", Language: "go", Content: "package demo\nfunc A(){C()}\n"},
		{Path: "c.go", Language: "go", Content: "package demo\nfunc C(){}\n"},
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
	nextFiles[0], nextFiles[1] = nextFiles[1], nextFiles[0]
	for i := range nextFiles {
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
	if err := db.QueryRowContext(ctx, `SELECT copied_call_edge_count,resolved_call_edge_count FROM code_repository_snapshots WHERE id=$1`, next).Scan(&copied, &resolved); err != nil {
		t.Fatal(err)
	}
	if copied != 0 || resolved != 1 {
		t.Fatalf("unsafe duplicate identity graph reuse: copied=%d resolved=%d", copied, resolved)
	}
}

// An imported or manually corrupted parent graph must not be blindly
// propagated into a clean native index generation.
func TestPGCallGraphCopyRejectsNonNativeParentEdges(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenant := testTenant(t, db)
	repo, err := store.EnsureRepository(ctx, tenant, "/p3-nonnative-copy/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	files := []PGFileInput{
		{Path: "caller.go", Language: "go", Content: "package demo\nfunc A(){B()}\n"},
		{Path: "target.go", Language: "go", Content: "package demo\nfunc B(){}\n"},
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
	_, err = db.ExecContext(ctx, `INSERT INTO code_edges
        (snapshot_id,source_symbol_version_id,target_symbol_version_id,relation,confidence_class)
        SELECT snapshot_id,source_symbol_version_id,target_symbol_version_id,'REFERENCES','deterministic'
        FROM code_edges WHERE snapshot_id=$1 AND relation='CALLS'`, parent)
	if err != nil {
		t.Fatal(err)
	}
	next, err := store.CreateSnapshot(ctx, repo, tenant, "main", "next", "go/parser-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	for i := range files {
		files[i].Content = ""
		files[i].ReuseParent = true
		files[i].ExpectedParentCommit = "parent"
	}
	if err := store.IndexSnapshot(ctx, next, repo, files); err != nil {
		t.Fatal(err)
	}
	var copied, resolved, edgeCount int
	if err := db.QueryRowContext(ctx, `SELECT copied_call_edge_count,resolved_call_edge_count FROM code_repository_snapshots WHERE id=$1`, next).Scan(&copied, &resolved); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM code_edges WHERE snapshot_id=$1`, next).Scan(&edgeCount); err != nil {
		t.Fatal(err)
	}
	if copied != 0 || resolved != 1 || edgeCount != 1 {
		t.Fatalf("non-native parent graph leaked: copied=%d resolved=%d total=%d", copied, resolved, edgeCount)
	}
}
