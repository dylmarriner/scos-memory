package hermesmemoryprovider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestHermesProvider(t *testing.T) {
	profile := HermesProfile{
		ProfileID: "prof-hermes-1",
		AgentID:   "agent-hermes",
		Namespace: "hermes-default",
	}

	p, err := NewProvider(profile)
	if err != nil {
		t.Fatalf("failed to create provider: %v", err)
	}

	entry, err := p.WriteMemory(context.Background(), "pref_theme", "dark", "user")
	if err != nil {
		t.Fatalf("unexpected write error: %v", err)
	}

	if entry.Value != "dark" {
		t.Fatalf("expected value 'dark', got '%s'", entry.Value)
	}

	got, found := p.ReadMemory(context.Background(), "pref_theme")
	if !found || got.Value != "dark" {
		t.Fatalf("unexpected read result: %v", got)
	}
}

func TestProviderRestartAndProfileIsolation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hermes.json")
	one, err := NewProviderWithStore(HermesProfile{ProfileID: "p1", AgentID: "a1"}, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := one.WriteMemory(context.Background(), "name", "one", "user"); err != nil {
		t.Fatal(err)
	}
	if ok, err := one.SyncTurn(context.Background(), Turn{SessionID: "s1", TurnID: "t1", Messages: []any{"hello"}}); err != nil || !ok {
		t.Fatalf("sync: %v %v", ok, err)
	}
	restarted, err := NewProviderWithStore(HermesProfile{ProfileID: "p1", AgentID: "a1"}, path)
	if err != nil {
		t.Fatal(err)
	}
	if entry, ok := restarted.ReadMemory(context.Background(), "name"); !ok || entry.Value != "one" {
		t.Fatalf("memory was not restored: %#v", entry)
	}
	if got := restarted.Recall(context.Background(), "hello", 10); len(got) != 1 {
		t.Fatalf("expected one recalled turn, got %d", len(got))
	}
	other, err := NewProviderWithStore(HermesProfile{ProfileID: "p2", AgentID: "a2"}, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := other.ReadMemory(context.Background(), "name"); ok || len(other.Recall(context.Background(), "hello", 10)) != 0 {
		t.Fatal("profiles must be isolated")
	}
}

func TestProviderDuplicateSync(t *testing.T) {
	p, err := NewProvider(HermesProfile{ProfileID: "p", AgentID: "a"})
	if err != nil {
		t.Fatal(err)
	}
	turn := Turn{SessionID: "s", TurnID: "t"}
	if ok, err := p.SyncTurn(context.Background(), turn); err != nil || !ok {
		t.Fatal(err)
	}
	if ok, err := p.SyncTurn(context.Background(), turn); err != nil || ok {
		t.Fatalf("duplicate should be ignored: %v %v", ok, err)
	}
}

func TestProviderCorruptStoreIsNotOverwritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hermes.json")
	if err := os.WriteFile(path, []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewProviderWithStore(HermesProfile{ProfileID: "p", AgentID: "a"}, path); err == nil {
		t.Fatal("a corrupt store must be reported, not treated as empty")
	}
	if b, _ := os.ReadFile(path); string(b) != "{not json" {
		t.Fatal("corrupt store was modified")
	}
}

func TestProviderStableIDsConfigAndQueue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hermes.json")
	ctx := context.Background()
	p, err := NewProviderWithStore(HermesProfile{ProfileID: "p", AgentID: "a", Namespace: "ns"}, path)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := p.WriteMemory(ctx, "k", "v1", "user")
	second, _ := p.WriteMemory(ctx, "k", "v2", "user")
	other, _ := p.WriteMemory(ctx, "k2", "v", "user")
	if first.ID != second.ID || first.ID == other.ID {
		t.Fatalf("ids: rewrite %q->%q, other %q", first.ID, second.ID, other.ID)
	}
	if err := p.SaveConfig(ProviderConfig{BaseURL: "https://scos", TenantID: "t", PrincipalID: "u"}); err != nil {
		t.Fatal(err)
	}
	if !p.QueuePrefetch(ctx, Turn{SessionID: "s", TurnID: "t1", Messages: []any{"queued"}}) {
		t.Fatal("queue rejected a valid turn")
	}
	if got := p.Shutdown(ctx); got["pending"] != 0 {
		t.Fatalf("shutdown left turns pending: %v", got)
	}
	restarted, err := NewProviderWithStore(HermesProfile{ProfileID: "p", AgentID: "a", Namespace: "ns"}, path)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.Config()["base_url"] != "https://scos" {
		t.Fatalf("config not persisted: %v", restarted.Config())
	}
	if len(restarted.Recall(ctx, "queued", 5)) != 1 {
		t.Fatal("queued turn was not recorded durably")
	}
	otherNS, _ := NewProviderWithStore(HermesProfile{ProfileID: "p", AgentID: "a", Namespace: "other"}, path)
	if _, ok := otherNS.ReadMemory(ctx, "k"); ok {
		t.Fatal("namespaces must be isolated")
	}
}

