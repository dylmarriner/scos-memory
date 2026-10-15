package main

import "github.com/dylmarriner/scos-memory/packages/release"

import "github.com/dylmarriner/scos-memory/packages/auth"
import "github.com/dylmarriner/scos-memory/packages/tlsserve"

import (
	"context"
	"crypto/sha256"
	"database/sql"
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

	"github.com/dylmarriner/scos-memory/packages/database"
	"github.com/dylmarriner/scos-memory/packages/telemetry"
	"github.com/dylmarriner/scos-memory/services/readiness"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// serviceMetrics emits this service's §32 measures. It is nil-safe and a
// no-op when no collector is configured.
var serviceMetrics = telemetry.FromEnvironment("code-intelligence")

const version = "0.1.0"

var startTime = time.Now()

func handleVersion(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":         "ok",
		"version":        version,
		"uptime_seconds": int(time.Since(startTime).Seconds()),
	})
}

// ── types ────────────────────────────────────────────────────────────────

type Repository struct {
	ID                string   `json:"repository_id"`
	TenantID          string   `json:"tenant_id,omitempty"`
	CanonicalPath     string   `json:"canonical_path"`
	RemoteIdentity    string   `json:"remote_identity"`
	Branch            string   `json:"branch_or_worktree"`
	CommitID          string   `json:"commit_id"`
	IndexGeneration   int      `json:"index_generation"`
	Languages         []string `json:"language_set"`
	ConfigurationHash string   `json:"configuration_hash"`
	CreatedAt         string   `json:"created_at"`
	UpdatedAt         string   `json:"updated_at"`
}

type LanguageAdapter struct {
	Language     string   `json:"language"`
	Tier         int      `json:"tier"` // 0-4
	Capabilities []string `json:"capabilities"`
}

type Symbol struct {
	ID                     string   `json:"symbol_id"`
	RepoID                 string   `json:"repository_id,omitempty"`
	FullyQualifiedName     string   `json:"fully_qualified_name"`
	Kind                   string   `json:"kind"` // package, module, class, function, method, type, interface, variable, constant, schema
	Signature              string   `json:"signature,omitempty"`
	Path                   string   `json:"path"`
	Range                  string   `json:"range"`
	ParentSymbol           string   `json:"parent_symbol,omitempty"`
	Language               string   `json:"language"`
	ContentHash            string   `json:"content_hash"`
	CommitID               string   `json:"commit_id,omitempty"`
	DocComment             string   `json:"doc_comment,omitempty"`
	Imports                []string `json:"imports,omitempty"`
	OutboundRelations      []string `json:"outbound_relations,omitempty"`
	InboundRelationSummary []string `json:"inbound_relation_summary,omitempty"`
	Tests                  []string `json:"tests,omitempty"`
	Visibility             string   `json:"visibility,omitempty"`
	// CalledNames lists the bare function/method names this symbol's body
	// calls syntactically (function/method kinds only). Populated by
	// extractTreeSitterSymbols the same way goextract.go's calledNames
	// works for Go: a name here is unresolved until pgstore.go's edge
	// builder matches it against another declaration in the same scope.
	CalledNames []string `json:"-"`
}

type IndexGeneration struct {
	Generation  int    `json:"generation"`
	RepoID      string `json:"repository_id"`
	CommitID    string `json:"commit_id"`
	SymbolCount int    `json:"symbol_count"`
	FileCount   int    `json:"file_count"`
	Status      string `json:"status"` // building, active, stale
	StartedAt   string `json:"started_at"`
	CompletedAt string `json:"completed_at,omitempty"`
}

type Chunk struct {
	SymbolID    string `json:"symbol_id"`
	Content     string `json:"content"`
	Path        string `json:"path"`
	Range       string `json:"range"`
	LineStart   int    `json:"line_start"`
	LineEnd     int    `json:"line_end"`
	ContentHash string `json:"content_hash"`
}

type CodeQuery struct {
	Intent      string `json:"intent"`
	Query       string `json:"query"`
	TenantID    string `json:"tenant_id,omitempty"`
	RepoID      string `json:"repository_id,omitempty"`
	Path        string `json:"path,omitempty"`
	SymbolName  string `json:"symbol_name,omitempty"`
	Language    string `json:"language,omitempty"`
	Branch      string `json:"branch,omitempty"`
	MaxResults  int    `json:"max_results,omitempty"`
	IncludeDocs bool   `json:"include_docs,omitempty"`
}

type CodeQueryResult struct {
	Symbol    Symbol  `json:"symbol"`
	Chunk     Chunk   `json:"chunk"`
	Relevance float64 `json:"relevance_score"`
	Plane     string  `json:"plane"`
	Repo      string  `json:"repo"`
	Branch    string  `json:"branch"`
	CommitID  string  `json:"commit_id"`
}

type ChangeImpactRequest struct {
	RepoID   string   `json:"repository_id"`
	CommitID string   `json:"commit_id"`
	Symbols  []string `json:"modified_symbols"`
}

type ChangeImpact struct {
	DirectModified       []string `json:"directly_modified_symbols"`
	DirectDependants     []string `json:"direct_dependants"`
	TransitiveDependants []string `json:"transitive_dependants"`
	PublicContractChange bool     `json:"public_contract_changes"`
	SchemaChange         bool     `json:"schema_or_migration_effects"`
	TestCoverage         []string `json:"test_coverage"`
	RiskScore            float64  `json:"risk_score"`
}

// ImpactReconciliationRequest reconciles a previously predicted change
// impact with observed build, test and runtime evidence (§19.9). Prediction
// errors recorded here become learning signals for future risk scoring.
type ImpactReconciliationRequest struct {
	RepoID              string  `json:"repository_id"`
	CommitID            string  `json:"commit_id"`
	PredictedRiskScore  float64 `json:"predicted_risk_score"`
	PredictedDependants int     `json:"predicted_dependants"`
	BuildPassed         *bool   `json:"build_passed,omitempty"`
	TestsPassed         *bool   `json:"tests_passed,omitempty"`
	ActualDependants    int     `json:"actual_dependants"`
}

// ImpactReconciliation is the durable, versioned learning-signal record
// produced by reconciling a prediction with reality.
type ImpactReconciliation struct {
	RepoID              string  `json:"repository_id"`
	CommitID            string  `json:"commit_id"`
	PredictedRiskScore  float64 `json:"predicted_risk_score"`
	PredictedDependants int     `json:"predicted_dependants"`
	ActualDependants    int     `json:"actual_dependants"`
	BuildPassed         *bool   `json:"build_passed,omitempty"`
	TestsPassed         *bool   `json:"tests_passed,omitempty"`
	DependantCountError int     `json:"dependant_count_error"`
	OutcomeMismatch     bool    `json:"outcome_mismatch"`
	RecordedAt          string  `json:"recorded_at"`
}

