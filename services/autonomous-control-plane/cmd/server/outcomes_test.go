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
	plane.recordStageOutcome(context.Background(), RouteRequest{}, "memory-gateway", false)
	after, _ := routingRegistry.Get("memory-gateway.retrieve")
	if after.HistoricalSuccessRate >= before.HistoricalSuccessRate {
		t.Fatalf("failure did not lower success rate: %v -> %v", before.HistoricalSuccessRate, after.HistoricalSuccessRate)
	}
	plane.recordStageOutcome(context.Background(), RouteRequest{}, "memory-gateway", true)
	recovered, _ := routingRegistry.Get("memory-gateway.retrieve")
	if recovered.HistoricalSuccessRate <= after.HistoricalSuccessRate {
		t.Fatalf("success did not raise success rate: %v -> %v", after.HistoricalSuccessRate, recovered.HistoricalSuccessRate)
	}
	if err := routingRegistry.RestoreObserved("memory-gateway.retrieve", 1.5, 0.5); err == nil {
		t.Fatal("out-of-range observation accepted")
	}
}
