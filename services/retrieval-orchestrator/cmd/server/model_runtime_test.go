package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestModelRuntimeEmbedTextSendsGovernedAuthAndValidatesSpace(t *testing.T) {
	var sawAuth, sawControl string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embed/text" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		sawAuth = r.Header.Get("Authorization")
		sawControl = r.Header.Get("X-SCOS-Control-Plane-Token")
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["input"] != "find postgres code" {
			t.Fatalf("unexpected input %q", body["input"])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"embedding":      []float64{0.1, 0.2, 0.3},
			"model":          "BAAI/bge-m3",
			"model_revision": "rev-1",
			"dimensions":     3,
			"role":           "text",
		})
	}))
	defer backend.Close()

	client := &modelRuntimeClient{
		baseURL:           backend.URL,
		httpClient:        backend.Client(),
		serviceToken:      "svc-token",
		controlPlaneToken: "cp-token",
	}
	space := embeddingSpace{Role: "text", Provider: "scos-model-runtime", Model: "BAAI/bge-m3", Revision: "rev-1", Dimensions: 3, Generation: 1}
	got, err := client.EmbedText(context.Background(), "find postgres code", space)
	if err != nil {
		t.Fatal(err)
	}
	if sawAuth != "Bearer svc-token" || sawControl != "cp-token" {
		t.Fatalf("runtime auth missing: authorization=%q control=%q", sawAuth, sawControl)
	}
	if len(got.Embedding) != 3 || got.Space != space {
		t.Fatalf("unexpected embedding result: %#v", got)
	}
}

func TestModelRuntimeEmbedTextRejectsWrongDimensions(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"embedding":      []float64{0.1, 0.2},
			"model":          "BAAI/bge-m3",
			"model_revision": "rev-1",
			"dimensions":     2,
			"role":           "text",
		})
	}))
	defer backend.Close()
	client := &modelRuntimeClient{baseURL: backend.URL, httpClient: backend.Client(), serviceToken: "svc", controlPlaneToken: "cp"}
	_, err := client.EmbedText(context.Background(), "q", embeddingSpace{Role: "text", Provider: "scos-model-runtime", Model: "BAAI/bge-m3", Revision: "rev-1", Dimensions: 3, Generation: 1})
	if err == nil {
		t.Fatal("expected dimension mismatch to fail closed")
	}
}

func TestModelRuntimeRerankValidatesIndexes(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/rerank" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":          "BAAI/bge-reranker-v2-m3",
			"model_revision": "rev-r",
			"results": []map[string]any{
				{"index": 1, "score": 0.9},
				{"index": 0, "score": 0.2},
			},
		})
	}))
	defer backend.Close()
	client := &modelRuntimeClient{baseURL: backend.URL, httpClient: backend.Client(), serviceToken: "svc", controlPlaneToken: "cp"}
	got, err := client.Rerank(context.Background(), "q", []string{"a", "b"}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Results) != 2 || got.Results[0].Index != 1 {
		t.Fatalf("unexpected rerank response: %#v", got)
	}
}

func TestVectorSearchWithoutEmbeddingContributesNoSemanticCandidates(t *testing.T) {
	s := &server{}
	got, err := s.vectorSearch(context.Background(), RetrieveRequest{}, nil, "internal", 20)
	if err != nil {
		t.Fatalf("no embedding should degrade cleanly, got %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected zero semantic candidates, got %d", len(got))
	}
}

func TestFuseResultsPreservesRawSemanticScoreAndSeparatesRRF(t *testing.T) {
	semantic := []RetrievalResult{{MemoryID: "m1", RawSemanticScore: 0.82, SemanticScore: 0.82, Provenance: "semantic"}}
	exact := []RetrievalResult{{MemoryID: "m1", RawLexicalScore: 1, LexicalScore: 1, Provenance: "exact"}}
	got := fuseResults(60, exact, nil, semantic)
	if len(got) != 1 {
		t.Fatalf("expected one fused result, got %d", len(got))
	}
	if got[0].RawSemanticScore != 0.82 || got[0].SemanticScore != 0.82 {
		t.Fatalf("semantic similarity was corrupted: %#v", got[0])
	}
	if got[0].RRFScore <= 0 {
		t.Fatalf("expected separate positive RRF score: %#v", got[0])
	}
}

func TestApplyRerankScoresOrdersByModelBeforeGovernance(t *testing.T) {
	in := []RetrievalResult{
		{MemoryID: "authority", Authority: 100, Confidence: 1, RRFScore: 0.03, FusedScore: 0.03},
		{MemoryID: "relevant", Authority: 10, Confidence: 0.5, RRFScore: 0.03, FusedScore: 0.03},
	}
	ranked, err := applyRerankScores(in, rerankResponse{Results: []rerankScore{{Index: 1, Score: 0.95}, {Index: 0, Score: 0.1}}})
	if err != nil {
		t.Fatal(err)
	}
	final := applyGovernanceWeighting(ranked)
	if final[0].MemoryID != "relevant" {
		t.Fatalf("governance boost should refine, not erase reranker relevance; got %#v", final)
	}
	if final[0].RerankerScore != 0.95 {
		t.Fatalf("reranker score not preserved: %#v", final[0])
	}
}

// A reranker slower than the configured budget is given up on within the
// budget, so retrieval answers from RRF + governance in time.
func TestRerankStageGivesUpAtItsBudget(t *testing.T) {
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer backend.Close()
	defer close(release)
	s := &server{
		modelRuntime: &modelRuntimeClient{baseURL: backend.URL, httpClient: backend.Client(), serviceToken: "svc", controlPlaneToken: "cp"},
		rerankLimit:  10,
		rerankBudget: 100 * time.Millisecond,
	}
	started := time.Now()
	_, _, _, err := s.rerankWithModel(context.Background(), "q", []RetrievalResult{{Content: "a"}, {Content: "b"}})
	if err == nil {
		t.Fatal("a reranker past its budget did not fail the stage")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("rerank stage took %v, budget 100ms", elapsed)
	}
}

func TestRerankDocumentIsCappedWithoutSplittingACharacter(t *testing.T) {
	doc := rerankDocument(RetrievalResult{Title: "t", Content: strings.Repeat("é", 50)}, 10)
	if len(doc) > 10 || !utf8.ValidString(doc) {
		t.Fatalf("document = %q (%d bytes)", doc, len(doc))
	}
	if got := rerankDocument(RetrievalResult{Content: strings.Repeat("x", 20000)}, 0); len(got) != defaultRerankDocumentChars {
		t.Fatalf("default cap = %d", len(got))
	}
}
