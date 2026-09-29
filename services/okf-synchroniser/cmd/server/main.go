package main

import "github.com/dylmarriner/scos-memory/packages/release"

import "github.com/dylmarriner/scos-memory/packages/auth"
import "github.com/dylmarriner/scos-memory/packages/tlsserve"

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dylmarriner/scos-memory/packages/database"
	"github.com/dylmarriner/scos-memory/services/readiness"
	"github.com/jackc/pgx/v5/pgxpool"
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
	proposals   map[string]*OKFProposal
	okfRoot     string
	statePath   string
	db          *pgxpool.Pool
	// roleLookup, when set, replaces the okf_governance_roles query. Only
	// tests set it; with neither it nor db, requireRole fails closed.
	roleLookup func(ctx context.Context, principal, role string) (bool, error)
}

func (s *okfServer) readinessCheck() error {
	if err := readiness.WritableDir(s.okfRoot); err != nil {
		return err
	}
	if err := readiness.WritableDir(filepath.Dir(s.statePath)); err != nil {
		return err
	}
	if s.db == nil {
		return fmt.Errorf("postgres dependency is not configured")
	}
	return s.db.Ping(context.Background())
}

func newOKFServer() *okfServer {
	root := findOKFRoot()
	statePath := os.Getenv("SCOS_OKF_SYNCHRONISER_STATE_PATH")
	if statePath == "" {
		statePath = filepath.Join(filepath.Dir(root), ".okf-projections.json")
	}
	s := &okfServer{
		documents:   make(map[string]*OKFDocument),
		projections: make(map[string]*IndexProjection),
		proposals:   make(map[string]*OKFProposal),
		okfRoot:     root,
		statePath:   statePath,
	}
	_ = s.loadProjections()
	return s
}

// findOKFRoot returns SCOS_OKF_ROOT when set; otherwise it probes paths
// relative to the working directory (repo checkouts and containers).
func findOKFRoot() string {
	if root := strings.TrimSpace(os.Getenv("SCOS_OKF_ROOT")); root != "" {
		return root
	}
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
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		log.Fatal("DATABASE_URL is required")
	}
	if err := database.RequireTLS(databaseURL); err != nil {
		log.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		log.Fatalf("connect to database: %v", err)
	}
	config.ConnConfig.RuntimeParams["scos.service_id"] = "okf-synchroniser"
	// OKF is platform-scoped; the service identity is the authenticated
	// principal for its RLS-gated governance projections.
	config.ConnConfig.RuntimeParams["scos.principal_id"] = "okf-synchroniser"
	db, err := pgxpool.NewWithConfig(context.Background(), database.TunePoolConfig(config))
	if err != nil {
		log.Fatalf("connect to database: %v", err)
	}
	defer db.Close()
	srv.db = db
	created, err := srv.bootstrapRepository(context.Background(), strings.TrimSpace(os.Getenv("SCOS_OKF_SEED_DIR")), strings.TrimSpace(os.Getenv("SCOS_OKF_PROTECTED_BRANCH")))
	if err != nil {
		log.Fatalf("OKF repository: %v", err)
	}
	if created {
		log.Printf("created OKF repository at %s with a baseline commit", srv.okfRoot)
	}
	if err := srv.loadConceptsFromDB(context.Background()); err != nil {
		log.Fatalf("load okf concepts: %v", err)
	}
	if err := srv.loadProposalsFromDB(context.Background()); err != nil {
		log.Fatalf("load okf proposals: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", healthOK)
	mux.HandleFunc("/health/version", release.Handler("okf-synchroniser"))
	mux.HandleFunc("/health/ready", readiness.Handler(func() error {
		return srv.readinessCheck()
	}))

	mux.HandleFunc("/v1/sync", srv.handleSync)
	mux.HandleFunc("/v1/documents", srv.handleDocuments)
	mux.HandleFunc("/v1/okf", srv.handleDocuments)
	mux.HandleFunc("/v1/documents/", srv.handleDocumentByID)
	mux.HandleFunc("/v1/projections", srv.handleProjections)
	mux.HandleFunc("/v1/divergence", srv.handleDivergence)
	mux.HandleFunc("/v1/proposals", srv.handleProposals)
	mux.HandleFunc("/v1/proposals/", srv.handleProposalAction)

	addr := os.Getenv("SCOS_OKF_SYNCHRONISER_HTTP_ADDR")
	if addr == "" {
		addr = ":8090"
	}
	log.Printf("okf synchroniser listening on %s (root: %s)", addr, srv.okfRoot)
	if err := tlsserve.ListenAndServe(addr, auth.AuthenticatedHandler(mux, auth.MustFromEnvironment())); err != nil {
		log.Fatal(err)
	}
}

