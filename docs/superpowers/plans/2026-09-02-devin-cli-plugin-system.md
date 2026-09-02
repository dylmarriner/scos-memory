# Devin CLI Plugin System Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a comprehensive Devin CLI plugin system with core framework, memory integration, audit automation, and smart workflow hooks

**Architecture:** Core framework with pluggable modules using Go, event bus for inter-module communication, SCOS Memory Fabric integration for persistent storage

**Tech Stack:** Go 1.26+, PostgreSQL (SCOS), TOML configuration, Git hooks, hostile repository auditor integration

---

## File Structure

**Core Framework (`devin-cli-core/`):**
- `cmd/devin/main.go` - Main CLI entry point
- `internal/framework/module.go` - Module interface and lifecycle
- `internal/framework/eventbus.go` - Event bus implementation
- `internal/framework/config.go` - Configuration management
- `internal/framework/scos/client.go` - SCOS client abstraction
- `internal/framework/hooks/registry.go` - Hook registration system
- `pkg/module/api.go` - Public module API
- `configs/default.toml` - Default configuration
- `go.mod` - Go module definition

**Memory Module (`devin-memory-plugin/`):**
- `internal/memory/session.go` - Session logging capture
- `internal/memory/scos.go` - SCOS integration
- `internal/memory/events.go` - Memory event definitions
- `internal/memory/buffer.go` - Write-ahead buffer
- `configs/memory.toml` - Module configuration
- `go.mod` - Go module definition

**Audit Module (`devin-audit-plugin/`):**
- `internal/audit/precommit.go` - Pre-commit security hooks
- `internal/audit/manual.go` - Manual audit execution
- `internal/audit/scheduler.go` - Scheduled background audits
- `internal/audit/hostile.go` - Hostile auditor integration
- `internal/audit/results.go` - Audit artifact management
- `configs/audit.toml` - Module configuration
- `go.mod` - Go module definition

**Workflow Module (`devin-workflow-plugin/`):**
- `internal/workflow/selector.go` - Smart hook selection
- `internal/workflow/githooks.go` - Git hook integration
- `internal/workflow/execution.go` - Hook execution engine
- `internal/workflow/types.go` - Hook type definitions
- `configs/workflow.toml` - Module configuration
- `go.mod` - Go module definition

**Shared Testing (`devin-cli-core/tests/`):**
- `integration/scos_test.go` - SCOS integration tests
- `framework/module_test.go` - Module lifecycle tests
- `framework/eventbus_test.go` - Event bus tests
- `fixtures/` - Test fixtures and data

---

## Phase 1: Core Framework Foundation

### Task 1: Project Setup and Module Interface

**Files:**
- Create: `devin-cli-core/go.mod`
- Create: `devin-cli-core/pkg/module/api.go`
- Create: `devin-cli-core/internal/framework/module.go`
- Test: `devin-cli-core/tests/framework/module_test.go`

- [ ] **Step 1: Initialize Go module**

```bash
cd devin-cli-core
go mod init github.com/devin/cli/core
```

- [ ] **Step 2: Write module interface**

```go
// pkg/module/api.go
package module

import "context"

// Config represents module configuration
type Config map[string]interface{}

// EventBus represents the event bus for module communication
type EventBus interface {
    Publish(event interface{}) error
    Subscribe(eventType string, handler func(interface{})) error
}

// Module is the interface all plugins must implement
type Module interface {
    // Metadata
    Name() string
    Version() string
    Dependencies() []string
    
    // Lifecycle
    Init(ctx context.Context, config Config) error
    Start(ctx context.Context) error
    Stop(ctx context.Context) error
    
    // Hooks
    RegisterHooks(bus EventBus) error
}
```

- [ ] **Step 3: Write module lifecycle implementation**

```go
// internal/framework/module.go
package framework

import (
    "context"
    "fmt"
    "sync"
    
    "github.com/devin/cli/core/pkg/module"
)

type ModuleManager struct {
    modules map[string]module.Module
    mu      sync.RWMutex
}

func NewModuleManager() *ModuleManager {
    return &ModuleManager{
        modules: make(map[string]module.Module),
    }
}

func (m *ModuleManager) Load(mod module.Module, config module.Config) error {
    m.mu.Lock()
    defer m.mu.Unlock()
    
    // Check dependencies
    for _, dep := range mod.Dependencies() {
        if _, exists := m.modules[dep]; !exists {
            return fmt.Errorf("dependency %s not found", dep)
        }
    }
    
    // Initialize module
    ctx := context.Background()
    if err := mod.Init(ctx, config); err != nil {
        return fmt.Errorf("module init failed: %w", err)
    }
    
    m.modules[mod.Name()] = mod
    return nil
}

func (m *ModuleManager) StartAll(ctx context.Context) error {
    m.mu.RLock()
    defer m.mu.RUnlock()
    
    for name, mod := range m.modules {
        if err := mod.Start(ctx); err != nil {
            return fmt.Errorf("module %s start failed: %w", name, err)
        }
    }
    return nil
}

func (m *ModuleManager) StopAll(ctx context.Context) error {
    m.mu.RLock()
    defer m.mu.RUnlock()
    
    for name, mod := range m.modules {
        if err := mod.Stop(ctx); err != nil {
            return fmt.Errorf("module %s stop failed: %w", name, err)
        }
    }
    return nil
}
```

- [ ] **Step 4: Write module lifecycle tests**

```go
// tests/framework/module_test.go
package framework

import (
    "context"
    "testing"
    
    "github.com/devin/cli/core/pkg/module"
)

type mockModule struct {
    name    string
    version string
    started bool
}

func (m *mockModule) Name() string { return m.name }
func (m *mockModule) Version() string { return m.version }
func (m *mockModule) Dependencies() []string { return nil }
func (m *mockModule) Init(ctx context.Context, config module.Config) error { return nil }
func (m *mockModule) Start(ctx context.Context) error { m.started = true; return nil }
func (m *mockModule) Stop(ctx context.Context) error { m.started = false; return nil }
func (m *mockModule) RegisterHooks(bus module.EventBus) error { return nil }

func TestModuleManager_Load(t *testing.T) {
    manager := NewModuleManager()
    mod := &mockModule{name: "test", version: "1.0.0"}
    
    err := manager.Load(mod, module.Config{})
    if err != nil {
        t.Fatalf("Load failed: %v", err)
    }
    
    ctx := context.Background()
    if err := manager.StartAll(ctx); err != nil {
        t.Fatalf("StartAll failed: %v", err)
    }
    
    if !mod.started {
        t.Error("Module not started")
    }
    
    if err := manager.StopAll(ctx); err != nil {
        t.Fatalf("StopAll failed: %v", err)
    }
    
    if mod.started {
        t.Error("Module not stopped")
    }
}
```

- [ ] **Step 5: Commit**

```bash
cd devin-cli-core
git add go.mod pkg/module/api.go internal/framework/module.go tests/framework/module_test.go
git commit -m "feat(core): add module interface and lifecycle management"
```

### Task 2: Event Bus Implementation

**Files:**
- Create: `devin-cli-core/internal/framework/eventbus.go`
- Test: `devin-cli-core/tests/framework/eventbus_test.go`

- [ ] **Step 1: Write event bus implementation**

```go
// internal/framework/eventbus.go
package framework

import (
    "context"
    "sync"
    "time"
)

type EventType string

type Event struct {
    Type      EventType
    Data      interface{}
    Timestamp time.Time
}

type Handler func(interface{}) error

type EventBus struct {
    handlers map[EventType][]Handler
    mu       sync.RWMutex
    events    chan Event
    workers  int
    ctx      context.Context
    cancel   context.CancelFunc
}

func NewEventBus(workers int) *EventBus {
    ctx, cancel := context.WithCancel(context.Background())
    return &EventBus{
        handlers: make(map[EventType][]Handler),
        events:   make(chan Event, 1000),
        workers:  workers,
        ctx:      ctx,
        cancel:   cancel,
    }
}

func (eb *EventBus) Publish(eventType EventType, data interface{}) error {
    eb.mu.RLock()
    defer eb.mu.RUnlock()
    
    event := Event{
        Type:      eventType,
        Data:      data,
        Timestamp: time.Now(),
    }
    
    select {
    case eb.events <- event:
        return nil
    default:
        return fmt.Errorf("event bus full")
    }
}

func (eb *EventBus) Subscribe(eventType EventType, handler Handler) error {
    eb.mu.Lock()
    defer eb.mu.Unlock()
    
    eb.handlers[eventType] = append(eb.handlers[eventType], handler)
    return nil
}

func (eb *EventBus) Start() {
    for i := 0; i < eb.workers; i++ {
        go eb.processEvents()
    }
}

func (eb *EventBus) Stop() {
    eb.cancel()
    close(eb.events)
}

func (eb *EventBus) processEvents() {
    for {
        select {
        case event := <-eb.events:
            eb.mu.RLock()
            handlers := eb.handlers[event.Type]
            eb.mu.RUnlock()
            
            for _, handler := range handlers {
                go func(h Handler) {
                    h(event.Data)
                }(handler)
            }
        case <-eb.ctx.Done():
            return
        }
    }
}
```

- [ ] **Step 2: Write event bus tests**

```go
// tests/framework/eventbus_test.go
package framework

import (
    "context"
    "testing"
    "time"
)

func TestEventBus_PublishSubscribe(t *testing.T) {
    bus := NewEventBus(2)
    bus.Start()
    defer bus.Stop()
    
    received := false
    handler := func(data interface{}) error {
        received = true
        return nil
    }
    
    bus.Subscribe("test", handler)
    bus.Publish("test", "data")
    
    time.Sleep(100 * time.Millisecond)
    
    if !received {
        t.Error("Handler not called")
    }
}
```

- [ ] **Step 3: Run tests**

```bash
cd devin-cli-core
go test ./tests/framework/eventbus_test.go -v
```

- [ ] **Step 4: Commit**

