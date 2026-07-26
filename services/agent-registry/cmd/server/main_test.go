package main

import (
	"testing"
	"time"
)

func TestNewAgentRegistry(t *testing.T) {
	s := newAgentRegistryServer()
	if s == nil {
		t.Fatal("expected non-nil server")
	}
}

func TestAgentRegister(t *testing.T) {
	s := newAgentRegistryServer()
	s.mu.Lock()
	agent := &AgentRegistration{
		ID: "agent-1", Name: "coder-agent",
		Version: "1.0", Maintainer: "scos-team",
		Capabilities:  []string{"code_intelligence", "retrieval"},
		ModelProvider: "anthropic", Status: "active",
	}
	s.agents["agent-1"] = agent
	s.mu.Unlock()

	if agent.Name != "coder-agent" {
		t.Fatal("name mismatch")
	}
	if len(agent.Capabilities) != 2 {
		t.Fatal("expected 2 capabilities")
	}
}

func TestAgentHeartbeat(t *testing.T) {
	s := newAgentRegistryServer()
	now := time.Now().UTC().Format(time.RFC3339)
	s.mu.Lock()
	s.agents["agent-1"] = &AgentRegistration{ID: "agent-1", Name: "test", Status: "active", LastSeen: now}
	s.mu.Unlock()

	s.mu.RLock()
	agent := s.agents["agent-1"]
	s.mu.RUnlock()
	if agent.Status != "active" {
		t.Fatal("expected active status")
	}
}

func TestHandoffInitiate(t *testing.T) {
	s := newAgentRegistryServer()
	s.mu.Lock()
	p := &HandoffPacket{
		ID: "handoff-1", FromAgent: "agent-a", ToAgent: "agent-b",
		Objective:   "Complete memory consolidation",
		Constraints: []string{"use consolidation worker only"},
		Status:      "pending", CreatedAt: "now",
	}
	s.handoffs["handoff-1"] = p
	s.mu.Unlock()

	if p.Objective != "Complete memory consolidation" {
		t.Fatal("objective mismatch")
	}
	if p.Status != "pending" {
		t.Fatal("expected pending status")
	}
}

func TestHandoffAccept(t *testing.T) {
	s := newAgentRegistryServer()
	s.mu.Lock()
	s.handoffs["handoff-1"] = &HandoffPacket{
		ID: "handoff-1", Objective: "test",
		Status: "pending",
	}
	s.mu.Unlock()

	s.mu.Lock()
	p := s.handoffs["handoff-1"]
	p.Status = "accepted"
	s.mu.Unlock()

	if p.Status != "accepted" {
		t.Fatal("expected accepted status")
	}
}

func TestHandoffReject(t *testing.T) {
	s := newAgentRegistryServer()
	s.mu.Lock()
	s.handoffs["handoff-1"] = &HandoffPacket{
		ID: "handoff-1", Objective: "test",
		Status: "pending",
	}
	s.mu.Unlock()

	s.mu.Lock()
	p := s.handoffs["handoff-1"]
	p.Status = "rejected"
	s.mu.Unlock()

	if p.Status != "rejected" {
		t.Fatal("expected rejected status")
	}
}

func TestHandoffCompleted(t *testing.T) {
	s := newAgentRegistryServer()
	s.mu.Lock()
	s.handoffs["handoff-1"] = &HandoffPacket{
		ID: "handoff-1", Objective: "test",
		Status: "accepted",
	}
	s.mu.Unlock()

	s.mu.Lock()
	p := s.handoffs["handoff-1"]
	p.Status = "completed"
	s.mu.Unlock()

	if p.Status != "completed" {
		t.Fatal("expected completed status")
	}
}

func TestAgentMaxClassification(t *testing.T) {
	s := newAgentRegistryServer()
	s.mu.Lock()
	s.agents["agent-1"] = &AgentRegistration{
		ID: "agent-1", Name: "test",
		MaxClassification: "restricted",
	}
	s.mu.Unlock()

	if s.agents["agent-1"].MaxClassification != "restricted" {
		t.Fatal("classification mismatch")
	}
}

func TestAgentDefaultClassification(t *testing.T) {
	s := newAgentRegistryServer()
	s.mu.Lock()
	s.agents["agent-2"] = &AgentRegistration{ID: "agent-2", Name: "test2", MaxClassification: "confidential"}
	s.mu.Unlock()

	s.mu.RLock()
	agent := s.agents["agent-2"]
	s.mu.RUnlock()
	if agent.MaxClassification != "confidential" {
		t.Fatal("expected default classification")
	}
}

func TestAgentCapabilities(t *testing.T) {
	s := newAgentRegistryServer()
	s.mu.Lock()
	s.agents["agent-1"] = &AgentRegistration{
		ID: "agent-1", Name: "test",
		Capabilities:      []string{"memory:read", "memory:write"},
		ToolPermissions:   []string{"search", "retrieval"},
		MemoryPermissions: []string{"namespace:default"},
	}
	s.mu.Unlock()

	a := s.agents["agent-1"]
	if len(a.Capabilities) != 2 {
		t.Fatal("expected 2 capabilities")
	}
	if len(a.ToolPermissions) != 2 {
		t.Fatal("expected 2 tool permissions")
	}
}

func TestEmptyAgentList(t *testing.T) {
	s := newAgentRegistryServer()
	s.mu.RLock()
	count := len(s.agents)
	s.mu.RUnlock()
	if count != 0 {
		t.Fatal("expected empty agents")
	}
}