func (s *okfServer) loadProposalsFromDB(ctx context.Context) error {
	if s.db == nil {
		return nil
	}
	rows, err := s.db.Query(ctx, `SELECT id,path,base_commit,base_hash,content,diff,status,proposer,COALESCE(reviewer,''),COALESCE(review_role,''),COALESCE(decision,''),reason,created_at,COALESCE(reviewed_at,'epoch'),COALESCE(merged_commit,''),COALESCE(indexed_at,'epoch'),diverged FROM okf_proposals`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var p OKFProposal
		var created, reviewed, indexed time.Time
		if err := rows.Scan(&p.ID, &p.Path, &p.BaseCommit, &p.BaseHash, &p.Content, &p.Diff, &p.Status, &p.Proposer, &p.Reviewer, &p.ReviewRole, &p.Decision, &p.Reason, &created, &reviewed, &p.MergedCommit, &indexed, &p.Diverged); err != nil {
			return err
		}
		p.CreatedAt = created.UTC().Format(time.RFC3339)
		if reviewed.Unix() > 0 {
			p.ReviewedAt = reviewed.UTC().Format(time.RFC3339)
		}
		if indexed.Unix() > 0 {
			p.IndexedAt = indexed.UTC().Format(time.RFC3339)
		}
		s.proposals[p.ID] = &p
	}
	return rows.Err()
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

	result := s.synchronise(r.Context())
	writeJSON(w, result)
}

func (s *okfServer) synchronise(ctx context.Context) SyncResult {
	result := SyncResult{}

	// Walk the OKF directory
	okfFiles := s.walkOKFDir()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.statePath == "" {
		s.statePath = filepath.Join(filepath.Dir(s.okfRoot), ".okf-projections.json")
	}
	commitHash := gitCommit(s.okfRoot)
	// §26: Git is authoritative for approved content. Outside a repository
	// nothing is authoritative, which is reported on every sync rather than
	// indexed as if it were.
	gitBacked := commitHash != ""
	if !gitBacked {
		result.Errors = append(result.Errors, "OKF root is not a git repository; content is indexed without Git authority")
	}

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
		fileCommit := commitHash
		if gitBacked {
			// Uncommitted edits are not approved content: keep what was
			// last imported and surface the file for review instead.
			if status, err := s.git(ctx, "status", "--porcelain", "--", file); err != nil {
				result.Errors = append(result.Errors, fmt.Sprintf("git status %s: %v", file, err))
				continue
			} else if status != "" {
				if doc := s.documentForPath(file); doc != nil {
					doc.Status = "diverged"
				}
				if proj := s.projections[file]; proj != nil {
					proj.ImportStatus = "diverged"
				}
				result.Diverged = appendUnique(result.Diverged, file)
				continue
			}
			// The commit that last touched this file, not merely HEAD.
			if last, err := s.git(ctx, "log", "-1", "--format=%H", "--", file); err == nil && last != "" {
				fileCommit = last
			}
		}
		metadata := extractMetadata(file, content)

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
				CommitID:    fileCommit,
				Status:      "current",
				ImportedAt:  time.Now().UTC().Format(time.RFC3339),
				UpdatedAt:   time.Now().UTC().Format(time.RFC3339),
				Content:     string(content),
				Metadata:    metadata,
			}
			s.documents[id] = doc
			s.projections[file] = &IndexProjection{
				CommitHash:   fileCommit,
				Path:         file,
				ContentHash:  contentHash,
				ImportStatus: "synced",
			}
			result.Added = append(result.Added, file)
		} else if doc := s.documentForPath(file); doc != nil && doc.ContentHash != existing.ContentHash {
			// The indexed document changed independently of its last imported projection.
			// Keep both versions and require an explicit review.
			doc.Status = "diverged"
			existing.ImportStatus = "diverged"
			result.Diverged = appendUnique(result.Diverged, file)
		} else if existing.ContentHash != contentHash {
			// Changed - update
			doc := s.documentForPath(file)
			if doc == nil {
				id := fmt.Sprintf("okf-%d", len(s.documents)+1)
				doc = &OKFDocument{ID: id, Path: file, Category: category, ImportedAt: time.Now().UTC().Format(time.RFC3339)}
				s.documents[id] = doc
			}
			doc.ContentHash = contentHash
			doc.CommitID = fileCommit
			doc.Content = string(content)
			doc.Title = extractTitle(content)
			doc.Metadata = metadata
			doc.Status = "changed"
			doc.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
			existing.ContentHash = contentHash
			existing.CommitHash = fileCommit
			existing.ImportStatus = "synced"
			result.Updated = append(result.Updated, file)
		} else {
			existing.CommitHash = fileCommit
			existing.ImportStatus = "synced"
			doc := s.documentForPath(file)
			if doc == nil {
				id := fmt.Sprintf("okf-%d", len(s.documents)+1)
				doc = &OKFDocument{ID: id, Path: file, Category: category, Title: extractTitle(content), ContentHash: contentHash, CommitID: fileCommit, Status: "current", ImportedAt: time.Now().UTC().Format(time.RFC3339), UpdatedAt: time.Now().UTC().Format(time.RFC3339), Content: string(content), Metadata: metadata}
				s.documents[id] = doc
			} else {
				doc.CommitID = fileCommit
				doc.Status = "current"
			}
		}
	}

	// Detect removed files
	for path := range s.projections {
		if !seen[path] {
			// A missing repository file is a destructive change. Preserve the
			// indexed record until an explicit governance action resolves it.
			if doc := s.documentForPath(path); doc != nil {
				doc.Status = "diverged"
			}
			s.projections[path].ImportStatus = "diverged"
			result.Removed = appendUnique(result.Removed, path)
			result.Diverged = appendUnique(result.Diverged, path)
		}
	}

	// Detect divergence between DB and filesystem
	for _, doc := range s.documents {
		proj, ok := s.projections[doc.Path]
		if ok && doc.ContentHash != proj.ContentHash {
			doc.Status = "diverged"
			proj.ImportStatus = "diverged"
			result.Diverged = appendUnique(result.Diverged, doc.Path)
		}
	}
	if err := s.persistProjections(); err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("persist projections: %v", err))
	}
	if err := s.persistConceptsToDB(ctx); err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("persist okf concepts: %v", err))
	}
	if err := s.recordBundle(ctx, commitHash, result); err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("record okf bundle: %v", err))
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

