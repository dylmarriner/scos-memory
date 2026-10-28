package main

import (
	"strings"
	"testing"
)

// A code result is evidence, not free metadata. The shared token ceiling
// must apply even when a query is classified as code-only.
func TestP4CodeEvidenceUsesSharedBudgetWithoutPartialProvenance(t *testing.T) {
	large := codeQueryResult{SymbolVersionID: "large", FullyQualifiedName: strings.Repeat("x", 512), Path: "large.go", SnapshotID: "snapshot"}
	small := codeQueryResult{SymbolVersionID: "small", FullyQualifiedName: "pkg.F", Path: "small.go", SnapshotID: "snapshot"}
	input := &codeQueryResponse{RepositoryID: "repo", Historical: true, SnapshotID: "snapshot", Results: []codeQueryResult{large, small}}
	smallCost := estimateCodeEvidenceTokens(small)
	largeCost := estimateCodeEvidenceTokens(large)
	if smallCost <= 0 || largeCost <= smallCost {
		t.Fatalf("invalid test costs: large=%d small=%d", largeCost, smallCost)
	}

	got, used := budgetCodeEvidence(input, smallCost)
	if got == nil || len(got.Results) != 1 || got.Results[0].SymbolVersionID != "small" {
		t.Fatalf("budget should preserve the complete fitting result: %+v", got)
	}
	if used != smallCost || used > smallCost {
		t.Fatalf("code evidence tokens = %d, want %d", used, smallCost)
	}
	if !got.Historical || got.RepositoryID != "repo" || got.SnapshotID != "snapshot" {
		t.Fatalf("immutable provenance was lost: %+v", got)
	}
	if len(input.Results) != 2 {
		t.Fatal("budgeting mutated downstream evidence")
	}

	none, used := budgetCodeEvidence(input, 1)
	if none == nil || len(none.Results) != 0 || used != 0 {
		t.Fatalf("oversized code evidence leaked under one-token ceiling: %+v, %d", none, used)
	}
	all, used := budgetCodeEvidence(input, largeCost+smallCost)
	if len(all.Results) != 2 || used != largeCost+smallCost {
		t.Fatalf("fitting evidence lost: %+v, %d", all, used)
	}
	if nilResult, used := budgetCodeEvidence(nil, 100); nilResult != nil || used != 0 {
		t.Fatalf("nil code response: %+v, %d", nilResult, used)
	}
}
