package languageadapters

import (
	"reflect"
	"testing"
)

func TestGoAdapterParsesDeclarationsWithFullSpans(t *testing.T) {
	adapter, err := GetAdapter(LangGo)
	if err != nil {
		t.Fatalf("failed to resolve adapter: %v", err)
	}

	src := `package main

type User struct {
	Name string
}

type (
	Store interface {
		Get(id string) (User, error)
	}
	ID = string
	Count int
)

// func NotAFunction() lives in a comment and must not be reported.
var banner = "type Fake struct{}"

func ProcessUser(u User) error {
	if u.Name == "" {
		return nil
	}
	return nil
}

func (s *List[T]) Push(v T) { s.items = append(s.items, v) }

type List[T any] struct{ items []T }
`

	symbols, err := adapter.ParseSymbols(src)
	if err != nil {
		t.Fatalf("failed to parse symbols: %v", err)
	}

	want := []SymbolDef{
		{Name: "User", Kind: "struct", StartLine: 3, EndLine: 5},
		{Name: "Store", Kind: "interface", StartLine: 8, EndLine: 10},
		{Name: "ID", Kind: "alias", StartLine: 11, EndLine: 11},
		{Name: "Count", Kind: "type", StartLine: 12, EndLine: 12},
		{Name: "ProcessUser", Kind: "function", StartLine: 18, EndLine: 23},
		{Name: "List.Push", Kind: "method", StartLine: 25, EndLine: 25},
		{Name: "List", Kind: "struct", StartLine: 27, EndLine: 27},
	}
	if !reflect.DeepEqual(symbols, want) {
		t.Fatalf("symbols mismatch\n got: %+v\nwant: %+v", symbols, want)
	}
}

func TestGoAdapterRejectsInvalidSource(t *testing.T) {
	adapter := &GoAdapter{}
	if _, err := adapter.ParseSymbols("func broken( {"); err == nil {
		t.Fatal("expected a parse error for invalid Go source")
	}
}

func TestGetAdapterRejectsUnsupportedLanguage(t *testing.T) {
	if _, err := GetAdapter(LangSQL); err == nil {
		t.Fatal("expected an error for an unsupported language")
	}
}
