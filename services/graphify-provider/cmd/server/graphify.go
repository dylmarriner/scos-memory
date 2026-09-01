package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// cacheKey turns an absolute repo path into a filesystem-safe directory
// name so each repo's cached graph lives in its own subdirectory of the
// shared cache volume, keyed by the repo's own path rather than a mutable
// per-request identifier.
func cacheKey(workDir string) string {
	// Hash rather than flatten. Replacing separators with underscores made
	// "/a/b" and "/a_b" collide onto the same cache directory, which serves
	// one repository's graph in answer to a question about another. A digest
	// of the cleaned path is collision-free and still filesystem-safe; the
	// trailing basename is kept only so the directory is recognisable to a
	// human clearing the cache by hand.
	clean := filepath.Clean(workDir)
	sum := sha256.Sum256([]byte(clean))
	return sanitiseComponent(filepath.Base(clean)) + "-" + hex.EncodeToString(sum[:8])
}

// sanitiseComponent reduces a path element to characters that are safe as a
// single directory name, so the human-readable half of a cache key can never
// itself introduce a separator.
func sanitiseComponent(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "repo"
	}
	return b.String()
}

// rejectFlagLike refuses a user-supplied value that graphify's CLI would
// parse as an option rather than as the positional it is meant to be. The
// arguments below reach exec.CommandContext directly, so there is no shell
// and no command injection -- but a query of "--out" is still argument
// injection into whatever flags that binary happens to expose.
func rejectFlagLike(field, value string) error {
	if strings.HasPrefix(value, "-") {
		return fmt.Errorf("%s must not begin with '-'", field)
	}
	return nil
}

// ensureCachedGraph builds workDir's graph.json under cacheDir on first use,
// so query/path/explain work against any repo without a separate manual
// extract call -- and without needing write access to workDir itself, which
// this service only ever mounts read-only. It only checks existence, not
// staleness: a repo that has changed since its last extract needs its cache
// subdirectory removed (or GRAPHIFY_FORCE=1) to pick up the new source --
// this is a one-time bootstrap, not a cache-invalidation mechanism. Returns
// the path to the resulting graph.json.
func ensureCachedGraph(ctx context.Context, workDir, cacheDir, backend string) (string, error) {
	outDir := filepath.Join(cacheDir, cacheKey(workDir))
	graphPath := filepath.Join(outDir, "graphify-out", "graph.json")
	if _, err := os.Stat(graphPath); err == nil {
		return graphPath, nil
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", fmt.Errorf("create cache dir: %w", err)
	}
	// --max-workers 1: this service's container is sized for a Go binary's
	// footprint (GOMEMLIMIT=96MiB, matching every other fabric service), not
	// for graphify's default cpu_count-worker AST extraction -- against a
	// repo with thousands of files, unbounded workers OOM-killed the
	// container even with the memory limit raised. One worker is slower but
	// bounded regardless of repo size or host core count.
	//
	// --no-cluster: even with AST extraction itself bounded, the default
	// post-extraction community-detection pass over a several-thousand-node
	// graph OOM-killed the container on its own (verified against paperclip:
	// AST extraction reached 100% then the process was killed during
	// clustering). query/path/explain traverse the raw node/edge graph and
	// don't read community labels, so skipping clustering costs nothing
	// those handlers use.
	args := []string{"extract", workDir, "--code-only", "--max-workers", "1", "--no-cluster", "--out", outDir}
	if backend != "" {
		args = append(args, "--backend", backend)
	}
	if _, err := runGraphify(ctx, outDir, args...); err != nil {
		return "", err
	}
	return graphPath, nil
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
