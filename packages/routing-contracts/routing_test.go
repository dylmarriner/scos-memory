package routingcontracts

import (
	"testing"
	"time"
)

func TestIngressEventHash(t *testing.T) {
	evt := &IngressEvent{
		EventID:    "evt-100",
		SessionID:  "sess-1",
		TenantID:   "tenant-scos",
		ActorID:    "user-1",
		Content:    "Hello Memory Fabric",
		OccurredAt: time.Date(2026, 7, 27, 8, 0, 0, 0, time.UTC),
	}

	hash1 := evt.ComputeEventHash()
	hash2 := evt.ComputeEventHash()

	if hash1 == "" {
		t.Fatal("expected non-empty hash")
	}
	if hash1 != hash2 {
		t.Fatalf("expected deterministic hash, got %s vs %s", hash1, hash2)
	}
}