// ── in-memory store ──────────────────────────────────────────────────────

type codeStore struct {
	mu              sync.RWMutex
	path            string
	repos           map[string]*Repository
	symbols         map[string]*Symbol          // symbol_id -> symbol
	chunks          map[string]*Chunk           // symbol_id -> chunk
	generations     map[string]*IndexGeneration // repo_id -> latest generation
	adapters        map[string]*LanguageAdapter
	relations       map[string][]string // symbol_id -> dependant symbol IDs
	reconciliations []ImpactReconciliation
}

func newCodeStore() *codeStore {
	return newCodeStoreAt("")
}

func newCodeStoreAt(path string) *codeStore {
	return &codeStore{
		path:        path,
		repos:       make(map[string]*Repository),
		symbols:     make(map[string]*Symbol),
		chunks:      make(map[string]*Chunk),
		generations: make(map[string]*IndexGeneration),
		adapters:    make(map[string]*LanguageAdapter),
		relations:   make(map[string][]string),
	}
}

type persistedCodeStore struct {
	Version         int                         `json:"version"`
	Repositories    map[string]*Repository      `json:"repositories"`
	Symbols         map[string]*Symbol          `json:"symbols"`
	Chunks          map[string]*Chunk           `json:"chunks"`
	Generations     map[string]*IndexGeneration `json:"generations"`
	Relations       map[string][]string         `json:"relations"`
	Reconciliations []ImpactReconciliation      `json:"reconciliations,omitempty"`
}

func (s *codeStore) snapshotLocked() persistedCodeStore {
	return persistedCodeStore{
		Version:         1,
		Repositories:    cloneRepositories(s.repos),
		Symbols:         cloneSymbols(s.symbols),
		Chunks:          cloneChunks(s.chunks),
		Generations:     cloneGenerations(s.generations),
		Relations:       cloneRelations(s.relations),
		Reconciliations: append([]ImpactReconciliation(nil), s.reconciliations...),
	}
}

func (s *codeStore) load() error {
	if s.path == "" {
		return nil
	}
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read code intelligence store: %w", err)
	}
	var persisted persistedCodeStore
	if err := json.Unmarshal(data, &persisted); err != nil {
		return fmt.Errorf("decode code intelligence store: %w", err)
	}
	if persisted.Version != 1 {
		return fmt.Errorf("unsupported code intelligence store version %d", persisted.Version)
	}
	if persisted.Repositories == nil || persisted.Symbols == nil || persisted.Chunks == nil || persisted.Generations == nil || persisted.Relations == nil {
		return fmt.Errorf("incomplete code intelligence store")
	}
	for repoID, generation := range persisted.Generations {
		if generation == nil || generation.Status != "active" {
			return fmt.Errorf("incomplete generation state for repository %s", repoID)
		}
		repo := persisted.Repositories[repoID]
		if repo == nil || repo.IndexGeneration != generation.Generation {
			return fmt.Errorf("generation metadata mismatch for repository %s", repoID)
		}
	}
	s.repos = persisted.Repositories
	s.symbols = persisted.Symbols
	s.chunks = persisted.Chunks
	s.generations = persisted.Generations
	s.relations = persisted.Relations
	s.reconciliations = persisted.Reconciliations
	return nil
}