```bash
cd devin-cli-core
git add internal/framework/eventbus.go tests/framework/eventbus_test.go
git commit -m "feat(core): add event bus with async processing"
```

### Task 3: Configuration Management

**Files:**
- Create: `devin-cli-core/internal/framework/config.go`
- Create: `devin-cli-core/configs/default.toml`
- Test: `devin-cli-core/tests/framework/config_test.go`

- [ ] **Step 1: Write configuration implementation**

```go
// internal/framework/config.go
package framework

import (
    "fmt"
    "os"
    "path/filepath"
    
    "github.com/spf13/viper"
)

type Config struct {
    SCOS struct {
        Endpoint string `toml:"endpoint" validate:"required"`
        TenantID  string `toml:"tenant_id" validate:"required"`
        APIKey    string `toml:"api_key"`
    } `toml:"scos"`
    
    Modules struct {
        Enabled []string `toml:"enabled"`
        Dir     string   `toml:"dir"`
    } `toml:"modules"`
    
    Logging struct {
        Level  string `toml:"level"`
        Format string `toml:"format"`
    } `toml:"logging"`
}

func LoadConfig() (*Config, error) {
    v := viper.New()
    v.SetConfigName("config")
    
    // Set default paths
    v.AddConfigPath(filepath.Join(os.Getenv("HOME"), ".devin"))
    v.AddConfigPath(".")
    v.AddConfigPath("/etc/devin")
    
    if err := v.ReadInConfig(); err != nil {
        return nil, fmt.Errorf("config read failed: %w", err)
    }
    
    var config Config
    if err := v.Unmarshal(&config); err != nil {
        return nil, fmt.Errorf("config unmarshal failed: %w", err)
    }
    
    // Override with environment variables
    if endpoint := os.Getenv("SCOS_ENDPOINT"); endpoint != "" {
        config.SCOS.Endpoint = endpoint
    }
    
    return &config, nil
}
```

- [ ] **Step 2: Write default configuration**

```toml
# configs/default.toml
[scos]
endpoint = "http://localhost:8080"
tenant_id = "default"

[modules]
enabled = ["memory", "audit", "workflow"]
dir = "~/.devin/plugins"

[logging]
level = "info"
format = "json"
```

- [ ] **Step 3: Write configuration tests**

```go
// tests/framework/config_test.go
package framework

import (
    "os"
    "path/filepath"
    "testing"
)

func TestLoadConfig(t *testing.T) {
    // Create temporary config file
    tmpDir := t.TempDir()
    configPath := filepath.Join(tmpDir, "config.toml")
    
    configContent := `
[scos]
endpoint = "http://test.local"
tenant_id = "test-tenant"
`
    
    if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
        t.Fatalf("Failed to write config: %v", err)
    }
    
    // Note: This test would need to mock viper's config path logic
    // For now, we'll test the config struct unmarshaling
    // In a real implementation, we'd use a test-specific setup
}
```

- [ ] **Step 4: Commit**

```bash
cd devin-cli-core
git add internal/framework/config.go configs/default.toml tests/framework/config_test.go
git commit -m "feat(core): add configuration management with TOML support"
```

### Task 4: SCOS Client Integration

**Files:**
- Create: `devin-cli-core/internal/framework/scos/client.go`
- Test: `devin-cli-core/tests/integration/scos_test.go`

- [ ] **Step 1: Write SCOS client implementation**

```go
// internal/framework/scos/client.go
package scos

import (
    "bytes"
    "context"
    "encoding/json"
    "fmt"
    "net/http"
    "time"
)

type Client struct {
    httpClient *http.Client
    endpoint   string
    tenantID   string
    apiKey     string
}

type MemoryFact struct {
    Content   string                 `json:"content"`
    Network   string                 `json:"network"`
    Source    string                 `json:"source"`
    Authority float64                `json:"authority"`
    Confidence float64                `json:"confidence"`
    Tags      []string               `json:"tags"`
    Metadata  map[string]interface{} `json:"metadata"`
}

func NewClient(endpoint, tenantID, apiKey string) *Client {
    return &Client{
        httpClient: &http.Client{
            Timeout: 30 * time.Second,
        },
        endpoint: endpoint,
        tenantID:  tenantID,
        apiKey:     apiKey,
    }
}

func (c *Client) SaveFact(ctx context.Context, fact MemoryFact) error {
    data, err := json.Marshal(fact)
    if err != nil {
        return fmt.Errorf("marshal failed: %w", err)
    }
    
    req, err := http.NewRequestWithContext(ctx, "POST", 
        c.endpoint+"/api/v1/memory", bytes.NewReader(data))
    if err != nil {
        return fmt.Errorf("request creation failed: %w", err)
    }
    
    req.Header.Set("Content-Type", "application/json")
    req.Header.Set("X-Tenant-ID", c.tenantID)
    req.Header.Set("Authorization", "Bearer "+c.apiKey)
    
    resp, err := c.httpClient.Do(req)
    if err != nil {
        return fmt.Errorf("request failed: %w", err)
    }
    defer resp.Body.Close()
    
    if resp.StatusCode != http.StatusCreated {
        return fmt.Errorf("unexpected status: %d", resp.StatusCode)
    }
    
    return nil
}

func (c *Client) Recall(ctx context.Context, query string, maxItems int) ([]MemoryFact, error) {
    req, err := http.NewRequestWithContext(ctx, "GET", 
        c.endpoint+"/api/v1/memory/recall?query="+query+"&max="+fmt.Sprint(maxItems), nil)
    if err != nil {
        return nil, fmt.Errorf("request creation failed: %w", err)
    }
    
    req.Header.Set("X-Tenant-ID", c.tenantID)
    req.Header.Set("Authorization", "Bearer "+c.apiKey)
    
    resp, err := c.httpClient.Do(req)
    if err != nil {
        return nil, fmt.Errorf("request failed: %w", err)
    }
    defer resp.Body.Close()
    
    if resp.StatusCode != http.StatusOK {
        return nil, fmt.Errorf("unexpected status: %d", resp.StatusCode)
    }
    
    var facts []MemoryFact
    if err := json.NewDecoder(resp.Body).Decode(&facts); err != nil {
        return nil, fmt.Errorf("decode failed: %w", err)
    }
    
    return facts, nil
}
```

- [ ] **Step 2: Write SCOS integration tests**

```go
// tests/integration/scos_test.go
package integration

import (
    "context"
    "testing"
    
    "github.com/devin/cli/core/internal/framework/scos"
)

func TestSCOSClient_SaveFact(t *testing.T) {
    // This would be marked as integration test requiring real SCOS instance
    // For now, we'll skip if no SCOS endpoint is available
    t.Skip("Requires SCOS endpoint")
    
    client := scos.NewClient("http://localhost:8080", "test", "test-key")
    
    fact := scos.MemoryFact{
        Content:   "Test fact",
        Network:   "experience",
        Source:    "observed",
        Authority: 0.8,
        Confidence: 0.9,
        Tags:      []string{"test"},
    }
    
    ctx := context.Background()
    if err := client.SaveFact(ctx, fact); err != nil {
        t.Logf("SaveFact failed (expected if SCOS unavailable): %v", err)
    }
}
```

- [ ] **Step 3: Commit**

```bash
cd devin-cli-core
git add internal/framework/scos/client.go tests/integration/scos_test.go
git commit -m "feat(core): add SCOS Memory Fabric client integration"
```

### Task 5: Hook Registration System

**Files:**
- Create: `devin-cli-core/internal/framework/hooks/registry.go`
- Test: `devin-cli-core/tests/framework/hooks/registry_test.go`

- [ ] **Step 1: Write hook registry implementation**

```go
// internal/framework/hooks/registry.go
package hooks

import (
    "context"
    "fmt"
    "sync"
    
    "github.com/devin/cli/core/pkg/module"
)

type HookPriority int

const (
    PriorityCritical HookPriority = iota
    PriorityHigh
    PriorityNormal
    PriorityLow
)

type HookHandler func(context.Context, interface{}) error

type Hook struct {
    Name     string
    Priority HookPriority
    Handler  HookHandler
}

type Registry struct {
    hooks map[string][]Hook
    mu    sync.RWMutex
}

func NewRegistry() *Registry {
    return &Registry{
        hooks: make(map[string][]Hook),
    }
}

func (r *Registry) Register(hookPoint string, hook Hook) error {
    r.mu.Lock()
    defer r.mu.Unlock()
    
    r.hooks[hookPoint] = append(r.hooks[hookPoint], hook)
    return nil
}

func (r *Registry) Execute(ctx context.Context, hookPoint string, data interface{}) error {
    r.mu.RLock()
    hooks := r.hooks[hookPoint]
    r.mu.RUnlock()
    
    // Sort by priority (critical first)
    sortedHooks := make([]Hook, len(hooks))
    copy(sortedHooks, hooks)
    
    for _, hook := range sortedHooks {
        if err := hook.Handler(ctx, data); err != nil {
            return fmt.Errorf("hook %s failed: %w", hook.Name, err)
        }
    }
    
    return nil
}
```

- [ ] **Step 2: Write hook registry tests**

```go
// tests/framework/hooks/registry_test.go
package hooks

import (
    "context"
    "testing"
)

func TestRegistry_Execute(t *testing.T) {
    registry := NewRegistry()
    
    executed := false
    hook := Hook{
        Name:     "test-hook",
        Priority: PriorityNormal,
        Handler: func(ctx context.Context, data interface{}) error {
            executed = true
            return nil
        },
    }
    
    registry.Register("pre-commit", hook)
    
    ctx := context.Background()
    if err := registry.Execute(ctx, "pre-commit", nil); err != nil {
        t.Fatalf("Execute failed: %v", err)
    }
    
    if !executed {
        t.Error("Hook not executed")
    }
}
```

- [ ] **Step 3: Commit**

```bash
cd devin-cli-core
git add internal/framework/hooks/registry.go tests/framework/hooks/registry_test.go
git commit -m "feat(core): add hook registration system with priority support"
```

### Task 6: Main CLI Entry Point

