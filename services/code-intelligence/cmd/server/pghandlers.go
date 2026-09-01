package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// v2 handlers expose pgCodeStore over HTTP, additive alongside every
// existing /v1/* route, which keeps working unchanged against the
// in-memory codeStore -- see pgstore.go's doc comment for why the /v1 wire
// protocol (bare path strings, no content hash) can't drive rename
// detection, and why migrating existing /v1 callers onto the durable store
// is a separate, coordinated change rather than something to do here.
//
// The durable store itself always runs (DATABASE_URL is required at
// startup, main.go), but an individual user can opt out of these routes
// from the dashboard settings tab (handlePGSettings, SetUserEnabled in
// pgstore.go) -- requireEnabled below is what every mutating/reading v2
// route checks before touching the store.

func (s *server) requirePGStore(w http.ResponseWriter) bool {
	if s.pgStore == nil {
		http.Error(w, "the durable code store is not configured on this instance (DATABASE_URL is unset)", http.StatusServiceUnavailable)
		return false
	}
	return true
}

// requireEnabled checks the *caller's own* opt-out preference, resolved the
// same way requireTenant/requirePrincipal resolve identity elsewhere in
// this service -- never a client-supplied tenant/principal, so a caller
// can only ever read or act on their own preference. Writes a response and
// returns false when the caller has disabled the durable store for
// themselves.
func (s *server) requireEnabled(w http.ResponseWriter, r *http.Request, tenant, principal string) bool {
	enabled, err := s.pgStore.IsUserEnabled(r.Context(), tenant, principal)
	if err != nil {
		http.Error(w, "load user settings: "+err.Error(), http.StatusInternalServerError)
		return false
	}
	if !enabled {
		http.Error(w, "the durable code store is disabled for this user (see dashboard settings)", http.StatusForbidden)
		return false
	}
	return true
}

// handlePGSettings is GET/PUT for the caller's own durable-store
// preference: GET returns the current value (true when no preference has
// ever been recorded), PUT records an explicit one. This is what the
// dashboard settings tab's toggle calls (through cloud-control-plane's
// proxy, matching how the dashboard reaches every other backend service).
func (s *server) handlePGSettings(w http.ResponseWriter, r *http.Request) {
	if !s.requirePGStore(w) {
		return
	}
	tenant, ok := requireTenant(r)
	if !ok {
		http.Error(w, "authenticated tenant header is required", http.StatusUnauthorized)
		return
	}
	principal, ok := requirePrincipal(r)
	if !ok {
		http.Error(w, "authenticated principal header is required", http.StatusUnauthorized)
		return
	}

	switch r.Method {
	case http.MethodGet:
		enabled, err := s.pgStore.IsUserEnabled(r.Context(), tenant, principal)
		if err != nil {
			http.Error(w, "load user settings: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"durable_store_enabled": enabled})
	case http.MethodPut:
		var req struct {
			DurableStoreEnabled *bool `json:"durable_store_enabled"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
			return
		}
		if req.DurableStoreEnabled == nil {
			http.Error(w, "durable_store_enabled is required", http.StatusBadRequest)
			return
		}
		if err := s.pgStore.SetUserEnabled(r.Context(), tenant, principal, *req.DurableStoreEnabled); err != nil {
			http.Error(w, "set user settings: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"durable_store_enabled": *req.DurableStoreEnabled})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
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
	principal, ok := requirePrincipal(r)
	if !ok {
		http.Error(w, "authenticated principal header is required", http.StatusUnauthorized)
		return
	}
	if !s.requireEnabled(w, r, tenant, principal) {
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
	// Content is optional; see PGFileInput's doc comment (pgstore.go) for
	// what supplying it actually buys a caller today (real Go declaration
	// extraction) versus every other language, which still only gets the
	// Tier 0 file-level symbol regardless of whether Content is sent.
	Content string `json:"content,omitempty"`
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
	principal, ok := requirePrincipal(r)
	if !ok {
		http.Error(w, "authenticated principal header is required", http.StatusUnauthorized)
		return
	}
	if !s.requireEnabled(w, r, tenant, principal) {
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
		files = append(files, PGFileInput{Path: f.Path, Language: f.Language, ContentHash: f.ContentHash, Content: f.Content})
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
	// The diff against whatever was previously active for this branch is
	// returned inline rather than requiring a second round-trip: it's the
	// cheapest possible way for a caller to confirm an incremental index
	// actually stayed incremental (blueprint §35's completion gate -- "a
	// one-line change in one file must not trigger a full repository
	// re-index" -- is directly checkable from this response: Modified
	// should have one entry, not len(files)).
	diff, err := s.pgStore.DiffSnapshot(r.Context(), snapshotID)
	if err != nil {
		http.Error(w, "diff snapshot: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"snapshot_id": snapshotID,
		"file_count":  len(files),
		"diff":        diff,
	})
}

func (s *server) handlePGSnapshotDiff(w http.ResponseWriter, r *http.Request) {
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
	principal, ok := requirePrincipal(r)
	if !ok {
		http.Error(w, "authenticated principal header is required", http.StatusUnauthorized)
		return
	}
	if !s.requireEnabled(w, r, tenant, principal) {
		return
	}
	var req struct {
		SnapshotID string `json:"snapshot_id"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.SnapshotID == "" {
		http.Error(w, "snapshot_id is required", http.StatusBadRequest)
		return
	}

	var owningTenant string
	if err := s.pgStore.db.QueryRowContext(r.Context(), `
		SELECT repo.tenant_id::text
		FROM code_repository_snapshots snap
		JOIN code_repositories repo ON repo.id = snap.repository_id
		WHERE snap.id = $1`, req.SnapshotID).Scan(&owningTenant); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "snapshot not found", http.StatusNotFound)
			return
		}
		http.Error(w, "load snapshot: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if owningTenant != tenant {
		http.Error(w, "snapshot not found", http.StatusNotFound)
		return
	}

	diff, err := s.pgStore.DiffSnapshot(r.Context(), req.SnapshotID)
	if err != nil {
		http.Error(w, "diff snapshot: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, diff)
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
	principal, ok := requirePrincipal(r)
	if !ok {
		http.Error(w, "authenticated principal header is required", http.StatusUnauthorized)
		return
	}
	if !s.requireEnabled(w, r, tenant, principal) {
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
