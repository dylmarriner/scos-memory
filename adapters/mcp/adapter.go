package mcpadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
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
type Handler struct {
	EndpointURL string
	HTTPClient  *http.Client
}

// HandleCall processes an incoming MCP tool invocation.
func (h *Handler) HandleCall(ctx context.Context, req *MCPRequest) (*MCPResponse, error) {
	if req == nil || req.Method == "" {
		return nil, fmt.Errorf("method is required")
	}
	if req.JSONRPC != "" && req.JSONRPC != "2.0" {
		return nil, fmt.Errorf("JSON-RPC version must be 2.0")
	}
	if req.Method == "tools/list" {
		return response(req.ID, map[string]any{"tools": governedTools()}), nil
	}
	if req.Method != "tools/call" {
		return nil, fmt.Errorf("unsupported MCP method %q", req.Method)
	}
	var call struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &call); err != nil || strings.TrimSpace(call.Name) == "" {
		return nil, fmt.Errorf("tools/call requires params.name and valid JSON arguments")
	}
	if strings.TrimSpace(h.EndpointURL) == "" {
		return nil, fmt.Errorf("MCP control-plane endpoint is not configured")
	}
	payload := map[string]any{"input": call.Arguments["input"], "interaction_id": call.Arguments["interaction_id"], "tenant_id": call.Arguments["tenant_id"], "actor_id": call.Arguments["actor_id"], "agent_id": call.Arguments["agent_id"], "session_id": call.Arguments["session_id"], "turn_id": call.Arguments["turn_id"], "proposed_tool": call.Arguments["proposed_tool"]}
	if payload["input"] == nil {
		payload["input"] = call.Name
	}
	if payload["interaction_id"] == nil {
		payload["interaction_id"] = req.ID
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	httpClient := h.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(h.EndpointURL, "/")+"/v1/control/ingest", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	for key, value := range map[string]any{"X-SCOS-Tenant-ID": payload["tenant_id"], "X-SCOS-Principal-ID": payload["actor_id"]} {
		if text, ok := value.(string); ok && text != "" {
			httpReq.Header.Set(key, text)
		}
	}
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("invoke control plane: %w", err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("control plane returned %s: %s", resp.Status, strings.TrimSpace(string(responseBody)))
	}
	var result any
	if err := json.Unmarshal(responseBody, &result); err != nil {
		return nil, fmt.Errorf("decode control-plane response: %w", err)
	}
	return response(req.ID, result), nil
}

func response(id string, result any) *MCPResponse {
	return &MCPResponse{JSONRPC: "2.0", ID: id, Result: result}
}

func governedTools() []map[string]any {
	return []map[string]any{{"name": "scos.route", "description": "Route an interaction through the SCOS Autonomous Cognitive Control Plane", "inputSchema": map[string]any{"type": "object"}}}
}