func (s *codeStore) persistLocked() error {
	if s.path == "" {
		return nil
	}
	data, err := json.Marshal(s.snapshotLocked())
	if err != nil {
		return fmt.Errorf("encode code intelligence store: %w", err)
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return fmt.Errorf("create code intelligence store directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".code-intelligence-*.tmp")
	if err != nil {
		return fmt.Errorf("create code intelligence store temporary file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set code intelligence store permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write code intelligence store: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync code intelligence store: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close code intelligence store: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("commit code intelligence store: %w", err)
	}
	directory, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open code intelligence store directory: %w", err)
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return fmt.Errorf("sync code intelligence store directory: %w", err)
	}
	if err := directory.Close(); err != nil {
		return fmt.Errorf("close code intelligence store directory: %w", err)
	}
	return nil
}

func cloneRepositories(in map[string]*Repository) map[string]*Repository {
	out := make(map[string]*Repository, len(in))
	for id, value := range in {
		copy := *value
		copy.Languages = append([]string(nil), value.Languages...)
		out[id] = &copy
	}
	return out
}

func cloneSymbols(in map[string]*Symbol) map[string]*Symbol {
	out := make(map[string]*Symbol, len(in))
	for id, value := range in {
		copy := *value
		copy.Imports = append([]string(nil), value.Imports...)
		copy.OutboundRelations = append([]string(nil), value.OutboundRelations...)
		copy.InboundRelationSummary = append([]string(nil), value.InboundRelationSummary...)
		copy.Tests = append([]string(nil), value.Tests...)
		out[id] = &copy
	}
	return out
}

func cloneChunks(in map[string]*Chunk) map[string]*Chunk {
	out := make(map[string]*Chunk, len(in))
	for id, value := range in {
		copy := *value
		out[id] = &copy
	}
	return out
}

func cloneGenerations(in map[string]*IndexGeneration) map[string]*IndexGeneration {
	out := make(map[string]*IndexGeneration, len(in))
	for id, value := range in {
		copy := *value
		out[id] = &copy
	}
	return out
}

func cloneRelations(in map[string][]string) map[string][]string {
	out := make(map[string][]string, len(in))
	for id, values := range in {
		out[id] = append([]string(nil), values...)
	}
	return out
}

// ── server ───────────────────────────────────────────────────────────────

type server struct {
	store  *codeStore
	nextID int
	// pgStore is the durable, snapshot-based store (see pgstore.go), backing
	// the /v2 routes in pghandlers.go. Required and always initialized, the
	// same as every other Postgres-backed service in this fleet -- DATABASE_URL
	// missing or unreachable fails startup rather than degrading quietly.
	pgStore *pgCodeStore
}

func main() {
	path := os.Getenv("SCOS_CODE_INTELLIGENCE_STORE_PATH")
	store := newCodeStoreAt(path)
	registerDefaultAdapters(store)
	if err := store.load(); err != nil {
		log.Fatal(err)
	}
	srv := &server{store: store, nextID: nextRepositoryID(store.repos)}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		log.Fatal("DATABASE_URL is required")
	}
	if err := database.RequireTLS(databaseURL); err != nil {
		log.Fatal(err)
	}
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		log.Fatalf("connect to database: %v", err)
	}
	database.TuneSQLDB(db)
	srv.pgStore = newPGCodeStore(db)

	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", healthOK)
	mux.HandleFunc("/health/version", release.Handler("code-intelligence"))
	mux.HandleFunc("/health/ready", readiness.Handler(func() error {
		if len(store.adapters) == 0 {
			return fmt.Errorf("language adapters are not configured")
		}
		// srv.pgStore is always set in main() (DATABASE_URL is required at
		// startup); the nil check only matters for tests that construct a
		// *server directly without a database, which must not be reported
		// unready for a dependency they never configured.
		if srv.pgStore != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := srv.pgStore.db.PingContext(ctx); err != nil {
				return fmt.Errorf("durable code store is unreachable: %w", err)
			}
		}
		return nil
	}))
	mux.HandleFunc("/v1/version", handleVersion)

	// Repository management
	mux.HandleFunc("/v1/repos", srv.handleRepos)
	mux.HandleFunc("/v1/repos/", srv.handleRepoByID)

	// Indexing
	mux.HandleFunc("/v1/index", srv.handleIndex)
	mux.HandleFunc("/v1/code/index", srv.handleIndex)
	mux.HandleFunc("/v1/index/status", srv.handleIndexStatus)

	// Querying
	mux.HandleFunc("/v1/query", srv.handleQuery)
	mux.HandleFunc("/v1/code/query", srv.handleQuery)
	mux.HandleFunc("/v1/context", srv.handleContext)

	// Change impact
	mux.HandleFunc("/v1/impact", srv.handleImpact)
	mux.HandleFunc("/v1/code/impact", srv.handleImpact)
	mux.HandleFunc("/v1/impact/reconcile", srv.handleImpactReconcile)

	// Adapters
	mux.HandleFunc("/v1/adapters", srv.handleAdapters)

	// v2: durable, snapshot-based store (pgstore.go/pghandlers.go). Returns
	// 503 on every route when SCOS_DATABASE_URL is not configured.
	mux.HandleFunc("/v2/code/repos", srv.handlePGRepos)
	mux.HandleFunc("/v2/code/index", srv.handlePGIndex)
	mux.HandleFunc("/v2/code/query", srv.handlePGQuery)
	mux.HandleFunc("/v2/code/snapshots/diff", srv.handlePGSnapshotDiff)
	mux.HandleFunc("/v2/code/settings", srv.handlePGSettings)
	mux.HandleFunc("/v2/code/impact", srv.handlePGImpact)
	mux.HandleFunc("/v2/code/index/scip", srv.handlePGIndexSCIP)

	addr := os.Getenv("SCOS_CODE_INTELLIGENCE_HTTP_ADDR")
	if addr == "" {
		addr = ":8084"
	}
	log.Printf("code intelligence layer listening on %s", addr)
	if err := tlsserve.ListenAndServe(addr, telemetry.Instrument("code-intelligence", auth.ControlPlaneHandler(mux, auth.MustFromEnvironment()))); err != nil {
		log.Fatal(err)
	}
}

// requireTenant enforces repository and namespace permissions before search
// or mutation (§19.10): every network request must carry the caller's
// authenticated tenant scope, and results are always confined to that
// tenant's own repositories.
//
// Verified bearer claims take precedence over the X-SCOS-Tenant-ID header
// whenever they are present. This service is wrapped in
// auth.ControlPlaneHandler, which additionally requires a shared
// control-plane token, so its caller set is intended to be narrow (chiefly
// mcp-gateway) -- but auth.AuthenticatedHandler also accepts the shared
// static internal-service token, which carries no claims at all. Any caller
// holding both the control-plane token and the static service token could
// previously set X-SCOS-Tenant-ID to an arbitrary tenant and requireTenant
// would trust it unconditionally, regardless of whatever an authenticated
// claims-bearing token actually said -- a full cross-tenant IDOR across
// repos/query/context/impact. When claims are present the header is now
// ignored entirely; the header remains the fallback identity source only
// for the legitimate no-claims service-to-service path, matching the
// pattern already used by hermes-provider's resolveIdentity.
func requireTenant(r *http.Request) (string, bool) {
	if claims, ok := auth.CanonicalClaims(r.Context()); ok {
		tenant := strings.TrimSpace(claims.TenantID)
		return tenant, tenant != ""
	}
	tenant := strings.TrimSpace(r.Header.Get("X-SCOS-Tenant-ID"))
	return tenant, tenant != ""
}

// requirePrincipal mirrors requireTenant for the caller's own principal ID
// -- used only to look up or write *that caller's own* durable-store
// preference (see pghandlers.go's settings routes), never to scope
// someone else's data, so there is no cross-tenant/cross-principal
// exposure to guard against the way requireTenant does for repositories.
func requirePrincipal(r *http.Request) (string, bool) {
	if claims, ok := auth.CanonicalClaims(r.Context()); ok {
		principal := strings.TrimSpace(claims.PrincipalID)
		return principal, principal != ""
	}
	principal := strings.TrimSpace(r.Header.Get("X-SCOS-Principal-ID"))
	return principal, principal != ""
}

