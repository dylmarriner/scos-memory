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

// ── agent registration (§29) ─────────────────────────────────────────────

type AgentRegistration struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	Version           string   `json:"version"`
	Maintainer        string   `json:"maintainer"`
	Capabilities      []string `json:"capabilities"`
	Inputs            []string `json:"inputs"`
	Outputs           []string `json:"outputs"`
	ToolPermissions   []string `json:"tool_permissions,omitempty"`
	MemoryPermissions []string `json:"memory_permissions,omitempty"`
	MaxClassification string   `json:"max_classification"`
	ModelProvider     string   `json:"model_provider"`
	CostPolicy        string   `json:"cost_policy"`
	HealthEndpoint    string   `json:"health_endpoint"`
	Status            string   `json:"status"` // active, inactive, degraded
	RegisteredAt      string   `json:"registered_at"`
	LastSeen          string   `json:"last_seen"`
}

// ── handoff packet ───────────────────────────────────────────────────────

type HandoffPacket struct {
	ID            string   `json:"id"`
	FromAgent     string   `json:"from_agent"`
	ToAgent       string   `json:"to_agent"`
	Objective     string   `json:"objective"`
	Constraints   []string `json:"constraints,omitempty"`
	Namespaces    []string `json:"authorised_namespaces,omitempty"`
	MemoryRefs    []string `json:"memory_references,omitempty"`
	ArtifactRefs  []string `json:"artifact_references,omitempty"`
	CompletedWork []string `json:"completed_work,omitempty"`
	OpenWork      []string `json:"open_work,omitempty"`
	KnownFailures []string `json:"known_failures,omitempty"`
	Acceptance    string   `json:"acceptance_criteria,omitempty"`
	ExpiresAt     string   `json:"expires_at,omitempty"`
	Status        string   `json:"status"` // pending, accepted, rejected, completed, expired
	CreatedAt     string   `json:"created_at"`
}

// ── server ───────────────────────────────────────────────────────────────

type agentRegistryServer struct {
	mu       sync.RWMutex
	agents   map[string]*AgentRegistration
	handoffs map[string]*HandoffPacket
}

func newAgentRegistryServer() *agentRegistryServer {
	return &agentRegistryServer{
		agents:   make(map[string]*AgentRegistration),
		handoffs: make(map[string]*HandoffPacket),
	}
}

func main() {
	s := newAgentRegistryServer()
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", healthOK)
	mux.HandleFunc("/health/ready", healthOK)

	// Agent registration
	mux.HandleFunc("/v1/agents", s.handleAgents)
	mux.HandleFunc("/v1/agents/", s.handleAgentByID)
	mux.HandleFunc("/v1/agents/register", s.handleAgentRegister)
	mux.HandleFunc("/v1/agents/heartbeat", s.handleAgentHeartbeat)

	// Handoffs
	mux.HandleFunc("/v1/handoffs", s.handleHandoffs)
	mux.HandleFunc("/v1/handoffs/", s.handleHandoffByID)
	mux.HandleFunc("/v1/handoffs/initiate", s.handleHandoffInitiate)
	mux.HandleFunc("/v1/handoffs/respond", s.handleHandoffRespond)

	addr := os.Getenv("SCOS_AGENT_REGISTRY_HTTP_ADDR")
	if addr == "" {
		addr = ":8093"
	}
	log.Printf("agent registry listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

func healthOK(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (s *agentRegistryServer) handleAgents(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	var agents []*AgentRegistration
	for _, a := range s.agents {
		agents = append(agents, a)
	}
	s.mu.RUnlock()
	writeJSON(w, agents)
}

func (s *agentRegistryServer) handleAgentByID(w http.ResponseWriter, r *http.Request) {
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

func (s *agentRegistryServer) handleAgentRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var reg AgentRegistration
	if err := json.NewDecoder(r.Body).Decode(&reg); err != nil {
		http.Error(w, "invalid registration: "+err.Error(), http.StatusBadRequest)
		return
	}
	if reg.Name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	reg.ID = fmt.Sprintf("agent-%d", len(s.agents)+1)
	reg.Status = "active"
	reg.RegisteredAt = time.Now().UTC().Format(time.RFC3339)
	reg.LastSeen = reg.RegisteredAt
	if reg.MaxClassification == "" {
		reg.MaxClassification = "confidential"
	}
	s.agents[reg.ID] = &reg
	s.mu.Unlock()
	writeJSON(w, reg)
}

func (s *agentRegistryServer) handleAgentHeartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		AgentID string `json:"agent_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	agent, ok := s.agents[req.AgentID]
	if !ok {
		s.mu.Unlock()
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	agent.LastSeen = time.Now().UTC().Format(time.RFC3339)
	agent.Status = "active"
	s.mu.Unlock()
	writeJSON(w, map[string]any{"status": "acknowledged", "last_seen": agent.LastSeen})
}

func (s *agentRegistryServer) handleHandoffs(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	var packets []*HandoffPacket
	for _, p := range s.handoffs {
		packets = append(packets, p)
	}
	s.mu.RUnlock()
	writeJSON(w, packets)
}

func (s *agentRegistryServer) handleHandoffByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/handoffs/")
	s.mu.RLock()
	p, ok := s.handoffs[id]
	s.mu.RUnlock()
	if !ok {
		http.Error(w, "handoff not found", http.StatusNotFound)
		return
	}
	writeJSON(w, p)
}

func (s *agentRegistryServer) handleHandoffInitiate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var p HandoffPacket
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		http.Error(w, "invalid handoff: "+err.Error(), http.StatusBadRequest)
		return
	}
	if p.Objective == "" {
		http.Error(w, "objective required", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	p.ID = fmt.Sprintf("handoff-%d", len(s.handoffs)+1)
	p.Status = "pending"
	p.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	s.handoffs[p.ID] = &p
	s.mu.Unlock()
	writeJSON(w, p)
}

func (s *agentRegistryServer) handleHandoffRespond(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		HandoffID string `json:"handoff_id"`
		Response  string `json:"response"` // accepted, rejected
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	p, ok := s.handoffs[req.HandoffID]
	if !ok {
		s.mu.Unlock()
		http.Error(w, "handoff not found", http.StatusNotFound)
		return
	}
	if req.Response == "accepted" {
		p.Status = "accepted"
	} else {
		p.Status = "rejected"
	}
	s.mu.Unlock()
	writeJSON(w, p)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("encode response: %v", err)
	}
}
