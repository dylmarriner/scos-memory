package main

import "github.com/dylmarriner/scos-memory/packages/auth"
import "github.com/dylmarriner/scos-memory/packages/tlsserve"

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
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
var serviceMetrics = telemetry.FromEnvironment("memory-intelligence")

const version = "0.1.0"

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

type Fact struct {
	ID           string        `json:"id"`
	Network      MemoryNetwork `json:"network"`
	Content      string        `json:"content"`
	Source       string        `json:"source"` // explicit, observed, inferred, external
	Authority    int           `json:"authority_level"`
	Confidence   float64       `json:"confidence"`
	Scope        string        `json:"scope"`
	ObservedAt   string        `json:"observed_at"`
	ValidFrom    string        `json:"valid_from,omitempty"`
	ValidUntil   string        `json:"valid_until,omitempty"`
	RecordedAt   string        `json:"recorded_at"`
	SupersededBy string        `json:"superseded_by,omitempty"`
	Evidence     []string      `json:"evidence,omitempty"`
	Inferred     bool          `json:"inferred"`
	Derivation   string        `json:"derivation,omitempty"`
	Revision     int           `json:"revision"`

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

	TenantID    string `json:"tenant_id,omitempty"`
	PrincipalID string `json:"principal_id,omitempty"`
}

type TemporalRelation struct {
	SourceID  string `json:"source_id"`
	TargetID  string `json:"target_id"`
	Type      string `json:"type"` // derived_from, contradicts, supersedes, evidence_for, used_in
	Scope     string `json:"scope"`
	CreatedAt string `json:"created_at"`
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
	ID        string          `json:"id"`
	Level     MemoryHierarchy `json:"level"`
	Name      string          `json:"name"`
	ParentID  string          `json:"parent_id,omitempty"`
	Scope     string          `json:"scope"`
	Summary   string          `json:"summary,omitempty"`
	FactCount int             `json:"fact_count"`
	UpdatedAt string          `json:"updated_at"`

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
}

// peerDefault names an environment variable and its fallback base URL for a
// service reported in the real-time system map.
type peerDefault struct {
	name     string
	envVar   string
	fallback string
}

var peerDefaults = []peerDefault{
	{"memory-gateway", "SCOS_MEMORY_GATEWAY_URL", "https://memory-gateway:8080"},
	{"retrieval-orchestrator", "SCOS_RETRIEVAL_ORCHESTRATOR_URL", "https://retrieval-orchestrator:8082"},
	{"context-optimisation", "SCOS_CONTEXT_OPTIMISATION_URL", "https://context-optimisation:8083"},
	{"code-intelligence", "SCOS_CODE_INTELLIGENCE_URL", "https://code-intelligence:8084"},
	{"execution-proxy", "SCOS_EXECUTION_PROXY_URL", "https://execution-proxy:8085"},
	{"extension-manager", "SCOS_EXTENSION_MANAGER_URL", "https://extension-manager:8086"},
	{"compatibility-gateway", "SCOS_COMPATIBILITY_GATEWAY_URL", "https://compatibility-gateway:8087"},
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
		Version:       version,
		UptimeSeconds: int(uptime.Seconds()),
	})
}

