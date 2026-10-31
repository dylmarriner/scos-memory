package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// declarationName matches a declaration name, bare ("SignClaims") or
// qualified by its enclosing type or module ("OutputReducer.reduce_output").
var snapshotIDUUID = regexp.MustCompile("^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$")

var declarationName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)*$`)

// declarationKeyword is a leading language keyword a query may name a
// declaration with ("class OutputReducer", "func SignClaims").
var declarationKeyword = regexp.MustCompile(`^(?:class|def|async def|func|function|fn|type|struct|interface|trait|enum)\s+`)

// declarationPatterns are the qualified-name suffixes a declaration name
// is tried against, most literal first: the name itself, then, for a Go
// method written "Type.Method", the receiver forms Go's indexer records
// ("pkg.(*Type).Method", "pkg.(Type).Method").
func declarationPatterns(name string) []string {
	patterns := []string{"%." + name}
	if typeName, method, ok := strings.Cut(name, "."); ok && !strings.Contains(method, ".") {
		patterns = append(patterns, "%.(*"+typeName+")."+method, "%.("+typeName+")."+method)
	}
	return patterns
}

// declarationQuery returns the declaration name a query anchors on, with
// any leading declaration keyword removed, or "" when it names none.
func declarationQuery(query string) string {
	name := strings.TrimSpace(declarationKeyword.ReplaceAllString(strings.TrimSpace(query), ""))
	name = strings.TrimSuffix(name, "()")
	if !declarationName.MatchString(name) {
		return ""
	}
	return name
}

// Code query and impact bodies are small. A bounded read deadline prevents a
// client from sending a complete JSON value and then withholding the EOF needed
// to prove that no second value follows. This deadline covers BOTH decodes.
const pgJSONReadTimeout = 15 * time.Second

func decodeSinglePGJSON(w http.ResponseWriter, r *http.Request, dst any, timeout time.Duration) error {
	controller := http.NewResponseController(w)
	if err := controller.SetReadDeadline(time.Now().Add(timeout)); err == nil {
		// The complete body has been consumed before a successful return.
		// Reset the connection deadline so subsequent keep-alive requests work.
		defer controller.SetReadDeadline(time.Time{})
	} else if !errors.Is(err, http.ErrNotSupported) {
		return fmt.Errorf("set request read deadline: %w", err)
	}
	// httptest.ResponseRecorder does not support socket deadlines; real HTTP
	// servers do, including through telemetry's Unwrap-capable recorder.
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		w.Header().Set("Connection", "close")
		return fmt.Errorf("invalid request: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		// Do not reuse a connection after malformed or stalled request framing.
		w.Header().Set("Connection", "close")
		return errors.New("request must contain one JSON object")
	}
	return nil
}

// v2 handlers expose pgCodeStore over HTTP as /v2/code/*.
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
		log.Printf("load user settings: %v", err)
		http.Error(w, "load user settings failed", http.StatusInternalServerError)
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
			log.Printf("load user settings: %v", err)
			http.Error(w, "load user settings failed", http.StatusInternalServerError)
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
			log.Printf("set user settings: %v", err)
			http.Error(w, "set user settings failed", http.StatusInternalServerError)
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
	Content     string `json:"content,omitempty"`
	ReuseParent bool   `json:"reuse_parent,omitempty"`
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
		ParentCommitID    string               `json:"parent_commit_id,omitempty"`
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
		// The hash is the file's identity across snapshots (rename and
		// unchanged-file detection key on it), so one that does not match
		// the content sent with it is refused rather than stored.
		if f.ReuseParent && (f.Content != "" || req.ParentCommitID == "") {
			http.Error(w, "reuse_parent requires omitted content and parent_commit_id", http.StatusBadRequest)
			return
		}
		if f.Content != "" {
			sum := sha256.Sum256([]byte(f.Content))
			if !strings.EqualFold(hex.EncodeToString(sum[:]), strings.TrimSpace(f.ContentHash)) {
				http.Error(w, "content_hash for "+f.Path+" does not match its content (sha256 hex expected)", http.StatusBadRequest)
				return
			}
		}
	}
	// The snapshot is labelled with the extractor that actually parses it;
	// a caller cannot name another.
	if req.ExtractorVersion != "" && req.ExtractorVersion != extractorVersion {
		http.Error(w, "extractor_version is set by the server ("+extractorVersion+"); omit it", http.StatusBadRequest)
		return
	}
	req.ExtractorVersion = extractorVersion

	// Ownership check: the repository must belong to the authenticated
	// tenant. EnsureRepository is keyed on (tenant_id,
	// canonical_path), not on repository_id, so a repository_id supplied by
	// one tenant that happens to belong to another is rejected here rather
	// than silently indexing into it.
	if _, err := s.pgStore.RepositoryPathForTenant(r.Context(), tenant, req.RepositoryID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "repository not found", http.StatusNotFound)
			return
		}
		http.Error(w, "load repository failed", http.StatusInternalServerError)
		return
	}

	files := make([]PGFileInput, 0, len(req.Files))
	for _, f := range req.Files {
		files = append(files, PGFileInput{Path: f.Path, Language: f.Language, ContentHash: f.ContentHash, Content: f.Content, ReuseParent: f.ReuseParent, ExpectedParentCommit: req.ParentCommitID})
	}

	result, err := s.indexDurableSnapshot(r.Context(), tenant, req.RepositoryID, req.Branch, req.CommitID, req.ExtractorVersion, req.ConfigurationHash, files)
	if errors.Is(err, errSnapshotConflict) {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, result)
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
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		http.Error(w, "request must contain one JSON object", http.StatusBadRequest)
		return
	}
	if !snapshotIDUUID.MatchString(req.SnapshotID) {
		http.Error(w, "valid snapshot_id is required", http.StatusBadRequest)
		return
	}

	// The identity check, parent resolution and symbol reads must share the
	// authenticated tenant's RLS transaction, not a pooled connection.
	diff, err := s.pgStore.DiffSnapshotScoped(r.Context(), tenant, req.SnapshotID)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "snapshot not found", http.StatusNotFound)
		return
	}
	if err != nil {
		log.Printf("scoped snapshot diff: %v", err)
		http.Error(w, "snapshot diff unavailable", http.StatusInternalServerError)
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
		AsOfCommit   string `json:"as_of_commit,omitempty"`
		SnapshotID   string `json:"snapshot_id,omitempty"`
		MaxResults   int    `json:"max_results,omitempty"`
	}
	if err := decodeSinglePGJSON(w, r, &req, pgJSONReadTimeout); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.RepositoryID == "" || req.Query == "" {
		http.Error(w, "repository_id and query are required", http.StatusBadRequest)
		return
	}
	if (req.AsOfCommit != "" || req.SnapshotID != "") && req.Branch == "" {
		http.Error(w, "branch is required for historical queries", http.StatusBadRequest)
		return
	}
	if req.Branch == "" {
		branch, err := s.pgStore.LatestIndexedBranch(r.Context(), req.RepositoryID, tenant)
		if err != nil {
			http.Error(w, "resolve branch: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if branch == "" {
			if building, err := s.pgStore.IndexingSnapshot(r.Context(), req.RepositoryID, tenant); err == nil && building != "" {
				http.Error(w, "repository is still being indexed (snapshot "+building+"); check scos_code_index_status", http.StatusConflict)
				return
			}
			http.Error(w, "repository has no indexed snapshot yet; run scos_code_index first", http.StatusNotFound)
			return
		}
		req.Branch = branch
	}
	limit := req.MaxResults
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}

	// The tenant scope always comes from the authenticated caller, never a
	// client-supplied field -- same rule as requireTenant (§19.10) -- so a
	// query can never be widened to another tenant's repositories even if
	// repository_id happens to collide.
	if _, err := s.pgStore.RepositoryPathForTenant(r.Context(), tenant, req.RepositoryID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, map[string]any{"results": []PGSymbolVersion{}})
			return
		}
		http.Error(w, "load repository failed", http.StatusInternalServerError)
		return
	}

	if req.AsOfCommit != "" || req.SnapshotID != "" {
		if req.SnapshotID != "" && !snapshotIDUUID.MatchString(req.SnapshotID) {
			http.Error(w, "snapshot_id must be a UUID", http.StatusBadRequest)
			return
		}
		historical, err := s.pgStore.QueryHistoricalCode(r.Context(), tenant, req.RepositoryID, req.Branch, req.AsOfCommit, req.SnapshotID, req.Query, limit)
		if errors.Is(err, errHistoricalNotFound) {
			http.Error(w, "historical snapshot not found", http.StatusNotFound)
			return
		}
		if errors.Is(err, errHistoricalAmbiguous) {
			http.Error(w, "commit has multiple indexed snapshots; specify snapshot_id", http.StatusConflict)
			return
		}
		if err != nil {
			log.Printf("historical code query: %v", err)
			http.Error(w, "historical query unavailable", http.StatusInternalServerError)
			return
		}
		writeJSON(w, historical)
		return
	}

	exact, err := s.pgStore.QueryByPathOrFQNScoped(r.Context(), tenant, req.RepositoryID, req.Branch, req.Query)
	if err != nil {
		http.Error(w, "query: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// A declaration name ("SignClaims", "OutputReducer.reduce_output",
	// "class OutputReducer") names a declaration as surely as its fully
	// qualified name does, so it anchors the same way when it is exactly
	// the trailing segments of a function, method, type, class or
	// interface's qualified name.
	if name := declarationQuery(req.Query); len(exact) == 0 && name != "" {
		for _, pattern := range declarationPatterns(name) {
			if exact, err = s.pgStore.QueryByPathOrFQNScoped(r.Context(), tenant, req.RepositoryID, req.Branch, pattern); err != nil {
				http.Error(w, "query: "+err.Error(), http.StatusInternalServerError)
				return
			}
			if len(exact) > 0 {
				break
			}
		}
	}
	// Exact path/FQN matches are precision anchors. Never bury one under
	// learned or lexical ranking just because a model happens to like another
	// symbol more.
	if len(exact) > 0 {
		if len(exact) > limit {
			exact = exact[:limit]
		}
		writeJSON(w, map[string]any{
			"results":         exact,
			"retrieval_mode":  "exact",
			"degraded_models": []string{},
		})
		return
	}

	lexical, err := s.pgStore.QueryLexicalScoped(r.Context(), tenant, req.RepositoryID, req.Branch, req.Query, limit)
	if err != nil {
		http.Error(w, "lexical query: "+err.Error(), http.StatusInternalServerError)
		return
	}
	lexicalHybrid := lexicalCodeResults(lexical)
	dense := []codeHybridResult{}
	degradedModels := []string{}
	mode := "lexical"
	var codeSpace any

	if codeModelRuntimeConfigured() {
		space, spaceErr := activeCodeEmbeddingSpaceFromEnvironment()
		if spaceErr != nil {
			log.Printf("code embedding space invalid: %v", spaceErr)
			degradedModels = append(degradedModels, "code_embedding")
		} else {
			codeSpace = space
			if err := s.ensureActiveCodeEmbeddings(r.Context(), tenant, req.RepositoryID, req.Branch); err != nil {
				log.Printf("code embedding backfill degraded for %s/%s: %v", req.RepositoryID, req.Branch, err)
				degradedModels = append(degradedModels, "code_embedding")
			} else if dense, err = s.queryDenseCode(r.Context(), tenant, req.RepositoryID, req.Branch, req.Query, limit); err != nil {
				log.Printf("dense code query degraded: %v", err)
				degradedModels = append(degradedModels, "code_embedding")
				dense = []codeHybridResult{}
			} else {
				mode = "hybrid"
			}
		}
	}

	seedIDs := make([]string, 0, len(lexicalHybrid)+len(dense))
	seenSeeds := make(map[string]bool, cap(seedIDs))
	for _, item := range append(append([]codeHybridResult(nil), lexicalHybrid...), dense...) {
		if item.SymbolVersionID != "" && !seenSeeds[item.SymbolVersionID] {
			seenSeeds[item.SymbolVersionID] = true
			seedIDs = append(seedIDs, item.SymbolVersionID)
		}
	}
	structural := []codeHybridResult{}
	degradedSources := []string{}
	if len(seedIDs) > 0 {
		var ranks map[string]float64
		structural, ranks, err = s.pgStore.QueryStructuralCandidatesScoped(r.Context(), tenant, req.RepositoryID, req.Branch, seedIDs, 2, limit)
		if err != nil {
			log.Printf("structural code retrieval degraded: %v", err)
			degradedSources = append(degradedSources, "structural_graph")
			structural = []codeHybridResult{}
		} else {
			for i := range lexicalHybrid {
				lexicalHybrid[i].Centrality = ranks[lexicalHybrid[i].SymbolVersionID]
			}
			for i := range dense {
				dense[i].Centrality = ranks[dense[i].SymbolVersionID]
			}
		}
	}

	results := fuseCodeSignals(lexicalHybrid, dense, structural, 60)
	if len(results) > limit {
		results = results[:limit]
	}
	signals := []string{"lexical"}
	if len(dense) > 0 {
		signals = append(signals, "dense")
	}
	if len(structural) > 0 {
		signals = append(signals, "structural", "pagerank")
	}
	writeJSON(w, map[string]any{
		"results":          results,
		"retrieval_mode":   mode,
		"signals":          signals,
		"degraded_models":  degradedModels,
		"degraded_sources": degradedSources,
		"embedding_space":  codeSpace,
	})
}

func (s *server) handlePGImpact(w http.ResponseWriter, r *http.Request) {
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
		MaxDepth     int    `json:"max_depth,omitempty"`
		AsOfCommit   string `json:"as_of_commit,omitempty"`
		SnapshotID   string `json:"snapshot_id,omitempty"`
	}
	if err := decodeSinglePGJSON(w, r, &req, pgJSONReadTimeout); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.RepositoryID == "" || req.Query == "" {
		http.Error(w, "repository_id and query are required", http.StatusBadRequest)
		return
	}
	if req.MaxDepth > maxImpactDepth {
		http.Error(w, "max_depth must be <= 8", http.StatusUnprocessableEntity)
		return
	}
	if (req.AsOfCommit != "" || req.SnapshotID != "") && req.Branch == "" {
		http.Error(w, "branch is required for historical impact", http.StatusBadRequest)
		return
	}
	if req.SnapshotID != "" && !snapshotIDUUID.MatchString(req.SnapshotID) {
		http.Error(w, "snapshot_id must be a UUID", http.StatusBadRequest)
		return
	}
	if req.Branch == "" {
		branch, err := s.pgStore.LatestIndexedBranch(r.Context(), req.RepositoryID, tenant)
		if err != nil {
			http.Error(w, "resolve branch: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if branch == "" {
			if building, err := s.pgStore.IndexingSnapshot(r.Context(), req.RepositoryID, tenant); err == nil && building != "" {
				http.Error(w, "repository is still being indexed (snapshot "+building+"); check scos_code_index_status", http.StatusConflict)
				return
			}
			http.Error(w, "repository has no indexed snapshot yet; run scos_code_index first", http.StatusNotFound)
			return
		}
		req.Branch = branch
	}

	// Same tenant-ownership rule as handlePGQuery: a repository belonging
	// to another tenant behaves as if it doesn't exist (§19.10), not as an
	// error that would confirm its existence.
	if _, err := s.pgStore.RepositoryPathForTenant(r.Context(), tenant, req.RepositoryID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "repository not found", http.StatusNotFound)
			return
		}
		http.Error(w, "load repository failed", http.StatusInternalServerError)
		return
	}

	var impact *PGImpact
	var err error
	if req.AsOfCommit != "" || req.SnapshotID != "" {
		impact, err = s.pgStore.ChangeImpactHistorical(
			r.Context(), tenant, req.RepositoryID, req.Branch,
			req.AsOfCommit, req.SnapshotID, req.Query, req.MaxDepth)
	} else {
		impact, err = s.pgStore.ChangeImpactScoped(r.Context(), tenant, req.RepositoryID, req.Branch, req.Query, req.MaxDepth)
	}
	if err != nil {
		switch {
		case errors.Is(err, errHistoricalAmbiguous):
			http.Error(w, "commit has multiple indexed snapshots; specify snapshot_id", http.StatusConflict)
		case errors.Is(err, errHistoricalNotFound), errors.Is(err, ErrSymbolNotFound):
			http.Error(w, "symbol or historical snapshot not found", http.StatusNotFound)
		case errors.Is(err, errImpactDepthTooLarge):
			http.Error(w, "max_depth must be <= 8", http.StatusUnprocessableEntity)
		case errors.Is(err, errImpactNodeBudgetExceeded):
			http.Error(w, "impact node budget exceeded; narrow the symbol query", http.StatusUnprocessableEntity)
		case errors.Is(err, errImpactEdgeBudgetExceeded):
			http.Error(w, "impact edge budget exceeded; narrow the symbol query", http.StatusUnprocessableEntity)
		case errors.Is(err, errHistoricalImpactTooBroad):
			http.Error(w, "too many matching symbols; specify a fully-qualified name", http.StatusUnprocessableEntity)
		default:
			log.Printf("change impact: %v", err)
			http.Error(w, "change impact unavailable", http.StatusInternalServerError)
		}
		return
	}
	knowledge, err := s.pgStore.KnowledgeImpact(r.Context(), tenant, principal, req.RepositoryID, impact)
	if err != nil {
		http.Error(w, "knowledge impact unavailable", http.StatusInternalServerError)
		return
	}
	impact.KnowledgeDependents = knowledge
	writeJSON(w, impact)
}
