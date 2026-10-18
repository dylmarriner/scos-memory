package main

import "github.com/dylmarriner/scos-memory/packages/release"

import "github.com/dylmarriner/scos-memory/packages/auth"
import "github.com/dylmarriner/scos-memory/packages/tlsserve"

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/dylmarriner/scos-memory/packages/database"
	"github.com/dylmarriner/scos-memory/packages/llmclient"
	secretscan "github.com/dylmarriner/scos-memory/packages/secret-scan"
	"github.com/dylmarriner/scos-memory/packages/telemetry"
	"github.com/dylmarriner/scos-memory/services/readiness"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// serviceMetrics emits this service's §32 measures. It is nil-safe and a
// no-op when no collector is configured.
var serviceMetrics = telemetry.FromEnvironment("memory-intelligence")

// llmClient is nil (and every llmclient method fails fast, honestly) unless
// DEEPSEEK_API_KEY is configured. Entity extraction (resolveEntityMentions)
// and memory summarization (handleDashboardSummary) degrade to deterministic
// fallbacks when it is nil rather than requiring it at startup.
var llmClient = llmclient.FromEnvironment(nil)

// buildVersion is the build this process runs, as every service reports it
// at /health/version: the commit it was built from, shortened like the
// peers' entries in the system map ("unknown" for a local build).
func buildVersion() string {
	v := release.SourceSHA
	if len(v) > 12 {
		v = v[:12]
	}
	return v
}

// ── Memory network types (§25.2) ────────────────────────────────────────

type MemoryNetwork string

const (
	NetworkWorld      MemoryNetwork = "world"
	NetworkExperience MemoryNetwork = "experience"
	NetworkEntity     MemoryNetwork = "entity"
	NetworkBelief     MemoryNetwork = "belief"
	NetworkProcedure  MemoryNetwork = "procedure"
	NetworkNegative   MemoryNetwork = "negative"
)

// Defaults for a fact the user stated explicitly, matching the semantic
// class in the consolidation worker's promotion policy.
const (
	explicitFactAuthority  = 70
	explicitFactConfidence = 0.75
	// A fact imported from another agent's memory store was curated by that
	// agent, not stated to this fabric by the user, so it ranks below an
	// explicit fact and below the OKF promotion threshold until confirmed.
	externalFactAuthority  = 50
	externalFactConfidence = 0.6
)

type Fact struct {
	ID      string        `json:"id"`
	Network MemoryNetwork `json:"network"`
	Content string        `json:"content"`
	Source  string        `json:"source"` // explicit, observed, inferred, external
	// Origin names where an external fact came from (the agent and the
	// store it was read from); empty for facts first recorded here.
	Origin       string   `json:"origin,omitempty"`
	Authority    int      `json:"authority_level"`
	Confidence   float64  `json:"confidence"`
	Scope        string   `json:"scope"`
	ObservedAt   string   `json:"observed_at"`
	ValidFrom    string   `json:"valid_from,omitempty"`
	ValidUntil   string   `json:"valid_until,omitempty"`
	RecordedAt   string   `json:"recorded_at"`
	SupersededBy string   `json:"superseded_by,omitempty"`
	Status       string   `json:"status,omitempty"` // active, merged -- see loadFactsIncludingMerged
	Evidence     []string `json:"evidence,omitempty"`
	// CodeSymbolVersionIDs are code_symbol_versions.id values the caller
	// already knows this fact cites (e.g. an agent recording a decision
	// right after scos_code_impact/scos_code_query returned a result). P1-C
	// write point 2: when set and this fact gets promoted to OKF, these are
	// linked automatically with source='okf_promotion' -- see
	// maybePromoteToOKF. Empty is the common case; this is never required.
	CodeSymbolVersionIDs []string `json:"code_symbol_version_ids,omitempty"`
	Inferred             bool     `json:"inferred"`
	Derivation           string   `json:"derivation,omitempty"`
	Revision             int      `json:"revision"`
	// Sensitivity is the class to store the fact at (default internal);
	// content the secret scanner flags is stored secret whatever it says.
	Sensitivity string `json:"sensitivity,omitempty"`

	// Identity/placement fields. These carry no meaning in the historical
	// in-memory shape but are required to satisfy the relational schema's
	// tenant isolation and namespace membership rules (database/policies);
	// see resolveIdentity.
	TenantID    string `json:"tenant_id,omitempty"`
	PrincipalID string `json:"principal_id,omitempty"`
	NamespaceID string `json:"namespace_id,omitempty"`
}

type Entity struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Type        string            `json:"type"` // user, agent, organisation
	Facts       []string          `json:"fact_ids,omitempty"`
	Preferences []string          `json:"preferences,omitempty"`
	Goals       []string          `json:"goals,omitempty"`
	Traits      map[string]string `json:"traits,omitempty"`
	Scope       string            `json:"scope"`

	TenantID    string `json:"tenant_id,omitempty"`
	PrincipalID string `json:"principal_id,omitempty"`
}

