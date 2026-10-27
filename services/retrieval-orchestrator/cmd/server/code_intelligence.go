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
	SnapshotID         string  `json:"SnapshotID,omitempty"`
	CommitID           string  `json:"CommitID,omitempty"`
	Branch             string  `json:"Branch,omitempty"`
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
	Historical      bool              `json:"historical,omitempty"`
	SnapshotID      string            `json:"snapshot_id,omitempty"`
	CommitID        string            `json:"commit_id,omitempty"`
	Branch          string            `json:"branch,omitempty"`
	Results         []codeQueryResult `json:"results"`
	RetrievalMode   string            `json:"retrieval_mode"`
	Signals         []string          `json:"signals,omitempty"`
	DegradedModels  []string          `json:"degraded_models,omitempty"`
	DegradedSources []string          `json:"degraded_sources,omitempty"`
	EmbeddingSpace  json.RawMessage   `json:"embedding_space,omitempty"`
}

// codeQueryHTTPError preserves only the downstream status, never the body.
// Historical callers may distinguish not-found and ambiguous generations.
type codeQueryHTTPError struct{ Status int }

func (e *codeQueryHTTPError) Error() string {
	return fmt.Sprintf("code-intelligence returned HTTP %d", e.Status)
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
	return c.query(ctx, tenantID, principalID, repositoryID, branch, query, limit, "", "")
}

// QueryHistorical reads a concrete immutable code generation. It never falls
// back to active code when the historical service rejects a selector.
func (c *codeIntelligenceClient) QueryHistorical(ctx context.Context, tenantID, principalID, repositoryID, branch, query string, limit int, asOfCommit, snapshotID string) (*codeQueryResponse, error) {
	if asOfCommit == "" && snapshotID == "" {
		return nil, fmt.Errorf("historical query requires a commit or snapshot")
	}
	return c.query(ctx, tenantID, principalID, repositoryID, branch, query, limit, asOfCommit, snapshotID)
}

func (c *codeIntelligenceClient) query(ctx context.Context, tenantID, principalID, repositoryID, branch, query string, limit int, asOfCommit, snapshotID string) (*codeQueryResponse, error) {
	if c == nil || c.httpClient == nil || c.baseURL == "" {
		return nil, fmt.Errorf("code-intelligence is not configured")
	}
	if repositoryID == "" || branch == "" || strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("repository_id, branch and query are required")
	}
	if limit <= 0 {
		limit = 20
	}
	payload := map[string]any{"repository_id": repositoryID, "branch": branch, "query": query, "max_results": limit}
	if asOfCommit != "" {
		payload["as_of_commit"] = asOfCommit
	}
	if snapshotID != "" {
		payload["snapshot_id"] = snapshotID
	}
	body, err := json.Marshal(payload)
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
		// Drain a bounded amount so short error responses keep HTTP connections
		// reusable. Larger responses are deliberately not read unboundedly.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, &codeQueryHTTPError{Status: resp.StatusCode}
	}
	var out codeQueryResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode code-intelligence response: %w", err)
	}
	if asOfCommit != "" || snapshotID != "" {
		// A 2xx response is insufficient: the code service must prove that
		// it returned the requested immutable generation, not active code.
		if !out.Historical || out.SnapshotID == "" || out.CommitID == "" || out.Branch != branch ||
			(asOfCommit != "" && out.CommitID != asOfCommit) ||
			(snapshotID != "" && !strings.EqualFold(out.SnapshotID, snapshotID)) {
			return nil, fmt.Errorf("code-intelligence historical provenance mismatch")
		}
		for _, item := range out.Results {
			if item.SymbolVersionID == "" || item.SnapshotID != out.SnapshotID || item.CommitID != out.CommitID || item.Branch != branch {
				return nil, fmt.Errorf("code-intelligence historical result provenance mismatch")
			}
		}
	}
	if out.Results == nil {
		out.Results = []codeQueryResult{}
	}
	return &out, nil
}