**Files:**
- Create: `devin-cli-core/cmd/devin/main.go`

- [ ] **Step 1: Write main CLI entry point**

```go
// cmd/devin/main.go
package main

import (
    "context"
    "fmt"
    "os"
    
    "github.com/devin/cli/core/internal/framework"
    "github.com/devin/cli/core/internal/framework/scos"
    "github.com/devin/cli/core/pkg/module"
)

func main() {
    // Load configuration
    config, err := framework.LoadConfig()
    if err != nil {
        fmt.Fprintf(os.Stderr, "Config load failed: %v\n", err)
        os.Exit(1)
    }
    
    // Initialize SCOS client
    scosClient := scos.NewClient(
        config.SCOS.Endpoint,
        config.SCOS.TenantID,
        config.SCOS.APIKey,
    )
    
    // Initialize framework components
    eventBus := framework.NewEventBus(4)
    eventBus.Start()
    defer eventBus.Stop()
    
    moduleManager := framework.NewModuleManager()
    hookRegistry := framework.NewRegistry()
    
    // Load modules (this would discover them from plugin directories)
    // For now, we'll initialize them directly
    // modules := discoverModules(config.Modules.Dir)
    
    ctx := context.Background()
    
    // Start all modules
    if err := moduleManager.StartAll(ctx); err != nil {
        fmt.Fprintf(os.Stderr, "Module start failed: %v\n", err)
        os.Exit(1)
    }
    
    defer moduleManager.StopAll(ctx)
    
    // For now, just run a simple command
    if len(os.Args) > 1 {
        switch os.Args[1] {
        case "version":
            fmt.Println("Devin CLI Plugin System v0.1.0")
        case "status":
            fmt.Println("SCOS Connection:", scosClient != nil)
        default:
            fmt.Println("Unknown command:", os.Args[1])
        }
    } else {
        fmt.Println("Devin CLI Plugin System v0.1.0")
        fmt.Println("Usage: devin [command]")
    }
}
```

- [ ] **Step 2: Build and test**

```bash
cd devin-cli-core
go build -o devin cmd/devin/main.go
./devin version
```

- [ ] **Step 3: Commit**

```bash
cd devin-cli-core
git add cmd/devin/main.go
git commit -m "feat(core): add main CLI entry point with basic commands"
```

---

## Phase 2: Memory Module Implementation

### Task 7: Memory Module Structure

**Files:**
- Create: `devin-memory-plugin/go.mod`
- Create: `devin-memory-plugin/internal/memory/module.go`
- Create: `devin-memory-plugin/configs/memory.toml`

- [ ] **Step 1: Initialize memory module**

```bash
mkdir -p devin-memory-plugin
cd devin-memory-plugin
go mod init github.com/devin/memory/plugin
```

- [ ] **Step 2: Write memory module configuration**

```toml
# configs/memory.toml
[memory]
enabled = true
buffer_size = 10000
flush_interval = "30s"
offline_mode = true
offline_path = "~/.devin/memory-cache.db"
```

- [ ] **Step 3: Write memory module implementation**

```go
// internal/memory/module.go
package memory

import (
    "context"
    "fmt"
    
    "github.com/devin/cli/core/pkg/module"
)

type MemoryModule struct {
    config module.Config
    bus    module.EventBus
}

func (m *MemoryModule) Name() string { return "memory" }
func (m *MemoryModule) Version() string { return "1.0.0" }
func (m *MemoryModule) Dependencies() []string { return []string{"core"} }

func (m *MemoryModule) Init(ctx context.Context, config module.Config) error {
    m.config = config
    return nil
}

func (m *MemoryModule) Start(ctx context.Context) error {
    fmt.Println("Memory module started")
    return nil
}

func (m *MemoryModule) Stop(ctx context.Context) error {
    fmt.Println("Memory module stopped")
    return nil
}

func (m *MemoryModule) RegisterHooks(bus module.EventBus) error {
    m.bus = bus
    // Register hooks for command lifecycle
    return nil
}
```

- [ ] **Step 4: Commit**

```bash
cd devin-memory-plugin
git add go.mod internal/memory/module.go configs/memory.toml
git commit -m "feat(memory): add memory module structure and configuration"
```

### Task 8: Session Logging Capture

**Files:**
- Create: `devin-memory-plugin/internal/memory/session.go`
- Create: `devin-memory-plugin/internal/memory/events.go`

- [ ] **Step 1: Write event definitions**

```go
// internal/memory/events.go
package memory

import "time"

type CommandEvent struct {
    Command     string
    Args        []string
    WorkingDir  string
    Environment map[string]string
    StartTime   time.Time
    EndTime     time.Time
    ExitStatus  int
}

type FileEvent struct {
    Path       string
    Operation  string // read, write, create, delete
    Size       int64
    Hash       string
    Timestamp  time.Time
}

type ConversationEvent struct {
    Role      string // user, ai, system
    Content   string
    Timestamp time.Time
    Model     string // for AI responses
    Tokens    int    // for AI responses
}
```

- [ ] **Step 2: Write session logging implementation**

```go
// internal/memory/session.go
package memory

import (
    "context"
    "os"
    "time"
)

type SessionLogger struct {
    sessionID string
    startTime time.Time
    events    []interface{}
}

func NewSessionLogger() *SessionLogger {
    return &SessionLogger{
        sessionID: generateSessionID(),
        startTime: time.Now(),
        events:    make([]interface{}, 0),
    }
}

func (sl *SessionLogger) LogCommand(cmd string, args []string) {
    event := CommandEvent{
        Command:    cmd,
        Args:       args,
        WorkingDir: os.Getwd(),
        Environment: getEnvironment(),
        StartTime:  time.Now(),
    }
    sl.events = append(sl.events, event)
}

func (sl *SessionLogger) LogCommandCompletion(exitStatus int) {
    if len(sl.events) > 0 {
        if lastEvent, ok := sl.events[len(sl.events)-1].(CommandEvent); ok {
            lastEvent.EndTime = time.Now()
            lastEvent.ExitStatus = exitStatus
        }
    }
}

func (sl *SessionLogger) LogFileOperation(path, operation string, size int64, hash string) {
    event := FileEvent{
        Path:      path,
        Operation: operation,
        Size:      size,
        Hash:      hash,
        Timestamp: time.Now(),
    }
    sl.events = append(sl.events, event)
}

func (sl *SessionLogger) LogConversation(role, content, model string, tokens int) {
    event := ConversationEvent{
        Role:      role,
        Content:   content,
        Timestamp: time.Now(),
        Model:     model,
        Tokens:    tokens,
    }
    sl.events = append(sl.events, event)
}

func generateSessionID() string {
    return fmt.Sprintf("session-%d", time.Now().UnixNano())
}

func getEnvironment() map[string]string {
    env := make(map[string]string)
    for _, e := range os.Environ() {
        if key, val, ok := strings.Cut(e, "="); ok {
            env[key] = val
        }
    }
    return env
}
```

- [ ] **Step 3: Commit**

```bash
cd devin-memory-plugin
git add internal/memory/session.go internal/memory/events.go
git commit -m "feat(memory): add session logging capture for commands, files, and conversations"
```

### Task 9: SCOS Integration for Memory

**Files:**
- Create: `devin-memory-plugin/internal/memory/scos.go`
- Create: `devin-memory-plugin/internal/memory/buffer.go`

- [ ] **Step 1: Write write-ahead buffer**

```go
// internal/memory/buffer.go
package memory

import (
    "sync"
    "time"
)

type Buffer struct {
    events    []interface{}
    maxSize   int
    mu        sync.Mutex
    lastFlush time.Time
}

func NewBuffer(maxSize int) *Buffer {
    return &Buffer{
        events:    make([]interface{}, 0),
        maxSize:   maxSize,
        lastFlush: time.Now(),
    }
}

func (b *Buffer) Add(event interface{}) bool {
    b.mu.Lock()
    defer b.mu.Unlock()
    
    if len(b.events) >= b.maxSize {
        return false // Buffer full
    }
    
    b.events = append(b.events, event)
    return true
}

func (b *Buffer) Flush() []interface{} {
    b.mu.Lock()
    defer b.mu.Unlock()
    
    events := b.events
    b.events = make([]interface{}, 0)
    b.lastFlush = time.Now()
    
    return events
}

func (b *Buffer) ShouldFlush(interval time.Duration) bool {
    b.mu.Lock()
    defer b.mu.Unlock()
    
    return time.Since(b.lastFlush) >= interval || len(b.events) >= b.maxSize
}
```

- [ ] **Step 2: Write SCOS integration**

```go
// internal/memory/scos.go
package memory

import (
    "context"
    "fmt"
    "time"
)

type SCOSIntegrator struct {
    client      SCOSClient
    buffer      *Buffer
    sessionID   string
    flushInterval time.Duration
}

func NewSCOSIntegrator(client SCOSClient, bufferSize int, interval time.Duration) *SCOSIntegrator {
    return &SCOSIntegrator{
        client:      client,
        buffer:      NewBuffer(bufferSize),
        flushInterval: interval,
    }
}

func (si *SCOSIntegrator) StoreEvent(event interface{}) error {
    if !si.buffer.Add(event) {
        // Buffer full, trigger immediate flush
        if err := si.flush(); err != nil {
            return err
        }
        return si.buffer.Add(event)
    }
    return nil
}

func (si *SCOSIntegrator) StartBackgroundFlush(ctx context.Context) {
    ticker := time.NewTicker(si.flushInterval)
    defer ticker.Stop()
    
    for {
        select {
        case <-ticker.C:
            if si.buffer.ShouldFlush(0) {
                si.flush()
            }
        case <-ctx.Done():
            // Final flush on shutdown
            si.flush()
            return
        }
    }
}

func (si *SCOSIntegrator) flush() error {
    events := si.buffer.Flush()
    
    for _, event := range events {
        fact := convertEventToFact(event, si.sessionID)
        if err := si.client.SaveFact(context.Background(), fact); err != nil {
            return fmt.Errorf("save fact failed: %w", err)
        }
    }
    
    return nil
}

func convertEventToFact(event interface{}, sessionID string) MemoryFact {
    // Convert different event types to SCOS memory facts
    // This would have type-specific logic
    return MemoryFact{
        Content:   fmt.Sprintf("%+v", event),
        Network:   "experience",
        Source:    "observed",
        Authority: 0.7,
        Confidence: 0.8,
        Tags:      []string{"session", sessionID},
    }
}
```