type Belief struct {
	ID         string   `json:"id"`
	Premise    string   `json:"premise"`
	Conclusion string   `json:"conclusion"`
	Confidence float64  `json:"confidence"`
	Evidence   []string `json:"evidence_ids"`
	RevisedBy  string   `json:"revised_by,omitempty"`
	CreatedAt  string   `json:"created_at"`
	// Status and UpdatedAt are read back from the store (loadBeliefs);
	// UpdatedAt is the belief's last revision, the point its evidence is
	// judged against for staleness (see beliefStaleness).
	Status    string `json:"status,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
	revisedAt time.Time

	TenantID    string `json:"tenant_id,omitempty"`
	PrincipalID string `json:"principal_id,omitempty"`
}

type TemporalRelation struct {
	SourceID   string  `json:"source_id"`
	TargetID   string  `json:"target_id"`
	Type       string  `json:"type"` // derived_from, contradicts, supersedes, evidence_for, used_in
	Confidence float64 `json:"confidence"`
	Scope      string  `json:"scope"`
	CreatedAt  string  `json:"created_at"`
}

// ── Organisational hierarchy (§25.6) ────────────────────────────────────

type MemoryHierarchy string

const (
	LevelPalace   MemoryHierarchy = "palace"
	LevelWing     MemoryHierarchy = "wing"
	LevelDrawer   MemoryHierarchy = "drawer"
	LevelCloset   MemoryHierarchy = "closet"
	LevelDiary    MemoryHierarchy = "diary"
	LevelArtifact MemoryHierarchy = "artifact"
)

type HierarchyNode struct {
	ID       string          `json:"id"`
	Level    MemoryHierarchy `json:"level"`
	Name     string          `json:"name"`
	ParentID string          `json:"parent_id,omitempty"`
	Scope    string          `json:"scope"`
	Summary  string          `json:"summary,omitempty"`
	// FactIDs are the active facts filed directly under this node and
	// FactCount the distinct active facts in its whole subtree. Both are
	// computed from stored facts on read (loadHierarchy); neither is
	// accepted from a caller.
	FactIDs   []string `json:"fact_ids,omitempty"`
	FactCount int      `json:"fact_count"`
	UpdatedAt string   `json:"updated_at"`

	TenantID    string `json:"tenant_id,omitempty"`
	PrincipalID string `json:"principal_id,omitempty"`
}

// ── Dashboard types (§25.9) ─────────────────────────────────────────────

type DashboardView struct {
	Type        string `json:"type"`
	Entries     []any  `json:"entries"`
	TotalCount  int    `json:"total_count"`
	LastUpdated string `json:"last_updated"`
}

type SystemMapEntry struct {
	Service string `json:"service"`
	Status  string `json:"status"`
	Version string `json:"version"`
	Uptime  string `json:"uptime"`
	// Detail says why a field could not be measured, e.g. a peer that
	// refused this service's credentials on /v1/version.
	Detail string `json:"detail,omitempty"`
}

// ── server state ─────────────────────────────────────────────────────────

// engineServer holds no domain data in memory: every fact, entity, belief,
// relation, hierarchy node and diary entry is read from and written to
// PostgreSQL on every request (see store.go). The only in-memory state left
// is operational (start time, HTTP client, peer addresses for the system
// map).
type engineServer struct {
	mu         sync.RWMutex
	startTime  time.Time
	db         *sql.DB
	httpClient *http.Client
	peers      map[string]string // service name -> base URL, for the real system map (§25.9)

	// okfSynchroniserURL and the promotion thresholds below back
	// maybePromoteToOKF (§16/§26): a fact whose authority and confidence
	// both clear their threshold gets an OKF proposal opened automatically,
	// but never merged automatically -- merge still requires an independent
	// knowledge_curator review, exactly as every other OKF proposal does.
	okfSynchroniserURL   string
	okfAuthorityThresh   int
	okfConfidenceThresh  float64
	internalServiceToken string
}

// peerDefault names an environment variable and its fallback base URL for a
// service reported in the real-time system map.
type peerDefault struct {
	name     string
	envVar   string
	fallback string
}

var peerDefaults = []peerDefault{
	{"cloud-control-plane", "SCOS_CLOUD_CONTROL_PLANE_URL", "https://cloud-control-plane:8079"},
	{"memory-gateway", "SCOS_MEMORY_GATEWAY_URL", "https://memory-gateway:8080"},
	{"autonomous-control-plane", "SCOS_AUTONOMOUS_CONTROL_PLANE_URL", "https://autonomous-control-plane:8081"},
	{"retrieval-orchestrator", "SCOS_RETRIEVAL_ORCHESTRATOR_URL", "https://retrieval-orchestrator:8082"},
	{"context-optimisation", "SCOS_CONTEXT_OPTIMISATION_URL", "https://context-optimisation:8083"},
	{"code-intelligence", "SCOS_CODE_INTELLIGENCE_URL", "https://code-intelligence:8084"},
	{"execution-proxy", "SCOS_EXECUTION_PROXY_URL", "https://execution-proxy:8085"},
	{"extension-manager", "SCOS_EXTENSION_MANAGER_URL", "https://extension-manager:8086"},
	{"compatibility-gateway", "SCOS_COMPATIBILITY_GATEWAY_URL", "https://compatibility-gateway:8087"},
	{"hermes-provider", "SCOS_HERMES_PROVIDER_URL", "https://hermes-provider:8088"},
	{"okf-synchroniser", "SCOS_OKF_SYNCHRONISER_URL", "https://okf-synchroniser:8090"},
	{"security-gate", "SCOS_SECURITY_GATE_URL", "https://security-gate:8091"},
	{"tool-governance", "SCOS_TOOL_GOVERNANCE_URL", "https://tool-governance:8092"},
	{"agent-registry", "SCOS_AGENT_REGISTRY_URL", "https://agent-registry:8093"},
	{"recovery-orchestrator", "SCOS_RECOVERY_ORCHESTRATOR_URL", "https://recovery-orchestrator:8094"},
	{"backup-manager", "SCOS_BACKUP_MANAGER_URL", "https://backup-manager:8095"},
	{"observability", "SCOS_OBSERVABILITY_URL", "https://observability:8096"},
	{"transparency-projection", "SCOS_TRANSPARENCY_URL", "https://transparency-projection:8097"},
	{"mcp-gateway", "SCOS_MCP_GATEWAY_URL", "https://mcp-gateway:8098"},
	{"graphify-provider", "SCOS_GRAPHIFY_PROVIDER_URL", "https://graphify-provider:8099"},
	{"credential-vault", "SCOS_CREDENTIAL_VAULT_URL", "https://credential-vault:8100"},
}

func resolveUpstream(envVar, fallback string) string {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv(envVar)), "/")
	if base == "" {
		base = fallback
	}
	return base
}

func envInt(envVar string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(envVar))
	if raw == "" {
		return fallback
	}
	if n, err := strconv.Atoi(raw); err == nil {
		return n
	}
	return fallback
}

func envFloat(envVar string, fallback float64) float64 {
	raw := strings.TrimSpace(os.Getenv(envVar))
	if raw == "" {
		return fallback
	}
	if f, err := strconv.ParseFloat(raw, 64); err == nil {
		return f
	}
	return fallback
}

func resolvePeers() map[string]string {
	resolved := make(map[string]string, len(peerDefaults))
	for _, d := range peerDefaults {
		base := strings.TrimRight(os.Getenv(d.envVar), "/")
		if base == "" {
			base = d.fallback
		}
		resolved[d.name] = base
	}
	return resolved
}

func (s *engineServer) handleVersion(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	uptime := time.Since(s.startTime)
	s.mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(versionInfo{
		Status:        "ok",
		Version:       buildVersion(),
		UptimeSeconds: int(uptime.Seconds()),
	})
}

func (s *engineServer) readinessCheck() error {
	if s.db == nil {
		return fmt.Errorf("postgres dependency is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return s.db.PingContext(ctx)
}

func newEngineServer() *engineServer {
	return &engineServer{startTime: time.Now()}
}

// ── identity resolution ──────────────────────────────────────────────────

// resolveIdentity determines the tenant/principal a request acts as. Claims
// carried on an authenticated bearer token take precedence; internal
// SCOS-Tenant/Principal headers are the fallback used by direct
// service-to-service calls and by tests that invoke handlers without the
// auth middleware in front of them. A caller-supplied tenant/principal on
// the JSON body must agree with whichever identity was authenticated. This
// mirrors the convention established by hermes-provider.
func resolveIdentity(r *http.Request, bodyTenant, bodyPrincipal string) (requestIdentity, int, string) {
	if claims, ok := auth.CanonicalClaims(r.Context()); ok {
		if (bodyTenant != "" && bodyTenant != claims.TenantID) || (bodyPrincipal != "" && bodyPrincipal != claims.PrincipalID) {
			return requestIdentity{}, http.StatusForbidden, "request identity does not match authenticated identity"
		}
		return requestIdentity{TenantID: claims.TenantID, PrincipalID: claims.PrincipalID}, 0, ""
	}
	if auth.Identity(r.Context()) != "" {
		return requestIdentity{}, http.StatusUnauthorized, "authenticated token has no tenant and principal claims"
	}
	tenantID := strings.TrimSpace(r.Header.Get("X-SCOS-Tenant-ID"))
	principalID := strings.TrimSpace(r.Header.Get("X-SCOS-Principal-ID"))
	if tenantID == "" {
		tenantID = bodyTenant
	}
	if principalID == "" {
		principalID = bodyPrincipal
	}
	if tenantID == "" || principalID == "" {
		return requestIdentity{}, http.StatusUnauthorized, "tenant_id and principal_id are required"
	}
	return requestIdentity{TenantID: tenantID, PrincipalID: principalID}, 0, ""
}

// resolveIdentityFromRequest resolves identity for read-only endpoints that
// carry no request body to cross-check against (dashboard/list views).
func resolveIdentityFromRequest(r *http.Request) (requestIdentity, int, string) {
	return resolveIdentity(r, "", "")
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
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		log.Fatalf("connect to database: %v", err)
	}
	if err := database.RequireNoBypassRLSDB(context.Background(), db); err != nil {
		log.Fatalf("refusing to start: %v", err)
	}
	database.TuneSQLDB(db)
	defer db.Close()

	srv := newEngineServer()
	srv.db = db
	client, err := tlsserve.TrustedClient(3 * time.Second)
	if err != nil {
		log.Fatalf("configure internal TLS client: %v", err)
	}
	srv.httpClient = client
	srv.peers = resolvePeers()
	srv.okfSynchroniserURL = resolveUpstream("SCOS_OKF_SYNCHRONISER_URL", "https://okf-synchroniser:8090")
	srv.okfAuthorityThresh = envInt("SCOS_OKF_PROMOTION_AUTHORITY_THRESHOLD", 90)
	srv.okfConfidenceThresh = envFloat("SCOS_OKF_PROMOTION_CONFIDENCE_THRESHOLD", 0.85)
	srv.internalServiceToken = strings.TrimSpace(os.Getenv("SCOS_INTERNAL_SERVICE_TOKEN"))

	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", healthOK)
	mux.HandleFunc("/health/version", release.Handler("memory-intelligence"))
	mux.HandleFunc("/health/ready", readiness.Handler(func() error {
		return srv.readinessCheck()
	}))
	mux.HandleFunc("/v1/version", srv.handleVersion)

	// Memory networks
	mux.HandleFunc("/v1/facts", srv.handleFacts)
	mux.HandleFunc("/v1/candidates", srv.handleCandidates)
	mux.HandleFunc("/v1/candidates/", srv.handleCandidateDecision)
	mux.HandleFunc("/v1/facts/batch-correct", srv.handleFactsBatchCorrect)
	mux.HandleFunc("/v1/facts/batch-forget", srv.handleFactsBatchForget)
	mux.HandleFunc("/v1/memory", srv.handleFacts)
	mux.HandleFunc("/v1/facts/", srv.handleFactByID)
	mux.HandleFunc("/v1/entities", srv.handleEntities)
	mux.HandleFunc("/v1/entities/", srv.handleEntityByID)
	mux.HandleFunc("/v1/beliefs", srv.handleBeliefs)
	mux.HandleFunc("/v1/relations", srv.handleRelations)
	mux.HandleFunc("/v1/hierarchy", srv.handleHierarchy)
	mux.HandleFunc("/v1/hierarchy/", srv.handleHierarchyFacts)

	// Reflection
	mux.HandleFunc("/v1/reflect", srv.handleReflect)

	// Dashboard views (§25.9)
	mux.HandleFunc("/v1/dashboard/system", srv.handleDashboardSystem)
	mux.HandleFunc("/v1/dashboard/memory-graph", srv.handleDashboardMemoryGraph)
	mux.HandleFunc("/v1/dashboard/facts", srv.handleDashboardFacts)
	mux.HandleFunc("/v1/dashboard/summary", srv.handleDashboardSummary)
	mux.HandleFunc("/v1/dashboard/learnings", srv.handleDashboardLearnings)
	mux.HandleFunc("/v1/dashboard/contradictions", srv.handleDashboardContradictions)
	mux.HandleFunc("/v1/dashboard/timeline", srv.handleDashboardTimeline)
	mux.HandleFunc("/v1/dashboard/user-model", srv.handleDashboardUserModel)
	mux.HandleFunc("/v1/memory/graph", srv.handleDashboardMemoryGraph)
	mux.HandleFunc("/v1/learnings", srv.handleDashboardLearnings)
	mux.HandleFunc("/v1/evidence-links", srv.handleEvidenceLinks)
	mux.HandleFunc("/v1/diary", srv.handleDiary)
	mux.HandleFunc("/v1/stream", srv.handleStream) // SSE
	mux.HandleFunc("/v1/transparency/stream", srv.handleStream)
	mux.HandleFunc("/v1/users/", srv.handleUserKnowledge)

	addr := os.Getenv("SCOS_MEMORY_INTELLIGENCE_HTTP_ADDR")
	if addr == "" {
		addr = ":8089"
	}
	log.Printf("memory intelligence engine listening on %s", addr)
	if err := tlsserve.ListenAndServe(addr, telemetry.Instrument("memory-intelligence", auth.AuthenticatedHandler(mux, auth.MustFromEnvironment()))); err != nil {
		log.Fatal(err)
	}
}

func (s *engineServer) handleEntityRepresentation(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	identity, status, msg := resolveIdentityFromRequest(r)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/entities/")
	id = strings.TrimSuffix(id, "/representation")
	ctx := r.Context()
	entity, err := withPrincipalContext(ctx, s.db, identity, func(q querier) (*Entity, error) {
		return getEntity(ctx, q, identity.TenantID, id)
	})
	if err == sql.ErrNoRows {
		http.Error(w, "not found", http.StatusNotFound)
		return
	} else if err != nil {
		http.Error(w, "load entity: "+err.Error(), http.StatusInternalServerError)
		return
	}
	facts := make([]*Fact, 0, len(entity.Facts))
	for _, factID := range entity.Facts {
		if fact, err := withPrincipalContext(ctx, s.db, identity, func(q querier) (*Fact, error) {
			return getFact(ctx, q, identity.TenantID, factID)
		}); err == nil {
			facts = append(facts, fact)
		}
	}
	writeJSON(w, map[string]any{"entity": entity, "facts": facts})
}

func (s *engineServer) handleUserKnowledge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	identity, status, msg := resolveIdentityFromRequest(r)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	userID := strings.TrimPrefix(r.URL.Path, "/v1/users/")
	userID = strings.TrimSuffix(userID, "/knowledge")
	if userID == "" || strings.Contains(userID, "/") {
		http.Error(w, "user_id is required", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	entity, err := withPrincipalContext(ctx, s.db, identity, func(q querier) (*Entity, error) {
		return getEntity(ctx, q, identity.TenantID, userID)
	})
	if err == sql.ErrNoRows || (err == nil && entity.Type != "user") {
		http.Error(w, "user not found", http.StatusNotFound)
		return
	} else if err != nil {
		http.Error(w, "load entity: "+err.Error(), http.StatusInternalServerError)
		return
	}
	facts := make([]*Fact, 0, len(entity.Facts))
	for _, factID := range entity.Facts {
		if fact, err := withPrincipalContext(ctx, s.db, identity, func(q querier) (*Fact, error) {
			return getFact(ctx, q, identity.TenantID, factID)
		}); err == nil {
			facts = append(facts, fact)
		}
	}
	writeJSON(w, map[string]any{
		"user_id": userID,
		"entity":  entity,
		"facts":   facts,
	})
}

func healthOK(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// ── facts ────────────────────────────────────────────────────────────────

func (s *engineServer) handleFacts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		identity, status, msg := resolveIdentityFromRequest(r)
		if status != 0 {
			http.Error(w, msg, status)
			return
		}
		loader := loadFacts
		if r.URL.Query().Get("include_merged") == "true" {
			loader = loadFactsIncludingMerged
		}
		facts, err := withPrincipalContext(r.Context(), s.db, identity, func(q querier) ([]*Fact, error) {
			return loader(r.Context(), q, identity.TenantID, "")
		})
		if err != nil {
			http.Error(w, "load facts: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, facts)

	case http.MethodDelete:
		s.handleFactsForgetAll(w, r)

	case http.MethodPost:
		var fact Fact
		if err := json.NewDecoder(r.Body).Decode(&fact); err != nil {
			http.Error(w, "invalid fact: "+err.Error(), http.StatusBadRequest)
			return
		}
		if fact.Content == "" {
			http.Error(w, "content required", http.StatusBadRequest)
			return
		}
		identity, status, msg := resolveIdentity(r, fact.TenantID, fact.PrincipalID)
		if status != 0 {
			http.Error(w, msg, status)
			return
		}
		if fact.NamespaceID == "" {
			// An agent saying "remember this" names no namespace: the fact
			// goes to the caller's own writable namespace.
			ns, err := withPrincipalContext(r.Context(), s.db, identity, func(q querier) (string, error) {
				return defaultWritableNamespace(r.Context(), q, identity)
			})
			if err != nil {
				http.Error(w, "resolve namespace: "+err.Error(), http.StatusInternalServerError)
				return
			}
			if ns == "" {
				http.Error(w, "namespace_id is required: the caller owns no namespace and is not a writing member of one", http.StatusBadRequest)
				return
			}
			fact.NamespaceID = ns
		}
		if fact.Sensitivity != "" {
			// A memory above its namespace's sensitivity ceiling would be
			// written and then hidden from everyone, its writer included
			// (memory_items_namespace_access), so the write is refused.
			ceiling, err := withPrincipalContext(r.Context(), s.db, identity, func(q querier) (string, error) {
				var ceiling string
				var allowed bool
				err := q.QueryRowContext(r.Context(),
					`SELECT sensitivity_ceiling::text, $2::sensitivity_class <= sensitivity_ceiling FROM namespaces WHERE id = $1`,
					fact.NamespaceID, fact.Sensitivity).Scan(&ceiling, &allowed)
				if err == nil && allowed {
					ceiling = ""
				}
				return ceiling, err
			})
			if errors.Is(err, sql.ErrNoRows) {
				http.Error(w, "namespace "+fact.NamespaceID+" is not visible to the caller", http.StatusBadRequest)
				return
			}
			if err != nil {
				http.Error(w, "invalid sensitivity "+fact.Sensitivity+": "+err.Error(), http.StatusBadRequest)
				return
			}
			if ceiling != "" {
				http.Error(w, fmt.Sprintf("namespace %s holds memories up to %s; a %s memory would be hidden from every reader", fact.NamespaceID, ceiling, fact.Sensitivity), http.StatusBadRequest)
				return
			}
		}
		if fact.Network == "" {
			fact.Network = NetworkWorld
		}
		if fact.Source == "explicit" {
			// A fact the user stated outright carries the same authority and
			// confidence the consolidation worker assigns semantic knowledge
			// (workers/consolidation promoter: authority 70, confidence 0.75)
			// unless the caller set them.
			if fact.Authority == 0 {
				fact.Authority = explicitFactAuthority
			}
			if fact.Confidence == 0 {
				fact.Confidence = explicitFactConfidence
			}
		}
		if fact.Source == "external" {
			if fact.Origin == "" {
				http.Error(w, "an external fact must name its origin", http.StatusBadRequest)
				return
			}
			if fact.Authority == 0 {
				fact.Authority = externalFactAuthority
			}
			if fact.Confidence == 0 {
				fact.Confidence = externalFactConfidence
			}
		}
		if fact.Scope == "" {
			fact.Scope = "global"
		}
		fact.RecordedAt = time.Now().UTC().Format(time.RFC3339)

		// Write-time secret/PII gate (P0-B), applied here rather than only
		// inside insertFact: every downstream consumer of fact.Content below
		// -- resolveEntityMentions and maybePromoteToOKF, both of which can
		// call DeepSeek -- must see the redacted form too, not just what
		// eventually lands in Postgres. Redacting only inside insertFact
		// left a secret in an unredacted fact.Content shipped to a
		// third-party LLM API in the same request that redacted it in
		// storage. insertFact's own scan is now a harmless no-op safety net
		// on already-redacted text, not the sole gate.
		if findings := secretscan.Scan(fact.Content); len(findings) > 0 {
			fact.Content = secretscan.Redact(fact.Content, findings)
		}

		ctx := r.Context()
		// Resolved before the transaction opens: both of these may be LLM
		// calls, and an outbound HTTP request must never hold a database
		// transaction open.
		mentions := resolveEntityMentions(ctx, fact.Content)

		// Detect contradictions and duplicates against every existing fact in
		// this tenant before inserting, exactly as the original in-memory
		// implementation did for contradictions, just sourced from the
		// durable store instead of a map.
		existing, err := withPrincipalContext(ctx, s.db, identity, func(q querier) ([]*Fact, error) {
			return loadFacts(ctx, q, identity.TenantID, "")
		})
		if err != nil {
			http.Error(w, "load existing facts: "+err.Error(), http.StatusInternalServerError)
			return
		}
		var contradictions []string
		contradicted := make(map[string]*Fact)
		for _, other := range existing {
			if isContradictory(&fact, other) {
				contradictions = append(contradictions, other.ID)
				contradicted[other.ID] = other
			}
		}
		merge := findMergeCandidate(ctx, &fact, existing)

		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			http.Error(w, "begin transaction: "+err.Error(), http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()
		if err := setPrincipalContext(ctx, tx, identity); err != nil {
			http.Error(w, "set principal context: "+err.Error(), http.StatusInternalServerError)
			return
		}

		if merge != nil && !merge.NewIsRicher {
			// The existing fact already states this at least as fully: keep
			// the single canonical memory rather than writing a second copy
			// (Dream's Merge -- "no second copy is created, so you simply
			// keep the one memory"). Nothing was written; let the deferred
			// rollback clean up the otherwise-empty transaction.
			serviceMetrics.Count(telemetry.MetricUserModelChanges,
				map[string]string{"change": "fact_merge_skipped"},
				telemetry.Correlation{Tenant: identity.TenantID})
			response := map[string]any{
				"fact": merge.ExistingFact, "merged": true, "canonical_fact_id": merge.ExistingFact.ID,
			}
			if len(fact.CodeSymbolVersionIDs) > 0 {
				response["code_symbol_links"] = dropAllCodeLinks(fact.CodeSymbolVersionIDs,
					"no new fact was stored (merged into "+merge.ExistingFact.ID+"); link the canonical fact with /v1/evidence-links")
			}
			writeJSON(w, response)
			return
		}

		factID, err := insertFact(ctx, tx, identity, fact.NamespaceID, &fact, mentions)
		if err != nil {
			http.Error(w, "failed to persist fact: "+err.Error(), http.StatusInternalServerError)
			return
		}
		fact.ID = factID
		for _, c := range contradictions {
			// A detected contradiction is as certain as the weaker of the
			// two claims it sets against each other.
			if err := insertRelation(ctx, tx, factID, c, "contradicts", relationConfidenceFromFacts(&fact, contradicted[c])); err != nil {
				http.Error(w, "failed to persist contradiction: "+err.Error(), http.StatusInternalServerError)
				return
			}
		}
		diary := map[string]any{
			"fact_id": factID, "network": fact.Network, "namespace_id": fact.NamespaceID,
			"summary": truncate(fact.Content, 120),
		}
		if len(contradictions) > 0 {
			diary["contradicts"] = contradictions
		}
		if merge != nil && merge.NewIsRicher {
			if err := markFactMerged(ctx, tx, identity.TenantID, merge.ExistingFact.ID, factID, merge.Score); err != nil {
				http.Error(w, "failed to mark duplicate merged: "+err.Error(), http.StatusInternalServerError)
				return
			}
			diary["merged_fact_id"] = merge.ExistingFact.ID
		}
		if err := recordDiaryEntry(ctx, tx, identity, "fact_recorded", diary); err != nil {
			http.Error(w, "failed to record diary entry: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if err := tx.Commit(); err != nil {
			http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
			return
		}
		// §32 correction propagation: a new fact that contradicts existing
		// ones propagates a correction across every contradicted record.
		serviceMetrics.Count(telemetry.MetricUserModelChanges,
			map[string]string{"change": "fact_recorded"},
			telemetry.Correlation{Tenant: identity.TenantID})
		if len(contradictions) > 0 {
			serviceMetrics.Gauge(telemetry.MetricCorrectionPropagation,
				float64(len(contradictions)), "count",
				map[string]string{"namespace": fact.NamespaceID},
				telemetry.Correlation{Tenant: identity.TenantID})
		}
		requestedCodeLinks := len(fact.CodeSymbolVersionIDs) > 0
		codeLinks := s.maybePromoteToOKF(&fact, identity)
		// Echo only the code links that were actually recorded; every
		// requested link that was not is reported as dropped with the reason.
		fact.CodeSymbolVersionIDs = codeLinks.Linked
		response := map[string]any{"fact": fact, "contradictions": contradictions}
		if requestedCodeLinks {
			response["code_symbol_links"] = codeLinks
		}
		if merge != nil && merge.NewIsRicher {
			serviceMetrics.Count(telemetry.MetricUserModelChanges,
				map[string]string{"change": "fact_merged"},
				telemetry.Correlation{Tenant: identity.TenantID})
			response["merged_fact_id"] = merge.ExistingFact.ID
		}
		writeJSON(w, response)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleFactsForgetAll implements bulk scos_forget / mem0's delete_all and
// reset (§25/§42): DELETE /v1/facts?namespace_id=X&confirm=true revokes every
// active fact in that namespace; omitting namespace_id revokes every active
// fact for the whole tenant (a reset of this tenant's factual memory).
// confirm=true is mandatory in both cases — this is bulk and irreversible in
// intent (soft-deleted, not physically removed, but not meant to be routine).
func (s *engineServer) handleFactsForgetAll(w http.ResponseWriter, r *http.Request) {
	identity, status, msg := resolveIdentityFromRequest(r)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	if r.URL.Query().Get("confirm") != "true" {
		http.Error(w, "bulk forget requires confirm=true", http.StatusBadRequest)
		return
	}
	namespaceID := strings.TrimSpace(r.URL.Query().Get("namespace_id"))
	revoked, err := forgetAllFacts(r.Context(), s.db, identity, namespaceID)
	if err != nil {
		http.Error(w, "bulk forget: "+err.Error(), http.StatusInternalServerError)
		return
	}
	serviceMetrics.Gauge(telemetry.MetricUserModelChanges, float64(revoked), "count",
		map[string]string{"change": "facts_bulk_forgotten", "namespace_scoped": strconv.FormatBool(namespaceID != "")},
		telemetry.Correlation{Tenant: identity.TenantID})
	writeJSON(w, map[string]any{
		"forgotten":     true,
		"namespace_id":  namespaceID,
		"revoked_count": revoked,
	})
}

func (s *engineServer) handleFactByID(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/history") {
		s.handleFactHistory(w, r)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/feedback") {
		s.handleFactFeedback(w, r)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/facts/")
	if id == "" {
		http.Error(w, "fact id is required", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodGet:
		identity, status, msg := resolveIdentityFromRequest(r)
		if status != 0 {
			http.Error(w, msg, status)
			return
		}
		fact, err := withPrincipalContext(r.Context(), s.db, identity, func(q querier) (*Fact, error) {
			return getFact(r.Context(), q, identity.TenantID, id)
		})
		if err == sql.ErrNoRows {
			http.Error(w, "not found", http.StatusNotFound)
			return
		} else if err != nil {
			http.Error(w, "load fact: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, fact)

	case http.MethodPatch:
		s.handleFactCorrect(w, r, id)

	case http.MethodDelete:
		s.handleFactForget(w, r, id)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleFactHistory implements scos_fact_history (mem0's history()):
// GET /v1/facts/{id}/history returns every version ever recorded for the
// fact, including versions from before a correction or after a forget — the
// append-only audit trail behind scos_correct/scos_forget.
func (s *engineServer) handleFactHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	identity, status, msg := resolveIdentityFromRequest(r)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/facts/"), "/history")
	if id == "" {
		http.Error(w, "fact id is required", http.StatusBadRequest)
		return
	}
	history, err := withPrincipalContext(r.Context(), s.db, identity, func(q querier) ([]FactVersion, error) {
		return loadFactHistory(r.Context(), q, identity.TenantID, id)
	})
	if err == sql.ErrNoRows {
		http.Error(w, "not found", http.StatusNotFound)
		return
	} else if err != nil {
		http.Error(w, "load fact history: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"fact_id": id, "history": history})
}

// handleFactCorrect implements scos_correct (§25/§42): PATCH /v1/facts/{id}
// with {"content": "...", "reason": "..."} records a new fact carrying the
// corrected content and marks the old one superseded. See correctFact.
func (s *engineServer) handleFactCorrect(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		Content     string `json:"content"`
		Reason      string `json:"reason"`
		TenantID    string `json:"tenant_id,omitempty"`
		PrincipalID string `json:"principal_id,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.Content) == "" {
		http.Error(w, "content is required", http.StatusBadRequest)
		return
	}
	identity, status, msg := resolveIdentity(r, body.TenantID, body.PrincipalID)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	corrected, err := correctFact(r.Context(), s.db, identity, id, body.Content, body.Reason)
	if err == sql.ErrNoRows {
		http.Error(w, "not found", http.StatusNotFound)
		return
	} else if err != nil {
		http.Error(w, "correct fact: "+err.Error(), http.StatusInternalServerError)
		return
	}
	serviceMetrics.Count(telemetry.MetricUserModelChanges,
		map[string]string{"change": "fact_corrected"},
		telemetry.Correlation{Tenant: identity.TenantID})
	s.maybePromoteToOKF(corrected, identity)
	writeJSON(w, map[string]any{
		"corrected":        true,
		"previous_fact_id": id,
		"fact":             corrected,
	})
}

