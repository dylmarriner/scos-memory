package main

import "github.com/dylmarriner/scos-memory/packages/telemetry"
import "github.com/dylmarriner/scos-memory/packages/release"

import "github.com/dylmarriner/scos-memory/packages/auth"
import "github.com/dylmarriner/scos-memory/packages/tlsserve"

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dylmarriner/scos-memory/services/readiness"
)

const version = "0.1.0"
const providerName = "graphify-provider"

// graphifyServer bridges the graphify CLI (github.com/Graphify-Labs/graphify)
// into memory-intelligence: extract builds a knowledge graph and ingests it
// as facts/relations, while query/path/explain proxy graphify's own graph
// traversal commands.
type graphifyServer struct {
	startTime             time.Time
	httpClient            *http.Client
	memoryIntelligenceURL string
	repoPath              string
	allowedRoot           string
	cacheDir              string
	namespaceID           string
	tenantID              string
	principalID           string
	backend               string

	extractorVersionOnce   sync.Once
	extractorVersionCached string
}

// extractorVersion identifies which graphify build this process will run,
// memoized since the binary on disk doesn't change while the service is up.
// A cache key derived from it is still correct across a deploy that
// upgrades graphify: entries built under the old version simply become
// unreachable under the new one and get rebuilt on next use, rather than a
// newer binary's differently-shaped graph being served from an older entry.
func (s *graphifyServer) extractorVersion(ctx context.Context) string {
	s.extractorVersionOnce.Do(func() {
		out, err := exec.CommandContext(ctx, "graphify", "--version").Output()
		if err != nil {
			s.extractorVersionCached = "unknown"
			return
		}
		s.extractorVersionCached = strings.TrimSpace(string(out))
	})
	return s.extractorVersionCached
}

// resolveWorkDir picks the directory query/path/explain operate on: the
// caller-supplied repoPath when given (letting one graphify-provider
// instance serve any repo it's asked about, not just the one bind-mounted
// at startup), or the service's configured default otherwise.
//
// This is the only thing standing between arbitrary MCP tool input and
// exec.CommandContext's cmd.Dir, so it is deliberately strict: a repo_path
// is accepted only when an allowed root is configured and the path's real
// location -- after symlink resolution on both sides -- lies inside it.
func (s *graphifyServer) resolveWorkDir(requested string) (string, error) {
	if requested == "" {
		return s.repoPath, nil
	}
	// Fail closed. An unset GRAPHIFY_ALLOWED_ROOT used to mean "no
	// containment at all", so any deployment that did not come from the
	// Compose file -- the native scripts, a directly-run binary -- accepted
	// an arbitrary caller-supplied directory. An unconfigured boundary is
	// not an absent boundary.
	if s.allowedRoot == "" {
		return "", fmt.Errorf("repo_path was supplied but GRAPHIFY_ALLOWED_ROOT is not configured; refusing to graph an unbounded path")
	}
	// Resolve symlinks on both sides before comparing. filepath.Clean is
	// purely lexical: it collapses "..", but a symlink inside allowedRoot
	// pointing anywhere on the filesystem survives it untouched, and the
	// os.Stat below follows that link happily. Comparing real paths is what
	// actually makes this containment rather than string matching.
	root, err := filepath.EvalSymlinks(s.allowedRoot)
	if err != nil {
		return "", fmt.Errorf("configured allowed root is unavailable")
	}
	clean, err := filepath.EvalSymlinks(filepath.Clean(requested))
	if err != nil {
		return "", fmt.Errorf("repo_path %s is not an accessible directory", filepath.Clean(requested))
	}
	rel, err := filepath.Rel(root, clean)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("repo_path must be inside %s", s.allowedRoot)
	}
	if info, err := os.Stat(clean); err != nil || !info.IsDir() {
		return "", fmt.Errorf("repo_path %s is not an accessible directory", clean)
	}
	return clean, nil
}

func (s *graphifyServer) readinessCheck() error {
	if _, err := exec.LookPath("graphify"); err != nil {
		return err
	}
	if s.memoryIntelligenceURL == "" {
		return errRequired("SCOS_MEMORY_INTELLIGENCE_URL is required")
	}
	return nil
}

type errRequired string

func (e errRequired) Error() string { return string(e) }

func newGraphifyServer() *graphifyServer {
	return &graphifyServer{startTime: time.Now()}
}

func resolveUpstream(envVar, fallback string) string {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv(envVar)), "/")
	if base == "" {
		base = fallback
	}
	return base
}

