package hermesmemoryprovider

import (
	"context"
	"path/filepath"
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
