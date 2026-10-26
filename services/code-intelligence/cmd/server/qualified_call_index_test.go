package main

import (
	"context"
	"fmt"
	"sort"
	"testing"
)

func TestQualifiedCallCandidateIndexMatchesSuffixResolution(t *testing.T) {
	decls := []indexedDecl{
		{Name: "Run", Kind: "function", Dir: "pkg", SymbolVerID: "a"},
		{Name: "Run", Kind: "function", Dir: "org/pkg", SymbolVerID: "b"},
		{Name: "Run", Kind: "function", Dir: "lib/pkg", SymbolVerID: "c"},
		{Name: "Run", Kind: "function", Dir: ".", SymbolVerID: "root"},
		{Name: "Run", Kind: "function", Dir: "x", SymbolVerID: "d"},
		{Name: "Other", Kind: "function", Dir: "pkg", SymbolVerID: "other"},
		{Name: "Run", Kind: "method", Dir: "pkg", SymbolVerID: "method"},
	}
	index := indexQualifiedCallTargets(decls)
	for _, path := range []string{"pkg", "example.com/org/pkg", "example.com/lib/pkg",
		"example.com/x", "example.com/else", "example.com/org/pkg/", "foo//pkg"} {
		var expected, actual []string
		for _, d := range decls {
			if d.Kind == "function" && d.Name == "Run" && importPathNamesDir(path, d.Dir) {
				expected = append(expected, d.SymbolVerID)
			}
		}
		for _, d := range qualifiedCallCandidates(index, path, "Run") {
			actual = append(actual, d.SymbolVerID)
		}
		sort.Strings(expected)
		sort.Strings(actual)
		if fmt.Sprint(expected) != fmt.Sprint(actual) {
			t.Errorf("%q: candidates %v, want %v", path, actual, expected)
		}
	}
}

func BenchmarkResolveCallEdgesQualifiedSameNameFanout(b *testing.B) {
	const count = 3000
	decls := make([]indexedDecl, count)
	for i := range decls {
		dir := fmt.Sprintf("org/package_%04d", i)
		decls[i] = indexedDecl{Kind: "function", Name: "Run", Scope: dir,
			Dir: dir, SymbolVerID: fmt.Sprintf("version-%d", i),
			CalledNames: []string{fmt.Sprintf("example.com/%s%sRun", dir, qualifiedCallSeparator)}}
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if got := len(resolveCallEdges(decls)); got != count {
			b.Fatalf("resolved %d edges, want %d", got, count)
		}
	}
}

// Real PostgreSQL regression: import-path suffix matching must preserve
// ambiguity when both a nested and a shorter repository directory match.
func TestPGQualifiedCallIndexPreservesNestedPackageAmbiguity(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenant := testTenant(t, db)
	repo, err := store.EnsureRepository(ctx, tenant, "/qualified-index/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	const auth = "package auth\nfunc Sign() {}\n"
	const other = "package other\nfunc Sign() {}\n"
	const gateway = `package gateway
import (
    a "example.com/org/auth"
    b "example.com/org/packages/auth"
    c "example.com/org/packages/other"
)
func valid() { a.Sign() }
func ambiguous() { b.Sign() }
func unrelated() { c.Sign() }
`
	snap, err := store.CreateSnapshot(ctx, repo, tenant, "main", "qualified-index-1", "go/parser-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	files := []PGFileInput{
		{Path: "auth/sign.go", Language: "go", Content: auth, ContentHash: hashContent(auth)},
		{Path: "packages/auth/sign.go", Language: "go", Content: auth, ContentHash: hashContent(auth)},
		{Path: "packages/other/sign.go", Language: "go", Content: other, ContentHash: hashContent(other)},
		{Path: "cmd/gateway/main.go", Language: "go", Content: gateway, ContentHash: hashContent(gateway)},
	}
	if err := store.IndexSnapshot(ctx, snap, repo, files); err != nil {
		t.Fatal(err)
	}
	rows, err := db.QueryContext(ctx, `SELECT src.fully_qualified_name, dst.path
        FROM code_edges e
        JOIN code_symbol_versions src ON src.id=e.source_symbol_version_id
        JOIN code_symbol_versions dst ON dst.id=e.target_symbol_version_id
        WHERE e.snapshot_id=$1 AND e.relation='CALLS'
        ORDER BY src.fully_qualified_name, dst.path`, snap)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var edges []string
	for rows.Next() {
		var caller, target string
		if err := rows.Scan(&caller, &target); err != nil {
			t.Fatal(err)
		}
		edges = append(edges, caller+" -> "+target)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"gateway.unrelated -> packages/other/sign.go",
		"gateway.valid -> auth/sign.go",
	}
	if fmt.Sprint(edges) != fmt.Sprint(want) {
		t.Fatalf("qualified CALLS edges %v, want %v", edges, want)
	}
}
