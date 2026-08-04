package main

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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dylmarriner/scos-memory/packages/database"
	"github.com/dylmarriner/scos-memory/packages/telemetry"
	"github.com/dylmarriner/scos-memory/services/readiness"
	"github.com/jackc/pgx/v5/pgxpool"
)

// serviceMetrics emits this service's §32 measures. It is nil-safe and a
// no-op when no collector is configured.
var serviceMetrics = telemetry.FromEnvironment("retrieval-orchestrator")

// ── request / response types ─────────────────────────────────────────────

type RetrieveRequest struct {
	TenantID       string            `json:"tenant_id"`
	PrincipalID    string            `json:"principal_id"`
	Query          string            `json:"query"`
	QueryEmbedding []float64         `json:"query_embedding,omitempty"`
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

func (s *server) readinessCheck() error {
	if os.Getenv("DATABASE_URL") == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}
	if s.db == nil {
		return fmt.Errorf("postgres dependency is not configured")
	}
	return s.db.Ping(context.Background())
}

// ── constants ────────────────────────────────────────────────────────────

const defaultMaxResults = 20
const defaultTokenBudget = 4000
const rrfK = 60.0

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

// ── main ─────────────────────────────────────────────────────────────────

func main() {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		log.Fatal("DATABASE_URL is required")
	}
	if err := database.RequireTLS(databaseURL); err != nil {
		log.Fatal(err)
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
	mux.HandleFunc("/health/ready", readiness.Handler(func() error {
		return srv.readinessCheck()
	}))
	mux.HandleFunc("/v1/retrieve", srv.handleRetrieve)
	mux.HandleFunc("/v1/retrieval-runs", srv.handleRetrieve)
	mux.HandleFunc("/v1/version", handleVersion)

	addr := os.Getenv("SCOS_RETRIEVAL_HTTP_ADDR")
	if addr == "" {
		addr = ":8082"
	}
	log.Printf("retrieval orchestrator listening on %s", addr)
	if err := tlsserve.ListenAndServe(addr, auth.ControlPlaneHandler(mux, auth.MustFromEnvironment())); err != nil {
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
	if req.PrincipalID == "" {
		http.Error(w, "principal_id is required", http.StatusBadRequest)
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
		sessionResults, _ = s.recentSessionState(ctx, req, namespaces, maxResults)
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

	// §32 retrieval latency. Staleness is not emitted here: nothing in the
	// result set carries a freshness timestamp, and a hardcoded zero would be
	// a fabricated metric (§42). Index freshness is owned and emitted by the
	// index-maintenance worker, which actually measures it.
	corr := telemetry.Correlation{
		SessionID: req.SessionID,
		AgentID:   req.AgentID,
		Tenant:    req.TenantID,
	}
	serviceMetrics.Observe(telemetry.MetricRetrievalLatency, time.Since(start),
		map[string]string{"sections": strconv.Itoa(len(sections))}, corr)

	writeJSON(w, resp)
}

// ── pipeline stages ──────────────────────────────────────────────────────

func (s *server) resolveAuthorisedNamespaces(ctx context.Context, req RetrieveRequest) ([]string, error) {
	// Always resolve through membership, including caller-supplied namespaces.
	rows, err := s.db.Query(ctx, `
		SELECT n.canonical_name
		FROM namespaces n
		JOIN namespace_memberships m ON m.namespace_id = n.id
		JOIN principals p ON p.id = m.principal_id
		WHERE n.tenant_id = $1
		  AND p.tenant_id = n.tenant_id
		  AND p.disabled_at IS NULL
		  AND m.principal_id = $2
		  AND m.can_read = true
		  AND (COALESCE(cardinality($3::text[]), 0) = 0 OR n.canonical_name = ANY($3::text[]))
		ORDER BY n.canonical_name
	`, req.TenantID, req.PrincipalID, req.Namespaces)
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(req.Namespaces) > 0 && !containsAllNamespaces(req.Namespaces, namespaces) {
		return nil, nil
	}
	return namespaces, nil
}

func containsAllNamespaces(requested, authorized []string) bool {
	authorizedSet := make(map[string]struct{}, len(authorized))
	for _, namespace := range authorized {
		authorizedSet[namespace] = struct{}{}
	}
	for _, namespace := range requested {
		if _, ok := authorizedSet[namespace]; !ok {
			return false
		}
	}
	return true
}

func (s *server) exactSearch(ctx context.Context, req RetrieveRequest, namespaces []string, sensitivityMax string, limit int) ([]RetrievalResult, error) {
	query := `
		SELECT mi.id, mi.title, mi.canonical_summary, mv.content,
		       mi.type, mi.authority_level, mi.confidence_score, mi.sensitivity,
		       mi.namespace_id,
		       COALESCE((SELECT jsonb_agg(jsonb_build_object('id', e.id, 'kind', e.evidence_kind, 'reference_id', e.reference_id, 'content_hash', e.content_hash) ORDER BY e.created_at) FROM memory_evidence e WHERE e.memory_id = mi.id), '[]'::jsonb),
		       COALESCE((SELECT jsonb_agg(jsonb_build_object('relation_type', rel.relation_type, 'target_memory_id', rel.target_memory_id, 'confidence_score', rel.confidence_score) ORDER BY rel.created_at) FROM memory_relations rel WHERE rel.source_memory_id = mi.id AND rel.relation_type ILIKE '%conflict%' AND (rel.valid_from IS NULL OR rel.valid_from <= now()) AND (rel.valid_until IS NULL OR rel.valid_until > now())), '[]'::jsonb)
		FROM memory_items mi
		LEFT JOIN memory_versions mv ON mv.id = mi.current_version_id
		WHERE mi.tenant_id = $1
		  AND mi.status = 'active'
		  AND mi.namespace_id = ANY($2::uuid[])
		  AND mi.sensitivity <= $3
		AND (mi.valid_from IS NULL OR mi.valid_from <= now())
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
		       mi.namespace_id,
		       COALESCE((SELECT jsonb_agg(jsonb_build_object('id', e.id, 'kind', e.evidence_kind, 'reference_id', e.reference_id, 'content_hash', e.content_hash) ORDER BY e.created_at) FROM memory_evidence e WHERE e.memory_id = mi.id), '[]'::jsonb),
		       COALESCE((SELECT jsonb_agg(jsonb_build_object('relation_type', rel.relation_type, 'target_memory_id', rel.target_memory_id, 'confidence_score', rel.confidence_score) ORDER BY rel.created_at) FROM memory_relations rel WHERE rel.source_memory_id = mi.id AND rel.relation_type ILIKE '%conflict%' AND (rel.valid_from IS NULL OR rel.valid_from <= now()) AND (rel.valid_until IS NULL OR rel.valid_until > now())), '[]'::jsonb)
		FROM memory_items mi
		LEFT JOIN memory_versions mv ON mv.id = mi.current_version_id
		WHERE mi.tenant_id = $1
		  AND mi.status = 'active'
		  AND mi.namespace_id = ANY($2::uuid[])
		  AND mi.sensitivity <= $3
		AND (mi.valid_from IS NULL OR mi.valid_from <= now())
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
	// Without a query embedding, retain the deterministic semantic fallback.
	if len(req.QueryEmbedding) == 0 {
		return s.vectorFallback(ctx, req, namespaces, sensitivityMax, limit)
	}

	query := `
		SELECT mi.id, mi.title, mi.canonical_summary, mv.content,
		       mi.type, mi.authority_level, mi.confidence_score, mi.sensitivity,
		       mi.namespace_id,
		       1 - (embedding.embedding <=> $4::vector) AS semantic_score,
		       COALESCE((SELECT jsonb_agg(jsonb_build_object(
				'id', e.id, 'kind', e.evidence_kind, 'reference_id', e.reference_id,
				'content_hash', e.content_hash) ORDER BY e.created_at)
			FROM memory_evidence e WHERE e.memory_id = mi.id), '[]'::jsonb) AS evidence,
		       COALESCE((SELECT jsonb_agg(jsonb_build_object(
				'relation_type', rel.relation_type, 'target_memory_id', rel.target_memory_id,
				'confidence_score', rel.confidence_score) ORDER BY rel.created_at)
			FROM memory_relations rel
			WHERE rel.source_memory_id = mi.id
			  AND rel.relation_type ILIKE '%conflict%'
			  AND (rel.valid_from IS NULL OR rel.valid_from <= now())
			  AND (rel.valid_until IS NULL OR rel.valid_until > now())), '[]'::jsonb) AS conflicts
		FROM memory_items mi
		JOIN memory_versions mv ON mv.id = mi.current_version_id
		JOIN LATERAL (
			SELECT me.embedding
			FROM memory_embeddings me
			WHERE me.memory_version_id = mv.id AND me.embedding IS NOT NULL
			ORDER BY me.embedding <=> $4::vector
			LIMIT 1
		) embedding ON true
		WHERE mi.tenant_id = $1
		  AND mi.status = 'active'
		  AND mi.namespace_id = ANY($2::uuid[])
		  AND mi.sensitivity <= $3
		  AND (mi.valid_from IS NULL OR mi.valid_from <= now())
		  AND (mi.valid_until IS NULL OR mi.valid_until > now())
		  AND (mi.expires_at IS NULL OR mi.expires_at > now())
		ORDER BY embedding.embedding <=> $4::vector ASC, mi.authority_level DESC
		LIMIT $5
	`
	results, err := s.queryVectorResults(ctx, req, namespaces, sensitivityMax, limit, query, vectorLiteral(req.QueryEmbedding))
	if err == nil {
		return results, nil
	}
	// pgvector is optional at runtime. A missing extension/index or invalid
	// embedding must not make lexical and metadata retrieval unavailable.
	return s.vectorFallback(ctx, req, namespaces, sensitivityMax, limit)
}

func (s *server) vectorFallback(ctx context.Context, req RetrieveRequest, namespaces []string, sensitivityMax string, limit int) ([]RetrievalResult, error) {
	query := `
		SELECT mi.id, mi.title, mi.canonical_summary, mv.content,
		       mi.type, mi.authority_level, mi.confidence_score, mi.sensitivity,
		       mi.namespace_id,
		       COALESCE((SELECT jsonb_agg(jsonb_build_object('id', e.id, 'kind', e.evidence_kind, 'reference_id', e.reference_id, 'content_hash', e.content_hash) ORDER BY e.created_at) FROM memory_evidence e WHERE e.memory_id = mi.id), '[]'::jsonb),
		       COALESCE((SELECT jsonb_agg(jsonb_build_object('relation_type', rel.relation_type, 'target_memory_id', rel.target_memory_id, 'confidence_score', rel.confidence_score) ORDER BY rel.created_at) FROM memory_relations rel WHERE rel.source_memory_id = mi.id AND rel.relation_type ILIKE '%conflict%' AND (rel.valid_from IS NULL OR rel.valid_from <= now()) AND (rel.valid_until IS NULL OR rel.valid_until > now())), '[]'::jsonb)
		FROM memory_items mi
		LEFT JOIN memory_versions mv ON mv.id = mi.current_version_id
		WHERE mi.tenant_id = $1
		  AND mi.status = 'active'
		  AND mi.namespace_id = ANY($2::uuid[])
		  AND mi.sensitivity <= $3
		  AND (mi.valid_from IS NULL OR mi.valid_from <= now())
		  AND (mi.expires_at IS NULL OR mi.expires_at > now())
		  AND (mi.valid_until IS NULL OR mi.valid_until > now())
		ORDER BY mi.confidence_score DESC, mi.authority_level DESC
		LIMIT $5
	`
	return s.queryResults(ctx, req, namespaces, sensitivityMax, limit, query, "semantic_fallback", "semantic")
}

func vectorLiteral(values []float64) string {
	parts := make([]string, len(values))
	for i, value := range values {
		parts[i] = strconv.FormatFloat(value, 'g', -1, 64)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func (s *server) recentSessionState(ctx context.Context, req RetrieveRequest, namespaces []string, limit int) ([]RetrievalResult, error) {
	if req.SessionID == "" {
		return nil, nil
	}
	query := `
		SELECT mi.id, mi.title, mi.canonical_summary, mv.content,
		       mi.type, mi.authority_level, mi.confidence_score, mi.sensitivity,
		       mi.namespace_id,
		       COALESCE((SELECT jsonb_agg(jsonb_build_object('id', e.id, 'kind', e.evidence_kind, 'reference_id', e.reference_id, 'content_hash', e.content_hash) ORDER BY e.created_at) FROM memory_evidence e WHERE e.memory_id = mi.id), '[]'::jsonb),
		       COALESCE((SELECT jsonb_agg(jsonb_build_object('relation_type', rel.relation_type, 'target_memory_id', rel.target_memory_id, 'confidence_score', rel.confidence_score) ORDER BY rel.created_at) FROM memory_relations rel WHERE rel.source_memory_id = mi.id AND rel.relation_type ILIKE '%conflict%' AND (rel.valid_from IS NULL OR rel.valid_from <= now()) AND (rel.valid_until IS NULL OR rel.valid_until > now())), '[]'::jsonb)
		FROM memory_items mi
		LEFT JOIN memory_versions mv ON mv.id = mi.current_version_id
		WHERE mi.tenant_id = $1
		  AND mi.namespace_id IN (
			  SELECT n.id
			  FROM namespaces n
			  JOIN namespace_memberships m ON m.namespace_id = n.id
			  JOIN principals p ON p.id = m.principal_id
			  WHERE n.tenant_id = $1
			    AND p.tenant_id = n.tenant_id
			    AND p.disabled_at IS NULL
			    AND m.principal_id = $2
			    AND m.can_read = true
			    AND n.canonical_name = ANY($3::text[])
		  )
		  AND mi.status IN ('active', 'candidate')
		  AND (mi.valid_from IS NULL OR mi.valid_from <= now())
		  AND (mi.valid_until IS NULL OR mi.valid_until > now())
		  AND (mi.expires_at IS NULL OR mi.expires_at > now())
		  AND mi.type IN ('working', 'episodic')
		  AND (mi.metadata->>'session_id' = $4 OR mi.metadata->>'session_id' IS NULL)
		ORDER BY mi.created_at DESC
		LIMIT $5
	`
	rows, err := s.db.Query(ctx, query, req.TenantID, req.PrincipalID, namespaces, req.SessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []RetrievalResult
	for rows.Next() {
		var r RetrievalResult
		var content, sensitivity string
		var evidenceJSON, conflictsJSON []byte
		if err := rows.Scan(&r.MemoryID, &r.Title, &r.Summary, &content,
			&r.MemoryClass, &r.Authority, &r.Confidence, &sensitivity,
			&r.NamespaceID, &evidenceJSON, &conflictsJSON); err != nil {
			return nil, err
		}
		r.Sensitivity = sensitivity
		r.Content = truncate(content, 2000)
		decodeResultJSON(&r, evidenceJSON, conflictsJSON)
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
		var evidenceJSON, conflictsJSON []byte
		if err := pgRows.Scan(&r.MemoryID, &r.Title, &r.Summary, &content,
			&r.MemoryClass, &r.Authority, &r.Confidence, &sensitivity,
			&r.NamespaceID, &evidenceJSON, &conflictsJSON); err != nil {
			return nil, err
		}
		r.Sensitivity = sensitivity
		r.Content = truncate(content, 2000)
		decodeResultJSON(&r, evidenceJSON, conflictsJSON)
		r.Provenance = provenance
		results = append(results, r)
	}
	return results, pgRows.Err()
}

func (s *server) queryVectorResults(ctx context.Context, req RetrieveRequest, namespaces []string, sensitivityMax string, limit int, query, embedding string) ([]RetrievalResult, error) {
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
	if err := nsRows.Err(); err != nil {
		return nil, err
	}
	if len(nsUUIDs) == 0 {
		return nil, nil
	}
	rows, err := s.db.Query(ctx, query, req.TenantID, nsUUIDs, sensitivityMax, embedding, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var results []RetrievalResult
	for rows.Next() {
		var r RetrievalResult
		var content, sensitivity string
		var semanticScore float64
		var evidenceJSON, conflictsJSON []byte
		if err := rows.Scan(&r.MemoryID, &r.Title, &r.Summary, &content,
			&r.MemoryClass, &r.Authority, &r.Confidence, &sensitivity,
			&r.NamespaceID, &semanticScore, &evidenceJSON, &conflictsJSON); err != nil {
			return nil, err
		}
		r.Sensitivity = sensitivity
		r.Content = truncate(content, 2000)
		r.SemanticScore = semanticScore
		r.Provenance = "semantic"
		decodeResultJSON(&r, evidenceJSON, conflictsJSON)
		results = append(results, r)
	}
	return results, rows.Err()
}

func decodeResultJSON(result *RetrievalResult, evidenceJSON, conflictsJSON []byte) {
	_ = json.Unmarshal(evidenceJSON, &result.Evidence)
	_ = json.Unmarshal(conflictsJSON, &result.Conflicts)
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
	// Persist exactly the results that survived fusion and token allocation.
	var selectedIDs []string
	var selected []RetrievalResult
	for _, sec := range sections {
		for _, item := range sec.Items {
			selectedIDs = append(selectedIDs, item.MemoryID)
			selected = append(selected, item)
		}
	}
	selectedJSON := toJSONB(selectedIDs)

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
	for rank, item := range selected {
		provenance := toJSONB(map[string]any{"source": item.Provenance})
		_, err = s.db.Exec(ctx, `
			INSERT INTO retrieval_results
				(retrieval_run_id, memory_id, rank, lexical_score, semantic_score, fused_score, provenance, conflicts)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
			ON CONFLICT DO NOTHING
		`, runID, item.MemoryID, rank+1, item.LexicalScore, item.SemanticScore, item.FusedScore, provenance, toJSONB(item.Conflicts))
		if err != nil {
			return runID, err
		}
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
