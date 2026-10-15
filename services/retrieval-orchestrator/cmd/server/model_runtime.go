package main

import "github.com/dylmarriner/scos-memory/packages/telemetry"

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

type embeddingSpace struct {
	Role       string `json:"role"`
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	Revision   string `json:"model_revision"`
	Dimensions int    `json:"dimensions"`
	Generation int    `json:"generation"`
}

func (s embeddingSpace) valid() bool {
	return strings.TrimSpace(s.Role) != "" && strings.TrimSpace(s.Provider) != "" && strings.TrimSpace(s.Model) != "" && strings.TrimSpace(s.Revision) != "" && s.Dimensions > 0 && s.Generation > 0
}

type embeddingResult struct {
	Embedding []float64
	Space     embeddingSpace
}

type rerankScore struct {
	Index int     `json:"index"`
	Score float64 `json:"score"`
}

type rerankResponse struct {
	Model         string        `json:"model"`
	ModelRevision string        `json:"model_revision"`
	Results       []rerankScore `json:"results"`
}

type modelRuntimeClient struct {
	baseURL           string
	httpClient        *http.Client
	serviceToken      string
	controlPlaneToken string
}

func newModelRuntimeClientFromEnvironment() (*modelRuntimeClient, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(os.Getenv("SCOS_MODEL_RUNTIME_URL")), "/")
	if baseURL == "" {
		return nil, nil
	}
	cfg, err := auth.FromEnvironment()
	if err != nil {
		return nil, fmt.Errorf("model runtime auth: %w", err)
	}
	timeoutSeconds := 30
	if raw := strings.TrimSpace(os.Getenv("SCOS_MODEL_RUNTIME_TIMEOUT_SECONDS")); raw != "" {
		parsed, parseErr := strconv.Atoi(raw)
		if parseErr != nil || parsed <= 0 {
			return nil, fmt.Errorf("SCOS_MODEL_RUNTIME_TIMEOUT_SECONDS must be a positive integer")
		}
		timeoutSeconds = parsed
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	if strings.HasPrefix(strings.ToLower(baseURL), "https://") {
		caFile := strings.TrimSpace(os.Getenv("SCOS_MODEL_RUNTIME_CA_FILE"))
		if caFile != "" {
			pem, readErr := os.ReadFile(caFile)
			if readErr != nil {
				return nil, fmt.Errorf("read model runtime CA: %w", readErr)
			}
			roots, rootErr := x509.SystemCertPool()
			if rootErr != nil || roots == nil {
				roots = x509.NewCertPool()
			}
			if !roots.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("model runtime CA contains no certificates")
			}
			transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
		}
	}
	return &modelRuntimeClient{
		baseURL:           baseURL,
		httpClient:        &http.Client{Timeout: time.Duration(timeoutSeconds) * time.Second, Transport: telemetry.ProcessTransport(transport)},
		serviceToken:      cfg.Token,
		controlPlaneToken: cfg.ControlPlaneToken,
	}, nil
}

func activeTextEmbeddingSpaceFromEnvironment() (embeddingSpace, error) {
	dimensions, err := positiveEnvInt("SCOS_MODEL_TEXT_DIMENSIONS", 1024)
	if err != nil {
		return embeddingSpace{}, err
	}
	generation, err := positiveEnvInt("SCOS_MODEL_TEXT_GENERATION", 1)
	if err != nil {
		return embeddingSpace{}, err
	}
	space := embeddingSpace{
		Role:       "text",
		Provider:   envOr("SCOS_MODEL_TEXT_PROVIDER", "scos-model-runtime"),
		Model:      envOr("SCOS_MODEL_TEXT_MODEL", "BAAI/bge-m3"),
		Revision:   envOr("SCOS_MODEL_TEXT_REVISION", "main"),
		Dimensions: dimensions,
		Generation: generation,
	}
	if !space.valid() {
		return embeddingSpace{}, fmt.Errorf("active text embedding space is incomplete")
	}
	return space, nil
}

