package main

import "github.com/dylmarriner/scos-memory/packages/telemetry"

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dylmarriner/scos-memory/packages/auth"
	"github.com/dylmarriner/scos-memory/packages/release"
	"github.com/dylmarriner/scos-memory/packages/tlsserve"
)

const (
	defaultAddr       = ":8103"
	maxRequestBytes   = 1 << 20
	maxRerankDocs     = 128
	defaultTimeoutSec = 60
)

// Backend protocols. TEI (text-embeddings-inference) serves /embed and
// /rerank. llama.cpp's server serves the OpenAI /v1/embeddings shape and a
// Jina-style /v1/rerank; its Vulkan build runs models entirely in GPU memory
// on cards TEI has no build for, such as AMD Polaris.
const (
	protocolTEI      = "tei"
	protocolLlamaCPP = "llamacpp"
)

type backendConfig struct {
	URL        string
	Model      string
	Revision   string
	Dimensions int
	Required   bool
	Protocol   string
}

type runtimeConfig struct {
	Text   backendConfig
	Code   backendConfig
	Rerank backendConfig
}

type runtimeServer struct {
	config runtimeConfig
	client *http.Client
}

type embeddingRequest struct {
	Input string `json:"input"`
}

type embeddingResponse struct {
	Embedding     []float64 `json:"embedding"`
	Model         string    `json:"model"`
	ModelRevision string    `json:"model_revision"`
	Dimensions    int       `json:"dimensions"`
	Role          string    `json:"role"`
}

type rerankRequest struct {
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
	TopN      int      `json:"top_n,omitempty"`
}

type rerankResult struct {
	Index int     `json:"index"`
	Score float64 `json:"score"`
}

type rerankResponse struct {
	Model         string         `json:"model"`
	ModelRevision string         `json:"model_revision"`
	Results       []rerankResult `json:"results"`
}

func main() {
	cfg, err := configFromEnvironment()
	if err != nil {
		log.Fatal(err)
	}
	timeoutSeconds := envInt("SCOS_MODEL_RUNTIME_TIMEOUT_SECONDS", defaultTimeoutSec)
	if timeoutSeconds < 1 {
		log.Fatal("SCOS_MODEL_RUNTIME_TIMEOUT_SECONDS must be positive")
	}
	client := &http.Client{Timeout: time.Duration(timeoutSeconds) * time.Second}
	handler := auth.ControlPlaneHandler(newRuntimeHandler(cfg, client), auth.MustFromEnvironment())
	addr := strings.TrimSpace(os.Getenv("SCOS_MODEL_RUNTIME_HTTP_ADDR"))
	if addr == "" {
		addr = defaultAddr
	}
	log.Printf("model runtime listening on %s", addr)
	if err := tlsserve.ListenAndServe(addr, telemetry.Instrument("model-runtime", handler)); err != nil {
		log.Fatal(err)
	}
}

func newRuntimeHandler(cfg runtimeConfig, client *http.Client) http.Handler {
	if client == nil {
		client = &http.Client{Timeout: defaultTimeoutSec * time.Second}
	}
	s := &runtimeServer{config: cfg, client: client}
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", s.live)
	mux.HandleFunc("/health/ready", s.ready)
	mux.HandleFunc("/health/version", release.Handler("model-runtime"))
	mux.HandleFunc("/v1/embed/text", func(w http.ResponseWriter, r *http.Request) {
		s.embed(w, r, "text", cfg.Text)
	})
	mux.HandleFunc("/v1/embed/code", func(w http.ResponseWriter, r *http.Request) {
		s.embed(w, r, "code", cfg.Code)
	})
	mux.HandleFunc("/v1/rerank", s.rerank)
	return mux
}

func (s *runtimeServer) live(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "live"})
}

