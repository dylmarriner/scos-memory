package languageadapters

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strings"

	ts "github.com/tree-sitter/go-tree-sitter"
	python "github.com/tree-sitter/tree-sitter-python/bindings/go"
	typescript "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
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
	Kind      string `json:"kind"` // function, method, struct, class, interface, type, table, view, ...
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

// LanguageAdapter defines interface for structural parsing.
type LanguageAdapter interface {
	Language() Language
	ParseSymbols(source string) ([]SymbolDef, error)
}

// GoAdapter parses Go source with the standard library's go/parser.
// Methods are named "Receiver.Method" so they do not collide with
// same-named functions or methods on other types.
type GoAdapter struct{}

func (g *GoAdapter) Language() Language { return LangGo }

func (g *GoAdapter) ParseSymbols(source string) ([]SymbolDef, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "source.go", source, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parse go source: %w", err)
	}
	span := func(n ast.Node) (int, int) {
		return fset.Position(n.Pos()).Line, fset.Position(n.End()).Line
	}
	var symbols []SymbolDef
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			name, kind := d.Name.Name, "function"
			if d.Recv != nil && len(d.Recv.List) > 0 {
				name, kind = receiverTypeName(d.Recv.List[0].Type)+"."+name, "method"
			}
			start, end := span(d)
			symbols = append(symbols, SymbolDef{Name: name, Kind: kind, StartLine: start, EndLine: end})
		case *ast.GenDecl:
			if d.Tok != token.TYPE {
				continue
			}
			for _, spec := range d.Specs {
				typeSpec := spec.(*ast.TypeSpec)
				kind := "type"
				switch typeSpec.Type.(type) {
				case *ast.StructType:
					kind = "struct"
				case *ast.InterfaceType:
					kind = "interface"
				}
				// A lone "type X ..." spans the whole GenDecl so its doc
				// and keyword are included; grouped specs use their own span.
				var node ast.Node = typeSpec
				if !d.Lparen.IsValid() {
					node = d
				}
				start, end := span(node)
				symbols = append(symbols, SymbolDef{Name: typeSpec.Name.Name, Kind: kind, StartLine: start, EndLine: end})
			}
		}
	}
	return symbols, nil
}

func receiverTypeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return receiverTypeName(t.X)
	case *ast.IndexExpr: // generic receiver T[K]
		return receiverTypeName(t.X)
	case *ast.IndexListExpr: // generic receiver T[K, V]
		return receiverTypeName(t.X)
	case *ast.Ident:
		return t.Name
	}
	return ""
}

// treeSitterAdapter parses Python and TypeScript with Tree-sitter. Nested
// declarations are reported with dotted names (Class.method).
type treeSitterAdapter struct {
	lang     Language
	grammar  func() *ts.Language
	declKind map[string]string
}

func (a *treeSitterAdapter) Language() Language { return a.lang }

func (a *treeSitterAdapter) ParseSymbols(source string) ([]SymbolDef, error) {
	p := ts.NewParser()
	defer p.Close()
	if err := p.SetLanguage(a.grammar()); err != nil {
		return nil, fmt.Errorf("set %s grammar: %w", a.lang, err)
	}
	src := []byte(source)
	tree := p.Parse(src, nil)
	if tree == nil {
		return nil, fmt.Errorf("parse %s source: no syntax tree", a.lang)
	}
	defer tree.Close()
	if tree.RootNode().HasError() {
		return nil, fmt.Errorf("parse %s source: syntax errors", a.lang)
	}
	var symbols []SymbolDef
	var walk func(n *ts.Node, parents []string, inClass bool)
	walk = func(n *ts.Node, parents []string, inClass bool) {
		childInClass := inClass
		if kind, ok := a.declKind[n.Kind()]; ok {
			if nameNode := n.ChildByFieldName("name"); nameNode != nil {
				name := nameNode.Utf8Text(src)
				if kind == "function" && inClass {
					kind = "method"
				}
				qualified := append(append([]string(nil), parents...), name)
				symbols = append(symbols, SymbolDef{
					Name:      strings.Join(qualified, "."),
					Kind:      kind,
					StartLine: int(n.StartPosition().Row) + 1,
					EndLine:   int(n.EndPosition().Row) + 1,
				})
				parents = qualified
				childInClass = kind == "class"
			}
		}
		for i := uint(0); i < n.NamedChildCount(); i++ {
			walk(n.NamedChild(i), parents, childInClass)
		}
	}
	walk(tree.RootNode(), nil, false)
	return symbols, nil
}

// SQLAdapter extracts DDL-defined objects (tables, views, functions, ...)
// from SQL scripts. Statements are split on semicolons outside of quoted
// strings, quoted identifiers, comments and dollar-quoted bodies, so a
// function body containing ';' is one statement.
type SQLAdapter struct{}

func (s *SQLAdapter) Language() Language { return LangSQL }