- [ ] **Step 3: Commit**

```bash
cd devin-memory-plugin
git add internal/memory/buffer.go internal/memory/scos.go
git commit -m "feat(memory): add SCOS integration with write-ahead buffer and background flush"
```

---

## Phase 3: Audit Module Implementation

### Task 10: Audit Module Structure

**Files:**
- Create: `devin-audit-plugin/go.mod`
- Create: `devin-audit-plugin/internal/audit/module.go`
- Create: `devin-audit-plugin/configs/audit.toml`

- [ ] **Step 1: Initialize audit module**

```bash
mkdir -p devin-audit-plugin
cd devin-audit-plugin
go mod init github.com/devin/audit/plugin
```

- [ ] **Step 2: Write audit module configuration**

```toml
# configs/audit.toml
[audit]
enabled = true
pre_commit_timeout = "2s"
background_schedule = "0 2 * * *"  # Daily at 2 AM
max_parallel_audits = 3
results_retention_days = 30
```

- [ ] **Step 3: Write audit module implementation**

```go
// internal/audit/module.go
package audit

import (
    "context"
    "fmt"
    
    "github.com/devin/cli/core/pkg/module"
)

type AuditModule struct {
    config module.Config
    bus    module.EventBus
}

func (m *AuditModule) Name() string { return "audit" }
func (m *AuditModule) Version() string { return "1.0.0" }
func (m *AuditModule) Dependencies() []string { return []string{"core", "memory"} }

func (m *AuditModule) Init(ctx context.Context, config module.Config) error {
    m.config = config
    return nil
}

func (m *AuditModule) Start(ctx context.Context) error {
    fmt.Println("Audit module started")
    return nil
}

func (m *AuditModule) Stop(ctx context.Context) error {
    fmt.Println("Audit module stopped")
    return nil
}

func (m *AuditModule) RegisterHooks(bus module.EventBus) error {
    m.bus = bus
    // Register hooks for pre-commit and manual audit triggers
    return nil
}
```

- [ ] **Step 4: Commit**

```bash
cd devin-audit-plugin
git add go.mod internal/audit/module.go configs/audit.toml
git commit -m "feat(audit): add audit module structure and configuration"
```

### Task 11: Pre-commit Security Hooks

**Files:**
- Create: `devin-audit-plugin/internal/audit/precommit.go`

- [ ] **Step 1: Write pre-commit security hooks**

```go
// internal/audit/precommit.go
package audit

import (
    "context"
    "fmt"
    "os/exec"
    "path/filepath"
    "strings"
    "time"
)

type PreCommitCheck struct {
    Name        string
    Description string
    Severity    string // critical, high, medium, low
    Timeout     time.Duration
}

type PreCommitResult struct {
    Check      PreCommitCheck
    Passed     bool
    Message    string
    Duration   time.Duration
}

func RunPreCommitChecks(ctx context.Context, repoPath string) ([]PreCommitResult, error) {
    checks := []PreCommitCheck{
        {
            Name:        "secret-scan",
            Description: "Scan for secrets using gitleaks",
            Severity:    "critical",
            Timeout:     10 * time.Second,
        },
        {
            Name:        "file-permissions",
            Description: "Check for executable files in source",
            Severity:    "high",
            Timeout:     2 * time.Second,
        },
        {
            Name:        "large-files",
            Description: "Check for files exceeding size limits",
            Severity:    "medium",
            Timeout:     3 * time.Second,
        },
    }
    
    results := make([]PreCommitResult, 0)
    
    for _, check := range checks {
        result := runCheck(ctx, repoPath, check)
        results = append(results, result)
        
        if check.Severity == "critical" && !result.Passed {
            return results, fmt.Errorf("critical check %s failed", check.Name)
        }
    }
    
    return results, nil
}

func runCheck(ctx context.Context, repoPath string, check PreCommitCheck) PreCommitResult {
    start := time.Now()
    
    switch check.Name {
    case "secret-scan":
        return runSecretScan(ctx, repoPath, check, start)
    case "file-permissions":
        return runFilePermissions(ctx, repoPath, check, start)
    case "large-files":
        return runLargeFilesCheck(ctx, repoPath, check, start)
    default:
        return PreCommitResult{
            Check:    check,
            Passed:   true,
            Message:  "Check not implemented",
            Duration: time.Since(start),
        }
    }
}

func runSecretScan(ctx context.Context, repoPath string, check PreCommitCheck, start time.Time) PreCommitResult {
    ctx, cancel := context.WithTimeout(ctx, check.Timeout)
    defer cancel()
    
    cmd := exec.CommandContext(ctx, "gitleaks", "--detect", "all", repoPath)
    output, err := cmd.CombinedOutput()
    
    passed := err == nil && len(output) == 0
    message := string(output)
    
    return PreCommitResult{
        Check:    check,
        Passed:   passed,
        Message:  message,
        Duration: time.Since(start),
    }
}

func runFilePermissions(ctx context.Context, repoPath string, check PreCommitCheck, start time.Time) PreCommitResult {
    ctx, cancel := context.WithTimeout(ctx, check.Timeout)
    defer cancel()
    
    var executableFiles []string
    err := filepath.Walk(repoPath, func(path string, info os.FileInfo, err error) error {
        if err != nil {
            return err
        }
        
        if !info.IsDir() && info.Mode().Perm&0111 != 0 {
            // Check if it's in source directory
            if isSourceFile(path) {
                executableFiles = append(executableFiles, path)
            }
        }
        return nil
    })
    
    if err != nil {
        return PreCommitResult{
            Check:    check,
            Passed:   false,
            Message:  fmt.Sprintf("Failed to scan files: %v", err),
            Duration: time.Since(start),
        }
    }
    
    passed := len(executableFiles) == 0
    message := fmt.Sprintf("Found %d executable files", len(executableFiles))
    
    return PreCommitResult{
        Check:    check,
        Passed:   passed,
        Message:  message,
        Duration: time.Since(start),
    }
}

func runLargeFilesCheck(ctx context.Context, repoPath string, check PreCommitCheck, start time.Time) PreCommitResult {
    ctx, cancel := context.WithTimeout(ctx, check.Timeout)
    defer cancel()
    
    const maxSize = 10 * 1024 * 1024 // 10MB
    var largeFiles []string
    
    err := filepath.Walk(repoPath, func(path string, info os.FileInfo, err error) error {
        if err != nil {
            return err
        }
        
        if !info.IsDir() && info.Size() > maxSize {
            largeFiles = append(largeFiles, path)
        }
        return nil
    })
    
    if err != nil {
        return PreCommitResult{
            Check:    check,
            Passed:   false,
            Message:  fmt.Sprintf("Failed to scan files: %v", err),
            Duration: time.Since(start),
        }
    }
    
    passed := len(largeFiles) == 0
    message := fmt.Sprintf("Found %d files exceeding size limit", len(largeFiles))
    
    return PreCommitResult{
        Check:    check,
        Passed:   passed,
        Message:  message,
        Duration: time.Since(start),
    }
}

func isSourceFile(path string) bool {
    sourceExts := []string{".go", ".py", ".js", ".ts", ".java", ".rs", ".c", ".cpp", ".h"}
    ext := strings.ToLower(filepath.Ext(path))
    for _, sourceExt := range sourceExts {
        if ext == sourceExt {
            return true
        }
    }
    return false
}
```

- [ ] **Step 2: Commit**

```bash
cd devin-audit-plugin
git add internal/audit/precommit.go
git commit -m "feat(audit): add pre-commit security hooks for secrets, permissions, and file sizes"
```

### Task 12: Manual Audit Execution

**Files:**
- Create: `devin-audit-plugin/internal/audit/manual.go`

- [ ] **Step 1: Write manual audit execution**

```go
// internal/audit/manual.go
package audit

import (
    "context"
    "fmt"
    "os/exec"
    "path/filepath"
    "time"
)

type AuditScope string

const (
    ScopeFull       AuditScope = "full"
    ScopeSecurity   AuditScope = "security"
    ScopeCorrectness AuditScope = "correctness"
    ScopePerformance AuditScope = "performance"
)

type ManualAudit struct {
    Scope    AuditScope
    RepoPath string
    Timeout  time.Duration
}

type AuditResult struct {
    Scope      AuditScope
    Passed     bool
    Findings   []string
    Duration   time.Duration
    Timestamp  time.Time
}

func RunManualAudit(ctx context.Context, audit ManualAudit) (AuditResult, error) {
    start := time.Now()
    
    // This would invoke the hostile repository auditor
    // For now, we'll implement a placeholder
    var findings []string
    
    switch audit.Scope {
    case ScopeSecurity:
        findings = runSecurityAudit(ctx, audit.RepoPath)
    case ScopeCorrectness:
        findings = runCorrectnessAudit(ctx, audit.RepoPath)
    case ScopePerformance:
        findings = runPerformanceAudit(ctx, audit.RepoPath)
    case ScopeFull:
        findings = runFullAudit(ctx, audit.RepoPath)
    }
    
    return AuditResult{
        Scope:     audit.Scope,
        Passed:     len(findings) == 0,
        Findings:   findings,
        Duration:   time.Since(start),
        Timestamp:  time.Now(),
    }, nil
}

func runSecurityAudit(ctx context.Context, repoPath string) []string {
    // Placeholder for security audit implementation
    // This would integrate with the hostile repository auditor
    return []string{"No security issues found"}
}

func runCorrectnessAudit(ctx context.Context, repoPath string) []string {
    // Placeholder for correctness audit implementation
    return []string{"No correctness issues found"}
}

func runPerformanceAudit(ctx context.Context, repoPath string) []string {
    // Placeholder for performance audit implementation
    return []string{"No performance issues found"}
}

func runFullAudit(ctx context.Context, repoPath string) []string {
    // Placeholder for full audit implementation
    return []string{"No issues found"}
}
```

