package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

func TestGovernedToolsListsAllGovernedTools(t *testing.T) {
	tools := governedTools()
	if len(tools) != 163 {
		t.Fatalf("expected 163 governed tools, got %d", len(tools))
	}
	names := governedToolNames()
	for _, want := range []string{"scos_recall", "scos_status", "scos_explain", "scos_user_model", "scos_memory_graph", "scos_remember", "scos_correct", "scos_forget", "scos_graphify_query", "scos_graphify_path", "scos_graphify_explain", "scos_code_revalidations", "scos_code_snapshot_events", "scos_code_snapshot_manifest", "scos_memory_code_links", "scos_code_memory_links"} {
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
	if len(resp.Result.Tools) != 163 {
		t.Fatalf("expected 163 tools in tools/list response, got %d", len(resp.Result.Tools))
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

// A hosted tenant reaches a local-mode gateway through cloud-control-plane,
// which signs claims for that tenant: the claims name the caller, so the
// tenant never lands in the local-mode tenant.
func TestLocalAuthModeHonoursVerifiedClaims(t *testing.T) {
	t.Setenv("SCOS_AUTH_MODE", "local")
	const secret = "local-mode-test-issuer-secret"
	// Local mode verifies tokens without the control-plane token it never
	// checks.
	t.Setenv("SCOS_CONTROL_PLANE_TOKEN", "")
	t.Setenv("SCOS_SECURITY_ISSUER_SECRET", secret)
	t.Setenv("SCOS_INTERNAL_SERVICE_TOKEN", "internal-service-token")
	config := localModeAuthConfig()
	var got requestIdentity
	var gotStatus int
	handler := localModeHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, gotStatus, _ = resolveIdentity(r)
	}), config)
	call := func(authorization string) int {
		got, gotStatus = requestIdentity{}, -1
		r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		if authorization != "" {
			r.Header.Set("Authorization", authorization)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}

	token, err := auth.SignClaims(auth.Claims{TenantID: "hosted-tenant", PrincipalID: "hosted-principal", ExpiresAt: time.Now().Add(time.Minute)}, secret)
	if err != nil {
		t.Fatal(err)
	}
	if code := call("Bearer " + token); code != http.StatusOK || gotStatus != 0 || got.TenantID != "hosted-tenant" || got.PrincipalID != "hosted-principal" {
		t.Fatalf("claims caller resolved to %+v (status %d, http %d), want its own tenant", got, gotStatus, code)
	}
	if code := call(""); code != http.StatusOK || got.TenantID != localModeTenantID {
		t.Fatalf("token-less local caller resolved to %+v (http %d), want the local-mode tenant", got, code)
	}
	if code := call("Bearer internal-service-token"); code != http.StatusOK || got.TenantID != localModeTenantID {
		t.Fatalf("service-token caller resolved to %+v (http %d), want the local-mode tenant", got, code)
	}
	forged, err := auth.SignClaims(auth.Claims{TenantID: "hosted-tenant", PrincipalID: "hosted-principal", ExpiresAt: time.Now().Add(time.Minute)}, "another-secret")
	if err != nil {
		t.Fatal(err)
	}
	if code := call("Bearer " + forged); code != http.StatusUnauthorized || gotStatus != -1 {
		t.Fatalf("forged token got http %d, want 401 before any identity is resolved", code)
	}
}

// TestRecallForwardsOnlyACallerNamedSession: a session_id the caller names
// reaches hermes-provider (which scopes the session lane to it); with none
// named, no session is invented; a malformed one is refused.
func TestRecallForwardsOnlyACallerNamedSession(t *testing.T) {
	var got map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/provider/tool_call" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		got = nil
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(map[string]any{"recall": []any{}})
	}))
	defer upstream.Close()
	s := &gatewayServer{hermesURL: upstream.URL, client: upstream.Client()}
	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	identity := requestIdentity{TenantID: "tenant-1", PrincipalID: "principal-1"}
	const session = "8b2f0c8e-1d7a-4c56-9a0b-2f6e8d1c3b4a"
	if _, err := s.dispatchTool(r.Context(), r, identity, "scos_recall", map[string]any{"query": "deploy target", "session_id": session}); err != nil {
		t.Fatal(err)
	}
	if got["session_id"] != session {
		t.Fatalf("named session not forwarded: %v", got)
	}
	if _, err := s.dispatchTool(r.Context(), r, identity, "scos_recall", map[string]any{"query": "deploy target"}); err != nil {
		t.Fatal(err)
	}
	if _, has := got["session_id"]; has {
		t.Fatalf("a session was invented for a recall that named none: %v", got)
	}
	got = nil
	if _, err := s.dispatchTool(r.Context(), r, identity, "scos_recall", map[string]any{"query": "q", "session_id": "not-a-uuid"}); err == nil {
		t.Fatal("a malformed session_id must be refused")
	}
	if got != nil {
		t.Fatal("a refused recall reached hermes-provider")
	}
}

