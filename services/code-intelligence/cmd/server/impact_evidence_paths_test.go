package main

import (
	"reflect"
	"testing"
)

func TestCodeImpactEvidencePathsShortestDeterministicAndCycleSafe(t *testing.T) {
	// Stored CALLS edges are caller -> callee. Exposed paths must travel
	// from the impacted callee to each caller, without inventing hops.
	edges := map[string][]string{
		"a-caller":     {"seed-b", "seed-a", "top"},
		"b-caller":     {"seed-a"},
		"top":          {"b-caller", "a-caller"},
		"disconnected": {"missing"},
	}
	want := map[string][]string{
		"seed-a":   {"code:seed-a"},
		"seed-b":   {"code:seed-b"},
		"a-caller": {"code:seed-a", "code:a-caller"},
		"b-caller": {"code:seed-a", "code:b-caller"},
		"top":      {"code:seed-a", "code:a-caller", "code:top"},
	}
	for i := 0; i < 50; i++ {
		got := codeImpactEvidencePaths([]string{"seed-b", "seed-a", "seed-b"}, edges)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("run %d: unstable, non-shortest, or invented evidence: got=%v want=%v", i, got, want)
		}
	}
	seedOnly := codeImpactEvidencePaths([]string{"seed-a"}, nil)
	if !reflect.DeepEqual(seedOnly, map[string][]string{"seed-a": {"code:seed-a"}}) {
		t.Fatalf("seed-only graph must have self provenance: %v", seedOnly)
	}
}