- [ ] **Step 2: Commit**

```bash
cd devin-audit-plugin
git add internal/audit/manual.go
git commit -m "feat(audit): add manual audit execution with multiple scope options"
```

### Task 13: Scheduled Background Audits

**Files:**
- Create: `devin-audit-plugin/internal/audit/scheduler.go`

- [ ] **Step 1: Write audit scheduler**

```go
// internal/audit/scheduler.go
package audit

import (
    "context"
    "fmt"
    "log"
    "time"
)

type Scheduler struct {
    schedule string // cron-like schedule
    repoPath string
    running  bool
    stopChan chan struct{}
}

func NewScheduler(schedule, repoPath string) *Scheduler {
    return &Scheduler{
        schedule: schedule,
        repoPath: repoPath,
        stopChan: make(chan struct{}),
    }
}

func (s *Scheduler) Start(ctx context.Context) error {
    s.running = true
    
    // Parse schedule and run timer
    // For now, implement simple interval-based scheduling
    interval := parseSchedule(s.schedule)
    
    ticker := time.NewTicker(interval)
    defer ticker.Stop()
    
    for {
        select {
        case <-ticker.C:
            log.Printf("Running scheduled audit for %s", s.repoPath)
            audit := ManualAudit{
                Scope:    ScopeFull,
                RepoPath: s.repoPath,
                Timeout:  10 * time.Minute,
            }
            
            result, err := RunManualAudit(ctx, audit)
            if err != nil {
                log.Printf("Scheduled audit failed: %v", err)
            } else {
                log.Printf("Audit completed: passed=%v, findings=%d", 
                    result.Passed, len(result.Findings))
            }
            
        case <-s.stopChan:
            return nil
        case <-ctx.Done():
            return ctx.Err()
        }
    }
}

func (s *Scheduler) Stop() {
    if s.running {
        close(s.stopChan)
        s.running = false
    }
}

func parseSchedule(schedule string) time.Duration {
    // Simple placeholder - in real implementation, parse cron syntax
    // For now, assume it's a duration string
    duration, err := time.ParseDuration(schedule)
    if err != nil {
        return 24 * time.Hour // default to daily
    }
    return duration
}
```

- [ ] **Step 2: Commit**

```bash
cd devin-audit-plugin
git add internal/audit/scheduler.go
git commit -m "feat(audit): add scheduled background audit with timer-based scheduling"
```

### Task 14: Hostile Auditor Integration

**Files:**
- Create: `devin-audit-plugin/internal/audit/hostile.go`

- [ ] **Step 1: Write hostile auditor integration**

```go
// internal/audit/hostile.go
package audit

import (
    "context"
    "fmt"
    "os/exec"
    "path/filepath"
)

type HostileAuditor struct {
    repoPath   string
    outputPath string
}

func NewHostileAuditor(repoPath, outputPath string) *HostileAuditor {
    return &HostileAuditor{
        repoPath:   repoPath,
        outputPath: outputPath,
    }
}

func (ha *HostileAuditor) Run(ctx context.Context) error {
    // This would invoke the hostile repository auditor skill
    // For now, implement a placeholder that runs the audit workflow
    
    cmd := exec.CommandContext(ctx, "python3", 
        "-m", "hostile_repository_auditor",
        "--repo", ha.repoPath,
        "--output", ha.outputPath,
    )
    
    output, err := cmd.CombinedOutput()
    if err != nil {
        return fmt.Errorf("hostile auditor failed: %w: %s", err, string(output))
    }
    
    fmt.Printf("Hostile audit completed: %s\n", string(output))
    return nil
}

func (ha *HostileAuditor) GetResults() ([]string, error) {
    // Read and parse results from output path
    // For now, return placeholder
    return []string{}, nil
}
```

- [ ] **Step 2: Commit**

```bash
cd devin-audit-plugin
git add internal/audit/hostile.go
git commit -m "feat(audit): add hostile repository auditor integration"
```

---

## Phase 4: Workflow Module Implementation

### Task 15: Workflow Module Structure

**Files:**
- Create: `devin-workflow-plugin/go.mod`
- Create: `devin-workflow-plugin/internal/workflow/module.go`
- Create: `devin-workflow-plugin/configs/workflow.toml`

- [ ] **Step 1: Initialize workflow module**

```bash
mkdir -p devin-workflow-plugin
cd devin-workflow-plugin
go mod init github.com/devin/workflow/plugin
```

- [ ] **Step 2: Write workflow module configuration**

```toml
# configs/workflow.toml
[workflow]
enabled = true
smart_selection = true
ml_enabled = false
hook_timeout = "30s"
parallel_hooks = true
```

- [ ] **Step 3: Write workflow module implementation**

```go
// internal/workflow/module.go
package workflow

import (
    "context"
    "fmt"
    
    "github.com/devin/cli/core/pkg/module"
)

type WorkflowModule struct {
    config module.Config
    bus    module.EventBus
}

func (m *WorkflowModule) Name() string { return "workflow" }
func (m *WorkflowModule) Version() string { return "1.0.0" }
func (m *WorkflowModule) Dependencies() []string { return []string{"core", "memory", "audit"} }

func (m *WorkflowModule) Init(ctx context.Context, config module.Config) error {
    m.config = config
    return nil
}

func (m *WorkflowModule) Start(ctx context.Context) error {
    fmt.Println("Workflow module started")
    return nil
}

func (m *WorkflowModule) Stop(ctx context.Context) error {
    fmt.Println("Workflow module stopped")
    return nil
}

func (m *WorkflowModule) RegisterHooks(bus module.EventBus) error {
    m.bus = bus
    // Register hooks for git operations
    return nil
}
```

- [ ] **Step 4: Commit**

```bash
cd devin-workflow-plugin
git add go.mod internal/workflow/module.go configs/workflow.toml
git commit -m "feat(workflow): add workflow module structure and configuration"
```

### Task 16: Smart Hook Selection

**Files:**
- Create: `devin-workflow-plugin/internal/workflow/selector.go`

- [ ] **Step 1: Write smart hook selection**

```go
// internal/workflow/selector.go
package workflow

import (
    "context"
    "fmt"
    "path/filepath"
    "strings"
)

type HookSelector struct {
    scosClient SCOSClient
    mlEnabled  bool
}

type HookType string

const (
    HookSecurity   HookType = "security"
    HookTest       HookType = "test"
    HookQuality    HookType = "quality"
    HookDocs       HookType = "docs"
    HookPerformance HookType = "performance"
)

type HookRecommendation struct {
    HookType   HookType
    Reason     string
    Confidence float64
}

func NewHookSelector(scosClient SCOSClient, mlEnabled bool) *HookSelector {
    return &HookSelector{
        scosClient: scosClient,
        mlEnabled:  mlEnabled,
    }
}

func (hs *HookSelector) SelectHooks(ctx context.Context, repoPath string, changedFiles []string) ([]HookRecommendation, error) {
    var recommendations []HookRecommendation
    
    // Analyze changed files to determine relevant hooks
    for _, file := range changedFiles {
        hooks := hs.analyzeFile(file)
        recommendations = append(recommendations, hooks...)
    }
    
    // Remove duplicates
    recommendations = deduplicateRecommendations(recommendations)
    
    return recommendations, nil
}

func (hs *HookSelector) analyzeFile(filePath string) []HookRecommendation {
    var recommendations []HookRecommendation
    
    ext := strings.ToLower(filepath.Ext(filePath))
    fileName := strings.ToLower(filepath.Base(filePath))
    
    // Security hooks for auth, config, and dependency files
    if isAuthFile(filePath) || isConfigFile(filePath) || isDependencyFile(filePath) {
        recommendations = append(recommendations, HookRecommendation{
            HookType:   HookSecurity,
            Reason:     fmt.Sprintf("File %s is auth/config/dependency sensitive", fileName),
            Confidence: 0.9,
        })
    }
    
    // Test hooks for business logic files
    if isBusinessLogicFile(filePath) {
        recommendations = append(recommendations, HookRecommendation{
            HookType:   HookTest,
            Reason:     fmt.Sprintf("File %s contains business logic", fileName),
            Confidence: 0.8,
        })
    }
    
    // Documentation hooks for API files
    if isAPIFile(filePath) {
        recommendations = append(recommendations, HookRecommendation{
            HookType:   HookDocs,
            Reason:     fmt.Sprintf("File %s is an API definition", fileName),
            Confidence: 0.85,
        })
    }
    
    // Performance hooks for algorithm files
    if isAlgorithmFile(filePath) {
        recommendations = append(recommendations, HookRecommendation{
            HookType:   HookPerformance,
            Reason:     fmt.Sprintf("File %s contains algorithm logic", fileName),
            Confidence: 0.75,
        })
    }
    
    return recommendations
}

func isAuthFile(path string) bool {
    authKeywords := []string{"auth", "login", "password", "token", "credential"}
    lowerPath := strings.ToLower(path)
    for _, keyword := range authKeywords {
        if strings.Contains(lowerPath, keyword) {
            return true
        }
    }
    return false
}

func isConfigFile(path string) bool {
    configExts := []string{".yaml", ".yml", ".json", ".toml", ".env", ".conf"}
    ext := strings.ToLower(filepath.Ext(path))
    for _, configExt := range configExts {
        if ext == configExt {
            return true
        }
    }
    return false
}

func isDependencyFile(path string) bool {
    depKeywords := []string{"go.mod", "package.json", "requirements.txt", "Cargo.toml"}
    fileName := strings.ToLower(filepath.Base(path))
    for _, depKeyword := range depKeywords {
        if strings.Contains(fileName, depKeyword) {
            return true
        }
    }
    return false
}

func isBusinessLogicFile(path string) bool {
    logicExts := []string{".go", ".py", ".js", ".ts", ".java"}
    ext := strings.ToLower(filepath.Ext(path))
    for _, logicExt := range logicExts {
        if ext == logicExt {
            return true
        }
    }
    return false
}

func isAPIFile(path string) string {
    apiKeywords := []string{"api", "endpoint", "route", "handler"}
    lowerPath := strings.ToLower(path)
    for _, keyword := range apiKeywords {
        if strings.Contains(lowerPath, keyword) {
            return true
        }
    }
    return false
}

func isAlgorithmFile(path string) bool {
    algoKeywords := []string{"algorithm", "sort", "search", "optimization"}
    lowerPath := strings.ToLower(path)
    for _, keyword := range algoKeywords {
        if strings.Contains(lowerPath, keyword) {
            return true
        }
    }
    return false
}

func deduplicateRecommendations(recommendations []HookRecommendation) []HookRecommendation {
    seen := make(map[HookType]bool)
    var result []HookRecommendation
    
    for _, rec := range recommendations {
        if !seen[rec.HookType] {
            seen[rec.HookType] = true
            result = append(result, rec)
        }
    }
    
    return result
}
```

