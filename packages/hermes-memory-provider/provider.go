package hermesmemoryprovider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Profile permissions. An empty HermesProfile.Permissions list is
// unrestricted; a non-empty list must grant the operation (or "admin"),
// matching the memory scopes SCOS claims tokens carry.
const (
	PermissionMemoryRead  = "memory:read"
	PermissionMemoryWrite = "memory:write"
)

// ErrPermissionDenied is returned when the profile lacks the permission an
// operation needs.
var ErrPermissionDenied = errors.New("hermes profile lacks permission")

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
	Config   map[string]string       `json:"config,omitempty"`
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
	if err := p.load(); err != nil {
		return nil, err
	}
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
		if err := p.load(); err != nil {
			return err
		}
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

// QueuePrefetch queues a completed turn without blocking the caller on
// disk I/O. Queued turns are recorded durably (deduplicated like SyncTurn)
// at the next turn start, session end or shutdown. It returns false when the
// turn is invalid or the queue is full.
func (p *Provider) QueuePrefetch(_ context.Context, turn Turn) bool {
	if turn.SessionID == "" || turn.TurnID == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.allowed(PermissionMemoryWrite) || len(p.queue) >= 256 {
		return false
	}
	p.queue = append(p.queue, turn)
	return true
}

// drainQueueLocked moves queued turns into the durable turn log and
// persists. On a persist failure the turns stay queued for the next drain.
func (p *Provider) drainQueueLocked() error {
	if len(p.queue) == 0 {
		return nil
	}
	before := len(p.turns)
	for _, turn := range p.queue {
		p.appendTurnLocked(turn)
	}
	if err := p.persistLocked(); err != nil {
		p.turns = p.turns[:before]
		return err
	}
	p.queue = nil
	return nil
}

// appendTurnLocked records turn unless the same (session, turn) exists.
func (p *Provider) appendTurnLocked(turn Turn) bool {
	for _, existing := range p.turns {
		if existing.SessionID == turn.SessionID && existing.TurnID == turn.TurnID {
			return false
		}
	}
	if turn.CreatedAt.IsZero() {
		turn.CreatedAt = time.Now().UTC()
	}
	p.turns = append(p.turns, turn)
	return true
}

func (p *Provider) allowed(permission string) bool {
	return len(p.profile.Permissions) == 0 ||
		slices.Contains(p.profile.Permissions, permission) ||
		slices.Contains(p.profile.Permissions, "admin")
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
		return map[string]any{"provider": p.Name(), "available": p.initialized, "queue_depth": len(p.queue), "memories": len(p.memories), "turns": len(p.turns)}, nil
	case "scos_explain":
		entry, err := p.memoryByID(stringArg(args, "memory_id"))
		if err != nil {
			return nil, err
		}
		return map[string]any{"memory": entry, "source": "hermes-local-store", "profile_id": p.profile.ProfileID, "agent_id": p.profile.AgentID, "namespace": p.profile.Namespace}, nil
	case "scos_correct":
		return p.CorrectMemory(ctx, stringArg(args, "memory_id"), stringArg(args, "value"))
	case "scos_forget":
		if err := p.ForgetMemory(ctx, stringArg(args, "memory_id")); err != nil {
			return nil, err
		}
		return map[string]any{"forgotten": stringArg(args, "memory_id")}, nil
	case "scos_user_model":
		return p.memoriesInScopes("user", "preference", "profile"), nil
	case "scos_memory_graph":
		return p.MemoryGraph(), nil
	default:
		return nil, fmt.Errorf("unknown tool %s", name)
	}
}

// memoryByID returns a copy of the memory with the given ID.
func (p *Provider) memoryByID(id string) (MemoryEntry, error) {
	if id == "" {
		return MemoryEntry{}, errors.New("memory_id is required")
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if !p.allowed(PermissionMemoryRead) {
		return MemoryEntry{}, fmt.Errorf("%w: %s", ErrPermissionDenied, PermissionMemoryRead)
	}
	for _, entry := range p.memories {
		if entry.ID == id {
			return *entry, nil
		}
	}
	return MemoryEntry{}, fmt.Errorf("memory %s not found", id)
}

// keyForIDLocked returns the store key holding memory id.
func (p *Provider) keyForIDLocked(id string) (string, bool) {
	for key, entry := range p.memories {
		if entry.ID == id {
			return key, true
		}
	}
	return "", false
}

// CorrectMemory replaces the value of an existing memory, keeping its ID.
func (p *Provider) CorrectMemory(ctx context.Context, id, value string) (*MemoryEntry, error) {
	if id == "" || value == "" {
		return nil, errors.New("memory_id and value are required")
	}
	p.mu.RLock()
	key, ok := p.keyForIDLocked(id)
	var scope string
	if ok {
		scope = p.memories[key].Scope
	}
	p.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("memory %s not found", id)
	}
	return p.WriteMemory(ctx, key, value, scope)
}