// TestAuthIssueIsNotAGatewayTool: token issuance needs the issuer secret,
// which must never travel as an agent tool argument.
func TestAuthIssueIsNotAGatewayTool(t *testing.T) {
	if governedToolNames()["scos_auth_issue"] {
		t.Fatal("scos_auth_issue must not be offered")
	}
	s := &gatewayServer{}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "scos_auth_issue", "arguments": map[string]any{}}})
	rec := httptest.NewRecorder()
	s.handleMCP(rec, httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body)))
	if !strings.Contains(rec.Body.String(), "unknown tool") {
		t.Fatalf("scos_auth_issue call = %s", rec.Body.String())
	}
}

// TestBenchmarksReportsRecordedRuns: the tool reads recorded history (with
// its suite and limit filters) and says it does not run benchmarks.
func TestBenchmarksReportsRecordedRuns(t *testing.T) {
	var query string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/benchmarks" {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		query = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{"history": []any{}})
	}))
	defer upstream.Close()
	s := &gatewayServer{observabilityURL: upstream.URL, client: upstream.Client()}
	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	if _, err := s.dispatchTool(r.Context(), r, requestIdentity{TenantID: "t", PrincipalID: "p"}, "scos_benchmarks", map[string]any{"suite": "deployed", "limit": float64(5)}); err != nil {
		t.Fatal(err)
	}
	if query != "limit=5&suite=deployed" {
		t.Fatalf("query = %q", query)
	}
	for _, tool := range governedTools() {
		if tool["name"] == "scos_benchmarks" {
			desc := tool["description"].(string)
			if strings.Contains(desc, "Run and report") || !strings.Contains(desc, "does not run benchmarks") {
				t.Fatalf("scos_benchmarks description = %q", desc)
			}
		}
	}
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

func TestDispatchCodeQueryResolvesLegacyIDToDurableRepository(t *testing.T) {
	const durable = "22222222-2222-2222-2222-222222222222"
	var calls []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/v1/repos/repo-7":
			_ = json.NewEncoder(w).Encode(map[string]any{"repository_id": "repo-7", "canonical_path": "/work/repo"})
		case "/v2/code/repos":
			_ = json.NewEncoder(w).Encode(map[string]any{"repository_id": durable})
		case "/v2/code/query":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["repository_id"] != durable || body["query"] != "auth middleware" {
				t.Fatalf("query body %#v", body)
			}
			if _, has := body["branch"]; has {
				t.Fatalf("an unset branch must be left to code-intelligence: %#v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"results": []any{}})
		default:
			t.Fatalf("unexpected %s", r.URL.Path)
		}
	}))
	defer upstream.Close()
	s := &gatewayServer{codeIntelligenceURL: upstream.URL, client: upstream.Client()}
	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	if _, err := s.dispatchTool(r.Context(), r, requestIdentity{TenantID: "tenant-1", PrincipalID: "principal-1"}, "scos_code_query", map[string]any{"query": "auth middleware", "repository_id": "repo-7"}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(calls, ",") != "GET /v1/repos/repo-7,POST /v2/code/repos,POST /v2/code/query" {
		t.Fatalf("calls = %v", calls)
	}
}

