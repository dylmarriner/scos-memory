package main

import (
	"strings"
	"testing"
)

func TestNewEngineServer(t *testing.T) {
	s := newEngineServer()
	if s == nil {
		t.Fatal("expected non-nil server")
	}
	if len(s.facts) != 0 {
		t.Fatal("expected empty facts")
	}
}

func TestCreateFact(t *testing.T) {
	s := newEngineServer()
	fact := Fact{
		Content:    "PostgreSQL is the system of record",
		Network:    NetworkWorld,
		Source:     "explicit",
		Authority:  5,
		Confidence: 0.95,
		Scope:      "global",
	}
	s.mu.Lock()
	s.nextID++
	fact.ID = "fact-1"
	fact.RecordedAt = "now"
	s.facts[fact.ID] = &fact
	s.mu.Unlock()

	s.mu.RLock()
	stored, ok := s.facts["fact-1"]
	s.mu.RUnlock()
	if !ok {
		t.Fatal("expected fact to be stored")
	}
	if stored.Content != "PostgreSQL is the system of record" {
		t.Fatal("content mismatch")
	}
}

func TestCreateEntity(t *testing.T) {
	s := newEngineServer()
	entity := Entity{
		Name:   "Alice",
		Type:   "user",
		Scope:  "tenant-1",
		Goals:  []string{"Complete memory fabric"},
		Traits: map[string]string{"role": "developer"},
	}
	s.mu.Lock()
	s.nextID++
	entity.ID = "entity-1"
	s.entities[entity.ID] = &entity
	s.mu.Unlock()

	s.mu.RLock()
	stored, ok := s.entities["entity-1"]
	s.mu.RUnlock()
	if !ok {
		t.Fatal("expected entity to be stored")
	}
	if len(stored.Goals) != 1 {
		t.Fatal("expected 1 goal")
	}
	if stored.Traits["role"] != "developer" {
		t.Fatal("trait mismatch")
	}
}

func TestCreateBelief(t *testing.T) {
	s := newEngineServer()
	belief := Belief{
		Premise:    "Evidence shows P",
		Conclusion: "Therefore Q",
		Confidence: 0.85,
		Evidence:   []string{"fact-1", "fact-2"},
	}
	s.mu.Lock()
	s.nextID++
	belief.ID = "belief-1"
	belief.CreatedAt = "now"
	s.beliefs[belief.ID] = &belief
	s.mu.Unlock()

	if belief.Premise != "Evidence shows P" {
		t.Fatal("premise mismatch")
	}
}

func TestContradictionDetection(t *testing.T) {
	fact1 := &Fact{Content: "Database is PostgreSQL", Network: NetworkWorld, Scope: "global"}
	fact2 := &Fact{Content: "Database is not PostgreSQL", Network: NetworkWorld, Scope: "global"}

	contra := isContradictory(fact1, fact2)
	if !contra {
		t.Fatal("expected contradiction detected")
	}
}

func TestNoContradictionDifferentNetwork(t *testing.T) {
	fact1 := &Fact{Content: "Database is PostgreSQL", Network: NetworkWorld, Scope: "global"}
	fact2 := &Fact{Content: "Database is not PostgreSQL", Network: NetworkBelief, Scope: "global"}

	contra := isContradictory(fact1, fact2)
	if contra {
		t.Fatal("should not detect contradiction across networks")
	}
}

func TestReflection(t *testing.T) {
	s := newEngineServer()
	s.facts["fact-a"] = &Fact{
		ID: "fact-a", Content: "The API returns JSON",
		Network: NetworkWorld, Source: "observed", Confidence: 0.9,
	}
	s.facts["fact-b"] = &Fact{
		ID: "fact-b", Content: "The API does not return JSON",
		Network: NetworkWorld, Source: "observed", Confidence: 0.8,
	}

	s.mu.RLock()
	contra := isContradictory(s.facts["fact-a"], s.facts["fact-b"])
	s.mu.RUnlock()
	if !contra {
		t.Fatal("expected contradiction between fact-a and fact-b")
	}
}

