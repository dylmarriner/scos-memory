package main

import "github.com/dylmarriner/scos-memory/packages/telemetry"

import "github.com/dylmarriner/scos-memory/packages/release"

import "github.com/dylmarriner/scos-memory/packages/auth"
import "github.com/dylmarriner/scos-memory/packages/tlsserve"

import (
	"context"
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
	client                    *http.Client
	startTime                 time.Time
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
		if durableID := stringArg(args, "durable_repository_id"); durableID != "" {
			branch := stringArg(args, "branch")
			commitID := stringArg(args, "commit_id")
			files, ok := args["files"].([]any)
			if branch == "" || commitID == "" || !ok || len(files) == 0 {
				return nil, fmt.Errorf("durable code index requires branch, commit_id, and structured files")
			}
			for _, file := range files {
				if _, ok := file.(map[string]any); !ok {
					return nil, fmt.Errorf("durable code index files must be objects with path and content_hash")
				}
			}
			payload := map[string]any{"repository_id": durableID, "branch": branch, "commit_id": commitID, "files": files}
			if value := stringArg(args, "extractor_version"); value != "" {
				payload["extractor_version"] = value
			}
			if value := stringArg(args, "configuration_hash"); value != "" {
				payload["configuration_hash"] = value
			}
			return s.forwardPOST(ctx, r, s.codeIntelligenceURL+"/v2/code/index", payload)
		}
		return s.forwardPOST(ctx, r, s.codeIntelligenceURL+"/v1/index", args)
	case "scos_code_query":
		if durableID := stringArg(args, "durable_repository_id"); durableID != "" {
			branch := stringArg(args, "branch")
			if branch == "" {
				return nil, fmt.Errorf("branch is required with durable_repository_id")
			}
			payload := map[string]any{"repository_id": durableID, "branch": branch, "query": stringArg(args, "query")}
			if maxResults, ok := args["max_results"]; ok {
				payload["max_results"] = maxResults
			}
			return s.forwardPOST(ctx, r, s.codeIntelligenceURL+"/v2/code/query", payload)
		}
		return s.forwardPOST(ctx, r, s.codeIntelligenceURL+"/v1/query", args)
	case "scos_code_impact":
		return s.forwardPOST(ctx, r, s.codeIntelligenceURL+"/v1/impact", args)
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
	case "scos_correct":
		memoryID := stringArg(args, "memory_id")
		if memoryID == "" {
			return nil, fmt.Errorf("memory_id is required")
		}
		return s.forwardPATCH(ctx, r, s.memoryIntelligenceURL+"/v1/facts/"+memoryID, map[string]any{
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
		return s.forwardPOST(ctx, r, s.memoryIntelligenceURL+"/v1/facts/"+memoryID+"/feedback", map[string]any{
			"feedback": stringArg(args, "feedback"),
			"reason":   stringArg(args, "reason"),
		})
	case "scos_fact_history":
		memoryID := stringArg(args, "memory_id")
		if memoryID == "" {
			return nil, fmt.Errorf("memory_id is required")
		}
		return s.forwardGET(ctx, r, s.memoryIntelligenceURL+"/v1/facts/"+memoryID+"/history")
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
		return s.forwardGET(ctx, r, s.agentRegistryURL+"/v1/agents/"+stringArg(args, "id"))
	case "scos_agent_register":
		return s.forwardPOST(ctx, r, s.agentRegistryURL+"/v1/agents/register", args)
	case "scos_agent_heartbeat":
		return s.forwardPOST(ctx, r, s.agentRegistryURL+"/v1/agents/heartbeat", args)
	case "scos_handoff_list":
		return s.forwardGET(ctx, r, s.agentRegistryURL+"/v1/handoffs")
	case "scos_handoff_get":
		return s.forwardGET(ctx, r, s.agentRegistryURL+"/v1/handoffs/"+stringArg(args, "id"))
	case "scos_handoff_initiate":
		return s.forwardPOST(ctx, r, s.agentRegistryURL+"/v1/handoffs/initiate", args)
	case "scos_handoff_respond":
		return s.forwardPOST(ctx, r, s.agentRegistryURL+"/v1/handoffs/respond", args)

	// ── autonomous-control-plane ─────────────────────────────────────────
	case "scos_control_decision_get":
		return s.forwardGET(ctx, r, s.controlPlaneURL+"/v1/control/decisions/"+stringArg(args, "interaction_id"))
	case "scos_control_route":
		return s.forwardPOST(ctx, r, s.controlPlaneURL+"/v1/control/route", args)
	case "scos_control_ingest":
		return s.forwardPOST(ctx, r, s.controlPlaneURL+"/v1/control/ingest", args)
	case "scos_control_replay":
		return s.forwardPOST(ctx, r, s.controlPlaneURL+"/v1/control/replay", args)
	case "scos_control_recover":
		return s.forwardPOST(ctx, r, s.controlPlaneURL+"/v1/control/recover", args)
	case "scos_control_status_get":
		return s.forwardGET(ctx, r, s.controlPlaneURL+"/v1/control/status/"+stringArg(args, "interaction_id"))
	case "scos_control_trigger":
		return s.forwardPOST(ctx, r, s.controlPlaneURL+"/v1/triggers", args)

	// ── backup-manager ───────────────────────────────────────────────────
	case "scos_backup_list":
		return s.forwardGET(ctx, r, s.backupManagerURL+"/v1/backups")
	case "scos_backup_get":
		return s.forwardGET(ctx, r, s.backupManagerURL+"/v1/backups/"+stringArg(args, "id"))
	case "scos_backup_start":
		return s.forwardPOST(ctx, r, s.backupManagerURL+"/v1/backups/start", args)
	case "scos_backup_complete":
		return s.forwardPOST(ctx, r, s.backupManagerURL+"/v1/backups/complete", args)
	case "scos_restore_test_list":
		return s.forwardGET(ctx, r, s.backupManagerURL+"/v1/restore-tests")
	case "scos_restore_test_start":
		return s.forwardPOST(ctx, r, s.backupManagerURL+"/v1/restore-tests/start", args)
	case "scos_restore_test_complete":
		return s.forwardPOST(ctx, r, s.backupManagerURL+"/v1/restore-tests/complete", args)
	case "scos_restore_cadence":
		return s.forwardGET(ctx, r, s.backupManagerURL+"/v1/restore-tests/cadence")
	case "scos_backup_plan":
		return s.forwardGET(ctx, r, s.backupManagerURL+"/v1/plan")
	case "scos_backup_retention":
		return s.forwardGET(ctx, r, s.backupManagerURL+"/v1/retention")

	// ── compatibility-gateway ────────────────────────────────────────────
	case "scos_adapter_list":
		return s.forwardGET(ctx, r, s.compatGatewayURL+"/v1/adapters")
	case "scos_adapter_get":
		return s.forwardGET(ctx, r, s.compatGatewayURL+"/v1/adapters/"+stringArg(args, "boundary"))
	case "scos_compat_status":
		return s.forwardGET(ctx, r, s.compatGatewayURL+"/v1/compatibility")
	case "scos_compat_agent_list":
		return s.forwardGET(ctx, r, s.compatGatewayURL+"/v1/agents")
	case "scos_compat_agent_register":
		return s.forwardPOST(ctx, r, s.compatGatewayURL+"/v1/agents", args)
	case "scos_compat_agent_get":
		return s.forwardGET(ctx, r, s.compatGatewayURL+"/v1/agents/"+stringArg(args, "id"))
	case "scos_envelope_get":
		return s.forwardGET(ctx, r, s.compatGatewayURL+"/v1/envelope?"+queryFromArgs(args, "session_id"))
	case "scos_envelope_set":
		return s.forwardPOST(ctx, r, s.compatGatewayURL+"/v1/envelope", args)
	case "scos_envelope_render":
		return s.forwardPOST(ctx, r, s.compatGatewayURL+"/v1/envelope/render", args)
	case "scos_translate":
		return s.forwardPOST(ctx, r, s.compatGatewayURL+"/v1/translate", args)

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
		return s.forwardPOST(ctx, r, s.executionProxyURL+"/v1/execute", args)
	case "scos_exec_results_list":
		return s.forwardGET(ctx, r, s.executionProxyURL+"/v1/results")
	case "scos_exec_result_get":
		return s.forwardGET(ctx, r, s.executionProxyURL+"/v1/results/"+stringArg(args, "operation_id"))
	case "scos_exec_restore":
		return s.forwardGET(ctx, r, s.executionProxyURL+"/v1/proxy/restore/"+stringArg(args, "handle"))
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
		return s.forwardGET(ctx, r, s.extensionManagerURL+"/v1/extensions/"+stringArg(args, "id"))
	case "scos_skill_list":
		return s.forwardGET(ctx, r, s.extensionManagerURL+"/v1/skills")
	case "scos_skill_create":
		return s.forwardPOST(ctx, r, s.extensionManagerURL+"/v1/skills", args)
	case "scos_skill_get":
		return s.forwardGET(ctx, r, s.extensionManagerURL+"/v1/skills/"+stringArg(args, "id"))
	case "scos_hook_list":
		return s.forwardGET(ctx, r, s.extensionManagerURL+"/v1/hooks")
	case "scos_hook_dispatch":
		return s.forwardPOST(ctx, r, s.extensionManagerURL+"/v1/hooks/dispatch", args)
	case "scos_hook_get":
		return s.forwardGET(ctx, r, s.extensionManagerURL+"/v1/hooks/"+stringArg(args, "event"))
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
		return s.forwardPOST(ctx, r, s.memoryGatewayURL+"/v1/sessions/"+stringArg(args, "session_id")+"/messages", args)
	case "scos_mg_chunk_append":
		return s.forwardPOST(ctx, r, s.memoryGatewayURL+"/v1/turns/"+stringArg(args, "turn_id")+"/chunks", args)
	case "scos_mg_response_start":
		return s.forwardPOST(ctx, r, s.memoryGatewayURL+"/v1/turns/"+stringArg(args, "turn_id")+"/response", args)
	case "scos_mg_turns_list":
		return s.forwardGET(ctx, r, s.memoryGatewayURL+"/v1/turns")
	case "scos_mg_turn_complete":
		return s.forwardPOST(ctx, r, s.memoryGatewayURL+"/v1/turns/"+stringArg(args, "turn_id")+"/complete", args)
	case "scos_mg_operation_prepare":
		return s.forwardPOST(ctx, r, s.memoryGatewayURL+"/v1/operations", args)
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
		return s.forwardGET(ctx, r, s.okfSynchroniserURL+"/v1/documents/"+stringArg(args, "id"))
	case "scos_okf_projections":
		return s.forwardGET(ctx, r, s.okfSynchroniserURL+"/v1/projections")
	case "scos_okf_divergence":
		return s.forwardGET(ctx, r, s.okfSynchroniserURL+"/v1/divergence")
	case "scos_okf_proposal_list":
		return s.forwardGET(ctx, r, s.okfSynchroniserURL+"/v1/proposals")
	case "scos_okf_proposal_create":
		return s.forwardPOST(ctx, r, s.okfSynchroniserURL+"/v1/proposals", args)
	case "scos_okf_proposal_action":
		return s.forwardPOST(ctx, r, s.okfSynchroniserURL+"/v1/proposals/"+stringArg(args, "id")+"/"+stringArg(args, "action"), args)

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
		return s.forwardGET(ctx, r, s.recoveryOrchestratorURL+"/v1/recovery/status/"+stringArg(args, "id"))
	case "scos_recovery_interrupted_turns":
		return s.forwardGET(ctx, r, s.recoveryOrchestratorURL+"/v1/recovery/interrupted-turns")

	// ── retrieval-orchestrator ───────────────────────────────────────────
	case "scos_retrieve":
		return s.forwardPOST(ctx, r, s.retrievalOrchestratorURL+"/v1/retrieve", args)

	// ── security-gate ────────────────────────────────────────────────────
	case "scos_auth_validate":
		return s.forwardPOST(ctx, r, s.securityGateURL+"/v1/auth/validate", args)
	case "scos_auth_issue":
		return s.forwardPOST(ctx, r, s.securityGateURL+"/v1/auth/issue", args)
	case "scos_auth_revoke":
		return s.forwardPOST(ctx, r, s.securityGateURL+"/v1/auth/revoke", args)
	case "scos_permission_check":
		return s.forwardPOST(ctx, r, s.securityGateURL+"/v1/permissions/check", args)
	case "scos_permission_classify":
		return s.forwardPOST(ctx, r, s.securityGateURL+"/v1/permissions/classify", args)
	case "scos_audit_list":
		return s.forwardGET(ctx, r, s.securityGateURL+"/v1/audit")
	case "scos_audit_get":
		return s.forwardGET(ctx, r, s.securityGateURL+"/v1/audit/"+stringArg(args, "id"))
	case "scos_audit_record":
		return s.forwardPOST(ctx, r, s.securityGateURL+"/v1/audit/record", args)
	case "scos_secret_list":
		return s.forwardGET(ctx, r, s.securityGateURL+"/v1/secrets")
	case "scos_secret_create":
		return s.forwardPOST(ctx, r, s.securityGateURL+"/v1/secrets", args)
	case "scos_secret_get":
		return s.forwardGET(ctx, r, s.securityGateURL+"/v1/secrets/"+stringArg(args, "id"))

	// ── tool-governance ──────────────────────────────────────────────────
	case "scos_tool_list":
		return s.forwardGET(ctx, r, s.toolGovernanceURL+"/v1/tools")
	case "scos_tool_get":
		return s.forwardGET(ctx, r, s.toolGovernanceURL+"/v1/tools/"+stringArg(args, "id"))
	case "scos_tool_register":
		return s.forwardPOST(ctx, r, s.toolGovernanceURL+"/v1/tools/register", args)
	case "scos_tool_transition":
		return s.forwardPOST(ctx, r, s.toolGovernanceURL+"/v1/tools/transition", args)
	case "scos_operation_list":
		return s.forwardGET(ctx, r, s.toolGovernanceURL+"/v1/operations")
	case "scos_operation_get":
		return s.forwardGET(ctx, r, s.toolGovernanceURL+"/v1/operations/"+stringArg(args, "id"))
	case "scos_operation_start":
		return s.forwardPOST(ctx, r, s.toolGovernanceURL+"/v1/operations/start", args)
	case "scos_operation_prepare":
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
		return s.forwardGET(ctx, r, s.credentialVaultURL+"/v1/credentials/"+stringArg(args, "id"))
	case "scos_credential_update":
		return s.forwardWithBody(ctx, r, http.MethodPut, s.credentialVaultURL+"/v1/credentials/"+stringArg(args, "id"), args)
	case "scos_credential_delete":
		return s.forwardDELETE(ctx, r, s.credentialVaultURL+"/v1/credentials/"+stringArg(args, "id"))
	case "scos_credential_acl_grant":
		return s.forwardPOST(ctx, r, s.credentialVaultURL+"/v1/credentials/"+stringArg(args, "id")+"/acl", args)
	case "scos_credential_acl_revoke":
		return s.forwardDELETE(ctx, r, s.credentialVaultURL+"/v1/credentials/"+stringArg(args, "id")+"/acl/"+stringArg(args, "principal_id")+"?permission="+url.QueryEscape(stringArg(args, "permission")))

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
		return s.forwardGET(ctx, r, s.observabilityURL+"/v1/traces/"+stringArg(args, "id"))
	case "scos_logs_list":
		return s.forwardGET(ctx, r, s.observabilityURL+"/v1/logs")
	case "scos_log_ingest":
		return s.forwardPOST(ctx, r, s.observabilityURL+"/v1/logs/ingest", args["entries"])
	case "scos_observability_targets":
		return s.forwardGET(ctx, r, s.observabilityURL+"/v1/targets")
	case "scos_observability_export_status":
		return s.forwardGET(ctx, r, s.observabilityURL+"/v1/export/status")
	case "scos_benchmarks":
		return s.forwardGET(ctx, r, s.observabilityURL+"/v1/benchmarks")
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

func (s *gatewayServer) forwardPOST(ctx context.Context, r *http.Request, url string, args any) (any, error) {
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
		// MCP tool calls carry no per-call header channel the caller can set,
		// so a caller can never supply Idempotency-Key even though several
		// governed endpoints (memory-gateway, tool-governance) require it.
		// Derive one deterministically from identity+url+body: identical
		// retries of the same logical call collapse to the same key, while
		// distinct calls (different args) still get distinct keys.
		req.Header.Set("Idempotency-Key", deriveIdempotencyKey(req.Header.Get("Authorization"), url, encoded))
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, errUnreachable("code-intelligence unavailable: " + err.Error())
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, errUnreachable("code-intelligence returned " + resp.Status + ": " + strings.TrimSpace(string(body)))
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

type errUnreachable string

func (e errUnreachable) Error() string { return string(e) }

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

// memoryTierToolNames is the curated memory subset documented in every
// desktop-agent plugin bundle (integrations/*/skills/scos-memory/SKILL.md,
// integrations/agent-plugins/*/*.md): recall, inspect, correct, forget --
// none of mcp-gateway's code-intelligence, agent-registry, or governance
// tooling, which those bundles never mention and are not built to drive.
var memoryTierToolNames = map[string]bool{
	"scos_recall":           true,
	"scos_status":           true,
	"scos_explain":          true,
	"scos_user_model":       true,
	"scos_memory_graph":     true,
	"scos_correct":          true,
	"scos_forget":           true,
	"scos_graphify_query":   true,
	"scos_graphify_path":    true,
	"scos_graphify_explain": true,
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
					"repo_path": map[string]any{"type": "string", "description": "Absolute path of the repo to graph. Omit to use the provider's default repo; first use against a given path builds its graph automatically."},
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
					"repo_path": map[string]any{"type": "string", "description": "Absolute path of the repo to graph. Omit to use the provider's default repo; first use against a given path builds its graph automatically."},
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
					"repo_path": map[string]any{"type": "string", "description": "Absolute path of the repo to graph. Omit to use the provider's default repo; first use against a given path builds its graph automatically."},
				},
				"required": []string{"node"},
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
			"description": "Index (or re-index) files in a repository for code intelligence search",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"repository_id":         map[string]any{"type": "string", "description": "Legacy repository identifier"},
					"durable_repository_id": map[string]any{"type": "string", "description": "Durable repository UUID returned by scos_code_repo_register; supplying it activates P4 snapshot indexing"},
					"branch":                map[string]any{"type": "string", "description": "Branch for durable snapshot indexing"},
					"commit_id":             map[string]any{"type": "string", "description": "Commit SHA being indexed"},
					"extractor_version":     map[string]any{"type": "string", "description": "Extractor version for durable snapshot identity"},
					"configuration_hash":    map[string]any{"type": "string", "description": "Indexer configuration hash"},
					"files": map[string]any{"type": "array", "items": map[string]any{"oneOf": []any{
						map[string]any{"type": "string"},
						map[string]any{"type": "object", "properties": map[string]any{
							"path": map[string]any{"type": "string"}, "language": map[string]any{"type": "string"},
							"content_hash": map[string]any{"type": "string"}, "content": map[string]any{"type": "string"},
						}, "required": []string{"path", "content_hash"}},
					}}, "description": "Legacy path strings, or structured files for durable P4 indexing"},
				},
				"required": []string{"repository_id", "files"},
			},
		},
		{
			"name":        "scos_code_query",
			"description": "Query indexed code by symbol, path, or natural-language intent",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"intent":                map[string]any{"type": "string", "description": "Query intent, e.g. \"definition\", \"references\", \"search\""},
					"query":                 map[string]any{"type": "string", "description": "Search text"},
					"repository_id":         map[string]any{"type": "string", "description": "Legacy repository identifier"},
					"durable_repository_id": map[string]any{"type": "string", "description": "Durable repository UUID; with branch, activates P4 lexical+dense+structural+PageRank retrieval"},
					"path":                  map[string]any{"type": "string", "description": "File path filter"},
					"symbol_name":           map[string]any{"type": "string", "description": "Symbol name filter"},
					"language":              map[string]any{"type": "string", "description": "Language filter"},
					"branch":                map[string]any{"type": "string", "description": "Branch filter"},
					"max_results":           map[string]any{"type": "integer", "description": "Max results"},
					"include_docs":          map[string]any{"type": "boolean", "description": "Include doc comments in results"},
				},
				"required": []string{"intent", "query"},
			},
		},
		{
			"name":        "scos_code_impact",
			"description": "Compute the change-impact set for modified symbols in a repository",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"repository_id":    map[string]any{"type": "string", "description": "Repository identifier"},
					"commit_id":        map[string]any{"type": "string", "description": "Commit SHA"},
					"modified_symbols": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Symbols that were modified"},
				},
				"required": []string{"repository_id", "modified_symbols"},
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
			"tool_permissions":   arrField(strField(""), "Granted tool permissions"),
			"memory_permissions": arrField(strField(""), "Granted memory permissions"),
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
			"from_agent":            strField("Originating agent"),
			"to_agent":              strField("Receiving agent"),
			"objective":             strField("Handoff objective"),
			"constraints":           arrField(strField(""), "Constraints"),
			"authorised_namespaces": arrField(strField(""), "Authorised namespaces"),
			"memory_references":     arrField(strField(""), "Memory references"),
			"artifact_references":   arrField(strField(""), "Artifact references"),
			"completed_work":        arrField(strField(""), "Completed work"),
			"open_work":             arrField(strField(""), "Open work"),
			"known_failures":        arrField(strField(""), "Known failures"),
			"acceptance_criteria":   strField("Acceptance criteria"),
			"expires_at":            strField("Expiry timestamp"),
		}, "from_agent", "to_agent", "objective"),
		tool("scos_handoff_respond", "Accept or reject a handoff", map[string]any{
			"handoff_id": strField("Handoff UUID"),
			"response":   strField("accepted or rejected"),
		}, "handoff_id", "response"),
	)

	// ── autonomous-control-plane ─────────────────────────────────────────
	tools = append(tools,
		tool("scos_control_decision_get", "Get the route decision for an interaction", map[string]any{
			"interaction_id": strField("Interaction ID"),
		}, "interaction_id"),
		tool("scos_control_route", "Route an interaction through the autonomous control plane", map[string]any{
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
		tool("scos_control_recover", "Begin recovery for an interrupted interaction", map[string]any{
			"interaction_id": strField("Interaction ID"),
		}, "interaction_id"),
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
		tool("scos_backup_start", "Start a new backup", map[string]any{"type": strField("Backup type")}),
		tool("scos_backup_complete", "Verify and complete a backup", map[string]any{
			"backup_id": strField("Backup ID"),
		}, "backup_id"),
		tool("scos_restore_test_list", "List restore-test results", map[string]any{}),
		tool("scos_restore_test_start", "Start a restore drill", map[string]any{
			"test_type": strField("Restore test type"),
			"backup_id": strField("Backup ID"),
		}, "test_type", "backup_id"),
		tool("scos_restore_test_complete", "Complete a restore drill", map[string]any{
			"test_id": strField("Restore test ID"),
		}, "test_id"),
		tool("scos_restore_cadence", "Check restore-test cadence status against §31 targets", map[string]any{}),
		tool("scos_backup_plan", "Get the backup coverage plan", map[string]any{}),
		tool("scos_backup_retention", "Get the retention policy", map[string]any{}),
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
			"format":                 strField("native, markdown, json, or text"),
		}, "agent_id", "session_id"),
		tool("scos_envelope_render", "Render a stored context envelope in a target format", map[string]any{
			"session_id":    strField("Session ID"),
			"target_format": strField("markdown, text, or json"),
			"agent_id":      strField("Agent ID"),
		}, "session_id", "target_format"),
		tool("scos_translate", "Translate a native event into the SCOS common event model", map[string]any{
			"event_type":    strField("Event type"),
			"source_data":   objField("Source event payload"),
			"source_format": strField("Source format"),
			"target_format": strField("Target format"),
		}, "event_type"),
	)

	// ── context-optimisation ─────────────────────────────────────────────
	tools = append(tools,
		tool("scos_context_optimize_input", "Compress and optimise input context into a capsule", map[string]any{
			"input":               strField("Raw input text"),
			"task_type":           strField("Task type"),
			"sensitivity":         strField("Sensitivity level"),
			"risk_class":          strField("Risk class"),
			"model":               strField("Target model"),
			"profile":             strField("Compression profile"),
			"token_budget":        intField("Token budget"),
			"non_droppable_hints": arrField(strField(""), "Hints that must not be dropped"),
			"code_symbols":        arrField(strField(""), "Referenced code symbols"),
			"memory_references":   arrField(strField(""), "Referenced memory IDs"),
		}, "input"),
		tool("scos_context_optimize_output", "Compress and optimise a raw model output", map[string]any{
			"raw_output":     strField("Raw output text"),
			"request_intent": strField("Original request intent"),
			"format":         strField("Target format"),
			"max_tokens":     intField("Max tokens"),
		}, "raw_output"),
		tool("scos_context_compile", "Compile input context into a capsule (alias of scos_context_optimize_input)", map[string]any{
			"input":               strField("Raw input text"),
			"task_type":           strField("Task type"),
			"sensitivity":         strField("Sensitivity level"),
			"risk_class":          strField("Risk class"),
			"model":               strField("Target model"),
			"profile":             strField("Compression profile"),
			"token_budget":        intField("Token budget"),
			"non_droppable_hints": arrField(strField(""), "Hints that must not be dropped"),
			"code_symbols":        arrField(strField(""), "Referenced code symbols"),
			"memory_references":   arrField(strField(""), "Referenced memory IDs"),
		}, "input"),
		tool("scos_context_restore", "Restore full content from a context capsule's source objects", map[string]any{
			"capsule": objField("Context capsule (as returned by scos_context_optimize_input)"),
		}, "capsule"),
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
			"confirmed":         boolField("Caller has confirmed execution"),
		}, "command"),
		tool("scos_exec_execute", "Execute a governed command through the execution proxy", map[string]any{
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
			"confirmed":         boolField("Caller has confirmed execution"),
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
		tool("scos_exec_rule_create", "Add a command rewrite/redaction rule", map[string]any{
			"pattern":     strField("Match pattern"),
			"replacement": strField("Replacement text"),
			"risk_class":  strField("Risk class"),
			"description": strField("Rule description"),
		}, "pattern", "replacement"),
	)

	// ── extension-manager ────────────────────────────────────────────────
	tools = append(tools,
		tool("scos_extension_list", "List installed extensions", map[string]any{}),
		tool("scos_extension_get", "Get an installed extension by package ID", map[string]any{"id": strField("Package ID")}, "id"),
		tool("scos_skill_list", "List registered skills", map[string]any{}),
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
		tool("scos_hook_dispatch", "Dispatch a hook event to subscribed extensions", map[string]any{
			"event":        strField("Hook event name"),
			"tenant_id":    strField("Tenant ID"),
			"principal_id": strField("Principal ID"),
			"payload":      objField("Event payload"),
		}, "event"),
		tool("scos_hook_get", "List hooks subscribed to an event", map[string]any{"event": strField("Hook event name")}, "event"),
		tool("scos_extension_install", "Install an extension from its manifest", map[string]any{
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
			"isolation":             strField("Isolation mode"),
			"capabilities_provided": arrField(strField(""), "Capabilities provided"),
			"capabilities_consumed": arrField(strField(""), "Capabilities consumed"),
			"hooks_subscribed":      arrField(strField(""), "Hooks subscribed"),
			"skills_exported":       arrField(strField(""), "Skills exported"),
			"permissions":           arrField(strField(""), "Permissions"),
			"rollback_plan":         strField("Rollback plan"),
			"content_hash":          strField("Manifest content hash"),
			"signature":             strField("Manifest signature"),
		}, "package_id", "version"),
		tool("scos_extension_install_dryrun", "Dry-run an extension install without applying it", map[string]any{
			"package_id": strField("Package ID"),
			"version":    strField("Extension version"),
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
		tool("scos_mg_operation_prepare", "Prepare a governed tool operation for journalling", map[string]any{
			"tool_name":             strField("Tool name"),
			"tool_version":          strField("Tool version"),
			"input":                 objField("Tool input"),
			"expected_effects":      objField("Expected effects"),
			"retry_policy":          objField("Retry policy"),
			"reconciliation_method": strField("Reconciliation method"),
		}, "tool_name"),
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
		tool("scos_okf_proposal_create", "Create an OKF change proposal", map[string]any{
			"Path":     strField("Document path"),
			"Content":  strField("Proposed content"),
			"Proposer": strField("Proposer identity"),
			"Reviewer": strField("Requested reviewer"),
		}, "Path", "Content", "Proposer"),
		tool("scos_okf_proposal_action", "Act on an OKF proposal: validate, diff, review, approve, reject, or merge", map[string]any{
			"id":       strField("Proposal ID"),
			"action":   strField("validate, diff, review, approve, reject, or merge"),
			"Reviewer": strField("Reviewer identity (for review/approve/reject)"),
			"Role":     strField("Reviewer role (for review/approve/reject)"),
			"Reason":   strField("Reason (for review/approve/reject)"),
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
			"tenant_id":       strField("Tenant ID"),
			"principal_id":    strField("Principal ID"),
			"query":           strField("Retrieval query"),
			"agent_id":        strField("Agent ID"),
			"session_id":      strField("Session ID"),
			"turn_id":         strField("Turn ID"),
			"namespaces":      arrField(strField(""), "Namespaces to search"),
			"memory_classes":  arrField(strField(""), "Memory classes to include"),
			"sensitivity_max": strField("Max sensitivity to include"),
			"max_results":     intField("Max results"),
			"token_budget":    intField("Token budget"),
			"include_recent":  boolField("Include recent items"),
			"include_session": boolField("Include session context"),
			"include_state":   boolField("Include task state"),
			"filters":         objField("Additional filters"),
			"repository_id":   strField("Durable code repository UUID; with branch, enables P4 code retrieval alongside memory"),
			"branch":          strField("Branch for durable code retrieval"),
		}, "tenant_id", "principal_id", "query"),
	)

	// ── security-gate ────────────────────────────────────────────────────
	tools = append(tools,
		tool("scos_auth_validate", "Validate an auth token", map[string]any{"token": strField("Bearer token")}, "token"),
		tool("scos_auth_issue", "Issue an auth token (requires issuer secret header)", map[string]any{
			"principal":          strField("Principal UUID"),
			"agent_id":           strField("Agent ID"),
			"tenant":             strField("Tenant UUID"),
			"session_id":         strField("Session ID"),
			"max_data_class":     strField("Max data classification"),
			"expires_in_seconds": intField("Token lifetime in seconds"),
		}, "principal", "tenant"),
		tool("scos_auth_revoke", "Revoke an auth token", map[string]any{"token": strField("Bearer token")}, "token"),
		tool("scos_permission_check", "Check whether a principal may perform an action on a resource", map[string]any{
			"principal":           strField("Principal"),
			"action":              strField("Action"),
			"resource":            strField("Resource"),
			"data_class":          strField("Data classification"),
			"tenant":              strField("Tenant"),
			"delegated_authority": strField("Delegated authority"),
			"expires_at":          strField("Expiry timestamp"),
			"trace_id":            strField("Trace ID"),
		}, "principal", "action", "resource", "tenant"),
		tool("scos_permission_classify", "Classify content's data sensitivity", map[string]any{
			"content": strField("Content to classify"),
		}, "content"),
		tool("scos_audit_list", "List audit log entries visible to the caller", map[string]any{}),
		tool("scos_audit_get", "Get an audit log entry by ID", map[string]any{"id": strField("Audit entry ID")}, "id"),
		tool("scos_audit_record", "Record an audit log entry", map[string]any{
			"action":     strField("Action"),
			"principal":  strField("Principal"),
			"agent_id":   strField("Agent ID"),
			"tenant":     strField("Tenant"),
			"session_id": strField("Session ID"),
			"resource":   strField("Resource"),
			"result":     strField("allowed, denied, or error"),
			"data_class": strField("Data classification"),
			"reason":     strField("Reason"),
			"trace_id":   strField("Trace ID"),
		}, "action", "principal", "tenant", "resource", "result"),
		tool("scos_secret_list", "List secret references (values are never returned)", map[string]any{}),
		tool("scos_secret_create", "Create or update a secret reference (values are never returned)", map[string]any{
			"id":       strField("Secret reference ID"),
			"name":     strField("Secret name"),
			"provider": strField("env, vault, aws-secrets, gcp-sm, or securestore"),
			"ref":      strField("Provider-specific reference"),
			"scope":    strField("Scope"),
		}, "id", "name", "provider", "ref"),
		tool("scos_secret_get", "Get a secret reference by ID (value is never returned)", map[string]any{
			"id": strField("Secret reference ID"),
		}, "id"),
	)

	// ── tool-governance ──────────────────────────────────────────────────
	tools = append(tools,
		tool("scos_tool_list", "List governed tool definitions", map[string]any{}),
		tool("scos_tool_get", "Get a governed tool definition by ID", map[string]any{"id": strField("Tool ID")}, "id"),
		tool("scos_tool_register", "Register a governed tool (requires Idempotency-Key header)", map[string]any{
			"name":        strField("Tool name"),
			"description": strField("Tool description"),
			"version":     strField("Tool version"),
			"properties":  objField("Tool properties (read_only, destructive, idempotent, externally_side_effecting, confirmation_gated, dry_run_capable, reconcilable)"),
		}, "name"),
		tool("scos_tool_transition", "Transition a governed tool's lifecycle state (requires Idempotency-Key header)", map[string]any{
			"tool_id":   strField("Tool ID"),
			"new_state": strField("New lifecycle state"),
		}, "tool_id", "new_state"),
		tool("scos_operation_list", "List governed tool operations", map[string]any{}),
		tool("scos_operation_get", "Get a governed tool operation by ID", map[string]any{"id": strField("Operation ID")}, "id"),
		tool("scos_operation_start", "Start a governed operation for an already-registered tool (requires Idempotency-Key header)", map[string]any{
			"tool_id": strField("Tool ID"),
			"input":   objField("Operation input"),
		}, "tool_id"),
		tool("scos_operation_prepare", "Prepare a governed operation, auto-registering the tool if unknown (requires Idempotency-Key header)", map[string]any{
			"tool_name": strField("Tool name"),
			"input":     objField("Operation input"),
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
		tool("scos_benchmarks", "Run and report frozen retrieval benchmark cases", map[string]any{}),
	)

	return tools
}
