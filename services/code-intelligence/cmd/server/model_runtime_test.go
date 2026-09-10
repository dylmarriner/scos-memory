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
	space := codeEmbeddingSpace{
		Role:       "code",
		Provider:   "scos-model-runtime",
		Model:      "BAAI/bge-code-v1",
		Revision:   "rev-a",
		Dimensions: 3,
		Generation: 2,
	}
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
		_ = json.NewEncoder(w).Encode(map[string]any{
			"embedding":      []float64{0.1, 0.2, 0.3},
			"model":          space.Model,
			"model_revision": space.Revision,
			"dimensions":     space.Dimensions,
			"role":           space.Role,
		})
	}))
	defer server.Close()

	client := &codeModelRuntimeClient{
		baseURL:           server.URL,
		httpClient:        server.Client(),
		serviceToken:      "service-token",
		controlPlaneToken: "control-token",
	}
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
		_ = json.NewEncoder(w).Encode(map[string]any{
			"embedding":      []float64{0.1, 0.2},
			"model":          "other-model",
			"model_revision": "rev-a",
			"dimensions":     2,
			"role":           "code",
		})
	}))
	defer server.Close()

	client := &codeModelRuntimeClient{baseURL: server.URL, httpClient: server.Client()}
	_, err := client.EmbedCode(context.Background(), "x", codeEmbeddingSpace{
		Role: "code", Provider: "scos-model-runtime", Model: "BAAI/bge-code-v1", Revision: "rev-a", Dimensions: 2, Generation: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "outside active space") {
		t.Fatalf("expected space mismatch, got %v", err)
	}
}

func TestCodeEmbeddingDocumentIsDeterministicAndContentBearing(t *testing.T) {
	symbol := PGSymbolVersion{
		FullyQualifiedName: "pkg.Repository.QueryDense",
		Path:               "services/code-intelligence/cmd/server/pgstore.go",
		Kind:               "method",
		Signature:          "func (s *pgCodeStore) QueryDense(...) ([]PGSymbolVersion, error)",
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
	lexical := []PGSymbolVersion{
		{SymbolVersionID: "a", FullyQualifiedName: "pkg.A", Path: "a.go", Relevance: 0.9, MatchType: "lexical"},
		{SymbolVersionID: "b", FullyQualifiedName: "pkg.B", Path: "b.go", Relevance: 0.5, MatchType: "lexical"},
	}
	dense := []PGSymbolVersion{
		{SymbolVersionID: "b", FullyQualifiedName: "pkg.B", Path: "b.go", DenseScore: 0.98, MatchType: "dense"},
		{SymbolVersionID: "c", FullyQualifiedName: "pkg.C", Path: "c.go", DenseScore: 0.8, MatchType: "dense"},
	}

	got := fuseCodeResults(lexical, dense, 60)
	if len(got) != 3 {
		t.Fatalf("len = %d", len(got))
	}
	if got[0].SymbolVersionID != "b" {
		t.Fatalf("cross-lane agreement should rank b first, got %#v", got)
	}
	if got[0].Relevance != 0.5 || got[0].DenseScore != 0.98 || got[0].RRFScore <= 0 {
		t.Fatalf("score provenance lost: %#v", got[0])
	}
	if got[0].MatchType != "hybrid" {
		t.Fatalf("match type = %q", got[0].MatchType)
	}
}