func main() {
	client, err := tlsserve.TrustedClient(15 * time.Second)
	if err != nil {
		log.Fatalf("configure internal TLS client: %v", err)
	}

	srv := newGraphifyServer()
	srv.httpClient = client
	srv.memoryIntelligenceURL = resolveUpstream("SCOS_MEMORY_INTELLIGENCE_URL", "https://memory-intelligence:8089")
	srv.repoPath = strings.TrimSpace(os.Getenv("GRAPHIFY_REPO_PATH"))
	if srv.repoPath == "" {
		srv.repoPath = "."
	}
	srv.allowedRoot = strings.TrimSpace(os.Getenv("GRAPHIFY_ALLOWED_ROOT"))
	srv.cacheDir = strings.TrimSpace(os.Getenv("GRAPHIFY_CACHE_DIR"))
	if srv.cacheDir == "" {
		srv.cacheDir = "/var/lib/graphify-cache"
	}
	srv.namespaceID = strings.TrimSpace(os.Getenv("GRAPHIFY_NAMESPACE_ID"))
	srv.tenantID = strings.TrimSpace(os.Getenv("GRAPHIFY_TENANT_ID"))
	srv.principalID = strings.TrimSpace(os.Getenv("GRAPHIFY_PRINCIPAL_ID"))
	srv.backend = strings.TrimSpace(os.Getenv("GRAPHIFY_BACKEND"))

	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", healthOK)
	mux.HandleFunc("/health/version", release.Handler("graphify-provider"))
	mux.HandleFunc("/health/ready", readiness.Handler(func() error {
		return srv.readinessCheck()
	}))
	mux.HandleFunc("/v1/graphify/extract", srv.handleExtract)
	mux.HandleFunc("/v1/graphify/query", srv.handleQuery)
	mux.HandleFunc("/v1/graphify/path", srv.handlePath)
	mux.HandleFunc("/v1/graphify/explain", srv.handleExplain)

	addr := os.Getenv("SCOS_GRAPHIFY_PROVIDER_HTTP_ADDR")
	if addr == "" {
		addr = ":8099"
	}
	log.Printf("graphify provider (%s) listening on %s", providerName, addr)
	if err := tlsserve.ListenAndServe(addr, telemetry.Instrument("graphify-provider", auth.AuthenticatedHandler(mux, auth.MustFromEnvironment()))); err != nil {
		log.Fatal(err)
	}
}

func healthOK(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("encode response: %v", err)
	}
}

// ── extraction / ingestion ──────────────────────────────────────────────

func (s *graphifyServer) handleExtract(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.namespaceID == "" || s.tenantID == "" || s.principalID == "" {
		http.Error(w, "GRAPHIFY_NAMESPACE_ID, GRAPHIFY_TENANT_ID and GRAPHIFY_PRINCIPAL_ID must be configured", http.StatusPreconditionFailed)
		return
	}

	graphPath, err := runExtract(r.Context(), s.repoPath, s.backend)
	if err != nil {
		http.Error(w, "graphify extract: "+err.Error(), http.StatusInternalServerError)
		return
	}
	graph, err := loadGraph(graphPath)
	if err != nil {
		http.Error(w, "load graph.json: "+err.Error(), http.StatusInternalServerError)
		return
	}

	summary := ingestSummary{}
	memoryIDByNode := make(map[string]string, len(graph.Nodes))
	for _, node := range graph.Nodes {
		memoryID, err := s.ingestNode(r.Context(), node)
		if err != nil {
			summary.Errors = append(summary.Errors, "node "+node.ID+": "+err.Error())
			continue
		}
		memoryIDByNode[node.ID] = memoryID
		summary.NodesIngested++
	}
	for _, edge := range graph.Edges {
		sourceID, sourceOK := memoryIDByNode[edge.Source]
		targetID, targetOK := memoryIDByNode[edge.Target]
		if !sourceOK || !targetOK {
			summary.Errors = append(summary.Errors, "edge "+edge.Source+"->"+edge.Target+": unmapped node")
			continue
		}
		if err := s.ingestEdge(r.Context(), sourceID, targetID, edge); err != nil {
			summary.Errors = append(summary.Errors, "edge "+edge.Source+"->"+edge.Target+": "+err.Error())
			continue
		}
		summary.EdgesIngested++
	}
	writeJSON(w, summary)
}

type ingestSummary struct {
	NodesIngested int      `json:"nodes_ingested"`
	EdgesIngested int      `json:"edges_ingested"`
	Errors        []string `json:"errors,omitempty"`
}

// relationTypeFor maps graphify's edge confidence tag onto a
// memory_relations relation_type; graphify's own EXTRACTED/INFERRED/
// AMBIGUOUS vocabulary is preserved (lowercased) rather than collapsed,
// since callers of GET /v1/relations may want to distinguish AMBIGUOUS
// edges from INFERRED ones.
func relationTypeFor(confidenceTag string) string {
	switch strings.ToUpper(confidenceTag) {
	case "EXTRACTED":
		return "extracted_from"
	default:
		return "inferred_from"
	}
}

