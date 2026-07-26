package languageadapters

import (
	"errors"
	"strings"
)

// Language defines supported programming language (§19.5).
type Language string

const (
	LangGo         Language = "go"
	LangPython     Language = "python"
	LangTypeScript Language = "typescript"
	LangSQL        Language = "sql"
)

// SymbolDef represents parsed code symbol (§19.6).
type SymbolDef struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"` // function, struct, class, interface, type
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

// LanguageAdapter defines interface for structural parsing.
type LanguageAdapter interface {
	Language() Language
	ParseSymbols(source string) ([]SymbolDef, error)
}

// GoAdapter implements AST symbol parsing for Go.
type GoAdapter struct{}

func (g *GoAdapter) Language() Language { return LangGo }
func (g *GoAdapter) ParseSymbols(source string) ([]SymbolDef, error) {
	lines := strings.Split(source, "\n")
	var symbols []SymbolDef
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "func ") {
			parts := strings.Fields(trimmed)
			if len(parts) >= 2 {
				symbols = append(symbols, SymbolDef{
					Name:      parts[1],
					Kind:      "function",
					StartLine: i + 1,
					EndLine:   i + 1,
				})
			}
		} else if strings.HasPrefix(trimmed, "type ") {
			parts := strings.Fields(trimmed)
			if len(parts) >= 3 {
				symbols = append(symbols, SymbolDef{
					Name:      parts[1],
					Kind:      parts[2],
					StartLine: i + 1,
					EndLine:   i + 1,
				})
			}
		}
	}
	return symbols, nil
}

// GetAdapter resolves language adapter by file extension or language name.
func GetAdapter(lang Language) (LanguageAdapter, error) {
	switch lang {
	case LangGo:
		return &GoAdapter{}, nil
	default:
		return nil, errors.New("unsupported language adapter: " + string(lang))
	}
}
