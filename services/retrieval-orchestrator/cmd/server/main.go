package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	serviceauth "github.com/dylanmarriner/scos-memory-fabric/packages/service-auth"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ── request / response types ─────────────────────────────────────────────

type RetrieveRequest struct {
	TenantID       string            `json:"tenant_id"`
	PrincipalID    string            `json:"principal_id"`
	Query          string            `json:"query"`
	AgentID        string            `json:"agent_id,omitempty"`
	SessionID      string            `json:"session_id,omitempty"`
	TurnID         string            `json:"turn_id,omitempty"`
	Namespaces     []string          `json:"namespaces,omitempty"`
	MemoryClasses  []string          `json:"memory_classes,omitempty"`
	SensitivityMax string            `json:"sensitivity_max,omitempty"`
	MaxResults     int               `json:"max_results,omitempty"`
	TokenBudget    int               `json:"token_budget,omitempty"`
	IncludeRecent  bool              `json:"include_recent,omitempty"`
	IncludeSession bool              `json:"include_session,omitempty"`
	IncludeState   bool              `json:"include_state,omitempty"`
	Filters        map[string]string `json:"filters,omitempty"`
}

type RetrievalResult struct {
	MemoryID      string  `json:"memory_id"`
	Title         string  `json:"title"`
	Summary       string  `json:"summary"`
	Content       string  `json:"content"`
	MemoryClass   string  `json:"memory_class"`
	Authority     int     `json:"authority_level"`
	Confidence    float64 `json:"confidence_score"`
	Sensitivity   string  `json:"sensitivity"`
	LexicalScore  float64 `json:"lexical_score,omitempty"`
	SemanticScore float64 `json:"semantic_score,omitempty"`
	FusedScore    float64 `json:"fused_score"`
	Rank          int     `json:"rank"`
	Evidence      []any   `json:"evidence,omitempty"`
	Conflicts     []any   `json:"conflicts,omitempty"`
	Provenance    string  `json:"provenance"`
	NamespaceID   string  `json:"namespace_id"`
}

type ContextSection struct {
	Heading string            `json:"heading"`
	Items   []RetrievalResult `json:"items"`
}

type RetrieveResponse struct {
	RetrievalRunID string           `json:"retrieval_run_id"`
	QueryHash      string           `json:"query_hash"`
	Sections       []ContextSection `json:"sections"`
	TotalTokens    int              `json:"total_tokens"`
	LatencyMs      int              `json:"latency_ms"`
}

// ── server state ─────────────────────────────────────────────────────────

type server struct {
	db *pgxpool.Pool
}

// ── constants ────────────────────────────────────────────────────────────

const defaultMaxResults = 20
const defaultTokenBudget = 4000
const rrfK = 60.0

// ── main ─────────────────────────────────────────────────────────────────

