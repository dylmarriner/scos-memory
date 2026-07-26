package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type RouteRequest struct {
	TenantID          string `json:"tenant_id,omitempty"`
	InteractionID     string `json:"interaction_id"`
	ActorID           string `json:"actor_id,omitempty"`
	AgentID           string `json:"agent_id,omitempty"`
	SessionID         string `json:"session_id,omitempty"`
	ProjectID         string `json:"project_id,omitempty"`
	Input             string `json:"input"`
	RepositoryPath    string `json:"repository_path,omitempty"`
	ProposedTool      string `json:"proposed_tool,omitempty"`
	RiskClass         string `json:"risk_class,omitempty"`
	RequiresFreshData bool   `json:"requires_fresh_data,omitempty"`
}

type controlPlane struct {
	db *pgxpool.Pool
}

func (c *controlPlane) persistRoute(ctx context.Context, req RouteRequest, decision RouteDecision) error {
	if c == nil || c.db == nil {
		return errors.New("control-plane database is unavailable")
	}
	if strings.TrimSpace(req.TenantID) == "" || strings.TrimSpace(req.ActorID) == "" {
		return errors.New("tenant_id and actor_id are required")
	}
	intentLabels, err := json.Marshal(decision.IntentLabels)
	if err != nil {
		return err
	}
	reasonCodes, err := json.Marshal(decision.ReasonCodes)
	if err != nil {
		return err
	}
	plan, err := json.Marshal(decision.SelectedPlan)
	if err != nil {
		return err
	}
	outcome, err := json.Marshal(map[string]bool{"confirmation_required": decision.ConfirmationRequired})
	if err != nil {
		return err
	}
	tx, err := c.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT set_config('scos.tenant_id',$1,true)`, req.TenantID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `SELECT set_config('scos.principal_id',$1,true)`, req.ActorID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO orchestration_runs (tenant_id,interaction_id,status,current_stage,policy_version,registry_generation,checkpoint) VALUES ($1,$2,'planned','routed',$3,$4,'{}'::jsonb) ON CONFLICT (tenant_id,interaction_id) DO UPDATE SET status='planned', current_stage='routed'`, req.TenantID, req.InteractionID, decision.PolicyVersion, decision.RegistryGeneration); err != nil {
		return err
	}
	var runID string
	if err = tx.QueryRow(ctx, `SELECT id FROM orchestration_runs WHERE tenant_id=$1 AND interaction_id=$2`, req.TenantID, req.InteractionID).Scan(&runID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO routing_decisions (tenant_id,actor_id,orchestration_run_id,decision_id,intent_labels,resolved_scope,risk_class,freshness_requirement,candidate_capabilities,selected_plan,reason_codes,confidence,policy_version,registry_generation,actual_outcome) VALUES ($1,$2,$3,$4,$5,'{}'::jsonb,$6,$7,'[]'::jsonb,$8,$9,$10,$11,$12,$13) ON CONFLICT DO NOTHING`, req.TenantID, req.ActorID, runID, decision.DecisionID, intentLabels, decision.RiskClass, decision.FreshnessRequirement, plan, reasonCodes, decision.Confidence, decision.PolicyVersion, decision.RegistryGeneration, outcome); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (c *controlPlane) decisionForInteraction(ctx context.Context, tenantID, actorID, interactionID string) (RouteDecision, error) {
	if c == nil || c.db == nil {
		return RouteDecision{}, errors.New("control-plane database is unavailable")
	}
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(actorID) == "" || strings.TrimSpace(interactionID) == "" {
		return RouteDecision{}, errors.New("tenant_id, actor_id and interaction_id are required")
	}

	tx, err := c.db.Begin(ctx)
	if err != nil {
		return RouteDecision{}, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT set_config('scos.tenant_id',$1,true)`, tenantID); err != nil {
		return RouteDecision{}, err
	}
	if _, err = tx.Exec(ctx, `SELECT set_config('scos.principal_id',$1,true)`, actorID); err != nil {
		return RouteDecision{}, err
	}

	var (
		decision             RouteDecision
		intentLabels, plan   []byte
		reasonCodes, outcome []byte
		registryGeneration   string
	)
	err = tx.QueryRow(ctx, `
		SELECT d.decision_id, r.interaction_id, d.intent_labels, d.risk_class,
		       COALESCE(d.freshness_requirement, 'none'), d.selected_plan,
		       d.reason_codes, d.confidence, d.policy_version,
		       d.registry_generation, d.actual_outcome, d.created_at
		FROM routing_decisions d
		JOIN orchestration_runs r ON r.id = d.orchestration_run_id
		WHERE d.tenant_id=$1 AND d.actor_id=$2 AND r.interaction_id=$3
		ORDER BY d.created_at DESC
		LIMIT 1`, tenantID, actorID, interactionID).Scan(
		&decision.DecisionID, &decision.InteractionID, &intentLabels,
		&decision.RiskClass, &decision.FreshnessRequirement, &plan,
		&reasonCodes, &decision.Confidence, &decision.PolicyVersion,
		&registryGeneration, &outcome, &decision.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RouteDecision{}, pgx.ErrNoRows
		}
		return RouteDecision{}, err
	}
	if err = json.Unmarshal(intentLabels, &decision.IntentLabels); err != nil {
		return RouteDecision{}, err
	}
	if err = json.Unmarshal(plan, &decision.SelectedPlan); err != nil {
		return RouteDecision{}, err
	}
	if err = json.Unmarshal(reasonCodes, &decision.ReasonCodes); err != nil {
		return RouteDecision{}, err
	}
	if registryGeneration != "" {
		decision.RegistryGeneration, err = strconv.ParseInt(registryGeneration, 10, 64)
		if err != nil {
			return RouteDecision{}, err
		}
	}
	if len(outcome) > 0 {
		var state struct {
			ConfirmationRequired bool `json:"confirmation_required"`
		}
		if err = json.Unmarshal(outcome, &state); err != nil {
			return RouteDecision{}, err
		}
		decision.ConfirmationRequired = state.ConfirmationRequired
	}
	if err = tx.Commit(ctx); err != nil {
		return RouteDecision{}, err
	}
	return decision, nil
}