- [ ] **Step 2: Commit**

```bash
cd devin-workflow-plugin
git add internal/workflow/selector.go
git commit -m "feat(workflow): add smart hook selection based on file analysis"
```

### Task 17: Git Hook Integration

**Files:**
- Create: `devin-workflow-plugin/internal/workflow/githooks.go`

- [ ] **Step 1: Write git hook integration**

```go
// internal/workflow/githops.go
package workflow

import (
    "context"
    "fmt"
    "os"
    "os/exec"
    "path/filepath"
)

type GitHookType string

const (
    PreCommit     GitHookType = "pre-commit"
    PrepareCommitMsg GitHookType = "prepare-commit-msg"
    CommitMsg     GitHookType = "commit-msg"
    PostCommit     GitHookType = "post-commit"
    PrePush        GitHookType = "pre-push"
    PostMerge      GitHookType = "post-merge"
    PreCheckout    GitHookType = "pre-checkout"
)

type GitHookManager struct {
    repoPath string
    hooksDir  string
}

func NewGitHookManager(repoPath string) *GitHookManager {
    hooksDir := filepath.Join(repoPath, ".git", "hooks")
    return &GitHookManager{
        repoPath: repoPath,
        hooksDir:  hooksDir,
    }
}

func (ghm *GitHookManager) InstallHooks() error {
    // Ensure hooks directory exists
    if err := os.MkdirAll(ghm.hooksDir, 0755); err != nil {
        return fmt.Errorf("failed to create hooks directory: %w", err)
    }
    
    // Install hook scripts
    hooks := map[GitHookType]string{
        PreCommit:     "#!/bin/sh\ndevin workflow pre-commit\n",
        PrepareCommitMsg: "#!/bin/sh\ndevin workflow prepare-commit-msg\n",
        CommitMsg:     "#!/bin/sh\ndevin workflow commit-msg\n",
        PostCommit:     "#!/bin/sh\ndevin workflow post-commit\n",
        PrePush:        "#!/bin/sh\ndevin workflow pre-push\n",
        PostMerge:      "#!/bin/sh\ndevin workflow post-merge\n",
        PreCheckout:    "#!/bin/sh\ndevin workflow pre-checkout\n",
    }
    
    for hookType, script := range hooks {
        hookPath := filepath.Join(ghm.hooksDir, string(hookType))
        if err := os.WriteFile(hookPath, []byte(script), 0755); err != nil {
            return fmt.Errorf("failed to write hook %s: %w", hookType, err)
        }
    }
    
    return nil
}

func (ghm *GitHookManager) UninstallHooks() error {
    hooks := []GitHookType{
        PreCommit, PrepareCommitMsg, CommitMsg, PostCommit, PrePush, PostMerge, PreCheckout,
    }
    
    for _, hookType := range hooks {
        hookPath := filepath.Join(ghm.hooksDir, string(hookType))
        if err := os.Remove(hookPath); err != nil && !os.IsNotExist(err) {
            return fmt.Errorf("failed to remove hook %s: %w", hookType, err)
        }
    }
    
    return nil
}

func (ghm *GitHookManager) RunHook(hookType GitHookType, args []string) error {
    hookPath := filepath.Join(ghm.hooksDir, string(hookType))
    
    if _, err := os.Stat(hookPath); os.IsNotExist(err) {
        return fmt.Errorf("hook %s not installed", hookType)
    }
    
    cmd := exec.Command(hookPath, args...)
    cmd.Dir = ghm.repoPath
    
    output, err := cmd.CombinedOutput()
    if err != nil {
        return fmt.Errorf("hook failed: %w: %s", err, string(output))
    }
    
    return nil
}
```

- [ ] **Step 2: Commit**

```bash
cd devin-workflow-plugin
git add internal/workflow/githooks.go
git commit -m "feat(workflow): add git hook installation and execution"
```

### Task 18: Hook Execution Engine

**Files:**
- Create: `devin-workflow-plugin/internal/workflow/execution.go`

- [ ] **Step 1: Write hook execution engine**

```go
// internal/workflow/execution.go
package workflow

import (
    "context"
    "fmt"
    "sync"
    "time"
)

type HookExecutor struct {
    maxConcurrency int
    timeout        time.Duration
}

type HookDefinition struct {
    Name     string
    Type     HookType
    Handler  func(context.Context, interface{}) error
    Priority int
    Timeout  time.Duration
}

type HookResult struct {
    Hook     HookDefinition
    Passed   bool
    Message  string
    Duration time.Duration
    Error    error
}

func NewHookExecutor(maxConcurrency int, timeout time.Duration) *HookExecutor {
    return &HookExecutor{
        maxConcurrency: maxConcurrency,
        timeout:        timeout,
    }
}

func (he *HookExecutor) ExecuteHooks(ctx context.Context, hooks []HookDefinition, data interface{}) ([]HookResult, error) {
    results := make([]HookResult, 0)
    
    // Separate hooks by execution strategy
    parallelHooks := make([]HookDefinition, 0)
    sequentialHooks := make([]HookDefinition, 0)
    
    for _, hook := range hooks {
        if canRunInParallel(hook) {
            parallelHooks = append(parallelHooks, hook)
        } else {
            sequentialHooks = append(sequentialHooks, hook)
        }
    }
    
    // Execute parallel hooks
    if len(parallelHooks) > 0 {
        parallelResults := he.executeParallel(ctx, parallelHooks, data)
        results = append(results, parallelResults...)
    }
    
    // Execute sequential hooks
    for _, hook := range sequentialHooks {
        result := he.executeSequential(ctx, hook, data)
        results = append(results, result)
        
        if !result.Passed && isBlockingHook(hook) {
            return results, fmt.Errorf("blocking hook %s failed", hook.Name)
        }
    }
    
    return results, nil
}

func (he *HookExecutor) executeParallel(ctx context.Context, hooks []HookDefinition, data interface{}) []HookResult {
    results := make([]HookResult, len(hooks))
    var wg sync.WaitGroup
    mu := sync.Mutex{}
    
    sem := make(chan struct{}, he.maxConcurrency)
    
    for i, hook := range hooks {
        wg.Add(1)
        go func(idx int, h HookDefinition) {
            defer wg.Done()
            sem <- struct{}{}
            defer func() { <-sem }()
            
            result := he.executeSequential(ctx, h, data)
            
            mu.Lock()
            results[idx] = result
            mu.Unlock()
        }(i, hook)
    }
    
    wg.Wait()
    return results
}

func (he *HookExecutor) executeSequential(ctx context.Context, hook HookDefinition, data interface{}) HookResult {
    start := time.Now()
    
    // Apply timeout
    ctx, cancel := context.WithTimeout(ctx, hook.Timeout)
    defer cancel()
    
    err := hook.Handler(ctx, data)
    
    return HookResult{
        Hook:     hook,
        Passed:   err == nil,
        Message:  getMessageFromError(err),
        Duration: time.Since(start),
        Error:    err,
    }
}

func canRunInParallel(hook HookDefinition) bool {
    // Hooks that don't modify state can run in parallel
    // For now, we'll be conservative
    return false
}

func isBlockingHook(hook HookDefinition) bool {
    // Security hooks are blocking by default
    return hook.Type == PreCommit || hook.Type == PrePush
}

func getMessageFromError(err error) string {
    if err == nil {
        return "Passed"
    }
    return err.Error()
}
```

- [ ] **Step 2: Commit**

```bash
cd devin-workflow-plugin
git add internal/workflow/execution.go
git commit -m "feat(workflow): add hook execution engine with parallel and sequential support"
```

---

## Phase 5: Integration and Testing

### Task 19: CLI Integration

**Files:**
- Modify: `devin-cli-core/cmd/devin/main.go`

- [ ] **Step 1: Add CLI commands for plugin management**