func (s *engineServer) readinessCheck() error {
	if s.db == nil {
		return fmt.Errorf("postgres dependency is not configured")
	}
	return s.db.PingContext(context.Background())
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

	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", healthOK)
	mux.HandleFunc("/health/ready", readiness.Handler(func() error {
		return srv.readinessCheck()
	}))
	mux.HandleFunc("/v1/version", srv.handleVersion)

	// Memory networks
	mux.HandleFunc("/v1/facts", srv.handleFacts)
	mux.HandleFunc("/v1/memory", srv.handleFacts)
	mux.HandleFunc("/v1/facts/", srv.handleFactByID)
	mux.HandleFunc("/v1/entities", srv.handleEntities)
	mux.HandleFunc("/v1/entities/", srv.handleEntityByID)
	mux.HandleFunc("/v1/beliefs", srv.handleBeliefs)
	mux.HandleFunc("/v1/relations", srv.handleRelations)
	mux.HandleFunc("/v1/hierarchy", srv.handleHierarchy)

	// Reflection
	mux.HandleFunc("/v1/reflect", srv.handleReflect)

	// Dashboard views (§25.9)
	mux.HandleFunc("/v1/dashboard/system", srv.handleDashboardSystem)
	mux.HandleFunc("/v1/dashboard/memory-graph", srv.handleDashboardMemoryGraph)
	mux.HandleFunc("/v1/dashboard/facts", srv.handleDashboardFacts)
	mux.HandleFunc("/v1/dashboard/learnings", srv.handleDashboardLearnings)
	mux.HandleFunc("/v1/dashboard/contradictions", srv.handleDashboardContradictions)
	mux.HandleFunc("/v1/dashboard/timeline", srv.handleDashboardTimeline)
	mux.HandleFunc("/v1/dashboard/user-model", srv.handleDashboardUserModel)
	mux.HandleFunc("/v1/memory/graph", srv.handleDashboardMemoryGraph)
	mux.HandleFunc("/v1/learnings", srv.handleDashboardLearnings)
	mux.HandleFunc("/v1/diary", srv.handleDiary)
	mux.HandleFunc("/v1/stream", srv.handleStream) // SSE
	mux.HandleFunc("/v1/transparency/stream", srv.handleStream)
	mux.HandleFunc("/v1/users/", srv.handleUserKnowledge)

	addr := os.Getenv("SCOS_MEMORY_INTELLIGENCE_HTTP_ADDR")
	if addr == "" {
		addr = ":8089"
	}
	log.Printf("memory intelligence engine listening on %s", addr)
	if err := tlsserve.ListenAndServe(addr, auth.AuthenticatedHandler(mux, auth.MustFromEnvironment())); err != nil {
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
	entity, err := getEntity(ctx, s.db, identity.TenantID, id)
	if err == sql.ErrNoRows {
		http.Error(w, "not found", http.StatusNotFound)
		return
	} else if err != nil {
		http.Error(w, "load entity: "+err.Error(), http.StatusInternalServerError)
		return
	}
	facts := make([]*Fact, 0, len(entity.Facts))
	for _, factID := range entity.Facts {
		if fact, err := getFact(ctx, s.db, identity.TenantID, factID); err == nil {
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
	entity, err := getEntity(ctx, s.db, identity.TenantID, userID)
	if err == sql.ErrNoRows || (err == nil && entity.Type != "user") {
		http.Error(w, "user not found", http.StatusNotFound)
		return
	} else if err != nil {
		http.Error(w, "load entity: "+err.Error(), http.StatusInternalServerError)
		return
	}
	facts := make([]*Fact, 0, len(entity.Facts))
	for _, factID := range entity.Facts {
		if fact, err := getFact(ctx, s.db, identity.TenantID, factID); err == nil {
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
		facts, err := loadFacts(r.Context(), s.db, identity.TenantID, "")
		if err != nil {
			http.Error(w, "load facts: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, facts)

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
		if fact.NamespaceID == "" {
			http.Error(w, "namespace_id is required", http.StatusBadRequest)
			return
		}
		identity, status, msg := resolveIdentity(r, fact.TenantID, fact.PrincipalID)
		if status != 0 {
			http.Error(w, msg, status)
			return
		}
		if fact.Network == "" {
			fact.Network = NetworkWorld
		}
		if fact.Scope == "" {
			fact.Scope = "global"
		}
		fact.RecordedAt = time.Now().UTC().Format(time.RFC3339)

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

		// Detect contradictions against every existing fact in this tenant
		// before inserting, exactly as the original in-memory implementation
		// did, just sourced from the durable store instead of a map.
		existing, err := loadFacts(ctx, s.db, identity.TenantID, "")
		if err != nil {
			http.Error(w, "load existing facts: "+err.Error(), http.StatusInternalServerError)
			return
		}
		var contradictions []string
		for _, other := range existing {
			if isContradictory(&fact, other) {
				contradictions = append(contradictions, other.ID)
			}
		}

		factID, err := insertFact(ctx, tx, identity, fact.NamespaceID, &fact)
		if err != nil {
			http.Error(w, "failed to persist fact: "+err.Error(), http.StatusInternalServerError)
			return
		}
		fact.ID = factID
		for _, c := range contradictions {
			if err := insertRelation(ctx, tx, factID, c, "contradicts"); err != nil {
				http.Error(w, "failed to persist contradiction: "+err.Error(), http.StatusInternalServerError)
				return
			}
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
		writeJSON(w, map[string]any{"fact": fact, "contradictions": contradictions})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *engineServer) handleFactByID(w http.ResponseWriter, r *http.Request) {
	identity, status, msg := resolveIdentityFromRequest(r)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/facts/")
	fact, err := getFact(r.Context(), s.db, identity.TenantID, id)
	if err == sql.ErrNoRows {
		http.Error(w, "not found", http.StatusNotFound)
		return
	} else if err != nil {
		http.Error(w, "load fact: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, fact)
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
		entities, err := loadEntities(r.Context(), s.db, identity.TenantID)
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
	entity, err := getEntity(r.Context(), s.db, identity.TenantID, id)
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
	entries, err := loadDiary(r.Context(), s.db, identity.TenantID)
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
		beliefs, err := loadBeliefs(r.Context(), s.db, identity.TenantID)
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
			if _, err := getFact(ctx, s.db, identity.TenantID, evidenceID); err != nil {
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
	identity, status, msg := resolveIdentityFromRequest(r)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	relations, err := loadRelations(r.Context(), s.db, identity.TenantID)
	if err != nil {
		http.Error(w, "load relations: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, relations)
}

// ── hierarchy ────────────────────────────────────────────────────────────

func (s *engineServer) handleHierarchy(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		identity, status, msg := resolveIdentityFromRequest(r)
		if status != 0 {
			http.Error(w, msg, status)
			return
		}
		nodes, err := loadHierarchy(r.Context(), s.db, identity.TenantID)
		if err != nil {
			http.Error(w, "load hierarchy: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, nodes)

	case http.MethodPost:
		var node HierarchyNode
		if err := json.NewDecoder(r.Body).Decode(&node); err != nil {
			http.Error(w, "invalid node: "+err.Error(), http.StatusBadRequest)
			return
		}
		if node.Name == "" {
			http.Error(w, "name required", http.StatusBadRequest)
			return
		}
		identity, status, msg := resolveIdentity(r, node.TenantID, node.PrincipalID)
		if status != 0 {
			http.Error(w, msg, status)
			return
		}
		if node.Level == "" {
			node.Level = LevelDrawer
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
		nodeID, err := insertHierarchyNode(ctx, tx, identity.TenantID, &node)
		if err != nil {
			http.Error(w, "failed to persist hierarchy: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if err := tx.Commit(); err != nil {
			http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
			return
		}
		node.ID = nodeID
		node.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		writeJSON(w, node)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ── reflection (§25.7) ───────────────────────────────────────────────────

func (s *engineServer) handleReflect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Evidence    []string `json:"evidence_ids"`
		Scope       string   `json:"scope,omitempty"`
		MaxBeliefs  int      `json:"max_beliefs,omitempty"`
		TenantID    string   `json:"tenant_id,omitempty"`
		PrincipalID string   `json:"principal_id,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.MaxBeliefs <= 0 {
		req.MaxBeliefs = 5
	}
	identity, status, msg := resolveIdentity(r, req.TenantID, req.PrincipalID)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}

	ctx := r.Context()
	var evidenceFacts []*Fact
	for _, id := range req.Evidence {
		if f, err := getFact(ctx, s.db, identity.TenantID, id); err == nil {
			evidenceFacts = append(evidenceFacts, f)
		}
	}

	var contradictions []map[string]any
	for i, f1 := range evidenceFacts {
		for j, f2 := range evidenceFacts {
			if i >= j {
				continue
			}
			if isContradictory(f1, f2) {
				contradictions = append(contradictions, map[string]any{
					"fact_a": f1.ID,
					"fact_b": f2.ID,
					"reason": "conflicting claims",
				})
			}
		}
	}

	var candidates []Belief
	for _, f := range evidenceFacts {
		if f.Inferred {
			continue
		}
		candidates = append(candidates, Belief{
			Conclusion: f.Content,
			Confidence: f.Confidence * 0.9,
			Evidence:   []string{f.ID},
		})
		if len(candidates) >= req.MaxBeliefs {
			break
		}
	}

	existingBeliefs, err := loadBeliefs(ctx, s.db, identity.TenantID)
	if err != nil {
		http.Error(w, "load beliefs: "+err.Error(), http.StatusInternalServerError)
		return
	}
	var staleBeliefs []string
	for _, b := range existingBeliefs {
		for _, f := range evidenceFacts {
			if strings.Contains(strings.ToLower(b.Conclusion), strings.ToLower(f.Content[:minInt(30, len(f.Content))])) {
				staleBeliefs = append(staleBeliefs, b.ID)
			}
		}
	}

	writeJSON(w, map[string]any{
		"evidence_analysed": len(evidenceFacts),
		"contradictions":    contradictions,
		"candidate_beliefs": candidates,
		"stale_beliefs":     staleBeliefs,
		"requires_revision": len(contradictions) > 0 || len(staleBeliefs) > 0,
	})
}

// ── dashboard views ──────────────────────────────────────────────────────

// versionInfo is the shape every governed service's GET /v1/version returns.
type versionInfo struct {
	Status        string `json:"status"`
	Version       string `json:"version"`
	UptimeSeconds int    `json:"uptime_seconds"`
}

// probePeer reports a real, freshly-measured status for one peer service: it
// calls /health/ready to determine liveness and /v1/version for version and
// uptime. A peer that cannot be reached is truthfully reported as
// unreachable rather than defaulting to a fabricated healthy/unknown value
// (ss42 prohibits fabricated operational state).
func (s *engineServer) probePeer(ctx context.Context, name, base string) SystemMapEntry {
	entry := SystemMapEntry{Service: name, Status: "unreachable", Version: "unknown", Uptime: "unknown"}
	client := s.httpClient
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Second}
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

	versionReq, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/version", nil)
	if err != nil {
		return entry
	}
	versionResp, err := client.Do(versionReq)
	if err != nil {
		return entry
	}
	defer versionResp.Body.Close()
	if versionResp.StatusCode == http.StatusOK {
		var info versionInfo
		if json.NewDecoder(versionResp.Body).Decode(&info) == nil {
			if info.Version != "" {
				entry.Version = info.Version
			}
			entry.Uptime = (time.Duration(info.UptimeSeconds) * time.Second).String()
		}
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
		Version: version,
		Uptime:  selfUptime.String(),
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
	facts, err := loadFacts(ctx, s.db, identity.TenantID, "")
	if err != nil {
		http.Error(w, "load facts: "+err.Error(), http.StatusInternalServerError)
		return
	}
	entities, err := loadEntities(ctx, s.db, identity.TenantID)
	if err != nil {
		http.Error(w, "load entities: "+err.Error(), http.StatusInternalServerError)
		return
	}
	relations, err := loadRelations(ctx, s.db, identity.TenantID)
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
			"type": rel.Type,
		})
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
	facts, err := loadFacts(r.Context(), s.db, identity.TenantID, network)
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

func (s *engineServer) handleDashboardLearnings(w http.ResponseWriter, r *http.Request) {
	identity, status, msg := resolveIdentityFromRequest(r)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	beliefs, err := loadBeliefs(r.Context(), s.db, identity.TenantID)
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
	relations, err := loadRelations(ctx, s.db, identity.TenantID)
	if err != nil {
		http.Error(w, "load relations: "+err.Error(), http.StatusInternalServerError)
		return
	}
	var contra []map[string]any
	for _, rel := range relations {
		if rel.Type != "contradicts" {
			continue
		}
		f1, err1 := getFact(ctx, s.db, identity.TenantID, rel.SourceID)
		f2, err2 := getFact(ctx, s.db, identity.TenantID, rel.TargetID)
		if err1 == nil && err2 == nil {
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
	facts, err := loadFacts(r.Context(), s.db, identity.TenantID, "")
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
	entities, err := loadEntities(ctx, s.db, identity.TenantID)
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
			if f, err := getFact(ctx, s.db, identity.TenantID, factID); err == nil {
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
		Scope: "tenant", ProjectionVersion: "events-v1", Data: map[string]any{"status": "live", "version": version},
	})
	flusher.Flush()

	poll := time.NewTicker(time.Second)
	defer poll.Stop()

	for {
		select {
		case <-poll.C:
			events, err := loadTransparencyEvents(r.Context(), s.db, identity.TenantID, lastSequence, 100)
			if err != nil {
				http.Error(w, "load transparency events: "+err.Error(), http.StatusServiceUnavailable)
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

func isContradictory(a, b *Fact) bool {
	if a.Network != b.Network || a.Scope != b.Scope {
		return false
	}
	aWords := strings.Fields(strings.ToLower(a.Content))
	bWords := strings.Fields(strings.ToLower(b.Content))

	negationWords := map[string]bool{"not": true, "never": true, "no": true, "isn't": true, "doesn't": true, "don't": true, "won't": true, "can't": true, "without": true}

	aHasNegation := false
	bHasNegation := false
	for _, w := range aWords {
		if negationWords[w] {
			aHasNegation = true
			break
		}
	}
	for _, w := range bWords {
		if negationWords[w] {
			bHasNegation = true
			break
		}
	}

	// One has negation, the other doesn't, and they share significant terms
	if !(aHasNegation != bHasNegation) {
		return false
	}

	sharedTerms := 0
	for _, w := range aWords {
		for _, w2 := range bWords {
			if w == w2 && len(w) > 3 && !negationWords[w] {
				sharedTerms++
			}
		}
	}

	return sharedTerms >= 1
}

// ── utility ──────────────────────────────────────────────────────────────

func truncateContent(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
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