func (s *okfServer) documentForPath(path string) *OKFDocument {
	for _, doc := range s.documents {
		if doc.Path == path {
			return doc
		}
	}
	return nil
}

// loadConceptsFromDB hydrates in-memory documents and projections from the
// PostgreSQL-indexed okf_concepts table at startup. It must be called before
// the server begins accepting requests.
func (s *okfServer) loadConceptsFromDB(ctx context.Context) error {
	if s.db == nil {
		return nil
	}
	rows, err := s.db.Query(ctx, `SELECT id, path, category, title, content, content_hash, commit_hash, import_status, status, imported_at, updated_at, metadata FROM okf_concepts`)
	if err != nil {
		return err
	}
	defer rows.Close()
	documents := make(map[string]*OKFDocument)
	projections := make(map[string]*IndexProjection)
	for rows.Next() {
		var (
			doc          OKFDocument
			commitHash   string
			importStatus string
			importedAt   time.Time
			updatedAt    time.Time
			metadataRaw  []byte
		)
		if err := rows.Scan(&doc.ID, &doc.Path, &doc.Category, &doc.Title, &doc.Content, &doc.ContentHash, &commitHash, &importStatus, &doc.Status, &importedAt, &updatedAt, &metadataRaw); err != nil {
			return err
		}
		if err := json.Unmarshal(metadataRaw, &doc.Metadata); err != nil {
			return fmt.Errorf("decode okf_concepts.metadata for %s: %w", doc.Path, err)
		}
		doc.CommitID = commitHash
		doc.ImportedAt = importedAt.UTC().Format(time.RFC3339)
		doc.UpdatedAt = updatedAt.UTC().Format(time.RFC3339)
		documents[doc.ID] = &doc
		projections[doc.Path] = &IndexProjection{CommitHash: commitHash, Path: doc.Path, ContentHash: doc.ContentHash, ImportStatus: importStatus}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	if len(documents) > 0 {
		s.documents = documents
		s.projections = projections
	}
	s.mu.Unlock()
	return nil
}

// persistConceptsToDB writes the current in-memory documents/projections to
// the indexed okf_concepts table. Callers must already hold s.mu.
func (s *okfServer) persistConceptsToDB(ctx context.Context) error {
	if s.db == nil {
		return nil
	}
	for _, doc := range s.documents {
		proj := s.projections[doc.Path]
		importStatus := "synced"
		if proj != nil {
			importStatus = proj.ImportStatus
		}
		metadata, err := json.Marshal(doc.Metadata)
		if err != nil {
			return err
		}
		if doc.Metadata == nil {
			metadata = []byte("{}")
		}
		if _, err := s.db.Exec(ctx, `INSERT INTO okf_concepts (id, path, category, title, content, content_hash, commit_hash, import_status, status, imported_at, updated_at, metadata)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,now(),now(),$10)
			ON CONFLICT (path) DO UPDATE SET
				category=EXCLUDED.category,
				title=EXCLUDED.title,
				content=EXCLUDED.content,
				content_hash=EXCLUDED.content_hash,
				commit_hash=EXCLUDED.commit_hash,
				import_status=EXCLUDED.import_status,
				status=EXCLUDED.status,
				metadata=EXCLUDED.metadata,
				updated_at=now()`,
			doc.ID, doc.Path, doc.Category, doc.Title, doc.Content, doc.ContentHash, doc.CommitID, importStatus, doc.Status, metadata); err != nil {
			return err
		}
	}
	return nil
}

// recordBundle appends an audit record of this sync run to okf_bundles.
// Callers must already hold s.mu.
func (s *okfServer) recordBundle(ctx context.Context, commitHash string, result SyncResult) error {
	if s.db == nil {
		return nil
	}
	added, err := json.Marshal(result.Added)
	if err != nil {
		return err
	}
	updated, err := json.Marshal(result.Updated)
	if err != nil {
		return err
	}
	removed, err := json.Marshal(result.Removed)
	if err != nil {
		return err
	}
	diverged, err := json.Marshal(result.Diverged)
	if err != nil {
		return err
	}
	errorsJSON, err := json.Marshal(result.Errors)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `INSERT INTO okf_bundles (commit_hash, added_count, updated_count, removed_count, diverged_count, added_paths, updated_paths, removed_paths, diverged_paths, errors)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		commitHash, len(result.Added), len(result.Updated), len(result.Removed), len(result.Diverged), added, updated, removed, diverged, errorsJSON)
	return err
}

func gitCommit(root string) string {
	cmd := exec.Command("git", "-C", root, "rev-parse", "--verify", "HEAD")
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

func (s *okfServer) loadProjections() error {
	data, err := os.ReadFile(s.statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var projections map[string]*IndexProjection
	if err := json.Unmarshal(data, &projections); err != nil {
		return err
	}
	for path, projection := range projections {
		if projection != nil && projection.Path == "" {
			projection.Path = path
		}
	}
	s.projections = projections
	return nil
}

func (s *okfServer) persistProjections() error {
	data, err := json.MarshalIndent(s.projections, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(s.statePath); dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	tmp := s.statePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.statePath); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

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

// extractMetadata reads flat "key: value" metadata: a markdown document's
// leading front-matter block, or a YAML document's top-level scalars.
func extractMetadata(path string, content []byte) map[string]string {
	text := strings.ReplaceAll(string(content), "\r\n", "\n")
	var block string
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md":
		if !strings.HasPrefix(text, "---\n") {
			return nil
		}
		end := strings.Index(text[4:], "\n---")
		if end < 0 {
			return nil
		}
		block = text[4 : 4+end]
	case ".yaml", ".yml":
		block = text
	default:
		return nil
	}
	meta := map[string]string{}
	for _, line := range strings.Split(block, "\n") {
		if line == "" || line[0] == ' ' || line[0] == '\t' || line[0] == '#' || line[0] == '-' {
			continue // nested, comment or list line: not a top-level scalar
		}
		key, value, ok := strings.Cut(line, ":")
		key, value = strings.TrimSpace(key), strings.Trim(strings.TrimSpace(value), `"'`)
		if !ok || key == "" || value == "" || value == "|" || value == ">" {
			continue
		}
		meta[key] = value
	}
	if len(meta) == 0 {
		return nil
	}
	return meta
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
