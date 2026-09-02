package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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

// cacheKey turns a repo's identity plus everything that can change what
// graphify would produce for it -- current content state, the graphify
// binary's own version, and the backend flag -- into a filesystem-safe
// directory name. Any one of those changing yields a different key, so a
// stale cache entry is never returned; it just becomes unreachable and gets
// rebuilt on next use. Old generations are not garbage-collected here.
func cacheKey(workDir, snapshotID, extractorVersion, backend string) string {
	// Hash rather than flatten. Replacing path separators with underscores
	// made "/a/b" and "/a_b" collide onto the same cache directory, which
	// served one repository's graph in answer to a question about another. A
	// digest of the cleaned path plus state is collision-free and still
	// filesystem-safe; the trailing basename is kept only so the directory is
	// recognisable to a human clearing the cache by hand.
	clean := filepath.Clean(workDir)
	h := sha256.New()
	h.Write([]byte(clean))
	h.Write([]byte{0})
	h.Write([]byte(snapshotID))
	h.Write([]byte{0})
	h.Write([]byte(extractorVersion))
	h.Write([]byte{0})
	h.Write([]byte(backend))
	sum := h.Sum(nil)
	return sanitiseComponent(filepath.Base(clean)) + "-" + hex.EncodeToString(sum[:8])
}

// repoSnapshotID fingerprints workDir's current on-disk state so a graph
// built from one snapshot is never served in answer to a question about a
// later, changed one. Git is authoritative when available: HEAD identifies
// every committed byte, but a bare HEAD says nothing about uncommitted
// edits, so status is consulted for anything git itself considers changed
// and those paths are content-hashed individually -- an edit made without a
// commit still changes the fingerprint. Only when workDir isn't a git
// checkout at all does this fall back to hashing every file's path/size/
// mtime, which is coarser but doesn't require git.
func repoSnapshotID(ctx context.Context, workDir string) (string, error) {
	if id, ok, err := gitSnapshotID(ctx, workDir); ok {
		return id, err
	}
	return treeSnapshotID(workDir)
}

// gitSnapshotID returns (id, true, err) when workDir is a git checkout --
// err non-nil there means git itself failed after confirming a checkout, a
// real error the caller should surface -- or ("", false, nil) when workDir
// isn't a git checkout at all, telling the caller to fall back.
func gitSnapshotID(ctx context.Context, workDir string) (string, bool, error) {
	head, err := runGitCapture(ctx, workDir, "rev-parse", "HEAD")
	if err != nil {
		return "", false, nil
	}
	status, err := runGitCapture(ctx, workDir, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return "", true, fmt.Errorf("git status: %w", err)
	}
	if status == "" {
		return "git:" + head, true, nil
	}
	lines := strings.Split(status, "\n")
	sort.Strings(lines)
	h := sha256.New()
	h.Write([]byte(head))
	for _, line := range lines {
		if line == "" {
			continue
		}
		h.Write([]byte(line))
		h.Write([]byte{'\n'})
		if len(line) < 4 {
			continue
		}
		path := strings.TrimSpace(line[3:])
		if path == "" {
			continue
		}
		content, err := os.ReadFile(filepath.Join(workDir, path))
		if err != nil {
			// Deleted path, submodule, or anything else that isn't a
			// readable regular file: the status line itself already changed
			// the hash, so there's nothing further to add.
			continue
		}
		sum := sha256.Sum256(content)
		h.Write(sum[:])
	}
	return "git:" + head + ":dirty:" + hex.EncodeToString(h.Sum(nil)[:8]), true, nil
}

func runGitCapture(ctx context.Context, workDir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = workDir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// treeSnapshotID fingerprints every regular file's path, size and mtime
// under workDir. It's the fallback for a workDir that isn't a git checkout;
// content isn't hashed here because doing so over an entire non-git
// directory tree (rather than just the handful of paths git status reports
// as changed) would make every cache lookup pay for a full read of the tree.
func treeSnapshotID(workDir string) (string, error) {
	h := sha256.New()
	err := filepath.WalkDir(workDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(workDir, path)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s:%d:%d\n", rel, info.Size(), info.ModTime().UnixNano())
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("fingerprint %s: %w", workDir, err)
	}
	return "tree:" + hex.EncodeToString(h.Sum(nil)[:8]), nil
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

// ensureCachedGraph returns workDir's graph.json, building it under
// cacheDir if no cache entry exists for the repo's *current* state -- so
// query/path/explain work against any repo without a separate manual
// extract call, and without needing write access to workDir itself, which
// this service only ever mounts read-only. The cache key folds in a content
// fingerprint of workDir (see repoSnapshotID), the graphify binary's own
// version, and the backend flag, so a repo that has changed since its last
// extract -- even by an uncommitted edit -- misses the cache and triggers a
// fresh extraction instead of silently serving stale graph data. Returns
// the path to the resulting graph.json.
func (s *graphifyServer) ensureCachedGraph(ctx context.Context, workDir string) (string, error) {
	snapshotID, err := repoSnapshotID(ctx, workDir)
	if err != nil {
		return "", fmt.Errorf("fingerprint repo state: %w", err)
	}
	outDir := filepath.Join(s.cacheDir, cacheKey(workDir, snapshotID, s.extractorVersion(ctx), s.backend))
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
	if s.backend != "" {
		args = append(args, "--backend", s.backend)
	}
	if _, err := runGraphify(ctx, outDir, args...); err != nil {
		return "", err
	}
	return graphPath, nil
}

// graphNode/graphEdge/graphDocument mirror graphify's actual networkx
// node-link output (originally verified against a real `graphify extract
// --code-only` run, graphifyy 0.9.48, which serialized edges under
// "links"). Confidence tags live only on edges (nodes themselves are
// always AST-observed, so they carry no confidence field), and
// source_location is a string like "L17", not a line number.
//
// The edge key is NOT stable across graphify versions: networkx's
// node_link_data changed its default edges-key parameter from "links" to
// "edges" between the two, and graphify inherited that change without a
// major version bump of its own. Verified live: the 0.9.48 binary this
// package's test fixture came from emits {"links": [...]}, while 0.9.52
// (deployed in production as of 2026-09) emits {"edges": [...]} for the
// same repository -- silently, with no top-level version field in the
// output to detect which format is in play. Decoding only "links" meant
// every edge from a 0.9.52+ extraction was dropped with no error: the
// call succeeded, node count looked plausible, and the graph was simply
// edgeless. graphDocument's UnmarshalJSON below tries both keys.
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
	Edges []graphEdge `json:"-"`
}

// UnmarshalJSON tries "links" (graphify <= 0.9.48, and this package's
// pinned test fixture) first, falling back to "edges" (graphify >= 0.9.52)
// when "links" is absent or empty. A document that genuinely has neither
// key, or an empty edge list either way, still decodes successfully with
// Edges == nil -- an edgeless graph is a valid (if unusual) answer for a
// tiny repository, not a decode failure.
func (d *graphDocument) UnmarshalJSON(data []byte) error {
	var raw struct {
		Nodes []graphNode `json:"nodes"`
		Links []graphEdge `json:"links"`
		Edges []graphEdge `json:"edges"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	d.Nodes = raw.Nodes
	if len(raw.Links) > 0 {
		d.Edges = raw.Links
	} else {
		d.Edges = raw.Edges
	}
	return nil
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
