package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"

	ts "github.com/tree-sitter/go-tree-sitter"
	javascript "github.com/tree-sitter/tree-sitter-javascript/bindings/go"
	python "github.com/tree-sitter/tree-sitter-python/bindings/go"
	rust "github.com/tree-sitter/tree-sitter-rust/bindings/go"
	typescript "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
)

// extractTreeSitterSymbols provides the broad syntax fallback required by the
// code-index roadmap. It deliberately reports syntax facts only: semantic
// definitions and references remain the responsibility of Go's parser or a
// SCIP index. Unsupported languages fail explicitly so callers cannot mistake
// a file-level symbol for a successful declaration extraction.
func extractTreeSitterSymbols(path, language, content, repoID string) ([]Symbol, error) {
	grammar := languageGrammar(language)
	if grammar == nil {
		return nil, fmt.Errorf("Tree-sitter grammar unavailable for %s", language)
	}
	parser := ts.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(grammar); err != nil {
		return nil, fmt.Errorf("set Tree-sitter language for %s: %w", language, err)
	}
	tree := parser.Parse([]byte(content), nil)
	if tree == nil {
		return nil, fmt.Errorf("Tree-sitter returned no syntax tree for %s", path)
	}
	defer tree.Close()
	if tree.RootNode().HasError() {
		return nil, fmt.Errorf("Tree-sitter syntax errors in %s", path)
	}
	module := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	var symbols []Symbol
	collectTreeSitterSymbols(tree.RootNode(), []string{module}, language, path, repoID, []byte(content), &symbols)
	return symbols, nil
}

func languageGrammar(language string) *ts.Language {
	switch language {
	case "python":
		return ts.NewLanguage(python.Language())
	case "typescript":
		return ts.NewLanguage(typescript.LanguageTypescript())
	case "javascript":
		return ts.NewLanguage(javascript.Language())
	case "rust":
		return ts.NewLanguage(rust.Language())
	default:
		return nil
	}
}

type treeSitterDeclaration struct {
	kind string
	name string
}

// callExpressionKind and calleeSelectorField describe, per language, how a
// call is shaped in that grammar's tree: the node kind that represents a
// call expression, and -- when the callee is a member/attribute access
// rather than a bare identifier -- the field name that holds the accessed
// property. This mirrors goextract.go's calledNames at the same
// permissiveness: "x.Bar()" contributes "Bar" regardless of what x is, and
// pgstore.go's edge builder is what turns an unresolved bare name into
// either a real edge or nothing.
type callShape struct {
	callKind      string
	selectorKind  string
	selectorField string
}

func callShapeFor(language string) (callShape, bool) {
	switch language {
	case "python":
		return callShape{callKind: "call", selectorKind: "attribute", selectorField: "attribute"}, true
	case "typescript", "javascript":
		return callShape{callKind: "call_expression", selectorKind: "member_expression", selectorField: "property"}, true
	case "rust":
		return callShape{callKind: "call_expression", selectorKind: "field_expression", selectorField: "field"}, true
	default:
		return callShape{}, false
	}
}

// treeSitterCalledNames walks node's full subtree (the declaration's own body,
// including any nested closures/functions -- deliberately permissive, same
// as goextract.go's ast.Inspect over a Go function body) and collects the
// bare name of every call expression's target.
func treeSitterCalledNames(node *ts.Node, language string, source []byte) []string {
	shape, ok := callShapeFor(language)
	if !ok {
		return nil
	}
	seen := make(map[string]bool)
	var names []string
	var walk func(n *ts.Node)
	walk = func(n *ts.Node) {
		if n.Kind() == shape.callKind {
			if callee := n.ChildByFieldName("function"); callee != nil {
				if name := calleeName(callee, shape, source); name != "" && !seen[name] {
					seen[name] = true
					names = append(names, name)
				}
			}
		}
		for i := uint(0); i < n.NamedChildCount(); i++ {
			walk(n.NamedChild(i))
		}
	}
	walk(node)
	return names
}

func calleeName(callee *ts.Node, shape callShape, source []byte) string {
	if callee.Kind() == "identifier" {
		return callee.Utf8Text(source)
	}
	if callee.Kind() == shape.selectorKind {
		if field := callee.ChildByFieldName(shape.selectorField); field != nil {
			return field.Utf8Text(source)
		}
	}
	return ""
}