// ForgetMemory deletes a memory by ID and persists the removal.
func (p *Provider) ForgetMemory(_ context.Context, id string) error {
	if id == "" {
		return errors.New("memory_id is required")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.allowed(PermissionMemoryWrite) {
		return fmt.Errorf("%w: %s", ErrPermissionDenied, PermissionMemoryWrite)
	}
	key, ok := p.keyForIDLocked(id)
	if !ok {
		return fmt.Errorf("memory %s not found", id)
	}
	previous := p.memories[key]
	delete(p.memories, key)
	if err := p.persistLocked(); err != nil {
		p.memories[key] = previous
		return err
	}
	return nil
}

// memoriesInScopes returns copies of memories whose scope is one of scopes,
// ordered by key.
func (p *Provider) memoriesInScopes(scopes ...string) []MemoryEntry {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if !p.allowed(PermissionMemoryRead) {
		return nil
	}
	out := []MemoryEntry{}
	for _, entry := range p.memories {
		if slices.Contains(scopes, entry.Scope) {
			out = append(out, *entry)
		}
	}
	slices.SortFunc(out, func(a, b MemoryEntry) int { return strings.Compare(a.Key, b.Key) })
	return out
}

// MemoryGraph returns memories as nodes plus the session-lineage edges
// recorded by OnSessionSwitch ("lineage:<to>" = "<from>:<reason>").
func (p *Provider) MemoryGraph() map[string]any {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if !p.allowed(PermissionMemoryRead) {
		return map[string]any{"nodes": []any{}, "edges": []any{}}
	}
	nodes := make([]map[string]any, 0, len(p.memories))
	edges := []map[string]any{}
	for key, entry := range p.memories {
		nodes = append(nodes, map[string]any{"id": entry.ID, "key": key, "scope": entry.Scope})
		if to, ok := strings.CutPrefix(key, "lineage:"); ok {
			from, reason, _ := strings.Cut(entry.Value, ":")
			edges = append(edges, map[string]any{"from": from, "to": to, "type": "session_switch", "reason": reason})
		}
	}
	slices.SortFunc(nodes, func(a, b map[string]any) int { return strings.Compare(a["key"].(string), b["key"].(string)) })
	return map[string]any{"nodes": nodes, "edges": edges, "turns": len(p.turns)}
}

// Shutdown drains queued turns and persists. "pending" is the number of
// turns that could not be recorded and "error" the reason, if any.
func (p *Provider) Shutdown(_ context.Context) map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	result := map[string]any{"pending": 0}
	err := p.drainQueueLocked()
	if err == nil {
		err = p.persistLocked()
	}
	if err != nil {
		result["pending"] = len(p.queue)
		result["error"] = err.Error()
	}
	p.initialized = false
	return result
}
func (p *Provider) OnTurnStart(ctx context.Context, sessionID, turnID string) []Turn {
	p.mu.Lock()
	_ = p.drainQueueLocked() // on failure turns stay queued; Shutdown reports them
	p.mu.Unlock()
	return p.Prefetch(ctx, "", 20)
}

// OnSessionEnd drains queued turns and persists; the error is returned so
// the runtime can surface a failed flush.
func (p *Provider) OnSessionEnd(_ context.Context, _ string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.drainQueueLocked(); err != nil {
		return err
	}
	return p.persistLocked()
}
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

// Config returns the persisted provider configuration (nil if unset).
func (p *Provider) Config() map[string]string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.config == nil {
		return nil
	}
	out := make(map[string]string, len(p.config))
	for k, v := range p.config {
		out[k] = v
	}
	return out
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

func (p *Provider) storeKey() string {
	return p.profile.Namespace + "/" + p.profile.ProfileID + "/" + p.profile.AgentID
}

// legacyStoreKey is the pre-namespace key, read so existing stores keep
// their data; the next write moves it under storeKey.
func (p *Provider) legacyStoreKey() string {
	return p.profile.ProfileID + "/" + p.profile.AgentID
}