func TestDispatchCodeIndexScansTheWorkspaceWhenNoFilesAreUploaded(t *testing.T) {
	const durable = "33333333-3333-3333-3333-333333333333"
	var path string
	var body map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(map[string]any{"snapshot_id": "s1"})
	}))
	defer upstream.Close()
	s := &gatewayServer{codeIntelligenceURL: upstream.URL, client: upstream.Client()}
	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	if _, err := s.dispatchTool(r.Context(), r, requestIdentity{TenantID: "t", PrincipalID: "p"}, "scos_code_index", map[string]any{"repository_id": durable}); err != nil {
		t.Fatal(err)
	}
	if path != "/v2/code/scan" || body["repository_id"] != durable || len(body) != 1 {
		t.Fatalf("scan request = %s %v", path, body)
	}
	if _, err := s.dispatchTool(r.Context(), r, requestIdentity{TenantID: "t", PrincipalID: "p"}, "scos_code_index", map[string]any{"repository_id": durable, "files": []any{map[string]any{"path": "a.go", "content_hash": "x"}}}); err == nil {
		t.Fatal("uploading files without branch and commit must be refused")
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
		if _, ok := props["as_of_valid_time"]; !ok {
			t.Fatal("scos_retrieve missing valid-time selector")
		}
		for _, selector := range []string{"as_of_commit", "snapshot_id"} {
			if _, ok := props[selector]; !ok {
				t.Fatalf("scos_retrieve missing immutable code selector %s", selector)
			}
		}
		return
	}
	t.Fatal("scos_retrieve not found")
}

func TestDispatchRememberSavesAnExplicitFact(t *testing.T) {
	var got map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/facts" {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(map[string]any{"fact": map[string]any{"id": "fact-1"}})
	}))
	defer upstream.Close()
	s := &gatewayServer{memoryIntelligenceURL: upstream.URL, client: upstream.Client()}
	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	identity := requestIdentity{TenantID: "tenant-1", PrincipalID: "principal-1"}
	if _, err := s.dispatchTool(r.Context(), r, identity, "scos_remember", map[string]any{"content": "  The API uses PostgreSQL 17.  ", "network": "world"}); err != nil {
		t.Fatal(err)
	}
	if got["content"] != "The API uses PostgreSQL 17." || got["source"] != "explicit" || got["network"] != "world" {
		t.Fatalf("forwarded fact = %v", got)
	}
	if _, has := got["namespace_id"]; has {
		t.Fatalf("an unset namespace must be resolved by memory-intelligence, not sent empty: %v", got)
	}
	if _, err := s.dispatchTool(r.Context(), r, identity, "scos_remember", map[string]any{"content": "  "}); err == nil {
		t.Fatal("empty content must be rejected before forwarding")
	}
}

func TestUpstreamErrorsNameTheServiceThatFailed(t *testing.T) {
	if got := upstreamName("https://backup-manager:8095/v1/backups/start"); got != "backup-manager" {
		t.Fatalf("upstreamName = %q", got)
	}
}

func TestDispatchControlConfirmForwardsThePrincipalsAnswer(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/control/confirm" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["interaction_id"] != "i-1" || body["decision_id"] != "d-1" || body["resolution"] != "confirmed" {
			t.Fatalf("body %#v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed"})
	}))
	defer upstream.Close()
	s := &gatewayServer{controlPlaneURL: upstream.URL, client: upstream.Client()}
	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	got, err := s.dispatchTool(r.Context(), r, requestIdentity{TenantID: "t", PrincipalID: "p"}, "scos_control_confirm", map[string]any{"interaction_id": "i-1", "decision_id": "d-1", "resolution": "confirmed"})
	if err != nil {
		t.Fatal(err)
	}
	if got.(map[string]any)["status"] != "completed" {
		t.Fatalf("result %#v", got)
	}
}

func TestDispatchOperationStartAndPrepareRefuseSelfAssertedConfirmation(t *testing.T) {
	var paths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		_ = json.NewEncoder(w).Encode(map[string]any{"operation_id": "op-1"})
	}))
	defer upstream.Close()
	s := &gatewayServer{toolGovernanceURL: upstream.URL, client: upstream.Client()}
	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	id := requestIdentity{TenantID: "t", PrincipalID: "p"}
	for _, name := range []string{"scos_operation_start", "scos_operation_prepare"} {
		_, err := s.dispatchTool(r.Context(), r, id, name, map[string]any{"tool_name": "execution-proxy", "risk": "destructive", "confirmed": true})
		if err == nil || !strings.Contains(err.Error(), "scos_control_confirm") {
			t.Fatalf("%s: self-asserted confirmation was not refused: %v", name, err)
		}
		if _, err := s.dispatchTool(r.Context(), r, id, name, map[string]any{"tool_name": "execution-proxy", "risk": "read"}); err != nil {
			t.Fatalf("%s: unconfirmed operation was not forwarded: %v", name, err)
		}
	}
	if len(paths) != 2 || paths[0] != "/v1/operations/start" || paths[1] != "/v1/operations/prepare" {
		t.Fatalf("governance calls = %v", paths)
	}
	for _, tool := range governedTools() {
		if tool["name"] != "scos_operation_start" && tool["name"] != "scos_operation_prepare" {
			continue
		}
		if _, ok := tool["inputSchema"].(map[string]any)["properties"].(map[string]any)["confirmed"]; ok {
			t.Fatalf("%s still advertises a confirmed argument", tool["name"])
		}
	}
}

