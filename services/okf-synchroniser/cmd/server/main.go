package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const version = "0.1.0"

// ── types ────────────────────────────────────────────────────────────────

type OKFDocument struct {
	ID          string            `json:"id"`
	Path        string            `json:"path"`
	Category    string            `json:"category"` // governance, architecture, projects, infrastructure, procedures, tools, decisions, glossary
	Title       string            `json:"title"`
	ContentHash string            `json:"content_hash"`
	CommitID    string            `json:"commit_id"`
	Status      string            `json:"status"` // current, changed, diverged, synced
	ImportedAt  string            `json:"imported_at"`
	UpdatedAt   string            `json:"updated_at"`
	Content     string            `json:"content,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

type IndexProjection struct {
	CommitHash   string `json:"commit_hash"`
	Path         string `json:"path"`
	ContentHash  string `json:"content_hash"`
	ImportStatus string `json:"import_status"`
}

type SyncResult struct {
	Added    []string `json:"added"`
	Updated  []string `json:"updated"`
	Removed  []string `json:"removed"`
	Diverged []string `json:"diverged"`
	Errors   []string `json:"errors"`
}

// ── server state ─────────────────────────────────────────────────────────

type okfServer struct {
	mu          sync.RWMutex
	documents   map[string]*OKFDocument
	projections map[string]*IndexProjection // path -> projection
	okfRoot     string
}

func newOKFServer() *okfServer {
	return &okfServer{
		documents:   make(map[string]*OKFDocument),
		projections: make(map[string]*IndexProjection),
		okfRoot:     findOKFRoot(),
	}
}

func findOKFRoot() string {
	candidates := []string{"okf", "docs", ".okf"}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && info.IsDir() {
			return c
		}
	}
	return "okf"
}

// ── main ─────────────────────────────────────────────────────────────────

func main() {
	srv := newOKFServer()
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", healthOK)
	mux.HandleFunc("/health/ready", healthOK)

	mux.HandleFunc("/v1/sync", srv.handleSync)
	mux.HandleFunc("/v1/documents", srv.handleDocuments)
	mux.HandleFunc("/v1/documents/", srv.handleDocumentByID)
	mux.HandleFunc("/v1/projections", srv.handleProjections)
	mux.HandleFunc("/v1/divergence", srv.handleDivergence)

	addr := os.Getenv("SCOS_OKF_SYNCHRONISER_HTTP_ADDR")
	if addr == "" {
		addr = ":8090"
	}
	log.Printf("okf synchroniser listening on %s (root: %s)", addr, srv.okfRoot)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

func healthOK(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// ── sync endpoint ────────────────────────────────────────────────────────

func (s *okfServer) handleSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	result := s.synchronise()
	writeJSON(w, result)
}

func (s *okfServer) synchronise() SyncResult {
	result := SyncResult{}

	// Walk the OKF directory
	okfFiles := s.walkOKFDir()

	s.mu.Lock()
	defer s.mu.Unlock()

	// Track seen paths
	seen := make(map[string]bool)

	for _, file := range okfFiles {
		seen[file] = true

		// Read and hash content
		content, err := os.ReadFile(file)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("read %s: %v", file, err))
			continue
		}

		contentHash := sha256Hex(content)
		category := classifyPath(file)

		existing, exists := s.projections[file]
		if !exists {
			// New document
			id := fmt.Sprintf("okf-%d", len(s.documents)+1)
			doc := &OKFDocument{
				ID:          id,
				Path:        file,
				Category:    category,
				Title:       extractTitle(content),
				ContentHash: contentHash,
				Status:      "current",
				ImportedAt:  time.Now().UTC().Format(time.RFC3339),
				UpdatedAt:   time.Now().UTC().Format(time.RFC3339),
				Content:     string(content),
			}
			s.documents[id] = doc
			s.projections[file] = &IndexProjection{
				Path:         file,
				ContentHash:  contentHash,
				ImportStatus: "synced",
			}
			result.Added = append(result.Added, file)
		} else if existing.ContentHash != contentHash {
			// Changed - update
			for _, doc := range s.documents {
				if doc.Path == file {
					doc.ContentHash = contentHash
					doc.Content = string(content)
					doc.Status = "changed"
					doc.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
					break
				}
			}
			existing.ContentHash = contentHash
			existing.ImportStatus = "synced"
			result.Updated = append(result.Updated, file)
		}
	}

	// Detect removed files
	for path := range s.projections {
		if !seen[path] {
			result.Removed = append(result.Removed, path)
			for id, doc := range s.documents {
				if doc.Path == path {
					delete(s.documents, id)
					break
				}
			}
			delete(s.projections, path)
		}
	}

	// Detect divergence between DB and filesystem
	for _, doc := range s.documents {
		proj, ok := s.projections[doc.Path]
		if ok && doc.ContentHash != proj.ContentHash {
			doc.Status = "diverged"
			result.Diverged = append(result.Diverged, doc.Path)
		}
	}

	return result
}

// ── document endpoints ───────────────────────────────────────────────────

func (s *okfServer) handleDocuments(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	var docs []*OKFDocument
	for _, d := range s.documents {
		docs = append(docs, d)
	}
	s.mu.RUnlock()
	sort.Slice(docs, func(i, j int) bool {
		return docs[i].Path < docs[j].Path
	})
	writeJSON(w, docs)
}

func (s *okfServer) handleDocumentByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/documents/")
	s.mu.RLock()
	doc, ok := s.documents[id]
	s.mu.RUnlock()
	if !ok {
		http.Error(w, "document not found", http.StatusNotFound)
		return
	}
	writeJSON(w, doc)
}

// ── projections endpoint ─────────────────────────────────────────────────

func (s *okfServer) handleProjections(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	var projs []*IndexProjection
	for _, p := range s.projections {
		projs = append(projs, p)
	}
	s.mu.RUnlock()
	writeJSON(w, projs)
}

// ── divergence endpoint ──────────────────────────────────────────────────

func (s *okfServer) handleDivergence(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	var diverged []*OKFDocument
	for _, d := range s.documents {
		if d.Status == "diverged" {
			diverged = append(diverged, d)
		}
	}
	s.mu.RUnlock()
	if diverged == nil {
		diverged = []*OKFDocument{}
	}
	writeJSON(w, diverged)
}

// ── helpers ──────────────────────────────────────────────────────────────

func (s *okfServer) walkOKFDir() []string {
	var files []string
	_ = filepath.WalkDir(s.okfRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			// Skip hidden dirs
			if strings.HasPrefix(d.Name(), ".") && d.Name() != "." {
				return filepath.SkipDir
			}
			return nil
		}
		// Only markdown and yaml files
		ext := strings.ToLower(filepath.Ext(path))
		if ext == ".md" || ext == ".yaml" || ext == ".yml" {
			files = append(files, path)
		}
		return nil
	})
	return files
}

func classifyPath(path string) string {
	path = strings.ToLower(path)
	switch {
	case strings.Contains(path, "/governance/"):
		return "governance"
	case strings.Contains(path, "/architecture/"):
		return "architecture"
	case strings.Contains(path, "/projects/"):
		return "projects"
	case strings.Contains(path, "/infrastructure/"):
		return "infrastructure"
	case strings.Contains(path, "/procedures/"):
		return "procedures"
	case strings.Contains(path, "/tools/"):
		return "tools"
	case strings.Contains(path, "/decisions/"):
		return "decisions"
	case strings.Contains(path, "/glossary/"):
		return "glossary"
	default:
		return "general"
	}
}

func extractTitle(content []byte) string {
	lines := strings.SplitN(string(content), "\n", 3)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "# ") {
			return strings.TrimPrefix(line, "# ")
		}
	}
	return "Untitled"
}

func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("encode response: %v", err)
	}
}