func healthOK(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// ── default adapters (§19.5) ─────────────────────────────────────────────

func registerDefaultAdapters(store *codeStore) {
	adapters := map[string]*LanguageAdapter{
		"go": {
			Language: "go",
			Tier:     4,
			Capabilities: []string{"detect", "parse", "extract_symbols", "extract_references",
				"extract_relations", "extract_diagnostics", "slice_context", "validate_change"},
		},
		"python": {
			Language: "python",
			Tier:     3,
			Capabilities: []string{"detect", "parse", "extract_symbols", "extract_references",
				"extract_relations", "slice_context"},
		},
		"typescript": {
			Language: "typescript",
			Tier:     3,
			Capabilities: []string{"detect", "parse", "extract_symbols", "extract_references",
				"extract_relations", "slice_context"},
		},
		"javascript": {
			Language: "javascript",
			Tier:     2,
			Capabilities: []string{"detect", "parse", "extract_symbols", "extract_references",
				"slice_context"},
		},
		"rust": {
			Language: "rust",
			Tier:     2,
			Capabilities: []string{"detect", "parse", "extract_symbols", "extract_references",
				"slice_context"},
		},
		"sql": {
			Language:     "sql",
			Tier:         1,
			Capabilities: []string{"detect", "extract_symbols", "slice_context"},
		},
		"yaml": {
			Language:     "yaml",
			Tier:         1,
			Capabilities: []string{"detect", "extract_symbols", "slice_context"},
		},
		"unknown": {
			Language:     "unknown",
			Tier:         0,
			Capabilities: []string{"detect", "slice_context"},
		},
	}
	for lang, ada := range adapters {
		store.adapters[lang] = ada
	}
}

// ── repository endpoints ─────────────────────────────────────────────────

func (s *server) handleRepos(w http.ResponseWriter, r *http.Request) {
	tenant, ok := requireTenant(r)
	if !ok {
		http.Error(w, "authenticated tenant header is required", http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.store.mu.RLock()
		var repos []*Repository
		for _, repo := range s.store.repos {
			if repo.TenantID == tenant {
				repos = append(repos, repo)
			}
		}
		s.store.mu.RUnlock()
		writeJSON(w, repos)

	case http.MethodPost:
		var repo Repository
		if err := json.NewDecoder(r.Body).Decode(&repo); err != nil {
			http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
			return
		}
		if repo.CanonicalPath == "" {
			http.Error(w, "canonical_path is required", http.StatusBadRequest)
			return
		}
		// The owning tenant is always derived from the authenticated caller,
		// never trusted from the request body, to prevent a caller from
		// registering a repository into another tenant's namespace.
		repo.TenantID = tenant
		s.store.mu.Lock()
		if current := nextRepositoryID(s.store.repos); current > s.nextID {
			s.nextID = current
		}
		candidate := newCodeStoreAt(s.store.path)
		candidate.repos = cloneRepositories(s.store.repos)
		candidate.symbols = cloneSymbols(s.store.symbols)
		candidate.chunks = cloneChunks(s.store.chunks)
		candidate.generations = cloneGenerations(s.store.generations)
		candidate.relations = cloneRelations(s.store.relations)
		s.nextID++
		repo.ID = fmt.Sprintf("repo-%d", s.nextID)
		now := time.Now().UTC().Format(time.RFC3339)
		repo.CreatedAt = now
		repo.UpdatedAt = now
		candidate.repos[repo.ID] = &repo
		if err := candidate.persistLocked(); err != nil {
			s.nextID--
			s.store.mu.Unlock()
			http.Error(w, "could not persist repository: "+err.Error(), http.StatusInternalServerError)
			return
		}
		s.store.repos = candidate.repos
		s.store.mu.Unlock()
		writeJSON(w, repo)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *server) handleRepoByID(w http.ResponseWriter, r *http.Request) {
	tenant, ok := requireTenant(r)
	if !ok {
		http.Error(w, "authenticated tenant header is required", http.StatusUnauthorized)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/repos/")
	if id == "" {
		http.Error(w, "repository_id required", http.StatusBadRequest)
		return
	}
	s.store.mu.RLock()
	repo, found := s.store.repos[id]
	s.store.mu.RUnlock()
	if !found || repo.TenantID != tenant {
		http.Error(w, "repository not found", http.StatusNotFound)
		return
	}
	writeJSON(w, repo)
}

// ── indexing endpoint ────────────────────────────────────────────────────

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	tenant, ok := requireTenant(r)
	if !ok {
		http.Error(w, "authenticated tenant header is required", http.StatusUnauthorized)
		return
	}

	var req struct {
		RepoID   string   `json:"repository_id"`
		CommitID string   `json:"commit_id"`
		Files    []string `json:"files"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.RepoID == "" || len(req.Files) == 0 {
		http.Error(w, "repository_id and files are required", http.StatusBadRequest)
		return
	}
	for _, file := range req.Files {
		if strings.TrimSpace(file) == "" {
			http.Error(w, "files must not contain empty paths", http.StatusBadRequest)
			return
		}
	}

	s.store.mu.Lock()
	repo, found := s.store.repos[req.RepoID]
	if !found || repo.TenantID != tenant {
		s.store.mu.Unlock()
		http.Error(w, "repository not found", http.StatusNotFound)
		return
	}

	// Build the complete next state separately. The live maps are replaced only
	// after the atomic snapshot has been committed.
	candidate := newCodeStoreAt(s.store.path)
	candidate.repos = cloneRepositories(s.store.repos)
	candidate.symbols = cloneSymbols(s.store.symbols)
	candidate.chunks = cloneChunks(s.store.chunks)
	candidate.generations = cloneGenerations(s.store.generations)
	candidate.relations = cloneRelations(s.store.relations)
	candidate.adapters = s.store.adapters

	// Create new generation
	gen := repo.IndexGeneration + 1
	now := time.Now().UTC().Format(time.RFC3339)
	generation := &IndexGeneration{
		Generation: gen,
		RepoID:     req.RepoID,
		CommitID:   req.CommitID,
		Status:     "building",
		StartedAt:  now,
	}

	// Build the next generation off to the side and drop the previous
	// generation's symbols/chunks for this repository before publishing the new
	// one. A committed generation must be a complete snapshot for that repo,
	// never an accumulation of stale rows from older generations (§19.3–§19.4).
	for symbolID, sym := range candidate.symbols {
		if sym != nil && sym.RepoID == req.RepoID {
			delete(candidate.symbols, symbolID)
			delete(candidate.chunks, symbolID)
		}
	}

	// Index each file
	for _, file := range req.Files {
		lang := detectLanguage(file)
		adapter := s.store.adapters[lang]
		tier := 0
		if adapter != nil {
			tier = adapter.Tier
		}
		if lang == "configuration" {
			tier = 1
		}

		// Extract symbols based on tier
		symbols := extractSymbols(file, lang, tier, req.RepoID)
		for _, sym := range symbols {
			sym.CommitID = req.CommitID
			candidate.symbols[sym.ID] = &sym

			// Create chunk for this symbol
			candidate.chunks[sym.ID] = &Chunk{
				SymbolID:    sym.ID,
				Content:     sym.Signature,
				Path:        sym.Path,
				Range:       sym.Range,
				ContentHash: sym.ContentHash,
			}
		}
	}

	generation.SymbolCount = len(candidate.symbols)
	generation.FileCount = len(req.Files)
	generation.Status = "active"
	generation.CompletedAt = time.Now().UTC().Format(time.RFC3339)

	candidate.generations[req.RepoID] = generation
	candidate.repos[req.RepoID].IndexGeneration = gen
	candidate.repos[req.RepoID].CommitID = req.CommitID
	candidate.repos[req.RepoID].UpdatedAt = now
	if err := candidate.persistLocked(); err != nil {
		s.store.mu.Unlock()
		http.Error(w, "could not persist index generation: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.store.repos = candidate.repos
	s.store.symbols = candidate.symbols
	s.store.chunks = candidate.chunks
	s.store.generations = candidate.generations
	s.store.relations = candidate.relations
	s.store.mu.Unlock()

	writeJSON(w, generation)
}

func nextRepositoryID(repos map[string]*Repository) int {
	max := 0
	for id := range repos {
		var number int
		if _, err := fmt.Sscanf(id, "repo-%d", &number); err == nil && number > max {
			max = number
		}
	}
	return max
}

// handleIndexStatus previously returned every generation in the store with
// no tenant filtering at all -- a caller from any tenant could see every
// other tenant's indexing activity (repo IDs, commit IDs, file/symbol
// counts). Generations carry no tenant of their own, so they are scoped the
// same way handleRepoByID scopes a single repo: through the owning
// repository's TenantID.
func (s *server) handleIndexStatus(w http.ResponseWriter, r *http.Request) {
	tenant, ok := requireTenant(r)
	if !ok {
		http.Error(w, "authenticated tenant header is required", http.StatusUnauthorized)
		return
	}
	s.store.mu.RLock()
	var gens []*IndexGeneration
	for repoID, g := range s.store.generations {
		if repo, found := s.store.repos[repoID]; found && repo.TenantID == tenant {
			gens = append(gens, g)
		}
	}
	s.store.mu.RUnlock()
	writeJSON(w, gens)
}

// ── query endpoint (§19.7) ───────────────────────────────────────────────

func (s *server) handleQuery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	tenant, ok := requireTenant(r)
	if !ok {
		http.Error(w, "authenticated tenant header is required", http.StatusUnauthorized)
		return
	}

	var q CodeQuery
	if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if q.Intent == "" || q.Query == "" {
		http.Error(w, "intent and query are required", http.StatusBadRequest)
		return
	}
	if q.MaxResults <= 0 {
		q.MaxResults = 10
	}
	// The tenant scope always comes from the authenticated caller, never a
	// client-supplied field, so a query can never be widened to another
	// tenant's repositories (§19.10).
	q.TenantID = tenant

	s.store.mu.RLock()
	defer s.store.mu.RUnlock()

	results := s.withProvenance(s.executeQuery(q))
	if results == nil {
		results = []CodeQueryResult{}
	}

	resp := struct {
		Intent     string            `json:"intent"`
		Results    []CodeQueryResult `json:"results"`
		Count      int               `json:"count"`
		PlanesUsed []string          `json:"planes_used"`
	}{
		Intent:     q.Intent,
		Results:    results,
		Count:      len(results),
		PlanesUsed: determinePlanes(q.Intent),
	}

	// §32 code-intelligence measures. Symbol quality is the share of returned
	// results that resolved to a named symbol rather than a raw chunk; token
	// count is what the caller would actually spend on this context.
	corr := telemetry.Correlation{Tenant: tenant}
	labels := map[string]string{"intent": q.Intent}
	serviceMetrics.Gauge(telemetry.MetricSymbolQuality, symbolQuality(results), "ratio", labels, corr)
	serviceMetrics.Gauge(telemetry.MetricContextTokensPerTask,
		float64(estimateResultTokens(results)), "tokens", labels, corr)

	writeJSON(w, resp)
}

// symbolQuality reports the share of results that resolved to a named symbol.
// An empty result set has no quality to report and scores zero rather than a
// misleading one.
func symbolQuality(results []CodeQueryResult) float64 {
	if len(results) == 0 {
		return 0
	}
	named := 0
	for _, r := range results {
		if strings.TrimSpace(r.Symbol.FullyQualifiedName) != "" {
			named++
		}
	}
	return float64(named) / float64(len(results))
}

// estimateResultTokens approximates the token cost of a result set using the
// same 4-bytes-per-token heuristic the rest of the platform uses.
func estimateResultTokens(results []CodeQueryResult) int {
	bytes := 0
	for _, r := range results {
		bytes += len(r.Chunk.Content)
	}
	return bytes / 4
}

// scopedSymbols returns the subset of the index restricted to the requested
// repository and, when a tenant is known (i.e. the request arrived through
// the HTTP boundary rather than an internal unit test), to repositories
// owned by that tenant. This prevents one repository's (or one branch's,
// tracked as a distinct Repository row) symbols from leaking into another's
// query results, and prevents cross-tenant leakage across the whole index
// when no repository_id is given (§19.10).
func (s *server) scopedSymbols(q CodeQuery) map[string]*Symbol {
	if q.RepoID == "" && q.TenantID == "" && q.Branch == "" {
		return s.store.symbols
	}
	scoped := make(map[string]*Symbol, len(s.store.symbols))
	for id, sym := range s.store.symbols {
		if q.RepoID != "" && sym.RepoID != q.RepoID {
			continue
		}
		repo, ok := s.store.repos[sym.RepoID]
		if q.TenantID != "" {
			if !ok || repo.TenantID != q.TenantID {
				continue
			}
		}
		// §19 forbids serving cross-branch results: a symbol indexed from one
		// branch must never satisfy a query scoped to another.
		if q.Branch != "" {
			if !ok || !strings.EqualFold(repo.Branch, q.Branch) {
				continue
			}
		}
		scoped[id] = sym
	}
	return scoped
}

// withProvenance stamps each result with the repository, branch and commit it
// came from. §19 requires a result to identify exactly that; the fields
// existed on the result type but were previously left empty, which made every
// result unattributable.
func (s *server) withProvenance(results []CodeQueryResult) []CodeQueryResult {
	for i := range results {
		repo, ok := s.store.repos[results[i].Symbol.RepoID]
		if !ok {
			continue
		}
		results[i].Repo = repo.ID
		results[i].Branch = repo.Branch
		// The symbol records the commit it was indexed from, which may lag the
		// repository head; prefer the symbol's own commit when it has one.
		if results[i].Symbol.CommitID != "" {
			results[i].CommitID = results[i].Symbol.CommitID
		} else {
			results[i].CommitID = repo.CommitID
		}
	}
	return results
}

func (s *server) executeQuery(q CodeQuery) []CodeQueryResult {
	var results []CodeQueryResult
	qLower := strings.ToLower(q.Query)
	symbols := s.scopedSymbols(q)

	switch q.Intent {
	case "exact_lookup":
		// Exact text match on paths and content
		for _, sym := range symbols {
			if strings.Contains(strings.ToLower(sym.Path), qLower) ||
				strings.Contains(strings.ToLower(sym.FullyQualifiedName), qLower) {
				chunk := s.store.chunks[sym.ID]
				if chunk == nil {
					continue
				}
				results = append(results, CodeQueryResult{
					Symbol:    *sym,
					Chunk:     *chunk,
					Relevance: 1.0,
					Plane:     "exact",
				})
			}
		}

	case "symbol_lookup":
		for _, sym := range symbols {
			if strings.EqualFold(sym.FullyQualifiedName, q.Query) ||
				strings.HasSuffix(strings.ToLower(sym.FullyQualifiedName), "."+qLower) {
				chunk := s.store.chunks[sym.ID]
				if chunk == nil {
					continue
				}
				results = append(results, CodeQueryResult{
					Symbol:    *sym,
					Chunk:     *chunk,
					Relevance: 1.0,
					Plane:     "symbol",
				})
			}
		}

	case "reference_search":
		for _, sym := range symbols {
			for _, imp := range sym.Imports {
				if strings.Contains(strings.ToLower(imp), qLower) {
					chunk := s.store.chunks[sym.ID]
					if chunk == nil {
						continue
					}
					results = append(results, CodeQueryResult{
						Symbol:    *sym,
						Chunk:     *chunk,
						Relevance: 0.8,
						Plane:     "relation",
					})
					break
				}
			}
		}

	case "implementation_search":
		for _, sym := range symbols {
			if sym.Kind == "class" || sym.Kind == "function" || sym.Kind == "method" {
				if strings.Contains(strings.ToLower(sym.FullyQualifiedName), qLower) ||
					strings.Contains(strings.ToLower(sym.Signature), qLower) {
					chunk := s.store.chunks[sym.ID]
					if chunk == nil {
						continue
					}
					results = append(results, CodeQueryResult{
						Symbol:    *sym,
						Chunk:     *chunk,
						Relevance: 0.9,
						Plane:     "structural",
					})
				}
			}
		}

	case "architecture_question":
		// Find packages/modules and their relations
		for _, sym := range symbols {
			if sym.Kind == "package" || sym.Kind == "module" || sym.Kind == "interface" {
				if strings.Contains(strings.ToLower(sym.FullyQualifiedName), qLower) ||
					strings.Contains(strings.ToLower(sym.DocComment), qLower) {
					chunk := s.store.chunks[sym.ID]
					if chunk == nil {
						continue
					}
					results = append(results, CodeQueryResult{
						Symbol:    *sym,
						Chunk:     *chunk,
						Relevance: 0.7,
						Plane:     "relation",
					})
				}
			}
		}

	case "test_discovery":
		for _, sym := range symbols {
			if len(sym.Tests) > 0 {
				if strings.Contains(strings.ToLower(sym.FullyQualifiedName), qLower) {
					chunk := s.store.chunks[sym.ID]
					if chunk == nil {
						continue
					}
					results = append(results, CodeQueryResult{
						Symbol:    *sym,
						Chunk:     *chunk,
						Relevance: 0.85,
						Plane:     "runtime_evidence",
					})
				}
			}
		}
		// Also find test files
		for _, sym := range symbols {
			if strings.Contains(strings.ToLower(sym.Path), "_test") ||
				strings.Contains(strings.ToLower(sym.Path), ".spec.") ||
				strings.Contains(strings.ToLower(sym.Path), "/test_") {
				if strings.Contains(strings.ToLower(sym.FullyQualifiedName), qLower) ||
					strings.Contains(strings.ToLower(sym.Path), qLower) {
					chunk := s.store.chunks[sym.ID]
					if chunk == nil {
						continue
					}
					results = append(results, CodeQueryResult{
						Symbol:    *sym,
						Chunk:     *chunk,
						Relevance: 0.9,
						Plane:     "runtime_evidence",
					})
				}
			}
		}

	case "configuration_lookup":
		for _, sym := range symbols {
			if sym.Kind == "configuration" || strings.HasSuffix(sym.Path, ".yaml") ||
				strings.HasSuffix(sym.Path, ".yml") || strings.HasSuffix(sym.Path, ".toml") ||
				strings.HasSuffix(sym.Path, ".json") || strings.HasSuffix(sym.Path, ".env") {
				if strings.Contains(strings.ToLower(sym.FullyQualifiedName), qLower) ||
					strings.Contains(strings.ToLower(sym.Path), qLower) {
					chunk := s.store.chunks[sym.ID]
					if chunk == nil {
						continue
					}
					results = append(results, CodeQueryResult{
						Symbol:    *sym,
						Chunk:     *chunk,
						Relevance: 0.9,
						Plane:     "exact",
					})
				}
			}
		}

	default: // structural_pattern, bug_localisation, history_question, security_review
		// Broader fallback: search all symbols
		for _, sym := range symbols {
			if strings.Contains(strings.ToLower(sym.FullyQualifiedName), qLower) ||
				strings.Contains(strings.ToLower(sym.Signature), qLower) ||
				strings.Contains(strings.ToLower(sym.DocComment), qLower) {
				chunk := s.store.chunks[sym.ID]
				if chunk == nil {
					continue
				}
				results = append(results, CodeQueryResult{
					Symbol:    *sym,
					Chunk:     *chunk,
					Relevance: 0.5,
					Plane:     "semantic",
				})
			}
		}
	}

	// Sort by relevance
	sort.Slice(results, func(i, j int) bool {
		return results[i].Relevance > results[j].Relevance
	})

	if len(results) > q.MaxResults {
		results = results[:q.MaxResults]
	}
	return results
}

func determinePlanes(intent string) []string {
	switch intent {
	case "exact_lookup":
		return []string{"exact"}
	case "symbol_lookup":
		return []string{"symbol", "exact"}
	case "reference_search":
		return []string{"relation", "exact"}
	case "implementation_search":
		return []string{"structural", "symbol", "exact"}
	case "architecture_question":
		return []string{"relation", "structural", "semantic"}
	case "bug_localisation":
		return []string{"runtime_evidence", "historical", "structural"}
	case "change_impact":
		return []string{"relation", "structural", "runtime_evidence"}
	case "test_discovery":
		return []string{"runtime_evidence", "exact"}
	case "configuration_lookup":
		return []string{"exact"}
	default:
		return []string{"semantic", "exact", "symbol"}
	}
}

// ── context assembly endpoint (§19.8) ────────────────────────────────────

func (s *server) handleContext(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	tenant, ok := requireTenant(r)
	if !ok {
		http.Error(w, "authenticated tenant header is required", http.StatusUnauthorized)
		return
	}

	var req struct {
		Symbol         string `json:"symbol"`
		IncludeCallers bool   `json:"include_callers,omitempty"`
		IncludeCallees bool   `json:"include_callees,omitempty"`
		IncludeTests   bool   `json:"include_tests,omitempty"`
		IncludeDocs    bool   `json:"include_docs,omitempty"`
		MaxSymbols     int    `json:"max_symbols,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Symbol == "" {
		http.Error(w, "symbol is required", http.StatusBadRequest)
		return
	}
	if req.MaxSymbols <= 0 {
		req.MaxSymbols = 10
	}

	s.store.mu.RLock()
	defer s.store.mu.RUnlock()

	// Every lookup below is confined to the caller's own tenant so that
	// context assembly can never surface another tenant's symbols (§19.10).
	tenantSymbols := s.scopedSymbols(CodeQuery{TenantID: tenant})

	// Find the target symbol
	var target *Symbol
	for _, sym := range tenantSymbols {
		if sym.FullyQualifiedName == req.Symbol || sym.ID == req.Symbol {
			target = sym
			break
		}
	}
	if target == nil {
		http.Error(w, "symbol not found", http.StatusNotFound)
		return
	}

	ctx := struct {
		Target      Symbol   `json:"target"`
		Chunk       Chunk    `json:"chunk"`
		Callers     []Symbol `json:"callers,omitempty"`
		Callees     []Symbol `json:"callees,omitempty"`
		Tests       []Symbol `json:"tests,omitempty"`
		TotalTokens int      `json:"total_tokens"`
	}{
		Target: *target,
	}

	if ch, ok := s.store.chunks[target.ID]; ok {
		ctx.Chunk = *ch
	}

	// Find callers (inbound references)
	if req.IncludeCallers {
		for _, sym := range tenantSymbols {
			for _, ref := range sym.OutboundRelations {
				if ref == target.FullyQualifiedName {
					ctx.Callers = append(ctx.Callers, *sym)
					break
				}
			}
			if len(ctx.Callers) >= req.MaxSymbols {
				break
			}
		}
	}

	// Find callees (outbound references)
	if req.IncludeCallees {
		for _, ref := range target.OutboundRelations {
			for _, sym := range tenantSymbols {
				if sym.FullyQualifiedName == ref {
					ctx.Callees = append(ctx.Callees, *sym)
					break
				}
			}
			if len(ctx.Callees) >= req.MaxSymbols {
				break
			}
		}
	}

	// Attach test symbols
	if req.IncludeTests {
		for _, testName := range target.Tests {
			for _, sym := range tenantSymbols {
				if sym.ID == testName || sym.FullyQualifiedName == testName {
					ctx.Tests = append(ctx.Tests, *sym)
					break
				}
			}
		}
	}

	writeJSON(w, ctx)
}

// ── change impact endpoint (§19.9) ───────────────────────────────────────

func (s *server) handleImpact(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	tenant, ok := requireTenant(r)
	if !ok {
		http.Error(w, "authenticated tenant header is required", http.StatusUnauthorized)
		return
	}

	var req ChangeImpactRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.RepoID == "" || len(req.Symbols) == 0 {
		http.Error(w, "repository_id and symbols are required", http.StatusBadRequest)
		return
	}

	s.store.mu.RLock()
	defer s.store.mu.RUnlock()

	if repo, found := s.store.repos[req.RepoID]; !found || repo.TenantID != tenant {
		http.Error(w, "repository not found", http.StatusNotFound)
		return
	}
	// Change-impact analysis only ever walks the caller's own tenant's
	// symbols (§19.10).
	tenantSymbols := s.scopedSymbols(CodeQuery{TenantID: tenant})

	impact := ChangeImpact{
		DirectModified: req.Symbols,
	}

	// Find direct dependants (symbols that reference modified symbols)
	modifiedSet := make(map[string]bool)
	for _, s := range req.Symbols {
		modifiedSet[strings.ToLower(s)] = true
	}

	directSet := make(map[string]bool)
	transitiveSet := make(map[string]bool)

	for _, sym := range tenantSymbols {
		for _, ref := range sym.OutboundRelations {
			if modifiedSet[strings.ToLower(ref)] {
				directSet[sym.FullyQualifiedName] = true
				// Check if this is a public contract change
				if sym.Visibility == "public" || sym.Kind == "interface" || sym.Kind == "type" {
					impact.PublicContractChange = true
				}
				break
			}
		}
	}

	for _, sym := range tenantSymbols {
		if directSet[sym.FullyQualifiedName] {
			continue
		}
		for _, ref := range sym.OutboundRelations {
			if directSet[ref] {
				transitiveSet[sym.FullyQualifiedName] = true
				break
			}
		}
	}

	for name := range directSet {
		impact.DirectDependants = append(impact.DirectDependants, name)
	}
	for name := range transitiveSet {
		impact.TransitiveDependants = append(impact.TransitiveDependants, name)
	}

	// Find test coverage for modified symbols
	for _, sym := range tenantSymbols {
		for _, modified := range req.Symbols {
			for _, test := range sym.Tests {
				if strings.Contains(strings.ToLower(test), strings.ToLower(modified)) {
					impact.TestCoverage = append(impact.TestCoverage, sym.FullyQualifiedName+" ("+test+")")
				}
			}
		}
	}

	// Risk score: more dependants = higher risk
	totalAffected := len(req.Symbols) + len(directSet) + len(transitiveSet)
	if totalAffected <= 1 {
		impact.RiskScore = 0.1
	} else if totalAffected <= 5 {
		impact.RiskScore = 0.3
	} else if totalAffected <= 15 {
		impact.RiskScore = 0.6
	} else {
		impact.RiskScore = 0.9
	}
	if impact.PublicContractChange {
		impact.RiskScore = minFloat(impact.RiskScore+0.2, 1.0)
	}

	writeJSON(w, impact)
}

// ── change-impact reconciliation endpoint (§19.9) ────────────────────────
//
// After a change, the layer SHALL reconcile the predicted impact with
// build, test and runtime evidence, and prediction errors SHALL become
// learning signals. This durably records each reconciliation so ranking
// and risk-scoring improvements have verified evidence to learn from.
func (s *server) handleImpactReconcile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	tenant, ok := requireTenant(r)
	if !ok {
		http.Error(w, "authenticated tenant header is required", http.StatusUnauthorized)
		return
	}

	var req ImpactReconciliationRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.RepoID == "" || req.CommitID == "" {
		http.Error(w, "repository_id and commit_id are required", http.StatusBadRequest)
		return
	}

	s.store.mu.Lock()
	repo, found := s.store.repos[req.RepoID]
	if !found || repo.TenantID != tenant {
		s.store.mu.Unlock()
		http.Error(w, "repository not found", http.StatusNotFound)
		return
	}

	dependantError := req.ActualDependants - req.PredictedDependants
	if dependantError < 0 {
		dependantError = -dependantError
	}
	outcomeMismatch := false
	if req.BuildPassed != nil && !*req.BuildPassed && req.PredictedRiskScore < 0.5 {
		outcomeMismatch = true
	}
	if req.TestsPassed != nil && !*req.TestsPassed && req.PredictedRiskScore < 0.5 {
		outcomeMismatch = true
	}

	record := ImpactReconciliation{
		RepoID:              req.RepoID,
		CommitID:            req.CommitID,
		PredictedRiskScore:  req.PredictedRiskScore,
		PredictedDependants: req.PredictedDependants,
		ActualDependants:    req.ActualDependants,
		BuildPassed:         req.BuildPassed,
		TestsPassed:         req.TestsPassed,
		DependantCountError: dependantError,
		OutcomeMismatch:     outcomeMismatch,
		RecordedAt:          time.Now().UTC().Format(time.RFC3339),
	}

	previous := s.store.reconciliations
	s.store.reconciliations = append(s.store.reconciliations, record)
	if err := s.store.persistLocked(); err != nil {
		s.store.reconciliations = previous
		s.store.mu.Unlock()
		http.Error(w, "could not persist impact reconciliation: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.store.mu.Unlock()

	writeJSON(w, record)
}

// ── adapters endpoint ────────────────────────────────────────────────────

func (s *server) handleAdapters(w http.ResponseWriter, r *http.Request) {
	s.store.mu.RLock()
	var adapters []*LanguageAdapter
	for _, ada := range s.store.adapters {
		adapters = append(adapters, ada)
	}
	s.store.mu.RUnlock()
	sort.Slice(adapters, func(i, j int) bool {
		return adapters[i].Tier > adapters[j].Tier
	})
	writeJSON(w, adapters)
}

// ── symbol extraction ────────────────────────────────────────────────────

// symbolScope salts a per-file hash with the owning repository ID so that
// two repositories indexing the same relative path (e.g. two branches of the
// same repo tracked as distinct Repository rows) never collide in the shared
// global symbol/chunk maps.
func symbolScope(repoID, discriminator string) string {
	return sha256Hex([]byte(repoID + "\x00" + discriminator))
}

func extractSymbols(path string, language string, tier int, repoID string) []Symbol {
	base := filepath.Base(path)
	dir := filepath.Dir(path)
	var symbols []Symbol

	kind := "file"
	if language == "configuration" {
		kind = "configuration"
	}

	// Tier 0: file-level indexing
	contentHash := symbolScope(repoID, path)
	symbols = append(symbols, Symbol{
		ID:                 contentHash[:16],
		RepoID:             repoID,
		FullyQualifiedName: path,
		Kind:               kind,
		Path:               path,
		Range:              "1-",
		Language:           language,
		ContentHash:        contentHash,
		Signature:          path,
	})

	if tier < 1 {
		return symbols
	}

	// Extract common symbols by extension and naming conventions
	switch language {
	case "go":
		symbols = append(symbols, extractGoSymbols(base, dir, path, repoID)...)
	case "python":
		symbols = append(symbols, extractPythonSymbols(base, dir, path, repoID)...)
	case "typescript", "javascript":
		symbols = append(symbols, extractTSJSSymbols(base, dir, path, language, repoID)...)
	case "configuration":
		symbols = append(symbols, extractConfigurationSymbols(base, path, repoID)...)
	}

	return symbols
}

func extractGoSymbols(base, dir, path, repoID string) []Symbol {
	var symbols []Symbol
	// Package
	pkgName := filepath.Base(dir)
	if pkgName == "." {
		pkgName = "main"
	}
	pkgID := symbolScope(repoID, dir)
	symbols = append(symbols, Symbol{
		ID:                 "pkg:" + pkgID[:12],
		RepoID:             repoID,
		FullyQualifiedName: pkgName,
		Kind:               "package",
		Path:               path,
		Range:              "1-",
		Language:           "go",
		ContentHash:        pkgID,
	})
	return symbols
}

func extractPythonSymbols(base, dir, path, repoID string) []Symbol {
	var symbols []Symbol
	moduleName := strings.TrimSuffix(base, ".py")
	modID := symbolScope(repoID, path)
	symbols = append(symbols, Symbol{
		ID:                 "mod:" + modID[:12],
		RepoID:             repoID,
		FullyQualifiedName: moduleName,
		Kind:               "module",
		Path:               path,
		Range:              "1-",
		Language:           "python",
		ContentHash:        modID,
	})
	return symbols
}

func extractTSJSSymbols(base, dir, path, language, repoID string) []Symbol {
	var symbols []Symbol
	moduleName := strings.TrimSuffix(base, ".ts")
	moduleName = strings.TrimSuffix(moduleName, ".tsx")
	moduleName = strings.TrimSuffix(moduleName, ".js")
	moduleName = strings.TrimSuffix(moduleName, ".jsx")
	modID := symbolScope(repoID, path)
	symbols = append(symbols, Symbol{
		ID:                 "ts:" + modID[:12],
		RepoID:             repoID,
		FullyQualifiedName: moduleName,
		Kind:               "module",
		Path:               path,
		Range:              "1-",
		Language:           language,
		ContentHash:        modID,
	})
	return symbols
}

func extractConfigurationSymbols(base, path, repoID string) []Symbol {
	configID := symbolScope(repoID, "config:"+path)
	return []Symbol{{
		ID:                 "cfg:" + configID[:12],
		RepoID:             repoID,
		FullyQualifiedName: base,
		Kind:               "configuration",
		Path:               path,
		Range:              "1-",
		Language:           "configuration",
		ContentHash:        configID,
		Signature:          path,
	}}
}

// ── language detection ───────────────────────────────────────────────────

func detectLanguage(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".go":
		return "go"
	case ".py":
		return "python"
	case ".ts", ".tsx":
		return "typescript"
	case ".js", ".jsx", ".mjs":
		return "javascript"
	case ".rs":
		return "rust"
	case ".sql":
		return "sql"
	case ".yaml", ".yml":
		return "yaml"
	case ".json", ".toml", ".env", ".ini", ".cfg":
		return "configuration"
	default:
		return "unknown"
	}
}

// ── utility ──────────────────────────────────────────────────────────────

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

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
