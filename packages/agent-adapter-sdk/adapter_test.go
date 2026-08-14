package agentadaptersdk

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewClient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/envelope/render" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"envelope_id":"env-1","session_id":"sess-1","tenant_id":"tenant-1","agent_id":"agent-test-1","context_items":["governed"],"generated_at":"2026-08-04T00:00:00Z"}`))
	}))
	defer server.Close()
	cfg := AdapterConfig{
		AgentID:    "agent-test-1",
		Boundary:   BoundaryHTTP,
		GatewayURL: server.URL,
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
