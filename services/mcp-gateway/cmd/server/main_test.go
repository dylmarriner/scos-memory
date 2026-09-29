package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

func TestGovernedToolsListsAllGovernedTools(t *testing.T) {
	tools := governedTools()
	if len(tools) != 149 {
		t.Fatalf("expected 149 governed tools, got %d", len(tools))
	}
	names := governedToolNames()
	for _, want := range []string{"scos_recall", "scos_status", "scos_explain", "scos_user_model", "scos_memory_graph", "scos_correct", "scos_forget", "scos_graphify_query", "scos_graphify_path", "scos_graphify_explain"} {
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
	if len(resp.Result.Tools) != 149 {
		t.Fatalf("expected 149 tools in tools/list response, got %d", len(resp.Result.Tools))
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

// This test previously asserted that resolveIdentity would fall back to
// caller-supplied X-SCOS-* headers when no claims were present -- it
// codified the tenant-spoofing path rather than guarding against it. The
// assertion is inverted here deliberately: at this boundary, headers alone
// must never produce an identity.
func TestResolveIdentityRejectsHeaderAssertedIdentity(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	r.Header.Set("X-SCOS-Tenant-ID", "tenant-1")
	r.Header.Set("X-SCOS-Principal-ID", "principal-1")
	if _, status, _ := resolveIdentity(r); status != http.StatusUnauthorized {
		t.Fatalf("expected 401 for header-asserted identity with no claims, got %d", status)
	}
}

func TestResolveIdentityUsesClaims(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	// An attacker-supplied header alongside a legitimate token must not
	// influence the resolved identity.
	r.Header.Set("X-SCOS-Tenant-ID", "attacker-tenant")
	r.Header.Set("X-SCOS-Principal-ID", "attacker-principal")
	ctx := auth.ContextWithClaims(r.Context(), auth.Claims{TenantID: "tenant-1", PrincipalID: "principal-1"})
	identity, status, _ := resolveIdentity(r.WithContext(ctx))
	if status != 0 {
		t.Fatalf("expected claims-bearing request to resolve, got status %d", status)
	}
	if identity.TenantID != "tenant-1" || identity.PrincipalID != "principal-1" {
		t.Fatalf("identity must come from claims, not headers: %+v", identity)
	}
}

func TestResolveIdentityRequiresClaims(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	r.Header.Set("X-SCOS-Tenant-ID", "tenant-1")
	if _, status, _ := resolveIdentity(r); status != http.StatusUnauthorized {
		t.Fatalf("expected 401 without claims, got %d", status)
	}
}

func TestResolveIdentityLocalAuthModeIgnoresClaimsAndHeaders(t *testing.T) {
	t.Setenv("SCOS_AUTH_MODE", "local")
	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	// Neither a bearer token nor a caller-asserted header should matter in
	// local mode -- every caller resolves to the same reserved identity.
	r.Header.Set("X-SCOS-Tenant-ID", "attacker-tenant")
	r.Header.Set("X-SCOS-Principal-ID", "attacker-principal")
	identity, status, _ := resolveIdentity(r)
	if status != 0 {
		t.Fatalf("expected local mode to resolve without claims, got status %d", status)
	}
	if identity.TenantID != localModeTenantID || identity.PrincipalID != localModePrincipalID {
		t.Fatalf("expected reserved local-mode identity, got %+v", identity)
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

func TestDispatchCodeRegisterReturnsLegacyAndDurableIDs(t *testing.T) {
	var legacyCalled, durableCalled bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-SCOS-Tenant-ID") != "tenant-1" || r.Header.Get("X-SCOS-Principal-ID") != "principal-1" {
			t.Fatalf("identity not forwarded")
		}
		switch r.URL.Path {
		case "/v1/repos":
			legacyCalled = true
			_ = json.NewEncoder(w).Encode(map[string]any{"repository_id": "repo-7", "canonical_path": "/work/repo"})
		case "/v2/code/repos":
			durableCalled = true
			_ = json.NewEncoder(w).Encode(map[string]any{"repository_id": "11111111-1111-1111-1111-111111111111"})
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer upstream.Close()
	s := &gatewayServer{codeIntelligenceURL: upstream.URL, client: upstream.Client()}
	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	got, err := s.dispatchTool(r.Context(), r, requestIdentity{TenantID: "tenant-1", PrincipalID: "principal-1"}, "scos_code_repo_register", map[string]any{"canonical_path": "/work/repo"})
	if err != nil {
		t.Fatal(err)
	}
	m, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("result type %T", got)
	}
	if m["repository_id"] != "repo-7" || m["durable_repository_id"] != "11111111-1111-1111-1111-111111111111" || !legacyCalled || !durableCalled {
		t.Fatalf("unexpected result %#v", m)
	}
}

func TestDispatchCodeQueryUsesDurableV2WhenDurableIDProvided(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/code/query" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["repository_id"] != "11111111-1111-1111-1111-111111111111" || body["branch"] != "main" || body["query"] != "auth middleware" {
			t.Fatalf("body %#v", body)
		}
		if _, ok := body["intent"]; ok {
			t.Fatalf("v1-only field leaked into v2 body: %#v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"retrieval_mode": "hybrid", "results": []any{}})
	}))
	defer upstream.Close()
	s := &gatewayServer{codeIntelligenceURL: upstream.URL, client: upstream.Client()}
	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	_, err := s.dispatchTool(r.Context(), r, requestIdentity{TenantID: "tenant-1", PrincipalID: "principal-1"}, "scos_code_query", map[string]any{"intent": "search", "query": "auth middleware", "durable_repository_id": "11111111-1111-1111-1111-111111111111", "branch": "main", "max_results": 12})
	if err != nil {
		t.Fatal(err)
	}
}

func TestDispatchCodeQueryKeepsLegacyV1Fallback(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/query" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": []any{}})
	}))
	defer upstream.Close()
	s := &gatewayServer{codeIntelligenceURL: upstream.URL, client: upstream.Client()}
	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	_, err := s.dispatchTool(r.Context(), r, requestIdentity{TenantID: "tenant-1", PrincipalID: "principal-1"}, "scos_code_query", map[string]any{"intent": "search", "query": "auth middleware", "repository_id": "repo-7"})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSCOSRetrieveSchemaExposesDurableRepositoryContext(t *testing.T) {
	for _, tool := range governedTools() {
		if tool["name"] != "scos_retrieve" {
			continue
		}
		schema := tool["inputSchema"].(map[string]any)
		props := schema["properties"].(map[string]any)
		if _, ok := props["repository_id"]; !ok {
			t.Fatal("scos_retrieve missing repository_id")
		}
		if _, ok := props["branch"]; !ok {
			t.Fatal("scos_retrieve missing branch")
		}
		return
	}
	t.Fatal("scos_retrieve not found")
}
