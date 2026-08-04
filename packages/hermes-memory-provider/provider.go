package hermesmemoryprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

type Turn struct {
	SessionID string    `json:"session_id"`
	TurnID    string    `json:"turn_id"`
	Messages  []any     `json:"messages"`
	ToolCalls []any     `json:"tool_calls,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Provider implements first-class Hermes memory provider interface (§24.1).
type Provider struct {
	mu        sync.RWMutex
	profile   HermesProfile
	memories  map[string]*MemoryEntry
	turns     []Turn
	storePath string
}

type persistedProvider struct {
	Memories map[string]*MemoryEntry `json:"memories"`
	Turns    []Turn                  `json:"turns"`
}

// NewProvider initializes a new Hermes Memory Provider.
func NewProvider(profile HermesProfile) (*Provider, error) {
	return NewProviderWithStore(profile, os.Getenv("SCOS_HERMES_MEMORY_STORE_PATH"))
}

// NewProviderWithStore creates a provider backed by an optional atomic local JSON store.
func NewProviderWithStore(profile HermesProfile, storePath string) (*Provider, error) {
	if profile.ProfileID == "" || profile.AgentID == "" {
		return nil, errors.New("profile_id and agent_id are required")
	}
	p := &Provider{
		profile:   profile,
		memories:  make(map[string]*MemoryEntry),
		storePath: storePath,
	}
	p.load()
	return p, nil
}

func (p *Provider) load() {
	if p.storePath == "" {
		return
	}
	b, err := os.ReadFile(p.storePath)
	if err != nil {
		return
	}
	var all map[string]persistedProvider
	if json.Unmarshal(b, &all) != nil {
		return
	}
	state, ok := all[p.profile.ProfileID+"/"+p.profile.AgentID]
	if !ok {
		return
	}
	if state.Memories != nil {
		p.memories = state.Memories
	}
	p.turns = state.Turns
}

func (p *Provider) persistLocked() error {
	if p.storePath == "" {
		return nil
	}
	all := make(map[string]persistedProvider)
	if b, err := os.ReadFile(p.storePath); err == nil {
		_ = json.Unmarshal(b, &all)
	}
	all[p.profile.ProfileID+"/"+p.profile.AgentID] = persistedProvider{p.memories, p.turns}
	b, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(p.storePath), 0755); err != nil {
		return err
	}
	tmp := p.storePath + ".tmp"
	if err = os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, p.storePath)
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
	if err := p.persistLocked(); err != nil {
		return nil, err
	}
	return entry, nil
}

// ReadMemory retrieves a key-value memory entry by key.
func (p *Provider) ReadMemory(ctx context.Context, key string) (*MemoryEntry, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	entry, found := p.memories[key]
	return entry, found
}

// SyncTurn durably records a turn once for this provider profile.
func (p *Provider) SyncTurn(ctx context.Context, turn Turn) (bool, error) {
	if turn.SessionID == "" || turn.TurnID == "" {
		return false, errors.New("session_id and turn_id are required")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, existing := range p.turns {
		if existing.SessionID == turn.SessionID && existing.TurnID == turn.TurnID {
			return false, nil
		}
	}
	if turn.CreatedAt.IsZero() {
		turn.CreatedAt = time.Now().UTC()
	}
	p.turns = append(p.turns, turn)
	if err := p.persistLocked(); err != nil {
		return false, err
	}
	return true, nil
}

// Recall returns stored turns matching the query, scoped to this profile.
func (p *Provider) Recall(ctx context.Context, query string, maxItems int) []Turn {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if maxItems <= 0 {
		maxItems = 20
	}
	result := make([]Turn, 0, maxItems)
	for i := len(p.turns) - 1; i >= 0 && len(result) < maxItems; i-- {
		turn := p.turns[i]
		if query != "" && !containsTurn(turn, query) {
			continue
		}
		result = append(result, turn)
	}
	return result
}

func containsTurn(turn Turn, query string) bool {
	b, _ := json.Marshal(turn.Messages)
	if stringContains(string(b), query) {
		return true
	}
	b, _ = json.Marshal(turn.ToolCalls)
	return stringContains(string(b), query)
}

func stringContains(value, query string) bool {
	for i := 0; i+len(query) <= len(value); i++ {
		if value[i:i+len(query)] == query {
			return true
		}
	}
	return false
}
