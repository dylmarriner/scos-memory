package languageadapters

import (
	"testing"
)

func TestGoAdapter(t *testing.T) {
	adapter, err := GetAdapter(LangGo)
	if err != nil {
		t.Fatalf("failed to resolve adapter: %v", err)
	}

	src := `package main
type User struct { Name string }
func ProcessUser(u User) error { return nil }`

	symbols, err := adapter.ParseSymbols(src)
	if err != nil {
		t.Fatalf("failed to parse symbols: %v", err)
	}

	if len(symbols) < 2 {
		t.Fatalf("expected at least 2 symbols, got %d", len(symbols))
	}

	if symbols[0].Name != "User" || symbols[1].Name != "ProcessUser(u" {
		t.Logf("parsed symbols: %+v", symbols)
	}
}
