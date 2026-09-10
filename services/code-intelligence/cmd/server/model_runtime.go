package main

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

type codeEmbeddingSpace struct {
	Role       string `json:"role"`
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	Revision   string `json:"model_revision"`
	Dimensions int    `json:"dimensions"`
	Generation int    `json:"generation"`
}

func (s codeEmbeddingSpace) valid() bool {
	return s.Role == "code" && strings.TrimSpace(s.Provider) != "" && strings.TrimSpace(s.Model) != "" && strings.TrimSpace(s.Revision) != "" && s.Dimensions > 0 && s.Generation > 0
}

type codeEmbeddingResult struct {
	Embedding []float64
	Space     codeEmbeddingSpace
}

type codeModelRuntimeClient struct {
	baseURL           string
	httpClient        *http.Client
	serviceToken      string
	controlPlaneToken string
}

type codeSymbolDocument struct {
	SymbolVersionID    string
	SymbolID           string
	FullyQualifiedName string
	Path               string
	Kind               string
	Signature          string
	DocComment         string
	ChunkContent       string
	ContentHash        string
	SnapshotID         string
	CommitID           string
	Branch             string
}

type codeHybridResult struct {
	PGSymbolVersion
	RawLexicalScore float64 `json:"raw_lexical_score,omitempty"`
	RawDenseScore   float64 `json:"raw_dense_score,omitempty"`
	RRFScore        float64 `json:"rrf_score,omitempty"`
}

