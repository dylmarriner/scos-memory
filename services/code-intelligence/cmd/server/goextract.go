package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"go/types"
	"strings"
)

// GoDeclaration is one top-level Go declaration found by extractGoDeclarations:
// a function, method, type, const or var. This is real semantic-adjacent
// extraction via go/parser's full AST -- not the filename/path-convention
// heuristic in extractGoSymbols (main.go) -- addressing the single most
// repeated finding across both competitive-audit passes: "symbol
// extraction is driven by language/filename conventions rather than a real
// parser." It stops short of type-checking (go/types' checker, which needs
// a fully loaded package/module), so CalledNames below are bare syntactic
// names, not resolved targets -- pgstore.go's edge-building pass only
// turns one into a CALLS edge when exactly one declaration in the same
// package/snapshot has that name, and drops it otherwise rather than
// guess among several candidates (e.g. two types with same-named
// methods). References to anything outside the indexed package (stdlib,
// other modules) are never turned into edges at all.
type GoDeclaration struct {
	// Name is the declared identifier alone (e.g. "CreateUser"), Receiver
	// is the method receiver's type expression rendered as source (e.g.
	// "*UserService") or "" for a non-method declaration.
	Name      string
	Receiver  string
	Kind      string // "function" | "method" | "type" | "const" | "var"
	Signature string // e.g. "func CreateUser(ctx context.Context, req *Request) (*Response, error)"
	// StartLine/EndLine are 1-based, inclusive, matching how git diff and
	// most editors report ranges.
	StartLine int
	EndLine   int
	// CalledNames lists the bare function/method names this declaration's
	// body calls syntactically (only set for function/method kinds). A
	// direct call "Foo()" contributes "Foo"; a selector call "x.Bar()"
	// contributes "Bar" regardless of what x actually is -- deliberately
	// permissive here because the ambiguity this could introduce is
	// resolved later, once every declaration in the package is known.
	CalledNames []string
	// ContentHash is over this declaration's own source span, not the
	// whole file -- so editing one function doesn't change every other
	// symbol in the file's identity the way file-level content hashing
	// would (see pgCodeStore's doc comment on content-hash-keyed identity).
	ContentHash string
	DocComment  string
}

// FullyQualifiedName renders this declaration's FQN within package pkgName,
// e.g. "pkg.CreateUser" or "pkg.(*UserService).CreateUser" for a method.
func (d GoDeclaration) FullyQualifiedName(pkgName string) string {
	if d.Receiver != "" {
		return fmt.Sprintf("%s.(%s).%s", pkgName, d.Receiver, d.Name)
	}
	return pkgName + "." + d.Name
}

// extractGoDeclarations parses Go source and returns its top-level
// declarations. path is used only for error messages (parser.ParseFile's
// filename argument); content is parsed directly rather than read from
// disk, since callers already have it in hand (the v2 index request body).
func extractGoDeclarations(path, content string) (pkgName string, decls []GoDeclaration, err error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, content, parser.ParseComments)
	if err != nil {
		return "", nil, fmt.Errorf("parse %s: %w", path, err)
	}
	pkgName = file.Name.Name
	src := []byte(content)
	imports := importAliases(file)

	for _, d := range file.Decls {
		switch decl := d.(type) {
		case *ast.FuncDecl:
			decls = append(decls, funcDeclaration(fset, src, decl, imports))
		case *ast.GenDecl:
			decls = append(decls, genDeclarations(fset, src, decl)...)
		}
	}
	return pkgName, decls, nil
}

func sourceSpan(fset *token.FileSet, src []byte, start, end token.Pos) (text string, startLine, endLine int) {
	startOffset := fset.Position(start).Offset
	endOffset := fset.Position(end).Offset
	if startOffset < 0 || endOffset > len(src) || startOffset > endOffset {
		return "", fset.Position(start).Line, fset.Position(end).Line
	}
	return string(src[startOffset:endOffset]), fset.Position(start).Line, fset.Position(end).Line
}

func spanHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// exprString renders an AST expression (a type, a receiver) back to source
// text without needing a type-checked package -- go/types.ExprString is a
// pure syntactic pretty-printer despite living in the types package.
func exprString(expr ast.Expr) string {
	return types.ExprString(expr)
}

func renderNode(fset *token.FileSet, node any) string {
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, node); err != nil {
		return ""
	}
	return buf.String()
}

func docText(doc *ast.CommentGroup) string {
	if doc == nil {
		return ""
	}
	return strings.TrimSpace(doc.Text())
}

func funcDeclaration(fset *token.FileSet, src []byte, decl *ast.FuncDecl, imports map[string]string) GoDeclaration {
	text, startLine, endLine := sourceSpan(fset, src, decl.Pos(), decl.End())
	receiver := ""
	kind := "function"
	if decl.Recv != nil && len(decl.Recv.List) == 1 {
		receiver = exprString(decl.Recv.List[0].Type)
		kind = "method"
	}
	// decl.Type is an *ast.FuncType, which printer renders as
	// "func(params) results" -- already including the "func" keyword, so
	// building the signature means splicing the name (and receiver, for a
	// method) in after that keyword rather than prepending another one.
	paramsAndResults := strings.TrimPrefix(renderNode(fset, decl.Type), "func")
	signature := "func " + decl.Name.Name + paramsAndResults
	if receiver != "" {
		signature = "func (" + receiver + ") " + decl.Name.Name + paramsAndResults
	}
	return GoDeclaration{
		Name:        decl.Name.Name,
		Receiver:    receiver,
		Kind:        kind,
		Signature:   signature,
		StartLine:   startLine,
		EndLine:     endLine,
		ContentHash: spanHash(text),
		DocComment:  docText(decl.Doc),
		CalledNames: calledNames(decl.Body, imports),
	}
}

