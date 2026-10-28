package main

import "github.com/dylmarriner/scos-memory/packages/auth"
import "github.com/dylmarriner/scos-memory/packages/tlsserve"

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/dylmarriner/scos-memory/packages/database"
	"github.com/dylmarriner/scos-memory/packages/release"
	"github.com/dylmarriner/scos-memory/packages/strictjson"
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
	TenantID               string            `json:"tenant_id"`
	PrincipalID            string            `json:"principal_id"`
	Query                  string            `json:"query"`
	QueryEmbedding         []float64         `json:"query_embedding,omitempty"`
	QueryEmbeddingSpace    *embeddingSpace   `json:"query_embedding_space,omitempty"`
	AgentID                string            `json:"agent_id,omitempty"`
	SessionID              string            `json:"session_id,omitempty"`
	TurnID                 string            `json:"turn_id,omitempty"`
	Namespaces             []string          `json:"namespaces,omitempty"`
	MemoryClasses          []string          `json:"memory_classes,omitempty"`
	SensitivityMax         string            `json:"sensitivity_max,omitempty"`
	MaxResults             int               `json:"max_results,omitempty"`
	TokenBudget            int               `json:"token_budget,omitempty"`
	IncludeRecent          bool              `json:"include_recent,omitempty"`
	IncludeSession         bool              `json:"include_session,omitempty"`
	IncludeState           bool              `json:"include_state,omitempty"`
	IncludeStandingAnswers bool              `json:"include_standing_answers,omitempty"`
	Filters                map[string]string `json:"filters,omitempty"`
	RepositoryID           string            `json:"repository_id,omitempty"`
	Branch                 string            `json:"branch,omitempty"`
	AsOfValidTime          string            `json:"as_of_valid_time,omitempty"`
	AsOfSystemTime         string            `json:"as_of_system_time,omitempty"`
	AsOfCommit             string            `json:"as_of_commit,omitempty"`
	SnapshotID             string            `json:"snapshot_id,omitempty"`
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
	RetrievalRunID  string                   `json:"retrieval_run_id"`
	QueryHash       string                   `json:"query_hash"`
	Sections        []ContextSection         `json:"sections"`
	Code            *codeQueryResponse       `json:"code,omitempty"`
	StandingAnswers []StandingAnswerEvidence `json:"standing_answers,omitempty"`
	AsOfValidTime   string                   `json:"as_of_valid_time,omitempty"`
	TemporalScope   string                   `json:"temporal_scope,omitempty"`
	TotalTokens     int                      `json:"total_tokens"`
	LatencyMs       int                      `json:"latency_ms"`
	// DegradedSources names every retrieval lane that failed for this
	// request, so a database or service failure never reads as "no
	// matches"; DegradedModels names models that were unavailable.
	DegradedSources []string `json:"degraded_sources"`
	DegradedModels  []string `json:"degraded_models"`
}

// ── server state ─────────────────────────────────────────────────────────

