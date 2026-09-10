package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCodeModelRuntimeEmbedCodeUsesGovernedAuthAndExactSpace(t *testing.T) {
	space := codeEmbeddingSpace{Role: "code", Provider: "scos-model-runtime", Model: "BAAI/bge-code-v1", Revision: "rev-a", Dimensions: 3, Generation: 2}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embed/code" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer service-token" {
			t.Fatalf("authorization = %q", got)
		}
		if got := r.Header.Get("X-SCOS-Control-Plane-Token"); got != "control-token" {
			t.Fatalf("control token = %q", got)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["input"] != "package symbol document" {
			t.Fatalf("input = %q", body["input"])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"embedding": []float64{0.1, 0.2, 0.3}, "model": space.Model, "model_revision": space.Revision, "dimensions": space.Dimensions, "role": space.Role})
	}))
	defer server.Close()

	client := &codeModelRuntimeClient{baseURL: server.URL, httpClient: server.Client(), serviceToken: "service-token", controlPlaneToken: "control-token"}
	result, err := client.EmbedCode(context.Background(), "package symbol document", space)
	if err != nil {
		t.Fatal(err)
	}
	if result.Space != space || len(result.Embedding) != 3 {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestCodeModelRuntimeEmbedCodeRejectsMismatchedSpace(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"embedding": []float64{0.1, 0.2}, "model": "other-model", "model_revision": "rev-a", "dimensions": 2, "role": "code"})
	}))
	defer server.Close()
	client := &codeModelRuntimeClient{baseURL: server.URL, httpClient: server.Client()}
	_, err := client.EmbedCode(context.Background(), "x", codeEmbeddingSpace{Role: "code", Provider: "scos-model-runtime", Model: "BAAI/bge-code-v1", Revision: "rev-a", Dimensions: 2, Generation: 1})
	if err == nil || !strings.Contains(err.Error(), "outside active space") {
		t.Fatalf("expected space mismatch, got %v", err)
	}
}

func TestCodeEmbeddingDocumentIsDeterministicAndContentBearing(t *testing.T) {
	symbol := codeSymbolDocument{
		FullyQualifiedName: "pkg.Repository.QueryDense",
		Path:               "services/code-intelligence/cmd/server/pgstore.go",
		Kind:               "method",
		Signature:          "func (s *pgCodeStore) QueryDense(...) ([]codeHybridResult, error)",
		DocComment:         "QueryDense searches the active snapshot.",
		ChunkContent:       "SELECT sv.id FROM code_symbol_versions sv",
	}
	first := codeEmbeddingDocument(symbol)
	second := codeEmbeddingDocument(symbol)
	if first != second {
		t.Fatal("embedding document is not deterministic")
	}
	for _, want := range []string{symbol.FullyQualifiedName, symbol.Path, symbol.Kind, symbol.Signature, symbol.DocComment, symbol.ChunkContent} {
		if !strings.Contains(first, want) {
			t.Fatalf("embedding document missing %q: %s", want, first)
		}
	}
}

func TestFuseCodeResultsKeepsLexicalAndDenseScoresSeparate(t *testing.T) {
	lexical := []codeHybridResult{
		{PGSymbolVersion: PGSymbolVersion{SymbolVersionID: "a", FullyQualifiedName: "pkg.A", Path: "a.go", Relevance: 0.9, MatchType: "lexical"}, RawLexicalScore: 0.9},
		{PGSymbolVersion: PGSymbolVersion{SymbolVersionID: "b", FullyQualifiedName: "pkg.B", Path: "b.go", Relevance: 0.5, MatchType: "lexical"}, RawLexicalScore: 0.5},
	}
	dense := []codeHybridResult{
		{PGSymbolVersion: PGSymbolVersion{SymbolVersionID: "b", FullyQualifiedName: "pkg.B", Path: "b.go", MatchType: "dense"}, RawDenseScore: 0.98},
		{PGSymbolVersion: PGSymbolVersion{SymbolVersionID: "c", FullyQualifiedName: "pkg.C", Path: "c.go", MatchType: "dense"}, RawDenseScore: 0.8},
	}
	got := fuseCodeResults(lexical, dense, 60)
	if len(got) != 3 {
		t.Fatalf("len = %d", len(got))
	}
	if got[0].SymbolVersionID != "b" {
		t.Fatalf("cross-lane agreement should rank b first, got %#v", got)
	}
	if got[0].RawLexicalScore != 0.5 || got[0].RawDenseScore != 0.98 || got[0].RRFScore <= 0 {
		t.Fatalf("score provenance lost: %#v", got[0])
	}
	if got[0].MatchType != "hybrid" {
		t.Fatalf("match type = %q", got[0].MatchType)
	}
}

func TestFuseCodeSignalsKeepsStructuralAndCentralitySeparate(t *testing.T) {
	lexical := []codeHybridResult{
		{PGSymbolVersion: PGSymbolVersion{SymbolVersionID: "a", FullyQualifiedName: "pkg.A", Path: "a.go", Centrality: 0.05}, RawLexicalScore: 0.9},
		{PGSymbolVersion: PGSymbolVersion{SymbolVersionID: "b", FullyQualifiedName: "pkg.B", Path: "b.go", Centrality: 0.90}, RawLexicalScore: 0.8},
	}
	dense := []codeHybridResult{
		{PGSymbolVersion: PGSymbolVersion{SymbolVersionID: "a", FullyQualifiedName: "pkg.A", Path: "a.go", Centrality: 0.05}, RawDenseScore: 0.9},
		{PGSymbolVersion: PGSymbolVersion{SymbolVersionID: "b", FullyQualifiedName: "pkg.B", Path: "b.go", Centrality: 0.90}, RawDenseScore: 0.8},
	}
	structural := []codeHybridResult{
		{PGSymbolVersion: PGSymbolVersion{SymbolVersionID: "b", FullyQualifiedName: "pkg.B", Path: "b.go", Centrality: 0.90}, RawStructuralScore: 1.0},
	}
	got := fuseCodeSignals(lexical, dense, structural, 60)
	if len(got) != 2 || got[0].SymbolVersionID != "b" {
		t.Fatalf("structural agreement plus centrality should rank b first: %#v", got)
	}
	if got[0].RawLexicalScore != 0.8 || got[0].RawDenseScore != 0.8 || got[0].RawStructuralScore != 1.0 {
		t.Fatalf("raw signal provenance corrupted: %#v", got[0])
	}
	if got[0].RRFScore <= 0 || got[0].FusedScore <= got[0].RRFScore {
		t.Fatalf("expected separate graph-aware final score: %#v", got[0])
	}
}
