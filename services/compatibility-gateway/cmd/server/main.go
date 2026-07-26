package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const version = "0.1.0"

// ── types ────────────────────────────────────────────────────────────────

type Adapter struct {
	Boundary     string   `json:"boundary"`
	Name         string   `json:"name"`
	Tier         string   `json:"tier"` // A, B, C, D
	Capabilities []string `json:"capabilities"`
	Supported    []string `json:"supported_operations"`
	Unsupported  []string `json:"unsupported_operations"`
	HealthStatus string   `json:"health_status"`
}

type AgentRegistration struct {
	AgentID      string   `json:"agent_id"`
	Name         string   `json:"name"`
	Boundary     string   `json:"boundary"`
	Tier         string   `json:"tier"`
	Adapters     []string `json:"adapters"`
	Capabilities []string `json:"capabilities"`
	Status       string   `json:"status"`
	LastSeen     string   `json:"last_seen"`
}

type ContextEnvelope struct {
	AgentID              string   `json:"agent_id"`
	SessionID            string   `json:"session_id"`
	GoverningConstraints string   `json:"governing_constraints,omitempty"`
	UserModel            string   `json:"user_model,omitempty"`
	Facts                []string `json:"facts,omitempty"`
	Learnings            []string `json:"learnings,omitempty"`
	TaskState            string   `json:"task_state,omitempty"`
	EpisodicHistory      []string `json:"episodic_history,omitempty"`
	Contradictions       []string `json:"contradictions,omitempty"`
	CodeRefs             []string `json:"code_references,omitempty"`
	Artifacts            []string `json:"artifacts,omitempty"`
	Sources              []string `json:"sources,omitempty"`
	RestorationRefs      []string `json:"restoration_references,omitempty"`
	Format               string   `json:"format"` // native, markdown, json, text
}

type AdapterContract struct {
	Operation string   `json:"operation"`
	Input     []string `json:"input_fields"`
	Output    []string `json:"output_fields"`
	Required  bool     `json:"required"`
}

// ── server state ─────────────────────────────────────────────────────────

type compatServer struct {
	mu        sync.RWMutex
	adapters  map[string]*Adapter // boundary -> adapter
	agents    map[string]*AgentRegistration
	envelopes map[string]*ContextEnvelope // session_id -> envelope
}

func newCompatServer() *compatServer {
	s := &compatServer{
		adapters:  make(map[string]*Adapter),
		agents:    make(map[string]*AgentRegistration),
		envelopes: make(map[string]*ContextEnvelope),
	}
	s.registerDefaultAdapters()
	return s
}

func (s *compatServer) registerDefaultAdapters() {
	s.adapters["native_sdk"] = &Adapter{
		Boundary:     "native_sdk",
		Name:         "Native Go SDK",
		Tier:         "A",
		Capabilities: []string{"full_ingress", "retrieval", "tools", "completion", "checkpoint", "recovery"},
		Supported: []string{"detect", "install", "configure", "health", "identify_user", "identify_agent",
			"identify_session", "capture_ingress", "inject_context", "observe_model_request",
			"observe_model_response", "observe_tool_call", "observe_tool_result", "commit_turn",
			"checkpoint", "resume", "shutdown", "uninstall"},
		HealthStatus: "healthy",
	}

	s.adapters["http_sidecar"] = &Adapter{
		Boundary:     "http_sidecar",
		Name:         "HTTP Sidecar",
		Tier:         "B",
		Capabilities: []string{"pre_turn_recall", "post_turn_persistence", "lifecycle_hooks"},
		Supported: []string{"detect", "configure", "health", "capture_ingress", "inject_context",
			"commit_turn", "checkpoint"},
		Unsupported:  []string{"observe_model_request", "observe_model_response", "resume"},
		HealthStatus: "healthy",
	}

	s.adapters["mcp_server"] = &Adapter{
		Boundary:     "mcp_server",
		Name:         "MCP Server",
		Tier:         "C",
		Capabilities: []string{"tool_recall", "automatic_transcript_capture"},
		Supported: []string{"detect", "install", "configure", "health", "capture_ingress",
			"inject_context", "observe_tool_call", "observe_tool_result", "commit_turn"},
		Unsupported:  []string{"observe_model_request", "observe_model_response", "checkpoint", "resume"},
		HealthStatus: "healthy",
	}

	s.adapters["cli_wrapper"] = &Adapter{
		Boundary:     "cli_wrapper",
		Name:         "CLI Wrapper",
		Tier:         "C",
		Capabilities: []string{"transcript_capture", "context_injection"},
		Supported:    []string{"detect", "configure", "health", "capture_ingress", "inject_context", "commit_turn"},
		Unsupported: []string{"observe_model_request", "observe_model_response", "observe_tool_call",
			"checkpoint", "resume"},
		HealthStatus: "healthy",
	}

	s.adapters["session_watcher"] = &Adapter{
		Boundary:     "session_watcher",
		Name:         "Session/File Watcher",
		Tier:         "D",
		Capabilities: []string{"passive_transcript_ingestion", "next_session_context"},
		Supported:    []string{"detect", "configure", "health", "capture_ingress", "inject_context"},
		Unsupported: []string{"observe_model_request", "observe_model_response", "observe_tool_call",
			"observe_tool_result", "commit_turn", "checkpoint", "resume"},
		HealthStatus: "healthy",
	}
}

