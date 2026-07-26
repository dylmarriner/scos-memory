package mcpadapter

import (
	"context"
	"testing"
)

func TestMCPHandler(t *testing.T) {
	h := &Handler{}
	req := &MCPRequest{
		JSONRPC: "2.0",
		ID:      "req-1",
		Method:  "tools/call",
	}

	resp, err := h.HandleCall(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.ID != "req-1" {
		t.Fatalf("expected ID 'req-1', got %s", resp.ID)
	}
}