type RouteStep struct {
	Order    int    `json:"order"`
	System   string `json:"system"`
	Action   string `json:"action"`
	Required bool   `json:"required"`
}

type RouteDecision struct {
	DecisionID           string      `json:"decision_id"`
	InteractionID        string      `json:"interaction_id"`
	IntentLabels         []string    `json:"intent_labels"`
	RiskClass            string      `json:"risk_class"`
	FreshnessRequirement string      `json:"freshness_requirement"`
	SelectedPlan         []RouteStep `json:"selected_plan"`
	Confidence           float64     `json:"confidence"`
	ConfirmationRequired bool        `json:"confirmation_required"`
	PolicyVersion        string      `json:"policy_version"`
	RegistryGeneration   int64       `json:"registry_generation"`
	ReasonCodes          []string    `json:"reason_codes"`
	CreatedAt            time.Time   `json:"created_at"`
}

func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func containsAny(value string, terms ...string) bool {
	value = strings.ToLower(value)
	for _, term := range terms {
		if strings.Contains(value, term) {
			return true
		}
	}
	return false
}

func buildRoute(req RouteRequest) (RouteDecision, error) {
	if strings.TrimSpace(req.InteractionID) == "" {
		return RouteDecision{}, errors.New("interaction_id is required")
	}
	if strings.TrimSpace(req.Input) == "" {
		return RouteDecision{}, errors.New("input is required")
	}

	risk := strings.ToLower(strings.TrimSpace(req.RiskClass))
	if risk == "" {
		risk = "read_only"
	}

	labels := map[string]bool{"conversation": true, "memory_recall": true}
	reasons := []string{"mandatory_interception", "automatic_memory_retrieval"}

	codeIntent := req.RepositoryPath != "" || containsAny(req.Input,
		"repository", "repo", "source code", "function", "class", "stack trace",
		"compile", "build", "test failure", ".go", ".py", ".ts", ".tsx", ".js")
	if codeIntent {
		labels["repository_analysis"] = true
		labels["code_lookup"] = true
		reasons = append(reasons, "code_indicator_detected")
	}

	toolIntent := req.ProposedTool != "" || containsAny(req.Input,
		"run command", "execute", "terminal", "shell", "deploy", "install", "restart service")
	if toolIntent {
		labels["tool_operation"] = true
		reasons = append(reasons, "tool_indicator_detected")
	}

	if req.RequiresFreshData || containsAny(req.Input, "current", "latest", "today", "right now", "real-time", "real time") {
		labels["fresh_external_information"] = true
		reasons = append(reasons, "freshness_required")
	}

	confirmation := risk == "high" || risk == "critical" || containsAny(req.Input,
		"delete", "remove access", "send email", "make payment", "production deploy", "permission")
	if confirmation {
		labels["high_impact_action"] = true
		reasons = append(reasons, "confirmation_policy_applies")
	}

	steps := []RouteStep{
		{System: "journal", Action: "commit_ingress", Required: true},
		{System: "autonomous-control-plane", Action: "resolve_identity_scope_and_policy", Required: true},
		{System: "memory-gateway", Action: "retrieve_scoped_context", Required: true},
	}
	if codeIntent {
		steps = append(steps, RouteStep{System: "code-intelligence", Action: "retrieve_minimal_sufficient_code_context", Required: true})
	}
	if labels["fresh_external_information"] {
		steps = append(steps, RouteStep{System: "capability-router", Action: "select_current_authoritative_source", Required: true})
	}
	steps = append(steps, RouteStep{System: "context-optimisation", Action: "compile_and_validate_context", Required: true})
	steps = append(steps, RouteStep{System: "agent-router", Action: "select_and_dispatch_agent", Required: true})
	if toolIntent {
		steps = append(steps,
			RouteStep{System: "tool-governance", Action: "authorise_and_plan_operation", Required: true},
			RouteStep{System: "adaptive-execution-proxy", Action: "execute_and_preserve_result", Required: true},
		)
	}
	steps = append(steps,
		RouteStep{System: "turn-coordinator", Action: "commit_reply_and_checkpoint", Required: true},
		RouteStep{System: "consolidation", Action: "enqueue_post_turn_analysis", Required: true},
	)
	for i := range steps {
		steps[i].Order = i + 1
	}

	intentLabels := make([]string, 0, len(labels))
	for label := range labels {
		intentLabels = append(intentLabels, label)
	}
	sort.Strings(intentLabels)

	confidence := 0.94
	if req.ProjectID == "" && codeIntent && req.RepositoryPath == "" {
		confidence = 0.78
		reasons = append(reasons, "repository_scope_inferred")
	}

	freshness := "none"
	if labels["fresh_external_information"] {
		freshness = "current"
	}

	decisionID, err := newID()
	if err != nil {
		return RouteDecision{}, err
	}
	return RouteDecision{
		DecisionID:           decisionID,
		InteractionID:        req.InteractionID,
		IntentLabels:         intentLabels,
		RiskClass:            risk,
		FreshnessRequirement: freshness,
		SelectedPlan:         steps,
		Confidence:           confidence,
		ConfirmationRequired: confirmation,
		PolicyVersion:        "1.0.0",
		RegistryGeneration:   1,
		ReasonCodes:          reasons,
		CreatedAt:            time.Now().UTC(),
	}, nil
}

