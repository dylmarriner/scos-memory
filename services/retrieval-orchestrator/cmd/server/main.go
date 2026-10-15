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
	"unicode"

	"github.com/dylmarriner/scos-memory/packages/database"
	"github.com/dylmarriner/scos-memory/packages/release"
	"github.com/dylmarriner/scos-memory/packages/telemetry"
	"github.com/dylmarriner/scos-memory/services/readiness"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// serviceMetrics emits this service's §32 measures. It is nil-safe and a
// no-op when no collector is configured.
var serviceMetrics = telemetry.FromEnvironment("retrieval-orchestrator")

// ── request / response types ─────────────────────────────────────────────

type RetrieveRequest struct {
	TenantID            string            `json:"tenant_id"`
	PrincipalID         string            `json:"principal_id"`
	Query               string            `json:"query"`
	QueryEmbedding      []float64         `json:"query_embedding,omitempty"`
	QueryEmbeddingSpace *embeddingSpace   `json:"query_embedding_space,omitempty"`
	AgentID             string            `json:"agent_id,omitempty"`
	SessionID           string            `json:"session_id,omitempty"`
	TurnID              string            `json:"turn_id,omitempty"`
	Namespaces          []string          `json:"namespaces,omitempty"`
	MemoryClasses       []string          `json:"memory_classes,omitempty"`
	SensitivityMax      string            `json:"sensitivity_max,omitempty"`
	MaxResults          int               `json:"max_results,omitempty"`
	TokenBudget         int               `json:"token_budget,omitempty"`
	IncludeRecent       bool              `json:"include_recent,omitempty"`
	IncludeSession      bool              `json:"include_session,omitempty"`
	IncludeState        bool              `json:"include_state,omitempty"`
	Filters             map[string]string `json:"filters,omitempty"`
	RepositoryID        string            `json:"repository_id,omitempty"`
	Branch              string            `json:"branch,omitempty"`
}

type RetrievalResult struct {
	MemoryID         string  `json:"memory_id"`
	Title            string  `json:"title"`
	Summary          string  `json:"summary"`
	Content          string  `json:"content"`
	MemoryClass      string  `json:"memory_class"`
	Authority        int     `json:"authority_level"`
	Confidence       float64 `json:"confidence_score"`
	Sensitivity      string  `json:"sensitivity"`
	LexicalScore     float64 `json:"lexical_score,omitempty"`
	SemanticScore    float64 `json:"semantic_score,omitempty"`
	RawLexicalScore  float64 `json:"raw_lexical_score,omitempty"`
	RawSemanticScore float64 `json:"raw_semantic_score,omitempty"`
	RRFScore         float64 `json:"rrf_score,omitempty"`
	RerankerScore    float64 `json:"reranker_score,omitempty"`
	FusedScore       float64 `json:"fused_score"`
	Rank             int     `json:"rank"`
	Evidence         []any   `json:"evidence,omitempty"`
	Conflicts        []any   `json:"conflicts,omitempty"`
	Provenance       string  `json:"provenance"`
	NamespaceID      string  `json:"namespace_id"`
}

type ContextSection struct {
	Heading string            `json:"heading"`
	Items   []RetrievalResult `json:"items"`
}

type RetrieveResponse struct {
	RetrievalRunID string             `json:"retrieval_run_id"`
	QueryHash      string             `json:"query_hash"`
	Sections       []ContextSection   `json:"sections"`
	Code           *codeQueryResponse `json:"code,omitempty"`
	TotalTokens    int                `json:"total_tokens"`
	LatencyMs      int                `json:"latency_ms"`
	// DegradedSources names every retrieval lane that failed for this
	// request, so a database or service failure never reads as "no
	// matches"; DegradedModels names models that were unavailable.
	DegradedSources []string `json:"degraded_sources"`
	DegradedModels  []string `json:"degraded_models"`
}

// ── server state ─────────────────────────────────────────────────────────

type server struct {
	db               *pgxpool.Pool
	modelRuntime     *modelRuntimeClient
	textSpace        embeddingSpace
	rerankModel      string
	rerankRevision   string
	rerankLimit      int
	codeIntelligence *codeIntelligenceClient
}