// ── main ─────────────────────────────────────────────────────────────────

func main() {
	srv := newCompatServer()
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", healthOK)
	mux.HandleFunc("/health/ready", healthOK)

	// Adapters
	mux.HandleFunc("/v1/adapters", srv.handleAdapters)
	mux.HandleFunc("/v1/adapters/", srv.handleAdapterByBoundary)

	// Agents
	mux.HandleFunc("/v1/agents", srv.handleAgents)
	mux.HandleFunc("/v1/agents/", srv.handleAgentByID)

	// Context envelope
	mux.HandleFunc("/v1/envelope", srv.handleEnvelope)
	mux.HandleFunc("/v1/envelope/render", srv.handleRender)

	// Translation
	mux.HandleFunc("/v1/translate", srv.handleTranslate)

	addr := os.Getenv("SCOS_COMPATIBILITY_GATEWAY_HTTP_ADDR")
	if addr == "" {
		addr = ":8087"
	}
	log.Printf("compatibility gateway listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

func healthOK(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// ── adapters ─────────────────────────────────────────────────────────────

func (s *compatServer) handleAdapters(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	var adapters []*Adapter
	for _, a := range s.adapters {
		adapters = append(adapters, a)
	}
	s.mu.RUnlock()
	writeJSON(w, adapters)
}

func (s *compatServer) handleAdapterByBoundary(w http.ResponseWriter, r *http.Request) {
	boundary := strings.TrimPrefix(r.URL.Path, "/v1/adapters/")
	s.mu.RLock()
	adapter, ok := s.adapters[boundary]
	s.mu.RUnlock()
	if !ok {
		http.Error(w, "adapter not found", http.StatusNotFound)
		return
	}
	writeJSON(w, adapter)
}

// ── agents ───────────────────────────────────────────────────────────────

func (s *compatServer) handleAgents(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.mu.RLock()
		var agents []*AgentRegistration
		for _, a := range s.agents {
			agents = append(agents, a)
		}
		s.mu.RUnlock()
		writeJSON(w, agents)

	case http.MethodPost:
		var reg AgentRegistration
		if err := json.NewDecoder(r.Body).Decode(&reg); err != nil {
			http.Error(w, "invalid registration: "+err.Error(), http.StatusBadRequest)
			return
		}
		if reg.AgentID == "" {
			http.Error(w, "agent_id required", http.StatusBadRequest)
			return
		}
		// Auto-detect tier from boundary
		if reg.Boundary != "" {
			s.mu.RLock()
			adapter, ok := s.adapters[reg.Boundary]
			s.mu.RUnlock()
			if ok {
				reg.Tier = adapter.Tier
				reg.Adapters = adapter.Supported
				reg.Capabilities = adapter.Capabilities
			}
		}
		if reg.Status == "" {
			reg.Status = "active"
		}
		reg.LastSeen = time.Now().UTC().Format(time.RFC3339)
		s.mu.Lock()
		s.agents[reg.AgentID] = &reg
		s.mu.Unlock()
		writeJSON(w, reg)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *compatServer) handleAgentByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/agents/")
	s.mu.RLock()
	agent, ok := s.agents[id]
	s.mu.RUnlock()
	if !ok {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	writeJSON(w, agent)
}

// ── context envelope ─────────────────────────────────────────────────────

func (s *compatServer) handleEnvelope(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var env ContextEnvelope
		if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
			http.Error(w, "invalid envelope: "+err.Error(), http.StatusBadRequest)
			return
		}
		if env.AgentID == "" || env.SessionID == "" {
			http.Error(w, "agent_id and session_id required", http.StatusBadRequest)
			return
		}
		if env.Format == "" {
			env.Format = "json"
		}
		s.mu.Lock()
		s.envelopes[env.SessionID] = &env
		s.mu.Unlock()
		writeJSON(w, env)

	case http.MethodGet:
		sessionID := r.URL.Query().Get("session_id")
		if sessionID == "" {
			http.Error(w, "session_id query param required", http.StatusBadRequest)
			return
		}
		s.mu.RLock()
		env, ok := s.envelopes[sessionID]
		s.mu.RUnlock()
		if !ok {
			http.Error(w, "envelope not found", http.StatusNotFound)
			return
		}
		writeJSON(w, env)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ── render endpoint ──────────────────────────────────────────────────────

func (s *compatServer) handleRender(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		SessionID    string `json:"session_id"`
		TargetFormat string `json:"target_format"` // markdown, text, json
		AgentID      string `json:"agent_id,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}

	s.mu.RLock()
	env, ok := s.envelopes[req.SessionID]
	s.mu.RUnlock()
	if !ok {
		http.Error(w, "envelope not found", http.StatusNotFound)
		return
	}

	rendered := renderEnvelope(env, req.TargetFormat)
	writeJSON(w, map[string]string{
		"format":  req.TargetFormat,
		"content": rendered,
	})
}

// ── translate endpoint ───────────────────────────────────────────────────

func (s *compatServer) handleTranslate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		EventType    string `json:"event_type"`
		SourceData   any    `json:"source_data"`
		SourceFormat string `json:"source_format"`
		TargetFormat string `json:"target_format"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Translate native event to SCOS common event model
	translated := translateEvent(req.EventType, req.SourceData, req.SourceFormat)
	writeJSON(w, map[string]any{
		"event_type":    req.EventType,
		"translated":    translated,
		"target_format": "scos_common",
	})
}

// ── envelope rendering ───────────────────────────────────────────────────

func renderEnvelope(env *ContextEnvelope, format string) string {
	switch format {
	case "markdown":
		return renderMarkdown(env)
	case "text":
		return renderText(env)
	default:
		return renderJSON(env)
	}
}

func renderMarkdown(env *ContextEnvelope) string {
	var b strings.Builder
	b.WriteString("# Context Envelope\n\n")
	b.WriteString(fmt.Sprintf("**Agent:** %s  \n", env.AgentID))
	b.WriteString(fmt.Sprintf("**Session:** %s  \n", env.SessionID))

	if env.GoverningConstraints != "" {
		b.WriteString("\n## Governing Constraints\n\n")
		b.WriteString(env.GoverningConstraints + "\n")
	}
	if env.UserModel != "" {
		b.WriteString("\n## User Model\n\n")
		b.WriteString(env.UserModel + "\n")
	}
	if len(env.Facts) > 0 {
		b.WriteString("\n## Facts\n\n")
		for _, f := range env.Facts {
			b.WriteString(fmt.Sprintf("- %s\n", f))
		}
	}
	if len(env.Learnings) > 0 {
		b.WriteString("\n## Learnings\n\n")
		for _, l := range env.Learnings {
			b.WriteString(fmt.Sprintf("- %s\n", l))
		}
	}
	if env.TaskState != "" {
		b.WriteString("\n## Task State\n\n")
		b.WriteString(env.TaskState + "\n")
	}
	if len(env.Contradictions) > 0 {
		b.WriteString("\n## Contradictions\n\n")
		for _, c := range env.Contradictions {
			b.WriteString(fmt.Sprintf("- ⚠ %s\n", c))
		}
	}
	if len(env.Sources) > 0 {
		b.WriteString("\n## Sources\n\n")
		for _, s := range env.Sources {
			b.WriteString(fmt.Sprintf("- `%s`\n", s))
		}
	}
	return b.String()
}

func renderText(env *ContextEnvelope) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("Context for Agent %s (Session %s)\n", env.AgentID, env.SessionID))
	b.WriteString("=" + strings.Repeat("=", len(env.AgentID)+len(env.SessionID)+20) + "\n\n")

	if env.GoverningConstraints != "" {
		b.WriteString("GOVERNING CONSTRAINTS:\n" + env.GoverningConstraints + "\n\n")
	}
	if len(env.Facts) > 0 {
		b.WriteString("FACTS:\n")
		for _, f := range env.Facts {
			b.WriteString("  * " + f + "\n")
		}
		b.WriteString("\n")
	}
	if len(env.Learnings) > 0 {
		b.WriteString("LEARNINGS:\n")
		for _, l := range env.Learnings {
			b.WriteString("  * " + l + "\n")
		}
		b.WriteString("\n")
	}
	return b.String()
}

func renderJSON(env *ContextEnvelope) string {
	data, _ := json.MarshalIndent(env, "", "  ")
	return string(data)
}

// ── event translation ────────────────────────────────────────────────────

func translateEvent(eventType string, sourceData any, sourceFormat string) map[string]any {
	translated := map[string]any{
		"event_type":    eventType,
		"source_format": sourceFormat,
		"scos_version":  version,
		"translated_at": time.Now().UTC().Format(time.RFC3339),
	}

	switch sourceFormat {
	case "openai":
		if data, ok := sourceData.(map[string]any); ok {
			translated["content"] = data
			translated["role"] = data["role"]
			translated["provenance"] = "openai_compatible"
		}
	case "anthropic":
		translated["provenance"] = "anthropic"
		translated["content"] = sourceData
	case "mcp":
		translated["provenance"] = "mcp"
		translated["tool_call"] = sourceData
	default:
		translated["provenance"] = sourceFormat
		translated["content"] = sourceData
	}

	return translated
}

// ── utility ──────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("encode response: %v", err)
	}
}
