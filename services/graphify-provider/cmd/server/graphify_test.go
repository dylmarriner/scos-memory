package main

import "testing"

// testdata/graph.json is real output from `graphify extract --code-only`
// (graphifyy 0.9.48) run against adapters/cli-wrapper, kept as a fixture so
// the graphNode/graphEdge json tags stay pinned to graphify's actual schema
// instead of silently drifting back to guessed field names.
func TestLoadGraphMatchesRealGraphifyOutput(t *testing.T) {
	doc, err := loadGraph("testdata/graph.json")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	if len(doc.Nodes) != 10 {
		t.Fatalf("expected 10 nodes, got %d", len(doc.Nodes))
	}
	if len(doc.Edges) != 10 {
		t.Fatalf("expected 10 edges (from the \"links\" key), got %d", len(doc.Edges))
	}

	node := doc.Nodes[0]
	if node.ID != "cli_wrapper_adapter" || node.Label != "adapter.go" {
		t.Fatalf("unexpected first node: %+v", node)
	}
	if node.SourceFile != "cli-wrapper/adapter.go" || node.SourceLocation != "L1" {
		t.Fatalf("unexpected node source fields: %+v", node)
	}

	var extracted, inferred graphEdge
	var sawExtracted, sawInferred bool
	for _, edge := range doc.Edges {
		switch edge.Confidence {
		case "EXTRACTED":
			extracted, sawExtracted = edge, true
		case "INFERRED":
			inferred, sawInferred = edge, true
		}
	}
	if !sawExtracted || !sawInferred {
		t.Fatalf("expected both EXTRACTED and INFERRED edges in fixture, got extracted=%v inferred=%v", sawExtracted, sawInferred)
	}
	if relationTypeFor(extracted.Confidence) != "extracted_from" {
		t.Fatalf("relationTypeFor(EXTRACTED) = %q, want extracted_from", relationTypeFor(extracted.Confidence))
	}
	if relationTypeFor(inferred.Confidence) != "inferred_from" {
		t.Fatalf("relationTypeFor(INFERRED) = %q, want inferred_from", relationTypeFor(inferred.Confidence))
	}
}