func (s *runtimeServer) ready(w http.ResponseWriter, r *http.Request) {
	type namedBackend struct {
		name string
		cfg  backendConfig
	}
	backends := []namedBackend{{"text", s.config.Text}, {"code", s.config.Code}, {"rerank", s.config.Rerank}}
	for _, backend := range backends {
		if !backend.cfg.Required {
			continue
		}
		if strings.TrimSpace(backend.cfg.URL) == "" {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unready", "error": backend.name + " backend is required but not configured"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		err := s.probeBackend(ctx, backend.cfg)
		cancel()
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unready", "error": backend.name + " backend unavailable"})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *runtimeServer) probeBackend(ctx context.Context, cfg backendConfig) error {
	endpoint := strings.TrimRight(cfg.URL, "/") + "/health"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("backend health returned %s", resp.Status)
	}
	return nil
}

func (s *runtimeServer) embed(w http.ResponseWriter, r *http.Request, role string, cfg backendConfig) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if strings.TrimSpace(cfg.URL) == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": role + " embedding backend is not configured"})
		return
	}
	var body embeddingRequest
	if err := decodeBody(w, r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	body.Input = strings.TrimSpace(body.Input)
	if body.Input == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "input is required"})
		return
	}

	path, payload := "/embed", map[string]any{"inputs": body.Input}
	if cfg.Protocol == protocolLlamaCPP {
		path, payload = "/v1/embeddings", map[string]any{"input": body.Input}
	}
	encoded, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, strings.TrimRight(cfg.URL, "/")+path, bytes.NewReader(encoded))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "create backend request"})
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "embedding backend unavailable"})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "embedding backend returned " + resp.Status})
		return
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "read embedding backend response"})
		return
	}
	embedding, err := decodeEmbedding(raw, cfg.Protocol)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "invalid embedding backend response"})
		return
	}
	if cfg.Dimensions > 0 && len(embedding) != cfg.Dimensions {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": fmt.Sprintf("embedding dimensions mismatch: expected %d, got %d", cfg.Dimensions, len(embedding))})
		return
	}
	for _, value := range embedding {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "embedding contains non-finite value"})
			return
		}
	}
	writeJSON(w, http.StatusOK, embeddingResponse{
		Embedding: embedding, Model: cfg.Model, ModelRevision: cfg.Revision,
		Dimensions: len(embedding), Role: role,
	})
}

func (s *runtimeServer) rerank(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	cfg := s.config.Rerank
	if strings.TrimSpace(cfg.URL) == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "rerank backend is not configured"})
		return
	}
	var body rerankRequest
	if err := decodeBody(w, r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	body.Query = strings.TrimSpace(body.Query)
	if body.Query == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "query is required"})
		return
	}
	if len(body.Documents) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "documents are required"})
		return
	}
	if len(body.Documents) > maxRerankDocs {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("documents exceeds maximum of %d", maxRerankDocs)})
		return
	}
	for i, document := range body.Documents {
		if strings.TrimSpace(document) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("documents[%d] is empty", i)})
			return
		}
	}
	if body.TopN <= 0 || body.TopN > len(body.Documents) {
		body.TopN = len(body.Documents)
	}

	path, payload := "/rerank", map[string]any{"query": body.Query, "texts": body.Documents}
	if cfg.Protocol == protocolLlamaCPP {
		path, payload = "/v1/rerank", map[string]any{"query": body.Query, "documents": body.Documents}
	}
	encoded, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, strings.TrimRight(cfg.URL, "/")+path, bytes.NewReader(encoded))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "create rerank backend request"})
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "rerank backend unavailable"})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "rerank backend returned " + resp.Status})
		return
	}
	raw, err := decodeRerank(io.LimitReader(resp.Body, 4<<20), cfg.Protocol)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "invalid rerank backend response"})
		return
	}
	seen := make(map[int]bool, len(raw))
	for _, result := range raw {
		if result.Index < 0 || result.Index >= len(body.Documents) || seen[result.Index] || math.IsNaN(result.Score) || math.IsInf(result.Score, 0) {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "invalid rerank backend result"})
			return
		}
		seen[result.Index] = true
	}
	sort.SliceStable(raw, func(i, j int) bool { return raw[i].Score > raw[j].Score })
	if len(raw) > body.TopN {
		raw = raw[:body.TopN]
	}
	writeJSON(w, http.StatusOK, rerankResponse{Model: cfg.Model, ModelRevision: cfg.Revision, Results: raw})
}

func decodeBody(w http.ResponseWriter, r *http.Request, target any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid request: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("invalid request: multiple JSON values")
		}
		return fmt.Errorf("invalid request: %w", err)
	}
	return nil
}

// decodeRerank reads a backend's scores: TEI answers [{index, score}],
// llama.cpp {"results": [{index, relevance_score}]}.
func decodeRerank(body io.Reader, protocol string) ([]rerankResult, error) {
	if protocol != protocolLlamaCPP {
		var raw []rerankResult
		err := json.NewDecoder(body).Decode(&raw)
		return raw, err
	}
	var resp struct {
		Results []struct {
			Index *int     `json:"index"`
			Score *float64 `json:"relevance_score"`
		} `json:"results"`
	}
	if err := json.NewDecoder(body).Decode(&resp); err != nil {
		return nil, err
	}
	out := make([]rerankResult, 0, len(resp.Results))
	for _, r := range resp.Results {
		if r.Index == nil || r.Score == nil {
			return nil, errors.New("rerank result without index or relevance_score")
		}
		out = append(out, rerankResult{Index: *r.Index, Score: *r.Score})
	}
	return out, nil
}