// okfCategories are the only valid targets for an auto-generated OKF
// proposal (§26's recommended okf/ structure). "memory" is deliberately not
// one of these -- a flat per-fact bucket is exactly the Markdown landfill
// OKF exists to avoid. A promoted fact lands in the same curated tree a
// human author would use.
var okfCategories = map[string]bool{
	"governance": true, "architecture": true, "projects": true,
	"infrastructure": true, "procedures": true, "tools": true,
	"decisions": true, "glossary": true,
}

var okfSlugSanitizer = regexp.MustCompile(`[^a-z0-9-]+`)

// codeLinkReport says what became of the code_symbol_version_ids a fact
// write cited: which were recorded as memory_code_links rows and which were
// not, each with the reason. A caller is never told a link exists that was
// not written.
type codeLinkReport struct {
	Linked  []string          `json:"linked"`
	Dropped []droppedCodeLink `json:"dropped"`
}

type droppedCodeLink struct {
	CodeSymbolVersionID string `json:"code_symbol_version_id"`
	Reason              string `json:"reason"`
}

func dropAllCodeLinks(ids []string, reason string) codeLinkReport {
	report := codeLinkReport{Linked: []string{}, Dropped: make([]droppedCodeLink, 0, len(ids))}
	for _, id := range ids {
		report.Dropped = append(report.Dropped, droppedCodeLink{CodeSymbolVersionID: id, Reason: reason})
	}
	return report
}