func TestProviderEnforcesPermissions(t *testing.T) {
	ctx := context.Background()
	ro, _ := NewProviderWithStore(HermesProfile{ProfileID: "p", AgentID: "a", Permissions: []string{PermissionMemoryRead}}, "")
	if _, err := ro.WriteMemory(ctx, "k", "v", "user"); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("read-only profile wrote memory: %v", err)
	}
	if _, err := ro.SyncTurn(ctx, Turn{SessionID: "s", TurnID: "t"}); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("read-only profile synced a turn: %v", err)
	}
	wo, _ := NewProviderWithStore(HermesProfile{ProfileID: "p", AgentID: "a", Permissions: []string{PermissionMemoryWrite}}, "")
	_, _ = wo.WriteMemory(ctx, "k", "v", "user")
	if _, ok := wo.ReadMemory(ctx, "k"); ok {
		t.Fatal("write-only profile read memory")
	}
}

func TestProviderConcurrentWritersKeepAllData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hermes.json")
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p, err := NewProviderWithStore(HermesProfile{ProfileID: fmt.Sprint("p", i), AgentID: "a"}, path)
			if err != nil {
				t.Error(err)
				return
			}
			if _, err := p.WriteMemory(ctx, "k", "v", "user"); err != nil {
				t.Error(err)
			}
			_ = p.OnSessionEnd(ctx, "s")
		}(i)
	}
	wg.Wait()
	for i := 0; i < 8; i++ {
		p, err := NewProviderWithStore(HermesProfile{ProfileID: fmt.Sprint("p", i), AgentID: "a"}, path)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := p.ReadMemory(ctx, "k"); !ok {
			t.Fatalf("profile p%d lost its write to a concurrent writer", i)
		}
	}
}

func TestHandleToolCallMemoryLifecycle(t *testing.T) {
	p, err := NewProviderWithStore(HermesProfile{ProfileID: "p", AgentID: "a"}, filepath.Join(t.TempDir(), "s.json"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	entry, err := p.WriteMemory(ctx, "tz", "UTC", "preference")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.OnSessionSwitch(ctx, "s1", "s2", "resume"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.HandleToolCall(ctx, "scos_correct", map[string]any{"memory_id": entry.ID, "value": "NZST"}); err != nil {
		t.Fatal(err)
	}
	got, err := p.HandleToolCall(ctx, "scos_explain", map[string]any{"memory_id": entry.ID})
	if err != nil || got.(map[string]any)["memory"].(MemoryEntry).Value != "NZST" {
		t.Fatalf("explain after correct = %v, %v", got, err)
	}
	um, _ := p.HandleToolCall(ctx, "scos_user_model", nil)
	if len(um.([]MemoryEntry)) != 1 {
		t.Fatalf("user model = %v", um)
	}
	g, _ := p.HandleToolCall(ctx, "scos_memory_graph", nil)
	if edges := g.(map[string]any)["edges"].([]map[string]any); len(edges) != 1 || edges[0]["from"] != "s1" || edges[0]["to"] != "s2" {
		t.Fatalf("graph edges = %v", edges)
	}
	if _, err := p.HandleToolCall(ctx, "scos_forget", map[string]any{"memory_id": entry.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.HandleToolCall(ctx, "scos_explain", map[string]any{"memory_id": entry.ID}); err == nil {
		t.Fatal("explain of forgotten memory succeeded")
	}
	if _, err := p.HandleToolCall(ctx, "nope", nil); err == nil {
		t.Fatal("unknown tool accepted")
	}
}

func TestQueuePrefetchRequiresWritePermission(t *testing.T) {
	p, _ := NewProviderWithStore(HermesProfile{ProfileID: "p", AgentID: "a", Permissions: []string{PermissionMemoryRead}}, "")
	if p.QueuePrefetch(context.Background(), Turn{SessionID: "s", TurnID: "t"}) {
		t.Fatal("read-only profile queued a turn")
	}
}
