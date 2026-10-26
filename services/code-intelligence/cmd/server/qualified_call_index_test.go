package main

import (
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