func positiveEnvInt(name string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return value, nil
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func (c *modelRuntimeClient) EmbedText(ctx context.Context, input string, expected embeddingSpace) (embeddingResult, error) {
	if c == nil || strings.TrimSpace(c.baseURL) == "" || c.httpClient == nil {
		return embeddingResult{}, fmt.Errorf("model runtime is not configured")
	}
	if !expected.valid() || expected.Role != "text" {
		return embeddingResult{}, fmt.Errorf("expected text embedding space is invalid")
	}
	var out struct {
		Embedding     []float64 `json:"embedding"`
		Model         string    `json:"model"`
		ModelRevision string    `json:"model_revision"`
		Dimensions    int       `json:"dimensions"`
		Role          string    `json:"role"`
	}
	if err := c.postJSON(ctx, "/v1/embed/text", map[string]string{"input": input}, &out); err != nil {
		return embeddingResult{}, err
	}
	if out.Role != expected.Role || out.Model != expected.Model || out.ModelRevision != expected.Revision || out.Dimensions != expected.Dimensions || len(out.Embedding) != expected.Dimensions {
		return embeddingResult{}, fmt.Errorf("model runtime returned embedding outside active space: role=%q model=%q revision=%q dimensions=%d vector=%d", out.Role, out.Model, out.ModelRevision, out.Dimensions, len(out.Embedding))
	}
	return embeddingResult{Embedding: out.Embedding, Space: expected}, nil
}

func (c *modelRuntimeClient) Rerank(ctx context.Context, query string, documents []string, topN int) (rerankResponse, error) {
	if c == nil || strings.TrimSpace(c.baseURL) == "" || c.httpClient == nil {
		return rerankResponse{}, fmt.Errorf("model runtime is not configured")
	}
	if strings.TrimSpace(query) == "" || len(documents) == 0 {
		return rerankResponse{}, fmt.Errorf("rerank query and documents are required")
	}
	if len(documents) > 128 {
		return rerankResponse{}, fmt.Errorf("rerank accepts at most 128 documents")
	}
	if topN <= 0 || topN > len(documents) {
		topN = len(documents)
	}
	payload := map[string]any{"query": query, "documents": documents, "top_n": topN}
	var out rerankResponse
	if err := c.postJSON(ctx, "/v1/rerank", payload, &out); err != nil {
		return rerankResponse{}, err
	}
	if strings.TrimSpace(out.Model) == "" || strings.TrimSpace(out.ModelRevision) == "" {
		return rerankResponse{}, fmt.Errorf("reranker response is missing model identity")
	}
	seen := make(map[int]struct{}, len(out.Results))
	for _, result := range out.Results {
		if result.Index < 0 || result.Index >= len(documents) {
			return rerankResponse{}, fmt.Errorf("reranker returned out-of-range index %d", result.Index)
		}
		if _, duplicate := seen[result.Index]; duplicate {
			return rerankResponse{}, fmt.Errorf("reranker returned duplicate index %d", result.Index)
		}
		seen[result.Index] = struct{}{}
	}
	return out, nil
}

func (c *modelRuntimeClient) postJSON(ctx context.Context, path string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.serviceToken)
	req.Header.Set("X-SCOS-Control-Plane-Token", c.controlPlaneToken)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		limited, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("model runtime %s returned %d: %s", path, resp.StatusCode, strings.TrimSpace(string(limited)))
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out); err != nil {
		return fmt.Errorf("decode model runtime %s: %w", path, err)
	}
	return nil
}

func applyRerankScores(results []RetrievalResult, response rerankResponse) ([]RetrievalResult, error) {
	if len(results) == 0 {
		return []RetrievalResult{}, nil
	}
	if len(response.Results) == 0 {
		return nil, fmt.Errorf("reranker returned no results")
	}
	ranked := make([]RetrievalResult, 0, len(response.Results))
	seen := make(map[int]struct{}, len(response.Results))
	for _, score := range response.Results {
		if score.Index < 0 || score.Index >= len(results) {
			return nil, fmt.Errorf("reranker returned out-of-range index %d", score.Index)
		}
		if _, duplicate := seen[score.Index]; duplicate {
			return nil, fmt.Errorf("reranker returned duplicate index %d", score.Index)
		}
		seen[score.Index] = struct{}{}
		item := results[score.Index]
		item.RerankerScore = score.Score
		ranked = append(ranked, item)
	}
	// A backend may legally return top-N rather than every document. Preserve
	// omitted candidates after the model-ranked window in their existing RRF
	// order so reranker truncation never silently drops evidence.
	for index, item := range results {
		if _, ok := seen[index]; !ok {
			ranked = append(ranked, item)
		}
	}
	return ranked, nil
}

func applyGovernanceWeighting(results []RetrievalResult) []RetrievalResult {
	weighted := make([]RetrievalResult, len(results))
	copy(weighted, results)
	for i := range weighted {
		base := weighted[i].RRFScore
		if weighted[i].RerankerScore != 0 {
			// Keep the learned score dominant while retaining a small contribution
			// from cross-source agreement. Reranker scores may be negative on some
			// backends, so shift into a positive monotonic base first.
			base = (weighted[i].RerankerScore + 1) + 0.1*weighted[i].RRFScore
		}
		if base <= 0 {
			base = weighted[i].FusedScore
		}
		authorityBoost := float64(weighted[i].Authority) / 100.0
		confidenceBoost := weighted[i].Confidence
		weighted[i].FusedScore = base * (1 + 0.15*authorityBoost + 0.15*confidenceBoost)
	}
	sort.SliceStable(weighted, func(i, j int) bool { return weighted[i].FusedScore > weighted[j].FusedScore })
	for i := range weighted {
		weighted[i].Rank = i + 1
	}
	return weighted
}
