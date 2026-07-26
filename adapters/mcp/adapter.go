package mcpadapter

import (
	"context"
	"encoding/json"
	"fmt"
)

// MCPRequest defines Model Context Protocol request envelope (§23).
type MCPRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      string          `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// MCPResponse defines Model Context Protocol response envelope.
type MCPResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      string      `json:"id"`
	Result  interface{} `json:"result,omitempty"`
	Error   interface{} `json:"error,omitempty"`
}

// Handler handles incoming MCP tool calls.
type Handler struct{}

// HandleCall processes an incoming MCP tool invocation.
func (h *Handler) HandleCall(ctx context.Context, req *MCPRequest) (*MCPResponse, error) {
	if req.Method == "" {
		return nil, fmt.Errorf("method is required")
	}
	return &MCPResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result:  map[string]string{"status": "handled", "method": req.Method},
	}, nil
}
