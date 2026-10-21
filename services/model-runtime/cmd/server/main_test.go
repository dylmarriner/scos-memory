package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestEmbedTextRoutesToConfiguredBackend(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embed" {
			t.Fatalf("expected /embed, got %s", r.URL.Path)
		}
		var req struct {
			Inputs string `json:"inputs"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Inputs != "hello scos" {
			t.Fatalf("unexpected input %q", req.Inputs)
		}
		_ = json.NewEncoder(w).Encode([][]float64{{0.1, 0.2, 0.3}})
	}))
	defer backend.Close()

	h := newRuntimeHandler(runtimeConfig{
		Text: backendConfig{URL: backend.URL, Model: "BAAI/bge-m3", Revision: "rev-a", Dimensions: 3, Required: true},
	}, &http.Client{Timeout: time.Second})

	req := httptest.NewRequest(http.MethodPost, "/v1/embed/text", strings.NewReader(`{"input":"hello scos"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp embeddingResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Model != "BAAI/bge-m3" || resp.ModelRevision != "rev-a" || resp.Role != "text" || resp.Dimensions != 3 {
		t.Fatalf("unexpected metadata: %+v", resp)
	}
	if len(resp.Embedding) != 3 {
		t.Fatalf("expected 3 dimensions, got %d", len(resp.Embedding))
	}
}

func TestEmbedRejectsDimensionMismatch(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([][]float64{{0.1, 0.2}})
	}))
	defer backend.Close()

	h := newRuntimeHandler(runtimeConfig{
		Text: backendConfig{URL: backend.URL, Model: "BAAI/bge-m3", Revision: "rev-a", Dimensions: 3, Required: true},
	}, backend.Client())

	req := httptest.NewRequest(http.MethodPost, "/v1/embed/text", strings.NewReader(`{"input":"hello"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRerankReturnsBackendOrdering(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rerank" {
			t.Fatalf("expected /rerank, got %s", r.URL.Path)
		}
		var req struct {
			Query string   `json:"query"`
			Texts []string `json:"texts"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Query != "memory" || len(req.Texts) != 2 {
			t.Fatalf("unexpected rerank payload: %+v", req)
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"index": 1, "score": 0.91},
			{"index": 0, "score": 0.42},
		})
	}))
	defer backend.Close()

	h := newRuntimeHandler(runtimeConfig{
		Rerank: backendConfig{URL: backend.URL, Model: "BAAI/bge-reranker-v2-m3", Revision: "rev-r", Required: true},
	}, backend.Client())

	req := httptest.NewRequest(http.MethodPost, "/v1/rerank", strings.NewReader(`{"query":"memory","documents":["a","b"],"top_n":2}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp rerankResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 2 || resp.Results[0].Index != 1 || resp.Results[0].Score != 0.91 {
		t.Fatalf("unexpected rerank result: %+v", resp)
	}
}

func TestRerankRejectsInvalidBackendIndex(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{{"index": 9, "score": 0.9}})
	}))
	defer backend.Close()

	h := newRuntimeHandler(runtimeConfig{
		Rerank: backendConfig{URL: backend.URL, Model: "BAAI/bge-reranker-v2-m3", Revision: "rev-r", Required: true},
	}, backend.Client())
	req := httptest.NewRequest(http.MethodPost, "/v1/rerank", strings.NewReader(`{"query":"q","documents":["a","b"]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestEmptyInputRejectedBeforeBackendRequest(t *testing.T) {
	h := newRuntimeHandler(runtimeConfig{
		Text: backendConfig{URL: "http://127.0.0.1:1", Model: "BAAI/bge-m3", Revision: "rev-a", Dimensions: 3},
	}, &http.Client{Timeout: time.Second})
	req := httptest.NewRequest(http.MethodPost, "/v1/embed/text", strings.NewReader(`{"input":"   "}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestConfigRejectsMalformedRequiredFlag(t *testing.T) {
	t.Setenv("SCOS_MODEL_TEXT_REQUIRED", "ture")
	if _, err := configFromEnvironment(); err == nil {
		t.Fatal("expected malformed required flag to fail configuration")
	}
}

// A llama.cpp backend (the Vulkan GPU build) is spoken to in its own shapes:
// OpenAI /v1/embeddings and Jina-style /v1/rerank.
func TestLlamaCPPBackendProtocol(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch r.URL.Path {
		case "/v1/embeddings":
			if req["input"] != "hello scos" {
				t.Fatalf("embeddings request = %v", req)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"index": 0, "embedding": []float64{0.1, 0.2, 0.3}}}})
		case "/v1/rerank":
			if req["query"] != "q" || len(req["documents"].([]any)) != 2 {
				t.Fatalf("rerank request = %v", req)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"results": []any{
				map[string]any{"index": 0, "relevance_score": -1.5},
				map[string]any{"index": 1, "relevance_score": 2.25},
			}})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer backend.Close()
	h := newRuntimeHandler(runtimeConfig{
		Text:   backendConfig{URL: backend.URL, Model: "BAAI/bge-m3", Revision: "gguf-q8_0", Dimensions: 3, Required: true, Protocol: protocolLlamaCPP},
		Rerank: backendConfig{URL: backend.URL, Model: "BAAI/bge-reranker-v2-m3", Revision: "gguf-q8_0", Required: true, Protocol: protocolLlamaCPP},
	}, &http.Client{Timeout: time.Second})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/embed/text", strings.NewReader(`{"input":"hello scos"}`)))
	var emb embeddingResponse
	if rec.Code != http.StatusOK || json.NewDecoder(rec.Body).Decode(&emb) != nil || len(emb.Embedding) != 3 || emb.ModelRevision != "gguf-q8_0" {
		t.Fatalf("embed = %d %+v", rec.Code, emb)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/rerank", strings.NewReader(`{"query":"q","documents":["a","b"]}`)))
	var rr rerankResponse
	if rec.Code != http.StatusOK || json.NewDecoder(rec.Body).Decode(&rr) != nil || len(rr.Results) != 2 || rr.Results[0].Index != 1 || rr.Results[0].Score != 2.25 {
		t.Fatalf("rerank = %d %+v", rec.Code, rr)
	}
}

func TestLlamaCPPRerankWithoutScoreIsInvalid(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[{"index":0}]}`))
	}))
	defer backend.Close()
	h := newRuntimeHandler(runtimeConfig{Rerank: backendConfig{URL: backend.URL, Protocol: protocolLlamaCPP}}, &http.Client{Timeout: time.Second})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/rerank", strings.NewReader(`{"query":"q","documents":["a"]}`)))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("missing relevance_score = %d %s", rec.Code, rec.Body.String())
	}
}

