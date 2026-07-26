package hermesmemoryprovider

import (
	"context"
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