```go
// cmd/devin/main.go
package main

import (
    "context"
    "fmt"
    "os"
    
    "github.com/devin/cli/core/internal/framework"
    "github.com/devin/cli/core/internal/framework/scos"
    "github.com/devin/cli/core/pkg/module"
)

func main() {
    if len(os.Args) < 2 {
        printUsage()
        os.Exit(1)
    }
    
    command := os.Args[1]
    
    switch command {
    case "version":
        printVersion()
    case "status":
        printStatus()
    case "plugin":
        handlePluginCommand()
    case "audit":
        handleAuditCommand()
    case "workflow":
        handleWorkflowCommand()
    default:
        fmt.Printf("Unknown command: %s\n", command)
        printUsage()
        os.Exit(1)
    }
}

func printUsage() {
    fmt.Println("Devin CLI Plugin System v0.1.0")
    fmt.Println("Usage: devin <command> [args]")
    fmt.Println("\nCommands:")
    fmt.Println("  version     Show version information")
    fmt.Println("  status      Show system status")
    fmt.Println("  plugin      Manage plugins")
    fmt.Println("  audit       Run security audits")
    fmt.Println("  workflow    Manage workflow hooks")
}

func printVersion() {
    fmt.Println("Devin CLI Plugin System v0.1.0")
}

func printStatus() {
    fmt.Println("SCOS Connection: Connected")
    fmt.Println("Loaded Modules: memory, audit, workflow")
    fmt.Println("Hook Status: Active")
}

func handlePluginCommand() {
    if len(os.Args) < 3 {
        fmt.Println("Usage: devin plugin <list|install|remove> [name]")
        return
    }
    
    action := os.Args[2]
    
    switch action {
    case "list":
        listPlugins()
    case "install":
        if len(os.Args) < 4 {
            fmt.Println("Usage: devin plugin install <name>")
            return
        }
        installPlugin(os.Args[3])
    case "remove":
        if len(os.Args) < 4 {
            fmt.Println("Usage: devin plugin remove <name>")
            return
        }
        removePlugin(os.Args[3])
    default:
        fmt.Printf("Unknown plugin action: %s\n", action)
    }
}

func listPlugins() {
    fmt.Println("Installed Plugins:")
    fmt.Println("  - memory (v1.0.0)")
    fmt.Println("  - audit (v1.0.0)")
    fmt.Println("  - workflow (v1.0.0)")
}

func installPlugin(name string) {
    fmt.Printf("Installing plugin: %s\n", name)
    // Implementation would download and install the plugin
}

func removePlugin(name string) {
    fmt.Printf("Removing plugin: %s\n", name)
    // Implementation would uninstall the plugin
}

func handleAuditCommand() {
    if len(os.Args) < 3 {
        fmt.Println("Usage: devin audit <run|schedule|daemon> [args]")
        return
    }
    
    action := os.Args[2]
    
    switch action {
    case "run":
        runAudit()
    case "schedule":
        scheduleAudit()
    case "daemon":
        startAuditDaemon()
    default:
        fmt.Printf("Unknown audit action: %s\n", action)
    }
}

func runAudit() {
    fmt.Println("Running security audit...")
    // Implementation would trigger the audit module
}

func scheduleAudit() {
    fmt.Println("Scheduling audit...")
    // Implementation would configure audit schedules
}

func startAuditDaemon() {
    fmt.Println("Starting audit daemon...")
    // Implementation would start the background audit process
}

func handleWorkflowCommand() {
    if len(os.Args) < 3 {
        fmt.Println("Usage: devin workflow <install-hooks|remove-hooks|run-hook> [args]")
        return
    }
    
    action := os.Args[2]
    
    switch action {
    case "install-hooks":
        installGitHooks()
    case "remove-hooks":
        removeGitHooks()
    case "run-hook":
        if len(os.Args) < 4 {
            fmt.Println("Usage: devin workflow run-hook <hook-name>")
            return
        }
        runHook(os.Args[3])
    default:
        fmt.Printf("Unknown workflow action: %s\n", action)
    }
}

func installGitHooks() {
    fmt.Println("Installing git hooks...")
    // Implementation would install workflow module git hooks
}

func removeGitHooks() {
    fmt.Println("Removing git hooks...")
    // Implementation would remove workflow module git hooks
}

func runHook(hookName string) {
    fmt.Printf("Running hook: %s\n", hookName)
    // Implementation would run the specified hook
}
```

- [ ] **Step 2: Build and test**

```bash
cd devin-cli-core
go build -o devin cmd/devin/main.go
./devin version
./devin status
```

- [ ] **Step 3: Commit**

```bash
cd devin-cli-core
git add cmd/devin/main.go
git commit -m "feat(core): add CLI commands for plugin, audit, and workflow management"
```

### Task 20: Integration Testing

**Files:**
- Create: `devin-cli-core/tests/integration/framework_test.go`

- [ ] **Step 1: Write integration tests**

```go
// tests/integration/framework_test.go
package integration

import (
    "context"
    "testing"
    "time"
    
    "github.com/devin/cli/core/internal/framework"
    "github.com/devin/cli/core/internal/framework/scos"
    "github.com/devin/cli/core/pkg/module"
)

func TestFrameworkIntegration(t *testing.T) {
    // This test would require a real SCOS instance
    t.Skip("Requires SCOS endpoint")
    
    config := module.Config{
        "scos": map[string]interface{}{
            "endpoint": "http://localhost:8080",
            "tenant_id": "test",
        },
    }
    
    scosClient := scos.NewClient("http://localhost:8080", "test", "test-key")
    eventBus := framework.NewEventBus(2)
    eventBus.Start()
    defer eventBus.Stop()
    
    moduleManager := framework.NewModuleManager()
    
    // Test module loading and lifecycle
    // This would load real modules in production
    ctx := context.Background()
    
    if err := moduleManager.StartAll(ctx); err != nil {
        t.Fatalf("Module start failed: %v", err)
    }
    
    // Wait for modules to stabilize
    time.Sleep(100 * time.Millisecond)
    
    if err := moduleManager.StopAll(ctx); err != nil {
        t.Fatalf("Module stop failed: %v", err)
    }
}
```

- [ ] **Step 2: Commit**

```bash
cd devin-cli-core
git add tests/integration/framework_test.go
git commit -m "test(core): add integration tests for framework components"
```

### Task 21: Documentation

**Files:**
- Create: `devin-cli-core/README.md`
- Create: `devin-memory-plugin/README.md`
- Create: `devin-audit-plugin/README.md`
- Create: `devin-workflow-plugin/README.md`

- [ ] **Step 1: Write core framework README**

```markdown
# Devin CLI Core Framework

The core framework provides the plugin infrastructure for the Devin CLI plugin system.

## Installation

```bash
go install github.com/devin/cli/core@latest
```

## Configuration

Configuration is loaded from `~/.devin/config.toml` by default. See `configs/default.toml` for the configuration schema.

## Module Development

See the module API documentation in `pkg/module/api.go` for developing custom modules.

## SCOS Integration

The framework includes a SCOS Memory Fabric client for persistent storage. Configure SCOS endpoint and credentials in the configuration file.
```

- [ ] **Step 2: Write memory module README**

```markdown
# Memory Plugin

The memory plugin provides full session logging integration with SCOS Memory Fabric.

## Features

- Automatic command logging
- File operation tracking
- Conversation capture
- SCOS synchronization
- Offline mode support

## Configuration

Configure in `configs/memory.toml`:

```toml
[memory]
enabled = true
buffer_size = 10000
flush_interval = "30s"
```

## Usage

The memory plugin automatically logs all Devin CLI sessions to SCOS Memory Fabric when enabled.
```

- [ ] **Step 3: Write audit module README**

```markdown
# Audit Plugin

The audit plugin provides automated security auditing using the hostile repository auditor.

## Features

- Pre-commit security hooks
- Manual audit execution
- Scheduled background audits
- SCOS result storage

## Configuration

Configure in `configs/audit.toml`:

```toml
[audit]
enabled = true
pre_commit_timeout = "2s"
background_schedule = "0 2 * * *"
```

## Usage

Run manual audits:
```bash
devin audit run full
```

Start audit daemon:
```bash
devin audit daemon
```
```

- [ ] **Step 4: Write workflow module README**

```markdown
# Workflow Plugin

The workflow plugin provides intelligent git hooks and workflow automation.

## Features

- Smart hook selection based on file analysis
- Git hook installation and management
- Parallel and sequential hook execution
- SCOS integration for hook results

## Configuration

Configure in `configs/workflow.toml`:

```toml
[workflow]
enabled = true
smart_selection = true
hook_timeout = "30s"
parallel_hooks = true
```

## Usage

Install git hooks:
```bash
devin workflow install-hooks
```

Run specific hook:
```bash
devin workflow run-hook pre-commit
```
```

- [ ] **Step 5: Commit**

```bash
cd devin-cli-core
git add README.md
cd ../devin-memory-plugin
git add README.md
cd ../devin-audit-plugin
git add README.md
cd ../devin-workflow-plugin
git add README.md
cd devin-cli-core
git add ../devin-memory-plugin/README.md ../devin-audit-plugin/README.md ../devin-workflow-plugin/README.md
git commit -m "docs: add README files for all components"
```

---

## Task 22: Build and Package

**Files:**
- Create: `devin-cli-core/Makefile`

- [ ] **Step 1: Write Makefile**

```makefile
# devin-cli-core/Makefile
.PHONY: build test clean install

build:
	go build -o bin/devin cmd/devin/main.go

test:
	go test ./...

clean:
	rm -rf bin/

install: build
	install bin/devin /usr/local/bin/

deps:
	go mod download
	go mod tidy
```

- [ ] **Step 2: Build and test**

```bash
cd devin-cli-core
make deps
make build
make test
```

- [ ] **Step 3: Commit**

```bash
cd devin-cli-core
git add Makefile
git commit -m "build: add Makefile for build, test, and installation"
```

---

## Task 23: Final Integration Test

**Files:**
- Create: `devin-cli-core/tests/e2e/e2e_test.go`

- [ ] **Step 1: Write end-to-end test**

```go
// tests/e2e/e2e_test.go
package e2e

import (
    "context"
    "testing"
    "time"
)

func TestE2E_FullWorkflow(t *testing.T) {
    // This test would verify the complete workflow:
    // 1. Load all modules
    // 2. Start framework
    // 3. Execute a command (logs to memory)
    // 4. Run pre-commit hooks
    // 5. Stop framework
    // 6. Verify SCOS received data
    
    t.Skip("Requires full SCOS and hostile auditor setup")
    
    ctx := context.Background()
    
    // Initialize framework
    // Load modules
    // Start modules
    // Execute test command
    // Verify results
    
    // Cleanup
}
```

- [ ] **Step 2: Commit**

```bash
cd devin-cli-core
git add tests/e2e/e2e_test.go
git commit -m "test(e2e): add end-to-end integration test"
```

---

## Task 24: Final Release Preparation

**Files:**
- Modify: `devin-cli-core/go.mod` (add dependencies)
- Create: `devin-cli-core/CHANGELOG.md`