func TestConfigRejectsUnknownProtocol(t *testing.T) {
	t.Setenv("SCOS_MODEL_TEXT_PROTOCOL", "onnx")
	if _, err := configFromEnvironment(); err == nil || !strings.Contains(err.Error(), "SCOS_MODEL_TEXT_PROTOCOL") {
		t.Fatalf("unknown protocol = %v", err)
	}
	t.Setenv("SCOS_MODEL_TEXT_PROTOCOL", "LlamaCPP")
	cfg, err := configFromEnvironment()
	if err != nil || cfg.Text.Protocol != protocolLlamaCPP || cfg.Rerank.Protocol != protocolTEI {
		t.Fatalf("cfg = %+v, %v", cfg, err)
	}
}

// An input longer than a llama.cpp backend's context is cut to fit, counted
// by the backend's own tokenizer, instead of being refused; a short one is
// sent as it is without a tokenizer round trip.
func TestLlamaCPPEmbeddingIsCutToTheBackendContext(t *testing.T) {
	var paths []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch r.URL.Path {
		case "/tokenize":
			// One token per word.
			words := strings.Fields(req["content"].(string))
			tokens := make([]int, len(words))
			for i := range tokens {
				tokens[i] = i
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"tokens": tokens})
		case "/detokenize":
			n := len(req["tokens"].([]any))
			_ = json.NewEncoder(w).Encode(map[string]any{"content": strings.TrimSpace(strings.Repeat("word ", n))})
		case "/v1/embeddings":
			if words := len(strings.Fields(req["input"].(string))); words > 32-llamaCPPSpecialTokens {
				http.Error(w, "input is too large to process", http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"index": 0, "embedding": []float64{0.1, 0.2, 0.3}}}})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer backend.Close()
	h := newRuntimeHandler(runtimeConfig{
		Text: backendConfig{URL: backend.URL, Model: "BAAI/bge-m3", Revision: "gguf-q8_0", Dimensions: 3, Protocol: protocolLlamaCPP, MaxTokens: 32},
	}, &http.Client{Timeout: time.Second})

	long, _ := json.Marshal(map[string]string{"input": strings.Repeat("word ", 100)})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/embed/text", bytes.NewReader(long)))
	if rec.Code != http.StatusOK {
		t.Fatalf("long input = %d %s", rec.Code, rec.Body.String())
	}
	if strings.Join(paths, ",") != "/tokenize,/detokenize,/v1/embeddings" {
		t.Fatalf("calls = %v", paths)
	}

	paths = nil
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/embed/text", strings.NewReader(`{"input":"short"}`)))
	if rec.Code != http.StatusOK || strings.Join(paths, ",") != "/v1/embeddings" {
		t.Fatalf("short input = %d, calls = %v", rec.Code, paths)
	}
}

// A token limit that leaves no room beside the special tokens is refused at
// start-up rather than failing every long input at request time.
func TestConfigRejectsATokenLimitWithNoRoomForInput(t *testing.T) {
	for _, value := range []string{"8", "-1"} {
		t.Setenv("SCOS_MODEL_TEXT_MAX_TOKENS", value)
		if _, err := configFromEnvironment(); err == nil {
			t.Errorf("SCOS_MODEL_TEXT_MAX_TOKENS=%s was accepted", value)
		}
	}
	t.Setenv("SCOS_MODEL_TEXT_MAX_TOKENS", "2048")
	if cfg, err := configFromEnvironment(); err != nil || cfg.Text.MaxTokens != 2048 {
		t.Fatalf("2048 = %+v, %v", cfg.Text, err)
	}
}
