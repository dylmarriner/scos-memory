package main

import "github.com/dylmarriner/scos-memory/packages/auth"
import "github.com/dylmarriner/scos-memory/packages/tlsserve"

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// mcp-gateway is the only service in this fleet that speaks native MCP
// (JSON-RPC 2.0) over HTTP. It exists so external agents that only support
// remote MCP connectors (rather than a local stdio bridge) have a single,
// correctly-scoped endpoint to reach the governed §24.5 tool set.
//
// It does not own any data itself. Every tool call is forwarded, with the
// caller's own identity headers, to the service that actually owns that
// tool's implementation:
//   - scos_status, scos_recall  -> hermes-provider  (already implemented)
//   - scos_explain              -> memory-intelligence /v1/facts/{id}
//   - scos_user_model           -> memory-intelligence /v1/dashboard/user-model
//   - scos_memory_graph         -> memory-intelligence /v1/dashboard/memory-graph
//   - scos_correct, scos_forget -> not implemented anywhere in this fleet yet
//     (memory-intelligence exposes no write/delete path for facts). This
//     handler reports that honestly rather than fabricating success (§42).
const version = "0.1.0"

type requestIdentity struct {
	TenantID    string
	PrincipalID string
}

type gatewayServer struct {
	hermesURL             string
	memoryIntelligenceURL string
	client                *http.Client
	startTime             time.Time
}

func resolveUpstream(envVar, fallback string) string {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv(envVar)), "/")
	if base == "" {
		base = fallback
	}
	return base
}

func main() {
	client, err := tlsserve.TrustedClient(15 * time.Second)
	if err != nil {
		log.Fatalf("configure internal TLS client: %v", err)
	}
	s := &gatewayServer{
		hermesURL:             resolveUpstream("SCOS_HERMES_PROVIDER_URL", "https://hermes-provider:8088"),
		memoryIntelligenceURL: resolveUpstream("SCOS_MEMORY_INTELLIGENCE_URL", "https://memory-intelligence:8089"),
		client:                client,
		startTime:             time.Now(),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", healthOK)
	mux.HandleFunc("/health/ready", s.handleReady)
	mux.HandleFunc("/mcp", s.handleMCP)

	addr := os.Getenv("SCOS_MCP_GATEWAY_HTTP_ADDR")
	if addr == "" {
		addr = ":8098"
	}
	log.Printf("mcp-gateway (%s) listening on %s, hermes=%s memory-intelligence=%s",
		version, addr, s.hermesURL, s.memoryIntelligenceURL)
	if err := tlsserve.ListenAndServe(addr, auth.AuthenticatedHandler(mux, auth.MustFromEnvironment())); err != nil {
		log.Fatal(err)
	}
}

func healthOK(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (s *gatewayServer) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.hermesURL+"/health/ready", nil)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unready", "error": err.Error()})
		return
	}
	resp, err := s.client.Do(req)
	if err != nil || resp.StatusCode/100 != 2 {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unready", "error": "hermes-provider unavailable"})
		return
	}
	defer resp.Body.Close()
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// ── JSON-RPC / MCP protocol ─────────────────────────────────────────────

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (s *gatewayServer) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeRPCError(w, nil, -32700, "parse error: "+err.Error())
		return
	}

	if strings.HasPrefix(req.Method, "notifications/") {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	switch req.Method {
	case "initialize":
		writeRPCResult(w, req.ID, map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "scos-mcp-gateway", "version": version},
		})
	case "ping":
		writeRPCResult(w, req.ID, map[string]any{})
	case "tools/list":
		writeRPCResult(w, req.ID, map[string]any{"tools": governedTools()})
	case "tools/call":
		s.handleToolsCall(w, r, req)
	default:
		writeRPCError(w, req.ID, -32601, "unknown method: "+req.Method)
	}
}

func (s *gatewayServer) handleToolsCall(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	var call struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &call); err != nil || strings.TrimSpace(call.Name) == "" {
		writeRPCError(w, req.ID, -32602, "tools/call requires params.name")
		return
	}
	if !governedToolNames()[call.Name] {
		writeRPCError(w, req.ID, -32602, "unknown tool: "+call.Name)
		return
	}

	identity, status, msg := resolveIdentity(r)
	if status != 0 {
		writeRPCError(w, req.ID, -32001, msg)
		return
	}

	result, err := s.dispatchTool(r.Context(), r, identity, call.Name, call.Arguments)
	if err != nil {
		writeRPCError(w, req.ID, -32000, err.Error())
		return
	}
	encoded, _ := json.Marshal(result)
	writeRPCResult(w, req.ID, map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(encoded)}},
	})
}

func (s *gatewayServer) dispatchTool(ctx context.Context, r *http.Request, identity requestIdentity, tool string, args map[string]any) (any, error) {
	switch tool {
	case "scos_status", "scos_recall":
		return s.callHermesProvider(ctx, r, identity, tool, args)
	case "scos_explain":
		memoryID, _ := args["memory_id"].(string)
		return s.forwardGET(ctx, r, s.memoryIntelligenceURL+"/v1/facts/"+memoryID)
	case "scos_user_model":
		return s.forwardGET(ctx, r, s.memoryIntelligenceURL+"/v1/dashboard/user-model")
	case "scos_memory_graph":
		return s.forwardGET(ctx, r, s.memoryIntelligenceURL+"/v1/dashboard/memory-graph")
	case "scos_correct", "scos_forget":
		// §42: report the boundary rather than fabricate a local answer.
		// memory-intelligence exposes GET /v1/facts/{id} only -- there is no
		// write or delete path for facts anywhere in this fleet yet.
		return map[string]any{
			"tool":        tool,
			"implemented": false,
			"reason":      "no write/delete endpoint exists for facts in memory-intelligence yet",
		}, nil
	}
	return nil, errUnreachable("unhandled tool: " + tool)
}