// ingestNode writes a graphify node as a Fact. Nodes are always AST-observed
// (graphify only tags *edges* with EXTRACTED/INFERRED/AMBIGUOUS confidence,
// not nodes -- a node either exists in the parsed source or isn't emitted at
// all), so every node maps to Fact.Source "explicit".
func (s *graphifyServer) ingestNode(ctx context.Context, node graphNode) (string, error) {
	evidence := []string(nil)
	if node.SourceFile != "" {
		ref := node.SourceFile
		if node.SourceLocation != "" {
			ref = ref + ":" + node.SourceLocation
		}
		evidence = []string{ref}
	}
	fact := map[string]any{
		"network":      "entity",
		"content":      node.Label,
		"source":       "explicit",
		"confidence":   1.0,
		"scope":        "namespace",
		"derivation":   "graphify:node:" + node.ID,
		"evidence":     evidence,
		"inferred":     false,
		"namespace_id": s.namespaceID,
		"tenant_id":    s.tenantID,
		"principal_id": s.principalID,
	}
	// memory-intelligence's POST /v1/facts nests the created (or merged)
	// fact under "fact" in both response branches -- see handleFacts.
	var resp struct {
		Fact struct {
			ID string `json:"id"`
		} `json:"fact"`
		CanonicalFactID string `json:"canonical_fact_id,omitempty"`
	}
	if err := s.postJSON(ctx, s.memoryIntelligenceURL+"/v1/facts", fact, &resp); err != nil {
		return "", err
	}
	if resp.CanonicalFactID != "" {
		return resp.CanonicalFactID, nil
	}
	return resp.Fact.ID, nil
}

func (s *graphifyServer) ingestEdge(ctx context.Context, sourceMemoryID, targetMemoryID string, edge graphEdge) error {
	relation := map[string]any{
		"source_memory_id": sourceMemoryID,
		"target_memory_id": targetMemoryID,
		"relation_type":    relationTypeFor(edge.Confidence),
		"tenant_id":        s.tenantID,
		"principal_id":     s.principalID,
	}
	return s.postJSON(ctx, s.memoryIntelligenceURL+"/v1/relations", relation, nil)
}

// ── read-side: query / path / explain ───────────────────────────────────

func (s *graphifyServer) handleQuery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Q        string `json:"q"`
		RepoPath string `json:"repo_path,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Q == "" {
		http.Error(w, "q is required", http.StatusBadRequest)
		return
	}
	if err := rejectFlagLike("q", req.Q); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	workDir, err := s.resolveWorkDir(req.RepoPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	graphPath, err := s.ensureCachedGraph(r.Context(), workDir)
	if err != nil {
		http.Error(w, "graphify extract: "+err.Error(), http.StatusInternalServerError)
		return
	}
	out, err := runGraphify(r.Context(), workDir, "query", req.Q, "--graph", graphPath)
	if err != nil {
		http.Error(w, "graphify query: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"result": out})
}

func (s *graphifyServer) handlePath(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		From     string `json:"from"`
		To       string `json:"to"`
		RepoPath string `json:"repo_path,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.From == "" || req.To == "" {
		http.Error(w, "from and to are required", http.StatusBadRequest)
		return
	}
	if err := rejectFlagLike("from", req.From); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := rejectFlagLike("to", req.To); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	workDir, err := s.resolveWorkDir(req.RepoPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	graphPath, err := s.ensureCachedGraph(r.Context(), workDir)
	if err != nil {
		http.Error(w, "graphify extract: "+err.Error(), http.StatusInternalServerError)
		return
	}
	out, err := runGraphify(r.Context(), workDir, "path", req.From, req.To, "--graph", graphPath)
	if err != nil {
		http.Error(w, "graphify path: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"result": out})
}

func (s *graphifyServer) handleExplain(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Node     string `json:"node"`
		RepoPath string `json:"repo_path,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Node == "" {
		http.Error(w, "node is required", http.StatusBadRequest)
		return
	}
	if err := rejectFlagLike("node", req.Node); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	workDir, err := s.resolveWorkDir(req.RepoPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	graphPath, err := s.ensureCachedGraph(r.Context(), workDir)
	if err != nil {
		http.Error(w, "graphify extract: "+err.Error(), http.StatusInternalServerError)
		return
	}
	out, err := runGraphify(r.Context(), workDir, "explain", req.Node, "--graph", graphPath)
	if err != nil {
		http.Error(w, "graphify explain: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"result": out})
}