func decodeEmbedding(raw []byte, protocol string) ([]float64, error) {
	if protocol == protocolLlamaCPP {
		var resp struct {
			Data []struct {
				Embedding []float64 `json:"embedding"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil || len(resp.Data) != 1 || len(resp.Data[0].Embedding) == 0 {
			return nil, errors.New("expected one embedding in data")
		}
		return resp.Data[0].Embedding, nil
	}
	var vector []float64
	if err := json.Unmarshal(raw, &vector); err == nil && len(vector) > 0 {
		return vector, nil
	}
	var vectors [][]float64
	if err := json.Unmarshal(raw, &vectors); err == nil && len(vectors) == 1 && len(vectors[0]) > 0 {
		return vectors[0], nil
	}
	return nil, errors.New("expected one embedding vector")
}

func configFromEnvironment() (runtimeConfig, error) {
	textRequired, err := envBoolStrict("SCOS_MODEL_TEXT_REQUIRED", false)
	if err != nil {
		return runtimeConfig{}, err
	}
	codeRequired, err := envBoolStrict("SCOS_MODEL_CODE_REQUIRED", false)
	if err != nil {
		return runtimeConfig{}, err
	}
	rerankRequired, err := envBoolStrict("SCOS_MODEL_RERANK_REQUIRED", false)
	if err != nil {
		return runtimeConfig{}, err
	}
	protocols := map[string]string{}
	for _, role := range []string{"TEXT", "CODE", "RERANK"} {
		p := strings.ToLower(envString("SCOS_MODEL_"+role+"_PROTOCOL", protocolTEI))
		if p != protocolTEI && p != protocolLlamaCPP {
			return runtimeConfig{}, fmt.Errorf("SCOS_MODEL_%s_PROTOCOL must be %s or %s, not %q", role, protocolTEI, protocolLlamaCPP, p)
		}
		protocols[role] = p
	}

	cfg := runtimeConfig{
		Text: backendConfig{
			URL: strings.TrimSpace(os.Getenv("SCOS_MODEL_TEXT_URL")), Model: envString("SCOS_MODEL_TEXT_MODEL", "BAAI/bge-m3"),
			Revision: envString("SCOS_MODEL_TEXT_REVISION", "main"), Dimensions: envInt("SCOS_MODEL_TEXT_DIMENSIONS", 1024), Required: textRequired, Protocol: protocols["TEXT"],
		},
		Code: backendConfig{
			URL: strings.TrimSpace(os.Getenv("SCOS_MODEL_CODE_URL")), Model: envString("SCOS_MODEL_CODE_MODEL", "BAAI/bge-code-v1"),
			Revision: envString("SCOS_MODEL_CODE_REVISION", "main"), Dimensions: envInt("SCOS_MODEL_CODE_DIMENSIONS", 1536), Required: codeRequired, Protocol: protocols["CODE"],
		},
		Rerank: backendConfig{
			URL: strings.TrimSpace(os.Getenv("SCOS_MODEL_RERANK_URL")), Model: envString("SCOS_MODEL_RERANK_MODEL", "BAAI/bge-reranker-v2-m3"),
			Revision: envString("SCOS_MODEL_RERANK_REVISION", "main"), Required: rerankRequired, Protocol: protocols["RERANK"],
		},
	}
	for name, backend := range map[string]backendConfig{"text": cfg.Text, "code": cfg.Code, "rerank": cfg.Rerank} {
		if backend.URL != "" {
			if err := validateBackendURL(backend.URL); err != nil {
				return runtimeConfig{}, fmt.Errorf("%s backend: %w", name, err)
			}
		}
		if name != "rerank" && backend.Dimensions <= 0 {
			return runtimeConfig{}, fmt.Errorf("%s backend dimensions must be positive", name)
		}
	}
	return cfg, nil
}

func validateBackendURL(value string) error {
	u, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("URL scheme must be http or https")
	}
	if u.Host == "" || u.User != nil {
		return errors.New("URL must contain a host and no embedded credentials")
	}
	return nil
}

func envString(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envInt(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envBoolStrict(name string, fallback bool) (bool, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean: %w", name, err)
	}
	return parsed, nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("encode response: %v", err)
	}
}
