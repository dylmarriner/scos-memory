package main

import (
	"context"
	"testing"
)

func TestMergeFallbackKeepsStatementsThatDifferInInformation(t *testing.T) {
	if llmClient.Enabled() {
		t.Skip("model-backed merge decision configured")
	}
	tag := " [benchmark run f63e5d833a94]"
	fact := func(id, content string) *Fact { return &Fact{ID: id, Content: content, Network: "world"} }
	alpha := fact("alpha", "project alpha deployment parameters for organization scos"+tag)

	for name, tc := range map[string]struct {
		content string
		merge   bool
	}{
		"other project":           {"project beta deployment parameters for organization scos" + tag, false},
		"short distinguishing id": {"project alpha deployment parameters for organization scos v2" + tag, true},
		"exact repeat":            {"Project alpha deployment parameters for organization SCOS." + tag, true},
		"stopwords only":          {"the project alpha deployment parameters are for the organization scos" + tag, true},
	} {
		t.Run(name, func(t *testing.T) {
			got := findMergeCandidate(context.Background(), fact("new", tc.content), []*Fact{alpha})
			if (got != nil) != tc.merge {
				t.Fatalf("merge = %v, want %v", got != nil, tc.merge)
			}
		})
	}

	versions := findMergeCandidate(context.Background(),
		fact("new", "the public API version is v2 for organization scos deployments"),
		[]*Fact{fact("old", "the public API version is v1 for organization scos deployments")})
	if versions != nil {
		t.Fatalf("v1 and v2 statements were merged")
	}
}