// calledNames walks body for call expressions and collects the bare name
// of each call's target: "Foo()" contributes "Foo", "x.Bar()" contributes
// "Bar" (whatever x is -- not resolved here), "pkg.Baz()" also contributes
// "Baz" (indistinguishable at this stage from a method call; the edge
// pass in pgstore.go only keeps it if a same-named declaration actually
// exists in the indexed package, so an unresolvable or wrong-looking name
// simply produces no edge rather than a wrong one). A nil body (an
// external/assembly function declared but not defined) yields nil.
func calledNames(body *ast.BlockStmt, imports map[string]string) []string {
	if body == nil {
		return nil
	}
	seen := make(map[string]bool)
	var names []string
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		var name string
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			name = fn.Name
		case *ast.SelectorExpr:
			name = fn.Sel.Name
			// A call through an imported package's name ("auth.Sign()")
			// is recorded against the import path, so the edge pass can
			// resolve it to that package's declaration across directories.
			if x, ok := fn.X.(*ast.Ident); ok {
				if path, imported := imports[x.Name]; imported {
					name = path + qualifiedCallSeparator + fn.Sel.Name
				}
			}
		}
		if name != "" && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
		return true
	})
	return names
}

// qualifiedCallSeparator joins an import path and a function name in a
// CalledNames entry for a call through an imported package.
const qualifiedCallSeparator = ":"

// importAliases maps the name each import is referred to by in this file
// to its import path. Dot and blank imports name nothing to select from.
func importAliases(file *ast.File) map[string]string {
	out := make(map[string]string, len(file.Imports))
	for _, spec := range file.Imports {
		path := strings.Trim(spec.Path.Value, "`\"")
		name := ""
		if spec.Name != nil {
			name = spec.Name.Name
		} else {
			name = defaultImportName(path)
		}
		if name == "" || name == "_" || name == "." {
			continue
		}
		out[name] = path
	}
	return out
}

// defaultImportName is the identifier an unaliased import is used by: the
// last path element, skipping a major-version suffix ("…/v2") and a
// "go-" prefix or ".go"/"-go" suffix that package names drop.
func defaultImportName(path string) string {
	parts := strings.Split(path, "/")
	name := parts[len(parts)-1]
	if len(parts) > 1 && len(name) > 1 && name[0] == 'v' && strings.Trim(name[1:], "0123456789") == "" {
		name = parts[len(parts)-2]
	}
	name = strings.TrimPrefix(name, "go-")
	name = strings.TrimSuffix(strings.TrimSuffix(name, ".go"), "-go")
	return strings.ReplaceAll(name, "-", "_")
}

// genDeclarations handles `type`/`const`/`var` blocks, which can each
// declare more than one name in a single spec (`var a, b int`) or group
// multiple specs under one `(...)` block (`const (\n A = 1\n B = 2\n)`).
// Every named identifier becomes its own GoDeclaration -- import specs are
// skipped, since an import is a dependency edge, not a symbol this
// service's caller would ever look up by name.
func genDeclarations(fset *token.FileSet, src []byte, decl *ast.GenDecl) []GoDeclaration {
	var out []GoDeclaration
	kind := ""
	switch decl.Tok {
	case token.TYPE:
		kind = "type"
	case token.CONST:
		kind = "const"
	case token.VAR:
		kind = "var"
	default:
		return nil // import, or a token kind with no symbol identity
	}

	for _, spec := range decl.Specs {
		switch s := spec.(type) {
		case *ast.TypeSpec:
			text, startLine, endLine := sourceSpan(fset, src, s.Pos(), s.End())
			doc := docText(s.Doc)
			if doc == "" {
				doc = docText(decl.Doc)
			}
			out = append(out, GoDeclaration{
				Name:        s.Name.Name,
				Kind:        kind,
				Signature:   "type " + s.Name.Name + " " + renderNode(fset, s.Type),
				StartLine:   startLine,
				EndLine:     endLine,
				ContentHash: spanHash(text),
				DocComment:  doc,
			})
		case *ast.ValueSpec:
			text, startLine, endLine := sourceSpan(fset, src, s.Pos(), s.End())
			doc := docText(s.Doc)
			if doc == "" {
				doc = docText(decl.Doc)
			}
			typeStr := ""
			if s.Type != nil {
				typeStr = " " + exprString(s.Type)
			}
			for _, name := range s.Names {
				if name.Name == "_" {
					continue
				}
				out = append(out, GoDeclaration{
					Name:      name.Name,
					Kind:      kind,
					Signature: kind + " " + name.Name + typeStr,
					StartLine: startLine,
					EndLine:   endLine,
					// Every name in a multi-name spec ("var a, b int") shares
					// the same source span, and therefore the same content
					// hash -- accurate, since editing that line changes all
					// of them at once. Each name still gets its own distinct
					// stable identity: pgstore.go's indexer derives
					// stable_key from name+ContentHash together, not
					// ContentHash alone.
					ContentHash: spanHash(text),
					DocComment:  doc,
				})
			}
		}
	}
	return out
}
