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

func findSymbol(symbols []Symbol, fqn string) (Symbol, bool) {
	for _, s := range symbols {
		if s.FullyQualifiedName == fqn {
			return s, true
		}
	}
	return Symbol{}, false
}

func TestTreeSitterCollectsCalledNamesPython(t *testing.T) {
	symbols, err := extractTreeSitterSymbols("service.py", "python", `class Store:
    def load(self):
        return self.fetch()

    def fetch(self):
        return validate(self.load())
`, "repo-1")
	if err != nil {
		t.Fatalf("extract Tree-sitter symbols: %v", err)
	}
	load, ok := findSymbol(symbols, "service.Store.load")
	if !ok {
		t.Fatalf("expected service.Store.load among %+v", symbols)
	}
	if len(load.CalledNames) != 1 || load.CalledNames[0] != "fetch" {
		t.Fatalf("expected load to call fetch via self.fetch(), got %+v", load.CalledNames)
	}
	fetch, ok := findSymbol(symbols, "service.Store.fetch")
	if !ok {
		t.Fatalf("expected service.Store.fetch among %+v", symbols)
	}
	if len(fetch.CalledNames) != 2 {
		t.Fatalf("expected fetch to call validate and load, got %+v", fetch.CalledNames)
	}
}

func TestTreeSitterCollectsCalledNamesTypeScript(t *testing.T) {
	symbols, err := extractTreeSitterSymbols("service.ts", "typescript", `function loadUser(id: string) {
  return normalize(fetchUser(id))
}
`, "repo-1")
	if err != nil {
		t.Fatalf("extract Tree-sitter symbols: %v", err)
	}
	fn, ok := findSymbol(symbols, "service.loadUser")
	if !ok {
		t.Fatalf("expected service.loadUser among %+v", symbols)
	}
	if len(fn.CalledNames) != 2 {
		t.Fatalf("expected loadUser to call normalize and fetchUser, got %+v", fn.CalledNames)
	}
}

func TestTreeSitterDoesNotCollectCalledNamesForNonCallableKinds(t *testing.T) {
	symbols, err := extractTreeSitterSymbols("service.ts", "typescript", `export interface User {
  id: string
}
`, "repo-1")
	if err != nil {
		t.Fatalf("extract Tree-sitter symbols: %v", err)
	}
	iface, ok := findSymbol(symbols, "service.User")
	if !ok {
		t.Fatalf("expected service.User among %+v", symbols)
	}
	if iface.CalledNames != nil {
		t.Fatalf("expected no CalledNames for an interface, got %+v", iface.CalledNames)
	}
}
