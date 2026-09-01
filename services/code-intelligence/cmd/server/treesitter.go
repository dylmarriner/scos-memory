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

func collectTreeSitterSymbols(node *ts.Node, parents []string, language, path, repoID string, source []byte, out *[]Symbol) {
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
					*out = append(*out, Symbol{
						ID: id[:16], RepoID: repoID,
						FullyQualifiedName: strings.Join(qualified, "."), Kind: kind,
						Path: path, Range: fmt.Sprintf("%d-%d", startLine, endLine),
						Language: language, ContentHash: contentHash,
						Signature: string(body),
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
