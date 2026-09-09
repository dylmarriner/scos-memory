package main

import (
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
