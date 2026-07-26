package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

const version = "0.1.0"

// ── Memory network types (§25.2) ────────────────────────────────────────

type MemoryNetwork string

const (
	NetworkWorld      MemoryNetwork = "world"
	NetworkExperience MemoryNetwork = "experience"
	NetworkEntity     MemoryNetwork = "entity"
	NetworkBelief     MemoryNetwork = "belief"
	NetworkProcedure  MemoryNetwork = "procedure"
	NetworkNegative   MemoryNetwork = "negative"
)

type Fact struct {
	ID           string        `json:"id"`
	Network      MemoryNetwork `json:"network"`
	Content      string        `json:"content"`
	Source       string        `json:"source"` // explicit, observed, inferred, external
	Authority    int           `json:"authority_level"`
	Confidence   float64       `json:"confidence"`
	Scope        string        `json:"scope"`
	ObservedAt   string        `json:"observed_at"`
	ValidFrom    string        `json:"valid_from,omitempty"`
	ValidUntil   string        `json:"valid_until,omitempty"`
	RecordedAt   string        `json:"recorded_at"`
	SupersededBy string        `json:"superseded_by,omitempty"`
	Evidence     []string      `json:"evidence,omitempty"`
	Inferred     bool          `json:"inferred"`
	Derivation   string        `json:"derivation,omitempty"`
	Revision     int           `json:"revision"`
}

type Entity struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Type        string            `json:"type"` // user, agent, organisation
	Facts       []string          `json:"fact_ids,omitempty"`
	Preferences []string          `json:"preferences,omitempty"`
	Goals       []string          `json:"goals,omitempty"`
	Traits      map[string]string `json:"traits,omitempty"`
	Scope       string            `json:"scope"`
}

type Belief struct {
	ID         string   `json:"id"`
	Premise    string   `json:"premise"`
	Conclusion string   `json:"conclusion"`
	Confidence float64  `json:"confidence"`
	Evidence   []string `json:"evidence_ids"`
	RevisedBy  string   `json:"revised_by,omitempty"`
	CreatedAt  string   `json:"created_at"`
}

type TemporalRelation struct {
	SourceID  string `json:"source_id"`
	TargetID  string `json:"target_id"`
	Type      string `json:"type"` // derived_from, contradicts, supersedes, evidence_for, used_in
	Scope     string `json:"scope"`
	CreatedAt string `json:"created_at"`
}

// ── Organisational hierarchy (§25.6) ────────────────────────────────────

type MemoryHierarchy string

const (
	LevelPalace   MemoryHierarchy = "palace"
	LevelWing     MemoryHierarchy = "wing"
	LevelDrawer   MemoryHierarchy = "drawer"
	LevelCloset   MemoryHierarchy = "closet"
	LevelDiary    MemoryHierarchy = "diary"
	LevelArtifact MemoryHierarchy = "artifact"
)

type HierarchyNode struct {
	ID        string          `json:"id"`
	Level     MemoryHierarchy `json:"level"`
	Name      string          `json:"name"`
	ParentID  string          `json:"parent_id,omitempty"`
	Scope     string          `json:"scope"`
	Summary   string          `json:"summary,omitempty"`
	FactCount int             `json:"fact_count"`
	UpdatedAt string          `json:"updated_at"`
}

// ── Dashboard types (§25.9) ─────────────────────────────────────────────

type DashboardView struct {
	Type        string `json:"type"`
	Entries     []any  `json:"entries"`
	TotalCount  int    `json:"total_count"`
	LastUpdated string `json:"last_updated"`
}

type SystemMapEntry struct {
	Service string `json:"service"`
	Status  string `json:"status"`
	Version string `json:"version"`
	Uptime  string `json:"uptime"`
}

// ── server state ─────────────────────────────────────────────────────────

type engineServer struct {
	mu        sync.RWMutex
	facts     map[string]*Fact
	entities  map[string]*Entity
	beliefs   map[string]*Belief
	relations []*TemporalRelation
	hierarchy map[string]*HierarchyNode
	startTime time.Time
	nextID    int
}

func newEngineServer() *engineServer {
	return &engineServer{
		facts:     make(map[string]*Fact),
		entities:  make(map[string]*Entity),
		beliefs:   make(map[string]*Belief),
		hierarchy: make(map[string]*HierarchyNode),
		startTime: time.Now(),
	}
}