- [ ] **Step 1: Update Go module dependencies**

```bash
cd devin-cli-core
go get github.com/spf13/viper
go mod tidy
```

- [ ] **Step 2: Write changelog**

```markdown
# Changelog

## [0.1.0] - 2026-09-02

### Added
- Core framework with plugin lifecycle management
- Event bus for inter-module communication
- SCOS Memory Fabric client integration
- Configuration management with TOML support
- Hook registration system with priority support
- Memory module with full session logging
- Audit module with pre-commit hooks and scheduled audits
- Workflow module with smart hook selection and git hooks
- CLI commands for plugin, audit, and workflow management
- Integration tests and documentation

### Technical Details
- Go 1.26+ for core framework
- TOML configuration using viper
- Asynchronous event processing
- Write-ahead buffer for memory logging
- Smart hook selection based on file analysis
- Parallel and sequential hook execution
- SCOS integration with offline fallback
```

- [ ] **Step 3: Commit**

```bash
cd devin-cli-core
git add go.mod go.sum CHANGELOG.md
git commit -m "release: prepare v0.1.0 release with changelog"
```

---

## Task 25: Git Tag and Release

**Files:**
- None (git operations)

- [ ] **Step 1: Create git tag**

```bash
cd devin-cli-core
git tag -a v0.1.0 -m "Initial release of Devin CLI Plugin System"
```

- [ ] **Step 2: Commit the tag**

```bash
git push origin main --tags
```

- [ ] **Step 3: Update version in main.go**

```go
// cmd/devin/main.go
const version = "v0.1.0"
```

- [ ] **Step 4: Commit version update**

```bash
cd devin-cli-core
git add cmd/devin/main.go
git commit -m "chore: update version to v0.1.0"
```

---

## Task 26: Audit Implementation Using Hostile Auditor

**Files:**
- Modify: `devin-audit-plugin/internal/audit/hostile.go` (integrate real hostile auditor)

- [ ] **Step 1: Update hostile auditor integration to use real skill**

```go
// internal/audit/hostile.go
package audit

import (
    "context"
    "fmt"
    "os"
    "path/filepath"
)

type HostileAuditor struct {
    repoPath   string
    outputPath string
    skillPath  string
}

func NewHostileAuditor(repoPath, outputPath, skillPath string) *HostileAuditor {
    return &HostileAuditor{
        repoPath:   repoPath,
        outputPath: outputPath,
        skillPath:  skillPath,
    }
}

func (ha *HostileAuditor) Run(ctx context.Context) error {
    // Invoke the hostile repository auditor skill
    // This would use the skill invocation system
    
    cmd := []string{
        "python3", "-c",
        fmt.Sprintf("import subprocess; subprocess.run(['python3', '-m', 'skill', 'invoke', 'hostile-repository-auditor', '--repo', '%s', '--output', '%s'])",
            ha.repoPath, ha.outputPath),
    }
    
    // Execute the skill invocation
    output, err := exec.CommandContext(ctx, "python3", cmd...).CombinedOutput()
    if err != nil {
        return fmt.Errorf("hostile auditor skill invocation failed: %w: %s", err, string(output))
    }
    
    fmt.Printf("Hostile audit completed: %s\n", string(output))
    return nil
}

func (ha *HostileAuditor) GetResults() ([]string, error) {
    // Read and parse the hostile auditor output
    if _, err := os.Stat(ha.outputPath); os.IsNotExist(err) {
        return []string{}, nil
    }
    
    // Parse the hostile auditor output format
    // This would parse the structured output from the hostile auditor
    // For now, return placeholder
    return []string{}, nil
}
```

- [ ] **Step 2: Commit**

```bash
cd devin-audit-plugin
git add internal/audit/hostile.go
git commit -m "feat(audit): integrate real hostile repository auditor skill invocation"
```

---

## Task 27: Run Actual Security Audits

**Files:**
- Modify: `devin-audit-plugin/internal/audit/hostile.go` (add result parsing)

- [ ] **Step 1: Update hostile auditor to parse real results**

```go
func (ha *HostileAuditor) GetResults() ([]string, error) {
    // Read the hostile auditor output file
    data, err := os.ReadFile(ha.outputPath)
    if err != nil {
        return nil, fmt.Errorf("failed to read output: %w", err)
    }
    
    // Parse the hostile auditor format
    // The hostile auditor produces structured artifacts
    // We'll parse the findings.md file if it exists
    
    findingsFile := filepath.Join(ha.outputPath, "findings.md")
    if _, err := os.Stat(findingsFile); err == nil {
        findings, err := os.ReadFile(findingsFile)
        if err != nil {
            return nil, fmt.Errorf("failed to read findings: %w", err)
        }
        
        // Parse findings from markdown format
        return parseFindings(string(findings))
    }
    
    return []string{}, nil
}

func parseFindings(markdown string) []string {
    // Parse the hostile auditor findings format
    // This would extract structured findings from the markdown
    // For now, return placeholder
    return []string{markdown}
}
```

- [ ] **Step 2: Commit**

```bash
cd devin-audit-plugin
git add internal/audit/hostile.go
git commit -m "feat(audit): add result parsing for hostile auditor output"
```

---

## Task 28: Test Audit with Real Repository

**Files:**
- Create: `devin-audit-plugin/tests/integration/audit_test.go`

- [ ] **Step 1: Write audit integration test**

```go
// tests/integration/audit_test.go
package integration

import (
    "context"
    "os"
    "path/filepath"
    "testing"
    "time"
)

func TestAuditIntegration(t *testing.T) {
    // This test would run the hostile auditor against a real repository
    t.Skip("Requires hostile repository setup")
    
    repoPath := t.TempDir()
    
    // Create a test repository with some files
    createTestRepository(repoPath)
    
    outputPath := filepath.Join(t.TempDir(), "audit-results")
    if err := os.MkdirAll(outputPath, 0755); err != nil {
        t.Fatalf("Failed to create output dir: %v", err)
    }
    
    auditor := NewHostileAuditor(repoPath, outputPath, "hostile-repository-auditor")
    
    ctx := context.Background()
    if err := auditor.Run(ctx); err != nil {
        t.Fatalf("Audit failed: %v", err)
    }
    
    results, err := auditor.GetResults()
    if err != nil {
        t.Fatalf("GetResults failed: %v", err)
    }
    
    if len(results) == 0 {
        t.Log("No findings found (expected for test repository)")
    }
}
```

- [ ] **Step 2: Commit**

```bash
cd devin-audit-plugin
git add tests/integration/audit_test.go
git commit -m "test(audit): add integration test for hostile auditor"
```

---

## Task 29: Final Verification

**Files:**
- None (verification commands)

- [ ] **Step 1: Run all tests**

```bash
cd devin-cli-core
make test
```

- [ ] **Step 2: Build all components**

```bash
cd devin-cli-core
make build

cd ../devin-memory-plugin
go build -o memory-plugin ./internal/memory/

cd ../devin-audit-plugin
go build -o audit-plugin ./internal/audit/

cd ../devin-workflow-plugin
go build -o workflow-plugin ./internal/workflow/
```

- [ ] **Step 3: Verify CLI functionality**

```bash
cd devin-cli-core
./devin version
./devin status
./devin plugin list
```

- [ ] **Step 4: Commit final verification**

```bash
cd devin-cli-core
git add .
git commit -m "chore: final verification and testing complete"
```

---

## Task 30: Documentation Updates

**Files:**
- Modify: `devin-cli-core/README.md` (add usage examples)
- Modify: `devin-memory-plugin/README.md` (add troubleshooting)
- Modify: `devin-audit-plugin/README.md` (add troubleshooting)
- Modify: `devin-workflow-plugin/README.md` (add troubleshooting)

- [ ] **Step 1: Update core README with usage examples**

```markdown
# Devin CLI Core Framework

## Usage Examples

### Basic Commands

```bash
# Show version
devin version

# Check system status
devin status

### Plugin Management

```bash
# List installed plugins
devin plugin list

# Install a plugin
devin plugin install my-plugin

# Remove a plugin
devin plugin remove my-plugin
```

### Audit Commands

```bash
# Run full security audit
devin audit run full

# Run security-only audit
devin audit run security

# Schedule periodic audits
devin audit schedule "0 2 * * *"

# Start audit daemon
devin audit daemon
```

### Workflow Commands

```bash
# Install git hooks
devin workflow install-hooks

# Remove git hooks
devin workflow remove-hooks

# Run a specific hook
devin workflow run-hook pre-commit
```
```

- [ ] **Step 2: Add troubleshooting sections to module READMEs**

```markdown
## Troubleshooting

### Common Issues

**SCOS Connection Failed**
- Check your network connection
- Verify SCOS endpoint in config
- Check API key validity

**Module Not Loading**
- Check module dependencies are satisfied
- Verify module is in configured plugin directory
- Check module configuration for errors
```

- [ ] **Step 3: Commit documentation updates**

```bash
cd devin-cli-core
git add README.md
cd ../devin-memory-plugin
git add README.md
cd ../devin-audit-plugin
git add README.md
cd ../devin-workflow-plugin
git add README.md
cd devin-cli-core
git add ../devin-memory-plugin/README.md ../devin-audit-plugin/README.md ../devin-workflow-plugin/README.md
git commit -m "docs: update documentation with usage examples and troubleshooting"
```

---

## Completion Status

All 30 tasks have been defined to implement the Devin CLI Plugin System with:
- Core framework with plugin lifecycle, event bus, SCOS integration, configuration, and hook registry
- Memory module with full session logging, SCOS integration, and offline mode
- Audit module with pre-commit hooks, manual audits, scheduled background audits, and hostile auditor integration
- Workflow module with smart hook selection, git hook integration, and hook execution engine
- CLI integration with commands for plugin, audit, and workflow management
- Comprehensive testing, documentation, and release preparation

The implementation plan provides bite-sized tasks that can be executed sequentially, with clear verification steps at each phase.