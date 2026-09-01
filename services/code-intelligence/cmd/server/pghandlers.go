package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// v2 handlers expose pgCodeStore over HTTP. They are additive: every
// existing /v1/* route keeps working unchanged against the in-memory
// codeStore, and these only activate when SCOS_DATABASE_URL /
// DATABASE_URL is configured (srv.pgStore is nil otherwise, and every
// handler here checks for that before touching it). This is deliberate --
// see pgstore.go's doc comment for why the /v1 wire protocol (bare path
// strings, no content hash) can't drive rename detection, and why
// switching the default store for existing callers is a separate,
// coordinated change rather than something to do silently here.

func (s *server) requirePGStore(w http.ResponseWriter) bool {
	if s.pgStore == nil {
		http.Error(w, "the durable code store is not configured on this instance (SCOS_DATABASE_URL is unset)", http.StatusServiceUnavailable)
		return false
	}
	return true
}

func (s *server) handlePGRepos(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.requirePGStore(w) {
		return
	}
	tenant, ok := requireTenant(r)
	if !ok {
		http.Error(w, "authenticated tenant header is required", http.StatusUnauthorized)
		return
	}
	var req struct {
		CanonicalPath string `json:"canonical_path"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.CanonicalPath) == "" {
		http.Error(w, "canonical_path is required", http.StatusBadRequest)
		return
	}
	repositoryID, err := s.pgStore.EnsureRepository(r.Context(), tenant, req.CanonicalPath)
	if err != nil {
		http.Error(w, "ensure repository: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"repository_id": repositoryID})
}

type pgIndexFileRequest struct {
	Path        string `json:"path"`
	Language    string `json:"language,omitempty"`
	ContentHash string `json:"content_hash"`
}

func (s *server) handlePGIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.requirePGStore(w) {
		return
	}
	tenant, ok := requireTenant(r)
	if !ok {
		http.Error(w, "authenticated tenant header is required", http.StatusUnauthorized)
		return
	}
	var req struct {
		RepositoryID      string               `json:"repository_id"`
		Branch            string               `json:"branch"`
		CommitID          string               `json:"commit_id"`
		ExtractorVersion  string               `json:"extractor_version,omitempty"`
		ConfigurationHash string               `json:"configuration_hash,omitempty"`
		Files             []pgIndexFileRequest `json:"files"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.RepositoryID == "" || req.Branch == "" || req.CommitID == "" || len(req.Files) == 0 {
		http.Error(w, "repository_id, branch, commit_id and files are required", http.StatusBadRequest)
		return
	}
	for _, f := range req.Files {
		if strings.TrimSpace(f.Path) == "" || strings.TrimSpace(f.ContentHash) == "" {
			http.Error(w, "every file requires a path and a content_hash", http.StatusBadRequest)
			return
		}
	}
	if req.ExtractorVersion == "" {
		req.ExtractorVersion = "heuristic-v1"
	}

	// Ownership check mirrors handleIndex's: the repository must belong to
	// the authenticated tenant. EnsureRepository is keyed on (tenant_id,
	// canonical_path), not on repository_id, so a repository_id supplied by
	// one tenant that happens to belong to another is rejected here rather
	// than silently indexing into it.
	var owningTenant string
	if err := s.pgStore.db.QueryRowContext(r.Context(),
		`SELECT tenant_id::text FROM code_repositories WHERE id = $1`, req.RepositoryID).Scan(&owningTenant); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "repository not found", http.StatusNotFound)
			return
		}
		http.Error(w, "load repository: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if owningTenant != tenant {
		http.Error(w, "repository not found", http.StatusNotFound)
		return
	}

	files := make([]PGFileInput, 0, len(req.Files))
	for _, f := range req.Files {
		files = append(files, PGFileInput{Path: f.Path, Language: f.Language, ContentHash: f.ContentHash})
	}

	snapshotID, err := s.pgStore.CreateSnapshot(r.Context(), req.RepositoryID, tenant, req.Branch, req.CommitID, req.ExtractorVersion, req.ConfigurationHash)
	if err != nil {
		http.Error(w, "create snapshot: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.pgStore.IndexSnapshot(r.Context(), snapshotID, req.RepositoryID, files); err != nil {
		http.Error(w, "index snapshot: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"snapshot_id": snapshotID,
		"file_count":  len(files),
	})
}

func (s *server) handlePGQuery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.requirePGStore(w) {
		return
	}
	tenant, ok := requireTenant(r)
	if !ok {
		http.Error(w, "authenticated tenant header is required", http.StatusUnauthorized)
		return
	}
	var req struct {
		RepositoryID string `json:"repository_id"`
		Branch       string `json:"branch"`
		Query        string `json:"query"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.RepositoryID == "" || req.Branch == "" || req.Query == "" {
		http.Error(w, "repository_id, branch and query are required", http.StatusBadRequest)
		return
	}

	// The tenant scope always comes from the authenticated caller, never a
	// client-supplied field -- same rule as handleQuery (§19.10) -- so a
	// query can never be widened to another tenant's repositories even if
	// repository_id happens to collide.
	var owningTenant string
	if err := s.pgStore.db.QueryRowContext(r.Context(),
		`SELECT tenant_id::text FROM code_repositories WHERE id = $1`, req.RepositoryID).Scan(&owningTenant); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, map[string]any{"results": []PGSymbolVersion{}})
			return
		}
		http.Error(w, "load repository: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if owningTenant != tenant {
		writeJSON(w, map[string]any{"results": []PGSymbolVersion{}})
		return
	}

	results, err := s.pgStore.QueryByPathOrFQN(r.Context(), req.RepositoryID, req.Branch, req.Query)
	if err != nil {
		http.Error(w, "query: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Lexical fallback only, never a blend: an exact path/FQN match is
	// unambiguous, so a caller who typed the identifier they meant should
	// never see it buried under ranked full-text guesses.
	if len(results) == 0 {
		results, err = s.pgStore.QueryLexical(r.Context(), req.RepositoryID, req.Branch, req.Query, 20)
		if err != nil {
			http.Error(w, "lexical query: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if results == nil {
		results = []PGSymbolVersion{}
	}
	writeJSON(w, map[string]any{"results": results})
}