func codePositiveEnvInt(name string, fallback int) (int, error) {
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

func codeEnvOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func activeCodeEmbeddingSpaceFromEnvironment() (codeEmbeddingSpace, error) {
	dimensions, err := codePositiveEnvInt("SCOS_MODEL_CODE_DIMENSIONS", 1536)
	if err != nil {
		return codeEmbeddingSpace{}, err
	}
	generation, err := codePositiveEnvInt("SCOS_MODEL_CODE_GENERATION", 1)
	if err != nil {
		return codeEmbeddingSpace{}, err
	}
	space := codeEmbeddingSpace{
		Role:       "code",
		Provider:   codeEnvOr("SCOS_MODEL_CODE_PROVIDER", "scos-model-runtime"),
		Model:      codeEnvOr("SCOS_MODEL_CODE_MODEL", "BAAI/bge-code-v1"),
		Revision:   codeEnvOr("SCOS_MODEL_CODE_REVISION", "main"),
		Dimensions: dimensions,
		Generation: generation,
	}
	if !space.valid() {
		return codeEmbeddingSpace{}, fmt.Errorf("active code embedding space is incomplete")
	}
	return space, nil
}

func newCodeModelRuntimeClientFromEnvironment() (*codeModelRuntimeClient, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(os.Getenv("SCOS_MODEL_RUNTIME_URL")), "/")
	if baseURL == "" {
		return nil, nil
	}
	cfg, err := auth.FromEnvironment()
	if err != nil {
		return nil, fmt.Errorf("model runtime auth: %w", err)
	}
	timeoutSeconds, err := codePositiveEnvInt("SCOS_MODEL_RUNTIME_TIMEOUT_SECONDS", 30)
	if err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if strings.HasPrefix(strings.ToLower(baseURL), "https://") {
		if caFile := strings.TrimSpace(os.Getenv("SCOS_MODEL_RUNTIME_CA_FILE")); caFile != "" {
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
	return &codeModelRuntimeClient{
		baseURL:           baseURL,
		httpClient:        &http.Client{Timeout: time.Duration(timeoutSeconds) * time.Second, Transport: transport},
		serviceToken:      cfg.Token,
		controlPlaneToken: cfg.ControlPlaneToken,
	}, nil
}

func codeRuntimeConfig() (*codeModelRuntimeClient, codeEmbeddingSpace, error) {
	space, err := activeCodeEmbeddingSpaceFromEnvironment()
	if err != nil {
		return nil, codeEmbeddingSpace{}, err
	}
	client, err := newCodeModelRuntimeClientFromEnvironment()
	if err != nil {
		return nil, codeEmbeddingSpace{}, err
	}
	return client, space, nil
}

func (c *codeModelRuntimeClient) EmbedCode(ctx context.Context, input string, expected codeEmbeddingSpace) (codeEmbeddingResult, error) {
	if c == nil || strings.TrimSpace(c.baseURL) == "" || c.httpClient == nil {
		return codeEmbeddingResult{}, fmt.Errorf("model runtime is not configured")
	}
	if strings.TrimSpace(input) == "" {
		return codeEmbeddingResult{}, fmt.Errorf("code embedding input is empty")
	}
	if !expected.valid() {
		return codeEmbeddingResult{}, fmt.Errorf("expected code embedding space is invalid")
	}
	body, err := json.Marshal(map[string]string{"input": input})
	if err != nil {
		return codeEmbeddingResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/embed/code", bytes.NewReader(body))
	if err != nil {
		return codeEmbeddingResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.serviceToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.serviceToken)
	}
	if c.controlPlaneToken != "" {
		req.Header.Set("X-SCOS-Control-Plane-Token", c.controlPlaneToken)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return codeEmbeddingResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		limited, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return codeEmbeddingResult{}, fmt.Errorf("model runtime /v1/embed/code returned %d: %s", resp.StatusCode, strings.TrimSpace(string(limited)))
	}
	var out struct {
		Embedding     []float64 `json:"embedding"`
		Model         string    `json:"model"`
		ModelRevision string    `json:"model_revision"`
		Dimensions    int       `json:"dimensions"`
		Role          string    `json:"role"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&out); err != nil {
		return codeEmbeddingResult{}, fmt.Errorf("decode code embedding response: %w", err)
	}
	if out.Role != expected.Role || out.Model != expected.Model || out.ModelRevision != expected.Revision || out.Dimensions != expected.Dimensions || len(out.Embedding) != expected.Dimensions {
		return codeEmbeddingResult{}, fmt.Errorf("model runtime returned embedding outside active space: role=%q model=%q revision=%q dimensions=%d vector=%d", out.Role, out.Model, out.ModelRevision, out.Dimensions, len(out.Embedding))
	}
	return codeEmbeddingResult{Embedding: out.Embedding, Space: expected}, nil
}

func codeEmbeddingDocument(symbol codeSymbolDocument) string {
	parts := []string{
		"fqn: " + strings.TrimSpace(symbol.FullyQualifiedName),
		"path: " + strings.TrimSpace(symbol.Path),
		"kind: " + strings.TrimSpace(symbol.Kind),
		"signature: " + strings.TrimSpace(symbol.Signature),
		"doc: " + strings.TrimSpace(symbol.DocComment),
		"content: " + strings.TrimSpace(symbol.ChunkContent),
	}
	return strings.Join(parts, "\n")
}

func codeVectorLiteral(values []float64) string {
	parts := make([]string, len(values))
	for i, value := range values {
		parts[i] = strconv.FormatFloat(value, 'g', -1, 64)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func lexicalCodeResults(items []PGSymbolVersion) []codeHybridResult {
	out := make([]codeHybridResult, 0, len(items))
	for _, item := range items {
		out = append(out, codeHybridResult{PGSymbolVersion: item, RawLexicalScore: item.Relevance})
	}
	return out
}

func fuseCodeResults(lexical, dense []codeHybridResult, rrfK int) []codeHybridResult {
	if rrfK <= 0 {
		rrfK = 60
	}
	type entry struct {
		item         codeHybridResult
		lexicalFound bool
		denseFound   bool
	}
	byID := make(map[string]*entry, len(lexical)+len(dense))
	for rank, item := range lexical {
		e := byID[item.SymbolVersionID]
		if e == nil {
			copyItem := item
			e = &entry{item: copyItem}
			byID[item.SymbolVersionID] = e
		}
		e.lexicalFound = true
		e.item.RawLexicalScore = item.RawLexicalScore
		e.item.Relevance = item.RawLexicalScore
		e.item.RRFScore += 1.0 / float64(rrfK+rank+1)
	}
	for rank, item := range dense {
		e := byID[item.SymbolVersionID]
		if e == nil {
			copyItem := item
			e = &entry{item: copyItem}
			byID[item.SymbolVersionID] = e
		}
		e.denseFound = true
		e.item.RawDenseScore = item.RawDenseScore
		e.item.RRFScore += 1.0 / float64(rrfK+rank+1)
	}
	out := make([]codeHybridResult, 0, len(byID))
	for _, e := range byID {
		switch {
		case e.lexicalFound && e.denseFound:
			e.item.MatchType = "hybrid"
		case e.denseFound:
			e.item.MatchType = "dense"
		default:
			e.item.MatchType = "lexical"
		}
		out = append(out, e.item)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].RRFScore != out[j].RRFScore {
			return out[i].RRFScore > out[j].RRFScore
		}
		if out[i].FullyQualifiedName != out[j].FullyQualifiedName {
			return out[i].FullyQualifiedName < out[j].FullyQualifiedName
		}
		return out[i].Path < out[j].Path
	})
	return out
}