// linkPromotedFactToCode is P1-C write point 2: a fact that already cites
// code_symbol_version_ids and clears the OKF durability bar gets those
// citations persisted as memory_code_links rows with source='okf_promotion'.
// The links are written in one transaction, so either all are recorded or
// none is, and the report says which. A failure never blocks the promotion
// this fact is already mid-way through.
func (s *engineServer) linkPromotedFactToCode(fact *Fact, identity requestIdentity) codeLinkReport {
	if s.db == nil {
		return dropAllCodeLinks(fact.CodeSymbolVersionIDs, "no database is configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	fail := func(stage string, err error) codeLinkReport {
		log.Printf("link promoted fact %s to code: %s: %v", fact.ID, stage, err)
		return dropAllCodeLinks(fact.CodeSymbolVersionIDs, "link not recorded: "+stage+": "+err.Error())
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fail("begin transaction", err)
	}
	defer tx.Rollback()
	if err := setPrincipalContext(ctx, tx, identity); err != nil {
		return fail("set principal context", err)
	}
	for _, symbolVersionID := range fact.CodeSymbolVersionIDs {
		if _, err := insertEvidenceLink(ctx, tx, identity.TenantID, fact.ID, symbolVersionID, "evidences", "okf_promotion", 1.0); err != nil {
			return fail("code symbol version "+symbolVersionID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fail("commit", err)
	}
	return codeLinkReport{Linked: append([]string{}, fact.CodeSymbolVersionIDs...), Dropped: []droppedCodeLink{}}
}

// maybePromoteToOKF implements the automatic half of §16/§26. The
// authority/confidence threshold (SCOS_OKF_PROMOTION_*) is only a cheap
// pre-filter to avoid spending an LLM call on every low-grade fact; the
// real durability gate is the model itself, prompted to reject anything
// that isn't governing policy, an architecture/project/infrastructure
// concept, a durable confirmed fact about a real entity, a procedure, tool
// documentation, a decision, or a glossary term -- exactly the "belongs in
// OKF" list, not raw conversational content. When the model says a fact
// doesn't qualify, or DEEPSEEK_API_KEY isn't configured, no proposal is
// opened: this is additive, never a requirement for the fact write it
// followed to succeed.
//
// What it produces either way is a *proposal*, never a merge -- the
// existing OKF workflow (proposed -> validated -> diffed -> under_review ->
// approved -> merged -> indexed) still requires an independent
// knowledge_curator (or role-appropriate reviewer) to approve it. Nothing
// here bypasses that; it only decides whether a proposal is worth opening
// and how to shape it.
//
// On promotion it also records the code links the fact cites, and it
// reports what became of those links: code links are written only on
// promotion (or explicitly through /v1/evidence-links), so a fact that is
// not promoted has every cited link reported as dropped, with the reason.
func (s *engineServer) maybePromoteToOKF(fact *Fact, identity requestIdentity) codeLinkReport {
	report := codeLinkReport{Linked: []string{}, Dropped: []droppedCodeLink{}}
	if fact == nil {
		return report
	}
	const explicitLink = "; link it explicitly with /v1/evidence-links"
	if fact.Authority < s.okfAuthorityThresh || fact.Confidence < s.okfConfidenceThresh {
		return dropAllCodeLinks(fact.CodeSymbolVersionIDs, fmt.Sprintf(
			"fact is below the OKF promotion threshold (authority %d, needs %d; confidence %.2f, needs %.2f) and code links are recorded only on promotion%s",
			fact.Authority, s.okfAuthorityThresh, fact.Confidence, s.okfConfidenceThresh, explicitLink))
	}
	if !llmClient.Enabled() || s.okfSynchroniserURL == "" || s.internalServiceToken == "" || s.httpClient == nil {
		return dropAllCodeLinks(fact.CodeSymbolVersionIDs,
			"OKF promotion is not configured (needs the classification model, the OKF synchroniser URL and the internal service token)"+explicitLink)
	}

	candidate, err := classifyForOKF(context.Background(), fact)
	if err != nil {
		log.Printf("okf classification for fact %s: %v", fact.ID, err)
		return dropAllCodeLinks(fact.CodeSymbolVersionIDs, "OKF classification failed: "+err.Error()+explicitLink)
	}
	if candidate == nil {
		// The model judged this fact not durable/structural enough for OKF.
		return dropAllCodeLinks(fact.CodeSymbolVersionIDs, "the fact was not judged durable enough for OKF promotion"+explicitLink)
	}

	if len(fact.CodeSymbolVersionIDs) > 0 {
		report = s.linkPromotedFactToCode(fact, identity)
	}

	path := candidate.Category + "/" + candidate.Slug + ".md"
	body, err := json.Marshal(map[string]any{
		"path":     path,
		"content":  candidate.render(fact),
		"proposer": "system:memory-intelligence-auto-promotion",
	})
	if err != nil {
		return report
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.okfSynchroniserURL+"/v1/proposals", bytes.NewReader(body))
	if err != nil {
		return report
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.internalServiceToken)
	resp, err := s.httpClient.Do(req)
	if err != nil {
		log.Printf("okf promotion proposal for fact %s: %v", fact.ID, err)
		return report
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		log.Printf("okf promotion proposal for fact %s (%s): %s: %s", fact.ID, path, resp.Status, strings.TrimSpace(string(respBody)))
		return report
	}
	serviceMetrics.Count(telemetry.MetricUserModelChanges,
		map[string]string{"change": "fact_okf_proposal_opened", "category": candidate.Category},
		telemetry.Correlation{Tenant: identity.TenantID})
	return report
}

// okfCandidate is what the model decides when asked whether, and how, a
// fact should become an OKF document.
type okfCandidate struct {
	Category  string   `json:"category"`
	Slug      string   `json:"slug"`
	Title     string   `json:"title"`
	Type      string   `json:"type"`
	Status    string   `json:"status"`
	Authority string   `json:"authority"`
	Tags      []string `json:"tags"`
	Summary   string   `json:"summary"`
}

// render produces the exact YAML-frontmatter Markdown shape OKF documents
// use elsewhere in this repository (see okf/glossary/index.md and
// GOVERNANCE.md §26): id, title, type, status, version, scope, authority,
// timestamps, evidence linking back to the source fact, and tags.
func (c *okfCandidate) render(fact *Fact) string {
	now := time.Now().UTC().Format(time.RFC3339)
	var tags strings.Builder
	for _, t := range c.Tags {
		fmt.Fprintf(&tags, "  - %s\n", t)
	}
	return fmt.Sprintf(`---
id: %s.%s
title: %s
type: %s
status: %s
version: 1
scope:
  - system/governance
authority: %s
created_at: %s
updated_at: %s
evidence:
  - fact: %s
tags:
%s---

# %s

%s
`, c.Category, c.Slug, c.Title, c.Type, c.Status, c.Authority, now, now, fact.ID, tags.String(), c.Title, c.Summary)
}

// classifyForOKF asks the model whether fact is durable/structural enough
// to become curated OKF knowledge and, if so, how to place and shape it.
// Returns (nil, nil) -- not an error -- when the model judges the fact does
// not belong in OKF; that is the expected, common outcome, not a failure.
func classifyForOKF(ctx context.Context, fact *Fact) (*okfCandidate, error) {
	llmCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var result struct {
		Durable   bool     `json:"durable"`
		Category  string   `json:"category"`
		Slug      string   `json:"slug"`
		Title     string   `json:"title"`
		Type      string   `json:"type"`
		Authority string   `json:"authority"`
		Tags      []string `json:"tags"`
		Summary   string   `json:"summary"`
	}
	err := llmClient.ChatJSON(llmCtx,
		`You decide whether a piece of remembered information is durable enough to become permanent, curated knowledge in an Open Knowledge Format (OKF) repository, and if so, how to classify it.

OKF is NOT a dump for every fact -- reject anything that is a one-off observation, unverified inferred fact, temporary state, narrow personal preference, or ordinary conversational content. Only accept: governing policies, architecture/design decisions, stable project or infrastructure concepts (hosts, services, tools), confirmed durable facts about real entities and their relationships, standard operating procedures, tool documentation, approved workflows, and terminology.

Respond with a JSON object of exactly this shape:
{"durable": true or false, "category": "one of governance|architecture|projects|infrastructure|procedures|tools|decisions|glossary", "slug": "lowercase-hyphenated-id-no-slashes", "title": "short human title", "type": "e.g. architecture-decision, host, policy, concept, procedure, tool", "authority": "one of governing|confirmed|approved|candidate", "tags": ["a few lowercase tags"], "summary": "1-3 sentence body content, factual, no speculation"}

If durable is false, category/slug/title/type/authority/tags/summary may be empty strings/arrays -- do not fabricate placement for content that doesn't belong in OKF.`,
		fmt.Sprintf("network: %s\nnamespace: %s\ncontent: %s", fact.Network, fact.NamespaceID, fact.Content),
		&result)
	if err != nil {
		return nil, err
	}
	if !result.Durable {
		return nil, nil
	}
	if !okfCategories[result.Category] {
		return nil, fmt.Errorf("model chose invalid category %q", result.Category)
	}
	slug := strings.Trim(okfSlugSanitizer.ReplaceAllString(strings.ToLower(strings.TrimSpace(result.Slug)), "-"), "-")
	if slug == "" {
		return nil, fmt.Errorf("model returned an empty slug")
	}
	if strings.TrimSpace(result.Title) == "" || strings.TrimSpace(result.Summary) == "" {
		return nil, fmt.Errorf("model returned an empty title or summary")
	}
	authority := result.Authority
	switch authority {
	case "governing", "confirmed", "approved", "candidate":
	default:
		authority = "candidate"
	}
	statusVal := "proposed"
	typeVal := strings.TrimSpace(result.Type)
	if typeVal == "" {
		typeVal = "concept"
	}
	return &okfCandidate{
		Category: result.Category, Slug: slug, Title: strings.TrimSpace(result.Title),
		Type: typeVal, Status: statusVal, Authority: authority,
		Tags: result.Tags, Summary: strings.TrimSpace(result.Summary),
	}, nil
}

// handleFactForget implements scos_forget (§25/§42): DELETE /v1/facts/{id}
// soft-deletes the fact (status='revoked'). ?cascade=true also revokes the
// fact's superseded predecessor chain (see forgetFact).
// handleEvidenceLinks implements scos_link_evidence (P1-C, §25): an agent
// explicitly records that a fact evidences, was motivated by, or was
// invalidated by a specific code symbol version. Zero inference -- the
// caller already knows the relationship; this just persists it. The other
// P1-C write point (maybePromoteToOKF auto-linking on promotion) is
// separate and unconditional on this endpoint existing.
func (s *engineServer) handleEvidenceLinks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		FactID              string  `json:"fact_id"`
		CodeSymbolVersionID string  `json:"code_symbol_version_id"`
		LinkType            string  `json:"link_type"`
		Confidence          float64 `json:"confidence"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.FactID == "" || req.CodeSymbolVersionID == "" {
		http.Error(w, "fact_id and code_symbol_version_id are required", http.StatusBadRequest)
		return
	}
	switch req.LinkType {
	case "evidences", "motivated_by", "invalidated_by":
	case "":
		req.LinkType = "evidences"
	default:
		http.Error(w, "link_type must be one of evidences, motivated_by, invalidated_by", http.StatusBadRequest)
		return
	}
	confidence := req.Confidence
	if confidence == 0 {
		confidence = 1.0
	}
	identity, status, msg := resolveIdentityFromRequest(r)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	ctx := r.Context()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		http.Error(w, "begin transaction: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	if err := setPrincipalContext(ctx, tx, identity); err != nil {
		http.Error(w, "set principal context: "+err.Error(), http.StatusInternalServerError)
		return
	}
	id, err := insertEvidenceLink(ctx, tx, identity.TenantID, req.FactID, req.CodeSymbolVersionID, req.LinkType, "explicit", confidence)
	if err != nil {
		http.Error(w, "insert evidence link: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"id": id, "fact_id": req.FactID, "code_symbol_version_id": req.CodeSymbolVersionID, "link_type": req.LinkType})
}

func (s *engineServer) handleFactForget(w http.ResponseWriter, r *http.Request, id string) {
	identity, status, msg := resolveIdentityFromRequest(r)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	cascade := r.URL.Query().Get("cascade") == "true"
	revoked, err := forgetFact(r.Context(), s.db, identity, id, cascade)
	if err == sql.ErrNoRows {
		http.Error(w, "not found", http.StatusNotFound)
		return
	} else if err != nil {
		http.Error(w, "forget fact: "+err.Error(), http.StatusInternalServerError)
		return
	}
	serviceMetrics.Count(telemetry.MetricUserModelChanges,
		map[string]string{"change": "fact_forgotten", "cascade": strconv.FormatBool(cascade)},
		telemetry.Correlation{Tenant: identity.TenantID})
	writeJSON(w, map[string]any{
		"forgotten":     true,
		"id":            id,
		"cascade":       cascade,
		"revoked_count": revoked,
		"revoked_at":    time.Now().UTC().Format(time.RFC3339),
	})
}

// handleFactFeedback implements scos_fact_feedback (mem0's feedback()):
// POST /v1/facts/{id}/feedback with {"feedback":"POSITIVE|NEGATIVE|VERY_NEGATIVE","reason":"..."}
// records quality signal against a fact without acting on it. See
// recordFactFeedback.
func (s *engineServer) handleFactFeedback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/facts/"), "/feedback")
	if id == "" {
		http.Error(w, "fact id is required", http.StatusBadRequest)
		return
	}
	var body struct {
		Feedback    string `json:"feedback"`
		Reason      string `json:"reason"`
		TenantID    string `json:"tenant_id,omitempty"`
		PrincipalID string `json:"principal_id,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	identity, status, msg := resolveIdentity(r, body.TenantID, body.PrincipalID)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	if _, err := withPrincipalContext(r.Context(), s.db, identity, func(q querier) (*Fact, error) {
		return getFact(r.Context(), q, identity.TenantID, id)
	}); err == sql.ErrNoRows {
		http.Error(w, "not found", http.StatusNotFound)
		return
	} else if err != nil {
		http.Error(w, "load fact: "+err.Error(), http.StatusInternalServerError)
		return
	}
	feedback := strings.ToUpper(strings.TrimSpace(body.Feedback))
	if err := recordFactFeedback(r.Context(), s.db, identity, id, feedback, body.Reason); err != nil {
		http.Error(w, "record feedback: "+err.Error(), http.StatusBadRequest)
		return
	}
	serviceMetrics.Count(telemetry.MetricUserModelChanges,
		map[string]string{"change": "fact_feedback", "feedback": feedback},
		telemetry.Correlation{Tenant: identity.TenantID})
	writeJSON(w, map[string]any{"recorded": true, "fact_id": id, "feedback": feedback})
}

