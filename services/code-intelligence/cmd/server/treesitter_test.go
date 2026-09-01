package main

import "testing"

func TestTreeSitterExtractsPythonDeclarations(t *testing.T) {
	symbols, err := extractTreeSitterSymbols("models.py", "python", `class User:
    def save(self):
        return True

def load_user(identifier):
    return User()
`, "repo-1")
	if err != nil {
		t.Fatalf("extract Tree-sitter symbols: %v", err)
	}
	if len(symbols) != 3 {
		t.Fatalf("expected class and two functions, got %d: %+v", len(symbols), symbols)
	}
	if symbols[0].Kind != "class" || symbols[0].FullyQualifiedName != "models.User" {
		t.Fatalf("unexpected class symbol: %+v", symbols[0])
	}
	if symbols[1].Kind != "method" || symbols[1].FullyQualifiedName != "models.User.save" {
		t.Fatalf("unexpected method symbol: %+v", symbols[1])
	}
	if symbols[2].Kind != "function" || symbols[2].FullyQualifiedName != "models.load_user" {
		t.Fatalf("unexpected function symbol: %+v", symbols[2])
	}
	if symbols[1].Range == "" || symbols[1].ContentHash == "" {
		t.Fatal("expected source range and content hash on Tree-sitter symbols")
	}
}

func TestTreeSitterExtractsTypeScriptDeclarations(t *testing.T) {
	symbols, err := extractTreeSitterSymbols("service.ts", "typescript", `export interface User {
  id: string
}

export function loadUser(id: string): User {
  return { id }
}
`, "repo-1")
	if err != nil {
		t.Fatalf("extract Tree-sitter symbols: %v", err)
	}
	if len(symbols) != 2 {
		t.Fatalf("expected interface and function, got %d: %+v", len(symbols), symbols)
	}
	if symbols[0].Kind != "interface" || symbols[0].FullyQualifiedName != "service.User" {
		t.Fatalf("unexpected interface symbol: %+v", symbols[0])
	}
	if symbols[1].Kind != "function" || symbols[1].FullyQualifiedName != "service.loadUser" {
		t.Fatalf("unexpected function symbol: %+v", symbols[1])
	}
}

func TestTreeSitterRejectsUnsupportedLanguage(t *testing.T) {
	if _, err := extractTreeSitterSymbols("config.yaml", "yaml", "name: value", "repo-1"); err == nil {
		t.Fatal("expected unsupported language error")
	}
}
