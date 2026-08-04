package mcpadapter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMCPHandler(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/control/ingest" || r.Method != http.MethodPost {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"completed","interaction_id":"req-1"}`))
	}))
	defer server.Close()
	h := &Handler{EndpointURL: server.URL}
	req := &MCPRequest{
		JSONRPC: "2.0",
		ID:      "req-1",
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"scos.route","arguments":{"input":"Hello","tenant_id":"tenant","actor_id":"principal"}}`),
	}

	resp, err := h.HandleCall(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.ID != "req-1" {
		t.Fatalf("expected ID 'req-1', got %s", resp.ID)
	}
}