func (s *gatewayServer) callHermesProvider(ctx context.Context, r *http.Request, identity requestIdentity, tool string, args map[string]any) (any, error) {
	body := map[string]any{
		"tenant_id":    identity.TenantID,
		"principal_id": identity.PrincipalID,
		"tool":         tool,
		"arguments":    args,
	}
	if tool == "scos_recall" {
		sessionID, _ := args["session_id"].(string)
		if sessionID == "" {
			sessionID = deriveSessionID(identity.TenantID, identity.PrincipalID)
		}
		body["session_id"] = sessionID
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.hermesURL+"/v1/provider/tool_call", strings.NewReader(string(encoded)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	copyIdentityHeaders(r, req)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, errUnreachable("hermes-provider unavailable: " + err.Error())
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, errUnreachable("hermes-provider returned " + resp.Status + ": " + strings.TrimSpace(string(respBody)))
	}
	var result any
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *gatewayServer) forwardGET(ctx context.Context, r *http.Request, url string) (any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	copyIdentityHeaders(r, req)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, errUnreachable("memory-intelligence unavailable: " + err.Error())
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, errUnreachable("memory-intelligence returned " + resp.Status + ": " + strings.TrimSpace(string(body)))
	}
	var result any
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	return result, nil
}

type errUnreachable string

func (e errUnreachable) Error() string { return string(e) }

func copyIdentityHeaders(src *http.Request, dst *http.Request) {
	for _, name := range []string{"Authorization", "X-SCOS-Tenant-ID", "X-SCOS-Principal-ID"} {
		if value := src.Header.Get(name); value != "" {
			dst.Header.Set(name, value)
		}
	}
}

// resolveIdentity mirrors the convention used by hermes-provider and
// memory-intelligence: an authenticated claims-bearing bearer token takes
// precedence, falling back to the internal X-SCOS-* headers.
func resolveIdentity(r *http.Request) (requestIdentity, int, string) {
	if claims, ok := auth.CanonicalClaims(r.Context()); ok {
		return requestIdentity{TenantID: claims.TenantID, PrincipalID: claims.PrincipalID}, 0, ""
	}
	tenantID := strings.TrimSpace(r.Header.Get("X-SCOS-Tenant-ID"))
	principalID := strings.TrimSpace(r.Header.Get("X-SCOS-Principal-ID"))
	if tenantID == "" || principalID == "" {
		return requestIdentity{}, http.StatusUnauthorized, "X-SCOS-Tenant-ID and X-SCOS-Principal-ID are required (or an authenticated bearer token carrying tenant/principal claims)"
	}
	return requestIdentity{TenantID: tenantID, PrincipalID: principalID}, 0, ""
}

// deriveSessionID gives scos_recall a stable UUID-shaped session bucket per
// (tenant, principal) when the caller supplies none -- MCP tool calls have
// no notion of a session the way the hermes stdio bridge's per-agent name
// does. Deterministic, not random: repeat calls from the same identity land
// in the same bucket.
func deriveSessionID(tenantID, principalID string) string {
	sum := sha256.Sum256([]byte("scos-mcp-gateway|" + tenantID + "|" + principalID))
	hexed := hex.EncodeToString(sum[:16])
	return hexed[0:8] + "-" + hexed[8:12] + "-" + hexed[12:16] + "-" + hexed[16:20] + "-" + hexed[20:32]
}

func writeRPCResult(w http.ResponseWriter, id any, result any) {
	writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func writeRPCError(w http.ResponseWriter, id any, code int, message string) {
	writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": id, "error": rpcError{Code: code, Message: message}})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// ── governed tool catalogue (§24.5) ──────────────────────────────────────

func governedToolNames() map[string]bool {
	names := make(map[string]bool)
	for _, t := range governedTools() {
		names[t["name"].(string)] = true
	}
	return names
}

func governedTools() []map[string]any {
	return []map[string]any{
		{
			"name":        "scos_recall",
			"description": "Search SCOS Nexus memory for relevant context",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query":     map[string]any{"type": "string", "description": "Search query"},
					"max_items": map[string]any{"type": "integer", "description": "Max results"},
				},
				"required": []string{"query"},
			},
		},
		{
			"name":        "scos_status",
			"description": "Check SCOS Nexus memory provider status",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name":        "scos_explain",
			"description": "Explain why a memory is held and what evidence supports it",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"memory_id": map[string]any{"type": "string", "description": "Memory ID to explain"}},
				"required":   []string{"memory_id"},
			},
		},
		{
			"name":        "scos_user_model",
			"description": "Inspect the stored user representation",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name":        "scos_memory_graph",
			"description": "Inspect memory entities and their relationships",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name":        "scos_correct",
			"description": "Correct a stored memory, preserving the superseded version",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"memory_id": map[string]any{"type": "string", "description": "Memory ID to correct"},
					"value":     map[string]any{"type": "string", "description": "Corrected value"},
				},
				"required": []string{"memory_id", "value"},
			},
		},
		{
			"name":        "scos_forget",
			"description": "Request removal of a specific memory",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"memory_id": map[string]any{"type": "string", "description": "Memory ID to forget"}},
				"required":   []string{"memory_id"},
			},
		},
	}
}
