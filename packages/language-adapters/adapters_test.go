package languageadapters

import (
	"reflect"
	"testing"
)

func parse(t *testing.T, lang Language, src string) []SymbolDef {
	t.Helper()
	adapter, err := GetAdapter(lang)
	if err != nil {
		t.Fatalf("GetAdapter(%s): %v", lang, err)
	}
	symbols, err := adapter.ParseSymbols(src)
	if err != nil {
		t.Fatalf("ParseSymbols(%s): %v", lang, err)
	}
	return symbols
}

func TestGoAdapter(t *testing.T) {
	src := `package main

type User struct {
	Name string
}

type (
	ID   string
	Repo interface{ Get(ID) (*User, error) }
)

func ProcessUser(u User) error {
	return nil
}

func (s *Store[K]) Get(id K) (*User, error) { return nil, nil }
`
	want := []SymbolDef{
		{"User", "struct", 3, 5},
		{"ID", "type", 8, 8},
		{"Repo", "interface", 9, 9},
		{"ProcessUser", "function", 12, 14},
		{"Store.Get", "method", 16, 16},
	}
	if got := parse(t, LangGo, src); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestGoAdapterRejectsInvalidSource(t *testing.T) {
	adapter, _ := GetAdapter(LangGo)
	if _, err := adapter.ParseSymbols("package main\nfunc {"); err == nil {
		t.Fatal("expected a parse error")
	}
}

func TestPythonAdapter(t *testing.T) {
	src := "class User:\n    def process(self):\n        def inner():\n            pass\n\nasync def fetch():\n    return 1\n"
	want := []SymbolDef{
		{"User", "class", 1, 4},
		{"User.process", "method", 2, 4},
		{"User.process.inner", "function", 3, 4},
		{"fetch", "function", 6, 7},
	}
	if got := parse(t, LangPython, src); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestTypeScriptAdapter(t *testing.T) {
	src := "export interface Repo { get(id: string): User }\ntype ID = string\nclass Store {\n  get(id: ID) { return null }\n}\nfunction main() {}\n"
	want := []SymbolDef{
		{"Repo", "interface", 1, 1},
		{"ID", "type", 2, 2},
		{"Store", "class", 3, 5},
		{"Store.get", "method", 4, 4},
		{"main", "function", 6, 6},
	}
	if got := parse(t, LangTypeScript, src); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestSQLAdapter(t *testing.T) {
	src := `-- schema; with a semicolon in a comment
CREATE TABLE IF NOT EXISTS scos.memory_items (
  id uuid PRIMARY KEY,
  note text DEFAULT 'a;b'
);

CREATE OR REPLACE FUNCTION touch() RETURNS trigger AS $fn$
BEGIN
  NEW.updated_at = now();
  RETURN NEW;
END;
$fn$ LANGUAGE plpgsql;
CREATE UNIQUE INDEX CONCURRENTLY idx_items ON scos.memory_items (id);
CREATE MATERIALIZED VIEW "Recent Items" AS SELECT 1;
INSERT INTO t VALUES (1);
`
	want := []SymbolDef{
		{"scos.memory_items", "table", 2, 5},
		{"touch", "function", 7, 12},
		{"idx_items", "index", 13, 13},
		{"Recent Items", "materialized_view", 14, 14},
	}
	if got := parse(t, LangSQL, src); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestSQLAdapterRejectsUnterminatedBody(t *testing.T) {
	adapter, _ := GetAdapter(LangSQL)
	if _, err := adapter.ParseSymbols("CREATE FUNCTION f() AS $$ BEGIN"); err == nil {
		t.Fatal("expected an error for an unterminated dollar-quoted body")
	}
}

func TestUnsupportedLanguage(t *testing.T) {
	if _, err := GetAdapter("cobol"); err == nil {
		t.Fatal("expected an unsupported-language error")
	}
}
