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

func TestComputeEventHashIsUnambiguous(t *testing.T) {
	at := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	a := IngressEvent{TenantID: "t", SessionID: "s:x", ActorID: "a", Content: "c", OccurredAt: at}
	b := IngressEvent{TenantID: "t", SessionID: "s", ActorID: "x:a", Content: "c", OccurredAt: at}
	if a.ComputeEventHash() == b.ComputeEventHash() {
		t.Fatal("separator-shifted fields collided")
	}
	c := a
	c.OccurredAt = at.Add(time.Millisecond)
	d := a
	d.AgentID = "other"
	if a.ComputeEventHash() == c.ComputeEventHash() || a.ComputeEventHash() == d.ComputeEventHash() {
		t.Fatal("sub-second time and agent must affect the hash")
	}
	if a.ComputeEventHash() != a.ComputeEventHash() {
		t.Fatal("hash must be deterministic")
	}
}