// ── main ─────────────────────────────────────────────────────────────────

func main() {
	srv := newEngineServer()
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", healthOK)
	mux.HandleFunc("/health/ready", healthOK)

	// Memory networks
	mux.HandleFunc("/v1/facts", srv.handleFacts)
	mux.HandleFunc("/v1/facts/", srv.handleFactByID)
	mux.HandleFunc("/v1/entities", srv.handleEntities)
	mux.HandleFunc("/v1/entities/", srv.handleEntityByID)
	mux.HandleFunc("/v1/beliefs", srv.handleBeliefs)
	mux.HandleFunc("/v1/relations", srv.handleRelations)
	mux.HandleFunc("/v1/hierarchy", srv.handleHierarchy)

	// Reflection
	mux.HandleFunc("/v1/reflect", srv.handleReflect)

	// Dashboard views (§25.9)
	mux.HandleFunc("/v1/dashboard/system", srv.handleDashboardSystem)
	mux.HandleFunc("/v1/dashboard/memory-graph", srv.handleDashboardMemoryGraph)
	mux.HandleFunc("/v1/dashboard/facts", srv.handleDashboardFacts)
	mux.HandleFunc("/v1/dashboard/learnings", srv.handleDashboardLearnings)
	mux.HandleFunc("/v1/dashboard/contradictions", srv.handleDashboardContradictions)
	mux.HandleFunc("/v1/dashboard/timeline", srv.handleDashboardTimeline)
	mux.HandleFunc("/v1/dashboard/user-model", srv.handleDashboardUserModel)
	mux.HandleFunc("/v1/stream", srv.handleStream) // SSE
	mux.HandleFunc("/v1/transparency/stream", srv.handleStream)
	mux.HandleFunc("/v1/users/", srv.handleUserKnowledge)

	addr := os.Getenv("SCOS_MEMORY_INTELLIGENCE_HTTP_ADDR")
	if addr == "" {
		addr = ":8089"
	}
	log.Printf("memory intelligence engine listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

func (s *engineServer) handleUserKnowledge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	userID := strings.TrimPrefix(r.URL.Path, "/v1/users/")
	userID = strings.TrimSuffix(userID, "/knowledge")
	if userID == "" || strings.Contains(userID, "/") {
		http.Error(w, "user_id is required", http.StatusBadRequest)
		return
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	entity, ok := s.entities[userID]
	if !ok || entity.Type != "user" {
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}
	facts := make([]*Fact, 0, len(entity.Facts))
	for _, factID := range entity.Facts {
		if fact, exists := s.facts[factID]; exists {
			facts = append(facts, fact)
		}
	}
	writeJSON(w, map[string]any{
		"user_id": userID,
		"entity":  entity,
		"facts":   facts,
	})
}

func healthOK(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// ── facts ────────────────────────────────────────────────────────────────

func (s *engineServer) handleFacts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.mu.RLock()
		var facts []*Fact
		for _, f := range s.facts {
			facts = append(facts, f)
		}
		s.mu.RUnlock()
		writeJSON(w, facts)

	case http.MethodPost:
		var fact Fact
		if err := json.NewDecoder(r.Body).Decode(&fact); err != nil {
			http.Error(w, "invalid fact: "+err.Error(), http.StatusBadRequest)
			return
		}
		if fact.Content == "" {
			http.Error(w, "content required", http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.nextID++
		fact.ID = fmt.Sprintf("fact-%d", s.nextID)
		fact.RecordedAt = time.Now().UTC().Format(time.RFC3339)
		if fact.Network == "" {
			fact.Network = NetworkWorld
		}
		if fact.Scope == "" {
			fact.Scope = "global"
		}
		// Detect contradictions
		contradictions := s.detectContradictions(&fact)
		if len(contradictions) > 0 {
			for _, c := range contradictions {
				s.relations = append(s.relations, &TemporalRelation{
					SourceID:  fact.ID,
					TargetID:  c,
					Type:      "contradicts",
					CreatedAt: fact.RecordedAt,
				})
			}
		}
		s.facts[fact.ID] = &fact
		s.mu.Unlock()
		writeJSON(w, map[string]any{"fact": fact, "contradictions": contradictions})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *engineServer) handleFactByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/facts/")
	s.mu.RLock()
	fact, ok := s.facts[id]
	s.mu.RUnlock()
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, fact)
}

// ── entities ─────────────────────────────────────────────────────────────

func (s *engineServer) handleEntities(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.mu.RLock()
		var entities []*Entity
		for _, e := range s.entities {
			entities = append(entities, e)
		}
		s.mu.RUnlock()
		writeJSON(w, entities)

	case http.MethodPost:
		var entity Entity
		if err := json.NewDecoder(r.Body).Decode(&entity); err != nil {
			http.Error(w, "invalid entity: "+err.Error(), http.StatusBadRequest)
			return
		}
		if entity.Name == "" {
			http.Error(w, "name required", http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.nextID++
		entity.ID = fmt.Sprintf("entity-%d", s.nextID)
		if entity.Type == "" {
			entity.Type = "user"
		}
		s.entities[entity.ID] = &entity
		s.mu.Unlock()
		writeJSON(w, entity)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *engineServer) handleEntityByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/entities/")
	s.mu.RLock()
	entity, ok := s.entities[id]
	s.mu.RUnlock()
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, entity)
}

// ── beliefs ──────────────────────────────────────────────────────────────

func (s *engineServer) handleBeliefs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.mu.RLock()
		var beliefs []*Belief
		for _, b := range s.beliefs {
			beliefs = append(beliefs, b)
		}
		s.mu.RUnlock()
		writeJSON(w, beliefs)

	case http.MethodPost:
		var belief Belief
		if err := json.NewDecoder(r.Body).Decode(&belief); err != nil {
			http.Error(w, "invalid belief: "+err.Error(), http.StatusBadRequest)
			return
		}
		if belief.Conclusion == "" {
			http.Error(w, "conclusion required", http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.nextID++
		belief.ID = fmt.Sprintf("belief-%d", s.nextID)
		belief.CreatedAt = time.Now().UTC().Format(time.RFC3339)
		s.beliefs[belief.ID] = &belief
		s.mu.Unlock()
		writeJSON(w, belief)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ── relations ────────────────────────────────────────────────────────────

func (s *engineServer) handleRelations(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	relations := s.relations
	s.mu.RUnlock()
	if relations == nil {
		relations = []*TemporalRelation{}
	}
	writeJSON(w, relations)
}

// ── hierarchy ────────────────────────────────────────────────────────────

func (s *engineServer) handleHierarchy(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.mu.RLock()
		var nodes []*HierarchyNode
		for _, n := range s.hierarchy {
			nodes = append(nodes, n)
		}
		s.mu.RUnlock()
		writeJSON(w, nodes)

	case http.MethodPost:
		var node HierarchyNode
		if err := json.NewDecoder(r.Body).Decode(&node); err != nil {
			http.Error(w, "invalid node: "+err.Error(), http.StatusBadRequest)
			return
		}
		if node.Name == "" {
			http.Error(w, "name required", http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.nextID++
		node.ID = fmt.Sprintf("hier-%d", s.nextID)
		if node.Level == "" {
			node.Level = LevelDrawer
		}
		node.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		s.hierarchy[node.ID] = &node
		s.mu.Unlock()
		writeJSON(w, node)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ── reflection (§25.7) ───────────────────────────────────────────────────

func (s *engineServer) handleReflect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Evidence   []string `json:"evidence_ids"`
		Scope      string   `json:"scope,omitempty"`
		MaxBeliefs int      `json:"max_beliefs,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.MaxBeliefs <= 0 {
		req.MaxBeliefs = 5
	}

	s.mu.RLock()
	// Gather evidence facts
	var evidenceFacts []*Fact
	for _, id := range req.Evidence {
		if f, ok := s.facts[id]; ok {
			evidenceFacts = append(evidenceFacts, f)
		}
	}

	// Find contradictions among evidence
	var contradictions []map[string]any
	for i, f1 := range evidenceFacts {
		for j, f2 := range evidenceFacts {
			if i >= j {
				continue
			}
			if isContradictory(f1, f2) {
				contradictions = append(contradictions, map[string]any{
					"fact_a": f1.ID,
					"fact_b": f2.ID,
					"reason": "conflicting claims",
				})
			}
		}
	}

	// Generate candidate beliefs from evidence
	var candidates []Belief
	for _, f := range evidenceFacts {
		if f.Inferred {
			continue
		}
		// Simple belief: claim with evidence
		candidates = append(candidates, Belief{
			Conclusion: f.Content,
			Confidence: f.Confidence * 0.9,
			Evidence:   []string{f.ID},
		})
		if len(candidates) >= req.MaxBeliefs {
			break
		}
	}

	// Check for prior beliefs that conflict
	var staleBeliefs []string
	for _, b := range s.beliefs {
		for _, f := range evidenceFacts {
			if strings.Contains(strings.ToLower(b.Conclusion), strings.ToLower(f.Content[:minInt(30, len(f.Content))])) {
				staleBeliefs = append(staleBeliefs, b.ID)
			}
		}
	}
	s.mu.RUnlock()

	writeJSON(w, map[string]any{
		"evidence_analysed": len(evidenceFacts),
		"contradictions":    contradictions,
		"candidate_beliefs": candidates,
		"stale_beliefs":     staleBeliefs,
		"requires_revision": len(contradictions) > 0 || len(staleBeliefs) > 0,
	})
}

// ── dashboard views ──────────────────────────────────────────────────────

func (s *engineServer) handleDashboardSystem(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, DashboardView{
		Type: "system_map",
		Entries: []any{
			SystemMapEntry{Service: "memory-gateway", Status: "healthy", Version: "0.1.0", Uptime: time.Since(s.startTime).Round(time.Second).String()},
			SystemMapEntry{Service: "retrieval-orchestrator", Status: "healthy", Version: "0.1.0", Uptime: "12m"},
			SystemMapEntry{Service: "context-optimisation", Status: "healthy", Version: "0.1.0", Uptime: "12m"},
			SystemMapEntry{Service: "code-intelligence", Status: "healthy", Version: "0.1.0", Uptime: "10m"},
			SystemMapEntry{Service: "execution-proxy", Status: "healthy", Version: "0.1.0", Uptime: "10m"},
			SystemMapEntry{Service: "extension-manager", Status: "healthy", Version: "0.1.0", Uptime: "8m"},
			SystemMapEntry{Service: "compatibility-gateway", Status: "healthy", Version: "0.1.0", Uptime: "8m"},
			SystemMapEntry{Service: "memory-intelligence", Status: "healthy", Version: version, Uptime: time.Since(s.startTime).Round(time.Second).String()},
		},
		TotalCount:  8,
		LastUpdated: time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *engineServer) handleDashboardMemoryGraph(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	var nodes []map[string]any
	for _, f := range s.facts {
		nodes = append(nodes, map[string]any{
			"id": f.ID, "label": truncate(f.Content, 60),
			"type": "fact", "network": f.Network,
		})
	}
	for _, e := range s.entities {
		nodes = append(nodes, map[string]any{
			"id": e.ID, "label": e.Name, "type": "entity",
		})
	}
	var edges []map[string]any
	for _, r := range s.relations {
		edges = append(edges, map[string]any{
			"source": r.SourceID, "target": r.TargetID,
			"type": r.Type,
		})
	}
	s.mu.RUnlock()

	writeJSON(w, map[string]any{
		"type":  "memory_graph",
		"nodes": nodes,
		"edges": edges,
	})
}

func (s *engineServer) handleDashboardFacts(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	network := r.URL.Query().Get("network")
	var facts []*Fact
	for _, f := range s.facts {
		if network == "" || string(f.Network) == network {
			facts = append(facts, f)
		}
	}
	s.mu.RUnlock()
	sort.Slice(facts, func(i, j int) bool {
		return facts[i].RecordedAt > facts[j].RecordedAt
	})
	writeJSON(w, DashboardView{
		Type:        "facts",
		Entries:     toAnySlice(facts),
		TotalCount:  len(facts),
		LastUpdated: time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *engineServer) handleDashboardLearnings(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	var beliefs []*Belief
	for _, b := range s.beliefs {
		beliefs = append(beliefs, b)
	}
	s.mu.RUnlock()
	writeJSON(w, DashboardView{
		Type:        "learnings",
		Entries:     toAnySlice(beliefs),
		TotalCount:  len(beliefs),
		LastUpdated: time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *engineServer) handleDashboardContradictions(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	var contra []map[string]any
	for _, rel := range s.relations {
		if rel.Type == "contradicts" {
			f1, ok1 := s.facts[rel.SourceID]
			f2, ok2 := s.facts[rel.TargetID]
			if ok1 && ok2 {
				contra = append(contra, map[string]any{
					"fact_a":       f1.Content,
					"fact_b":       f2.Content,
					"source_a":     f1.Source,
					"source_b":     f2.Source,
					"confidence_a": f1.Confidence,
					"confidence_b": f2.Confidence,
				})
			}
		}
	}
	s.mu.RUnlock()
	writeJSON(w, DashboardView{
		Type:        "contradictions",
		Entries:     toAnySlice(contra),
		TotalCount:  len(contra),
		LastUpdated: time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *engineServer) handleDashboardTimeline(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	var entries []map[string]any
	for _, f := range s.facts {
		entries = append(entries, map[string]any{
			"time": f.RecordedAt, "type": "fact",
			"content": truncate(f.Content, 80),
			"source":  f.Source,
		})
	}
	s.mu.RUnlock()
	sort.Slice(entries, func(i, j int) bool {
		return entries[i]["time"].(string) > entries[j]["time"].(string)
	})
	if len(entries) > 100 {
		entries = entries[:100]
	}
	writeJSON(w, DashboardView{
		Type:        "timeline",
		Entries:     toAnySlice(entries),
		TotalCount:  len(entries),
		LastUpdated: time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *engineServer) handleDashboardUserModel(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	var entityList []*Entity
	for _, e := range s.entities {
		if e.Type == "user" {
			entityList = append(entityList, e)
		}
	}
	// Also get user-related facts
	var userFactIDs []string
	for _, e := range entityList {
		userFactIDs = append(userFactIDs, e.Facts...)
	}
	var userFacts []*Fact
	for _, id := range userFactIDs {
		if f, ok := s.facts[id]; ok {
			userFacts = append(userFacts, f)
		}
	}
	s.mu.RUnlock()

	writeJSON(w, map[string]any{
		"type":         "user_model",
		"entities":     entityList,
		"facts":        userFacts,
		"fact_count":   len(userFacts),
		"last_updated": time.Now().UTC().Format(time.RFC3339),
	})
}

// ── SSE stream (§25.12) ──────────────────────────────────────────────────

func (s *engineServer) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Send initial heartbeat
	fmt.Fprintf(w, "event: connected\ndata: {\"status\":\"live\",\"version\":\"%s\"}\n\n", version)
	flusher.Flush()

	// Keep-alive
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			fmt.Fprintf(w, "event: heartbeat\ndata: {\"time\":\"%s\"}\n\n", time.Now().UTC().Format(time.RFC3339))
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// ── contradiction detection ──────────────────────────────────────────────

func (s *engineServer) detectContradictions(fact *Fact) []string {
	var contradictions []string
	for _, existing := range s.facts {
		if isContradictory(fact, existing) {
			contradictions = append(contradictions, existing.ID)
		}
	}
	return contradictions
}

func isContradictory(a, b *Fact) bool {
	if a.Network != b.Network || a.Scope != b.Scope {
		return false
	}
	aWords := strings.Fields(strings.ToLower(a.Content))
	bWords := strings.Fields(strings.ToLower(b.Content))

	negationWords := map[string]bool{"not": true, "never": true, "no": true, "isn't": true, "doesn't": true, "don't": true, "won't": true, "can't": true, "without": true}

	aHasNegation := false
	bHasNegation := false
	for _, w := range aWords {
		if negationWords[w] {
			aHasNegation = true
			break
		}
	}
	for _, w := range bWords {
		if negationWords[w] {
			bHasNegation = true
			break
		}
	}

	// One has negation, the other doesn't, and they share significant terms
	if !(aHasNegation != bHasNegation) {
		return false
	}

	sharedTerms := 0
	for _, w := range aWords {
		for _, w2 := range bWords {
			if w == w2 && len(w) > 3 && !negationWords[w] {
				sharedTerms++
			}
		}
	}

	return sharedTerms >= 1
}

// ── utility ──────────────────────────────────────────────────────────────

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func toAnySlice[T any](items []T) []any {
	result := make([]any, len(items))
	for i, item := range items {
		result[i] = item
	}
	return result
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("encode response: %v", err)
	}
}
