package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// runGraphify execs the graphify CLI with args passed as a slice (never
// through a shell), since query/path/explain arguments are user-controlled
// MCP tool input.
func runGraphify(ctx context.Context, workDir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "graphify", args...)
	cmd.Dir = workDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// runExtract runs graphify's headless build (`graphify extract --code-only`,
// no API key required) and returns the path to the resulting graph.json.
func runExtract(ctx context.Context, workDir, backend string) (string, error) {
	args := []string{"extract", workDir, "--code-only"}
	if backend != "" {
		args = []string{"extract", workDir, "--backend", backend}
	}
	if _, err := runGraphify(ctx, workDir, args...); err != nil {
		return "", err
	}
	return filepath.Join(workDir, "graphify-out", "graph.json"), nil
}

// graphNode/graphEdge/graphDocument mirror graphify's actual networkx
// node-link output (verified against a real `graphify extract --code-only`
// run, graphifyy 0.9.48): the graph is serialized as {nodes, links}, not
// {nodes, edges}, confidence tags live only on links (nodes themselves are
// always AST-observed, so they carry no confidence field), and
// source_location is a string like "L17", not a line number.
type graphNode struct {
	ID             string `json:"id"`
	Label          string `json:"label"`
	FileType       string `json:"file_type,omitempty"`
	SourceFile     string `json:"source_file,omitempty"`
	SourceLocation string `json:"source_location,omitempty"`
}

type graphEdge struct {
	Source          string  `json:"source"`
	Target          string  `json:"target"`
	Relation        string  `json:"relation,omitempty"`
	Confidence      string  `json:"confidence,omitempty"`
	ConfidenceScore float64 `json:"confidence_score,omitempty"`
	SourceFile      string  `json:"source_file,omitempty"`
	SourceLocation  string  `json:"source_location,omitempty"`
}

type graphDocument struct {
	Nodes []graphNode `json:"nodes"`
	Edges []graphEdge `json:"links"`
}

func loadGraph(path string) (*graphDocument, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc graphDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	return &doc, nil
}

// postJSON POSTs a JSON body to an internal SCOS peer, sending the tenant/
// principal identity headers this service was configured with, matching
// the copyIdentityHeaders convention used elsewhere (mcp-gateway, hermes
// adapter). out is optional; pass nil to discard the response body.
func (s *graphifyServer) postJSON(ctx context.Context, url string, body, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-SCOS-Tenant-ID", s.tenantID)
	req.Header.Set("X-SCOS-Principal-ID", s.principalID)
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("upstream returned %s", resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
