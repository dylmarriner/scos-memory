package main

import (
	"context"
	"testing"
)

func TestStageOutcomesUpdateCapabilityRanking(t *testing.T) {
	ids := capabilitiesForSystem("memory-gateway")
	if len(ids) != 1 || ids[0] != "memory-gateway.retrieve" {
		t.Fatalf("capabilitiesForSystem = %v", ids)
	}
	if got := capabilitiesForSystem("journal"); len(got) != 0 {
		t.Fatalf("journal stage owns no registry capability, got %v", got)
	}
	before, _ := routingRegistry.Get("memory-gateway.retrieve")
	t.Cleanup(func() {
		_ = routingRegistry.RestoreObserved("memory-gateway.retrieve", before.HistoricalSuccessRate, before.QualityScore)
	})
	plane := &controlPlane{}
	plane.recordStageOutcome(context.Background(), RouteRequest{}, "memory-gateway", false, 0)
	after, _ := routingRegistry.Get("memory-gateway.retrieve")
	if after.HistoricalSuccessRate >= before.HistoricalSuccessRate {
		t.Fatalf("failure did not lower success rate: %v -> %v", before.HistoricalSuccessRate, after.HistoricalSuccessRate)
	}
	plane.recordStageOutcome(context.Background(), RouteRequest{}, "memory-gateway", true, 1)
	recovered, _ := routingRegistry.Get("memory-gateway.retrieve")
	if recovered.HistoricalSuccessRate <= after.HistoricalSuccessRate {
		t.Fatalf("success did not raise success rate: %v -> %v", after.HistoricalSuccessRate, recovered.HistoricalSuccessRate)
	}
	if recovered.QualityScore <= after.QualityScore {
		t.Fatalf("a complete result did not raise quality: %v -> %v", after.QualityScore, recovered.QualityScore)
	}
	if err := routingRegistry.RestoreObserved("memory-gateway.retrieve", 1.5, 0.5); err == nil {
		t.Fatal("out-of-range observation accepted")
	}
}

func TestRetrievalStageGradedByWhatItWasBuiltFrom(t *testing.T) {
	cases := []struct {
		body    string
		status  string
		quality float64
	}{
		{`{"sections":[{},{}],"degraded_sources":[],"degraded_models":[]}`, stageCompleted, 1},
		{`{"sections":[{},{},{}],"degraded_models":["reranker"]}`, stageCompleted, 0.75},
		{`{"sections":[{}],"degraded_sources":["episodic"],"degraded_models":[]}`, stageDegraded, 0.5},
		{`{"sections":[],"degraded_sources":["facts","episodic"]}`, stageDegraded, 0},
		{`not json`, stageDegraded, 0},
	}
	for _, c := range cases {
		got := retrievalStageResult([]byte(c.body))
		if got.status != c.status || got.quality != c.quality {
			t.Errorf("%s: got %s/%v, want %s/%v (%s)", c.body, got.status, got.quality, c.status, c.quality, got.detail)
		}
	}
}