// batchLimit caps batch fact operations at a defensible size: large enough
// for real agent workflows, small enough that one request can't tie up the
// database indefinitely or be used to bulk-drain the tenant one call at a time.
const batchLimit = 100

// handleFactsBatchCorrect implements scos_correct_batch (mem0's
// batch_update): POST /v1/facts/batch-correct with
// {"corrections":[{"memory_id":"...","value":"...","reason":"..."}]}.
// Each correction is applied independently in its own transaction (via
// correctFact) — one bad ID in the batch does not roll back the others.
func (s *engineServer) handleFactsBatchCorrect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Corrections []struct {
			MemoryID string `json:"memory_id"`
			Value    string `json:"value"`
			Reason   string `json:"reason"`
		} `json:"corrections"`
		TenantID    string `json:"tenant_id,omitempty"`
		PrincipalID string `json:"principal_id,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(body.Corrections) == 0 {
		http.Error(w, "corrections must not be empty", http.StatusBadRequest)
		return
	}
	if len(body.Corrections) > batchLimit {
		http.Error(w, fmt.Sprintf("corrections must not exceed %d items", batchLimit), http.StatusBadRequest)
		return
	}
	identity, status, msg := resolveIdentity(r, body.TenantID, body.PrincipalID)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}

	results := make([]map[string]any, 0, len(body.Corrections))
	succeeded := 0
	for _, c := range body.Corrections {
		if strings.TrimSpace(c.Value) == "" {
			results = append(results, map[string]any{"memory_id": c.MemoryID, "ok": false, "error": "content required"})
			continue
		}
		corrected, err := correctFact(r.Context(), s.db, identity, c.MemoryID, c.Value, c.Reason)
		if err != nil {
			results = append(results, map[string]any{"memory_id": c.MemoryID, "ok": false, "error": err.Error()})
			continue
		}
		succeeded++
		results = append(results, map[string]any{"memory_id": c.MemoryID, "ok": true, "fact": corrected})
	}
	serviceMetrics.Count(telemetry.MetricUserModelChanges,
		map[string]string{"change": "facts_batch_corrected"},
		telemetry.Correlation{Tenant: identity.TenantID})
	writeJSON(w, map[string]any{"succeeded": succeeded, "failed": len(results) - succeeded, "results": results})
}

// handleFactsBatchForget implements scos_forget_batch (mem0's batch_delete):
// POST /v1/facts/batch-forget with {"memory_ids":["...","..."],"cascade":false}.
// Each forget is applied independently (via forgetFact) — one bad ID in the
// batch does not fail the others.
func (s *engineServer) handleFactsBatchForget(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		MemoryIDs   []string `json:"memory_ids"`
		Cascade     bool     `json:"cascade"`
		TenantID    string   `json:"tenant_id,omitempty"`
		PrincipalID string   `json:"principal_id,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(body.MemoryIDs) == 0 {
		http.Error(w, "memory_ids must not be empty", http.StatusBadRequest)
		return
	}
	if len(body.MemoryIDs) > batchLimit {
		http.Error(w, fmt.Sprintf("memory_ids must not exceed %d items", batchLimit), http.StatusBadRequest)
		return
	}
	identity, status, msg := resolveIdentity(r, body.TenantID, body.PrincipalID)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}

	results := make([]map[string]any, 0, len(body.MemoryIDs))
	succeeded, totalRevoked := 0, 0
	for _, memoryID := range body.MemoryIDs {
		revoked, err := forgetFact(r.Context(), s.db, identity, memoryID, body.Cascade)
		if err != nil {
			results = append(results, map[string]any{"memory_id": memoryID, "ok": false, "error": err.Error()})
			continue
		}
		succeeded++
		totalRevoked += revoked
		results = append(results, map[string]any{"memory_id": memoryID, "ok": true, "revoked_count": revoked})
	}
	serviceMetrics.Count(telemetry.MetricUserModelChanges,
		map[string]string{"change": "facts_batch_forgotten", "cascade": strconv.FormatBool(body.Cascade)},
		telemetry.Correlation{Tenant: identity.TenantID})
	writeJSON(w, map[string]any{
		"succeeded": succeeded, "failed": len(results) - succeeded,
		"total_revoked": totalRevoked, "results": results,
	})
}

// ── entities ─────────────────────────────────────────────────────────────