func main() {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		log.Fatal("DATABASE_URL is required")
	}
	db, err := pgxpool.New(context.Background(), databaseURL)
	if err != nil {
		log.Fatalf("connect to database: %v", err)
	}
	defer db.Close()
	plane := &controlPlane{db: db}
	if err := db.Ping(context.Background()); err != nil {
		log.Fatalf("database is not ready: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"live"}`))
	})
	mux.HandleFunc("/health/ready", func(w http.ResponseWriter, r *http.Request) {
		if err := plane.db.Ping(r.Context()); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"unready","dependency":"postgres"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	})
	mux.HandleFunc("/v1/control/decisions/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		tenantID := r.Header.Get("X-SCOS-Tenant-ID")
		actorID := r.Header.Get("X-SCOS-Principal-ID")
		interactionID := strings.TrimPrefix(r.URL.Path, "/v1/control/decisions/")
		decision, err := plane.decisionForInteraction(r.Context(), tenantID, actorID, interactionID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				http.Error(w, "decision not found", http.StatusNotFound)
				return
			}
			if strings.Contains(err.Error(), "required") {
				http.Error(w, err.Error(), http.StatusUnauthorized)
				return
			}
			http.Error(w, "load route decision: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(decision)
	})
	mux.HandleFunc("/v1/control/route", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		defer r.Body.Close()
		var req RouteRequest
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
			return
		}
		// Identity is established by the authenticated gateway boundary, never by
		// caller-controlled JSON. Reject mismatches rather than allowing tenant or
		// principal spoofing through this internal service.
		headerTenant := strings.TrimSpace(r.Header.Get("X-SCOS-Tenant-ID"))
		headerActor := strings.TrimSpace(r.Header.Get("X-SCOS-Principal-ID"))
		if headerTenant == "" || headerActor == "" {
			http.Error(w, "authenticated tenant and principal headers are required", http.StatusUnauthorized)
			return
		}
		if (req.TenantID != "" && req.TenantID != headerTenant) || (req.ActorID != "" && req.ActorID != headerActor) {
			http.Error(w, "request identity does not match authenticated identity", http.StatusForbidden)
			return
		}
		req.TenantID = headerTenant
		req.ActorID = headerActor
		decision, err := buildRoute(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := plane.persistRoute(r.Context(), req, decision); err != nil {
			http.Error(w, "persist route decision: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(decision); err != nil {
			log.Printf("encode route decision: %v", err)
		}
	})

	addr := os.Getenv("SCOS_CONTROL_HTTP_ADDR")
	if addr == "" {
		addr = ":8081"
	}
	log.Printf("autonomous control plane listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}