func main() {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		log.Fatal("DATABASE_URL is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		log.Fatalf("connect to database: %v", err)
	}
	defer pool.Close()

	srv := &server{db: pool}

	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"live"}`))
	})
	mux.HandleFunc("/health/ready", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	})
	mux.HandleFunc("/v1/retrieve", srv.handleRetrieve)

	addr := os.Getenv("SCOS_RETRIEVAL_HTTP_ADDR")
	if addr == "" {
		addr = ":8082"
	}
	log.Printf("retrieval orchestrator listening on %s", addr)
	if err := http.ListenAndServe(addr, serviceauth.Require(mux)); err != nil {
		log.Fatal(err)
	}
}

// ── handler ──────────────────────────────────────────────────────────────

func (s *server) handleRetrieve(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req RetrieveRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}

	if req.TenantID == "" {
		http.Error(w, "tenant_id is required", http.StatusBadRequest)
		return
	}
	if req.Query == "" {
		http.Error(w, "query is required", http.StatusBadRequest)
		return
	}

	maxResults := req.MaxResults
	if maxResults <= 0 {
		maxResults = defaultMaxResults
	}
	tokenBudget := req.TokenBudget
	if tokenBudget <= 0 {
		tokenBudget = defaultTokenBudget
	}

	ctx := r.Context()

	// ── 1. Authorise / resolve namespaces ───────────────────────────
	namespaces, err := s.resolveAuthorisedNamespaces(ctx, req)
	if err != nil {
		http.Error(w, "authorisation error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if len(namespaces) == 0 {
		writeJSON(w, RetrieveResponse{
			Sections:    []ContextSection{},
			TotalTokens: 0,
			LatencyMs:   int(time.Since(start).Milliseconds()),
		})
		return
	}

	// ── 2. Parse query ──────────────────────────────────────────────
	queryHash := hashQuery(req.Query)
	tsQuery := toTSQuery(req.Query)

	// ── 3. Apply validity and sensitivity filters ────────────────────
	sensitivityFilter := req.SensitivityMax
	if sensitivityFilter == "" {
		sensitivityFilter = "internal"
	}

	// ── 4. Exact + metadata search ──────────────────────────────────
	exactResults, err := s.exactSearch(ctx, req, namespaces, sensitivityFilter, maxResults)
	if err != nil {
		log.Printf("exact search error: %v", err)
	}

	// ── 5. Full-text search ─────────────────────────────────────────
	ftsResults, err := s.fullTextSearch(ctx, req, namespaces, sensitivityFilter, tsQuery, maxResults)
	if err != nil {
		log.Printf("full-text search error: %v", err)
	}

	// ── 6. Vector search ────────────────────────────────────────────
	vectorResults, err := s.vectorSearch(ctx, req, namespaces, sensitivityFilter, maxResults)
	if err != nil {
		log.Printf("vector search error: %v", err)
	}

	// ── 7. Recent session state ─────────────────────────────────────
	var sessionResults []RetrievalResult
	if req.IncludeSession && req.SessionID != "" {
		sessionResults, _ = s.recentSessionState(ctx, req, maxResults)
	}

	// ── 8. Fusion via RRF ───────────────────────────────────────────
	fused := fuseResults(rrfK, exactResults, ftsResults, vectorResults, sessionResults)

	// ── 9. Dedup by memory_id ───────────────────────────────────────
	deduped := deduplicateResults(fused)

	// ── 10. Apply token budget ─────────────────────────────────────
	allocated := allocateByTokenBudget(deduped, tokenBudget)

	// ── 11. Assemble into sections per §17 ─────────────────────────
	sections := assembleSections(allocated)

	// ── 12. Record retrieval run ────────────────────────────────────
	retrievalRunID, err := s.recordRetrieval(ctx, req, queryHash, namespaces, sections, int(time.Since(start).Milliseconds()))
	if err != nil {
		log.Printf("record retrieval run: %v", err)
	}

	totalTokens := 0
	for _, sec := range sections {
		for _, item := range sec.Items {
			totalTokens += estimateTokens(item.Content)
		}
	}

	resp := RetrieveResponse{
		RetrievalRunID: retrievalRunID,
		QueryHash:      queryHash,
		Sections:       sections,
		TotalTokens:    totalTokens,
		LatencyMs:      int(time.Since(start).Milliseconds()),
	}

	writeJSON(w, resp)
}

// ── pipeline stages ──────────────────────────────────────────────────────

func (s *server) resolveAuthorisedNamespaces(ctx context.Context, req RetrieveRequest) ([]string, error) {
	if len(req.Namespaces) > 0 {
		return req.Namespaces, nil
	}
	// Default: query namespaces the principal can read
	rows, err := s.db.Query(ctx, `
		SELECT n.canonical_name
		FROM namespaces n
		JOIN namespace_memberships m ON m.namespace_id = n.id
		WHERE n.tenant_id = $1
		  AND m.principal_id = $2
		  AND m.can_read = true
		ORDER BY n.canonical_name
	`, req.TenantID, req.PrincipalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var namespaces []string
	for rows.Next() {
		var ns string
		if err := rows.Scan(&ns); err != nil {
			return nil, err
		}
		namespaces = append(namespaces, ns)
	}
	return namespaces, rows.Err()
}

func (s *server) exactSearch(ctx context.Context, req RetrieveRequest, namespaces []string, sensitivityMax string, limit int) ([]RetrievalResult, error) {
	query := `
		SELECT mi.id, mi.title, mi.canonical_summary, mv.content,
		       mi.type, mi.authority_level, mi.confidence_score, mi.sensitivity,
		       mi.namespace_id
		FROM memory_items mi
		LEFT JOIN memory_versions mv ON mv.id = mi.current_version_id
		WHERE mi.tenant_id = $1
		  AND mi.status = 'active'
		  AND mi.namespace_id = ANY($2::uuid[])
		  AND mi.sensitivity <= $3
		  AND (mi.expires_at IS NULL OR mi.expires_at > now())
		  AND (mi.valid_until IS NULL OR mi.valid_until > now())
		  AND (LOWER(mi.title) LIKE LOWER($4) OR LOWER(mv.content) LIKE LOWER($4))
		ORDER BY mi.authority_level DESC, mi.confidence_score DESC
		LIMIT $5
	`
	return s.queryResults(ctx, req, namespaces, sensitivityMax, limit, query, "%"+req.Query+"%", "exact")
}

func (s *server) fullTextSearch(ctx context.Context, req RetrieveRequest, namespaces []string, sensitivityMax string, tsQuery string, limit int) ([]RetrievalResult, error) {
	if tsQuery == "" {
		return nil, nil
	}
	query := `
		SELECT mi.id, mi.title, mi.canonical_summary, mv.content,
		       mi.type, mi.authority_level, mi.confidence_score, mi.sensitivity,
		       mi.namespace_id
		FROM memory_items mi
		LEFT JOIN memory_versions mv ON mv.id = mi.current_version_id
		WHERE mi.tenant_id = $1
		  AND mi.status = 'active'
		  AND mi.namespace_id = ANY($2::uuid[])
		  AND mi.sensitivity <= $3
		  AND (mi.expires_at IS NULL OR mi.expires_at > now())
		  AND (mi.valid_until IS NULL OR mi.valid_until > now())
		  AND (
		      to_tsvector('english', COALESCE(mi.title, '')) @@ to_tsquery('english', $4)
		   OR to_tsvector('english', COALESCE(mv.content, '')) @@ to_tsquery('english', $4)
		  )
		ORDER BY ts_rank(to_tsvector('english', COALESCE(mi.title, '') || ' ' || COALESCE(mv.content, '')), to_tsquery('english', $4)) DESC,
		         mi.authority_level DESC
		LIMIT $5
	`
	return s.queryResults(ctx, req, namespaces, sensitivityMax, limit, query, tsQuery, "fulltext")
}

func (s *server) vectorSearch(ctx context.Context, req RetrieveRequest, namespaces []string, sensitivityMax string, limit int) ([]RetrievalResult, error) {
	// Return results sorted by authority/confidence when no embedding available.
	// In production this would generate an embedding and query: embedding <-> $4
	query := `
		SELECT mi.id, mi.title, mi.canonical_summary, mv.content,
		       mi.type, mi.authority_level, mi.confidence_score, mi.sensitivity,
		       mi.namespace_id
		FROM memory_items mi
		LEFT JOIN memory_versions mv ON mv.id = mi.current_version_id
		WHERE mi.tenant_id = $1
		  AND mi.status = 'active'
		  AND mi.namespace_id = ANY($2::uuid[])
		  AND mi.sensitivity <= $3
		  AND (mi.expires_at IS NULL OR mi.expires_at > now())
		  AND (mi.valid_until IS NULL OR mi.valid_until > now())
		ORDER BY mi.confidence_score DESC, mi.authority_level DESC
		LIMIT $4
	`
	return s.queryResults(ctx, req, namespaces, sensitivityMax, limit, query, "semantic_fallback", "semantic")
}

func (s *server) recentSessionState(ctx context.Context, req RetrieveRequest, limit int) ([]RetrievalResult, error) {
	if req.SessionID == "" {
		return nil, nil
	}
	query := `
		SELECT mi.id, mi.title, mi.canonical_summary, mv.content,
		       mi.type, mi.authority_level, mi.confidence_score, mi.sensitivity,
		       mi.namespace_id
		FROM memory_items mi
		LEFT JOIN memory_versions mv ON mv.id = mi.current_version_id
		WHERE mi.tenant_id = $1
		  AND mi.status IN ('active', 'candidate')
		  AND mi.type IN ('working', 'episodic')
		  AND (mi.metadata->>'session_id' = $2 OR mi.metadata->>'session_id' IS NULL)
		ORDER BY mi.created_at DESC
		LIMIT $3
	`
	rows, err := s.db.Query(ctx, query, req.TenantID, req.SessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []RetrievalResult
	for rows.Next() {
		var r RetrievalResult
		var content, sensitivity string
		if err := rows.Scan(&r.MemoryID, &r.Title, &r.Summary, &content,
			&r.MemoryClass, &r.Authority, &r.Confidence, &sensitivity,
			&r.NamespaceID); err != nil {
			return nil, err
		}
		r.Sensitivity = sensitivity
		r.Content = truncate(content, 2000)
		r.Provenance = "session_state"
		results = append(results, r)
	}
	return results, rows.Err()
}

// ── query helper ─────────────────────────────────────────────────────────

func (s *server) queryResults(ctx context.Context, req RetrieveRequest, namespaces []string, sensitivityMax string, limit int, query string, queryParam string, provenance string) ([]RetrievalResult, error) {
	// Convert namespaces to UUIDs
	nsRows, err := s.db.Query(ctx, `SELECT id FROM namespaces WHERE tenant_id = $1 AND canonical_name = ANY($2)`, req.TenantID, namespaces)
	if err != nil {
		return nil, err
	}
	defer nsRows.Close()

	var nsUUIDs []string
	for nsRows.Next() {
		var id string
		if err := nsRows.Scan(&id); err != nil {
			return nil, err
		}
		nsUUIDs = append(nsUUIDs, id)
	}
	if nsRows.Err() != nil {
		return nil, nsRows.Err()
	}

	if len(nsUUIDs) == 0 {
		return nil, nil
	}

	pgRows, err := s.db.Query(ctx, query, req.TenantID, nsUUIDs, sensitivityMax, queryParam, limit)
	if err != nil {
		return nil, err
	}
	defer pgRows.Close()

	var results []RetrievalResult
	for pgRows.Next() {
		var r RetrievalResult
		var content, sensitivity string
		if err := pgRows.Scan(&r.MemoryID, &r.Title, &r.Summary, &content,
			&r.MemoryClass, &r.Authority, &r.Confidence, &sensitivity,
			&r.NamespaceID); err != nil {
			return nil, err
		}
		r.Sensitivity = sensitivity
		r.Content = truncate(content, 2000)
		r.Provenance = provenance
		results = append(results, r)
	}
	return results, pgRows.Err()
}

// ── fusion ───────────────────────────────────────────────────────────────

type scoredResult struct {
	result     RetrievalResult
	rankScores map[string]float64 // source -> RRF score
}

func fuseResults(k float64, sets ...[]RetrievalResult) []RetrievalResult {
	seen := make(map[string]*scoredResult)
	allSources := []string{"exact", "fulltext", "semantic", "session_state"}

	for sourceIdx, results := range sets {
		sourceName := allSources[sourceIdx]
		for rank, r := range results {
			sr, exists := seen[r.MemoryID]
			if !exists {
				sr = &scoredResult{
					result:     r,
					rankScores: make(map[string]float64),
				}
				seen[r.MemoryID] = sr
			}
			// RRF: 1 / (k + rank)
			sr.rankScores[sourceName] = 1.0 / (k + float64(rank))
		}
	}

	// Compute fused score: sum of RRF scores across sources
	var fused []RetrievalResult
	for _, sr := range seen {
		var total float64
		for _, score := range sr.rankScores {
			total += score
		}
		sr.result.FusedScore = total

		// Break down per-source scores
		if src, ok := sr.rankScores["exact"]; ok {
			sr.result.LexicalScore = src
		}
		if src, ok := sr.rankScores["semantic"]; ok {
			sr.result.SemanticScore = src
		} else if src, ok := sr.rankScores["fulltext"]; ok {
			// Use fulltext as lexical score if no exact match
			sr.result.LexicalScore = src
		}

		fused = append(fused, sr.result)
	}

	sort.Slice(fused, func(i, j int) bool {
		return fused[i].FusedScore > fused[j].FusedScore
	})
	for i := range fused {
		fused[i].Rank = i + 1
	}
	return fused
}

// ── dedup ────────────────────────────────────────────────────────────────

func deduplicateResults(results []RetrievalResult) []RetrievalResult {
	if len(results) == 0 {
		return results
	}
	seen := make(map[string]bool)
	var deduped []RetrievalResult
	for _, r := range results {
		if seen[r.MemoryID] {
			continue
		}
		seen[r.MemoryID] = true
		deduped = append(deduped, r)
	}
	return deduped
}

// ── token budget ─────────────────────────────────────────────────────────

func allocateByTokenBudget(results []RetrievalResult, budget int) []RetrievalResult {
	var allocated []RetrievalResult
	used := 0
	for _, r := range results {
		tokens := estimateTokens(r.Content)
		if used+tokens > budget {
			remaining := budget - used
			if remaining > 50 {
				r.Content = truncateByTokens(r.Content, remaining)
				allocated = append(allocated, r)
			}
			break
		}
		used += tokens
		allocated = append(allocated, r)
	}
	return allocated
}

// ── section assembly ─────────────────────────────────────────────────────

func assembleSections(results []RetrievalResult) []ContextSection {
	sections := []ContextSection{
		{Heading: "Governance"},
		{Heading: "Active Task State"},
		{Heading: "Confirmed Facts"},
		{Heading: "Prior Episodes"},
		{Heading: "Procedures"},
		{Heading: "Conflicts"},
		{Heading: "Source References"},
	}

	for _, r := range results {
		switch r.MemoryClass {
		case "governing":
			r.Provenance = "governance"
			sections[0].Items = append(sections[0].Items, r)
		case "working":
			r.Provenance = "working_state"
			sections[1].Items = append(sections[1].Items, r)
		case "episodic":
			r.Provenance = "episodic"
			sections[3].Items = append(sections[3].Items, r)
		case "procedural":
			r.Provenance = "procedural"
			sections[4].Items = append(sections[4].Items, r)
		default:
			r.Provenance = "semantic"
			sections[2].Items = append(sections[2].Items, r)
		}
	}

	// Remove empty sections
	var nonEmpty []ContextSection
	for _, sec := range sections {
		if len(sec.Items) > 0 {
			nonEmpty = append(nonEmpty, sec)
		}
	}
	return nonEmpty
}

// ── persistence ──────────────────────────────────────────────────────────

func (s *server) recordRetrieval(ctx context.Context, req RetrieveRequest, queryHash string, namespaces []string, sections []ContextSection, latencyMs int) (string, error) {
	// Collect selected memory IDs from all sections
	var selectedIDs []string
	for _, sec := range sections {
		for _, item := range sec.Items {
			selectedIDs = append(selectedIDs, item.MemoryID)
		}
	}
	selectedJSON, _ := json.Marshal(selectedIDs)

	var runID string
	err := s.db.QueryRow(ctx, `
		INSERT INTO retrieval_runs
			(tenant_id, principal_id, agent_id, session_id, turn_id,
			 query_hash, namespaces, filters, ranking_config, selected_memory_ids,
			 latency_ms)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		RETURNING id
	`, req.TenantID, req.PrincipalID, req.AgentID, req.SessionID, req.TurnID,
		queryHash, toJSONB(namespaces), toJSONB(req.Filters),
		`{"fusion":"rrf","k":60}`, selectedJSON, latencyMs).Scan(&runID)
	if err != nil {
		return "", err
	}

	// Record individual results
	for rank, item := range selectedIDs {
		s.db.Exec(ctx, `
			INSERT INTO retrieval_results
				(retrieval_run_id, memory_id, rank, fused_score, provenance)
			VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT DO NOTHING
		`, runID, item, rank+1, 0.0, "retrieval")
	}

	return runID, nil
}

// ── utility functions ────────────────────────────────────────────────────

func hashQuery(q string) string {
	h := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(q))))
	return hex.EncodeToString(h[:])
}

func toTSQuery(input string) string {
	words := strings.Fields(strings.ToLower(input))
	var parts []string
	for _, w := range words {
		w = strings.Trim(w, ".,!?;:'\"()[]{}")
		if w == "" || len(w) > 50 {
			continue
		}
		parts = append(parts, w+":*")
	}
	return strings.Join(parts, " & ")
}

func estimateTokens(text string) int {
	// Rough estimate: ~4 chars per token
	return len(text) / 4
}

func truncate(text string, maxLen int) string {
	if len(text) <= maxLen {
		return text
	}
	return text[:maxLen] + "..."
}

func truncateByTokens(text string, maxTokens int) string {
	maxLen := maxTokens * 4
	return truncate(text, maxLen)
}

func toJSONB(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("encode response: %v", err)
	}
}

// Ensure float64 conversion doesn't panic on large ints
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func init() {
	// Override math.Min for ints

}
