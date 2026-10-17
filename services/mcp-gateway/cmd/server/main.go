package main

import "github.com/dylmarriner/scos-memory/packages/telemetry"

import "github.com/dylmarriner/scos-memory/packages/release"

import "github.com/dylmarriner/scos-memory/packages/auth"
import "github.com/dylmarriner/scos-memory/packages/tlsserve"

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// mcp-gateway is the only service in this fleet that speaks native MCP
// (JSON-RPC 2.0) over HTTP. It exists so external agents that only support
// remote MCP connectors (rather than a local stdio bridge) have a single,
// correctly-scoped endpoint to reach the governed §24.5 tool set.
//
// It does not own any data itself. Every tool call is forwarded, with the
// caller's own identity headers, to the service that actually owns that
// tool's implementation. See governedTools() for the full, current catalogue
// (memory/fact ops, agent-registry, autonomous-control-plane, backup-manager,
// compatibility-gateway, context-optimisation, credential-vault,
// execution-proxy, extension-manager, memory-gateway, okf-synchroniser,
// recovery-orchestrator, retrieval-orchestrator, security-gate,
// tool-governance, transparency-projection, and observability) and
// dispatchTool() for the per-tool forwarding logic.
const version = "0.1.0"

type requestIdentity struct {
	TenantID    string
	PrincipalID string
}