var sqlCreate = regexp.MustCompile(`(?is)^CREATE\s+(?:OR\s+REPLACE\s+)?(?:(?:GLOBAL|LOCAL)\s+)?(?:TEMP(?:ORARY)?\s+|UNLOGGED\s+)?(?:UNIQUE\s+)?` +
	`(MATERIALIZED\s+VIEW|TABLE|VIEW|FUNCTION|PROCEDURE|INDEX|TYPE|TRIGGER|POLICY|SCHEMA|SEQUENCE|EXTENSION|DOMAIN)\s+` +
	`(?:CONCURRENTLY\s+)?(?:IF\s+NOT\s+EXISTS\s+)?((?:"[^"]+"|[A-Za-z_][\w$]*)(?:\.(?:"[^"]+"|[A-Za-z_][\w$]*))?)`)

func (s *SQLAdapter) ParseSymbols(source string) ([]SymbolDef, error) {
	statements, err := splitSQL(source)
	if err != nil {
		return nil, err
	}
	var symbols []SymbolDef
	for _, st := range statements {
		m := sqlCreate.FindStringSubmatch(st.text)
		if m == nil {
			continue
		}
		kind := strings.ToLower(strings.Join(strings.Fields(m[1]), "_"))
		symbols = append(symbols, SymbolDef{
			Name:      strings.ReplaceAll(m[2], `"`, ""),
			Kind:      kind,
			StartLine: st.startLine,
			EndLine:   st.endLine,
		})
	}
	return symbols, nil
}

type sqlStatement struct {
	text               string
	startLine, endLine int
}

var dollarTag = regexp.MustCompile(`^\$[A-Za-z_]*\$`)

func splitSQL(source string) ([]sqlStatement, error) {
	var out []sqlStatement
	var buf strings.Builder
	line, startLine := 1, 0
	flush := func(endLine int) {
		if text := strings.TrimSpace(buf.String()); text != "" {
			out = append(out, sqlStatement{text: text, startLine: startLine, endLine: endLine})
		}
		buf.Reset()
		startLine = 0
	}
	// emit appends statement text, recording the line of its first
	// non-whitespace character as the statement start.
	emit := func(chunk string) {
		if startLine == 0 && strings.TrimSpace(chunk) != "" {
			startLine = line + strings.Count(chunk[:len(chunk)-len(strings.TrimLeft(chunk, " \t\r\n"))], "\n")
		}
		buf.WriteString(chunk)
		line += strings.Count(chunk, "\n")
	}
	for i := 0; i < len(source); {
		rest := source[i:]
		switch {
		case strings.HasPrefix(rest, "--"):
			end := strings.IndexByte(rest, '\n')
			if end < 0 {
				end = len(rest)
			}
			line += strings.Count(rest[:end], "\n")
			i += end
		case strings.HasPrefix(rest, "/*"):
			end := strings.Index(rest[2:], "*/")
			if end < 0 {
				return nil, errors.New("parse sql source: unterminated block comment")
			}
			line += strings.Count(rest[:end+4], "\n")
			i += end + 4
		case rest[0] == '\'' || rest[0] == '"':
			end := closingQuote(rest, rest[0])
			if end < 0 {
				return nil, errors.New("parse sql source: unterminated quoted string")
			}
			emit(rest[:end+1])
			i += end + 1
		case rest[0] == '$' && dollarTag.MatchString(rest):
			tag := dollarTag.FindString(rest)
			end := strings.Index(rest[len(tag):], tag)
			if end < 0 {
				return nil, fmt.Errorf("parse sql source: unterminated %s body", tag)
			}
			emit(rest[:len(tag)+end+len(tag)])
			i += len(tag) + end + len(tag)
		case rest[0] == ';':
			flush(line)
			i++
		default:
			emit(rest[:1])
			i++
		}
	}
	flush(line)
	return out, nil
}

// closingQuote returns the index of the quote closing s[0], treating a
// doubled quote as an escaped literal quote.
func closingQuote(s string, q byte) int {
	for i := 1; i < len(s); i++ {
		if s[i] == q {
			if i+1 < len(s) && s[i+1] == q {
				i++
				continue
			}
			return i
		}
	}
	return -1
}

// GetAdapter resolves a language adapter by language name.
func GetAdapter(lang Language) (LanguageAdapter, error) {
	switch lang {
	case LangGo:
		return &GoAdapter{}, nil
	case LangPython:
		return &treeSitterAdapter{
			lang:     LangPython,
			grammar:  func() *ts.Language { return ts.NewLanguage(python.Language()) },
			declKind: map[string]string{"class_definition": "class", "function_definition": "function"},
		}, nil
	case LangTypeScript:
		return &treeSitterAdapter{
			lang:    LangTypeScript,
			grammar: func() *ts.Language { return ts.NewLanguage(typescript.LanguageTypescript()) },
			declKind: map[string]string{
				"class_declaration": "class", "abstract_class_declaration": "class",
				"interface_declaration": "interface", "type_alias_declaration": "type",
				"enum_declaration": "enum", "function_declaration": "function",
				"method_definition": "method",
			},
		}, nil
	case LangSQL:
		return &SQLAdapter{}, nil
	default:
		return nil, errors.New("unsupported language adapter: " + string(lang))
	}
}
