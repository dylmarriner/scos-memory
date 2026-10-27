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
	"strconv"
	"strings"
	"time"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

type codeQueryResult struct {
	SymbolID           string  `json:"SymbolID"`
	SymbolVersionID    string  `json:"SymbolVersionID"`
	FullyQualifiedName string  `json:"FullyQualifiedName"`
	Path               string  `json:"Path"`
	MatchType          string  `json:"MatchType"`
	Centrality         float64 `json:"Centrality"`
	RawLexicalScore    float64 `json:"raw_lexical_score,omitempty"`
	RawDenseScore      float64 `json:"raw_dense_score,omitempty"`
	RawStructuralScore float64 `json:"raw_structural_score,omitempty"`
	RRFScore           float64 `json:"rrf_score,omitempty"`
	FusedScore         float64 `json:"fused_score,omitempty"`
}

type codeQueryResponse struct {
	Results         []codeQueryResult `json:"results"`
	RetrievalMode   string            `json:"retrieval_mode"`
	Signals         []string          `json:"signals,omitempty"`
	DegradedModels  []string          `json:"degraded_models,omitempty"`
	DegradedSources []string          `json:"degraded_sources,omitempty"`
	EmbeddingSpace  json.RawMessage   `json:"embedding_space,omitempty"`
}

type codeIntelligenceClient struct {
	baseURL           string
	httpClient        *http.Client
	serviceToken      string
	controlPlaneToken string
}

func newCodeIntelligenceClientFromEnvironment() (*codeIntelligenceClient, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(os.Getenv("SCOS_CODE_INTELLIGENCE_URL")), "/")
	if baseURL == "" {
		return nil, nil
	}
	cfg, err := auth.FromEnvironment()
	if err != nil {
		return nil, fmt.Errorf("code-intelligence auth: %w", err)
	}
	timeout := 15
	if raw := strings.TrimSpace(os.Getenv("SCOS_CODE_INTELLIGENCE_TIMEOUT_SECONDS")); raw != "" {
		v, e := strconv.Atoi(raw)
		if e != nil || v <= 0 {
			return nil, fmt.Errorf("SCOS_CODE_INTELLIGENCE_TIMEOUT_SECONDS must be a positive integer")
		}
		timeout = v
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if strings.HasPrefix(strings.ToLower(baseURL), "https://") {
		if caFile := strings.TrimSpace(os.Getenv("SCOS_TLS_CA_FILE")); caFile != "" {
			pem, e := os.ReadFile(caFile)
			if e != nil {
				return nil, fmt.Errorf("read SCOS service CA: %w", e)
			}
			roots, e := x509.SystemCertPool()
			if e != nil || roots == nil {
				roots = x509.NewCertPool()
			}
			if !roots.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("SCOS service CA contains no certificates")
			}
			transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
		}
	}
	return &codeIntelligenceClient{baseURL: baseURL, httpClient: &http.Client{Timeout: time.Duration(timeout) * time.Second, Transport: telemetry.ProcessTransport(transport)}, serviceToken: cfg.Token, controlPlaneToken: cfg.ControlPlaneToken}, nil
}

func (c *codeIntelligenceClient) Query(ctx context.Context, tenantID, principalID, repositoryID, branch, query string, limit int) (*codeQueryResponse, error) {
	if c == nil || c.httpClient == nil || c.baseURL == "" {
		return nil, fmt.Errorf("code-intelligence is not configured")
	}
	if repositoryID == "" || branch == "" || strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("repository_id, branch and query are required")
	}
	if limit <= 0 {
		limit = 20
	}
	body, err := json.Marshal(map[string]any{"repository_id": repositoryID, "branch": branch, "query": query, "max_results": limit})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v2/code/query", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.serviceToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.serviceToken)
	}
	if c.controlPlaneToken != "" {
		req.Header.Set("X-SCOS-Control-Plane-Token", c.controlPlaneToken)
	}
	req.Header.Set("X-SCOS-Tenant-ID", tenantID)
	req.Header.Set("X-SCOS-Principal-ID", principalID)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		// Downstream error bodies can contain credentials, SQL and tenant data.
		// Retain the status code for diagnostics, never the untrusted body.
		return nil, fmt.Errorf("code-intelligence returned HTTP %d", resp.StatusCode)
	}
	var out codeQueryResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode code-intelligence response: %w", err)
	}
	if out.Results == nil {
		out.Results = []codeQueryResult{}
	}
	return &out, nil
}