type gatewayServer struct {
	hermesURL                 string
	memoryIntelligenceURL     string
	codeIntelligenceURL       string
	agentRegistryURL          string
	controlPlaneURL           string
	backupManagerURL          string
	compatGatewayURL          string
	contextOptimisationURL    string
	executionProxyURL         string
	extensionManagerURL       string
	memoryGatewayURL          string
	okfSynchroniserURL        string
	recoveryOrchestratorURL   string
	retrievalOrchestratorURL  string
	securityGateURL           string
	toolGovernanceURL         string
	transparencyProjectionURL string
	observabilityURL          string
	graphifyProviderURL       string
	credentialVaultURL        string
	controlPlaneToken         string
	localBearer               localIdentityToken
	client                    *http.Client
	// longClient carries calls that do bounded but long work upstream --
	// taking a backup, running a restore drill -- which the 15s client
	// would abandon part way through.
	longClient *http.Client
	startTime  time.Time
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
	longClient, err := tlsserve.TrustedClient(30 * time.Minute)
	if err != nil {
		log.Fatalf("configure internal TLS client: %v", err)
	}
	s := &gatewayServer{
		hermesURL:                 resolveUpstream("SCOS_HERMES_PROVIDER_URL", "https://hermes-provider:8088"),
		memoryIntelligenceURL:     resolveUpstream("SCOS_MEMORY_INTELLIGENCE_URL", "https://memory-intelligence:8089"),
		codeIntelligenceURL:       resolveUpstream("SCOS_CODE_INTELLIGENCE_URL", "https://code-intelligence:8084"),
		agentRegistryURL:          resolveUpstream("SCOS_AGENT_REGISTRY_URL", "https://agent-registry:8093"),
		controlPlaneURL:           resolveUpstream("SCOS_AUTONOMOUS_CONTROL_PLANE_URL", "https://autonomous-control-plane:8081"),
		backupManagerURL:          resolveUpstream("SCOS_BACKUP_MANAGER_URL", "https://backup-manager:8095"),
		compatGatewayURL:          resolveUpstream("SCOS_COMPATIBILITY_GATEWAY_URL", "https://compatibility-gateway:8087"),
		contextOptimisationURL:    resolveUpstream("SCOS_CONTEXT_OPTIMISATION_URL", "https://context-optimisation:8083"),
		executionProxyURL:         resolveUpstream("SCOS_EXECUTION_PROXY_URL", "https://execution-proxy:8085"),
		extensionManagerURL:       resolveUpstream("SCOS_EXTENSION_MANAGER_URL", "https://extension-manager:8086"),
		memoryGatewayURL:          resolveUpstream("SCOS_MEMORY_GATEWAY_URL", "https://memory-gateway:8080"),
		okfSynchroniserURL:        resolveUpstream("SCOS_OKF_SYNCHRONISER_URL", "https://okf-synchroniser:8090"),
		recoveryOrchestratorURL:   resolveUpstream("SCOS_RECOVERY_ORCHESTRATOR_URL", "https://recovery-orchestrator:8094"),
		retrievalOrchestratorURL:  resolveUpstream("SCOS_RETRIEVAL_ORCHESTRATOR_URL", "https://retrieval-orchestrator:8082"),
		securityGateURL:           resolveUpstream("SCOS_SECURITY_GATE_URL", "https://security-gate:8091"),
		toolGovernanceURL:         resolveUpstream("SCOS_TOOL_GOVERNANCE_URL", "https://tool-governance:8092"),
		transparencyProjectionURL: resolveUpstream("SCOS_TRANSPARENCY_PROJECTION_URL", "https://transparency-projection:8097"),
		observabilityURL:          resolveUpstream("SCOS_OBSERVABILITY_URL", "https://observability:8096"),
		graphifyProviderURL:       resolveUpstream("SCOS_GRAPHIFY_PROVIDER_URL", "https://graphify-provider:8099"),
		credentialVaultURL:        resolveUpstream("SCOS_CREDENTIAL_VAULT_URL", "https://credential-vault:8100"),
		controlPlaneToken:         strings.TrimSpace(os.Getenv("SCOS_CONTROL_PLANE_TOKEN")),
		client:                    client,
		longClient:                longClient,
		startTime:                 time.Now(),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", healthOK)
	mux.HandleFunc("/health/version", release.Handler("mcp-gateway"))
	mux.HandleFunc("/health/ready", s.handleReady)
	mux.HandleFunc("/mcp", s.handleMCP)

	addr := os.Getenv("SCOS_MCP_GATEWAY_HTTP_ADDR")
	if addr == "" {
		addr = ":8098"
	}

	var handler http.Handler = mux
	if isLocalAuthMode() {
		if _, err := s.localBearer.token(); err != nil {
			log.Fatalf("mcp-gateway: SCOS_AUTH_MODE=local needs SCOS_SECURITY_ISSUER_SECRET to present the local identity to the services behind it: %v", err)
		}
		log.Printf("mcp-gateway (%s): SCOS_AUTH_MODE=local -- no bearer token required, every caller resolves to the reserved local-mode tenant %s", version, localModeTenantID)
	} else {
		handler = auth.AuthenticatedHandler(mux, auth.MustFromEnvironment())
	}

	log.Printf("mcp-gateway (%s) listening on %s, hermes=%s memory-intelligence=%s",
		version, addr, s.hermesURL, s.memoryIntelligenceURL)
	if err := tlsserve.ListenAndServe(addr, telemetry.Instrument("mcp-gateway", handler)); err != nil {
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
		writeRPCResult(w, req.ID, map[string]any{"tools": agentTools(r.Header.Get("X-SCOS-Agent-Name"))})
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
	agentName := r.Header.Get("X-SCOS-Agent-Name")
	if names := desktopAgentToolNames[agentName]; names != nil && !names[call.Name] {
		writeRPCError(w, req.ID, -32602, "tool "+call.Name+" is not exposed to agent "+agentName)
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
	// Downstream services resolve identity from X-SCOS-Tenant-ID /
	// X-SCOS-Principal-ID headers. Overwrite unconditionally rather than only
	// filling a gap: whatever the caller sent is discarded and replaced with
	// the identity proven by their token claims. Preserving a caller-supplied
	// value here was the forwarding half of the tenant-spoofing path closed
	// in resolveIdentity above -- both halves have to go, or an inbound
	// header still reaches the services behind this one.
	r.Header.Set("X-SCOS-Tenant-ID", identity.TenantID)
	r.Header.Set("X-SCOS-Principal-ID", identity.PrincipalID)
	if isLocalAuthMode() {
		// A local-mode caller presents no token, so the services behind
		// this gateway would see an anonymous request and answer 401 to
		// every tool. Present the local identity to them as a signed claims
		// token, exactly as an authenticated caller's token would be.
		token, err := s.localBearer.token()
		if err != nil {
			return nil, fmt.Errorf("local identity token: %w", err)
		}
		r.Header.Set("Authorization", "Bearer "+token)
	}
	switch tool {
	case "scos_status", "scos_recall":
		return s.callHermesProvider(ctx, r, identity, tool, args)
	case "scos_explain":
		memoryID, _ := args["memory_id"].(string)
		return s.forwardGET(ctx, r, s.memoryIntelligenceURL+"/v1/facts/"+url.PathEscape(memoryID))
	case "scos_user_model":
		return s.forwardGET(ctx, r, s.memoryIntelligenceURL+"/v1/dashboard/user-model")
	case "scos_memory_graph":
		return s.forwardGET(ctx, r, s.memoryIntelligenceURL+"/v1/dashboard/memory-graph")
	case "scos_memory_summary":
		u := s.memoryIntelligenceURL + "/v1/dashboard/summary"
		q := url.Values{}
		if n := stringArg(args, "network"); n != "" {
			q.Set("network", n)
		}
		if mf := stringArg(args, "max_facts"); mf != "" {
			q.Set("max_facts", mf)
		}
		if encoded := q.Encode(); encoded != "" {
			u += "?" + encoded
		}
		return s.forwardGET(ctx, r, u)
	case "scos_graphify_query":
		return s.forwardPOST(ctx, r, s.graphifyProviderURL+"/v1/graphify/query", args)
	case "scos_graphify_path":
		return s.forwardPOST(ctx, r, s.graphifyProviderURL+"/v1/graphify/path", args)
	case "scos_graphify_explain":
		return s.forwardPOST(ctx, r, s.graphifyProviderURL+"/v1/graphify/explain", args)
	case "scos_graphify_ingest":
		return s.forwardPOST(ctx, r, s.graphifyProviderURL+"/v1/graphify/extract", args)
	case "scos_graphify_ingest_status":
		jobID := stringArg(args, "job_id")
		if jobID == "" || strings.ContainsAny(jobID, "/?#") {
			return nil, fmt.Errorf("job_id is required")
		}
		return s.forwardGET(ctx, r, s.graphifyProviderURL+"/v1/graphify/extract/"+url.PathEscape(jobID))
	case "scos_code_repo_register":
		legacy, err := s.forwardPOST(ctx, r, s.codeIntelligenceURL+"/v1/repos", args)
		if err != nil {
			return nil, err
		}
		result, ok := legacy.(map[string]any)
		if !ok {
			return legacy, nil
		}
		durable, derr := s.forwardPOST(ctx, r, s.codeIntelligenceURL+"/v2/code/repos", map[string]any{
			"canonical_path": stringArg(args, "canonical_path"),
		})
		if derr != nil {
			result["durable_available"] = false
			result["durable_error"] = derr.Error()
			return result, nil
		}
		if dm, ok := durable.(map[string]any); ok {
			result["durable_repository_id"] = dm["repository_id"]
			result["durable_available"] = true
		}
		return result, nil
	case "scos_code_repo_list":
		return s.forwardGET(ctx, r, s.codeIntelligenceURL+"/v1/repos")
	case "scos_code_repo_get":
		repoID := stringArg(args, "repository_id")
		if repoID == "" {
			return nil, fmt.Errorf("repository_id is required")
		}
		return s.forwardGET(ctx, r, s.codeIntelligenceURL+"/v1/repos/"+repoID)
	case "scos_code_index":
		repositoryID, err := s.durableRepositoryID(ctx, r, args)
		if err != nil {
			return nil, err
		}
		files, hasFiles := args["files"].([]any)
		if !hasFiles || len(files) == 0 {
			// The repository is read from the mounted workspace by
			// code-intelligence itself: nothing has to be uploaded.
			return s.forwardPOST(ctx, r, s.codeIntelligenceURL+"/v2/code/scan", map[string]any{"repository_id": repositoryID})
		}
		branch, commitID := stringArg(args, "branch"), stringArg(args, "commit_id")
		if branch == "" || commitID == "" {
			return nil, fmt.Errorf("uploading files requires branch and commit_id; omit files to index the repository from the workspace")
		}
		for _, file := range files {
			if _, ok := file.(map[string]any); !ok {
				return nil, fmt.Errorf("uploaded files must be objects with path, content_hash and content")
			}
		}
		payload := map[string]any{"repository_id": repositoryID, "branch": branch, "commit_id": commitID, "files": files}
		for _, key := range []string{"extractor_version", "configuration_hash"} {
			if value := stringArg(args, key); value != "" {
				payload[key] = value
			}
		}
		return s.forwardPOST(ctx, r, s.codeIntelligenceURL+"/v2/code/index", payload)
	case "scos_code_index_status":
		snapshotID := stringArg(args, "snapshot_id")
		if snapshotID == "" {
			return nil, fmt.Errorf("snapshot_id is required (from scos_code_index)")
		}
		return s.forwardGET(ctx, r, s.codeIntelligenceURL+"/v2/code/snapshots/"+url.PathEscape(snapshotID))
	case "scos_code_query":
		repositoryID, err := s.durableRepositoryID(ctx, r, args)
		if err != nil {
			return nil, err
		}
		payload := map[string]any{"repository_id": repositoryID, "query": stringArg(args, "query")}
		if branch := stringArg(args, "branch"); branch != "" {
			payload["branch"] = branch
		}
		if maxResults, ok := args["max_results"]; ok {
			payload["max_results"] = maxResults
		}
		return s.forwardPOST(ctx, r, s.codeIntelligenceURL+"/v2/code/query", payload)
	case "scos_code_impact":
		repositoryID, err := s.durableRepositoryID(ctx, r, args)
		if err != nil {
			return nil, err
		}
		symbols := []string{}
		if symbol := stringArg(args, "symbol"); symbol != "" {
			symbols = append(symbols, symbol)
		}
		if list, ok := args["modified_symbols"].([]any); ok {
			for _, item := range list {
				if symbol, ok := item.(string); ok && symbol != "" {
					symbols = append(symbols, symbol)
				}
			}
		}
		if len(symbols) == 0 {
			return nil, fmt.Errorf("symbol (or modified_symbols) is required")
		}
		impacts := make([]any, 0, len(symbols))
		for _, symbol := range symbols {
			payload := map[string]any{"repository_id": repositoryID, "query": symbol}
			if branch := stringArg(args, "branch"); branch != "" {
				payload["branch"] = branch
			}
			if depth, ok := args["max_depth"]; ok {
				payload["max_depth"] = depth
			}
			impact, err := s.forwardPOST(ctx, r, s.codeIntelligenceURL+"/v2/code/impact", payload)
			if err != nil {
				return nil, fmt.Errorf("impact of %s: %w", symbol, err)
			}
			impacts = append(impacts, map[string]any{"symbol": symbol, "impact": impact})
		}
		return map[string]any{"repository_id": repositoryID, "impacts": impacts}, nil
	case "scos_link_evidence":
		factID := stringArg(args, "fact_id")
		symbolVersionID := stringArg(args, "code_symbol_version_id")
		if factID == "" || symbolVersionID == "" {
			return nil, fmt.Errorf("fact_id and code_symbol_version_id are required")
		}
		return s.forwardPOST(ctx, r, s.memoryIntelligenceURL+"/v1/evidence-links", map[string]any{
			"fact_id":                factID,
			"code_symbol_version_id": symbolVersionID,
			"link_type":              stringArg(args, "link_type"),
			"confidence":             args["confidence"],
		})
	case "scos_remember":
		content := strings.TrimSpace(stringArg(args, "content"))
		if content == "" {
			return nil, fmt.Errorf("content is required")
		}
		fact := map[string]any{"content": content, "source": "explicit"}
		for _, key := range []string{"network", "namespace_id", "scope", "valid_until"} {
			if value := strings.TrimSpace(stringArg(args, key)); value != "" {
				fact[key] = value
			}
		}
		return s.forwardPOST(ctx, r, s.memoryIntelligenceURL+"/v1/facts", fact)
	case "scos_memory_review_list":
		status := stringArg(args, "status")
		if status == "" {
			status = "pending_review"
		}
		return s.forwardGET(ctx, r, s.memoryIntelligenceURL+"/v1/candidates?status="+url.QueryEscape(status))
	case "scos_memory_review":
		candidateID := stringArg(args, "candidate_id")
		if candidateID == "" {
			return nil, fmt.Errorf("candidate_id is required")
		}
		body := map[string]any{"decision": stringArg(args, "decision")}
		for _, key := range []string{"rationale", "content", "network"} {
			if value := stringArg(args, key); value != "" {
				body[key] = value
			}
		}
		return s.forwardPOST(ctx, r, s.memoryIntelligenceURL+"/v1/candidates/"+url.PathEscape(candidateID)+"/review", body)
	case "scos_correct":
		memoryID := stringArg(args, "memory_id")
		if memoryID == "" {
			return nil, fmt.Errorf("memory_id is required")
		}
		return s.forwardPATCH(ctx, r, s.memoryIntelligenceURL+"/v1/facts/"+url.PathEscape(memoryID), map[string]any{
			"content": stringArg(args, "value"),
			"reason":  stringArg(args, "reason"),
		})
	case "scos_forget":
		memoryID := stringArg(args, "memory_id")
		if memoryID == "" {
			return nil, fmt.Errorf("memory_id is required")
		}
		u := s.memoryIntelligenceURL + "/v1/facts/" + memoryID
		if cascade, _ := args["cascade"].(bool); cascade {
			u += "?cascade=true"
		}
		return s.forwardDELETE(ctx, r, u)
	case "scos_correct_batch":
		return s.forwardPOST(ctx, r, s.memoryIntelligenceURL+"/v1/facts/batch-correct", map[string]any{
			"corrections": args["corrections"],
		})
	case "scos_forget_batch":
		return s.forwardPOST(ctx, r, s.memoryIntelligenceURL+"/v1/facts/batch-forget", map[string]any{
			"memory_ids": args["memory_ids"],
			"cascade":    args["cascade"],
		})
	case "scos_fact_feedback":
		memoryID := stringArg(args, "memory_id")
		if memoryID == "" {
			return nil, fmt.Errorf("memory_id is required")
		}
		return s.forwardPOST(ctx, r, s.memoryIntelligenceURL+"/v1/facts/"+url.PathEscape(memoryID)+"/feedback", map[string]any{
			"feedback": stringArg(args, "feedback"),
			"reason":   stringArg(args, "reason"),
		})
	case "scos_fact_history":
		memoryID := stringArg(args, "memory_id")
		if memoryID == "" {
			return nil, fmt.Errorf("memory_id is required")
		}
		return s.forwardGET(ctx, r, s.memoryIntelligenceURL+"/v1/facts/"+url.PathEscape(memoryID)+"/history")
	case "scos_forget_all":
		confirm, _ := args["confirm"].(bool)
		if !confirm {
			return nil, fmt.Errorf("confirm must be true to bulk-forget memory")
		}
		u := s.memoryIntelligenceURL + "/v1/facts?confirm=true"
		if ns := stringArg(args, "namespace_id"); ns != "" {
			u += "&namespace_id=" + url.QueryEscape(ns)
		}
		return s.forwardDELETE(ctx, r, u)

	// ── agent-registry ──────────────────────────────────────────────────
	case "scos_agent_list":
		return s.forwardGET(ctx, r, s.agentRegistryURL+"/v1/agents")
	case "scos_agent_get":
		return s.forwardGET(ctx, r, s.agentRegistryURL+"/v1/agents/"+pathArg(args, "id"))
	case "scos_agent_register":
		return s.forwardPOST(ctx, r, s.agentRegistryURL+"/v1/agents/register", args)
	case "scos_agent_heartbeat":
		return s.forwardPOST(ctx, r, s.agentRegistryURL+"/v1/agents/heartbeat", args)
	case "scos_handoff_list":
		return s.forwardGET(ctx, r, s.agentRegistryURL+"/v1/handoffs")
	case "scos_handoff_get":
		return s.forwardGET(ctx, r, s.agentRegistryURL+"/v1/handoffs/"+pathArg(args, "id"))
	case "scos_handoff_initiate":
		return s.forwardPOST(ctx, r, s.agentRegistryURL+"/v1/handoffs/initiate", args)
	case "scos_handoff_respond":
		return s.forwardPOST(ctx, r, s.agentRegistryURL+"/v1/handoffs/respond", args)

	// ── autonomous-control-plane ─────────────────────────────────────────
	case "scos_control_decision_get":
		return s.forwardGET(ctx, r, s.controlPlaneURL+"/v1/control/decisions/"+pathArg(args, "interaction_id"))
	case "scos_control_route":
		return s.forwardPOST(ctx, r, s.controlPlaneURL+"/v1/control/route", args)
	case "scos_control_ingest":
		return s.forwardPOST(ctx, r, s.controlPlaneURL+"/v1/control/ingest", args)
	case "scos_control_replay":
		return s.forwardPOST(ctx, r, s.controlPlaneURL+"/v1/control/replay", args)
	case "scos_control_recover":
		return s.forwardPOST(ctx, r, s.controlPlaneURL+"/v1/control/recover", args)
	case "scos_control_confirm":
		return s.forwardPOST(ctx, r, s.controlPlaneURL+"/v1/control/confirm", args)
	case "scos_control_status_get":
		return s.forwardGET(ctx, r, s.controlPlaneURL+"/v1/control/status/"+pathArg(args, "interaction_id"))
	case "scos_control_trigger":
		return s.forwardPOST(ctx, r, s.controlPlaneURL+"/v1/triggers", args)

	// ── backup-manager ───────────────────────────────────────────────────
	case "scos_backup_list":
		return s.forwardGET(ctx, r, s.backupManagerURL+"/v1/backups")
	case "scos_backup_get":
		return s.forwardGET(ctx, r, s.backupManagerURL+"/v1/backups/"+pathArg(args, "id"))
	case "scos_backup_start":
		return s.forwardLongPOST(ctx, r, s.backupManagerURL+"/v1/backups/start", args)
	case "scos_backup_complete":
		return s.forwardPOST(ctx, r, s.backupManagerURL+"/v1/backups/complete", args)
	case "scos_restore_test_list":
		return s.forwardGET(ctx, r, s.backupManagerURL+"/v1/restore-tests")
	case "scos_restore_test_start":
		return s.forwardLongPOST(ctx, r, s.backupManagerURL+"/v1/restore-tests/start", args)
	case "scos_restore_test_complete":
		return s.forwardPOST(ctx, r, s.backupManagerURL+"/v1/restore-tests/complete", args)
	case "scos_restore_cadence":
		return s.forwardGET(ctx, r, s.backupManagerURL+"/v1/restore-tests/cadence")
	case "scos_backup_plan":
		return s.forwardGET(ctx, r, s.backupManagerURL+"/v1/plan")
	case "scos_okf_mirror_status":
		return s.forwardGET(ctx, r, s.backupManagerURL+"/v1/git-mirror")
	case "scos_okf_mirror_push":
		return s.forwardLongPOST(ctx, r, s.backupManagerURL+"/v1/git-mirror/push", args)
	case "scos_backup_retention":
		return s.forwardGET(ctx, r, s.backupManagerURL+"/v1/retention")

	// ── compatibility-gateway ────────────────────────────────────────────
	case "scos_adapter_list":
		return s.forwardGET(ctx, r, s.compatGatewayURL+"/v1/adapters")
	case "scos_adapter_get":
		return s.forwardGET(ctx, r, s.compatGatewayURL+"/v1/adapters/"+pathArg(args, "boundary"))
	case "scos_compat_status":
		return s.forwardGET(ctx, r, s.compatGatewayURL+"/v1/compatibility")
	case "scos_compat_agent_list":
		return s.forwardGET(ctx, r, s.compatGatewayURL+"/v1/agents")
	case "scos_compat_agent_register":
		return s.forwardPOST(ctx, r, s.compatGatewayURL+"/v1/agents", args)
	case "scos_compat_agent_get":
		return s.forwardGET(ctx, r, s.compatGatewayURL+"/v1/agents/"+pathArg(args, "id"))
	case "scos_envelope_get":
		return s.forwardGET(ctx, r, s.compatGatewayURL+"/v1/envelope?"+queryFromArgs(args, "session_id"))
	case "scos_envelope_set":
		return s.forwardPOST(ctx, r, s.compatGatewayURL+"/v1/envelope", args)
	case "scos_envelope_render":
		return s.forwardPOST(ctx, r, s.compatGatewayURL+"/v1/envelope/render", args)
	case "scos_translate":
		return s.forwardPOST(ctx, r, s.compatGatewayURL+"/v1/translate", args)
	case "scos_compat_conformance":
		u := s.compatGatewayURL + "/v1/compatibility/conformance"
		if boundary := stringArg(args, "boundary"); boundary != "" {
			u += "?boundary=" + url.QueryEscape(boundary)
		}
		return s.forwardGET(ctx, r, u)

	// ── context-optimisation ─────────────────────────────────────────────
	case "scos_context_optimize_input":
		return s.forwardPOST(ctx, r, s.contextOptimisationURL+"/v1/optimize/input", args)
	case "scos_context_optimize_output":
		return s.forwardPOST(ctx, r, s.contextOptimisationURL+"/v1/optimize/output", args)
	case "scos_context_compile":
		return s.forwardPOST(ctx, r, s.contextOptimisationURL+"/v1/context/compile", args)
	case "scos_context_restore":
		return s.forwardPOST(ctx, r, s.contextOptimisationURL+"/v1/context/restore", args)
	case "scos_context_evaluate":
		return s.forwardPOST(ctx, r, s.contextOptimisationURL+"/v1/context/evaluate", args)

	// ── execution-proxy ──────────────────────────────────────────────────
	case "scos_exec_plan":
		return s.forwardPOST(ctx, r, s.executionProxyURL+"/v1/plan", args)
	case "scos_exec_execute":
		// The proxy runs a destructive command only when its caller asserts
		// confirmation. A tool argument is the agent's own claim, not the
		// principal's confirmation, so this route never carries one: a
		// command that needs confirmation goes through scos_control_route and
		// runs after the principal answers with scos_control_confirm.
		if confirmed, _ := args["confirmed"].(bool); confirmed {
			return nil, fmt.Errorf("scos_exec_execute cannot assert confirmation; route the command with scos_control_route and confirm it with scos_control_confirm")
		}
		return s.forwardPOST(ctx, r, s.executionProxyURL+"/v1/execute", args)
	case "scos_exec_results_list":
		return s.forwardGET(ctx, r, s.executionProxyURL+"/v1/results")
	case "scos_exec_result_get":
		return s.forwardGET(ctx, r, s.executionProxyURL+"/v1/results/"+pathArg(args, "operation_id"))
	case "scos_exec_restore":
		return s.forwardGET(ctx, r, s.executionProxyURL+"/v1/proxy/restore/"+pathArg(args, "handle"))
	case "scos_exec_explain":
		return s.forwardPOST(ctx, r, s.executionProxyURL+"/v1/proxy/explain", args)
	case "scos_exec_rules_list":
		return s.forwardGET(ctx, r, s.executionProxyURL+"/v1/rules")
	case "scos_exec_rule_create":
		return s.forwardPOST(ctx, r, s.executionProxyURL+"/v1/rules", args)

	// ── extension-manager ────────────────────────────────────────────────
	case "scos_extension_list":
		return s.forwardGET(ctx, r, s.extensionManagerURL+"/v1/extensions")
	case "scos_extension_get":
		return s.forwardGET(ctx, r, s.extensionManagerURL+"/v1/extensions/"+pathArg(args, "id"))
	case "scos_skill_list":
		return s.forwardGET(ctx, r, s.extensionManagerURL+"/v1/skills")
	case "scos_skill_create":
		return s.forwardPOST(ctx, r, s.extensionManagerURL+"/v1/skills", args)
	case "scos_skill_get":
		return s.forwardGET(ctx, r, s.extensionManagerURL+"/v1/skills/"+pathArg(args, "id"))
	case "scos_hook_list":
		return s.forwardGET(ctx, r, s.extensionManagerURL+"/v1/hooks")
	case "scos_hook_dispatch":
		return s.forwardPOST(ctx, r, s.extensionManagerURL+"/v1/hooks/dispatch", args)
	case "scos_hook_get":
		return s.forwardGET(ctx, r, s.extensionManagerURL+"/v1/hooks/"+pathArg(args, "event"))
	case "scos_extension_install":
		return s.forwardPOST(ctx, r, s.extensionManagerURL+"/v1/install", args)
	case "scos_extension_install_dryrun":
		return s.forwardPOST(ctx, r, s.extensionManagerURL+"/v1/install/dryrun", args)
	case "scos_extension_receipts":
		return s.forwardGET(ctx, r, s.extensionManagerURL+"/v1/receipts")
	case "scos_extension_capabilities":
		return s.forwardGET(ctx, r, s.extensionManagerURL+"/v1/capabilities")

	// ── memory-gateway ───────────────────────────────────────────────────
	case "scos_mg_session_create":
		return s.forwardPOST(ctx, r, s.memoryGatewayURL+"/v1/sessions", args)
	case "scos_mg_message_append":
		return s.forwardPOST(ctx, r, s.memoryGatewayURL+"/v1/sessions/"+pathArg(args, "session_id")+"/messages", args)
	case "scos_mg_chunk_append":
		return s.forwardPOST(ctx, r, s.memoryGatewayURL+"/v1/turns/"+pathArg(args, "turn_id")+"/chunks", args)
	case "scos_mg_response_start":
		return s.forwardPOST(ctx, r, s.memoryGatewayURL+"/v1/turns/"+pathArg(args, "turn_id")+"/response", args)
	case "scos_mg_turns_list":
		return s.forwardGET(ctx, r, s.memoryGatewayURL+"/v1/turns")
	case "scos_mg_turn_complete":
		return s.forwardPOST(ctx, r, s.memoryGatewayURL+"/v1/turns/"+pathArg(args, "turn_id")+"/complete", args)
	case "scos_mg_outbox_status":
		return s.forwardGET(ctx, r, s.memoryGatewayURL+"/v1/outbox/status")
	case "scos_mg_outbox_dead_letters":
		return s.forwardGET(ctx, r, s.memoryGatewayURL+"/v1/outbox/dead-letters")

	// ── okf-synchroniser ─────────────────────────────────────────────────
	case "scos_okf_sync":
		return s.forwardPOST(ctx, r, s.okfSynchroniserURL+"/v1/sync", map[string]any{})
	case "scos_okf_document_list":
		return s.forwardGET(ctx, r, s.okfSynchroniserURL+"/v1/documents")
	case "scos_okf_document_get":
		return s.forwardGET(ctx, r, s.okfSynchroniserURL+"/v1/documents/"+pathArg(args, "id"))
	case "scos_okf_projections":
		return s.forwardGET(ctx, r, s.okfSynchroniserURL+"/v1/projections")
	case "scos_okf_divergence":
		return s.forwardGET(ctx, r, s.okfSynchroniserURL+"/v1/divergence")
	case "scos_okf_proposal_list":
		return s.forwardGET(ctx, r, s.okfSynchroniserURL+"/v1/proposals")
	case "scos_okf_proposal_create":
		return s.forwardPOST(ctx, r, s.okfSynchroniserURL+"/v1/proposals", args)
	case "scos_okf_proposal_action":
		return s.forwardPOST(ctx, r, s.okfSynchroniserURL+"/v1/proposals/"+pathArg(args, "id")+"/"+pathArg(args, "action"), args)

	// ── recovery-orchestrator ────────────────────────────────────────────
	case "scos_recovery_start":
		return s.forwardPOST(ctx, r, s.recoveryOrchestratorURL+"/v1/recovery/start", args)
	case "scos_recovery_checklist_update":
		return s.forwardPOST(ctx, r, s.recoveryOrchestratorURL+"/v1/recovery/checklist", args)
	case "scos_recovery_complete":
		return s.forwardPOST(ctx, r, s.recoveryOrchestratorURL+"/v1/recovery/complete", args)
	case "scos_recovery_status_list":
		return s.forwardGET(ctx, r, s.recoveryOrchestratorURL+"/v1/recovery/status")
	case "scos_recovery_status_get":
		return s.forwardGET(ctx, r, s.recoveryOrchestratorURL+"/v1/recovery/status/"+pathArg(args, "id"))
	case "scos_recovery_interrupted_turns":
		return s.forwardGET(ctx, r, s.recoveryOrchestratorURL+"/v1/recovery/interrupted-turns")

	// ── retrieval-orchestrator ───────────────────────────────────────────
	case "scos_retrieve":
		return s.forwardPOST(ctx, r, s.retrievalOrchestratorURL+"/v1/retrieve", args)

	// ── security-gate ────────────────────────────────────────────────────
	case "scos_auth_validate":
		return s.forwardPOST(ctx, r, s.securityGateURL+"/v1/auth/validate", args)
	case "scos_auth_revoke":
		return s.forwardPOST(ctx, r, s.securityGateURL+"/v1/auth/revoke", args)
	case "scos_permission_check":
		return s.forwardPOST(ctx, r, s.securityGateURL+"/v1/permissions/check", args)
	case "scos_permission_classify":
		return s.forwardPOST(ctx, r, s.securityGateURL+"/v1/permissions/classify", args)
	case "scos_audit_list":
		return s.forwardGET(ctx, r, s.securityGateURL+"/v1/audit")
	case "scos_audit_get":
		return s.forwardGET(ctx, r, s.securityGateURL+"/v1/audit/"+pathArg(args, "id"))
	case "scos_audit_record":
		return s.forwardPOST(ctx, r, s.securityGateURL+"/v1/audit/record", args)
	case "scos_secret_list":
		return s.forwardGET(ctx, r, s.securityGateURL+"/v1/secrets")
	case "scos_secret_create":
		return s.forwardPOST(ctx, r, s.securityGateURL+"/v1/secrets", args)
	case "scos_secret_get":
		return s.forwardGET(ctx, r, s.securityGateURL+"/v1/secrets/"+pathArg(args, "id"))

	// ── tool-governance ──────────────────────────────────────────────────
	case "scos_tool_list":
		return s.forwardGET(ctx, r, s.toolGovernanceURL+"/v1/tools")
	case "scos_tool_get":
		return s.forwardGET(ctx, r, s.toolGovernanceURL+"/v1/tools/"+pathArg(args, "id"))
	case "scos_tool_register":
		return s.forwardPOST(ctx, r, s.toolGovernanceURL+"/v1/tools/register", args)
	case "scos_tool_transition":
		return s.forwardPOST(ctx, r, s.toolGovernanceURL+"/v1/tools/transition", args)
	case "scos_operation_list":
		return s.forwardGET(ctx, r, s.toolGovernanceURL+"/v1/operations")
	case "scos_operation_get":
		return s.forwardGET(ctx, r, s.toolGovernanceURL+"/v1/operations/"+pathArg(args, "id"))
	case "scos_operation_start", "scos_operation_prepare":
		// Governance acts on the confirmation recorded with the operation,
		// and every service reaches it with the same control-plane
		// credential, so the agent boundary is where a self-asserted one is
		// stopped: confirmation comes only from the principal's answer to
		// scos_control_confirm, which the control plane carries itself.
		if confirmed, _ := args["confirmed"].(bool); confirmed {
			return nil, fmt.Errorf("%s cannot assert confirmation; route the operation with scos_control_route and confirm it with scos_control_confirm", tool)
		}
		if tool == "scos_operation_start" {
			return s.forwardPOST(ctx, r, s.toolGovernanceURL+"/v1/operations/start", args)
		}
		return s.forwardPOST(ctx, r, s.toolGovernanceURL+"/v1/operations/prepare", args)
	case "scos_operation_complete":
		return s.forwardPOST(ctx, r, s.toolGovernanceURL+"/v1/operations/complete", args)
	case "scos_operation_reconcile":
		return s.forwardPOST(ctx, r, s.toolGovernanceURL+"/v1/operations/reconcile", args)

	// ── credential-vault ─────────────────────────────────────────────────
	case "scos_credential_list":
		return s.forwardGET(ctx, r, s.credentialVaultURL+"/v1/credentials")
	case "scos_credential_create":
		return s.forwardPOST(ctx, r, s.credentialVaultURL+"/v1/credentials", args)
	case "scos_credential_get":
		return s.forwardGET(ctx, r, s.credentialVaultURL+"/v1/credentials/"+pathArg(args, "id"))
	case "scos_credential_update":
		return s.forwardWithBody(ctx, r, http.MethodPut, s.credentialVaultURL+"/v1/credentials/"+pathArg(args, "id"), args)
	case "scos_credential_delete":
		return s.forwardDELETE(ctx, r, s.credentialVaultURL+"/v1/credentials/"+pathArg(args, "id"))
	case "scos_credential_acl_grant":
		return s.forwardPOST(ctx, r, s.credentialVaultURL+"/v1/credentials/"+pathArg(args, "id")+"/acl", args)
	case "scos_credential_acl_revoke":
		return s.forwardDELETE(ctx, r, s.credentialVaultURL+"/v1/credentials/"+pathArg(args, "id")+"/acl/"+pathArg(args, "principal_id")+"?permission="+url.QueryEscape(stringArg(args, "permission")))

	// ── transparency-projection ──────────────────────────────────────────
	case "scos_transparency_snapshot":
		return s.forwardGET(ctx, r, s.transparencyProjectionURL+"/v1/transparency/snapshot")

	// ── observability ────────────────────────────────────────────────────
	case "scos_metrics_list":
		return s.forwardGET(ctx, r, s.observabilityURL+"/v1/metrics")
	case "scos_metrics_ingest":
		return s.forwardPOST(ctx, r, s.observabilityURL+"/v1/metrics/ingest", args["points"])
	case "scos_metrics_query":
		return s.forwardGET(ctx, r, s.observabilityURL+"/v1/metrics/query?"+queryFromArgs(args, "name", "service", "since"))
	case "scos_traces_list":
		return s.forwardGET(ctx, r, s.observabilityURL+"/v1/traces")
	case "scos_trace_ingest":
		return s.forwardPOST(ctx, r, s.observabilityURL+"/v1/traces/ingest", args["spans"])
	case "scos_trace_get":
		return s.forwardGET(ctx, r, s.observabilityURL+"/v1/traces/"+pathArg(args, "id"))
	case "scos_logs_list":
		return s.forwardGET(ctx, r, s.observabilityURL+"/v1/logs")
	case "scos_log_ingest":
		return s.forwardPOST(ctx, r, s.observabilityURL+"/v1/logs/ingest", args["entries"])
	case "scos_observability_targets":
		return s.forwardGET(ctx, r, s.observabilityURL+"/v1/targets")
	case "scos_observability_export_status":
		return s.forwardGET(ctx, r, s.observabilityURL+"/v1/export/status")
	case "scos_benchmarks":
		q := url.Values{}
		if suite := stringArg(args, "suite"); suite != "" {
			q.Set("suite", suite)
		}
		if limit, ok := args["limit"].(float64); ok {
			q.Set("limit", fmt.Sprintf("%d", int(limit)))
		}
		u := s.observabilityURL + "/v1/benchmarks"
		if encoded := q.Encode(); encoded != "" {
			u += "?" + encoded
		}
		return s.forwardGET(ctx, r, u)
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
		// A caller-named session scopes the recall's session lane to that
		// session (hermes-provider asks retrieval for include_session). With
		// no session named, recall is long-term memory only: no session is
		// invented for the caller.
		if sessionID := strings.TrimSpace(stringArg(args, "session_id")); sessionID != "" {
			if !isUUID(sessionID) {
				return nil, fmt.Errorf("session_id must be a session UUID")
			}
			body["session_id"] = sessionID
		}
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
	s.copyIdentityHeaders(r, req)
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
	s.copyIdentityHeaders(r, req)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, errUnreachable(upstreamName(url) + " unavailable: " + err.Error())
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, errUnreachable(upstreamName(url) + " returned " + resp.Status + ": " + strings.TrimSpace(string(body)))
	}
	var result any
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *gatewayServer) forwardPOST(ctx context.Context, r *http.Request, url string, args any) (any, error) {
	return s.forwardPOSTWith(s.client, ctx, r, url, args)
}

// forwardLongPOST is forwardPOST for upstream work measured in minutes.
func (s *gatewayServer) forwardLongPOST(ctx context.Context, r *http.Request, url string, args any) (any, error) {
	client := s.longClient
	if client == nil {
		client = s.client
	}
	return s.forwardPOSTWith(client, ctx, r, url, args)
}

func (s *gatewayServer) forwardPOSTWith(client *http.Client, ctx context.Context, r *http.Request, url string, args any) (any, error) {
	// A caller makes a retry safe by passing the same idempotency_key
	// argument; it selects the Idempotency-Key and is not forwarded.
	callerKey := ""
	if m, ok := args.(map[string]any); ok {
		if k, ok := m["idempotency_key"].(string); ok && strings.TrimSpace(k) != "" {
			callerKey = strings.TrimSpace(k)
			stripped := make(map[string]any, len(m))
			for key, v := range m {
				if key != "idempotency_key" {
					stripped[key] = v
				}
			}
			args = stripped
		}
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(encoded)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	s.copyIdentityHeaders(r, req)
	if req.Header.Get("Idempotency-Key") == "" {
		// MCP tool calls carry no header channel the caller can set, yet
		// several governed endpoints (memory-gateway, tool-governance)
		// require an Idempotency-Key. Each call is its own action -- two
		// identical "append yes" or "create session" calls are two actions,
		// not a retry -- so each gets a fresh key, unless the caller names
		// the call with idempotency_key, in which case calls naming the same
		// key (from the same identity, to the same endpoint) are one.
		if callerKey != "" {
			req.Header.Set("Idempotency-Key", deriveIdempotencyKey(req.Header.Get("Authorization"), url, []byte(callerKey)))
		} else {
			req.Header.Set("Idempotency-Key", freshIdempotencyKey())
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, errUnreachable(upstreamName(url) + " unavailable: " + err.Error())
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, errUnreachable(upstreamName(url) + " returned " + resp.Status + ": " + strings.TrimSpace(string(body)))
	}
	var result any
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *gatewayServer) forwardPATCH(ctx context.Context, r *http.Request, url string, args any) (any, error) {
	return s.forwardWithBody(ctx, r, http.MethodPatch, url, args)
}

func (s *gatewayServer) forwardDELETE(ctx context.Context, r *http.Request, url string) (any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return nil, err
	}
	s.copyIdentityHeaders(r, req)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, errUnreachable(upstreamName(url) + " unavailable: " + err.Error())
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, errUnreachable(upstreamName(url) + " returned " + resp.Status + ": " + strings.TrimSpace(string(body)))
	}
	var result any
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *gatewayServer) forwardWithBody(ctx context.Context, r *http.Request, method, url string, args any) (any, error) {
	encoded, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, url, strings.NewReader(string(encoded)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	s.copyIdentityHeaders(r, req)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, errUnreachable(upstreamName(url) + " unavailable: " + err.Error())
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, errUnreachable(upstreamName(url) + " returned " + resp.Status + ": " + strings.TrimSpace(string(body)))
	}
	var result any
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// durableRepositoryID resolves the repository a code tool names to its
// durable (snapshot) repository id. scos_code_repo_register returns both a
// legacy id and the durable id; either is accepted, the legacy one being
// resolved through its canonical path.
func (s *gatewayServer) durableRepositoryID(ctx context.Context, r *http.Request, args map[string]any) (string, error) {
	id := stringArg(args, "durable_repository_id")
	if id == "" {
		id = stringArg(args, "repository_id")
	}
	if id == "" {
		return "", fmt.Errorf("repository_id is required (from scos_code_repo_register)")
	}
	if isUUID(id) {
		return id, nil
	}
	legacy, err := s.forwardGET(ctx, r, s.codeIntelligenceURL+"/v1/repos/"+url.PathEscape(id))
	if err != nil {
		return "", err
	}
	repo, _ := legacy.(map[string]any)
	path, _ := repo["canonical_path"].(string)
	if path == "" {
		return "", fmt.Errorf("repository %s has no canonical path", id)
	}
	durable, err := s.forwardPOST(ctx, r, s.codeIntelligenceURL+"/v2/code/repos", map[string]any{"canonical_path": path})
	if err != nil {
		return "", err
	}
	dm, _ := durable.(map[string]any)
	durableID, _ := dm["repository_id"].(string)
	if durableID == "" {
		return "", fmt.Errorf("code-intelligence returned no durable repository for %s", path)
	}
	return durableID, nil
}

func isUUID(v string) bool {
	if len(v) != 36 {
		return false
	}
	for i, c := range v {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return false
		}
	}
	return true
}

// pathArg is a string argument escaped for use as one URL path segment, so
// an id like "x/acl/y" cannot reach a different route of the service.
func pathArg(args map[string]any, key string) string {
	return url.PathEscape(stringArg(args, key))
}

func stringArg(args map[string]any, key string) string {
	value, _ := args[key].(string)
	return value
}

func queryFromArgs(args map[string]any, keys ...string) string {
	values := url.Values{}
	for _, key := range keys {
		if value := stringArg(args, key); value != "" {
			values.Set(key, value)
		}
	}
	return values.Encode()
}

// upstreamName names the service a forwarded call went to, from its URL
// host, so a failure is attributed to the service that produced it.
func upstreamName(target string) string {
	if u, err := url.Parse(target); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	return "upstream"
}

type errUnreachable string

func (e errUnreachable) Error() string { return string(e) }

// freshIdempotencyKey is a random key for a call that names none.
func freshIdempotencyKey() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("mcp-gateway:%d", time.Now().UnixNano())
	}
	return "mcp-gateway:" + hex.EncodeToString(b)
}

func deriveIdempotencyKey(authHeader, url string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(authHeader))
	h.Write([]byte("\x00"))
	h.Write([]byte(url))
	h.Write([]byte("\x00"))
	h.Write(body)
	return "mcp-gateway:" + hex.EncodeToString(h.Sum(nil))
}

