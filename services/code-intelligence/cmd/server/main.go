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

type Repository struct {
	ID                string   `json:"repository_id"`
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

// ── in-memory store ──────────────────────────────────────────────────────

type codeStore struct {
	mu          sync.RWMutex
	repos       map[string]*Repository
	symbols     map[string]*Symbol          // symbol_id -> symbol
	chunks      map[string]*Chunk           // symbol_id -> chunk
	generations map[string]*IndexGeneration // repo_id -> latest generation
	adapters    map[string]*LanguageAdapter
	relations   map[string][]string // symbol_id -> dependant symbol IDs
}

func newCodeStore() *codeStore {
	return &codeStore{
		repos:       make(map[string]*Repository),
		symbols:     make(map[string]*Symbol),
		chunks:      make(map[string]*Chunk),
		generations: make(map[string]*IndexGeneration),
		adapters:    make(map[string]*LanguageAdapter),
		relations:   make(map[string][]string),
	}
}

// ── server ───────────────────────────────────────────────────────────────

type server struct {
	store  *codeStore
	nextID int
}

func main() {
	store := newCodeStore()
	registerDefaultAdapters(store)
	srv := &server{store: store}

	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", healthOK)
	mux.HandleFunc("/health/ready", healthOK)

	// Repository management
	mux.HandleFunc("/v1/repos", srv.handleRepos)
	mux.HandleFunc("/v1/repos/", srv.handleRepoByID)

	// Indexing
	mux.HandleFunc("/v1/index", srv.handleIndex)
	mux.HandleFunc("/v1/index/status", srv.handleIndexStatus)

	// Querying
	mux.HandleFunc("/v1/query", srv.handleQuery)
	mux.HandleFunc("/v1/code/query", srv.handleQuery)
	mux.HandleFunc("/v1/context", srv.handleContext)

	// Change impact
	mux.HandleFunc("/v1/impact", srv.handleImpact)
	mux.HandleFunc("/v1/code/impact", srv.handleImpact)

	// Adapters
	mux.HandleFunc("/v1/adapters", srv.handleAdapters)

	addr := os.Getenv("SCOS_CODE_INTELLIGENCE_HTTP_ADDR")
	if addr == "" {
		addr = ":8084"
	}
	log.Printf("code intelligence layer listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
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
	switch r.Method {
	case http.MethodGet:
		s.store.mu.RLock()
		var repos []*Repository
		for _, repo := range s.store.repos {
			repos = append(repos, repo)
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
		s.store.mu.Lock()
		s.nextID++
		repo.ID = fmt.Sprintf("repo-%d", s.nextID)
		now := time.Now().UTC().Format(time.RFC3339)
		repo.CreatedAt = now
		repo.UpdatedAt = now
		s.store.repos[repo.ID] = &repo
		s.store.mu.Unlock()
		writeJSON(w, repo)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *server) handleRepoByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/repos/")
	if id == "" {
		http.Error(w, "repository_id required", http.StatusBadRequest)
		return
	}
	s.store.mu.RLock()
	repo, ok := s.store.repos[id]
	s.store.mu.RUnlock()
	if !ok {
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

	var req struct {
		RepoID   string   `json:"repository_id"`
		CommitID string   `json:"commit_id"`
		Files    []string `json:"files"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.RepoID == "" || len(req.Files) == 0 {
		http.Error(w, "repository_id and files are required", http.StatusBadRequest)
		return
	}

	s.store.mu.Lock()
	repo, ok := s.store.repos[req.RepoID]
	if !ok {
		s.store.mu.Unlock()
		http.Error(w, "repository not found", http.StatusNotFound)
		return
	}

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

	// Index each file
	for _, file := range req.Files {
		lang := detectLanguage(file)
		adapter := s.store.adapters[lang]
		tier := 0
		if adapter != nil {
			tier = adapter.Tier
		}

		// Extract symbols based on tier
		symbols := extractSymbols(file, lang, tier)
		for _, sym := range symbols {
			sym.CommitID = req.CommitID
			s.store.symbols[sym.ID] = &sym

			// Create chunk for this symbol
			s.store.chunks[sym.ID] = &Chunk{
				SymbolID: sym.ID,
				Content:  sym.Signature,
				Path:     sym.Path,
				Range:    sym.Range,
			}
		}
	}

	generation.SymbolCount = len(s.store.symbols)
	generation.FileCount = len(req.Files)
	generation.Status = "active"
	generation.CompletedAt = time.Now().UTC().Format(time.RFC3339)

	s.store.generations[req.RepoID] = generation
	repo.IndexGeneration = gen
	repo.CommitID = req.CommitID
	repo.UpdatedAt = now
	s.store.mu.Unlock()

	writeJSON(w, generation)
}

func (s *server) handleIndexStatus(w http.ResponseWriter, r *http.Request) {
	s.store.mu.RLock()
	var gens []*IndexGeneration
	for _, g := range s.store.generations {
		gens = append(gens, g)
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

	s.store.mu.RLock()
	defer s.store.mu.RUnlock()

	results := s.executeQuery(q)
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
	writeJSON(w, resp)
}

func (s *server) executeQuery(q CodeQuery) []CodeQueryResult {
	var results []CodeQueryResult
	qLower := strings.ToLower(q.Query)

	switch q.Intent {
	case "exact_lookup":
		// Exact text match on paths and content
		for _, sym := range s.store.symbols {
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
		for _, sym := range s.store.symbols {
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
		for _, sym := range s.store.symbols {
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
		for _, sym := range s.store.symbols {
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
		for _, sym := range s.store.symbols {
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
		for _, sym := range s.store.symbols {
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
		for _, sym := range s.store.symbols {
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
		for _, sym := range s.store.symbols {
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
		for _, sym := range s.store.symbols {
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

	// Find the target symbol
	var target *Symbol
	for _, sym := range s.store.symbols {
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
		for _, sym := range s.store.symbols {
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
			for _, sym := range s.store.symbols {
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
			for _, sym := range s.store.symbols {
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

	for _, sym := range s.store.symbols {
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

	for _, sym := range s.store.symbols {
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
	for _, sym := range s.store.symbols {
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

func extractSymbols(path string, language string, tier int) []Symbol {
	base := filepath.Base(path)
	dir := filepath.Dir(path)
	var symbols []Symbol

	// Tier 0: file-level indexing
	contentHash := sha256Hex([]byte(path))
	symbols = append(symbols, Symbol{
		ID:                 contentHash[:16],
		FullyQualifiedName: path,
		Kind:               "file",
		Path:               path,
		Range:              "1-",
		Language:           language,
		ContentHash:        contentHash,
	})

	if tier < 1 {
		return symbols
	}

	// Extract common symbols by extension and naming conventions
	switch language {
	case "go":
		symbols = append(symbols, extractGoSymbols(base, dir, path)...)
	case "python":
		symbols = append(symbols, extractPythonSymbols(base, dir, path)...)
	case "typescript", "javascript":
		symbols = append(symbols, extractTSJSSymbols(base, dir, path, language)...)
	}

	return symbols
}

func extractGoSymbols(base, dir, path string) []Symbol {
	var symbols []Symbol
	// Package
	pkgName := filepath.Base(dir)
	if pkgName == "." {
		pkgName = "main"
	}
	pkgID := sha256Hex([]byte(dir))
	symbols = append(symbols, Symbol{
		ID:                 "pkg:" + pkgID[:12],
		FullyQualifiedName: pkgName,
		Kind:               "package",
		Path:               path,
		Range:              "1-",
		Language:           "go",
		ContentHash:        pkgID,
	})
	return symbols
}

func extractPythonSymbols(base, dir, path string) []Symbol {
	var symbols []Symbol
	moduleName := strings.TrimSuffix(base, ".py")
	modID := sha256Hex([]byte(path))
	symbols = append(symbols, Symbol{
		ID:                 "mod:" + modID[:12],
		FullyQualifiedName: moduleName,
		Kind:               "module",
		Path:               path,
		Range:              "1-",
		Language:           "python",
		ContentHash:        modID,
	})
	return symbols
}

func extractTSJSSymbols(base, dir, path, language string) []Symbol {
	var symbols []Symbol
	moduleName := strings.TrimSuffix(base, ".ts")
	moduleName = strings.TrimSuffix(moduleName, ".tsx")
	moduleName = strings.TrimSuffix(moduleName, ".js")
	moduleName = strings.TrimSuffix(moduleName, ".jsx")
	modID := sha256Hex([]byte(path))
	symbols = append(symbols, Symbol{
		ID:                 "ts:" + modID[:12],
		FullyQualifiedName: moduleName,
		Kind:               "module",
		Path:               path,
		Range:              "1-",
		Language:           language,
		ContentHash:        modID,
	})
	return symbols
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