// readStore parses the whole store file. A missing file is an empty store;
// an unreadable or corrupt one is an error, never silently treated as empty
// (the next write would otherwise erase every other profile's data).
func (p *Provider) readStore() (map[string]persistedProvider, error) {
	all := make(map[string]persistedProvider)
	b, err := os.ReadFile(p.storePath)
	if errors.Is(err, os.ErrNotExist) {
		return all, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read hermes memory store: %w", err)
	}
	if err := json.Unmarshal(b, &all); err != nil {
		return nil, fmt.Errorf("hermes memory store %s is corrupt, refusing to use it: %w", p.storePath, err)
	}
	return all, nil
}

func (p *Provider) load() error {
	if p.storePath == "" {
		return nil
	}
	all, err := p.readStore()
	if err != nil {
		return err
	}
	state, ok := all[p.storeKey()]
	if !ok && p.profile.Namespace != "" {
		state, ok = all[p.legacyStoreKey()]
	}
	if !ok {
		return nil
	}
	if state.Memories != nil {
		p.memories = state.Memories
	}
	p.turns = state.Turns
	p.config = state.Config
	return nil
}

const storeLockTimeout = 10 * time.Second

// lockStore takes an exclusive lock file beside the store so concurrent
// processes serialise their read-modify-write. A lock older than
// storeLockTimeout is treated as abandoned by a crashed writer.
func (p *Provider) lockStore() (func(), error) {
	lock := p.storePath + ".lock"
	deadline := time.Now().Add(storeLockTimeout)
	for {
		f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err == nil {
			_ = f.Close()
			return func() { _ = os.Remove(lock) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("lock hermes memory store: %w", err)
		}
		if info, statErr := os.Stat(lock); statErr == nil && time.Since(info.ModTime()) > storeLockTimeout {
			_ = os.Remove(lock)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("lock hermes memory store: timed out waiting for %s", lock)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (p *Provider) persistLocked() error {
	if p.storePath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p.storePath), 0755); err != nil {
		return err
	}
	unlock, err := p.lockStore()
	if err != nil {
		return err
	}
	defer unlock()
	all, err := p.readStore()
	if err != nil {
		return err
	}
	if p.profile.Namespace != "" {
		delete(all, p.legacyStoreKey())
	}
	all[p.storeKey()] = persistedProvider{Memories: p.memories, Turns: p.turns, Config: p.config}
	b, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p.storePath), filepath.Base(p.storePath)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err = tmp.Write(b); err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p.storePath)
}

func newMemoryID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "hermes-mem-" + hex.EncodeToString(b[:]), nil
}

// WriteMemory records a key-value memory entry into Hermes profile scope.
func (p *Provider) WriteMemory(ctx context.Context, key, value, scope string) (*MemoryEntry, error) {
	if key == "" {
		return nil, errors.New("key is required")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.allowed(PermissionMemoryWrite) {
		return nil, fmt.Errorf("%w: %s", ErrPermissionDenied, PermissionMemoryWrite)
	}

	// Rewriting a key keeps its ID so references to the memory stay valid.
	previous, existed := p.memories[key]
	id := ""
	if existed {
		id = previous.ID
	} else {
		var err error
		if id, err = newMemoryID(); err != nil {
			return nil, fmt.Errorf("generate memory id: %w", err)
		}
	}
	entry := &MemoryEntry{ID: id, Key: key, Value: value, Scope: scope, CreatedAt: time.Now().UTC()}
	p.memories[key] = entry
	if err := p.persistLocked(); err != nil {
		if existed {
			p.memories[key] = previous
		} else {
			delete(p.memories, key)
		}
		return nil, err
	}
	return entry, nil
}

// ReadMemory retrieves a key-value memory entry by key.
func (p *Provider) ReadMemory(ctx context.Context, key string) (*MemoryEntry, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if !p.allowed(PermissionMemoryRead) {
		return nil, false
	}
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
	if !p.allowed(PermissionMemoryWrite) {
		return false, fmt.Errorf("%w: %s", ErrPermissionDenied, PermissionMemoryWrite)
	}
	if !p.appendTurnLocked(turn) {
		return false, nil
	}
	if err := p.persistLocked(); err != nil {
		p.turns = p.turns[:len(p.turns)-1]
		return false, err
	}
	return true, nil
}

// Recall returns stored turns matching the query, scoped to this profile.
func (p *Provider) Recall(ctx context.Context, query string, maxItems int) []Turn {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if !p.allowed(PermissionMemoryRead) {
		return nil
	}
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

func stringContains(value, query string) bool { return strings.Contains(value, query) }