func (s *gatewayServer) copyIdentityHeaders(src *http.Request, dst *http.Request) {
	for _, name := range []string{"Authorization", "X-SCOS-Tenant-ID", "X-SCOS-Principal-ID", "Idempotency-Key"} {
		if value := src.Header.Get(name); value != "" {
			dst.Header.Set(name, value)
		}
	}
	// Control-plane-protected services (code-intelligence etc.) require the
	// control-plane token in addition to the bearer identity; forward the
	// gateway's own token so governed tool calls reach them.
	if s.controlPlaneToken != "" {
		dst.Header.Set("X-SCOS-Control-Plane-Token", s.controlPlaneToken)
	}
}

// localModeTenantID and localModePrincipalID are the reserved, well-known
// identity every caller resolves to under SCOS_AUTH_MODE=local (self-hosted,
// single-user, no accounts). A migration seeds a matching tenant/principal/
// workspace/namespace row for this UUID so downstream RLS policies -- which
// deny all access when scos.tenant_id is unset, not "allow all" -- keep
// working unchanged; local mode is a startup-time identity shortcut here,
// never a change to auth enforcement or row-level security elsewhere.
const (
	localModeTenantID    = "00000000-0000-0000-0000-000000000001"
	localModePrincipalID = "00000000-0000-0000-0000-000000000001"
)

