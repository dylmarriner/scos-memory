package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGovernedToolsListsAllGovernedTools(t *testing.T) {
	tools := governedTools()
	if len(tools) != 127 {
		t.Fatalf("expected 127 governed tools, got %d", len(tools))
	}
	names := governedToolNames()
	for _, want := range []string{"scos_recall", "scos_status", "scos_explain", "scos_user_model", "scos_memory_graph", "scos_correct", "scos_forget"} {
		if !names[want] {
			t.Fatalf("expected governed tool %q to be present", want)
		}
	}
}

func TestGovernedToolSchemasUseCamelCaseInputSchema(t *testing.T) {
	for _, tool := range governedTools() {
		if _, ok := tool["inputSchema"]; !ok {
			t.Fatalf("tool %v missing MCP-spec inputSchema key", tool["name"])
		}
	}
}

func TestHandleMCPToolsList(t *testing.T) {
	s := &gatewayServer{}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	r := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleMCP(rec, r)

	var resp struct {
		Result struct {
			Tools []map[string]any `json:"tools"`
		} `json:"result"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Result.Tools) != 127 {
		t.Fatalf("expected 127 tools in tools/list response, got %d", len(resp.Result.Tools))
	}
}

func TestHandleMCPInitialize(t *testing.T) {
	s := &gatewayServer{}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "abc", "method": "initialize"})
	r := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleMCP(rec, r)

	var resp map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["id"] != "abc" {
		t.Fatalf("expected echoed id 'abc', got %v", resp["id"])
	}
	if resp["error"] != nil {
		t.Fatalf("expected no error, got %v", resp["error"])
	}
}

func TestHandleMCPUnknownMethod(t *testing.T) {
	s := &gatewayServer{}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "not/a/method"})
	r := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleMCP(rec, r)

	var resp map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected a JSON-RPC error, got %v", resp)
	}
	if int(errObj["code"].(float64)) != -32601 {
		t.Fatalf("expected method-not-found code -32601, got %v", errObj["code"])
	}
}

func TestHandleMCPToolsCallRequiresIdentity(t *testing.T) {
	// No auth middleware in front of this direct call, and no X-SCOS-*
	// headers on the request -- resolveIdentity must fail closed rather
	// than proxy an anonymous tool call upstream.
	s := &gatewayServer{}
	params, _ := json.Marshal(map[string]any{"name": "scos_status", "arguments": map[string]any{}})
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": json.RawMessage(params)})
	r := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleMCP(rec, r)

	var resp map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["error"] == nil {
		t.Fatalf("expected an identity error, got %v", resp)
	}
}

func TestHandleMCPToolsCallRejectsUnknownTool(t *testing.T) {
	s := &gatewayServer{}
	params, _ := json.Marshal(map[string]any{"name": "not_a_real_tool", "arguments": map[string]any{}})
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": json.RawMessage(params)})
	r := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	r.Header.Set("X-SCOS-Tenant-ID", "tenant-1")
	r.Header.Set("X-SCOS-Principal-ID", "principal-1")
	rec := httptest.NewRecorder()
	s.handleMCP(rec, r)

	var resp map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected an error for unknown tool, got %v", resp)
	}
	if int(errObj["code"].(float64)) != -32602 {
		t.Fatalf("expected invalid-params code -32602, got %v", errObj["code"])
	}
}

func TestHandleMCPNotificationReturnsNoContent(t *testing.T) {
	s := &gatewayServer{}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	r := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleMCP(rec, r)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204 for a notification, got %d", rec.Code)
	}
}

func TestResolveIdentityFallsBackToHeaders(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	r.Header.Set("X-SCOS-Tenant-ID", "tenant-1")
	r.Header.Set("X-SCOS-Principal-ID", "principal-1")
	identity, status, _ := resolveIdentity(r)
	if status != 0 {
		t.Fatalf("expected identity to resolve, got status %d", status)
	}
	if identity.TenantID != "tenant-1" || identity.PrincipalID != "principal-1" {
		t.Fatalf("unexpected identity: %+v", identity)
	}
}

func TestResolveIdentityRequiresBothHeaders(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	r.Header.Set("X-SCOS-Tenant-ID", "tenant-1")
	if _, status, _ := resolveIdentity(r); status != http.StatusUnauthorized {
		t.Fatalf("expected 401 when principal id is missing, got %d", status)
	}
}

func TestDeriveSessionIDIsDeterministicAndValidUUIDShape(t *testing.T) {
	a := deriveSessionID("tenant-1", "principal-1")
	b := deriveSessionID("tenant-1", "principal-1")
	if a != b {
		t.Fatalf("expected deterministic session id, got %q vs %q", a, b)
	}
	if !isUUIDShape(a) {
		t.Fatalf("expected UUID-shaped session id, got %q", a)
	}
	c := deriveSessionID("tenant-1", "principal-2")
	if a == c {
		t.Fatalf("expected different principals to derive different session ids")
	}
}

func isUUIDShape(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
				return false
			}
		}
	}
	return true
}
