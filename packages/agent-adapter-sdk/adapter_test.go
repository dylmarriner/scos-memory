package agentadaptersdk

import (
	"context"
	"testing"
)

func TestNewClient(t *testing.T) {
	cfg := AdapterConfig{
		AgentID:  "agent-test-1",
		Boundary: BoundaryHTTP,
	}

	cli, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	env, err := cli.RenderEnvelope(context.Background(), "sess-1", "tenant-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if env.AgentID != "agent-test-1" {
		t.Fatalf("expected agent_id 'agent-test-1', got %s", env.AgentID)
	}
}