type server struct {
	db             *pgxpool.Pool
	modelRuntime   *modelRuntimeClient
	textSpace      embeddingSpace
	rerankModel    string
	rerankRevision string
	rerankLimit    int
	// rerankBudget, when positive, bounds the rerank stage on its own: a
	// reranker slower than that is treated as unavailable, so the request
	// still answers in time from RRF + governance instead of waiting out its
	// caller's whole deadline (a GPU reranker scores one document at a time).
	rerankBudget time.Duration
	// rerankDocChars caps each document sent to the reranker; a cross-encoder
	// scores the leading text, and its context bounds query plus document.
	rerankDocChars   int
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
	if validAt, ok := ctx.Value(validTimeContextKey{}).(string); ok && validAt != "" {
		if _, err := tx.Exec(ctx, "SELECT set_config('scos.as_of_valid_time',$1,true)", validAt); err != nil {
			return fmt.Errorf("set memory valid-time context: %w", err)
		}
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

// The candidate count is multiplied across independent retrieval lanes.
// Keep caller-controlled work bounded before planning any database queries.
const maxRetrievalResults = 100
const maxRetrievalTokenBudget = 32768

func validateRetrievalBudgets(requestedResults, requestedTokens int) (int, int, error) {
	if requestedResults < 0 || requestedResults > maxRetrievalResults {
		return 0, 0, fmt.Errorf("max_results must be between 0 and %d", maxRetrievalResults)
	}
	if requestedTokens < 0 || requestedTokens > maxRetrievalTokenBudget {
		return 0, 0, fmt.Errorf("token_budget must be between 0 and %d", maxRetrievalTokenBudget)
	}
	if requestedResults == 0 {
		requestedResults = defaultMaxResults
	}
	if requestedTokens == 0 {
		requestedTokens = defaultTokenBudget
	}
	return requestedResults, requestedTokens, nil
}

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
	rerankBudgetMS, err := nonNegativeEnvInt("SCOS_MODEL_RERANK_BUDGET_MS", 0)
	if err != nil {
		log.Fatal(err)
	}
	rerankDocChars, err := positiveEnvInt("SCOS_MODEL_RERANK_DOCUMENT_CHARS", defaultRerankDocumentChars)
	if err != nil {
		log.Fatal(err)
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
		rerankBudget:     time.Duration(rerankBudgetMS) * time.Millisecond,
		rerankDocChars:   rerankDocChars,
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
	mux.HandleFunc("/v1/memory-state-events", s.handleMemoryStateEvents)
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
		// Static service authentication supplies no per-user claims. The
		// trusted gateway headers are authoritative, and conflicting JSON
		// selectors must be rejected rather than silently ignored.
		if (bodyTenant != "" && bodyTenant != tenantID) || (bodyPrincipal != "" && bodyPrincipal != principalID) {
			return "", "", http.StatusForbidden, "request identity does not match authenticated identity"
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
	// All retrieval results are principal-scoped governed evidence, not just
	// opt-in standing answers. Never permit shared/client cache persistence.
	w.Header().Set("Cache-Control", "no-store")

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req RetrieveRequest
	// Reject ambiguous duplicate JSON selectors at any depth, as well as
	// trailing values and unknown fields, before identity resolution.
	if err := strictjson.DecodeObject(http.MaxBytesReader(w, r.Body, 1<<20), &req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	tenantID, principalID, status, msg := resolveIdentity(r, req.TenantID, req.PrincipalID)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	req.TenantID = tenantID
	req.PrincipalID = principalID
	// Validate both temporal selectors before reporting unsupported historical
	// reconstruction. Malformed timestamps are bad requests, not a capability
	// limitation, even when the request supplies both temporal dimensions.
	if req.AsOfValidTime != "" {
		parsed, err := time.Parse(time.RFC3339Nano, req.AsOfValidTime)
		if err != nil {
			http.Error(w, "as_of_valid_time must be an RFC3339 timestamp with timezone", http.StatusBadRequest)
			return
		}
		req.AsOfValidTime = parsed.UTC().Format(time.RFC3339Nano)
	}
	if req.AsOfSystemTime != "" {
		if _, err := time.Parse(time.RFC3339Nano, req.AsOfSystemTime); err != nil {
			http.Error(w, "as_of_system_time must be an RFC3339 timestamp with timezone", http.StatusBadRequest)
			return
		}
		// System-time travel requires reconstructing historical approvals,
		// revocations and supersessions. Current rows cannot do so safely.
		http.Error(w, "as_of_system_time requires the historical belief ledger", http.StatusNotImplemented)
		return
	}
	if req.AsOfValidTime != "" {
		r = r.WithContext(withRequestedValidTime(r.Context(), req.AsOfValidTime))
	}
	if req.Query == "" {
		http.Error(w, "query is required", http.StatusBadRequest)
		return
	}
	// A current evidence cache must never answer historical or class-filtered queries.
	if req.IncludeStandingAnswers {
		if standingAnswerQuestionKey(req.Query) == "" {
			http.Error(w, "standing-answer query must be nonempty UTF-8 (max 1024 bytes)", http.StatusBadRequest)
			return
		}
		if req.AsOfValidTime != "" || req.AsOfCommit != "" || req.SnapshotID != "" || len(req.MemoryClasses) > 0 {
			http.Error(w, "standing answers support only current unfiltered memory-class retrieval", http.StatusBadRequest)
			return
		}
	}
	// Historical selectors are code-only. Do not accept them without the
	// repository/branch binding that makes the indexed generation unambiguous.
	historicalCode := req.AsOfCommit != "" || req.SnapshotID != ""
	if historicalCode && (strings.TrimSpace(req.RepositoryID) == "" || strings.TrimSpace(req.Branch) == "") {
		http.Error(w, "historical code retrieval requires repository_id and branch", http.StatusBadRequest)
		return
	}

	maxResults, tokenBudget, budgetErr := validateRetrievalBudgets(req.MaxResults, req.TokenBudget)
	if budgetErr != nil {
		http.Error(w, budgetErr.Error(), http.StatusBadRequest)
		return
	}

	if len(req.Filters) > 0 {
		http.Error(w, "filters are not supported; narrow a retrieval with namespaces, memory_classes or sensitivity_max", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	plan := planRetrieval(req, maxResults)
	// A historical code selector is explicit intent even when the natural
	// language planner classifies the query as memory-only.
	if historicalCode {
		plan.CodeLimit = maxResults
	}
	meta := retrievalExecutionMeta{TextSpace: s.textSpace, PlannerMode: plan.Mode}

	// Repo-aware P4 code retrieval is independent from memory namespace
	// membership. The downstream code service independently enforces tenant
	// ownership and durable-store policy. Failure degrades this lane only.
	var codeResponse *codeQueryResponse
	if plan.CodeLimit > 0 {
		if s.codeIntelligence == nil {
			if historicalCode {
				http.Error(w, "historical code retrieval unavailable", http.StatusBadGateway)
				return
			}
			meta.DegradedSources = append(meta.DegradedSources, "code_intelligence")
			codeResponse = &codeQueryResponse{Results: []codeQueryResult{}, RetrievalMode: "unavailable", DegradedSources: []string{"code_intelligence"}}
		} else {
			var codeErr error
			if historicalCode {
				codeResponse, codeErr = s.codeIntelligence.QueryHistorical(ctx, req.TenantID, req.PrincipalID, req.RepositoryID, req.Branch, req.Query, plan.CodeLimit, req.AsOfCommit, req.SnapshotID)
			} else {
				codeResponse, codeErr = s.codeIntelligence.Query(ctx, req.TenantID, req.PrincipalID, req.RepositoryID, req.Branch, req.Query, plan.CodeLimit)
			}
			if codeErr != nil {
				if historicalCode {
					var statusErr *codeQueryHTTPError
					if errors.As(codeErr, &statusErr) {
						log.Printf("historical code retrieval failed: code-intelligence HTTP %d", statusErr.Status)
						switch statusErr.Status {
						case http.StatusBadRequest:
							http.Error(w, "invalid historical code selector", http.StatusBadRequest)
							return
						case http.StatusNotFound:
							http.Error(w, "historical code snapshot not found", http.StatusNotFound)
							return
						case http.StatusConflict:
							http.Error(w, "historical commit is ambiguous; specify snapshot_id", http.StatusConflict)
							return
						}
					} else if errors.Is(codeErr, errHistoricalProvenanceMismatch) {
						log.Printf("historical code retrieval failed: code-intelligence provenance mismatch")
					} else {
						log.Printf("historical code retrieval failed (%T)", codeErr)
					}
					http.Error(w, "historical code retrieval unavailable", http.StatusBadGateway)
					return
				}
				log.Printf("code-intelligence retrieval degraded (%T)", codeErr)
				meta.DegradedSources = append(meta.DegradedSources, "code_intelligence")
				codeResponse = &codeQueryResponse{Results: []codeQueryResult{}, RetrievalMode: "unavailable", DegradedSources: []string{"code_intelligence"}}
			}
		}
	}

	// ── 1. Authorise / resolve namespaces ───────────────────────────
	namespaces, err := s.resolveAuthorisedNamespaces(ctx, req)
	if err != nil {
		// Database driver errors can contain connection hosts, roles and database
		// names. Do not return those details to the authenticated caller.
		log.Printf("retrieval namespace lookup failed (%T)", err)
		http.Error(w, "retrieval temporarily unavailable", http.StatusInternalServerError)
		return
	}
	if len(namespaces) == 0 {
		// No readable memory namespace does not erase independently-authorized
		// code evidence. Disable memory lanes and continue when code was asked for.
		plan.ExactLimit, plan.FullTextLimit, plan.VectorLimit = 0, 0, 0
		plan.RelationLimit, plan.SessionLimit = 0, 0
		// Even an empty successful result must pass through the normal
		// retrieval-run audit path, including the valid-time selector.
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
	} else if plan.VectorLimit > 0 && s.modelRuntime == nil {
		// No model runtime is configured: the semantic lane cannot run, and
		// the answer says so rather than quietly ranking without it.
		meta.DegradedModels = append(meta.DegradedModels, "text_embedding")
	} else if plan.VectorLimit > 0 {
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
		sessionResults, err = s.recentSessionState(ctx, req, namespaces, sensitivityFilter, plan.SessionLimit)
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
	if s.modelRuntime == nil && len(deduped) > 0 {
		meta.DegradedModels = append(meta.DegradedModels, "reranker")
	}
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

	// Code versions are evidence too: their full serialized identifiers,
	// paths and provenance must share the same caller token budget.
	codeCandidates := 0
	if codeResponse != nil {
		codeCandidates = len(codeResponse.Results)
	}
	codeResponse, codeTokens := budgetCodeEvidence(codeResponse, tokenBudget-estimateSectionsTokens(sections))

	// ── 12.5. Optional, evidence-current exact-title cache read ─────
	var standingAnswers []StandingAnswerEvidence
	standingTokens := 0
	if req.IncludeStandingAnswers && len(namespaces) > 0 {
		candidates, cacheErr := s.readStandingAnswers(ctx, req, namespaces, sensitivityFilter, maxResults)
		if cacheErr != nil {
			log.Printf("standing-answer retrieval unavailable (%T)", cacheErr)
			http.Error(w, "standing-answer retrieval unavailable", http.StatusServiceUnavailable)
			return
		}
		standingAnswers, standingTokens = selectBudgetedStandingAnswers(candidates, tokenBudget-estimateSectionsTokens(sections)-codeTokens)
	}

	// ── 13. Record retrieval run ────────────────────────────────────
	retrievalRunID, err := s.recordRetrieval(ctx, req, queryHash, namespaces, sections, codeResponse, standingAnswers, map[string]int{
		"exact": len(exactResults), "fulltext": len(ftsResults), "vector": len(vectorResults), "session_state": len(sessionResults),
		"relation": len(relationResults), "code": codeCandidates, "recent": len(recentResults), "task_state": len(stateResults),
		"standing_answers": len(standingAnswers),
	}, int(time.Since(start).Milliseconds()), meta)
	if err != nil {
		log.Printf("record retrieval run failed (%T)", err)
		// No retrieval lane may return an unaudited HTTP 200. This applies
		// to ordinary memory/code evidence and empty results as well as
		// opt-in standing answers. Never release evidence after audit failure.
		http.Error(w, "retrieval audit unavailable", http.StatusServiceUnavailable)
		return
	}

	totalTokens := standingTokens + codeTokens
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
		StandingAnswers: standingAnswers,
		AsOfValidTime:   req.AsOfValidTime,
		TemporalScope:   temporalScopeFor(req.AsOfValidTime),
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
		SELECT mi.id, mi.title, mi.canonical_summary, COALESCE(mv.content, mi.canonical_summary),
		       mi.type, mi.authority_level, mi.confidence_score, mi.sensitivity,
		       mi.namespace_id,
		       1.0::double precision AS lexical_score,
		       COALESCE((SELECT jsonb_agg(jsonb_build_object('id', e.id, 'kind', e.evidence_kind, 'reference_id', e.reference_id, 'content_hash', e.content_hash) ORDER BY e.created_at) FROM memory_evidence e WHERE e.memory_id = mi.id), '[]'::jsonb),
		       COALESCE((SELECT jsonb_agg(jsonb_build_object(
            'relation_type', rel.relation_type,
            'target_memory_id', other.id,
            'confidence_score', rel.confidence_score) ORDER BY rel.created_at)
            FROM memory_relations rel
            JOIN memory_items other ON other.id = CASE WHEN rel.source_memory_id=mi.id THEN rel.target_memory_id ELSE rel.source_memory_id END
            WHERE (rel.source_memory_id=mi.id OR rel.target_memory_id=mi.id)
              AND (rel.relation_type='contradicts' OR rel.relation_type ILIKE '%conflict%')
              AND other.tenant_id=mi.tenant_id
              AND other.status IN ('active')
              AND other.namespace_id=ANY($2::uuid[])
              AND other.sensitivity <= $3
              AND other.sensitivity <= (SELECT sensitivity_ceiling FROM namespaces WHERE id=other.namespace_id)
              AND (other.valid_from IS NULL OR other.valid_from <= COALESCE(NULLIF(current_setting('scos.as_of_valid_time',true), '')::timestamptz, now()))
              AND (other.valid_until IS NULL OR other.valid_until > COALESCE(NULLIF(current_setting('scos.as_of_valid_time',true), '')::timestamptz, now()))
              AND (other.expires_at IS NULL OR other.expires_at > now())
              AND (rel.valid_from IS NULL OR rel.valid_from <= COALESCE(NULLIF(current_setting('scos.as_of_valid_time',true), '')::timestamptz, now()))
              AND (rel.valid_until IS NULL OR rel.valid_until > COALESCE(NULLIF(current_setting('scos.as_of_valid_time',true), '')::timestamptz, now()))), '[]'::jsonb)
		FROM memory_items mi
		LEFT JOIN memory_versions mv ON mv.id = mi.current_version_id
		WHERE mi.tenant_id = $1
		  AND mi.status = 'active'
		  AND mi.namespace_id = ANY($2::uuid[])
		  AND mi.sensitivity <= $3
		AND (mi.valid_from IS NULL OR mi.valid_from <= COALESCE(NULLIF(current_setting('scos.as_of_valid_time',true), '')::timestamptz, now()))
		  AND (mi.expires_at IS NULL OR mi.expires_at > now())
		  AND (mi.valid_until IS NULL OR mi.valid_until > COALESCE(NULLIF(current_setting('scos.as_of_valid_time',true), '')::timestamptz, now()))
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
		SELECT mi.id, mi.title, mi.canonical_summary, COALESCE(mv.content, mi.canonical_summary),
		       mi.type, mi.authority_level, mi.confidence_score, mi.sensitivity,
		       mi.namespace_id,
		       ts_rank(to_tsvector('english', COALESCE(mi.title, '') || ' ' || COALESCE(mv.content, '')), to_tsquery('english', $4)) AS lexical_score,
		       COALESCE((SELECT jsonb_agg(jsonb_build_object('id', e.id, 'kind', e.evidence_kind, 'reference_id', e.reference_id, 'content_hash', e.content_hash) ORDER BY e.created_at) FROM memory_evidence e WHERE e.memory_id = mi.id), '[]'::jsonb),
		       COALESCE((SELECT jsonb_agg(jsonb_build_object(
            'relation_type', rel.relation_type,
            'target_memory_id', other.id,
            'confidence_score', rel.confidence_score) ORDER BY rel.created_at)
            FROM memory_relations rel
            JOIN memory_items other ON other.id = CASE WHEN rel.source_memory_id=mi.id THEN rel.target_memory_id ELSE rel.source_memory_id END
            WHERE (rel.source_memory_id=mi.id OR rel.target_memory_id=mi.id)
              AND (rel.relation_type='contradicts' OR rel.relation_type ILIKE '%conflict%')
              AND other.tenant_id=mi.tenant_id
              AND other.status IN ('active')
              AND other.namespace_id=ANY($2::uuid[])
              AND other.sensitivity <= $3
              AND other.sensitivity <= (SELECT sensitivity_ceiling FROM namespaces WHERE id=other.namespace_id)
              AND (other.valid_from IS NULL OR other.valid_from <= COALESCE(NULLIF(current_setting('scos.as_of_valid_time',true), '')::timestamptz, now()))
              AND (other.valid_until IS NULL OR other.valid_until > COALESCE(NULLIF(current_setting('scos.as_of_valid_time',true), '')::timestamptz, now()))
              AND (other.expires_at IS NULL OR other.expires_at > now())
              AND (rel.valid_from IS NULL OR rel.valid_from <= COALESCE(NULLIF(current_setting('scos.as_of_valid_time',true), '')::timestamptz, now()))
              AND (rel.valid_until IS NULL OR rel.valid_until > COALESCE(NULLIF(current_setting('scos.as_of_valid_time',true), '')::timestamptz, now()))), '[]'::jsonb)
		FROM memory_items mi
		LEFT JOIN memory_versions mv ON mv.id = mi.current_version_id
		WHERE mi.tenant_id = $1
		  AND mi.status = 'active'
		  AND mi.namespace_id = ANY($2::uuid[])
		  AND mi.sensitivity <= $3
		AND (mi.valid_from IS NULL OR mi.valid_from <= COALESCE(NULLIF(current_setting('scos.as_of_valid_time',true), '')::timestamptz, now()))
		  AND (mi.expires_at IS NULL OR mi.expires_at > now())
		  AND (mi.valid_until IS NULL OR mi.valid_until > COALESCE(NULLIF(current_setting('scos.as_of_valid_time',true), '')::timestamptz, now()))
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
		SELECT mi.id, mi.title, mi.canonical_summary, COALESCE(mv.content, mi.canonical_summary),
		       mi.type, mi.authority_level, mi.confidence_score, mi.sensitivity,
		       mi.namespace_id,
		       1 - (embedding.embedding <=> $4::vector) AS semantic_score,
		       COALESCE((SELECT jsonb_agg(jsonb_build_object(
				'id', e.id, 'kind', e.evidence_kind, 'reference_id', e.reference_id,
				'content_hash', e.content_hash) ORDER BY e.created_at)
			FROM memory_evidence e WHERE e.memory_id = mi.id), '[]'::jsonb) AS evidence,
		       COALESCE((SELECT jsonb_agg(jsonb_build_object(
				'relation_type', rel.relation_type, 'target_memory_id', other.id,
				'confidence_score', rel.confidence_score) ORDER BY rel.created_at)
			FROM memory_relations rel
            JOIN memory_items other ON other.id = CASE WHEN rel.source_memory_id=mi.id THEN rel.target_memory_id ELSE rel.source_memory_id END
			WHERE (rel.source_memory_id = mi.id OR rel.target_memory_id = mi.id)
			  AND (rel.relation_type = 'contradicts' OR rel.relation_type ILIKE '%conflict%')
              AND other.tenant_id=mi.tenant_id AND other.status='active'
              AND other.namespace_id=ANY($2::uuid[]) AND other.sensitivity <= $3
              AND other.sensitivity <= (SELECT sensitivity_ceiling FROM namespaces WHERE id=other.namespace_id)
              AND (other.valid_from IS NULL OR other.valid_from <= COALESCE(NULLIF(current_setting('scos.as_of_valid_time',true), '')::timestamptz, now()))
              AND (other.valid_until IS NULL OR other.valid_until > COALESCE(NULLIF(current_setting('scos.as_of_valid_time',true), '')::timestamptz, now()))
              AND (other.expires_at IS NULL OR other.expires_at > now())
			  AND (rel.valid_from IS NULL OR rel.valid_from <= COALESCE(NULLIF(current_setting('scos.as_of_valid_time',true), '')::timestamptz, now()))
			  AND (rel.valid_until IS NULL OR rel.valid_until > COALESCE(NULLIF(current_setting('scos.as_of_valid_time',true), '')::timestamptz, now()))), '[]'::jsonb) AS conflicts
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
		  AND (mi.valid_from IS NULL OR mi.valid_from <= COALESCE(NULLIF(current_setting('scos.as_of_valid_time',true), '')::timestamptz, now()))
		  AND (mi.valid_until IS NULL OR mi.valid_until > COALESCE(NULLIF(current_setting('scos.as_of_valid_time',true), '')::timestamptz, now()))
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

// recentSessionState returns this session's working and episodic memories,
// including its own not-yet-reviewed candidates, within the caller's
// sensitivity ceiling. Memories recorded without a session are not part of
// any session's state.
func (s *server) recentSessionState(ctx context.Context, req RetrieveRequest, namespaces []string, sensitivityFilter string, limit int) ([]RetrievalResult, error) {
	if req.SessionID == "" {
		return nil, nil
	}
	query := `
		SELECT mi.id, mi.title, mi.canonical_summary, COALESCE(mv.content, mi.canonical_summary),
		       mi.type, mi.authority_level, mi.confidence_score, mi.sensitivity,
		       mi.namespace_id,
		       COALESCE((SELECT jsonb_agg(jsonb_build_object('id', e.id, 'kind', e.evidence_kind, 'reference_id', e.reference_id, 'content_hash', e.content_hash) ORDER BY e.created_at) FROM memory_evidence e WHERE e.memory_id = mi.id), '[]'::jsonb),
		       COALESCE((SELECT jsonb_agg(jsonb_build_object(
            'relation_type', rel.relation_type,
            'target_memory_id', other.id,
            'confidence_score', rel.confidence_score) ORDER BY rel.created_at)
            FROM memory_relations rel
            JOIN memory_items other ON other.id = CASE WHEN rel.source_memory_id=mi.id THEN rel.target_memory_id ELSE rel.source_memory_id END
            WHERE (rel.source_memory_id=mi.id OR rel.target_memory_id=mi.id)
              AND (rel.relation_type='contradicts' OR rel.relation_type ILIKE '%conflict%')
              AND other.tenant_id=mi.tenant_id
              AND other.status IN ('active', 'candidate')
              AND other.namespace_id IN (SELECT n.id FROM namespaces n JOIN namespace_memberships m ON m.namespace_id=n.id AND m.principal_id=$2 AND m.can_read WHERE n.tenant_id=$1 AND n.canonical_name=ANY($3::text[]))
              AND other.sensitivity <= $6
              AND other.sensitivity <= (SELECT sensitivity_ceiling FROM namespaces WHERE id=other.namespace_id)
              AND (other.valid_from IS NULL OR other.valid_from <= COALESCE(NULLIF(current_setting('scos.as_of_valid_time',true), '')::timestamptz, now()))
              AND (other.valid_until IS NULL OR other.valid_until > COALESCE(NULLIF(current_setting('scos.as_of_valid_time',true), '')::timestamptz, now()))
              AND (other.expires_at IS NULL OR other.expires_at > now())
              AND (rel.valid_from IS NULL OR rel.valid_from <= COALESCE(NULLIF(current_setting('scos.as_of_valid_time',true), '')::timestamptz, now()))
              AND (rel.valid_until IS NULL OR rel.valid_until > COALESCE(NULLIF(current_setting('scos.as_of_valid_time',true), '')::timestamptz, now()))), '[]'::jsonb)
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
		  AND (mi.valid_from IS NULL OR mi.valid_from <= COALESCE(NULLIF(current_setting('scos.as_of_valid_time',true), '')::timestamptz, now()))
		  AND (mi.valid_until IS NULL OR mi.valid_until > COALESCE(NULLIF(current_setting('scos.as_of_valid_time',true), '')::timestamptz, now()))
		  AND (mi.expires_at IS NULL OR mi.expires_at > now())
		  AND mi.type IN ('working', 'episodic')
		  AND mi.metadata->>'session_id' = $4
		  AND mi.sensitivity <= $6
		ORDER BY mi.created_at DESC
		LIMIT $5
	`
	var results []RetrievalResult
	err := s.withPrincipalContext(ctx, req.TenantID, req.PrincipalID, func(q pgxQuerier) error {
		rows, err := q.Query(ctx, query, req.TenantID, req.PrincipalID, namespaces, req.SessionID, limit, sensitivityFilter)
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
			SELECT DISTINCT ON (mi.id) mi.id, mi.title, mi.canonical_summary, COALESCE(mv.content, mi.canonical_summary) AS content, mi.type,
			       mi.authority_level, mi.confidence_score, mi.sensitivity, mi.namespace_id,
			       COALESCE((SELECT jsonb_agg(jsonb_build_object('id', e.id, 'kind', e.evidence_kind, 'reference_id', e.reference_id, 'content_hash', e.content_hash) ORDER BY e.created_at) FROM memory_evidence e WHERE e.memory_id=mi.id),'[]'::jsonb) AS evidence,
			       COALESCE((SELECT jsonb_agg(jsonb_build_object(
            'relation_type', r.relation_type,
            'target_memory_id', other.id,
            'confidence_score', r.confidence_score) ORDER BY r.created_at)
            FROM memory_relations r
            JOIN memory_items other ON other.id = CASE WHEN r.source_memory_id=mi.id THEN r.target_memory_id ELSE r.source_memory_id END
            WHERE (r.source_memory_id=mi.id OR r.target_memory_id=mi.id)
              AND (r.relation_type='contradicts' OR r.relation_type ILIKE '%conflict%')
              AND other.tenant_id=mi.tenant_id
              AND other.status IN ('active')
              AND other.namespace_id IN (SELECT n.id FROM namespaces n JOIN namespace_memberships m ON m.namespace_id=n.id AND m.principal_id=$2 AND m.can_read WHERE n.tenant_id=$1 AND n.canonical_name=ANY($5::text[]))
              AND other.sensitivity <= $4
              AND other.sensitivity <= (SELECT sensitivity_ceiling FROM namespaces WHERE id=other.namespace_id)
              AND (other.valid_from IS NULL OR other.valid_from <= COALESCE(NULLIF(current_setting('scos.as_of_valid_time',true), '')::timestamptz, now()))
              AND (other.valid_until IS NULL OR other.valid_until > COALESCE(NULLIF(current_setting('scos.as_of_valid_time',true), '')::timestamptz, now()))
              AND (other.expires_at IS NULL OR other.expires_at > now())
              AND (r.valid_from IS NULL OR r.valid_from <= COALESCE(NULLIF(current_setting('scos.as_of_valid_time',true), '')::timestamptz, now()))
              AND (r.valid_until IS NULL OR r.valid_until > COALESCE(NULLIF(current_setting('scos.as_of_valid_time',true), '')::timestamptz, now()))), '[]'::jsonb) AS conflicts,
			       array_position($3::uuid[], rel.source_memory_id) AS seed_pos
			FROM memory_relations rel
			JOIN memory_items mi ON mi.id=rel.target_memory_id
			LEFT JOIN memory_versions mv ON mv.id=mi.current_version_id
			JOIN namespaces n ON n.id=mi.namespace_id
			JOIN namespace_memberships m ON m.namespace_id=n.id AND m.principal_id=$2 AND m.can_read=true
			WHERE mi.tenant_id=$1 AND rel.source_memory_id=ANY($3::uuid[]) AND mi.status='active'
                AND (rel.valid_from IS NULL OR rel.valid_from <= COALESCE(NULLIF(current_setting('scos.as_of_valid_time',true), '')::timestamptz, now()))
                AND (rel.valid_until IS NULL OR rel.valid_until > COALESCE(NULLIF(current_setting('scos.as_of_valid_time',true), '')::timestamptz, now()))
			  AND mi.sensitivity <= $4 AND (mi.valid_from IS NULL OR mi.valid_from <= COALESCE(NULLIF(current_setting('scos.as_of_valid_time',true), '')::timestamptz, now()))
			  AND (mi.valid_until IS NULL OR mi.valid_until > COALESCE(NULLIF(current_setting('scos.as_of_valid_time',true), '')::timestamptz, now())) AND (mi.expires_at IS NULL OR mi.expires_at > now())
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

func (s *server) recordRetrieval(ctx context.Context, req RetrieveRequest, queryHash string, namespaces []string, sections []ContextSection, code *codeQueryResponse, standing []StandingAnswerEvidence, candidateCounts map[string]int, latencyMs int, meta retrievalExecutionMeta) (string, error) {
	var selectedIDs []string
	var selected []RetrievalResult
	for _, sec := range sections {
		for _, item := range sec.Items {
			selectedIDs = append(selectedIDs, item.MemoryID)
			selected = append(selected, item)
		}
	}
	selectedJSON := toJSONB(selectedIDs)
	selectedCodeIDs := make([]string, 0)
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

	filters := appliedRestrictions(req)
	if req.IncludeStandingAnswers {
		evidence := make([]map[string]any, 0, len(standing))
		for _, a := range standing {
			digest := sha256.Sum256([]byte(a.Answer))
			evidence = append(evidence, map[string]any{
				"id": a.ID, "namespace_id": a.NamespaceID,
				"evidence_fact_ids": a.EvidenceFactIDs,
				"answer_sha256":     hex.EncodeToString(digest[:]),
				"last_refreshed_at": a.LastRefreshedAt.UTC().Format(time.RFC3339Nano),
			})
		}
		filters["include_standing_answers"] = true
		filters["selected_standing_answers"] = evidence
	}

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
			queryHash, toJSONB(namespaces), toJSONB(filters), rankingConfig, selectedJSON, selectedCodeJSON, latencyMs,
			toJSONB(candidateCounts), estimateSectionsTokens(sections)+codeEvidenceTokens(code)+standingAnswerAuditTokens(standing), modelVersions).Scan(&runID); err != nil {
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

// truncate cuts text to at most maxLen bytes, never inside a UTF-8
// character, and marks the cut.
func truncate(text string, maxLen int) string {
	if len(text) <= maxLen {
		return text
	}
	for maxLen > 0 && !utf8.RuneStart(text[maxLen]) {
		maxLen--
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
		SELECT mi.id, mi.title, mi.canonical_summary, COALESCE(mv.content, mi.canonical_summary),
		       mi.type, mi.authority_level, mi.confidence_score, mi.sensitivity,
		       mi.namespace_id,
		       0::double precision AS lexical_score,
		       COALESCE((SELECT jsonb_agg(jsonb_build_object('id', e.id, 'kind', e.evidence_kind, 'reference_id', e.reference_id, 'content_hash', e.content_hash) ORDER BY e.created_at) FROM memory_evidence e WHERE e.memory_id = mi.id), '[]'::jsonb),
		       COALESCE((SELECT jsonb_agg(jsonb_build_object(
            'relation_type', rel.relation_type,
            'target_memory_id', other.id,
            'confidence_score', rel.confidence_score) ORDER BY rel.created_at)
            FROM memory_relations rel
            JOIN memory_items other ON other.id = CASE WHEN rel.source_memory_id=mi.id THEN rel.target_memory_id ELSE rel.source_memory_id END
            WHERE (rel.source_memory_id=mi.id OR rel.target_memory_id=mi.id)
              AND (rel.relation_type='contradicts' OR rel.relation_type ILIKE '%conflict%')
              AND other.tenant_id=mi.tenant_id
              AND other.status IN ('active')
              AND other.namespace_id=ANY($2::uuid[])
              AND other.sensitivity <= $3
              AND other.sensitivity <= (SELECT sensitivity_ceiling FROM namespaces WHERE id=other.namespace_id)
              AND (other.valid_from IS NULL OR other.valid_from <= COALESCE(NULLIF(current_setting('scos.as_of_valid_time',true), '')::timestamptz, now()))
              AND (other.valid_until IS NULL OR other.valid_until > COALESCE(NULLIF(current_setting('scos.as_of_valid_time',true), '')::timestamptz, now()))
              AND (other.expires_at IS NULL OR other.expires_at > now())
              AND (rel.valid_from IS NULL OR rel.valid_from <= COALESCE(NULLIF(current_setting('scos.as_of_valid_time',true), '')::timestamptz, now()))
              AND (rel.valid_until IS NULL OR rel.valid_until > COALESCE(NULLIF(current_setting('scos.as_of_valid_time',true), '')::timestamptz, now()))), '[]'::jsonb)
		FROM memory_items mi
		LEFT JOIN memory_versions mv ON mv.id = mi.current_version_id
		WHERE mi.tenant_id = $1
		  AND mi.status = 'active'
		  AND mi.namespace_id = ANY($2::uuid[])
		  AND mi.sensitivity <= $3
		  AND (mi.valid_from IS NULL OR mi.valid_from <= COALESCE(NULLIF(current_setting('scos.as_of_valid_time',true), '')::timestamptz, now()))
		  AND (mi.expires_at IS NULL OR mi.expires_at > now())
		  AND (mi.valid_until IS NULL OR mi.valid_until > COALESCE(NULLIF(current_setting('scos.as_of_valid_time',true), '')::timestamptz, now()))
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
		SELECT mi.id, mi.title, mi.canonical_summary, COALESCE(mv.content, mi.canonical_summary),
		       mi.type, mi.authority_level, mi.confidence_score, mi.sensitivity,
		       mi.namespace_id,
		       0::double precision AS lexical_score,
		       COALESCE((SELECT jsonb_agg(jsonb_build_object('id', e.id, 'kind', e.evidence_kind, 'reference_id', e.reference_id, 'content_hash', e.content_hash) ORDER BY e.created_at) FROM memory_evidence e WHERE e.memory_id = mi.id), '[]'::jsonb),
		       COALESCE((SELECT jsonb_agg(jsonb_build_object(
            'relation_type', rel.relation_type,
            'target_memory_id', other.id,
            'confidence_score', rel.confidence_score) ORDER BY rel.created_at)
            FROM memory_relations rel
            JOIN memory_items other ON other.id = CASE WHEN rel.source_memory_id=mi.id THEN rel.target_memory_id ELSE rel.source_memory_id END
            WHERE (rel.source_memory_id=mi.id OR rel.target_memory_id=mi.id)
              AND (rel.relation_type='contradicts' OR rel.relation_type ILIKE '%conflict%')
              AND other.tenant_id=mi.tenant_id
              AND other.status IN ('active')
              AND other.namespace_id=ANY($2::uuid[])
              AND other.sensitivity <= $3
              AND other.sensitivity <= (SELECT sensitivity_ceiling FROM namespaces WHERE id=other.namespace_id)
              AND (other.valid_from IS NULL OR other.valid_from <= COALESCE(NULLIF(current_setting('scos.as_of_valid_time',true), '')::timestamptz, now()))
              AND (other.valid_until IS NULL OR other.valid_until > COALESCE(NULLIF(current_setting('scos.as_of_valid_time',true), '')::timestamptz, now()))
              AND (other.expires_at IS NULL OR other.expires_at > now())
              AND (rel.valid_from IS NULL OR rel.valid_from <= COALESCE(NULLIF(current_setting('scos.as_of_valid_time',true), '')::timestamptz, now()))
              AND (rel.valid_until IS NULL OR rel.valid_until > COALESCE(NULLIF(current_setting('scos.as_of_valid_time',true), '')::timestamptz, now()))), '[]'::jsonb)
		FROM memory_items mi
		LEFT JOIN memory_versions mv ON mv.id = mi.current_version_id
		WHERE mi.tenant_id = $1
		  AND mi.status = 'active'
		  AND mi.namespace_id = ANY($2::uuid[])
		  AND mi.sensitivity <= $3
		  AND (mi.valid_from IS NULL OR mi.valid_from <= COALESCE(NULLIF(current_setting('scos.as_of_valid_time',true), '')::timestamptz, now()))
		  AND (mi.expires_at IS NULL OR mi.expires_at > now())
		  AND (mi.valid_until IS NULL OR mi.valid_until > COALESCE(NULLIF(current_setting('scos.as_of_valid_time',true), '')::timestamptz, now()))
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
	if req.AsOfValidTime != "" {
		out["as_of_valid_time"] = req.AsOfValidTime
		out["temporal_scope"] = temporalScopeFor(req.AsOfValidTime)
	}
	if req.AsOfCommit != "" || req.SnapshotID != "" {
		// The selector alone does not identify a historical code generation:
		// repository and branch must survive even when no symbols matched.
		out["repository_id"] = req.RepositoryID
		out["branch"] = req.Branch
		out["code_temporal_scope"] = "immutable_snapshot"
		if req.AsOfCommit != "" {
			out["as_of_commit"] = req.AsOfCommit
		}
		if req.SnapshotID != "" {
			out["snapshot_id"] = req.SnapshotID
		}
	}
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
