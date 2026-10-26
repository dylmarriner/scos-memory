package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestResolveCallEdgesAndBoundedInsertBatch(t *testing.T) {
	const count = 257
	decls := make([]indexedDecl, 0, count+3)
	for i := 0; i < count; i++ {
		decls = append(decls, indexedDecl{
			SymbolVerID: fmt.Sprintf("version-%d", i), Scope: "pkg",
			Name: fmt.Sprintf("f%d", i), Kind: "function",
			CalledNames: []string{fmt.Sprintf("f%d", (i+1)%count)},
			Extractor:   "go/parser",
		})
	}
	// Ambiguous calls must never manufacture a target.
	decls = append(decls,
		indexedDecl{SymbolVerID: "ambig-1", Scope: "other", Name: "dup", Kind: "function"},
		indexedDecl{SymbolVerID: "ambig-2", Scope: "other", Name: "dup", Kind: "function"},
		indexedDecl{SymbolVerID: "caller", Scope: "other", Name: "caller", Kind: "function",
			CalledNames: []string{"dup"}, Extractor: "go/parser"},
	)
	edges := resolveCallEdges(decls)
	if len(edges) != count {
		t.Fatalf("resolved %d edges, want %d (ambiguous must be excluded)", len(edges), count)
	}
	if edges[0].SourceID != "version-0" || edges[0].TargetID != "version-1" ||
		edges[0].Extractor != "go/parser" {
		t.Fatalf("first edge lost identity or provenance: %+v", edges[0])
	}
	if edges[count-1].TargetID != "version-0" {
		t.Fatalf("last edge lost cycle: %+v", edges[count-1])
	}
	for start := 0; start < len(edges); start += callEdgeBatchSize {
		end := start + callEdgeBatchSize
		if end > len(edges) {
			end = len(edges)
		}
		statement, args := buildCallEdgeInsert("snapshot", edges[start:end])
		if len(args) != 1+3*(end-start) {
			t.Fatalf("bad argument count: %d", len(args))
		}
		if !strings.Contains(statement, fmt.Sprintf("$%d", len(args))) {
			t.Fatalf("last placeholder missing for %d edges", end-start)
		}
		if args[0] != "snapshot" {
			t.Fatal("batch lost snapshot identity")
		}
	}
}