func TestDispatchExecExecuteRefusesSelfAssertedConfirmation(t *testing.T) {
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
	}))
	defer upstream.Close()
	s := &gatewayServer{executionProxyURL: upstream.URL, client: upstream.Client()}
	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	_, err := s.dispatchTool(r.Context(), r, requestIdentity{TenantID: "t", PrincipalID: "p"}, "scos_exec_execute", map[string]any{"command": "rm -rf /srv/build", "confirmed": true})
	if err == nil || !strings.Contains(err.Error(), "scos_control_confirm") {
		t.Fatalf("self-asserted confirmation was not refused: %v", err)
	}
	if called {
		t.Fatal("the execution proxy was called with a self-asserted confirmation")
	}
	if _, err := s.dispatchTool(r.Context(), r, requestIdentity{TenantID: "t", PrincipalID: "p"}, "scos_exec_execute", map[string]any{"command": "ls"}); err != nil || !called {
		t.Fatalf("an unconfirmed command was not forwarded: %v", err)
	}
	for _, tool := range governedTools() {
		if tool["name"] != "scos_exec_execute" {
			continue
		}
		properties := tool["inputSchema"].(map[string]any)["properties"].(map[string]any)
		if _, ok := properties["confirmed"]; ok {
			t.Fatal("scos_exec_execute still advertises a confirmed argument")
		}
	}
}

// TestCodeRevalidationToolForwardsOnlyTheVerifiedIdentity ensures this new
// read-only tool cannot use caller-injected headers to widen review scope.
func TestCodeRevalidationToolForwardsOnlyTheVerifiedIdentity(t *testing.T) {
	var gotPath, gotTenant, gotPrincipal string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		gotTenant = r.Header.Get("X-SCOS-Tenant-ID")
		gotPrincipal = r.Header.Get("X-SCOS-Principal-ID")
		if r.Method != http.MethodGet {
			t.Errorf("upstream method %s, want GET", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	}))
	defer upstream.Close()
	s := &gatewayServer{codeIntelligenceURL: upstream.URL, client: upstream.Client()}
	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	r.Header.Set("X-SCOS-Tenant-ID", "forged-tenant")
	r.Header.Set("X-SCOS-Principal-ID", "forged-principal")
	got, err := s.dispatchTool(r.Context(), r,
		requestIdentity{TenantID: "real-tenant", PrincipalID: "real-principal"},
		"scos_code_revalidations",
		map[string]any{"repository_id": "11111111-1111-1111-1111-111111111111", "limit": float64(3), "pending_only": true})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.([]any); !ok {
		t.Fatalf("unexpected event payload %T: %v", got, got)
	}
	if gotPath != "/v2/code/revalidations?limit=3&pending_only=true&repository_id=11111111-1111-1111-1111-111111111111" {
		t.Errorf("unexpected forwarded route %q", gotPath)
	}
	if gotTenant != "real-tenant" || gotPrincipal != "real-principal" {
		t.Fatalf("identity spoofing: forwarded tenant=%q principal=%q", gotTenant, gotPrincipal)
	}
	for _, bad := range []any{"true", float64(1), nil} {
		if _, err := s.dispatchTool(r.Context(), r,
			requestIdentity{TenantID: "real-tenant", PrincipalID: "real-principal"},
			"scos_code_revalidations",
			map[string]any{"repository_id": "11111111-1111-1111-1111-111111111111", "pending_only": bad}); err == nil {
			t.Errorf("invalid pending_only value %v accepted", bad)
		}
	}
	for _, input := range []any{float64(0), float64(101), float64(1.5), "100"} {
		if _, err := s.dispatchTool(r.Context(), r,
			requestIdentity{TenantID: "real-tenant", PrincipalID: "real-principal"},
			"scos_code_revalidations", map[string]any{
				"repository_id": "11111111-1111-1111-1111-111111111111", "limit": input,
			}); err == nil {
			t.Errorf("invalid limit %v was accepted", input)
		}
	}
}
