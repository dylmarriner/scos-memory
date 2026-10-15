package languageadapters

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
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

// GoAdapter implements AST symbol parsing for Go using go/parser.
type GoAdapter struct{}

func (g *GoAdapter) Language() Language { return LangGo }

// ParseSymbols parses a complete Go source file and returns its top-level
// functions, methods and type declarations with their full line spans.
// Source that is not a valid Go file is an error, not a partial result.
// Methods are named Receiver.Method, with the receiver's pointer and type
// parameters removed.
func (g *GoAdapter) ParseSymbols(source string) ([]SymbolDef, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "source.go", source, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parse go source: %w", err)
	}
	span := func(node ast.Node) (int, int) {
		return fset.Position(node.Pos()).Line, fset.Position(node.End()).Line
	}
	var symbols []SymbolDef
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			start, end := span(d)
			sym := SymbolDef{Name: d.Name.Name, Kind: "function", StartLine: start, EndLine: end}
			if d.Recv != nil && len(d.Recv.List) > 0 {
				sym.Kind = "method"
				if recv := receiverTypeName(d.Recv.List[0].Type); recv != "" {
					sym.Name = recv + "." + d.Name.Name
				}
			}
			symbols = append(symbols, sym)
		case *ast.GenDecl:
			if d.Tok != token.TYPE {
				continue
			}
			for _, spec := range d.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				// A lone `type X ...` declaration spans from its `type`
				// keyword; a spec inside `type ( ... )` spans only itself.
				node := ast.Node(ts)
				if !d.Lparen.IsValid() {
					node = d
				}
				start, end := span(node)
				symbols = append(symbols, SymbolDef{Name: ts.Name.Name, Kind: typeKind(ts), StartLine: start, EndLine: end})
			}
		}
	}
	return symbols, nil
}

// typeKind classifies a type declaration by its underlying type expression.
func typeKind(ts *ast.TypeSpec) string {
	if ts.Assign.IsValid() {
		return "alias"
	}
	switch ts.Type.(type) {
	case *ast.StructType:
		return "struct"
	case *ast.InterfaceType:
		return "interface"
	default:
		return "type"
	}
}

// receiverTypeName returns the base type name of a method receiver.
func receiverTypeName(expr ast.Expr) string {
	for {
		switch e := expr.(type) {
		case *ast.StarExpr:
			expr = e.X
		case *ast.ParenExpr:
			expr = e.X
		case *ast.IndexExpr:
			expr = e.X
		case *ast.IndexListExpr:
			expr = e.X
		case *ast.Ident:
			return e.Name
		default:
			return ""
		}
	}
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
