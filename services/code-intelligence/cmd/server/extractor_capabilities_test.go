package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type extractorCapabilityMatrix struct {
	SchemaVersion int `json:"schema_version"`
	Languages     []struct {
		Language   string   `json:"language"`
		Extensions []string `json:"extensions"`
		Extractor  string   `json:"extractor"`
		Confidence string   `json:"confidence"`
	} `json:"languages"`
}

func TestP2PublishedExtractorCapabilitiesMatchRuntime(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "docs", "P2_EXTRACTOR_CAPABILITIES.json"))
	if err != nil {
		t.Fatal(err)
	}
	var matrix extractorCapabilityMatrix
	if err := json.Unmarshal(raw, &matrix); err != nil {
		t.Fatal(err)
	}
	if matrix.SchemaVersion != 1 {
		t.Fatalf("unexpected matrix version: %d", matrix.SchemaVersion)
	}
	expected := map[string]struct {
		extractor, confidence, source, path, symbol string
	}{
		"go":            {"go/parser", "static_analysis", "package p2\nfunc Execute() {}\n", "fixture.go", "Execute"},
		"python":        {"tree-sitter:python", "syntax", "def execute():\n    return True\n", "fixture.py", "execute"},
		"javascript":    {"tree-sitter:javascript", "syntax", "function execute() { return 1; }\n", "fixture.js", "execute"},
		"typescript":    {"tree-sitter:typescript", "syntax", "function execute(): number { return 1; }\n", "fixture.ts", "execute"},
		"rust":          {"tree-sitter:rust", "syntax", "fn execute() -> i32 { 1 }\n", "fixture.rs", "execute"},
		"sql":           {"path-convention", "heuristic", "", "", ""},
		"yaml":          {"path-convention", "heuristic", "", "", ""},
		"configuration": {"path-convention", "heuristic", "", "", ""},
	}
	seen := map[string]bool{}
	for _, row := range matrix.Languages {
		spec, ok := expected[row.Language]
		if !ok || seen[row.Language] {
			t.Fatalf("unexpected/duplicate language %q", row.Language)
		}
		seen[row.Language] = true
		if row.Extractor != spec.extractor || row.Confidence != spec.confidence {
			t.Errorf("%s: claimed extractor/confidence %q/%q, want %q/%q", row.Language, row.Extractor, row.Confidence, spec.extractor, spec.confidence)
		}
		if len(row.Extensions) == 0 {
			t.Errorf("%s: no extensions", row.Language)
		}
		for _, ext := range row.Extensions {
			if got := detectLanguage("fixture" + ext); got != row.Language {
				t.Errorf("%s: extension %s detected as %q", row.Language, ext, got)
			}
		}
		if spec.source == "" {
			if languageGrammar(row.Language) != nil {
				t.Errorf("%s: unexpectedly has a grammar", row.Language)
			}
			continue
		}
		if row.Language == "go" {
			_, decls, err := extractGoDeclarations(spec.path, spec.source)
			if err != nil || len(decls) != 1 || decls[0].Name != spec.symbol {
				t.Errorf("Go extraction mismatch: %v, %+v", err, decls)
			}
		} else {
			if languageGrammar(row.Language) == nil {
				t.Errorf("%s: missing grammar", row.Language)
				continue
			}
			symbols, err := extractTreeSitterSymbols(spec.path, row.Language, spec.source, "p2-fixture")
			if err != nil {
				t.Errorf("%s extraction: %v", row.Language, err)
				continue
			}
			found := false
			for _, sym := range symbols {
				if sym.FullyQualifiedName == "fixture."+spec.symbol {
					found = true
				}
			}
			if !found {
				t.Errorf("%s: expected fixture.%s in %+v", row.Language, spec.symbol, symbols)
			}
		}
	}
	if len(seen) != len(expected) {
		t.Errorf("matrix missing languages: got %d want %d", len(seen), len(expected))
	}
}

func TestP2WorkspaceScanIndexesModuleSuffixes(t *testing.T) {
	root := t.TempDir()
	cases := []struct{ path, language, source, symbol string }{
		{"entry.cjs", "javascript", "function ready() { return true; }\n", "entry.ready"},
		{"entry.mts", "typescript", "function ready(): boolean { return true; }\n", "entry.ready"},
		{"entry.cts", "typescript", "function ready(): boolean { return true; }\n", "entry.ready"},
	}
	for _, tc := range cases {
		if err := os.WriteFile(filepath.Join(root, tc.path), []byte(tc.source), 0600); err != nil {
			t.Fatal(err)
		}
	}
	files, skipped, err := scanRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 0 || len(files) != len(cases) {
		t.Fatalf("scan lost module files: %d files, skipped=%v", len(files), skipped)
	}
	byPath := map[string]PGFileInput{}
	for _, file := range files {
		byPath[file.Path] = file
	}
	for _, tc := range cases {
		file, ok := byPath[tc.path]
		if !ok || file.Language != tc.language || file.Content != tc.source {
			t.Errorf("%s: missing or incorrectly classified: %+v", tc.path, file)
			continue
		}
		symbols, err := extractTreeSitterSymbols(file.Path, file.Language, file.Content, "p2-fixture")
		if err != nil {
			t.Errorf("%s: %v", tc.path, err)
			continue
		}
		found := false
		for _, symbol := range symbols {
			if symbol.FullyQualifiedName == tc.symbol {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: declaration not extracted: %+v", tc.path, symbols)
		}
	}
}