func (s *engineServer) handleEntities(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		identity, status, msg := resolveIdentityFromRequest(r)
		if status != 0 {
			http.Error(w, msg, status)
			return
		}
		entities, err := withPrincipalContext(r.Context(), s.db, identity, func(q querier) ([]*Entity, error) {
			return loadEntities(r.Context(), q, identity.TenantID)
		})
		if err != nil {
			http.Error(w, "load entities: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, entities)

	case http.MethodPost:
		var entity Entity
		if err := json.NewDecoder(r.Body).Decode(&entity); err != nil {
			http.Error(w, "invalid entity: "+err.Error(), http.StatusBadRequest)
			return
		}
		if entity.Name == "" {
			http.Error(w, "name required", http.StatusBadRequest)
			return
		}
		identity, status, msg := resolveIdentity(r, entity.TenantID, entity.PrincipalID)
		if status != 0 {
			http.Error(w, msg, status)
			return
		}
		if entity.Type == "" {
			entity.Type = "user"
		}

		ctx := r.Context()
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			http.Error(w, "begin transaction: "+err.Error(), http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()
		if err := setPrincipalContext(ctx, tx, identity); err != nil {
			http.Error(w, "set principal context: "+err.Error(), http.StatusInternalServerError)
			return
		}
		entityID, err := insertEntity(ctx, tx, identity, &entity)
		if err != nil {
			http.Error(w, "failed to persist entity: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if err := tx.Commit(); err != nil {
			http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
			return
		}
		entity.ID = entityID
		writeJSON(w, entity)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *engineServer) handleEntityByID(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/representation") {
		s.handleEntityRepresentation(w, r)
		return
	}
	identity, status, msg := resolveIdentityFromRequest(r)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/entities/")
	entity, err := withPrincipalContext(r.Context(), s.db, identity, func(q querier) (*Entity, error) {
		return getEntity(r.Context(), q, identity.TenantID, id)
	})
	if err == sql.ErrNoRows {
		http.Error(w, "not found", http.StatusNotFound)
		return
	} else if err != nil {
		http.Error(w, "load entity: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, entity)
}

func (s *engineServer) handleDiary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	identity, status, msg := resolveIdentityFromRequest(r)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	entries, err := withPrincipalContext(r.Context(), s.db, identity, func(q querier) ([]any, error) {
		return loadDiary(r.Context(), q, identity.TenantID)
	})
	if err != nil {
		http.Error(w, "load diary: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, DashboardView{
		Type: "diary", Entries: entries, TotalCount: len(entries),
		LastUpdated: time.Now().UTC().Format(time.RFC3339),
	})
}

// ── beliefs ──────────────────────────────────────────────────────────────

func (s *engineServer) handleBeliefs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		identity, status, msg := resolveIdentityFromRequest(r)
		if status != 0 {
			http.Error(w, msg, status)
			return
		}
		beliefs, err := withPrincipalContext(r.Context(), s.db, identity, func(q querier) ([]*Belief, error) {
			return loadBeliefs(r.Context(), q, identity.TenantID)
		})
		if err != nil {
			http.Error(w, "load beliefs: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, beliefs)

	case http.MethodPost:
		var belief Belief
		if err := json.NewDecoder(r.Body).Decode(&belief); err != nil {
			http.Error(w, "invalid belief: "+err.Error(), http.StatusBadRequest)
			return
		}
		if belief.Conclusion == "" {
			http.Error(w, "conclusion required", http.StatusBadRequest)
			return
		}
		if len(belief.Evidence) == 0 {
			http.Error(w, "evidence_ids required: a belief must cite at least one verifiable fact", http.StatusBadRequest)
			return
		}
		identity, status, msg := resolveIdentity(r, belief.TenantID, belief.PrincipalID)
		if status != 0 {
			http.Error(w, msg, status)
			return
		}

		ctx := r.Context()
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			http.Error(w, "begin transaction: "+err.Error(), http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()
		if err := setPrincipalContext(ctx, tx, identity); err != nil {
			http.Error(w, "set principal context: "+err.Error(), http.StatusInternalServerError)
			return
		}
		for _, evidenceID := range belief.Evidence {
			if _, err := withPrincipalContext(ctx, s.db, identity, func(q querier) (*Fact, error) {
				return getFact(ctx, q, identity.TenantID, evidenceID)
			}); err != nil {
				http.Error(w, fmt.Sprintf("evidence_ids references unknown fact %q", evidenceID), http.StatusUnprocessableEntity)
				return
			}
		}
		belief.CreatedAt = time.Now().UTC().Format(time.RFC3339)
		beliefID, err := insertBelief(ctx, tx, identity, &belief)
		if err != nil {
			http.Error(w, "failed to persist belief: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if err := recordDiaryEntry(ctx, tx, identity, "belief_recorded", map[string]any{
			"belief_id": beliefID, "evidence_ids": belief.Evidence,
			"confidence": clampFloat(belief.Confidence, 0, 1), "summary": truncate(belief.Conclusion, 120),
		}); err != nil {
			http.Error(w, "failed to record diary entry: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if err := tx.Commit(); err != nil {
			http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
			return
		}
		belief.ID = beliefID
		// §32 belief revisions: a new belief over existing evidence is a
		// revision of the user model, so both measures move together.
		serviceMetrics.Count(telemetry.MetricBeliefRevisions,
			map[string]string{"evidence_count": strconv.Itoa(len(belief.Evidence))},
			telemetry.Correlation{Tenant: identity.TenantID})
		serviceMetrics.Count(telemetry.MetricUserModelChanges,
			map[string]string{"change": "belief_recorded"},
			telemetry.Correlation{Tenant: identity.TenantID})
		writeJSON(w, belief)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ── relations ────────────────────────────────────────────────────────────

func (s *engineServer) handleRelations(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		identity, status, msg := resolveIdentityFromRequest(r)
		if status != 0 {
			http.Error(w, msg, status)
			return
		}
		relations, err := withPrincipalContext(r.Context(), s.db, identity, func(q querier) ([]*TemporalRelation, error) {
			return loadRelations(r.Context(), q, identity.TenantID)
		})
		if err != nil {
			http.Error(w, "load relations: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, relations)

	case http.MethodPost:
		var req struct {
			TenantID       string `json:"tenant_id,omitempty"`
			PrincipalID    string `json:"principal_id,omitempty"`
			SourceMemoryID string `json:"source_memory_id"`
			TargetMemoryID string `json:"target_memory_id"`
			RelationType   string `json:"relation_type"`
			// Confidence is optional; when absent it is derived from the
			// two facts the relation connects (relationConfidenceFromFacts).
			Confidence *float64 `json:"confidence,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
			return
		}
		if req.SourceMemoryID == "" || req.TargetMemoryID == "" || req.RelationType == "" {
			http.Error(w, "source_memory_id, target_memory_id and relation_type are required", http.StatusBadRequest)
			return
		}
		if req.Confidence != nil && !validRelationConfidence(*req.Confidence) {
			http.Error(w, "confidence must be between 0 and 1", http.StatusBadRequest)
			return
		}
		identity, status, msg := resolveIdentity(r, req.TenantID, req.PrincipalID)
		if status != 0 {
			http.Error(w, msg, status)
			return
		}

		ctx := r.Context()
		// A relation links two existing fact IDs supplied by the caller.
		// Without verifying both belong to the caller's own tenant, a
		// caller in tenant A who merely learns (or guesses) a fact UUID
		// belonging to tenant B could link it into their own tenant's
		// memory graph -- surfacing a foreign tenant's memory ID (and its
		// existence) through GET /v1/relations and the dashboard memory
		// graph. Both endpoints already trust every relation row they load
		// as belonging to the caller, so ownership has to be enforced here,
		// at write time, exactly as handleBeliefs already does for
		// evidence_ids.
		source, err := withPrincipalContext(ctx, s.db, identity, func(q querier) (*Fact, error) {
			return getFact(ctx, q, identity.TenantID, req.SourceMemoryID)
		})
		if err != nil {
			http.Error(w, fmt.Sprintf("source_memory_id references unknown fact %q", req.SourceMemoryID), http.StatusUnprocessableEntity)
			return
		}
		target, err := withPrincipalContext(ctx, s.db, identity, func(q querier) (*Fact, error) {
			return getFact(ctx, q, identity.TenantID, req.TargetMemoryID)
		})
		if err != nil {
			http.Error(w, fmt.Sprintf("target_memory_id references unknown fact %q", req.TargetMemoryID), http.StatusUnprocessableEntity)
			return
		}
		confidence, confidenceSource := relationConfidenceFromFacts(source, target), "evidence"
		if req.Confidence != nil {
			confidence, confidenceSource = *req.Confidence, "request"
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			http.Error(w, "begin transaction: "+err.Error(), http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()
		if err := setPrincipalContext(ctx, tx, identity); err != nil {
			http.Error(w, "set principal context: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if err := insertRelation(ctx, tx, req.SourceMemoryID, req.TargetMemoryID, req.RelationType, confidence); err != nil {
			http.Error(w, "insert relation: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if err := tx.Commit(); err != nil {
			http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{
			"created":           true,
			"source_memory_id":  req.SourceMemoryID,
			"target_memory_id":  req.TargetMemoryID,
			"relation_type":     req.RelationType,
			"confidence":        confidence,
			"confidence_source": confidenceSource,
		})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ── dashboard views ──────────────────────────────────────────────────────

// versionInfo is the shape every governed service's GET /v1/version returns.
type versionInfo struct {
	Status        string `json:"status"`
	Version       string `json:"version"`
	UptimeSeconds int    `json:"uptime_seconds"`
}

func (s *engineServer) probePeer(ctx context.Context, name, base string) SystemMapEntry {
	entry := SystemMapEntry{Service: name, Status: "unreachable", Version: "unknown", Uptime: "unknown"}
	client := s.httpClient
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Second, Transport: telemetry.ProcessTransport(nil)}
	}

	readyReq, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/health/ready", nil)
	if err != nil {
		return entry
	}
	readyResp, err := client.Do(readyReq)
	if err != nil {
		return entry
	}
	readyResp.Body.Close()
	if readyResp.StatusCode == http.StatusOK {
		entry.Status = "healthy"
	} else {
		entry.Status = "degraded"
	}

	// /health/version is served by every service (packages/release), without
	// credentials: the build it runs and how long the process has been up.
	versionReq, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/health/version", nil)
	if err != nil {
		return entry
	}
	versionResp, err := client.Do(versionReq)
	if err != nil {
		entry.Detail = "version probe failed: " + err.Error()
		return entry
	}
	defer versionResp.Body.Close()
	if versionResp.StatusCode != http.StatusOK {
		entry.Detail = fmt.Sprintf("version probe answered %d", versionResp.StatusCode)
		return entry
	}
	var info release.Info
	if err := json.NewDecoder(versionResp.Body).Decode(&info); err != nil {
		entry.Detail = "version response unreadable: " + err.Error()
		return entry
	}
	if info.SourceSHA != "" && info.SourceSHA != "unknown" {
		entry.Version = info.SourceSHA
		if len(entry.Version) > 12 {
			entry.Version = entry.Version[:12]
		}
	}
	// A build that predates uptime reporting sends no started_at; its
	// uptime stays unknown rather than reading as zero.
	if info.StartedAt != "" {
		entry.Uptime = (time.Duration(info.UptimeSeconds) * time.Second).String()
	}
	return entry
}

func (s *engineServer) handleDashboardSystem(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	selfUptime := time.Since(s.startTime)
	peers := s.peers
	s.mu.RUnlock()
	selfStatus := "healthy"
	if err := s.readinessCheck(); err != nil {
		selfStatus = "degraded"
	}

	entries := make([]any, 0, len(peerDefaults)+1)
	entries = append(entries, SystemMapEntry{
		Service: "memory-intelligence",
		Status:  selfStatus,
		Version: buildVersion(),
		Uptime:  selfUptime.Round(time.Second).String(),
	})

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	type result struct {
		index int
		entry SystemMapEntry
	}
	resultsCh := make(chan result, len(peerDefaults))
	for i, d := range peerDefaults {
		base := peers[d.name]
		if base == "" {
			base = d.fallback
		}
		go func(i int, name, base string) {
			resultsCh <- result{index: i, entry: s.probePeer(ctx, name, base)}
		}(i, d.name, base)
	}
	ordered := make([]SystemMapEntry, len(peerDefaults))
	for range peerDefaults {
		r := <-resultsCh
		ordered[r.index] = r.entry
	}
	for _, e := range ordered {
		entries = append(entries, e)
	}

	writeJSON(w, DashboardView{
		Type:        "system_map",
		Entries:     entries,
		TotalCount:  len(entries),
		LastUpdated: time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *engineServer) handleDashboardMemoryGraph(w http.ResponseWriter, r *http.Request) {
	identity, status, msg := resolveIdentityFromRequest(r)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	ctx := r.Context()
	facts, err := withPrincipalContext(ctx, s.db, identity, func(q querier) ([]*Fact, error) {
		return loadFacts(ctx, q, identity.TenantID, "")
	})
	if err != nil {
		http.Error(w, "load facts: "+err.Error(), http.StatusInternalServerError)
		return
	}
	entities, err := withPrincipalContext(ctx, s.db, identity, func(q querier) ([]*Entity, error) {
		return loadEntities(ctx, q, identity.TenantID)
	})
	if err != nil {
		http.Error(w, "load entities: "+err.Error(), http.StatusInternalServerError)
		return
	}
	relations, err := withPrincipalContext(ctx, s.db, identity, func(q querier) ([]*TemporalRelation, error) {
		return loadRelations(ctx, q, identity.TenantID)
	})
	if err != nil {
		http.Error(w, "load relations: "+err.Error(), http.StatusInternalServerError)
		return
	}

	nodes := make([]map[string]any, 0, len(facts)+len(entities))
	for _, f := range facts {
		nodes = append(nodes, map[string]any{
			"id": f.ID, "label": truncate(f.Content, 60),
			"type": "fact", "network": f.Network,
		})
	}
	for _, e := range entities {
		nodes = append(nodes, map[string]any{
			"id": e.ID, "label": e.Name, "type": "entity",
		})
	}
	edges := make([]map[string]any, 0, len(relations))
	for _, rel := range relations {
		edges = append(edges, map[string]any{
			"source": rel.SourceID, "target": rel.TargetID,
			"type": rel.Type, "confidence": rel.Confidence,
		})
	}
	// Entity-mention edges: linkEntityMentions (store.go) records, on every
	// fact write, which entities that fact's content mentions. Surface those
	// here so the graph shows entity<->fact links the way mem0's entity
	// store does, not just fact<->fact relations.
	for _, e := range entities {
		for _, factID := range e.Facts {
			edges = append(edges, map[string]any{
				"source": e.ID, "target": factID, "type": "mentions",
			})
		}
	}

	writeJSON(w, map[string]any{
		"type":  "memory_graph",
		"nodes": nodes,
		"edges": edges,
	})
}

func (s *engineServer) handleDashboardFacts(w http.ResponseWriter, r *http.Request) {
	identity, status, msg := resolveIdentityFromRequest(r)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	network := r.URL.Query().Get("network")
	loader := loadFacts
	if r.URL.Query().Get("include_merged") == "true" {
		loader = loadFactsIncludingMerged
	}
	facts, err := withPrincipalContext(r.Context(), s.db, identity, func(q querier) ([]*Fact, error) {
		return loader(r.Context(), q, identity.TenantID, network)
	})
	if err != nil {
		http.Error(w, "load facts: "+err.Error(), http.StatusInternalServerError)
		return
	}
	sort.Slice(facts, func(i, j int) bool {
		return facts[i].RecordedAt > facts[j].RecordedAt
	})
	writeJSON(w, DashboardView{
		Type:        "facts",
		Entries:     toAnySlice(facts),
		TotalCount:  len(facts),
		LastUpdated: time.Now().UTC().Format(time.RFC3339),
	})
}

// handleDashboardSummary implements scos_memory_summary (mem0 Platform's
// get_summary): GET /v1/dashboard/summary?network=&max_facts= produces a
// natural-language summary of the caller's facts. When llmClient is
// configured it asks the model for a real summary; otherwise it falls back
// to a deterministic network-grouped digest and says so explicitly in
// "mode" -- §42 forbids presenting the fallback as if it were model-
// generated prose.
func (s *engineServer) handleDashboardSummary(w http.ResponseWriter, r *http.Request) {
	identity, status, msg := resolveIdentityFromRequest(r)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	network := r.URL.Query().Get("network")
	facts, err := withPrincipalContext(r.Context(), s.db, identity, func(q querier) ([]*Fact, error) {
		return loadFacts(r.Context(), q, identity.TenantID, network)
	})
	if err != nil {
		http.Error(w, "load facts: "+err.Error(), http.StatusInternalServerError)
		return
	}
	sort.Slice(facts, func(i, j int) bool { return facts[i].RecordedAt > facts[j].RecordedAt })

	maxFacts := 200
	if raw := r.URL.Query().Get("max_facts"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			maxFacts = clampInt(n, 1, 500)
		}
	}
	if len(facts) > maxFacts {
		facts = facts[:maxFacts]
	}

	if len(facts) == 0 {
		writeJSON(w, map[string]any{
			"type": "summary", "mode": "empty", "summary": "No active facts to summarize.",
			"fact_count": 0, "last_updated": time.Now().UTC().Format(time.RFC3339),
		})
		return
	}

	summary, themes, err := summarizeFactsWithLLM(r.Context(), facts)
	if err == nil {
		writeJSON(w, map[string]any{
			"type": "summary", "mode": "llm_generated", "summary": summary, "themes": themes,
			"fact_count": len(facts), "last_updated": time.Now().UTC().Format(time.RFC3339),
		})
		return
	}
	// Degrading to the deterministic digest is honest (§42) but must never be
	// silent: an operator reading the logs has to be able to tell a real
	// outage from a healthy no-LLM deployment.
	log.Printf("dashboard summary: LLM call failed, serving deterministic fallback: %v", err)

	writeJSON(w, map[string]any{
		"type": "summary", "mode": "deterministic_fallback",
		"summary":      summarizeFactsDeterministic(facts),
		"fact_count":   len(facts),
		"last_updated": time.Now().UTC().Format(time.RFC3339),
	})
}

// summarizeFactsWithLLM asks llmClient for a short prose summary plus a
// handful of named themes across facts. Returns an error (never a fabricated
// summary) if the client is disabled or the call fails, so the caller can
// fall back honestly.
func summarizeFactsWithLLM(ctx context.Context, facts []*Fact) (string, []string, error) {
	if !llmClient.Enabled() {
		return "", nil, llmclient.ErrDisabled
	}
	var b strings.Builder
	for i, f := range facts {
		fmt.Fprintf(&b, "%d. [%s] %s\n", i+1, f.Network, truncateContent(f.Content, 300))
	}
	llmCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	var result struct {
		Summary string   `json:"summary"`
		Themes  []string `json:"themes"`
	}
	err := llmClient.ChatJSON(llmCtx,
		`You summarize a list of stored memory facts for a user. Respond with a JSON object of the exact shape {"summary": "2-4 sentence prose summary", "themes": ["short theme 1", "short theme 2"]}. Be factual and concise; do not invent facts not present in the list. List at most 6 themes.`,
		b.String(), &result)
	if err != nil {
		return "", nil, err
	}
	if strings.TrimSpace(result.Summary) == "" {
		return "", nil, fmt.Errorf("model returned an empty summary")
	}
	return result.Summary, result.Themes, nil
}

// summarizeFactsDeterministic is the no-LLM fallback: a network-grouped
// count digest. It makes no claim to prose quality — it is data, formatted
// as a sentence, not a generated summary.
func summarizeFactsDeterministic(facts []*Fact) string {
	counts := make(map[MemoryNetwork]int)
	for _, f := range facts {
		counts[f.Network]++
	}
	networks := make([]string, 0, len(counts))
	for n := range counts {
		networks = append(networks, string(n))
	}
	sort.Strings(networks)
	parts := make([]string, 0, len(networks))
	for _, n := range networks {
		parts = append(parts, fmt.Sprintf("%d %s", counts[MemoryNetwork(n)], n))
	}
	return fmt.Sprintf("%d active facts: %s.", len(facts), strings.Join(parts, ", "))
}

func (s *engineServer) handleDashboardLearnings(w http.ResponseWriter, r *http.Request) {
	identity, status, msg := resolveIdentityFromRequest(r)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	beliefs, err := withPrincipalContext(r.Context(), s.db, identity, func(q querier) ([]*Belief, error) {
		return loadBeliefs(r.Context(), q, identity.TenantID)
	})
	if err != nil {
		http.Error(w, "load beliefs: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, DashboardView{
		Type:        "learnings",
		Entries:     toAnySlice(beliefs),
		TotalCount:  len(beliefs),
		LastUpdated: time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *engineServer) handleDashboardContradictions(w http.ResponseWriter, r *http.Request) {
	identity, status, msg := resolveIdentityFromRequest(r)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	ctx := r.Context()
	relations, err := withPrincipalContext(ctx, s.db, identity, func(q querier) ([]*TemporalRelation, error) {
		return loadRelations(ctx, q, identity.TenantID)
	})
	if err != nil {
		http.Error(w, "load relations: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Load every fact once and index by ID. The previous N+1 (getFact per
	// relation endpoint, each loading the full fact set) turned ~1k relations
	// into ~2k full-table scans; with a big relation set the contradictions
	// panel took minutes. One query, one map, O(1) lookups.
	facts, err := withPrincipalContext(ctx, s.db, identity, func(q querier) ([]*Fact, error) {
		return loadFacts(ctx, q, identity.TenantID, "")
	})
	if err != nil {
		http.Error(w, "load facts: "+err.Error(), http.StatusInternalServerError)
		return
	}
	byID := make(map[string]*Fact, len(facts))
	for _, f := range facts {
		byID[f.ID] = f
	}
	var contra []map[string]any
	for _, rel := range relations {
		if rel.Type != "contradicts" {
			continue
		}
		f1, ok1 := byID[rel.SourceID]
		f2, ok2 := byID[rel.TargetID]
		if ok1 && ok2 {
			contra = append(contra, map[string]any{
				"fact_a":       f1.Content,
				"fact_b":       f2.Content,
				"source_a":     f1.Source,
				"source_b":     f2.Source,
				"confidence_a": f1.Confidence,
				"confidence_b": f2.Confidence,
			})
		}
	}
	writeJSON(w, DashboardView{
		Type:        "contradictions",
		Entries:     toAnySlice(contra),
		TotalCount:  len(contra),
		LastUpdated: time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *engineServer) handleDashboardTimeline(w http.ResponseWriter, r *http.Request) {
	identity, status, msg := resolveIdentityFromRequest(r)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	facts, err := withPrincipalContext(r.Context(), s.db, identity, func(q querier) ([]*Fact, error) {
		return loadFacts(r.Context(), q, identity.TenantID, "")
	})
	if err != nil {
		http.Error(w, "load facts: "+err.Error(), http.StatusInternalServerError)
		return
	}
	var entries []map[string]any
	for _, f := range facts {
		entries = append(entries, map[string]any{
			"time": f.RecordedAt, "type": "fact",
			"content": truncate(f.Content, 80),
			"source":  f.Source,
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i]["time"].(string) > entries[j]["time"].(string)
	})
	if len(entries) > 100 {
		entries = entries[:100]
	}
	writeJSON(w, DashboardView{
		Type:        "timeline",
		Entries:     toAnySlice(entries),
		TotalCount:  len(entries),
		LastUpdated: time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *engineServer) handleDashboardUserModel(w http.ResponseWriter, r *http.Request) {
	identity, status, msg := resolveIdentityFromRequest(r)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	ctx := r.Context()
	entities, err := withPrincipalContext(ctx, s.db, identity, func(q querier) ([]*Entity, error) {
		return loadEntities(ctx, q, identity.TenantID)
	})
	if err != nil {
		http.Error(w, "load entities: "+err.Error(), http.StatusInternalServerError)
		return
	}
	var entityList []*Entity
	for _, e := range entities {
		if e.Type == "user" {
			entityList = append(entityList, e)
		}
	}
	var userFacts []*Fact
	for _, e := range entityList {
		for _, factID := range e.Facts {
			if f, err := withPrincipalContext(ctx, s.db, identity, func(q querier) (*Fact, error) {
				return getFact(ctx, q, identity.TenantID, factID)
			}); err == nil {
				userFacts = append(userFacts, f)
			}
		}
	}

	writeJSON(w, map[string]any{
		"type":         "user_model",
		"entities":     entityList,
		"facts":        userFacts,
		"fact_count":   len(userFacts),
		"last_updated": time.Now().UTC().Format(time.RFC3339),
	})
}

// ── SSE stream (§25.12) ──────────────────────────────────────────────────

func (s *engineServer) handleStream(w http.ResponseWriter, r *http.Request) {
	identity, status, msg := resolveIdentityFromRequest(r)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	if s.db == nil {
		http.Error(w, "event store unavailable", http.StatusServiceUnavailable)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	lastSequence := int64(0)
	if raw := strings.TrimSpace(r.Header.Get("Last-Event-ID")); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 0 {
			http.Error(w, "Last-Event-ID must be a non-negative stream sequence", http.StatusBadRequest)
			return
		}
		lastSequence = parsed
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	writeTransparencySSE(w, "connected", transparencyStreamFrame{
		EventID: "0", Sequence: lastSequence, Timestamp: time.Now().UTC(),
		Source: "scos.memory-intelligence", TenantID: identity.TenantID,
		Scope: "tenant", ProjectionVersion: "events-v1", Data: map[string]any{"status": "live", "version": buildVersion()},
	})
	flusher.Flush()

	poll := time.NewTicker(time.Second)
	defer poll.Stop()

	for {
		select {
		case <-poll.C:
			events, err := loadTransparencyEvents(r.Context(), s.db, identity.TenantID, lastSequence, 100)
			if err != nil {
				// Headers and the "connected" frame are already flushed —
				// http.Error here would emit a superfluous WriteHeader. Log
				// and terminate the stream; the client reconnects.
				log.Printf("transparency stream: load events: %v", err)
				return
			}
			for _, event := range events {
				writeTransparencySSE(w, event.EventType, event.Frame)
				lastSequence = event.Frame.Sequence
			}
			if len(events) == 0 {
				writeTransparencySSE(w, "heartbeat", transparencyStreamFrame{
					EventID: strconv.FormatInt(lastSequence, 10), Sequence: lastSequence,
					Timestamp: time.Now().UTC(), Source: "scos.memory-intelligence",
					TenantID: identity.TenantID, Scope: "tenant", ProjectionVersion: "events-v1",
					Data: map[string]any{"status": "live"},
				})
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

type transparencyStreamFrame struct {
	EventID           string         `json:"event_id"`
	Sequence          int64          `json:"sequence"`
	Timestamp         time.Time      `json:"timestamp"`
	Source            string         `json:"source"`
	TenantID          string         `json:"tenant_id"`
	Scope             string         `json:"scope"`
	ProjectionVersion string         `json:"projection_version"`
	Data              map[string]any `json:"data"`
}

type transparencyEvent struct {
	EventType string
	Frame     transparencyStreamFrame
}

func writeTransparencySSE(w http.ResponseWriter, eventType string, frame transparencyStreamFrame) {
	raw, err := json.Marshal(frame)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", frame.EventID, eventType, raw)
}

func loadTransparencyEvents(ctx context.Context, db *sql.DB, tenantID string, after int64, limit int) ([]transparencyEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('scos.tenant_id',$1,true)`, tenantID); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT stream_sequence, id::text, source, event_type, occurred_at,
		       COALESCE(data->>'scope','tenant'), COALESCE(data->>'projection_version','events-v1'), data
		FROM events
		WHERE tenant_id=$1 AND stream_sequence>$2
		ORDER BY stream_sequence
		LIMIT $3
	`, tenantID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]transparencyEvent, 0)
	for rows.Next() {
		var sequence int64
		var frame transparencyStreamFrame
		var eventType string
		var dataRaw []byte
		if err := rows.Scan(&sequence, &frame.EventID, &frame.Source, &eventType, &frame.Timestamp, &frame.Scope, &frame.ProjectionVersion, &dataRaw); err != nil {
			return nil, err
		}
		frame.Sequence = sequence
		frame.TenantID = tenantID
		if err := json.Unmarshal(dataRaw, &frame.Data); err != nil {
			return nil, err
		}
		result = append(result, transparencyEvent{EventType: eventType, Frame: frame})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, tx.Commit()
}

// ── contradiction detection ──────────────────────────────────────────────

// ── Dream-style Merge (§16/§25) ─────────────────────────────────────────

// keyValueNorm splits content's significant (len > 3) words into a leading
// key_norm and trailing value_norm, the coarse heuristic P0-C's plan
// recommends: not reliable NLP subject/predicate extraction, just enough to
// catch same-polarity conflicts sharing a leading phrase ("ships on
// tuesdays" vs "ships on thursdays") that isContradictory's negation check
// structurally cannot. keyWords bounds how many leading words form the key;
// the rest is the value. Returns ("", "") when content has no significant
// words to split.
const keyNormWords = 3

func keyValueNorm(content string) (keyNorm, valueNorm string) {
	// A memory that names its key outright ("Email: ...", "Phase 3 Task 7
	// done: ...") is split there, so two different labels never read as one
	// key; anything else keeps the leading-words heuristic.
	if key, value, ok := explicitKey(content); ok {
		return strings.Join(key, " "), strings.Join(value, " ")
	}
	words := significantWords(content)
	if len(words) == 0 {
		return "", ""
	}
	n := keyNormWords
	if n > len(words) {
		n = len(words)
	}
	keyNorm = strings.Join(words[:n], " ")
	valueNorm = strings.Join(words[n:], " ")
	return keyNorm, valueNorm
}

// explicitKey splits "label: value" content at its first colon when the
// label is short (at most maxExplicitKeyWords words) and the colon is not
// part of a URL scheme or a clock time.
func explicitKey(content string) (key, value []string, ok bool) {
	text := redactionMarker.ReplaceAllString(content, " ")
	for i := 0; i < len(text); i++ {
		if text[i] != ':' {
			continue
		}
		if strings.HasPrefix(text[i:], "://") || (i > 0 && i+1 < len(text) && isASCIIDigit(text[i-1]) && isASCIIDigit(text[i+1])) {
			continue
		}
		label := strings.Fields(text[:i])
		if len(label) == 0 || len(label) > maxExplicitKeyWords {
			return nil, nil, false
		}
		key, value = significantWords(text[:i]), significantWords(text[i+1:])
		return key, value, len(key) > 0
	}
	return nil, nil, false
}

const maxExplicitKeyWords = 8

func isASCIIDigit(b byte) bool { return b >= '0' && b <= '9' }

// redactionMarker is what the secret scanner leaves where it removed a
// secret. It says nothing about what the memory is about, so it never forms
// part of a key: two memories that each had a different token redacted are
// not two values for one key.
var redactionMarker = regexp.MustCompile(`\[REDACTED:[a-z0-9_]+\]`)

func significantWords(content string) []string {
	words := strings.Fields(strings.ToLower(redactionMarker.ReplaceAllString(content, " ")))
	out := make([]string, 0, len(words))
	for _, w := range words {
		// Edge punctuation is not part of the word: "scos." restates "scos".
		w = strings.TrimFunc(w, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
		// Short words are mostly filler, but a short token with a digit
		// identifies something ("task 7", "v2", "p95"): dropping it made
		// "Phase 3 Task 7 done" and "Phase 3 Task 5 done" share a key.
		if len(w) > 3 || (w != "" && strings.ContainsAny(w, "0123456789")) {
			out = append(out, w)
		}
	}
	return out
}

// mergeStopwords carry no information of their own, so a statement that
// differs from another only by them still restates it.
var mergeStopwords = map[string]bool{
	"a": true, "an": true, "the": true, "is": true, "are": true, "was": true, "were": true, "be": true,
	"of": true, "to": true, "for": true, "in": true, "on": true, "at": true, "by": true, "with": true,
	"from": true, "as": true, "and": true, "or": true, "it": true, "its": true, "this": true, "that": true,
}

// informationTokens is every word and number in content, lower-cased,
// short ones included (a "v1" against a "v2" is the whole difference),
// minus mergeStopwords.
func informationTokens(content string) map[string]bool {
	out := map[string]bool{}
	for _, tok := range strings.FieldsFunc(strings.ToLower(content), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if !mergeStopwords[tok] {
			out[tok] = true
		}
	}
	return out
}

// restates reports whether one statement's information tokens are all
// contained in the other's: the two say the same thing, or one says it
// with more detail. When each has a token the other lacks they state
// different things, however many words they share.
func restates(a, b string) bool {
	at, bt := informationTokens(a), informationTokens(b)
	subset := func(x, y map[string]bool) bool {
		for tok := range x {
			if !y[tok] {
				return false
			}
		}
		return true
	}
	return subset(at, bt) || subset(bt, at)
}

// overlapScore is the fraction of significant words (len > 3) the shorter
// of a/b's content shares with the longer, restricted to the same
// network/scope. It is a cheap shortlist filter, not the merge decision
// itself -- see findMergeCandidate.
func overlapScore(a, b *Fact) float64 {
	if a.Network != b.Network || a.Scope != b.Scope {
		return 0
	}
	aWords := significantWords(a.Content)
	bWords := significantWords(b.Content)
	if len(aWords) == 0 || len(bWords) == 0 {
		return 0
	}
	bSet := make(map[string]bool, len(bWords))
	for _, w := range bWords {
		bSet[w] = true
	}
	seen := make(map[string]bool)
	shared := 0
	for _, w := range aWords {
		if bSet[w] && !seen[w] {
			shared++
			seen[w] = true
		}
	}
	shorter := len(aWords)
	if len(bWords) < shorter {
		shorter = len(bWords)
	}
	return float64(shared) / float64(shorter)
}

// mergeDuplicateThreshold is the minimum heuristic overlap score before a
// candidate is even worth an LLM call to confirm as a duplicate -- keeps
// the LLM call bounded to genuinely close candidates instead of running on
// every write.
const mergeDuplicateThreshold = 0.5
const maxMergeCandidates = 3

type mergeDecision struct {
	ExistingFact *Fact
	NewIsRicher  bool
	// Score is the significant-word overlap between the two statements
	// (overlapScore), the measured basis for treating them as one.
	Score float64
}

// findMergeCandidate implements Dream's Merge duplicate-detection: a new
// fact that's an exact or near-exact restatement of an existing one should
// fold into a single canonical memory rather than create a second copy.
// isContradictory candidates are excluded up front -- a fact that
// contradicts an existing one belongs on the correction path
// (scos_correct), not the duplicate path. Returns nil when nothing looks
// like a duplicate, which is the common case.
func findMergeCandidate(ctx context.Context, fact *Fact, existing []*Fact) *mergeDecision {
	type scored struct {
		fact  *Fact
		score float64
	}
	var candidates []scored
	for _, other := range existing {
		if isContradictory(fact, other) {
			continue
		}
		if score := overlapScore(fact, other); score >= mergeDuplicateThreshold {
			candidates = append(candidates, scored{other, score})
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].score > candidates[j].score })
	if len(candidates) > maxMergeCandidates {
		candidates = candidates[:maxMergeCandidates]
	}

	if !llmClient.Enabled() {
		// Deterministic fallback: only trust a very high heuristic overlap,
		// and keep whichever content is longer as canonical.
		// Word overlap alone would fold "project alpha ..." into "project
		// beta ...": the one word that differs is the information. So the
		// fallback also requires one statement to say nothing the other
		// does not (restates), i.e. a repeat or a strictly richer version.
		for _, c := range candidates {
			if c.score >= 0.85 && restates(fact.Content, c.fact.Content) {
				return &mergeDecision{ExistingFact: c.fact, NewIsRicher: len(fact.Content) > len(c.fact.Content), Score: c.score}
			}
		}
		return nil
	}

	var options strings.Builder
	for i, c := range candidates {
		fmt.Fprintf(&options, "%d. id=%s: %s\n", i, c.fact.ID, c.fact.Content)
	}
	llmCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var result struct {
		IsDuplicate    bool   `json:"is_duplicate"`
		CandidateIndex int    `json:"candidate_index"`
		Richer         string `json:"richer"`
	}
	err := llmClient.ChatJSON(llmCtx,
		`You decide whether a new fact is a duplicate or near-duplicate restatement of any of the given existing facts (not a correction or contradiction -- those are a different case and won't appear here). Respond with a JSON object of the exact shape {"is_duplicate": true or false, "candidate_index": the numbered index of the matching existing fact if is_duplicate is true else -1, "richer": "new" or "existing" -- whichever states the same information more completely}. Be conservative: only mark is_duplicate true when the new fact and the existing one genuinely restate the same underlying information.`,
		fmt.Sprintf("New fact: %s\n\nExisting candidates:\n%s", fact.Content, options.String()),
		&result)
	if err != nil || !result.IsDuplicate || result.CandidateIndex < 0 || result.CandidateIndex >= len(candidates) {
		return nil
	}
	chosen := candidates[result.CandidateIndex]
	return &mergeDecision{ExistingFact: chosen.fact, NewIsRicher: result.Richer != "existing", Score: chosen.score}
}

// ── utility ──────────────────────────────────────────────────────────────

func truncateContent(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return truncate(s, n) + "..."
}

func toAnySlice[T any](items []T) []any {
	result := make([]any, len(items))
	for i, item := range items {
		result[i] = item
	}
	return result
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("encode response: %v", err)
	}
}