// pgxQuerier is satisfied by both *pgxpool.Pool and pgx.Tx, letting the
// read helpers below run against either a bare pool connection or an
// existing transaction with no change to their bodies.
type pgxQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// withPrincipalContext opens a short-lived transaction, sets the RLS
// tenant/principal context and runs fn against it, then commits.
//
// namespaces and principals (joined by resolveAuthorisedNamespaces) and
// memory_items (joined by every search function below) are all under
// FORCE ROW LEVEL SECURITY. Every one of this file's queries ran via the
// bare s.db.Query/QueryRow/Exec against the pool with no transaction, so
// current_setting('scos.tenant_id'/'scos.principal_id') resolved to NULL
// and RLS silently matched zero rows on every retrieval -- not an error,
// scos_recall just always returned empty, regardless of real data. Same
// bug class, same fix, as memory-intelligence's querier/
// withPrincipalContext (see that file's comment for the full story).
func (s *server) withPrincipalContext(ctx context.Context, tenantID, principalID string, fn func(pgxQuerier) error) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT set_config('scos.tenant_id',$1,true), set_config('scos.principal_id',$2,true)`, tenantID, principalID); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
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

	pool, err := database.NewPool(ctx, databaseURL)
	if err != nil {
		log.Fatalf("connect to database: %v", err)
	}
	defer pool.Close()

	runtimeClient, err := newModelRuntimeClientFromEnvironment()
	if err != nil {
		log.Fatalf("configure model runtime: %v", err)
	}
	textSpace, err := activeTextEmbeddingSpaceFromEnvironment()
	if err != nil {
		log.Fatalf("configure text embedding space: %v", err)
	}
	rerankLimit, err := positiveEnvInt("SCOS_MODEL_RERANK_CANDIDATES", 50)
	if err != nil {
		log.Fatal(err)
	}
	if rerankLimit > 128 {
		log.Fatal("SCOS_MODEL_RERANK_CANDIDATES must be <= 128")
	}
	codeClient, err := newCodeIntelligenceClientFromEnvironment()
	if err != nil {
		log.Fatalf("configure code-intelligence client: %v", err)
	}

	srv := &server{
		db:               pool,
		modelRuntime:     runtimeClient,
		textSpace:        textSpace,
		rerankModel:      envOr("SCOS_MODEL_RERANK_MODEL", "BAAI/bge-reranker-v2-m3"),
		rerankRevision:   envOr("SCOS_MODEL_RERANK_REVISION", "main"),
		rerankLimit:      rerankLimit,
		codeIntelligence: codeClient,
	}

	mux := newRetrievalMux(srv)

	addr := os.Getenv("SCOS_RETRIEVAL_HTTP_ADDR")
	if addr == "" {
		addr = ":8082"
	}
	log.Printf("retrieval orchestrator listening on %s", addr)
	if err := tlsserve.ListenAndServe(addr, telemetry.Instrument("retrieval-orchestrator", auth.ControlPlaneHandler(mux, auth.MustFromEnvironment()))); err != nil {
		log.Fatal(err)
	}
}

// newRetrievalMux wires the routes, kept separate from main so tests can
// exercise resolveIdentity's claims/header precedence through the actual
// mux rather than by calling handlers directly.
func newRetrievalMux(s *server) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"live"}`))
	})
	mux.HandleFunc("/health/ready", readiness.Handler(func() error {
		return s.readinessCheck()
	}))
	mux.HandleFunc("/health/version", release.Handler("retrieval-orchestrator"))
	mux.HandleFunc("/v1/retrieve", s.handleRetrieve)
	mux.HandleFunc("/v1/retrieval-runs", s.handleRetrieve)
	mux.HandleFunc("/v1/version", handleVersion)
	return mux
}

