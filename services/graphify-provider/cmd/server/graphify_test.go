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

// testdata/graph_0.9.52.json is real output from `graphify extract
// --code-only` (graphifyy 0.9.52, the version deployed to production as of
// 2026-09) against this repository itself. Found live: networkx's
// node_link_data changed its default edges-key parameter from "links" to
// "edges" somewhere between 0.9.48 and 0.9.52, and graphify inherited that
// change with no version marker in the output to detect it by -- every
// edge from a real production extraction was silently dropped (decode
// succeeded, node count looked right, Edges was just empty) until
// graphDocument.UnmarshalJSON learned to fall back to "edges" when "links"
// is absent. This is the regression guard for that: if a future graphify
// version renames the key again, this fails loudly instead of silently
// serving an edgeless graph.
func TestLoadGraphAcceptsNewerGraphifyEdgesKey(t *testing.T) {
	doc, err := loadGraph("testdata/graph_0.9.52.json")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	if len(doc.Nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(doc.Nodes))
	}
	if len(doc.Edges) != 1 {
		t.Fatalf("expected 1 edge (from the \"edges\" key, not \"links\"), got %d", len(doc.Edges))
	}
	if doc.Edges[0].Source != "claude_workflows_hostile_repo_audit_scos_fabric" {
		t.Fatalf("unexpected edge: %+v", doc.Edges[0])
	}
}

// A document with genuinely no edges under either key must decode as an
// edgeless graph, not error -- that's a valid (if unusual) answer for a
// tiny or single-file repository.
func TestLoadGraphHandlesNoEdgesUnderEitherKey(t *testing.T) {
	doc, err := loadGraph("testdata/graph_no_edges.json")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	if len(doc.Nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(doc.Nodes))
	}
	if len(doc.Edges) != 0 {
		t.Fatalf("expected 0 edges, got %d", len(doc.Edges))
	}
}