// localIdentityToken mints and caches the claims token that carries the
// local-mode identity to downstream services. The local user owns the
// whole installation, so the token also carries the operator scopes the
// backup and recovery routes and tool-governance's tool registration and
// authorisation routes require.
type localIdentityToken struct {
	mu      sync.Mutex
	value   string
	expires time.Time
}

const localTokenTTL = time.Hour

func (l *localIdentityToken) token() (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.value != "" && time.Until(l.expires) > 5*time.Minute {
		return l.value, nil
	}
	expires := time.Now().Add(localTokenTTL)
	value, err := auth.SignClaims(auth.Claims{
		TenantID:    localModeTenantID,
		PrincipalID: localModePrincipalID,
		Subject:     localModePrincipalID,
		AgentID:     "mcp-gateway-local",
		ExpiresAt:   expires,
		Scopes:      []string{"backups:admin", "recovery:admin", "tools:admin", "extensions:admin"},
	}, os.Getenv("SCOS_SECURITY_ISSUER_SECRET"))
	if err != nil {
		return "", err
	}
	l.value, l.expires = value, expires
	return value, nil
}

func isLocalAuthMode() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("SCOS_AUTH_MODE")), "local")
}

// resolveIdentity derives the caller's tenant and principal from validated
// bearer claims and from nothing else -- unless SCOS_AUTH_MODE=local, in
// which case every caller resolves to the reserved local-mode identity
// above and no bearer token is required at all (see main(), which skips
// auth.AuthenticatedHandler entirely in that mode).
//
// The internal services (hermes-provider, memory-intelligence,
// tool-governance) still accept X-SCOS-Tenant-ID / X-SCOS-Principal-ID as a
// fallback for direct service-to-service calls on the private network. This
// gateway must not, because it is the one externally reachable surface in
// the fleet: every agent plugin, desktop extension and CLI reaches the fabric
// through here. Honouring a caller-supplied tenant header at this boundary
// let any holder of the shared service token read and write any tenant's
// memory simply by naming a different UUID -- the tenant boundary was
// enforced by real code everywhere downstream, but the value it enforced on
// originated with the attacker.
//
// Claims are therefore mandatory here (outside local mode). A caller
// presenting only the shared static service token is authenticated but
// anonymous, and is refused rather than being allowed to nominate an
// identity.
func resolveIdentity(r *http.Request) (requestIdentity, int, string) {
	if isLocalAuthMode() {
		return requestIdentity{TenantID: localModeTenantID, PrincipalID: localModePrincipalID}, 0, ""
	}
	claims, ok := auth.CanonicalClaims(r.Context())
	if !ok {
		return requestIdentity{}, http.StatusUnauthorized, "a bearer token carrying tenant_id and principal_id claims is required; X-SCOS-* identity headers are not accepted at this boundary"
	}
	return requestIdentity{TenantID: claims.TenantID, PrincipalID: claims.PrincipalID}, 0, ""
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

// memoryTierToolNames is the curated memory subset documented in every
// desktop-agent plugin bundle (integrations/*/skills/scos-memory/SKILL.md,
// integrations/agent-plugins/*/*.md): recall, inspect, correct, forget --
// none of mcp-gateway's code-intelligence, agent-registry, or governance
// tooling, which those bundles never mention and are not built to drive.
var memoryTierToolNames = map[string]bool{
	"scos_recall":                 true,
	"scos_status":                 true,
	"scos_explain":                true,
	"scos_user_model":             true,
	"scos_memory_graph":           true,
	"scos_remember":               true,
	"scos_memory_review_list":     true,
	"scos_memory_review":          true,
	"scos_correct":                true,
	"scos_forget":                 true,
	"scos_graphify_query":         true,
	"scos_graphify_path":          true,
	"scos_graphify_explain":       true,
	"scos_graphify_ingest":        true,
	"scos_graphify_ingest_status": true,
}

// desktopAgentToolNames maps X-SCOS-Agent-Name values (set by the shared
// stdio bridge from SCOS_AGENT_NAME, see scripts/install_agent_plugins.py)
// to the tool subset that agent's bundle documents and is scoped to. Agents
// absent from this map (native-plugin integrations that may grow beyond the
// memory tier, and any caller that omits the header) get the full governed
// catalogue -- this map only ever narrows, never widens, access.
var desktopAgentToolNames = map[string]map[string]bool{
	"cline":          memoryTierToolNames,
	"cursor":         memoryTierToolNames,
	"windsurf-devin": memoryTierToolNames,
	"antigravity":    memoryTierToolNames,
	"continue":       memoryTierToolNames,
	"gemini-cli":     memoryTierToolNames,
}

// agentTools returns the tools/list payload for one caller: the full
// governed catalogue, or the narrower memory-tier subset for agents in
// desktopAgentToolNames.
func agentTools(agentName string) []map[string]any {
	names := desktopAgentToolNames[agentName]
	if names == nil {
		return governedTools()
	}
	tools := make([]map[string]any, 0, len(names))
	for _, t := range governedTools() {
		if names[t["name"].(string)] {
			tools = append(tools, t)
		}
	}
	return tools
}

// strField, intField, boolField, arrField and objField build inputSchema
// property entries; tool assembles a governed tool catalogue entry. These
// exist because governedTools() below enumerates well over a hundred tools
// -- writing each out as a fully literal nested map is what actually reads
// as "no abstraction", not the reverse.
func strField(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}
func intField(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}
func boolField(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}
func arrField(items map[string]any, desc string) map[string]any {
	return map[string]any{"type": "array", "items": items, "description": desc}
}
func objField(desc string) map[string]any {
	return map[string]any{"type": "object", "description": desc}
}

func tool(name, desc string, props map[string]any, required ...string) map[string]any {
	schema := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		schema["required"] = required
	}
	return map[string]any{"name": name, "description": desc, "inputSchema": schema}
}

