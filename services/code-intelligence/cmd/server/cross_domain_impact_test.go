package main

import "testing"

func TestSplitImpactPathsKeepsDomainSignalsSeparate(t *testing.T) {
	paths := []PGImpactPath{
		{NodeType: "memory_fact", NodeKey: "decision-1"},
		{NodeType: "evidence", NodeKey: "citation-1"},
		{NodeType: "impact", NodeKey: "reconciliation-1"},
	}
	knowledge, evidence, reconciliations := splitImpactPaths(paths)
	if len(knowledge) != 1 || knowledge[0].NodeKey != "decision-1" {
		t.Fatalf("knowledge signal lost or mixed: %#v", knowledge)
	}
	if len(evidence) != 1 || evidence[0].NodeKey != "citation-1" {
		t.Fatalf("evidence signal lost or mixed: %#v", evidence)
	}
	if len(reconciliations) != 1 || reconciliations[0].NodeKey != "reconciliation-1" {
		t.Fatalf("reconciliation signal lost or mixed: %#v", reconciliations)
	}
}

func TestPGImpactCarriesSnapshotProvenance(t *testing.T) {
	impact := PGImpact{SnapshotID: "snapshot-1", CommitID: "commit-1", Branch: "main"}
	if impact.SnapshotID == "" || impact.CommitID == "" || impact.Branch == "" {
		t.Fatalf("snapshot provenance missing: %#v", impact)
	}
}