func classifyTreeSitterDeclaration(kind, language string) (treeSitterDeclaration, bool) {
	declarationKinds := map[string]map[string]string{
		"python": {
			"class_definition": "class", "function_definition": "function",
		},
		"typescript": {
			"class_declaration": "class", "interface_declaration": "interface",
			"function_declaration": "function", "method_definition": "method",
		},
		"javascript": {
			"class_declaration": "class", "function_declaration": "function",
			"method_definition": "method",
		},
		"rust": {
			"function_item": "function", "struct_item": "type", "enum_item": "type",
			"trait_item": "interface", "impl_item": "class",
		},
	}
	declKind, ok := declarationKinds[language][kind]
	return treeSitterDeclaration{kind: declKind}, ok
}

// pythonClassAttribute returns the name a Python class body statement
// declares as a class attribute -- "is_rewritten: bool" (a dataclass field)
// or "retries = 3" -- and the assignment node, or ("", nil) when the
// statement is anything else.
func pythonClassAttribute(node *ts.Node, source []byte) (string, *ts.Node) {
	if node.Kind() != "expression_statement" || node.NamedChildCount() != 1 {
		return "", nil
	}
	block := node.Parent()
	if block == nil || block.Kind() != "block" || block.Parent() == nil || block.Parent().Kind() != "class_definition" {
		return "", nil
	}
	assignment := node.NamedChild(0)
	if assignment == nil || assignment.Kind() != "assignment" {
		return "", nil
	}
	left := assignment.ChildByFieldName("left")
	if left == nil || left.Kind() != "identifier" {
		return "", nil
	}
	return left.Utf8Text(source), assignment
}

func collectTreeSitterSymbols(node *ts.Node, parents []string, language, path, repoID string, source []byte, out *[]Symbol) {
	if language == "python" && len(parents) > 0 {
		if name, assignment := pythonClassAttribute(node, source); name != "" {
			start, end := assignment.ByteRange()
			if end <= uint(len(source)) && start < end {
				body := source[start:end]
				sum := sha256.Sum256(body)
				contentHash := hex.EncodeToString(sum[:])
				qualified := append(append([]string(nil), parents...), name)
				id := symbolScope(repoID, path+"\x00"+strings.Join(qualified, ".")+"\x00"+contentHash)
				*out = append(*out, Symbol{
					ID: id[:16], RepoID: repoID,
					FullyQualifiedName: strings.Join(qualified, "."), Kind: "field",
					Path: path, Range: fmt.Sprintf("%d-%d", node.StartPosition().Row+1, node.EndPosition().Row+1),
					Language: language, ContentHash: contentHash, Signature: string(body),
				})
			}
			return
		}
	}
	if declaration, ok := classifyTreeSitterDeclaration(node.Kind(), language); ok {
		nameNode := node.ChildByFieldName("name")
		if nameNode != nil {
			name := nameNode.Utf8Text(source)
			if name != "" {
				kind := declaration.kind
				if kind == "function" && len(parents) > 1 && language == "python" {
					kind = "method"
				}
				start, end := node.ByteRange()
				if end <= uint(len(source)) && start < end {
					body := source[start:end]
					sum := sha256.Sum256(body)
					contentHash := hex.EncodeToString(sum[:])
					qualified := append(append([]string(nil), parents...), name)
					id := symbolScope(repoID, path+"\x00"+strings.Join(qualified, ".")+"\x00"+contentHash)
					startLine := node.StartPosition().Row + 1
					endLine := node.EndPosition().Row + 1
					var called []string
					if kind == "function" || kind == "method" {
						called = treeSitterCalledNames(node, language, source)
					}
					*out = append(*out, Symbol{
						ID: id[:16], RepoID: repoID,
						FullyQualifiedName: strings.Join(qualified, "."), Kind: kind,
						Path: path, Range: fmt.Sprintf("%d-%d", startLine, endLine),
						Language: language, ContentHash: contentHash,
						Signature: string(body), CalledNames: called,
					})
					parents = append(parents, name)
				}
			}
		}
	}
	for i := uint(0); i < node.NamedChildCount(); i++ {
		child := node.NamedChild(i)
		collectTreeSitterSymbols(child, parents, language, path, repoID, source, out)
	}
}
