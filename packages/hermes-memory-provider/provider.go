package hermesmemoryprovider

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// HermesProfile defines Hermes agent memory profile isolation (§24.6).
type HermesProfile struct {
	ProfileID   string   `json:"profile_id"`
	AgentID     string   `json:"agent_id"`
	Namespace   string   `json:"namespace"`
	Permissions []string `json:"permissions"`
}

// MemoryEntry represents a memory item mapped into Hermes runtime.
type MemoryEntry struct {
	ID        string    `json:"id"`
	Key       string    `json:"key"`
	Value     string    `json:"value"`
	Scope     string    `json:"scope"`
	CreatedAt time.Time `json:"created_at"`
}

// Provider implements first-class Hermes memory provider interface (§24.1).
type Provider struct {
	mu       sync.RWMutex
	profile  HermesProfile
	memories map[string]*MemoryEntry
}

// NewProvider initializes a new Hermes Memory Provider.
func NewProvider(profile HermesProfile) (*Provider, error) {
	if profile.ProfileID == "" || profile.AgentID == "" {
		return nil, errors.New("profile_id and agent_id are required")
	}
	return &Provider{
		profile:  profile,
		memories: make(map[string]*MemoryEntry),
	}, nil
}

// WriteMemory records a key-value memory entry into Hermes profile scope.
func (p *Provider) WriteMemory(ctx context.Context, key, value, scope string) (*MemoryEntry, error) {
	if key == "" {
		return nil, errors.New("key is required")
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	entry := &MemoryEntry{
		ID:        fmt.Sprintf("hermes-mem-%d", len(p.memories)+1),
		Key:       key,
		Value:     value,
		Scope:     scope,
		CreatedAt: time.Now().UTC(),
	}
	p.memories[key] = entry
	return entry, nil
}

// ReadMemory retrieves a key-value memory entry by key.
func (p *Provider) ReadMemory(ctx context.Context, key string) (*MemoryEntry, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	entry, found := p.memories[key]
	return entry, found
}
