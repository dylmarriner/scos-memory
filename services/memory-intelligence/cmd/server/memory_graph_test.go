package main

import "testing"

// TestMemoryGraphDropsEdgesToFactsItDoesNotShow covers a graph whose
// relations and entity mentions still name a superseded fact: the edges to
// it must go, or the dashboard ranks nodes by links it can never draw.
func TestMemoryGraphDropsEdgesToFactsItDoesNotShow(t *testing.T) {
	facts := []*Fact{{ID: "fact-current", Content: "Deploys run on Tuesday", Network: "world"}}
	entities := []*Entity{{ID: "entity-deploys", Name: "deploys", Facts: []string{"fact-current", "fact-superseded"}}}
	relations := []*TemporalRelation{
		{SourceID: "fact-current", TargetID: "fact-superseded", Type: "supersedes", Confidence: 1},
		{SourceID: "fact-current", TargetID: "entity-deploys", Type: "evidence_for", Confidence: 0.9},
	}

	nodes, edges := memoryGraph(facts, entities, relations)
	if len(nodes) != 2 {
		t.Fatalf("nodes = %v, want the fact and the entity", nodes)
	}
	if len(edges) != 2 {
		t.Fatalf("edges = %v, want only the two between shown nodes", edges)
	}
	for _, e := range edges {
		if e["source"] == "fact-superseded" || e["target"] == "fact-superseded" {
			t.Fatalf("edge %v names a fact the graph does not show", e)
		}
	}
}
