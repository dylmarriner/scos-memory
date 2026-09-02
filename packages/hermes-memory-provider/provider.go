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
	mu          sync.RWMutex
	profile     HermesProfile
	memories    map[string]*MemoryEntry
	turns       []Turn
	storePath   string
	initialized bool
	queue       []Turn
	config      map[string]string
}

// ProviderConfig is deliberately limited to the Hermes/SCOS setup contract.
// BaseURL is the SCOS memory service endpoint, never an LLM/inference endpoint;
// model-provider selection belongs to the Hermes runtime and can change
// without changing memory identity or persistence. Auth values are references
// to Hermes-managed secrets, never secret material.
type ProviderConfig struct{ BaseURL, ProfileScope, TenantID, PrincipalID, Workspace, TransportSecurity, OptionalAuthReference string }

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
	p.initialized = true
	return p, nil
}

func (p *Provider) Name() string { return "scos-nexus" }
func (p *Provider) IsAvailable() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.initialized && p.profile.ProfileID != "" && p.profile.AgentID != ""
}
func (p *Provider) Initialize(profilePath string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if profilePath != "" {
		p.storePath = filepath.Join(profilePath, "scos-nexus-memory.json")
		p.load()
	}
	p.initialized = true
	return nil
}
func (p *Provider) SystemPromptBlock() string {
	if !p.IsAvailable() {
		return ""
	}
	return "SCOS Nexus memory is active; recall and persistence are automatic."
}
func (p *Provider) Prefetch(ctx context.Context, query string, maxItems int) []Turn {
	return p.Recall(ctx, query, maxItems)
}
func (p *Provider) QueuePrefetch(_ context.Context, turn Turn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.queue) >= 256 {
		return false
	}
	p.queue = append(p.queue, turn)
	return true
}
func (p *Provider) GetToolSchemas() []map[string]any {
	return []map[string]any{{"name": "scos_recall"}, {"name": "scos_explain"}, {"name": "scos_user_model"}, {"name": "scos_memory_graph"}, {"name": "scos_correct"}, {"name": "scos_forget"}, {"name": "scos_status"}}
}
func (p *Provider) HandleToolCall(ctx context.Context, name string, args map[string]any) (any, error) {
	switch name {
	case "scos_recall":
		return p.Recall(ctx, stringArg(args, "query"), intArg(args, "max_items", 20)), nil
	case "scos_status":
		p.mu.RLock()
		defer p.mu.RUnlock()
		return map[string]any{"provider": p.Name(), "available": p.initialized, "queue_depth": len(p.queue)}, nil
	default:
		return nil, fmt.Errorf("tool %s is delegated to the authoritative SCOS service", name)
	}
}
func (p *Provider) Shutdown(_ context.Context) map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	pending := len(p.queue)
	p.queue = nil
	p.persistLocked()
	p.initialized = false
	return map[string]any{"pending": pending}
}
func (p *Provider) OnTurnStart(ctx context.Context, sessionID, turnID string) []Turn {
	return p.Prefetch(ctx, "", 20)
}
func (p *Provider) OnSessionEnd(_ context.Context, _ string) { p.persistLocked() }
func (p *Provider) OnSessionSwitch(_ context.Context, fromSession, toSession, reason string) error {
	_, err := p.WriteMemory(context.Background(), "lineage:"+toSession, fromSession+":"+reason, "session")
	return err
}
func (p *Provider) OnPreCompress(ctx context.Context, messages []any) error {
	_, err := p.WriteMemory(ctx, "precompress", fmt.Sprint(messages), "episodic")
	return err
}
func (p *Provider) OnMemoryWrite(ctx context.Context, key, value, scope string) error {
	_, err := p.WriteMemory(ctx, key, value, scope)
	return err
}
func (p *Provider) OnDelegation(ctx context.Context, parentSession, subagent, observation string) error {
	_, err := p.WriteMemory(ctx, "delegation:"+parentSession+":"+subagent, observation, "episodic")
	return err
}
func (p *Provider) BackupPaths() []string {
	if p.storePath == "" {
		return nil
	}
	return []string{p.storePath}
}
func (p *Provider) GetConfigSchema() map[string]any {
	return map[string]any{"required": []string{"base_url", "tenant_id", "principal_id"}, "secret": "optional_auth_reference"}
}
func (p *Provider) SaveConfig(config ProviderConfig) error {
	if config.BaseURL == "" || config.TenantID == "" || config.PrincipalID == "" {
		return errors.New("base_url, tenant_id and principal_id are required")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.config = map[string]string{"base_url": config.BaseURL, "profile_scope": config.ProfileScope, "tenant_id": config.TenantID, "principal_id": config.PrincipalID, "workspace": config.Workspace, "transport_security": config.TransportSecurity, "optional_auth_reference": config.OptionalAuthReference}
	return p.persistLocked()
}

func stringArg(args map[string]any, key string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	return ""
}
func intArg(args map[string]any, key string, fallback int) int {
	if v, ok := args[key].(float64); ok && int(v) > 0 {
		return int(v)
	}
	return fallback
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