// resolveIdentity determines the tenant/principal a retrieval request acts
// as. RetrieveRequest.TenantID/PrincipalID are ordinary JSON fields on a
// body that -- via mcp-gateway's scos_retrieve tool -- ultimately carries
// caller-supplied (and therefore attacker-controlled) tool-call arguments.
// Trusting them directly would let any caller read another tenant's memory
// by simply naming a different tenant_id/principal_id in the request body;
// this is the same class of spoofing F-01 closed for mcp-gateway's own
// header forwarding, just one hop further downstream and via the body
// instead of a header. Claims on an authenticated bearer token take
// precedence; the X-SCOS-Tenant-ID/X-SCOS-Principal-ID headers (set
// authoritatively by mcp-gateway's dispatchTool from the caller's own
// validated claims, never by the caller directly) are the fallback for
// direct service-to-service calls authenticated with the shared static
// token. A caller-supplied tenant/principal on the JSON body must agree
// with whichever identity was actually authenticated -- it is never used on
// its own to select whose memory to search.
func resolveIdentity(r *http.Request, bodyTenant, bodyPrincipal string) (tenantID, principalID string, status int, msg string) {
	if claims, ok := auth.CanonicalClaims(r.Context()); ok {
		if (bodyTenant != "" && bodyTenant != claims.TenantID) || (bodyPrincipal != "" && bodyPrincipal != claims.PrincipalID) {
			return "", "", http.StatusForbidden, "request identity does not match authenticated identity"
		}
		return claims.TenantID, claims.PrincipalID, 0, ""
	}
	if auth.Identity(r.Context()) != "" {
		tenantID = strings.TrimSpace(r.Header.Get("X-SCOS-Tenant-ID"))
		principalID = strings.TrimSpace(r.Header.Get("X-SCOS-Principal-ID"))
		if tenantID == "" || principalID == "" {
			return "", "", http.StatusUnauthorized, "X-SCOS-Tenant-ID and X-SCOS-Principal-ID are required"
		}
		return tenantID, principalID, 0, ""
	}
	// Reached only when no auth middleware ran in front of this handler
	// (e.g. a unit test invoking handleRetrieve directly). main() always
	// wraps routes in auth.ControlPlaneHandler, so this branch is never
	// exercised in production; it exists so the request-validation tests
	// below can still exercise the missing-field error paths without a full
	// auth context.
	if bodyTenant == "" {
		return "", "", http.StatusBadRequest, "tenant_id is required"
	}
	if bodyPrincipal == "" {
		return "", "", http.StatusBadRequest, "principal_id is required"
	}
	return bodyTenant, bodyPrincipal, 0, ""
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

	tenantID, principalID, status, msg := resolveIdentity(r, req.TenantID, req.PrincipalID)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	req.TenantID = tenantID
	req.PrincipalID = principalID
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

	if len(req.Filters) > 0 {
		http.Error(w, "filters are not supported; narrow a retrieval with namespaces, memory_classes or sensitivity_max", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	plan := planRetrieval(req, maxResults)
	meta := retrievalExecutionMeta{TextSpace: s.textSpace, PlannerMode: plan.Mode}

	// Repo-aware P4 code retrieval is independent from memory namespace
	// membership. The downstream code service independently enforces tenant
	// ownership and durable-store policy. Failure degrades this lane only.
	var codeResponse *codeQueryResponse
	if plan.CodeLimit > 0 {
		if s.codeIntelligence == nil {
			meta.DegradedSources = append(meta.DegradedSources, "code_intelligence")
			codeResponse = &codeQueryResponse{Results: []codeQueryResult{}, RetrievalMode: "unavailable", DegradedSources: []string{"code_intelligence"}}
		} else {
			var codeErr error
			codeResponse, codeErr = s.codeIntelligence.Query(ctx, req.TenantID, req.PrincipalID, req.RepositoryID, req.Branch, req.Query, plan.CodeLimit)
			if codeErr != nil {
				log.Printf("code-intelligence retrieval degraded: %v", codeErr)
				meta.DegradedSources = append(meta.DegradedSources, "code_intelligence")
				codeResponse = &codeQueryResponse{Results: []codeQueryResult{}, RetrievalMode: "unavailable", DegradedSources: []string{"code_intelligence"}}
			}
		}
	}

	// ── 1. Authorise / resolve namespaces ───────────────────────────
	namespaces, err := s.resolveAuthorisedNamespaces(ctx, req)
	if err != nil {
		http.Error(w, "authorisation error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if len(namespaces) == 0 {
		// No readable memory namespace does not erase independently-authorized
		// code evidence. Disable memory lanes and continue when code was asked for.
		plan.ExactLimit, plan.FullTextLimit, plan.VectorLimit = 0, 0, 0
		plan.RelationLimit, plan.SessionLimit = 0, 0
		if codeResponse == nil {
			writeJSON(w, RetrieveResponse{Sections: []ContextSection{}, TotalTokens: 0, LatencyMs: int(time.Since(start).Milliseconds())})
			return
		}
	}

	// ── 1.5. Standing-answer cache (P1-B) ────────────────────────────
	// A normalized-exact-match repeat of a prior question skips the whole
	// 5-source fan-out below. question_key is a lowercased/trimmed exact
	// match on query text, not a classifier -- see the migration's comment
	// for why (a fixed named-question set needs a classification step this
	// doesn't have yet).
	if plan.CodeLimit == 0 {
		if answer, err := s.lookupStandingAnswer(ctx, req.TenantID, req.PrincipalID, namespaces, req.Query); err != nil {
			log.Printf("standing answer lookup error: %v", err)
		} else if answer != nil {
			writeJSON(w, RetrieveResponse{
				Sections: []ContextSection{{
					Heading: "Standing Answer",
					Items: []RetrievalResult{{
						Summary:     answer.Answer,
						Content:     answer.Answer,
						Confidence:  answer.Confidence,
						Provenance:  "standing_answer",
						NamespaceID: answer.NamespaceID,
						Rank:        1,
					}},
				}},
				TotalTokens: estimateTokens(answer.Answer),
				LatencyMs:   int(time.Since(start).Milliseconds()),
			})
			return
		}
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
	var exactResults []RetrievalResult
	if plan.ExactLimit > 0 {
		exactResults, err = s.exactSearch(ctx, req, namespaces, sensitivityFilter, plan.ExactLimit)
		if err != nil {
			log.Printf("exact search error: %v", err)
			meta.DegradedSources = append(meta.DegradedSources, "exact")
		}
	}

	// ── 5. Full-text search ─────────────────────────────────────────
	var ftsResults []RetrievalResult
	if plan.FullTextLimit > 0 {
		ftsResults, err = s.fullTextSearch(ctx, req, namespaces, sensitivityFilter, tsQuery, plan.FullTextLimit)
		if err != nil {
			log.Printf("full-text search error: %v", err)
			meta.DegradedSources = append(meta.DegradedSources, "fulltext")
		}
	}

	// ── 6. Vector search ────────────────────────────────────────────
	if plan.VectorLimit > 0 && len(req.QueryEmbedding) > 0 {
		if req.QueryEmbeddingSpace == nil || *req.QueryEmbeddingSpace != s.textSpace {
			http.Error(w, "query_embedding_space must exactly match the active text embedding space", http.StatusBadRequest)
			return
		}
		if len(req.QueryEmbedding) != s.textSpace.Dimensions {
			http.Error(w, "query_embedding dimensions do not match the active text embedding space", http.StatusBadRequest)
			return
		}
	} else if plan.VectorLimit > 0 && s.modelRuntime != nil {
		embedded, embedErr := s.modelRuntime.EmbedText(ctx, req.Query, s.textSpace)
		if embedErr != nil {
			log.Printf("query embedding unavailable (%v); semantic lane contributes zero candidates", embedErr)
			meta.DegradedModels = append(meta.DegradedModels, "text_embedding")
		} else {
			req.QueryEmbedding = embedded.Embedding
			req.QueryEmbeddingSpace = &embedded.Space
		}
	}
	var vectorResults []RetrievalResult
	if plan.VectorLimit > 0 {
		vectorResults, err = s.vectorSearch(ctx, req, namespaces, sensitivityFilter, plan.VectorLimit)
		if err != nil {
			log.Printf("vector search error: %v; semantic lane contributes zero candidates", err)
			meta.DegradedModels = append(meta.DegradedModels, "memory_vector")
			vectorResults = nil
		}
	}

	// ── 7. Relation expansion ───────────────────────────────────────
	var relationResults []RetrievalResult
	if plan.RelationLimit > 0 {
		relationResults, err = s.relationExpansion(ctx, req, namespaces, sensitivityFilter, append(append(exactResults, ftsResults...), vectorResults...), plan.RelationLimit)
		if err != nil {
			log.Printf("relation expansion error: %v", err)
			meta.DegradedSources = append(meta.DegradedSources, "relation")
		}
	}

	// ── 8. Recent session state ─────────────────────────────────────
	var sessionResults []RetrievalResult
	if plan.SessionLimit > 0 {
		sessionResults, err = s.recentSessionState(ctx, req, namespaces, plan.SessionLimit)
		if err != nil {
			log.Printf("session state error: %v", err)
			meta.DegradedSources = append(meta.DegradedSources, "session_state")
		}
	}

	// ── 8.5. Recent memories and active task state ──────────────────
	var recentResults []RetrievalResult
	if req.IncludeRecent && len(namespaces) > 0 {
		recentResults, err = s.recentMemories(ctx, req, namespaces, sensitivityFilter, recentWindow, plan.recentLimit(maxResults))
		if err != nil {
			log.Printf("recent memories error: %v", err)
			meta.DegradedSources = append(meta.DegradedSources, "recent")
		}
	}
	var stateResults []RetrievalResult
	if req.IncludeState && len(namespaces) > 0 {
		stateResults, err = s.activeTaskState(ctx, req, namespaces, sensitivityFilter, plan.recentLimit(maxResults))
		if err != nil {
			log.Printf("task state error: %v", err)
			meta.DegradedSources = append(meta.DegradedSources, "task_state")
		}
	}

	// ── 9. Fusion via RRF ───────────────────────────────────────────
	fused := fuseResults(rrfK, exactResults, ftsResults, vectorResults, relationResults, sessionResults, recentResults, stateResults)

	// ── 10. Dedup by memory_id, then restrict to requested classes ──
	deduped := filterMemoryClasses(deduplicateResults(fused), req.MemoryClasses)

	// ── 10.5. Query-aware rerank, then governance weighting ─────────
	modelRanked := deduped
	if s.modelRuntime != nil && len(deduped) > 0 {
		var rerankErr error
		modelRanked, meta.RerankerModel, meta.RerankerRevision, rerankErr = s.rerankWithModel(ctx, req.Query, deduped)
		if rerankErr != nil {
			log.Printf("reranker unavailable (%v); using RRF + governance", rerankErr)
			meta.DegradedModels = append(meta.DegradedModels, "reranker")
			modelRanked = deduped
		}
	}
	reranked := applyGovernanceWeighting(modelRanked)

	// ── 11. Apply token budget ──────────────────────────────────────
	allocated := allocateByTokenBudget(reranked, tokenBudget)

	// ── 12. Assemble into sections per §17 ─────────────────────────
	sections := assembleSections(allocated)

	// ── 13. Record retrieval run ────────────────────────────────────
	codeCount := 0
	if codeResponse != nil {
		codeCount = len(codeResponse.Results)
	}
	retrievalRunID, err := s.recordRetrieval(ctx, req, queryHash, namespaces, sections, codeResponse, map[string]int{
		"exact": len(exactResults), "fulltext": len(ftsResults), "vector": len(vectorResults), "session_state": len(sessionResults),
		"relation": len(relationResults), "code": codeCount, "recent": len(recentResults), "task_state": len(stateResults),
	}, int(time.Since(start).Milliseconds()), meta)
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
		RetrievalRunID:  retrievalRunID,
		QueryHash:       queryHash,
		Sections:        sections,
		Code:            codeResponse,
		TotalTokens:     totalTokens,
		LatencyMs:       int(time.Since(start).Milliseconds()),
		DegradedSources: nonNil(meta.DegradedSources),
		DegradedModels:  nonNil(meta.DegradedModels),
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
	var namespaces []string
	err := s.withPrincipalContext(ctx, req.TenantID, req.PrincipalID, func(q pgxQuerier) error {
		// Always resolve through membership, including caller-supplied namespaces.
		rows, err := q.Query(ctx, `
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
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var ns string
			if err := rows.Scan(&ns); err != nil {
				return err
			}
			namespaces = append(namespaces, ns)
		}
		return rows.Err()
	})
	if err != nil {
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
		       1.0::double precision AS lexical_score,
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
		       ts_rank(to_tsvector('english', COALESCE(mi.title, '') || ' ' || COALESCE(mv.content, '')), to_tsquery('english', $4)) AS lexical_score,
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

// embeddingSpaceID resolves the exact persisted space used by this request.
// It is deliberately a metadata lookup before any pgvector operator runs, so
// rows from other dimensions/models are never candidates for a distance
// operation.
func (s *server) embeddingSpaceID(ctx context.Context, space embeddingSpace) (string, error) {
	if !space.valid() {
		return "", fmt.Errorf("invalid embedding space")
	}
	var id string
	err := s.db.QueryRow(ctx, `
		SELECT id::text FROM embedding_spaces
		WHERE role=$1 AND provider=$2 AND model=$3 AND model_revision=$4
		  AND dimensions=$5 AND generation=$6
	`, space.Role, space.Provider, space.Model, space.Revision, space.Dimensions, space.Generation).Scan(&id)
	if err == pgx.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return id, nil
}

func (s *server) vectorSearch(ctx context.Context, req RetrieveRequest, namespaces []string, sensitivityMax string, limit int) ([]RetrievalResult, error) {
	if len(req.QueryEmbedding) == 0 {
		return nil, nil
	}
	if req.QueryEmbeddingSpace == nil || *req.QueryEmbeddingSpace != s.textSpace {
		return nil, fmt.Errorf("query embedding does not identify the active text space")
	}
	spaceID, err := s.embeddingSpaceID(ctx, s.textSpace)
	if err != nil {
		return nil, fmt.Errorf("resolve active embedding space: %w", err)
	}
	if spaceID == "" {
		return nil, nil
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
			WHERE me.memory_version_id = mv.id
			  AND me.embedding_space_id = $5::uuid
			  AND me.embedding IS NOT NULL
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
		LIMIT $6
	`
	return s.queryVectorResults(ctx, req, namespaces, sensitivityMax, limit, query, vectorLiteral(req.QueryEmbedding), spaceID)
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
	var results []RetrievalResult
	err := s.withPrincipalContext(ctx, req.TenantID, req.PrincipalID, func(q pgxQuerier) error {
		rows, err := q.Query(ctx, query, req.TenantID, req.PrincipalID, namespaces, req.SessionID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r RetrievalResult
			var content, sensitivity string
			var evidenceJSON, conflictsJSON []byte
			if err := rows.Scan(&r.MemoryID, &r.Title, &r.Summary, &content,
				&r.MemoryClass, &r.Authority, &r.Confidence, &sensitivity,
				&r.NamespaceID, &evidenceJSON, &conflictsJSON); err != nil {
				return err
			}
			r.Sensitivity = sensitivity
			r.Content = truncate(content, 2000)
			decodeResultJSON(&r, evidenceJSON, conflictsJSON)
			r.Provenance = "session_state"
			results = append(results, r)
		}
		return rows.Err()
	})
	return results, err
}

// relationExpansion follows authorised memory relations from lexical and
// semantic candidates. It is deliberately bounded and applies the same
// tenant, membership, validity and sensitivity predicates as primary search.
func (s *server) relationExpansion(ctx context.Context, req RetrieveRequest, namespaces []string, sensitivityMax string, candidates []RetrievalResult, limit int) ([]RetrievalResult, error) {
	ids := make([]string, 0, len(candidates))
	seen := make(map[string]bool)
	for _, candidate := range candidates {
		if candidate.MemoryID != "" && !seen[candidate.MemoryID] {
			seen[candidate.MemoryID] = true
			ids = append(ids, candidate.MemoryID)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	var results []RetrievalResult
	err := s.withPrincipalContext(ctx, req.TenantID, req.PrincipalID, func(q pgxQuerier) error {
		rows, err := q.Query(ctx, `
			SELECT id, title, canonical_summary, content, type, authority_level, confidence_score, sensitivity, namespace_id, evidence, conflicts
			FROM (
			SELECT DISTINCT ON (mi.id) mi.id, mi.title, mi.canonical_summary, mv.content, mi.type,
			       mi.authority_level, mi.confidence_score, mi.sensitivity, mi.namespace_id,
			       COALESCE((SELECT jsonb_agg(jsonb_build_object('id', e.id, 'kind', e.evidence_kind, 'reference_id', e.reference_id, 'content_hash', e.content_hash) ORDER BY e.created_at) FROM memory_evidence e WHERE e.memory_id=mi.id),'[]'::jsonb) AS evidence,
			       COALESCE((SELECT jsonb_agg(jsonb_build_object('relation_type', r.relation_type, 'target_memory_id', r.target_memory_id, 'confidence_score', r.confidence_score) ORDER BY r.created_at) FROM memory_relations r WHERE r.source_memory_id=mi.id AND r.relation_type ILIKE '%conflict%'),'[]'::jsonb) AS conflicts,
			       array_position($3::uuid[], rel.source_memory_id) AS seed_pos
			FROM memory_relations rel
			JOIN memory_items mi ON mi.id=rel.target_memory_id
			LEFT JOIN memory_versions mv ON mv.id=mi.current_version_id
			JOIN namespaces n ON n.id=mi.namespace_id
			JOIN namespace_memberships m ON m.namespace_id=n.id AND m.principal_id=$2 AND m.can_read=true
			WHERE mi.tenant_id=$1 AND rel.source_memory_id=ANY($3::uuid[]) AND mi.status='active'
			  AND mi.sensitivity <= $4 AND (mi.valid_from IS NULL OR mi.valid_from <= now())
			  AND (mi.valid_until IS NULL OR mi.valid_until > now()) AND (mi.expires_at IS NULL OR mi.expires_at > now())
			  AND n.canonical_name=ANY($5::text[])
			  -- Expansion reaches memories the direct lanes missed. A direct
			  -- hit re-found as its neighbour's neighbour would count twice,
			  -- so mutually related weak matches outranked a strong one.
			  AND NOT (mi.id = ANY($3::uuid[]))
			ORDER BY mi.id, seed_pos
			) expanded
			ORDER BY seed_pos, authority_level DESC
			LIMIT $6`, req.TenantID, req.PrincipalID, ids, sensitivityMax, namespaces, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r RetrievalResult
			var content, sensitivity string
			var evidence, conflicts []byte
			if err := rows.Scan(&r.MemoryID, &r.Title, &r.Summary, &content, &r.MemoryClass, &r.Authority, &r.Confidence, &sensitivity, &r.NamespaceID, &evidence, &conflicts); err != nil {
				return err
			}
			r.Sensitivity = sensitivity
			r.Content = truncate(content, 2000)
			r.Provenance = "relation"
			decodeResultJSON(&r, evidence, conflicts)
			results = append(results, r)
		}
		return rows.Err()
	})
	return results, err
}

// ── query helper ─────────────────────────────────────────────────────────

func (s *server) queryResults(ctx context.Context, req RetrieveRequest, namespaces []string, sensitivityMax string, limit int, query string, queryParam string, provenance string) ([]RetrievalResult, error) {
	var results []RetrievalResult
	err := s.withPrincipalContext(ctx, req.TenantID, req.PrincipalID, func(q pgxQuerier) error {
		// Convert namespaces to UUIDs
		nsRows, err := q.Query(ctx, `SELECT id FROM namespaces WHERE tenant_id = $1 AND canonical_name = ANY($2)`, req.TenantID, namespaces)
		if err != nil {
			return err
		}
		var nsUUIDs []string
		for nsRows.Next() {
			var id string
			if err := nsRows.Scan(&id); err != nil {
				nsRows.Close()
				return err
			}
			nsUUIDs = append(nsUUIDs, id)
		}
		nsErr := nsRows.Err()
		nsRows.Close() // must close before the next query on this connection -- see withPrincipalContext's doc comment
		if nsErr != nil {
			return nsErr
		}
		if len(nsUUIDs) == 0 {
			return nil
		}

		pgRows, err := q.Query(ctx, query, req.TenantID, nsUUIDs, sensitivityMax, queryParam, limit)
		if err != nil {
			return err
		}
		defer pgRows.Close()
		for pgRows.Next() {
			var r RetrievalResult
			var content, sensitivity string
			var lexicalScore float64
			var evidenceJSON, conflictsJSON []byte
			if err := pgRows.Scan(&r.MemoryID, &r.Title, &r.Summary, &content,
				&r.MemoryClass, &r.Authority, &r.Confidence, &sensitivity,
				&r.NamespaceID, &lexicalScore, &evidenceJSON, &conflictsJSON); err != nil {
				return err
			}
			r.Sensitivity = sensitivity
			r.Content = truncate(content, 2000)
			r.RawLexicalScore = lexicalScore
			r.LexicalScore = lexicalScore
			decodeResultJSON(&r, evidenceJSON, conflictsJSON)
			r.Provenance = provenance
			results = append(results, r)
		}
		return pgRows.Err()
	})
	return results, err
}

func (s *server) queryVectorResults(ctx context.Context, req RetrieveRequest, namespaces []string, sensitivityMax string, limit int, query, embedding, spaceID string) ([]RetrievalResult, error) {
	var results []RetrievalResult
	err := s.withPrincipalContext(ctx, req.TenantID, req.PrincipalID, func(q pgxQuerier) error {
		nsRows, err := q.Query(ctx, `SELECT id FROM namespaces WHERE tenant_id = $1 AND canonical_name = ANY($2)`, req.TenantID, namespaces)
		if err != nil {
			return err
		}
		var nsUUIDs []string
		for nsRows.Next() {
			var id string
			if err := nsRows.Scan(&id); err != nil {
				nsRows.Close()
				return err
			}
			nsUUIDs = append(nsUUIDs, id)
		}
		nsErr := nsRows.Err()
		nsRows.Close()
		if nsErr != nil {
			return nsErr
		}
		if len(nsUUIDs) == 0 {
			return nil
		}
		rows, err := q.Query(ctx, query, req.TenantID, nsUUIDs, sensitivityMax, embedding, spaceID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r RetrievalResult
			var content, sensitivity string
			var semanticScore float64
			var evidenceJSON, conflictsJSON []byte
			if err := rows.Scan(&r.MemoryID, &r.Title, &r.Summary, &content,
				&r.MemoryClass, &r.Authority, &r.Confidence, &sensitivity,
				&r.NamespaceID, &semanticScore, &evidenceJSON, &conflictsJSON); err != nil {
				return err
			}
			r.Sensitivity = sensitivity
			r.Content = truncate(content, 2000)
			r.RawSemanticScore = semanticScore
			r.SemanticScore = semanticScore
			r.Provenance = "semantic"
			decodeResultJSON(&r, evidenceJSON, conflictsJSON)
			results = append(results, r)
		}
		return rows.Err()
	})
	return results, err
}

func decodeResultJSON(result *RetrievalResult, evidenceJSON, conflictsJSON []byte) {
	_ = json.Unmarshal(evidenceJSON, &result.Evidence)
	_ = json.Unmarshal(conflictsJSON, &result.Conflicts)
}

// ── fusion ───────────────────────────────────────────────────────────────

type scoredResult struct {
	result     RetrievalResult
	rankScores map[string]float64
}

func rerankResults(results []RetrievalResult) []RetrievalResult {
	return applyGovernanceWeighting(results)
}

func fuseResults(k float64, sets ...[]RetrievalResult) []RetrievalResult {
	seen := make(map[string]*scoredResult)
	allSources := []string{"exact", "fulltext", "semantic", "relation", "session_state", "recent", "task_state"}

	for sourceIdx, results := range sets {
		if sourceIdx >= len(allSources) {
			break
		}
		sourceName := allSources[sourceIdx]
		for rank, r := range results {
			sr, exists := seen[r.MemoryID]
			if !exists {
				sr = &scoredResult{result: r, rankScores: make(map[string]float64)}
				seen[r.MemoryID] = sr
			}
			if sourceName == "semantic" {
				sr.result.RawSemanticScore = r.RawSemanticScore
				sr.result.SemanticScore = r.SemanticScore
			}
			if sourceName == "exact" || sourceName == "fulltext" {
				if r.RawLexicalScore > sr.result.RawLexicalScore {
					sr.result.RawLexicalScore = r.RawLexicalScore
					sr.result.LexicalScore = r.LexicalScore
				}
			}
			sr.rankScores[sourceName] = 1.0 / (k + float64(rank))
		}
	}

	// Rank fusion keeps only order, so the first and second full-text hits
	// differ by under 2% however much better the first one matched, and the
	// governance weighting applied after fusion could then put a memory
	// that shares one query word above one that answers the question. A
	// bounded lexical-strength term -- at most one top rank's worth, scaled
	// by how strongly a memory matched relative to the best match -- keeps
	// that magnitude.
	var maxLexical float64
	for _, sr := range seen {
		if sr.result.RawLexicalScore > maxLexical {
			maxLexical = sr.result.RawLexicalScore
		}
	}
	var fused []RetrievalResult
	for _, sr := range seen {
		var total float64
		if maxLexical > 0 {
			total += (sr.result.RawLexicalScore / maxLexical) / k
		}
		var lanes []string
		for _, name := range allSources {
			if score, ok := sr.rankScores[name]; ok {
				total += score
				lanes = append(lanes, name)
			}
		}
		// Provenance is the lanes that actually found this memory, in a
		// fixed order; the section a result lands in says what class it is.
		sr.result.Provenance = strings.Join(lanes, "+")
		sr.result.RRFScore = total
		sr.result.FusedScore = total
		fused = append(fused, sr.result)
	}

	sort.Slice(fused, func(i, j int) bool { return fused[i].RRFScore > fused[j].RRFScore })
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
			sections[0].Items = append(sections[0].Items, r)
		case "working":
			sections[1].Items = append(sections[1].Items, r)
		case "episodic":
			sections[3].Items = append(sections[3].Items, r)
		case "procedural":
			sections[4].Items = append(sections[4].Items, r)
		default:
			sections[2].Items = append(sections[2].Items, r)
		}
	}

	// Remove empty sections
	nonEmpty := []ContextSection{}
	for _, sec := range sections {
		if len(sec.Items) > 0 {
			nonEmpty = append(nonEmpty, sec)
		}
	}
	return nonEmpty
}

// ── persistence ──────────────────────────────────────────────────────────

func (s *server) recordRetrieval(ctx context.Context, req RetrieveRequest, queryHash string, namespaces []string, sections []ContextSection, code *codeQueryResponse, candidateCounts map[string]int, latencyMs int, meta retrievalExecutionMeta) (string, error) {
	var selectedIDs []string
	var selected []RetrievalResult
	for _, sec := range sections {
		for _, item := range sec.Items {
			selectedIDs = append(selectedIDs, item.MemoryID)
			selected = append(selected, item)
		}
	}
	selectedJSON := toJSONB(selectedIDs)
	var selectedCodeIDs []string
	if code != nil {
		for _, item := range code.Results {
			if item.SymbolVersionID != "" {
				selectedCodeIDs = append(selectedCodeIDs, item.SymbolVersionID)
			}
		}
	}
	selectedCodeJSON := toJSONB(selectedCodeIDs)
	rankingConfig := toJSONB(map[string]any{
		"fusion": "rrf", "k": rrfK, "reranker": meta.RerankerModel,
		"governance": "authority_confidence_v1", "planner": meta.PlannerMode,
		"degraded_models":  meta.DegradedModels,
		"degraded_sources": meta.DegradedSources,
	})
	modelVersions := toJSONB(map[string]any{
		"embedding_space":   meta.TextSpace,
		"reranker_model":    meta.RerankerModel,
		"reranker_revision": meta.RerankerRevision,
		"code_embedding_space": func() any {
			if code != nil {
				return code.EmbeddingSpace
			}
			return nil
		}(),
	})

	var runID string
	err := s.withPrincipalContext(ctx, req.TenantID, req.PrincipalID, func(q pgxQuerier) error {
		if err := q.QueryRow(ctx, `
			INSERT INTO retrieval_runs
				(tenant_id, principal_id, agent_id, session_id, turn_id,
				 query_hash, namespaces, filters, ranking_config, selected_memory_ids, selected_code_symbol_version_ids,
				 latency_ms, candidate_counts, token_estimate, model_versions)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
			RETURNING id
		`, req.TenantID, req.PrincipalID, nullIfBlank(req.AgentID), nullIfBlank(req.SessionID), nullIfBlank(req.TurnID),
			queryHash, toJSONB(namespaces), toJSONB(appliedRestrictions(req)), rankingConfig, selectedJSON, selectedCodeJSON, latencyMs,
			toJSONB(candidateCounts), estimateSectionsTokens(sections), modelVersions).Scan(&runID); err != nil {
			return err
		}

		for rank, item := range selected {
			provenance := toJSONB(map[string]any{
				"source":             item.Provenance,
				"raw_lexical_score":  item.RawLexicalScore,
				"raw_semantic_score": item.RawSemanticScore,
				"rrf_score":          item.RRFScore,
				"reranker_score":     item.RerankerScore,
			})
			if _, err := q.Exec(ctx, `
				INSERT INTO retrieval_results
					(retrieval_run_id, memory_id, rank, lexical_score, semantic_score, fused_score, provenance, conflicts)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
				ON CONFLICT DO NOTHING
			`, runID, item.MemoryID, rank+1, item.RawLexicalScore, item.RawSemanticScore, item.FusedScore, provenance, toJSONB(item.Conflicts)); err != nil {
				return err
			}
		}
		return nil
	})
	return runID, err
}

func estimateSectionsTokens(sections []ContextSection) int {
	total := 0
	for _, section := range sections {
		for _, item := range section.Items {
			total += estimateTokens(item.Content)
		}
	}
	return total
}

// ── utility functions ────────────────────────────────────────────────────

func hashQuery(q string) string {
	h := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(q))))
	return hex.EncodeToString(h[:])
}

// standingAnswer is one memory_standing_answers row.
type standingAnswer struct {
	Answer      string
	Confidence  float64
	NamespaceID string
}

// lookupStandingAnswer checks the standing-answer cache (P1-B) for query,
// normalized the same way hashQuery does (lowercased/trimmed exact match),
// across every namespace the caller is authorised for. Returns nil, nil on
// a miss -- that is the expected, common outcome, not an error. On
// multiple matching namespaces (rare -- a caller with more than one
// authorised namespace that both happen to have cached the same normalized
// question), the highest-confidence row wins.
func (s *server) lookupStandingAnswer(ctx context.Context, tenantID, principalID string, namespaces []string, query string) (*standingAnswer, error) {
	questionKey := hashQuery(query)
	var answer *standingAnswer
	err := s.withPrincipalContext(ctx, tenantID, principalID, func(q pgxQuerier) error {
		rows, err := q.Query(ctx, `
			SELECT sa.answer, sa.confidence, sa.namespace_id
			FROM memory_standing_answers sa
			JOIN namespaces n ON n.id = sa.namespace_id
			WHERE sa.tenant_id = $1 AND sa.question_key = $2 AND n.canonical_name = ANY($3::text[])
			ORDER BY sa.confidence DESC
			LIMIT 1`, tenantID, questionKey, namespaces)
		if err != nil {
			return err
		}
		defer rows.Close()
		if rows.Next() {
			var a standingAnswer
			if err := rows.Scan(&a.Answer, &a.Confidence, &a.NamespaceID); err != nil {
				return err
			}
			answer = &a
		}
		return rows.Err()
	})
	return answer, err
}

// toTSQuery turns a natural-language query into a prefix-matching tsquery
// that matches any of its terms. Requiring every word ("which database do
// we use") vetoed relevant memories over filler words; ts_rank still orders
// memories that match more of the terms first. Terms are split on anything
// but letters and digits, so tsquery operators in user text ("c++", "a:b",
// "x|y") can never make the query invalid.
func toTSQuery(input string) string {
	words := strings.FieldsFunc(strings.ToLower(input), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	seen := make(map[string]bool, len(words))
	var parts []string
	for _, w := range words {
		if len(w) > 50 || seen[w] {
			continue
		}
		seen[w] = true
		parts = append(parts, w+":*")
	}
	return strings.Join(parts, " | ")
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

func nullIfBlank(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
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

// recentWindow is how far back include_recent looks.
const recentWindow = "7 days"

// recentLimit sizes the include_recent and include_state lanes: a third of
// the requested results, at least one.
func (p retrievalPlan) recentLimit(maxResults int) int {
	if n := maxResults / 3; n > 0 {
		return n
	}
	return 1
}

// recentMemories is the include_recent lane: the caller's active memories
// recorded in the last window, newest first, whatever the query says.
func (s *server) recentMemories(ctx context.Context, req RetrieveRequest, namespaces []string, sensitivityMax, window string, limit int) ([]RetrievalResult, error) {
	query := `
		SELECT mi.id, mi.title, mi.canonical_summary, mv.content,
		       mi.type, mi.authority_level, mi.confidence_score, mi.sensitivity,
		       mi.namespace_id,
		       0::double precision AS lexical_score,
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
		  AND mi.created_at >= now() - $4::interval
		ORDER BY mi.created_at DESC
		LIMIT $5
	`
	return s.queryResults(ctx, req, namespaces, sensitivityMax, limit, query, window, "recent")
}

// activeTaskState is the include_state lane: the caller's active working
// memories -- the state of tasks in progress -- most recent first.
func (s *server) activeTaskState(ctx context.Context, req RetrieveRequest, namespaces []string, sensitivityMax string, limit int) ([]RetrievalResult, error) {
	query := `
		SELECT mi.id, mi.title, mi.canonical_summary, mv.content,
		       mi.type, mi.authority_level, mi.confidence_score, mi.sensitivity,
		       mi.namespace_id,
		       0::double precision AS lexical_score,
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
		  AND mi.type::text = $4
		ORDER BY mi.created_at DESC
		LIMIT $5
	`
	return s.queryResults(ctx, req, namespaces, sensitivityMax, limit, query, "working", "task_state")
}

// filterMemoryClasses keeps only results of the requested memory classes;
// no classes requested keeps everything.
func filterMemoryClasses(results []RetrievalResult, classes []string) []RetrievalResult {
	if len(classes) == 0 {
		return results
	}
	want := make(map[string]bool, len(classes))
	for _, c := range classes {
		want[strings.ToLower(strings.TrimSpace(c))] = true
	}
	out := results[:0:0]
	for _, r := range results {
		if want[strings.ToLower(r.MemoryClass)] {
			out = append(out, r)
		}
	}
	return out
}

// appliedRestrictions is what retrieval_runs records as the run's filters:
// the restrictions this run actually applied, never fields it ignored.
func appliedRestrictions(req RetrieveRequest) map[string]any {
	sensitivity := req.SensitivityMax
	if sensitivity == "" {
		sensitivity = "internal"
	}
	out := map[string]any{"sensitivity_max": sensitivity}
	if len(req.MemoryClasses) > 0 {
		out["memory_classes"] = req.MemoryClasses
	}
	if req.IncludeRecent {
		out["include_recent"] = recentWindow
	}
	if req.IncludeState {
		out["include_state"] = true
	}
	if req.IncludeSession && req.SessionID != "" {
		out["session_id"] = req.SessionID
	}
	return out
}

func nonNil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
