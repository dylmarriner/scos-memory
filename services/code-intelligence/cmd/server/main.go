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
	"strings"
	"sync"
	"time"

	"github.com/dylmarriner/scos-memory/packages/database"
	"github.com/dylmarriner/scos-memory/packages/telemetry"
	"github.com/dylmarriner/scos-memory/services/readiness"
	_ "github.com/jackc/pgx/v5/stdlib"
)

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

// Repository is a /v1/repos registration record. mcp-gateway's
// scos_code_repo_* tools register, list and fetch repositories through it;
// indexing, querying and change impact all run on the durable /v2 store.
type Repository struct {
	ID                string   `json:"repository_id"`
	TenantID          string   `json:"tenant_id,omitempty"`
	CanonicalPath     string   `json:"canonical_path"`
	RemoteIdentity    string   `json:"remote_identity"`
	Branch            string   `json:"branch_or_worktree"`
	CommitID          string   `json:"commit_id"`
	Languages         []string `json:"language_set"`
	ConfigurationHash string   `json:"configuration_hash"`
	CreatedAt         string   `json:"created_at"`
	UpdatedAt         string   `json:"updated_at"`
}

// Symbol is one declaration extracted by Tree-sitter (treesitter.go), which
// pgstore.go records as a syntax-tier code_symbol_versions row.
type Symbol struct {
	ID                 string
	RepoID             string
	FullyQualifiedName string
	Kind               string // class, function, method, type, interface
	Signature          string
	Path               string
	Range              string
	Language           string
	ContentHash        string
	// CalledNames lists the bare function/method names this symbol's body
	// calls syntactically (function/method kinds only). Populated by
	// extractTreeSitterSymbols the same way goextract.go's calledNames
	// works for Go: a name here is unresolved until pgstore.go's edge
	// builder matches it against another declaration in the same scope.
	CalledNames []string
}

// ── repository registry ──────────────────────────────────────────────────

// repoStore is the /v1/repos registry, persisted as a JSON file at
// SCOS_CODE_INTELLIGENCE_STORE_PATH when that is set.
type repoStore struct {
	mu    sync.RWMutex
	path  string
	repos map[string]*Repository
}

func newRepoStore() *repoStore {
	return newRepoStoreAt("")
}

func newRepoStoreAt(path string) *repoStore {
	return &repoStore{
		path:  path,
		repos: make(map[string]*Repository),
	}
}

// persistedRepoStore is the on-disk format. Files written before the
// in-memory symbol index was removed also carry symbols, chunks,
// generations, relations and reconciliations; those keys are ignored on
// load and dropped on the next write.
type persistedRepoStore struct {
	Version      int                    `json:"version"`
	Repositories map[string]*Repository `json:"repositories"`
}

func (s *repoStore) load() error {
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
	var persisted persistedRepoStore
	if err := json.Unmarshal(data, &persisted); err != nil {
		return fmt.Errorf("decode code intelligence store: %w", err)
	}
	if persisted.Version != 1 {
		return fmt.Errorf("unsupported code intelligence store version %d", persisted.Version)
	}
	if persisted.Repositories == nil {
		return fmt.Errorf("incomplete code intelligence store")
	}
	s.repos = persisted.Repositories
	return nil
}

// persistLocked atomically writes repos to s.path. The caller holds s.mu.
func (s *repoStore) persistLocked(repos map[string]*Repository) error {
	if s.path == "" {
		return nil
	}
	data, err := json.Marshal(persistedRepoStore{Version: 1, Repositories: repos})
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

// ── server ───────────────────────────────────────────────────────────────

type server struct {
	store  *repoStore
	nextID int
	// pgStore is the durable, snapshot-based store (see pgstore.go), backing
	// the /v2 routes in pghandlers.go. Required and always initialized, the
	// same as every other Postgres-backed service in this fleet -- DATABASE_URL
	// missing or unreachable fails startup rather than degrading quietly.
	pgStore *pgCodeStore
}

func main() {
	path := os.Getenv("SCOS_CODE_INTELLIGENCE_STORE_PATH")
	store := newRepoStoreAt(path)
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
	if err := database.RequireNoBypassRLSDB(context.Background(), db); err != nil {
		log.Fatalf("refusing to start: %v", err)
	}
	database.TuneSQLDB(db)
	srv.pgStore = newPGCodeStore(db)
	if n, err := srv.pgStore.FailInterruptedSnapshots(context.Background()); err != nil {
		log.Printf("mark interrupted code snapshots failed: %v", err)
	} else if n > 0 {
		log.Printf("marked %d code snapshot(s) interrupted by a previous stop as failed", n)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", healthOK)
	mux.HandleFunc("/health/version", release.Handler("code-intelligence"))
	mux.HandleFunc("/health/ready", readiness.Handler(func() error {
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

	// Repository registration (mcp-gateway's scos_code_repo_* tools).
	mux.HandleFunc("/v1/repos", srv.handleRepos)
	mux.HandleFunc("/v1/repos/", srv.handleRepoByID)

	// Durable, snapshot-based store (pgstore.go/pghandlers.go): indexing,
	// querying, snapshots, settings and change impact.
	mux.HandleFunc("/v2/code/repos", srv.handlePGRepos)
	mux.HandleFunc("/v2/code/index", srv.handlePGIndex)
	mux.HandleFunc("/v2/code/scan", srv.handlePGScan)
	mux.HandleFunc("/v2/code/snapshots", srv.handlePGSnapshots)
	mux.HandleFunc("/v2/code/snapshots/", srv.handlePGSnapshot)
	mux.HandleFunc("/v2/code/query", srv.handlePGQuery)
	mux.HandleFunc("/v2/code/snapshots/diff", srv.handlePGSnapshotDiff)
	mux.HandleFunc("/v2/code/snapshots/events", srv.handlePGSnapshotEvents)
	mux.HandleFunc("/v2/code/settings", srv.handlePGSettings)
	mux.HandleFunc("/v2/code/impact", srv.handlePGImpact)
	mux.HandleFunc("/v2/code/revalidations", srv.handlePGRevalidations)
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
// every repository and code route. When claims are present the header is
// now ignored entirely; the header remains the fallback identity source only
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
		candidate := cloneRepositories(s.store.repos)
		s.nextID++
		repo.ID = fmt.Sprintf("repo-%d", s.nextID)
		now := time.Now().UTC().Format(time.RFC3339)
		repo.CreatedAt = now
		repo.UpdatedAt = now
		candidate[repo.ID] = &repo
		if err := s.store.persistLocked(candidate); err != nil {
			s.nextID--
			s.store.mu.Unlock()
			http.Error(w, "could not persist repository: "+err.Error(), http.StatusInternalServerError)
			return
		}
		s.store.repos = candidate
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

// symbolScope salts a per-file hash with the owning repository ID so that
// two repositories declaring the same name at the same path never produce
// the same symbol ID.
func symbolScope(repoID, discriminator string) string {
	return sha256Hex([]byte(repoID + "\x00" + discriminator))
}

// ── language detection ───────────────────────────────────────────────────

func detectLanguage(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".go":
		return "go"
	case ".py":
		return "python"
	case ".ts", ".tsx", ".mts", ".cts":
		return "typescript"
	case ".js", ".jsx", ".mjs", ".cjs":
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
