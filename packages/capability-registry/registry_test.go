package capabilityregistry

import (
	"testing"
)

func TestRegistry(t *testing.T) {
	reg := NewRegistry()

	cap := &CapabilityRecord{
		ID:                  "cap-memory-recall",
		Name:                "Memory Recall",
		Type:                CapTypeMemory,
		ConfidenceThreshold: 0.75,
	}

	if err := reg.Register(cap); err != nil {
		t.Fatalf("failed to register capability: %v", err)
	}

	got, found := reg.Get("cap-memory-recall")
	if !found || got.Name != "Memory Recall" {
		t.Fatalf("unexpected capability lookup: %v", got)
	}

	healthy := reg.ListHealthy(CapTypeMemory)
	if len(healthy) != 1 {
		t.Fatalf("expected 1 healthy memory capability, got %d", len(healthy))
	}

	if err := reg.UpdateHealth("cap-memory-recall", "degraded"); err != nil {
		t.Fatalf("failed health update: %v", err)
	}

	healthyAfter := reg.ListHealthy(CapTypeMemory)
	if len(healthyAfter) != 0 {
		t.Fatalf("expected 0 healthy memory capabilities after degradation, got %d", len(healthyAfter))
	}
}