func governedTools() []map[string]any {
	tools := []map[string]any{
		{
			"name":        "scos_recall",
			"description": "Search SCOS Nexus memory for relevant context",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query":      map[string]any{"type": "string", "description": "Search query"},
					"max_items":  map[string]any{"type": "integer", "description": "Max results"},
					"session_id": map[string]any{"type": "string", "description": "Session UUID; when given, the session's working and episodic memories are recalled alongside long-term memory"},
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
			"name":        "scos_memory_summary",
			"description": "Summarize the caller's active facts. Uses a real LLM summary when configured, otherwise a deterministic network-grouped digest -- the response's mode field says which",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"network":   map[string]any{"type": "string", "description": "Limit to one memory network (world, experience, entity, belief, procedure, negative)"},
					"max_facts": map[string]any{"type": "string", "description": "Max facts to include (default 200, max 500)"},
				},
			},
		},
		{
			"name":        "scos_graphify_query",
			"description": "Ask a plain-language question against the graphify knowledge graph and get back the scoped subgraph that answers it",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"q":         map[string]any{"type": "string", "description": "The question to ask the graph"},
					"repo_path": map[string]any{"type": "string", "description": "Absolute path of the repo to graph. Omit to use the provider's default repo. The first question about a repository (or the first after it changes) builds its graph in the background; until it is ready the call returns status \"building\" with retry_after_seconds, and the same call should be repeated after that delay."},
				},
				"required": []string{"q"},
			},
		},
		{
			"name":        "scos_graphify_path",
			"description": "Trace the shortest path between two concepts in the graphify knowledge graph",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"from":      map[string]any{"type": "string", "description": "Starting node name"},
					"to":        map[string]any{"type": "string", "description": "Target node name"},
					"repo_path": map[string]any{"type": "string", "description": "Absolute path of the repo to graph. Omit to use the provider's default repo. The first question about a repository (or the first after it changes) builds its graph in the background; until it is ready the call returns status \"building\" with retry_after_seconds, and the same call should be repeated after that delay."},
				},
				"required": []string{"from", "to"},
			},
		},
		{
			"name":        "scos_graphify_explain",
			"description": "Explain one node in the graphify knowledge graph and list its connections",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"node":      map[string]any{"type": "string", "description": "Node name to explain"},
					"repo_path": map[string]any{"type": "string", "description": "Absolute path of the repo to graph. Omit to use the provider's default repo. The first question about a repository (or the first after it changes) builds its graph in the background; until it is ready the call returns status \"building\" with retry_after_seconds, and the same call should be repeated after that delay."},
				},
				"required": []string{"node"},
			},
		},
		{
			"name":        "scos_graphify_ingest",
			"description": "Ingest a repository's graphify knowledge graph into memory as the caller: every code entity becomes an entity memory (with its source location) and every edge a relation. Runs in the background; returns a job_id to poll with scos_graphify_ingest_status, or status \"building\" with retry_after_seconds while the graph itself is still being built.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"repo_path":    map[string]any{"type": "string", "description": "Absolute path of the repo to ingest. Omit to use the provider's default repo."},
					"namespace_id": map[string]any{"type": "string", "description": "Namespace to write the memories into. Omit to use the deployment default or the caller's own writable namespace."},
				},
			},
		},
		{
			"name":        "scos_graphify_ingest_status",
			"description": "Report the progress of a graph ingestion started with scos_graphify_ingest: nodes and edges ingested so far, errors, and whether it has finished",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"job_id": map[string]any{"type": "string", "description": "The job_id scos_graphify_ingest returned"},
				},
				"required": []string{"job_id"},
			},
		},
		{
			"name":        "scos_code_repo_register",
			"description": "Register a repository with code intelligence. Returns legacy repository_id plus durable_repository_id for P4 snapshot/hybrid retrieval when available.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"canonical_path":     map[string]any{"type": "string", "description": "Absolute canonical path of the repository on disk"},
					"remote_identity":    map[string]any{"type": "string", "description": "Remote identity, e.g. a git remote URL"},
					"branch_or_worktree": map[string]any{"type": "string", "description": "Branch or worktree name"},
					"language_set":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Languages present in the repository"},
				},
				"required": []string{"canonical_path"},
			},
		},
		{
			"name":        "scos_code_repo_list",
			"description": "List repositories registered with code intelligence for the caller's tenant",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			"name":        "scos_code_repo_get",
			"description": "Get a registered repository's details, including its current index generation",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"repository_id": map[string]any{"type": "string", "description": "Repository identifier"},
				},
				"required": []string{"repository_id"},
			},
		},
		{
			"name":        "scos_code_index",
			"description": "Index a registered repository so it can be queried. Returns at once with a snapshot_id; follow it with scos_code_index_status. With just repository_id, code-intelligence reads the repository from the mounted workspace itself (Go AST and Tree-sitter for Python/TypeScript/JavaScript/Rust), records the branch and commit from its git checkout, and indexes only what changed since the last snapshot. Uploading files is only needed for repositories outside the workspace",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"repository_id": map[string]any{"type": "string", "description": "Repository id from scos_code_repo_register (either id it returns)"},
					"branch":        map[string]any{"type": "string", "description": "Upload mode only: branch the files belong to"},
					"commit_id":     map[string]any{"type": "string", "description": "Upload mode only: commit the files belong to"},
					"files": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{
						"path": map[string]any{"type": "string"}, "language": map[string]any{"type": "string"},
						"content_hash": map[string]any{"type": "string"}, "content": map[string]any{"type": "string"},
					}, "required": []string{"path", "content_hash"}}, "description": "Upload mode only: files to index instead of reading the workspace"},
				},
				"required": []string{"repository_id"},
			},
		},
		{
			"name":        "scos_code_index_status",
			"description": "Progress of a repository index started by scos_code_index: indexing, ready (with file and symbol counts) or failed (with the reason)",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"snapshot_id": map[string]any{"type": "string", "description": "snapshot_id returned by scos_code_index"},
				},
				"required": []string{"snapshot_id"},
			},
		},
		{
			"name":        "scos_code_query",
			"description": "Search an indexed repository by symbol name, qualified name, path or natural-language description; returns ranked symbols with their file, kind and signature",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"repository_id": map[string]any{"type": "string", "description": "Repository id from scos_code_repo_register"},
					"query":         map[string]any{"type": "string", "description": "Symbol, path or description to search for"},
					"branch":        map[string]any{"type": "string", "description": "Defaults to the most recently indexed branch"},
					"max_results":   map[string]any{"type": "integer", "description": "Max results"},
				},
				"required": []string{"repository_id", "query"},
			},
		},
		{
			"name":        "scos_code_impact",
			"description": "What depends on a symbol: its callers and transitive dependants in the indexed call graph, and the tests that reach it. Use before changing a function or type",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"repository_id":    map[string]any{"type": "string", "description": "Repository id from scos_code_repo_register"},
					"symbol":           map[string]any{"type": "string", "description": "Symbol name or qualified name being changed"},
					"modified_symbols": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Several symbols at once"},
					"branch":           map[string]any{"type": "string", "description": "Defaults to the most recently indexed branch"},
					"max_depth":        map[string]any{"type": "integer", "description": "How far to follow dependants"},
				},
				"required": []string{"repository_id"},
			},
		},
		{
			"name":        "scos_link_evidence",
			"description": "Record that a memory fact evidences, was motivated by, or was invalidated by a specific code symbol version (P1-C explicit memory<->code links)",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"fact_id":                map[string]any{"type": "string", "description": "Memory fact ID"},
					"code_symbol_version_id": map[string]any{"type": "string", "description": "code_symbol_versions.id from scos_code_query/scos_code_impact"},
					"link_type":              map[string]any{"type": "string", "enum": []string{"evidences", "motivated_by", "invalidated_by"}, "description": "Relationship type; defaults to evidences"},
					"confidence":             map[string]any{"type": "number", "description": "0-1, defaults to 1.0"},
				},
				"required": []string{"fact_id", "code_symbol_version_id"},
			},
		},
		{
			"name":        "scos_remember",
			"description": "Save a fact to long-term memory now. Use when the user states something to remember or a durable decision is made. Duplicates merge into the existing memory and contradictions are flagged; the stored fact is returned with its id for later scos_correct/scos_forget",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"content":      map[string]any{"type": "string", "description": "The fact, stated as a standalone sentence"},
					"network":      map[string]any{"type": "string", "enum": []string{"world", "experience", "entity", "belief", "procedure", "negative"}, "description": "Kind of memory; defaults to world (facts about the world/project)"},
					"namespace_id": map[string]any{"type": "string", "description": "Namespace to store in; defaults to the caller's own namespace"},
					"scope":        map[string]any{"type": "string", "description": "Scope the fact holds in (e.g. global, project, session); defaults to global"},
					"valid_until":  map[string]any{"type": "string", "description": "RFC 3339 time after which the fact no longer holds"},
				},
				"required": []string{"content"},
			},
		},
		{
			"name":        "scos_memory_review_list",
			"description": "List memories held back for a person's approval (high-impact, governing, sensitive or conflicting content extracted from conversations). They are not recalled until approved",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"status": map[string]any{"type": "string", "enum": []string{"pending_review", "promoted", "rejected"}, "description": "Defaults to pending_review"},
				},
			},
		},
		{
			"name":        "scos_memory_review",
			"description": "Approve a held-back memory into long-term memory (optionally with corrected wording) or reject it with a reason. Only do this on the user's explicit instruction",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"candidate_id": map[string]any{"type": "string", "description": "Candidate id from scos_memory_review_list"},
					"decision":     map[string]any{"type": "string", "enum": []string{"approve", "reject"}},
					"rationale":    map[string]any{"type": "string", "description": "Why; required to reject"},
					"content":      map[string]any{"type": "string", "description": "Approve only: corrected wording to store instead of the extracted text"},
					"network":      map[string]any{"type": "string", "enum": []string{"world", "experience", "entity", "belief", "procedure", "negative"}, "description": "Approve only: kind of memory, if the proposed one is wrong"},
				},
				"required": []string{"candidate_id", "decision"},
			},
		},
		{
			"name":        "scos_correct",
			"description": "Correct a stored memory, preserving the superseded version",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"memory_id": map[string]any{"type": "string", "description": "Memory ID to correct"},
					"value":     map[string]any{"type": "string", "description": "Corrected value"},
					"reason":    map[string]any{"type": "string", "description": "Why the memory is being corrected; kept in its history"},
				},
				"required": []string{"memory_id", "value"},
			},
		},
		{
			"name":        "scos_forget",
			"description": "Request removal of a specific memory. cascade=true also forgets the fact's superseded predecessor chain",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"memory_id": map[string]any{"type": "string", "description": "Memory ID to forget"},
					"cascade":   map[string]any{"type": "boolean", "description": "Also revoke this fact's superseded predecessor chain"},
				},
				"required": []string{"memory_id"},
			},
		},
		{
			"name":        "scos_correct_batch",
			"description": "Correct multiple facts in one call, each independently -- one failure does not fail the others",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"corrections": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"memory_id": map[string]any{"type": "string", "description": "Memory ID to correct"},
								"value":     map[string]any{"type": "string", "description": "Corrected value"},
								"reason":    map[string]any{"type": "string", "description": "Why this correction was made"},
							},
							"required": []string{"memory_id", "value"},
						},
						"description": "Up to 100 corrections",
					},
				},
				"required": []string{"corrections"},
			},
		},
		{
			"name":        "scos_forget_batch",
			"description": "Forget multiple specific facts by ID in one call, each independently -- one failure does not fail the others",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"memory_ids": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Up to 100 fact IDs to forget"},
					"cascade":    map[string]any{"type": "boolean", "description": "Also revoke each fact's superseded predecessor chain"},
				},
				"required": []string{"memory_ids"},
			},
		},
		{
			"name":        "scos_fact_feedback",
			"description": "Record quality feedback (POSITIVE, NEGATIVE, or VERY_NEGATIVE) against a fact. Recorded as a signal only -- does not itself correct or forget the fact",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"memory_id": map[string]any{"type": "string", "description": "Fact ID"},
					"feedback":  map[string]any{"type": "string", "description": "POSITIVE, NEGATIVE, or VERY_NEGATIVE"},
					"reason":    map[string]any{"type": "string", "description": "Why this feedback was given"},
				},
				"required": []string{"memory_id", "feedback"},
			},
		},
		{
			"name":        "scos_fact_history",
			"description": "Get the full append-only revision history of a fact, including versions from before a correction or after a forget",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"memory_id": map[string]any{"type": "string", "description": "Fact ID"}},
				"required":   []string{"memory_id"},
			},
		},
		{
			"name":        "scos_forget_all",
			"description": "Bulk-forget memory: all active facts in one namespace, or (omitting namespace_id) every active fact for the caller's tenant. Irreversible in intent -- requires confirm=true",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"namespace_id": map[string]any{"type": "string", "description": "Limit to one namespace; omit to forget every namespace for the tenant"},
					"confirm":      map[string]any{"type": "boolean", "description": "Must be true to perform the bulk forget"},
				},
				"required": []string{"confirm"},
			},
		},
	}

	// ── agent-registry ──────────────────────────────────────────────────
	tools = append(tools,
		tool("scos_agent_list", "List registered agents for the caller's tenant", map[string]any{}),
		tool("scos_agent_get", "Get a registered agent by ID", map[string]any{"id": strField("Agent UUID")}, "id"),
		tool("scos_agent_register", "Register (or update) an agent", map[string]any{
			"name":               strField("Agent name"),
			"version":            strField("Agent version"),
			"maintainer":         strField("Maintainer"),
			"capabilities":       arrField(strField(""), "Capabilities"),
			"inputs":             arrField(strField(""), "Accepted inputs"),
			"outputs":            arrField(strField(""), "Produced outputs"),
			"tool_permissions":   arrField(strField(""), "Tool permissions this agent declares; a handoff requiring a tool not covered here is refused. Exact names, \"*\", or a prefix ending in *"),
			"memory_permissions": arrField(strField(""), "Memory namespaces this agent declares; a handoff authorising a namespace not covered here is refused. Exact names, \"*\", or a prefix ending in *"),
			"max_classification": strField("Max data classification"),
			"model_provider":     strField("Model provider"),
			"cost_policy":        strField("Cost policy"),
			"health_endpoint":    strField("Health check endpoint"),
			"status":             strField("active, inactive, or degraded"),
		}, "name", "version", "maintainer", "capabilities", "inputs", "outputs", "max_classification", "model_provider", "cost_policy", "health_endpoint"),
		tool("scos_agent_heartbeat", "Record an agent heartbeat", map[string]any{
			"agent_id": strField("Agent UUID"),
		}, "agent_id"),
		tool("scos_handoff_list", "List handoff packets for the caller's tenant", map[string]any{}),
		tool("scos_handoff_get", "Get a handoff packet by ID", map[string]any{"id": strField("Handoff UUID")}, "id"),
		tool("scos_handoff_initiate", "Initiate an agent-to-agent handoff", map[string]any{
			"from_agent":                strField("Originating agent (defaults to, and must match, the caller's agent claim when it has one)"),
			"to_agent":                  strField("Registration UUID of the receiving agent"),
			"objective":                 strField("Handoff objective"),
			"constraints":               arrField(strField(""), "Constraints"),
			"authorised_namespaces":     arrField(strField(""), "Authorised namespaces"),
			"memory_references":         arrField(strField(""), "Memory references"),
			"artifact_references":       arrField(strField(""), "Artifact references"),
			"completed_work":            arrField(strField(""), "Completed work"),
			"open_work":                 arrField(strField(""), "Open work"),
			"known_failures":            arrField(strField(""), "Known failures"),
			"acceptance_criteria":       strField("Acceptance criteria"),
			"expires_at":                strField("RFC 3339 expiry; after it the handoff can no longer be answered"),
			"required_tool_permissions": arrField(strField(""), "Tool permissions the receiving agent must declare"),
		}, "to_agent", "objective"),
		tool("scos_handoff_respond", "Accept or reject a handoff. Only the addressed agent, or the principal that registered it, may answer; a handoff is answered once and an expired one is refused", map[string]any{
			"handoff_id": strField("Handoff UUID"),
			"response":   strField("accepted or rejected"),
			"reason":     strField("Optional reason recorded with the answer"),
		}, "handoff_id", "response"),
	)

	// ── autonomous-control-plane ─────────────────────────────────────────
	tools = append(tools,
		tool("scos_control_decision_get", "Get the route decision for an interaction", map[string]any{
			"interaction_id": strField("Interaction ID"),
		}, "interaction_id"),
		tool("scos_control_route", "Route an interaction through the autonomous control plane. A decision that requires confirmation returns status awaiting_confirmation and runs nothing until scos_control_confirm", map[string]any{
			"interaction_id":      strField("Interaction ID"),
			"actor_id":            strField("Actor ID"),
			"agent_id":            strField("Agent ID"),
			"session_id":          strField("Session ID"),
			"turn_id":             strField("Turn ID"),
			"project_id":          strField("Project ID"),
			"input":               strField("Input text"),
			"repository_path":     strField("Repository path"),
			"proposed_tool":       strField("Proposed tool"),
			"risk_class":          strField("Risk class"),
			"requires_fresh_data": boolField("Requires fresh data"),
		}, "interaction_id", "input"),
		tool("scos_control_ingest", "Ingest and route an interaction (alias of scos_control_route)", map[string]any{
			"interaction_id":      strField("Interaction ID"),
			"actor_id":            strField("Actor ID"),
			"agent_id":            strField("Agent ID"),
			"session_id":          strField("Session ID"),
			"turn_id":             strField("Turn ID"),
			"project_id":          strField("Project ID"),
			"input":               strField("Input text"),
			"repository_path":     strField("Repository path"),
			"proposed_tool":       strField("Proposed tool"),
			"risk_class":          strField("Risk class"),
			"requires_fresh_data": boolField("Requires fresh data"),
		}, "interaction_id", "input"),
		tool("scos_control_replay", "Replay a previously routed interaction's decision", map[string]any{
			"interaction_id": strField("Interaction ID"),
			"decision_id":    strField("Decision ID"),
		}, "interaction_id"),
		tool("scos_control_recover", "Resume an interrupted interaction from its persisted state; reports reconciliation_required when its command may already have run", map[string]any{
			"interaction_id": strField("Interaction ID"),
		}, "interaction_id"),
		tool("scos_control_confirm", "Confirm or reject a routed action that is awaiting_confirmation. Only the principal that requested it may answer, and only for the exact decision_id it was shown; a confirmation runs the action", map[string]any{
			"interaction_id": strField("Interaction ID"),
			"decision_id":    strField("Decision ID returned with the awaiting_confirmation route"),
			"resolution":     strField("confirmed or rejected"),
		}, "interaction_id", "decision_id", "resolution"),
		tool("scos_control_status_get", "Get lifecycle status for an interaction", map[string]any{
			"interaction_id": strField("Interaction ID"),
		}, "interaction_id"),
		tool("scos_control_trigger", "Submit a trigger event to the control plane", map[string]any{
			"interaction_id": strField("Interaction ID"),
			"input":          strField("Input text"),
		}, "interaction_id", "input"),
	)

	// ── backup-manager ───────────────────────────────────────────────────
	tools = append(tools,
		tool("scos_backup_list", "List backups", map[string]any{}),
		tool("scos_backup_get", "Get a backup by ID", map[string]any{"id": strField("Backup ID")}, "id"),
		tool("scos_backup_start", "Take a backup, verify it as stored, then apply retention: base_backup (pg_dump archive plus a data fingerprint of the same snapshot) physical_base_backup (pg_basebackup for point-in-time recovery; needs working WAL archiving) or configuration_backup (the deployment's configuration files, SCOS_CONFIG_BACKUP_PATHS); requires backups:admin", map[string]any{"type": strField("base_backup (default), physical_base_backup or configuration_backup")}),
		tool("scos_backup_complete", "Re-verify a stored backup against its manifest (backups are completed when taken); requires backups:admin", map[string]any{
			"backup_id": strField("Backup ID"),
		}, "backup_id"),
		tool("scos_restore_test_list", "List restore-test results", map[string]any{}),
		tool("scos_restore_test_start", "Run a restore drill to completion: automated_integrity (archive and table of contents), monthly_restore (restore and validate key tables), quarterly_full_recovery (restore and validate every table and the schema) or pitr_recovery (replay archived WAL onto a physical base backup up to a fresh restore point); requires backups:admin", map[string]any{
			"test_type": strField("automated_integrity, monthly_restore, quarterly_full_recovery or pitr_recovery"),
			"backup_id": strField("Backup ID; defaults to the newest verified backup"),
		}, "test_type"),
		tool("scos_restore_test_complete", "Return a recorded restore drill result (drills run to completion when started); requires backups:admin", map[string]any{
			"test_id": strField("Restore test ID"),
		}, "test_id"),
		tool("scos_restore_cadence", "Check restore-test cadence status against §31 targets, each from passing drills of its own type", map[string]any{}),
		tool("scos_backup_plan", "Get §31 backup coverage, each item derived from current evidence with the reason when not covered", map[string]any{}),
		tool("scos_backup_retention", "Get the retention policy", map[string]any{}),
		tool("scos_okf_mirror_status", "Get the last OKF repository mirror run: remote, refs pushed and verified, head, and why it failed if it did", map[string]any{}),
		tool("scos_okf_mirror_push", "Mirror the OKF Git repository to the configured remote now (git push --mirror, then every ref read back with git ls-remote); requires backups:admin", map[string]any{}),
	)

	// ── compatibility-gateway ────────────────────────────────────────────
	tools = append(tools,
		tool("scos_adapter_list", "List compatibility adapters", map[string]any{}),
		tool("scos_adapter_get", "Get a compatibility adapter by boundary", map[string]any{
			"boundary": strField("Adapter boundary"),
		}, "boundary"),
		tool("scos_compat_status", "Get overall compatibility status", map[string]any{}),
		tool("scos_compat_agent_list", "List agent instances known to the compatibility gateway", map[string]any{}),
		tool("scos_compat_agent_register", "Register an agent instance with the compatibility gateway", map[string]any{
			"agent_id": strField("Agent ID"),
			"name":     strField("Agent name"),
			"boundary": strField("Adapter boundary"),
		}, "agent_id"),
		tool("scos_compat_agent_get", "Get an agent instance by ID", map[string]any{"id": strField("Agent ID")}, "id"),
		tool("scos_envelope_get", "Get a context envelope by session", map[string]any{
			"session_id": strField("Session ID"),
		}, "session_id"),
		tool("scos_envelope_set", "Create or update a context envelope", map[string]any{
			"agent_id":               strField("Agent ID"),
			"session_id":             strField("Session ID"),
			"governing_constraints":  strField("Governing constraints"),
			"user_model":             strField("User model summary"),
			"facts":                  arrField(strField(""), "Facts"),
			"learnings":              arrField(strField(""), "Learnings"),
			"task_state":             strField("Task state"),
			"episodic_history":       arrField(strField(""), "Episodic history"),
			"contradictions":         arrField(strField(""), "Contradictions"),
			"code_references":        arrField(strField(""), "Code references"),
			"artifacts":              arrField(strField(""), "Artifacts"),
			"sources":                arrField(strField(""), "Sources"),
			"restoration_references": arrField(strField(""), "Restoration references"),
			"format":                 strField("markdown, json, or text (default json)"),
		}, "agent_id", "session_id"),
		tool("scos_envelope_render", "Render a stored context envelope in a target format. The response's format is the format actually produced; an unsupported format is refused", map[string]any{
			"session_id":    strField("Session ID"),
			"target_format": strField("markdown, text, or json (omit to use the envelope's stored format)"),
			"agent_id":      strField("Agent ID"),
		}, "session_id"),
		tool("scos_translate", "Translate one event between wire formats through the SCOS common event model. Unsupported formats are refused; an event the target format cannot express is refused rather than approximated", map[string]any{
			"event_type":    strField("Optional: message, tool_call, or tool_result; must match the source event"),
			"source_data":   objField("Source event: an OpenAI chat message, an Anthropic message, an MCP tools/call request or result frame, or a scos_common event"),
			"source_format": strField("openai, anthropic, mcp, or scos_common"),
			"target_format": strField("openai, anthropic, mcp, or scos_common (default scos_common)"),
		}, "source_format", "source_data"),
		tool("scos_compat_conformance", "Report the conformance run each adapter boundary's tested flag is derived from", map[string]any{
			"boundary": strField("Optional adapter boundary"),
		}),
	)

	// ── context-optimisation ─────────────────────────────────────────────
	tools = append(tools,
		tool("scos_context_optimize_input", "Compress and optimise input context into a capsule", map[string]any{
			"input":               strField("Raw input text"),
			"task_type":           strField("Task type"),
			"sensitivity":         strField("Sensitivity level"),
			"risk_class":          strField("Risk class"),
			"profile":             strField("Compression profile: exact, conservative, balanced, aggressive or adaptive"),
			"token_budget":        intField("Token budget (0 selects 8000)"),
			"non_droppable_hints": arrField(strField(""), "Hints that must not be dropped"),
			"code_symbols":        arrField(strField(""), "Code symbols that occur in the input and must be kept verbatim"),
			"memory_references":   arrField(strField(""), "Memory IDs that occur in the input and must be kept verbatim"),
		}, "input"),
		tool("scos_context_optimize_output", "Compress and optimise a raw model output", map[string]any{
			"raw_output": strField("Raw output text"),
			"format":     strField("Output format: standard or compact"),
			"max_tokens": intField("Max tokens (0 selects 2000)"),
		}, "raw_output"),
		tool("scos_context_compile", "Compile input context into a capsule (alias of scos_context_optimize_input)", map[string]any{
			"input":               strField("Raw input text"),
			"task_type":           strField("Task type"),
			"sensitivity":         strField("Sensitivity level"),
			"risk_class":          strField("Risk class"),
			"profile":             strField("Compression profile: exact, conservative, balanced, aggressive or adaptive"),
			"token_budget":        intField("Token budget (0 selects 8000)"),
			"non_droppable_hints": arrField(strField(""), "Hints that must not be dropped"),
			"code_symbols":        arrField(strField(""), "Code symbols that occur in the input and must be kept verbatim"),
			"memory_references":   arrField(strField(""), "Memory IDs that occur in the input and must be kept verbatim"),
		}, "input"),
		tool("scos_context_restore", "Restore the authoritative source of a context capsule by its capsule_id", map[string]any{
			"capsule_id": strField("Capsule id returned by scos_context_compile or scos_context_optimize_output"),
			"capsule":    objField("Full context capsule; its source objects must match the recorded capsule"),
		}),
		tool("scos_context_evaluate", "Validate that a compiled context is faithful to its source", map[string]any{
			"input":               strField("Original input text"),
			"compiled_text":       strField("Compiled/compressed text"),
			"capsule":             objField("Context capsule to cross-check against"),
			"non_droppable_hints": arrField(strField(""), "Hints that must not be dropped"),
		}, "input", "compiled_text"),
	)

	// ── execution-proxy ──────────────────────────────────────────────────
	tools = append(tools,
		tool("scos_exec_plan", "Plan (and optionally explain) a command execution without running it", map[string]any{
			"tenant_id":         strField("Tenant ID"),
			"principal_id":      strField("Principal ID"),
			"agent_id":          strField("Agent ID"),
			"operation_id":      strField("Governed operation ID"),
			"command":           strField("Command to plan"),
			"working_directory": strField("Working directory"),
			"allow_rewrite":     boolField("Allow safety rewrites"),
			"risk_tolerance":    strField("exact, conservative, or normal"),
			"mode":              strField("bypass, observe, safe, adaptive, or forensic"),
			"dry_run":           boolField("Dry run"),
			"explain":           boolField("Include rule explanations"),
		}, "command"),
		tool("scos_exec_execute", "Execute a governed command through the execution proxy. Commands that need confirmation (destructive commands) are refused here: route them with scos_control_route and answer with scos_control_confirm", map[string]any{
			"tenant_id":         strField("Tenant ID"),
			"principal_id":      strField("Principal ID"),
			"agent_id":          strField("Agent ID"),
			"operation_id":      strField("Governed operation ID"),
			"command":           strField("Command to execute"),
			"working_directory": strField("Working directory"),
			"allow_rewrite":     boolField("Allow safety rewrites"),
			"risk_tolerance":    strField("exact, conservative, or normal"),
			"mode":              strField("bypass, observe, safe, adaptive, or forensic"),
			"dry_run":           boolField("Dry run"),
			"explain":           boolField("Include rule explanations"),
		}, "command"),
		tool("scos_exec_results_list", "List recent execution results", map[string]any{}),
		tool("scos_exec_result_get", "Get an execution result by operation ID", map[string]any{
			"operation_id": strField("Operation ID"),
		}, "operation_id"),
		tool("scos_exec_restore", "Restore raw (unredacted) output for an execution by restoration handle", map[string]any{
			"handle": strField("64-character hex restoration handle"),
		}, "handle"),
		tool("scos_exec_explain", "Explain how a command would be planned, without executing it", map[string]any{
			"tenant_id":    strField("Tenant ID"),
			"principal_id": strField("Principal ID"),
			"command":      strField("Command to explain"),
			"mode":         strField("bypass, observe, safe, adaptive, or forensic"),
		}, "command"),
		tool("scos_exec_rules_list", "List command rewrite/redaction rules", map[string]any{}),
		tool("scos_exec_rule_create", "Add a command rewrite rule for your tenant's commands (it rewrites the command before it runs; it never applies to destructive commands and is dropped if the rewrite would raise the command's risk)", map[string]any{
			"pattern":     strField("Regular expression matched against the command; its first match is replaced"),
			"replacement": strField("Replacement text; may use capture groups ($1, ${name})"),
			"unless":      strField("Regular expression; the rule does not apply to a command it matches"),
			"risk_class":  strField("read, test or write: the rule applies only to commands graded at this risk (default read)"),
			"lossless":    boolField("The rewritten command does the same work and keeps all of the original's output; safe mode applies only lossless rules"),
			"description": strField("Rule description"),
		}, "pattern", "replacement"),
	)

	// ── extension-manager ────────────────────────────────────────────────
	tools = append(tools,
		tool("scos_extension_list", "List installed extensions", map[string]any{}),
		tool("scos_extension_get", "Get an installed extension by package ID", map[string]any{"id": strField("Package ID")}, "id"),
		tool("scos_skill_list", "List available skills (an extension's skills are available only while it is enabled)", map[string]any{}),
		tool("scos_skill_create", "Register a skill", map[string]any{
			"name":                  strField("Skill name"),
			"version":               strField("Skill version"),
			"purpose":               strField("Skill purpose"),
			"activation_conditions": strField("Activation conditions"),
			"capabilities":          arrField(strField(""), "Capabilities"),
			"required_tools":        arrField(strField(""), "Required tools"),
			"permissions":           arrField(strField(""), "Permissions"),
			"status":                strField("Status"),
			"token_budget":          intField("Token budget"),
		}, "name"),
		tool("scos_skill_get", "Get a skill by ID", map[string]any{"id": strField("Skill ID")}, "id"),
		tool("scos_hook_list", "List registered hooks", map[string]any{}),
		tool("scos_hook_dispatch", "Dispatch a hook event to the enabled extensions subscribed to it; a package with an active canary is routed to the candidate version for callers in its cohort", map[string]any{
			"event":        strField("Hook event name"),
			"tenant_id":    strField("Tenant ID"),
			"principal_id": strField("Principal ID"),
			"payload":      objField("Event payload"),
			"agent_id":     strField("Caller agent for canary routing (an agent token's own id is used and cannot be overridden)"),
			"profile_id":   strField("Caller profile for canary routing"),
			"workspace_id": strField("Caller workspace for canary routing"),
			"project_id":   strField("Caller project for canary routing"),
		}, "event"),
		tool("scos_hook_get", "List hooks subscribed to an event", map[string]any{"event": strField("Hook event name")}, "event"),
		tool("scos_extension_install", "Install an extension from its manifest (operators only: requires the extensions:admin scope, because an extension's hooks receive the events of every tenant)", map[string]any{
			"schema_version":        strField("Manifest schema version"),
			"package_id":            strField("Package ID"),
			"name":                  strField("Extension name"),
			"version":               strField("Extension version"),
			"publisher":             strField("Publisher"),
			"license":               strField("License"),
			"extension_types":       arrField(strField(""), "plugin, skill, hook, agent_adapter, provider, ui_extension"),
			"supported_platforms":   arrField(strField(""), "Supported platforms"),
			"required_core_version": strField("Required core version"),
			"dependencies":          arrField(objField(""), "Package dependencies"),
			"isolation":             strField("Must be remote_endpoint: the extension manager never runs extension code, hooks are delivered to the extension's own endpoint"),
			"capabilities_provided": arrField(strField(""), "Capabilities provided"),
			"capabilities_consumed": arrField(strField(""), "Capabilities consumed"),
			"hooks_subscribed":      arrField(strField(""), "Hooks subscribed"),
			"hook_endpoints":        objField("Map of each subscribed hook event to the HTTPS endpoint it is delivered to"),
			"skills_exported":       arrField(strField(""), "Skills exported"),
			"permissions":           arrField(strField(""), "Permissions"),
			"rollback_plan":         strField("Rollback plan"),
			"content_hash":          strField("sha256:<hex> of the manifest JSON with content_hash and signature empty; verified on install and enable"),
			"signature":             strField("Manifest signature"),
		}, "package_id", "version"),
		tool("scos_extension_install_dryrun", "Validate an extension manifest and report whether installing it would be accepted, without applying it", map[string]any{
			"schema_version":        strField("Manifest schema version"),
			"package_id":            strField("Package ID"),
			"name":                  strField("Extension name"),
			"version":               strField("Extension version"),
			"publisher":             strField("Publisher"),
			"license":               strField("License"),
			"extension_types":       arrField(strField(""), "plugin, skill, hook, agent_adapter, provider, ui_extension"),
			"supported_platforms":   arrField(strField(""), "Supported platforms"),
			"required_core_version": strField("Required core version"),
			"dependencies":          arrField(objField(""), "Package dependencies"),
			"isolation":             strField("Must be remote_endpoint: the extension manager never runs extension code, hooks are delivered to the extension's own endpoint"),
			"capabilities_provided": arrField(strField(""), "Capabilities provided"),
			"capabilities_consumed": arrField(strField(""), "Capabilities consumed"),
			"hooks_subscribed":      arrField(strField(""), "Hooks subscribed"),
			"hook_endpoints":        objField("Map of each subscribed hook event to the HTTPS endpoint it is delivered to"),
			"skills_exported":       arrField(strField(""), "Skills exported"),
			"permissions":           arrField(strField(""), "Permissions"),
			"rollback_plan":         strField("Rollback plan"),
			"content_hash":          strField("sha256:<hex> of the manifest JSON with content_hash and signature empty; verified on install and enable"),
			"signature":             strField("Manifest signature"),
		}, "package_id", "version"),
		tool("scos_extension_receipts", "List extension installation receipts", map[string]any{}),
		tool("scos_extension_capabilities", "List capabilities exposed by installed extensions", map[string]any{}),
	)

	// ── memory-gateway ───────────────────────────────────────────────────
	tools = append(tools,
		tool("scos_mg_session_create", "Create a new journalled session", map[string]any{
			"namespace_id": strField("Namespace ID"),
		}, "namespace_id"),
		tool("scos_mg_message_append", "Append a message to a session", map[string]any{
			"session_id": strField("Session ID"),
			"role":       strField("Message role"),
			"content":    strField("Message content"),
		}, "session_id", "role", "content"),
		tool("scos_mg_chunk_append", "Append a streamed response chunk to a turn", map[string]any{
			"turn_id":        strField("Turn ID"),
			"message_id":     strField("Message ID"),
			"chunk_sequence": intField("Chunk sequence number"),
			"content":        strField("Chunk content"),
		}, "turn_id", "message_id", "content"),
		tool("scos_mg_response_start", "Mark the start of a turn's response", map[string]any{
			"turn_id": strField("Turn ID"),
		}, "turn_id"),
		tool("scos_mg_turns_list", "List recent turns", map[string]any{}),
		tool("scos_mg_turn_complete", "Complete a turn", map[string]any{
			"turn_id": strField("Turn ID"),
			"content": strField("Final response content"),
			"state":   objField("Task state to checkpoint"),
		}, "turn_id", "content"),
		tool("scos_mg_outbox_status", "Get outbox delivery status", map[string]any{}),
		tool("scos_mg_outbox_dead_letters", "List outbox dead letters", map[string]any{}),
	)

	// ── okf-synchroniser ─────────────────────────────────────────────────
	tools = append(tools,
		tool("scos_okf_sync", "Synchronise the OKF index against the governance repository", map[string]any{}),
		tool("scos_okf_document_list", "List indexed OKF documents", map[string]any{}),
		tool("scos_okf_document_get", "Get an OKF document by ID", map[string]any{"id": strField("Document ID")}, "id"),
		tool("scos_okf_projections", "List OKF index projections", map[string]any{}),
		tool("scos_okf_divergence", "List OKF documents that have diverged from their source", map[string]any{}),
		tool("scos_okf_proposal_list", "List OKF change proposals", map[string]any{}),
		// okf-synchroniser takes proposer and reviewer identity from the
		// forwarded bearer token's claims only, so neither is a tool argument;
		// Role merely selects among governance roles the caller already holds.
		tool("scos_okf_proposal_create", "Create an OKF change proposal; the authenticated caller is the proposer", map[string]any{
			"Path":    strField("Document path"),
			"Content": strField("Proposed content"),
		}, "Path", "Content"),
		tool("scos_okf_proposal_action", "Act on an OKF proposal: validate, diff, review, approve, or reject (as the authenticated caller, who must not be the proposer), or merge", map[string]any{
			"id":     strField("Proposal ID"),
			"action": strField("validate, diff, review, approve, reject, or merge"),
			"Role":   strField("Governance role to act under, which the caller must hold (optional, for review/approve/reject)"),
			"Reason": strField("Reason (for review/approve/reject)"),
		}, "id", "action"),
	)

	// ── recovery-orchestrator ────────────────────────────────────────────
	tools = append(tools,
		tool("scos_recovery_start", "Start a recovery run for a service", map[string]any{
			"service_id": strField("Service ID"),
		}),
		tool("scos_recovery_checklist_update", "Update a step in a recovery checklist", map[string]any{
			"service_id": strField("Service ID"),
			"step":       intField("Checklist step number"),
			"status":     strField("pending, verifying, ok, failed, or skipped"),
			"detail":     strField("Detail"),
		}, "service_id", "step", "status"),
		tool("scos_recovery_complete", "Mark a recovery run complete", map[string]any{
			"service_id": strField("Service ID"),
		}, "service_id"),
		tool("scos_recovery_status_list", "List recovery statuses for all services", map[string]any{}),
		tool("scos_recovery_status_get", "Get recovery status for a service", map[string]any{
			"id": strField("Service ID"),
		}, "id"),
		tool("scos_recovery_interrupted_turns", "List all interrupted turns across recovery runs", map[string]any{}),
	)

	// ── retrieval-orchestrator ───────────────────────────────────────────
	tools = append(tools,
		tool("scos_retrieve", "Retrieve and assemble relevant context for a query", map[string]any{
			"tenant_id":       strField("Optional; taken from your credentials, and must match them if given"),
			"principal_id":    strField("Optional; taken from your credentials, and must match them if given"),
			"query":           strField("Retrieval query"),
			"agent_id":        strField("Agent ID"),
			"session_id":      strField("Session ID"),
			"turn_id":         strField("Turn ID"),
			"namespaces":      arrField(strField(""), "Namespaces to search"),
			"memory_classes":  arrField(strField(""), "Only return memories of these classes (semantic, episodic, working, procedural, governing, ...)"),
			"sensitivity_max": strField("Max sensitivity to include (default internal)"),
			"max_results":     intField("Max results"),
			"token_budget":    intField("Token budget"),
			"include_recent":  boolField("Also include memories recorded in the last 7 days, whatever the query"),
			"include_session": boolField("Also include working and episodic memories of session_id"),
			"include_state":   boolField("Also include active working memories (task state)"),
			"repository_id":   strField("Durable code repository UUID; with branch, enables P4 code retrieval alongside memory"),
			"branch":          strField("Branch for durable code retrieval"),
		}, "query"),
	)

	// ── security-gate ────────────────────────────────────────────────────
	tools = append(tools,
		tool("scos_auth_validate", "Validate an auth token", map[string]any{"token": strField("Bearer token")}, "token"),
		// Token issuance (security-gate /v1/auth/issue) is deliberately not a
		// tool: it is authorised by the fabric's issuer secret, which mints
		// any identity, and an MCP tool argument lands in agent transcripts
		// and model context. Tokens are issued by the operator or the cloud
		// control plane, never through this gateway.
		tool("scos_auth_revoke", "Revoke an auth token", map[string]any{"token": strField("Bearer token")}, "token"),
		tool("scos_permission_check", "Check whether a principal may perform an action on a resource; decided against the principal's grants (or a delegation and its delegator's grants), denying unknown data classes", map[string]any{
			"principal":           strField("Principal"),
			"action":              strField("Action"),
			"resource":            strField("Resource"),
			"data_class":          strField("Data classification: public, internal, confidential, restricted or secret"),
			"tenant":              strField("Optional; taken from your credentials, and must match them if given"),
			"delegated_authority": strField("Delegation UUID issued to the principal; the check is then decided against that delegation and its delegator's grants"),
			"expires_at":          strField("RFC3339 time until which the permission is needed; the covering grant must remain valid that long"),
			"trace_id":            strField("Trace ID"),
		}, "principal", "action", "resource"),
		tool("scos_permission_classify", "Classify content's data sensitivity", map[string]any{
			"content": strField("Content to classify"),
		}, "content"),
		tool("scos_audit_list", "List audit log entries visible to the caller", map[string]any{}),
		tool("scos_audit_get", "Get an audit log entry by ID", map[string]any{"id": strField("Audit entry ID")}, "id"),
		tool("scos_audit_record", "Append a caller annotation to the audit log. The entry is attributed to the authenticated caller and stored as action caller_annotation / decision recorded; the fields below are kept under caller_annotation as unverified assertions, never as audited facts", map[string]any{
			"action":     strField("Action the caller reports (stored as caller_annotation.action)"),
			"principal":  strField("Optional; must equal the authenticated principal or the call is refused"),
			"agent_id":   strField("Optional; must equal the authenticated agent or the call is refused"),
			"tenant":     strField("Optional; must equal the authenticated tenant or the call is refused"),
			"session_id": strField("Session ID the caller reports"),
			"resource":   strField("Resource the annotation is about"),
			"result":     strField("Outcome the caller reports (stored as caller_annotation.result)"),
			"data_class": strField("Data classification the caller reports"),
			"reason":     strField("Reason the caller reports"),
			"trace_id":   strField("Trace ID the caller reports"),
		}, "action", "resource"),
		tool("scos_secret_list", "List secret references (values are never returned)", map[string]any{}),
		tool("scos_secret_create", "Create or update a secret reference (values are never returned)", map[string]any{
			"id":         strField("Secret reference ID"),
			"name":       strField("Secret name"),
			"provider":   strField("env, vault, aws-secrets, gcp-sm, or securestore"),
			"ref":        strField("Provider-specific reference"),
			"scope":      strField("Who may resolve the value: comma-separated tenant, principal:<id> and agent:<id> selectors; empty means never resolvable"),
			"data_class": strField("Sensitivity of the value: public, internal, confidential, restricted or secret (default secret); resolving requires a data_class:<class> claims scope at or above it"),
		}, "id", "name", "provider", "ref"),
		tool("scos_secret_get", "Get a secret reference by ID (value is never returned)", map[string]any{
			"id": strField("Secret reference ID"),
		}, "id"),
	)

	// ── tool-governance ──────────────────────────────────────────────────
	tools = append(tools,
		tool("scos_tool_list", "List governed tool definitions", map[string]any{}),
		tool("scos_tool_get", "Get a governed tool definition by ID", map[string]any{"id": strField("Tool ID")}, "id"),
		tool("scos_tool_register", "Register a governed tool in state proposed; requires the tools:admin scope (requires Idempotency-Key header)", map[string]any{
			"name":        strField("Tool name"),
			"description": strField("Tool description"),
			"version":     strField("Tool version"),
			"properties":  objField("Tool properties (read_only, destructive, idempotent, externally_side_effecting, confirmation_gated, dry_run_capable, reconcilable)"),
		}, "name"),
		tool("scos_tool_transition", "Transition a governed tool's lifecycle state (proposed -> prepared -> authorised); requires the tools:admin scope (requires Idempotency-Key header)", map[string]any{
			"tool_id":   strField("Tool ID"),
			"new_state": strField("New lifecycle state"),
		}, "tool_id", "new_state"),
		tool("scos_operation_list", "List governed tool operations", map[string]any{}),
		tool("scos_operation_get", "Get a governed tool operation by ID", map[string]any{"id": strField("Operation ID")}, "id"),
		tool("scos_operation_start", "Start a governed operation for a registered, authorised tool, named by tool_id or tool_name; the risk and plan are checked against the tool's declared properties (requires Idempotency-Key header). A destructive or confirmation-gated operation is refused here; route it with scos_control_route and confirm it with scos_control_confirm", map[string]any{
			"tool_id":      strField("Tool ID (or give tool_name)"),
			"tool_name":    strField("Tool name (or give tool_id)"),
			"operation_id": strField("Operation ID (UUID); required for a side-effecting operation on a non-idempotent tool"),
			"input":        objField("Operation input"),
			"risk":         strField("Declared risk: read, test, write, or destructive (required unless the plan names a command)"),
			"plan":         objField("Operation plan (original_request, executed_request, working_directory, risk_level, mode, dry_run)"),
		}),
		tool("scos_operation_prepare", "Prepare a governed operation for a registered, authorised tool or a built-in tool; unknown tools are refused, and the risk and plan are checked against the tool's declared properties (requires Idempotency-Key header). A destructive or confirmation-gated operation is refused here; route it with scos_control_route and confirm it with scos_control_confirm", map[string]any{
			"tool_name":    strField("Tool name"),
			"operation_id": strField("Operation ID (UUID); required for a side-effecting operation on a non-idempotent tool"),
			"input":        objField("Operation input"),
			"risk":         strField("Declared risk: read, test, write, or destructive (required unless the plan names a command)"),
			"plan":         objField("Operation plan (original_request, executed_request, working_directory, risk_level, mode, dry_run)"),
		}, "tool_name"),
		tool("scos_operation_complete", "Complete a governed operation (requires Idempotency-Key header)", map[string]any{
			"operation_id": strField("Operation ID"),
			"result":       strField("succeeded, failed, or unknown"),
			"output":       objField("Operation output"),
			"error":        strField("Error detail"),
		}, "operation_id", "result"),
		tool("scos_operation_reconcile", "Reconcile a governed operation left in an unknown state (requires Idempotency-Key header)", map[string]any{
			"operation_id": strField("Operation ID"),
			"result":       strField("Result to reconcile to"),
			"output":       objField("Operation output"),
		}, "operation_id"),
	)

	// ── credential-vault ─────────────────────────────────────────────────
	tools = append(tools,
		tool("scos_credential_list", "List credentials the caller can read or write (metadata only, values are never included)", map[string]any{}),
		tool("scos_credential_create", "Store a new encrypted credential (API key, password, token) for the tenant's agents to share", map[string]any{
			"name":  strField("Credential name, unique per tenant"),
			"value": strField("Secret value to encrypt and store"),
			"acl":   objField("Optional extra grants: list of {principal_id, permission} where permission is read or write. The creator always receives both automatically."),
		}, "name", "value"),
		tool("scos_credential_get", "Read a credential's decrypted value (requires read permission on this credential)", map[string]any{
			"id": strField("Credential ID"),
		}, "id"),
		tool("scos_credential_update", "Rotate a credential's value (requires write permission on this credential)", map[string]any{
			"id":    strField("Credential ID"),
			"value": strField("New secret value to encrypt and store"),
		}, "id", "value"),
		tool("scos_credential_delete", "Delete a credential permanently (requires write permission on this credential)", map[string]any{
			"id": strField("Credential ID"),
		}, "id"),
		tool("scos_credential_acl_grant", "Grant another principal read or write access to a credential (requires write permission on this credential)", map[string]any{
			"id":           strField("Credential ID"),
			"principal_id": strField("Principal (agent) ID to grant access to"),
			"permission":   strField("read or write"),
		}, "id", "principal_id", "permission"),
		tool("scos_credential_acl_revoke", "Revoke a principal's read or write access to a credential (requires write permission on this credential; refused if it would remove the last write holder)", map[string]any{
			"id":           strField("Credential ID"),
			"principal_id": strField("Principal (agent) ID to revoke access from"),
			"permission":   strField("read or write"),
		}, "id", "principal_id", "permission"),
	)

	// ── transparency-projection ──────────────────────────────────────────
	tools = append(tools,
		tool("scos_transparency_snapshot", "Get a consolidated transparency dashboard snapshot", map[string]any{}),
	)

	// ── observability ────────────────────────────────────────────────────
	tools = append(tools,
		tool("scos_metrics_list", "List all recorded metric points", map[string]any{}),
		tool("scos_metrics_ingest", "Ingest metric points", map[string]any{
			"points": arrField(objField("Metric point"), "Metric points to ingest"),
		}, "points"),
		tool("scos_metrics_query", "Query metric points by name, service, and time", map[string]any{
			"name":    strField("Metric name filter"),
			"service": strField("Service filter"),
			"since":   strField("RFC3339 timestamp lower bound"),
		}),
		tool("scos_traces_list", "List all recorded trace spans", map[string]any{}),
		tool("scos_trace_ingest", "Ingest trace spans", map[string]any{
			"spans": arrField(objField("Trace span"), "Trace spans to ingest"),
		}, "spans"),
		tool("scos_trace_get", "Get a trace by ID", map[string]any{"id": strField("Trace ID")}, "id"),
		tool("scos_logs_list", "List all recorded log entries", map[string]any{}),
		tool("scos_log_ingest", "Ingest log entries", map[string]any{
			"entries": arrField(objField("Log entry"), "Log entries to ingest"),
		}, "entries"),
		tool("scos_observability_targets", "Get observability SLO targets", map[string]any{}),
		tool("scos_observability_export_status", "Get OTLP export configuration status", map[string]any{}),
		tool("scos_benchmarks", "Report recorded benchmark runs: the latest run per suite and a newest-first run history, as published to observability by the benchmark harnesses (benchmarks/run_deployed.py, benchmarks/run_acceptance.py). This tool does not run benchmarks; it returns an empty history when none has been published.", map[string]any{
			"suite": strField("Only runs of this suite"),
			"limit": intField("Maximum history entries, 1-500 (default 100)"),
		}),
	)

	return tools
}
