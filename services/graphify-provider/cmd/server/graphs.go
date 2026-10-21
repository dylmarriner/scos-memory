package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

// graphMetaFile sits beside graphify-out/ in each cache entry and records
// what the entry is a graph of. Cache directories are named by a digest, so
// without it the repository a graph belongs to cannot be read back.
const graphMetaFile = "scos-graph.json"

// graphMeta describes one built graph.
type graphMeta struct {
	RepoPath         string    `json:"repo_path"`
	BuiltAt          time.Time `json:"built_at"`
	Nodes            int       `json:"nodes"`
	Edges            int       `json:"edges"`
	ExtractorVersion string    `json:"extractor_version,omitempty"`
	Backend          string    `json:"backend,omitempty"`
}

// writeGraphMeta records dir's graph. dir is the build directory, so the
// record is published with the graph in the same rename.
func writeGraphMeta(dir string, meta graphMeta) (graphMeta, error) {
	graph, err := loadGraph(filepath.Join(dir, "graphify-out", "graph.json"))
	if err != nil {
		return meta, err
	}
	meta.Nodes, meta.Edges = len(graph.Nodes), len(graph.Edges)
	data, err := json.Marshal(meta)
	if err != nil {
		return meta, err
	}
	return meta, os.WriteFile(filepath.Join(dir, graphMetaFile), data, 0o644)
}

// cacheEntrySuffix is the digest cacheKey appends to a repository's name.
var cacheEntrySuffix = regexp.MustCompile(`-[0-9a-f]{16}$`)

// readGraphMeta returns the record of the graph cached in dir. An entry
// built before records were kept gets one now: its counts come from the
// graph itself, its name from the directory, and its repository path stays
// unknown because the directory name carries only a digest of it.
func readGraphMeta(dir string) (graphMeta, string, bool) {
	name := cacheEntrySuffix.ReplaceAllString(filepath.Base(dir), "")
	graphPath := filepath.Join(dir, "graphify-out", "graph.json")
	info, err := os.Stat(graphPath)
	if err != nil {
		return graphMeta{}, "", false
	}
	var meta graphMeta
	if data, err := os.ReadFile(filepath.Join(dir, graphMetaFile)); err == nil && json.Unmarshal(data, &meta) == nil {
		if meta.RepoPath != "" {
			name = filepath.Base(meta.RepoPath)
		}
		return meta, name, true
	}
	meta, err = writeGraphMeta(dir, graphMeta{BuiltAt: info.ModTime().UTC()})
	if err != nil {
		return graphMeta{}, "", false
	}
	return meta, name, true
}

// graphListing is one repository's entry in GET /v1/graphify/graphs.
type graphListing struct {
	Name             string             `json:"name"`
	RepoPath         string             `json:"repo_path,omitempty"`
	Status           string             `json:"status"` // built, building, failed
	Nodes            int                `json:"nodes"`
	Edges            int                `json:"edges"`
	BuiltAt          *time.Time         `json:"built_at,omitempty"`
	Builds           int                `json:"builds"`
	ExtractorVersion string             `json:"extractor_version,omitempty"`
	Error            string             `json:"error,omitempty"`
	Extractions      []extractJobStatus `json:"extractions,omitempty"`
}

// handleGraphs lists every repository with a graph: the newest build of
// each, how many builds are cached for it, any build in progress or failed,
// and the caller's extraction jobs into memory.
func (s *graphifyServer) handleGraphs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	claims, ok := auth.CanonicalClaims(r.Context())
	if !ok {
		http.Error(w, "a claims-bearing identity is required", http.StatusUnauthorized)
		return
	}
	byKey := map[string]*graphListing{}
	key := func(name, repoPath string) string {
		if repoPath != "" {
			return filepath.Clean(repoPath)
		}
		return "name:" + name
	}

	entries, err := os.ReadDir(s.cacheDir)
	if err != nil && !os.IsNotExist(err) {
		http.Error(w, "read graph cache: "+err.Error(), http.StatusInternalServerError)
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		meta, name, ok := readGraphMeta(filepath.Join(s.cacheDir, entry.Name()))
		if !ok {
			continue
		}
		k := key(name, meta.RepoPath)
		listing := byKey[k]
		if listing == nil {
			listing = &graphListing{Name: name, RepoPath: meta.RepoPath, Status: "built"}
			byKey[k] = listing
		}
		listing.Builds++
		if listing.BuiltAt == nil || meta.BuiltAt.After(*listing.BuiltAt) {
			builtAt := meta.BuiltAt
			listing.BuiltAt = &builtAt
			listing.Nodes, listing.Edges = meta.Nodes, meta.Edges
			listing.ExtractorVersion = meta.ExtractorVersion
		}
	}

	s.buildsMu.Lock()
	for _, build := range s.builds {
		if build.workDir == "" {
			continue
		}
		name := filepath.Base(build.workDir)
		k := key(name, build.workDir)
		listing := byKey[k]
		if listing == nil {
			listing = &graphListing{Name: name, RepoPath: build.workDir}
			byKey[k] = listing
		}
		switch {
		case build.finished.IsZero():
			listing.Status = "building"
		case build.err != nil:
			listing.Status = "failed"
			listing.Error = build.err.Error()
		}
	}
	s.buildsMu.Unlock()

	s.jobsMu.Lock()
	for _, job := range s.jobs {
		if job.tenantID != claims.TenantID {
			continue
		}
		status := job.snapshot()
		name := filepath.Base(status.RepoPath)
		k := key(name, status.RepoPath)
		listing := byKey[k]
		if listing == nil {
			listing = &graphListing{Name: name, RepoPath: status.RepoPath, Status: "built"}
			byKey[k] = listing
		}
		listing.Extractions = append(listing.Extractions, status)
	}
	s.jobsMu.Unlock()

	out := make([]graphListing, 0, len(byKey))
	for _, listing := range byKey {
		sort.Slice(listing.Extractions, func(i, j int) bool {
			return listing.Extractions[i].StartedAt.After(listing.Extractions[j].StartedAt)
		})
		out = append(out, *listing)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, out)
}