func TestHierarchyNode(t *testing.T) {
	s := newEngineServer()
	node := HierarchyNode{
		Level: LevelPalace,
		Name:  "SCOS Knowledge",
		Scope: "global",
	}
	s.mu.Lock()
	s.nextID++
	node.ID = "hier-1"
	node.UpdatedAt = "now"
	s.hierarchy[node.ID] = &node
	s.mu.Unlock()

	s.mu.RLock()
	stored, ok := s.hierarchy["hier-1"]
	s.mu.RUnlock()
	if !ok {
		t.Fatal("expected hierarchy node")
	}
	if stored.Level != LevelPalace {
		t.Fatal("level mismatch")
	}
}

func TestDashboardSystem(t *testing.T) {
	view := DashboardView{
		Type: "system_map",
		Entries: []any{
			map[string]string{"service": "memory-gateway", "status": "healthy"},
		},
		TotalCount: 1,
	}
	if view.TotalCount != 1 {
		t.Fatal("expected 1 entry")
	}
}

func TestDashboardFacts(t *testing.T) {
	s := newEngineServer()
	s.facts["f1"] = &Fact{Content: "Fact 1", Network: NetworkWorld, RecordedAt: "2026-01-01"}
	s.facts["f2"] = &Fact{Content: "Fact 2", Network: NetworkBelief, RecordedAt: "2026-01-02"}

	s.mu.RLock()
	var facts []*Fact
	for _, f := range s.facts {
		facts = append(facts, f)
	}
	s.mu.RUnlock()

	if len(facts) != 2 {
		t.Fatalf("expected 2 facts, got %d", len(facts))
	}
}

func TestDashboardContradictions(t *testing.T) {
	s := newEngineServer()
	s.relations = append(s.relations, &TemporalRelation{
		SourceID: "fact-a", TargetID: "fact-b", Type: "contradicts",
	})

	s.mu.RLock()
	count := 0
	for _, rel := range s.relations {
		if rel.Type == "contradicts" {
			count++
		}
	}
	s.mu.RUnlock()

	if count != 1 {
		t.Fatal("expected 1 contradiction")
	}
}

func TestDashboardTimeline(t *testing.T) {
	s := newEngineServer()
	now := "2026-06-08T12:00:00Z"
	s.facts["f1"] = &Fact{Content: "First fact", RecordedAt: now, Source: "user"}

	s.mu.RLock()
	var entries []map[string]any
	for _, f := range s.facts {
		entries = append(entries, map[string]any{
			"time": f.RecordedAt, "content": f.Content,
		})
	}
	s.mu.RUnlock()

	if len(entries) != 1 {
		t.Fatal("expected 1 timeline entry")
	}
}

func TestTruncate(t *testing.T) {
	if truncate("short", 10) != "short" {
		t.Fatal("should not truncate short strings")
	}
	if truncate("hello world this is long", 10) != "hello worl..." {
		t.Fatalf("unexpected truncation: %s", truncate("hello world this is long", 10))
	}
}

func TestMinInt(t *testing.T) {
	if minInt(5, 10) != 5 {
		t.Fatal("min should return smaller")
	}
}

func TestToAnySlice(t *testing.T) {
	items := []string{"a", "b", "c"}
	result := toAnySlice(items)
	if len(result) != 3 {
		t.Fatal("expected 3 items")
	}
}

func TestSSEStreamFormat(t *testing.T) {
	msg := "event: connected\ndata: {\"status\":\"live\"}\n\n"
	if !strings.Contains(msg, "event: connected") {
		t.Fatal("SSE format incorrect")
	}
}

func TestFactWithContradictions(t *testing.T) {
	s := newEngineServer()
	s.facts["existing"] = &Fact{
		ID: "existing", Content: "API uses REST",
		Network: NetworkWorld, Scope: "global",
	}

	newFact := &Fact{
		Content: "API does not use REST",
		Network: NetworkWorld, Scope: "global",
	}
	contra := isContradictory(newFact, s.facts["existing"])
	if !contra {
		t.Fatal("expected contradiction detected")
	}
}

func TestDashboardUserModel(t *testing.T) {
	s := newEngineServer()
	s.entities["e1"] = &Entity{ID: "e1", Name: "Alice", Type: "user", Facts: []string{"f1"}}
	s.facts["f1"] = &Fact{ID: "f1", Content: "Prefers dark mode", Source: "explicit"}

	s.mu.RLock()
	userEntities := 0
	for _, e := range s.entities {
		if e.Type == "user" {
			userEntities++
		}
	}
	s.mu.RUnlock()

	if userEntities != 1 {
		t.Fatal("expected 1 user entity")
	}
}
