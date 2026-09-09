package main

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

type backendConfig struct {
	URL        string
	Model      string
	Revision   string
	Dimensions int
	Required   bool
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
	if err := tlsserve.ListenAndServe(addr, handler); err != nil {
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

	payload, _ := json.Marshal(map[string]any{"inputs": body.Input})
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, strings.TrimRight(cfg.URL, "/")+"/embed", bytes.NewReader(payload))
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
	embedding, err := decodeEmbedding(raw)
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

	payload, _ := json.Marshal(map[string]any{"query": body.Query, "texts": body.Documents})
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, strings.TrimRight(cfg.URL, "/")+"/rerank", bytes.NewReader(payload))
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
	var raw []rerankResult
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&raw); err != nil {
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

func decodeEmbedding(raw []byte) ([]float64, error) {
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
	cfg := runtimeConfig{
		Text: backendConfig{
			URL: strings.TrimSpace(os.Getenv("SCOS_MODEL_TEXT_URL")), Model: envString("SCOS_MODEL_TEXT_MODEL", "BAAI/bge-m3"),
			Revision: envString("SCOS_MODEL_TEXT_REVISION", "main"), Dimensions: envInt("SCOS_MODEL_TEXT_DIMENSIONS", 1024), Required: envBool("SCOS_MODEL_TEXT_REQUIRED", false),
		},
		Code: backendConfig{
			URL: strings.TrimSpace(os.Getenv("SCOS_MODEL_CODE_URL")), Model: envString("SCOS_MODEL_CODE_MODEL", "BAAI/bge-code-v1"),
			Revision: envString("SCOS_MODEL_CODE_REVISION", "main"), Dimensions: envInt("SCOS_MODEL_CODE_DIMENSIONS", 1536), Required: envBool("SCOS_MODEL_CODE_REQUIRED", false),
		},
		Rerank: backendConfig{
			URL: strings.TrimSpace(os.Getenv("SCOS_MODEL_RERANK_URL")), Model: envString("SCOS_MODEL_RERANK_MODEL", "BAAI/bge-reranker-v2-m3"),
			Revision: envString("SCOS_MODEL_RERANK_REVISION", "main"), Required: envBool("SCOS_MODEL_RERANK_REQUIRED", false),
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

func envBool(name string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("encode response: %v", err)
	}
}
