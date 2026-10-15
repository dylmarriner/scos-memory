package main

import "github.com/dylmarriner/scos-memory/packages/telemetry"

import "github.com/dylmarriner/scos-memory/packages/auth"
import "github.com/dylmarriner/scos-memory/packages/release"
import "github.com/dylmarriner/scos-memory/packages/tlsserve"
import capabilityregistry "github.com/dylmarriner/scos-memory/packages/capability-registry"

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dylmarriner/scos-memory/packages/database"
	executionadapters "github.com/dylmarriner/scos-memory/packages/execution-adapters"
	"github.com/dylmarriner/scos-memory/packages/httpmid"
	"github.com/dylmarriner/scos-memory/packages/llmclient"
	"github.com/dylmarriner/scos-memory/services/readiness"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var logger = httpmid.NewLogger("autonomous-control-plane")

type RouteRequest struct {
	TenantID          string `json:"tenant_id,omitempty"`
	InteractionID     string `json:"interaction_id"`
	ActorID           string `json:"actor_id,omitempty"`
	AgentID           string `json:"agent_id,omitempty"`
	SessionID         string `json:"session_id,omitempty"`
	TurnID            string `json:"turn_id,omitempty"`
	ProjectID         string `json:"project_id,omitempty"`
	Input             string `json:"input"`
	RepositoryPath    string `json:"repository_path,omitempty"`
	ProposedTool      string `json:"proposed_tool,omitempty"`
	RiskClass         string `json:"risk_class,omitempty"`
	RequiresFreshData bool   `json:"requires_fresh_data,omitempty"`

	// Structured client metadata and active task state. §21.5 requires these
	// explicit signals to override weaker phrase-level guesses.
	ArtifactID         string `json:"artifact_id,omitempty"`
	TaskID             string `json:"task_id,omitempty"`
	WorkflowState      string `json:"workflow_state,omitempty"`
	OperationType      string `json:"operation_type,omitempty"`
	DataClassification string `json:"data_classification,omitempty"`
}

type controlPlane struct {
	db         *pgxpool.Pool
	store      *localStateStore
	client     *http.Client
	dispatcher agentDispatcher
	committer  turnCommitter
}

type agentRequest struct {
	InteractionID string `json:"interaction_id"`
	TenantID      string `json:"tenant_id"`
	PrincipalID   string `json:"principal_id"`
	AgentID       string `json:"agent_id"`
	Input         string `json:"input"`
	Context       string `json:"context,omitempty"`
}
type agentReply struct {
	Content string `json:"content"`
}
type agentDispatcher interface {
	Dispatch(context.Context, agentRequest) (agentReply, error)
}
type turnCommitter interface {
	Commit(context.Context, RouteRequest, agentReply) (turnCommitReceipt, error)
}
type turnCommitReceipt struct {
	MessageID    string
	CheckpointID string
}
type httpAgentDispatcher struct {
	endpoint string
	client   *http.Client
}

type deepseekAgentDispatcher struct {
	client *llmclient.Client
}

func (d deepseekAgentDispatcher) Dispatch(ctx context.Context, input agentRequest) (agentReply, error) {
	if d.client == nil {
		return agentReply{}, llmclient.ErrDisabled
	}
	systemPrompt := "You are the SCOS agent selected for this interaction. Produce a useful, direct answer to the user. Do not claim that an action was completed unless the supplied context proves it. Agent ID: " + input.AgentID
	userPrompt := input.Input
	if strings.TrimSpace(input.Context) != "" {
		userPrompt += "\n\nAuthoritative SCOS context:\n" + input.Context
	}
	content, err := d.client.Chat(ctx, systemPrompt, userPrompt)
	if err != nil {
		return agentReply{}, err
	}
	return agentReply{Content: content}, nil
}

func (d httpAgentDispatcher) Dispatch(ctx context.Context, input agentRequest) (agentReply, error) {
	body, err := json.Marshal(input)
	if err != nil {
		return agentReply{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.endpoint, bytes.NewReader(body))
	if err != nil {
		return agentReply{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token := auth.BearerToken(ctx); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if token := strings.TrimSpace(os.Getenv("SCOS_CONTROL_PLANE_TOKEN")); token != "" {
		req.Header.Set("X-SCOS-Control-Plane-Token", token)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return agentReply{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return agentReply{}, errors.New("agent endpoint returned " + resp.Status)
	}
	var reply agentReply
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&reply); err != nil {
		return agentReply{}, err
	}
	if strings.TrimSpace(reply.Content) == "" {
		return agentReply{}, errors.New("agent endpoint returned an empty reply")
	}
	return reply, nil
}

func (c *controlPlane) dispatchAgent(ctx context.Context, input agentRequest) (agentReply, error) {
	if strings.TrimSpace(input.AgentID) == "" {
		return agentReply{}, errors.New("agent_id is required; no agent was selected")
	}
	if c.dispatcher != nil {
		return c.dispatcher.Dispatch(ctx, input)
	}
	if endpoint := strings.TrimSpace(os.Getenv("SCOS_AGENT_ENDPOINT")); endpoint != "" {
		client := c.client
		if client == nil {
			client = &http.Client{Timeout: 10 * time.Second}
		}
		return (httpAgentDispatcher{endpoint: endpoint, client: client}).Dispatch(ctx, input)
	}
	if deepseek := llmclient.FromEnvironment(c.client); deepseek != nil {
		return (deepseekAgentDispatcher{client: deepseek}).Dispatch(ctx, input)
	}
	return agentReply{}, errors.New("no agent dispatcher is configured: set SCOS_AGENT_ENDPOINT or DEEPSEEK_API_KEY")
}

type memoryGatewayCommitter struct {
	endpoint string
	client   *http.Client
}

func (c memoryGatewayCommitter) Commit(ctx context.Context, req RouteRequest, reply agentReply) (turnCommitReceipt, error) {
	if strings.TrimSpace(req.TurnID) == "" {
		return turnCommitReceipt{}, errors.New("turn_id is required for authoritative completion")
	}
	payload, err := json.Marshal(map[string]any{
		"content": reply.Content,
		"state":   map[string]any{"interaction_id": req.InteractionID, "agent_id": req.AgentID},
	})
	if err != nil {
		return turnCommitReceipt{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.endpoint, "/")+"/v1/turns/"+req.TurnID+"/complete", bytes.NewReader(payload))
	if err != nil {
		return turnCommitReceipt{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Idempotency-Key", "scos-"+req.InteractionID+"-complete")
	httpReq.Header.Set("X-SCOS-Tenant-ID", req.TenantID)
	httpReq.Header.Set("X-SCOS-Principal-ID", req.ActorID)
	if token := auth.BearerToken(ctx); token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+token)
	}
	if token := strings.TrimSpace(os.Getenv("SCOS_CONTROL_PLANE_TOKEN")); token != "" {
		httpReq.Header.Set("X-SCOS-Control-Plane-Token", token)
	}
	client := c.client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second, Transport: telemetry.ProcessTransport(nil)}
	}
	response, err := client.Do(httpReq)
	if err != nil {
		return turnCommitReceipt{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 2<<10))
		return turnCommitReceipt{}, errors.New("memory-gateway completion failed: " + strings.TrimSpace(string(body)))
	}
	var result struct {
		MessageID    string `json:"message_id"`
		CheckpointID string `json:"checkpoint_id"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 2<<10)).Decode(&result); err != nil {
		return turnCommitReceipt{}, err
	}
	if strings.TrimSpace(result.MessageID) == "" || strings.TrimSpace(result.CheckpointID) == "" {
		return turnCommitReceipt{}, errors.New("memory-gateway completion returned an incomplete receipt")
	}
	return turnCommitReceipt{MessageID: result.MessageID, CheckpointID: result.CheckpointID}, nil
}

func (c *controlPlane) commitReply(ctx context.Context, req RouteRequest, reply agentReply) (turnCommitReceipt, error) {
	if c.committer != nil {
		return c.committer.Commit(ctx, req, reply)
	}
	return turnCommitReceipt{}, errors.New("authoritative turn commit is not configured")
}

func (p *controlPlane) readinessCheck() error {
	if os.Getenv("DATABASE_URL") == "" {
		return errors.New("DATABASE_URL is required")
	}
	if p.db != nil {
		return p.db.Ping(context.Background())
	}
	if p.store == nil {
		return errors.New("control-state dependency is not configured")
	}
	return nil
}

type lifecycleResponse struct {
	Status        string          `json:"status"`
	InteractionID string          `json:"interaction_id"`
	DecisionID    string          `json:"decision_id"`
	Stages        []stageState    `json:"stages"`
	Reply         replyState      `json:"reply"`
	Checkpoint    checkpointState `json:"checkpoint"`
	Outbox        outboxState     `json:"outbox"`
	// Detail explains a status that is not completed: why the run is
	// awaiting confirmation, blocked, interrupted or needs reconciliation.
	Detail string `json:"detail,omitempty"`
	// Confirmation is the recorded confirmation state of a decision that
	// requires one.
	Confirmation *confirmationRecord `json:"confirmation,omitempty"`
}

type routeReceipt struct {
	RouteDecision
	Status    string            `json:"status"`
	Lifecycle lifecycleResponse `json:"lifecycle"`
}

type controlRequest struct {
	TenantID      string `json:"tenant_id,omitempty"`
	ActorID       string `json:"actor_id,omitempty"`
	InteractionID string `json:"interaction_id,omitempty"`
	DecisionID    string `json:"decision_id,omitempty"`
}

// confirmRequest is the requesting principal's answer to a decision awaiting
// confirmation. resolution is "confirmed" or "rejected"; decision_id binds
// the answer to the exact decision that was presented.
type confirmRequest struct {
	TenantID      string `json:"tenant_id,omitempty"`
	ActorID       string `json:"actor_id,omitempty"`
	InteractionID string `json:"interaction_id"`
	DecisionID    string `json:"decision_id"`
	Resolution    string `json:"resolution"`
}

type controlStatus struct {
	Status        string             `json:"status"`
	Performed     bool               `json:"performed"`
	InteractionID string             `json:"interaction_id,omitempty"`
	DecisionID    string             `json:"decision_id,omitempty"`
	Decision      *RouteDecision     `json:"decision,omitempty"`
	Detail        string             `json:"detail,omitempty"`
	Lifecycle     *lifecycleResponse `json:"lifecycle,omitempty"`
}

func writeControlJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeControlError(w http.ResponseWriter, status int, message string) {
	writeControlJSON(w, status, map[string]string{"error": message})
}

func decodeControlJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	defer r.Body.Close()
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("request must contain a single JSON value")
		}
		return err
	}
	return nil
}

func authenticatedIdentity(r *http.Request, reqTenant, reqActor string) (string, string, int, string) {
	if claims, ok := auth.CanonicalClaims(r.Context()); ok {
		if (reqTenant != "" && reqTenant != claims.TenantID) || (reqActor != "" && reqActor != claims.PrincipalID) {
			return "", "", http.StatusForbidden, "request identity does not match authenticated identity"
		}
		return claims.TenantID, claims.PrincipalID, 0, ""
	}
	if auth.Identity(r.Context()) != "" {
		return "", "", http.StatusUnauthorized, "authenticated token has no tenant and principal claims"
	}
	// The handlers are also used as embedded functions in tests. Network entry
	// points always pass through auth.AuthenticatedHandler, which supplies claims.
	tenantID := strings.TrimSpace(r.Header.Get("X-SCOS-Tenant-ID"))
	actorID := strings.TrimSpace(r.Header.Get("X-SCOS-Principal-ID"))
	if tenantID == "" || actorID == "" {
		return "", "", http.StatusUnauthorized, "authenticated tenant and principal headers are required"
	}
	if (reqTenant != "" && reqTenant != tenantID) || (reqActor != "" && reqActor != actorID) {
		return "", "", http.StatusForbidden, "request identity does not match authenticated identity"
	}
	return tenantID, actorID, 0, ""
}

func (c *controlPlane) persistRoute(ctx context.Context, req RouteRequest, decision RouteDecision) error {
	if c == nil || (c.db == nil && c.store == nil) {
		return errors.New("control-plane persistence is unavailable")
	}
	if strings.TrimSpace(req.TenantID) == "" || strings.TrimSpace(req.ActorID) == "" {
		return errors.New("tenant_id and actor_id are required")
	}
	if c.db != nil {
		return c.persistRouteDB(ctx, req, decision)
	}
	return c.store.saveRoute(req.TenantID, req.ActorID, req, decision)
}

func (c *controlPlane) serviceURL(system string) string {
	keys := map[string][]string{
		"memory-gateway":           {"SCOS_RETRIEVAL_ORCHESTRATOR_URL", "SCOS_MEMORY_GATEWAY_URL"},
		"context-optimisation":     {"SCOS_CONTEXT_OPTIMISATION_URL"},
		"code-intelligence":        {"SCOS_CODE_INTELLIGENCE_URL"},
		"tool-governance":          {"SCOS_TOOL_GOVERNANCE_URL"},
		"adaptive-execution-proxy": {"SCOS_EXECUTION_PROXY_URL"},
		"extension-manager":        {"SCOS_EXTENSION_MANAGER_URL"},
	}
	for _, key := range keys[system] {
		if value := strings.TrimRight(strings.TrimSpace(os.Getenv(key)), "/"); value != "" {
			return value
		}
	}
	return ""
}

func (c *controlPlane) callService(ctx context.Context, system, path string, payload any, req RouteRequest) ([]byte, error) {
	base := c.serviceURL(system)
	if base == "" {
		return nil, errors.New("service URL is not configured")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	if token := auth.BearerToken(ctx); token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if token := strings.TrimSpace(os.Getenv("SCOS_CONTROL_PLANE_TOKEN")); token != "" {
		request.Header.Set("X-SCOS-Control-Plane-Token", token)
	}
	request.Header.Set("X-SCOS-Tenant-ID", req.TenantID)
	request.Header.Set("X-SCOS-Principal-ID", req.ActorID)
	request.Header.Set("Idempotency-Key", "scos-"+req.InteractionID+"-"+strings.ReplaceAll(path, "/", "-"))
	client := c.client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second, Transport: telemetry.ProcessTransport(nil)}
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if readErr != nil {
		return nil, readErr
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, errors.New(strings.TrimSpace(string(responseBody)))
	}
	return responseBody, nil
}

func (c *controlPlane) dispatchHook(ctx context.Context, event string, payload map[string]any, req RouteRequest) error {
	if strings.TrimSpace(os.Getenv("SCOS_EXTENSION_MANAGER_URL")) == "" {
		return nil
	}
	_, err := c.callService(ctx, "extension-manager", "/v1/hooks/dispatch", map[string]any{
		"event":   event,
		"payload": payload,
	}, req)
	return err
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (c *controlPlane) lifecycle(_ context.Context, tenantID, actorID, interactionID string, decision RouteDecision) lifecycleResponse {
	result := lifecycleResponse{Status: "unknown", InteractionID: interactionID, DecisionID: decision.DecisionID}
	if c.store == nil {
		return result
	}
	c.store.mu.RLock()
	state, ok := c.store.data.Lifecycles[stateKey(tenantID, actorID, interactionID)]
	c.store.mu.RUnlock()
	if ok {
		result.Status, result.Stages, result.Reply, result.Checkpoint, result.Outbox = state.State, state.Stages, state.Reply, state.Checkpoint, state.Outbox
		result.Detail = state.Detail
		if state.Confirmation != nil {
			record := *state.Confirmation
			result.Confirmation = &record
		}
	}
	return result
}

func (c *controlPlane) lifecycleDB(ctx context.Context, tenantID, actorID, interactionID string, decision RouteDecision) (lifecycleResponse, error) {
	result := lifecycleResponse{Status: "unknown", InteractionID: interactionID, DecisionID: decision.DecisionID}
	tx, err := c.beginScopedTx(ctx, tenantID, actorID)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	var (
		runID      string
		checkpoint []byte
	)
	if err = tx.QueryRow(ctx, `SELECT id::text,status,checkpoint FROM orchestration_runs WHERE tenant_id=$1 AND interaction_id=$2`, tenantID, interactionID).Scan(&runID, &result.Status, &checkpoint); err != nil {
		return result, err
	}
	var durable struct {
		Reply      replyState      `json:"reply"`
		Checkpoint checkpointState `json:"checkpoint"`
		Outbox     outboxState     `json:"outbox"`
		Detail     string          `json:"detail"`
	}
	if len(checkpoint) > 0 {
		if err = json.Unmarshal(checkpoint, &durable); err != nil {
			return result, err
		}
	}
	result.Reply, result.Checkpoint, result.Outbox, result.Detail = durable.Reply, durable.Checkpoint, durable.Outbox, durable.Detail
	if result.Reply.Status == "" {
		result.Reply.Status = "pending"
	}
	if result.Checkpoint.Status == "" {
		result.Checkpoint.Status = "pending"
	}
	if result.Outbox.Status == "" {
		result.Outbox.Status = "pending"
	}
	if result.Stages, err = stagesDB(ctx, tx, tenantID, interactionID, decision); err != nil {
		return result, err
	}
	if result.Confirmation, err = confirmationForDecisionDB(ctx, tx, runID, decision.DecisionID); err != nil {
		return result, err
	}
	if err = tx.Commit(ctx); err != nil {
		return result, err
	}
	return result, nil
}

// currentLifecycle reads the persisted lifecycle of an interaction.
func (c *controlPlane) currentLifecycle(ctx context.Context, tenantID, actorID, interactionID string, decision RouteDecision) (lifecycleResponse, error) {
	if c.store != nil {
		return c.lifecycle(ctx, tenantID, actorID, interactionID, decision), nil
	}
	if c.db == nil {
		return lifecycleResponse{}, errors.New("control-plane persistence is unavailable")
	}
	return c.lifecycleDB(ctx, tenantID, actorID, interactionID, decision)
}

// persistRouteDB commits the ingress atomically: the run (with the frozen
// route request a confirmation or recovery executes), the decision, its
// pending stages, and, when the decision requires confirmation, the
// awaiting confirmation record. A re-routed interaction supersedes any
// confirmation still awaiting an answer for its previous decision.
func (c *controlPlane) persistRouteDB(ctx context.Context, req RouteRequest, decision RouteDecision) error {
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
	routeRequest, err := json.Marshal(req)
	if err != nil {
		return err
	}
	status, detail := runPlanned, ""
	if decision.ConfirmationRequired {
		status, detail = runAwaitingConfirmation, awaitingConfirmationDetail
	}
	checkpoint, err := json.Marshal(map[string]any{
		"reply": replyState{Status: "pending"}, "checkpoint": checkpointState{Status: "pending"},
		"outbox": outboxState{Status: "pending"}, "status": status, "detail": detail,
	})
	if err != nil {
		return err
	}
	tx, err := c.beginScopedTx(ctx, req.TenantID, req.ActorID)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var runID string
	if err = tx.QueryRow(ctx, `INSERT INTO orchestration_runs (tenant_id,interaction_id,status,current_stage,policy_version,registry_generation,checkpoint,route_request) VALUES ($1,$2,$3,'routed',$4,$5,$6,$7)
		ON CONFLICT (tenant_id,interaction_id) DO UPDATE SET status=EXCLUDED.status, current_stage='routed', policy_version=EXCLUDED.policy_version, registry_generation=EXCLUDED.registry_generation, checkpoint=EXCLUDED.checkpoint, route_request=EXCLUDED.route_request, execution_attempt=NULL, completed_at=NULL
		RETURNING id::text`, req.TenantID, req.InteractionID, status, decision.PolicyVersion, decision.RegistryGeneration, checkpoint, routeRequest).Scan(&runID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE control_confirmations SET status='superseded', resolved_at=now() WHERE orchestration_run_id=$1 AND status='awaiting'`, runID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO routing_decisions (tenant_id,actor_id,orchestration_run_id,decision_id,intent_labels,resolved_scope,risk_class,freshness_requirement,candidate_capabilities,selected_plan,reason_codes,confidence,policy_version,registry_generation,actual_outcome) VALUES ($1,$2,$3,$4,$5,'{}'::jsonb,$6,$7,'[]'::jsonb,$8,$9,$10,$11,$12,$13)`, req.TenantID, req.ActorID, runID, decision.DecisionID, intentLabels, decision.RiskClass, decision.FreshnessRequirement, plan, reasonCodes, decision.Confidence, decision.PolicyVersion, strconv.FormatInt(decision.RegistryGeneration, 10), outcome); err != nil {
		return err
	}
	if decision.ConfirmationRequired {
		if _, err = tx.Exec(ctx, `INSERT INTO control_confirmations (tenant_id,orchestration_run_id,decision_id,requested_by,status) VALUES ($1,$2,$3,$4,'awaiting')`, req.TenantID, runID, decision.DecisionID, req.ActorID); err != nil {
			return err
		}
	}
	if err = replaceStagesDB(ctx, tx, req.TenantID, req.InteractionID, decision.PolicyVersion, pendingStages(decision)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if c.store != nil {
		return c.store.saveRoute(req.TenantID, req.ActorID, req, decision)
	}
	return nil
}

// decisionSelect is the projection scanRouteDecision reads.
const decisionSelect = `
		SELECT d.decision_id, r.interaction_id, d.intent_labels, d.risk_class,
		       COALESCE(d.freshness_requirement, 'none'), d.selected_plan,
		       d.reason_codes, d.confidence, d.policy_version,
		       d.registry_generation, d.actual_outcome, d.created_at
		FROM routing_decisions d
		JOIN orchestration_runs r ON r.id = d.orchestration_run_id`

func scanRouteDecision(row pgx.Row) (RouteDecision, error) {
	var (
		decision             RouteDecision
		intentLabels, plan   []byte
		reasonCodes, outcome []byte
		registryGeneration   string
	)
	if err := row.Scan(
		&decision.DecisionID, &decision.InteractionID, &intentLabels,
		&decision.RiskClass, &decision.FreshnessRequirement, &plan,
		&reasonCodes, &decision.Confidence, &decision.PolicyVersion,
		&registryGeneration, &outcome, &decision.CreatedAt); err != nil {
		return RouteDecision{}, err
	}
	if err := json.Unmarshal(intentLabels, &decision.IntentLabels); err != nil {
		return RouteDecision{}, err
	}
	if err := json.Unmarshal(plan, &decision.SelectedPlan); err != nil {
		return RouteDecision{}, err
	}
	if err := json.Unmarshal(reasonCodes, &decision.ReasonCodes); err != nil {
		return RouteDecision{}, err
	}
	if registryGeneration != "" {
		gen, err := strconv.ParseInt(registryGeneration, 10, 64)
		if err != nil {
			return RouteDecision{}, err
		}
		decision.RegistryGeneration = gen
	}
	if len(outcome) > 0 {
		var state struct {
			ConfirmationRequired bool `json:"confirmation_required"`
		}
		if err := json.Unmarshal(outcome, &state); err != nil {
			return RouteDecision{}, err
		}
		decision.ConfirmationRequired = state.ConfirmationRequired
	}
	return decision, nil
}

// recentDecisions lists the newest route decisions, newest first. With
// tenantWide false only the actor's own decisions are returned; tenantWide
// is reserved for operator callers holding the control-plane token.
func (c *controlPlane) recentDecisions(ctx context.Context, tenantID, actorID string, tenantWide bool, limit int) ([]RouteDecision, error) {
	if c == nil || (c.db == nil && c.store == nil) {
		return nil, errors.New("control-plane persistence is unavailable")
	}
	if c.db == nil {
		return c.store.recentDecisions(tenantID, actorID, tenantWide, limit), nil
	}
	tx, err := c.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT set_config('scos.tenant_id',$1,true)`, tenantID); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `SELECT set_config('scos.principal_id',$1,true)`, actorID); err != nil {
		return nil, err
	}
	query := decisionSelect + ` WHERE d.tenant_id=$1 AND d.actor_id=$2 ORDER BY d.created_at DESC LIMIT $3`
	args := []any{tenantID, actorID, limit}
	if tenantWide {
		query = decisionSelect + ` WHERE d.tenant_id=$1 ORDER BY d.created_at DESC LIMIT $2`
		args = []any{tenantID, limit}
	}
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	out := make([]RouteDecision, 0, limit)
	for rows.Next() {
		d, err := scanRouteDecision(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, tx.Commit(ctx)
}

// hasControlPlaneToken reports whether the request carries the configured
// control-plane operator token.
func hasControlPlaneToken(r *http.Request) bool {
	expected := strings.TrimSpace(os.Getenv("SCOS_CONTROL_PLANE_TOKEN"))
	provided := strings.TrimSpace(r.Header.Get("X-SCOS-Control-Plane-Token"))
	return expected != "" && provided != "" && subtle.ConstantTimeCompare([]byte(expected), []byte(provided)) == 1
}

func (c *controlPlane) decisionForInteraction(ctx context.Context, tenantID, actorID, interactionID string) (RouteDecision, error) {
	if c == nil || (c.db == nil && c.store == nil) {
		return RouteDecision{}, errors.New("control-plane persistence is unavailable")
	}
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(actorID) == "" || strings.TrimSpace(interactionID) == "" {
		return RouteDecision{}, errors.New("tenant_id, actor_id and interaction_id are required")
	}

	if c.db == nil {
		return c.store.decision(tenantID, actorID, interactionID)
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

	row := tx.QueryRow(ctx, `
		SELECT d.decision_id, r.interaction_id, d.intent_labels, d.risk_class,
		       COALESCE(d.freshness_requirement, 'none'), d.selected_plan,
		       d.reason_codes, d.confidence, d.policy_version,
		       d.registry_generation, d.actual_outcome, d.created_at
		FROM routing_decisions d
		JOIN orchestration_runs r ON r.id = d.orchestration_run_id
		WHERE d.tenant_id=$1 AND d.actor_id=$2 AND r.interaction_id=$3
		ORDER BY d.created_at DESC
		LIMIT 1`, tenantID, actorID, interactionID)
	decision, err := scanRouteDecision(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RouteDecision{}, pgx.ErrNoRows
		}
		return RouteDecision{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return RouteDecision{}, err
	}
	return decision, nil
}

func (c *controlPlane) beginRecovery(ctx context.Context, tenantID, actorID, interactionID string) (string, error) {
	if c == nil || (c.db == nil && c.store == nil) {
		return "", errors.New("control-plane persistence is unavailable")
	}
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(actorID) == "" || strings.TrimSpace(interactionID) == "" {
		return "", errors.New("tenant_id, actor_id and interaction_id are required")
	}
	if c.db == nil {
		return c.store.beginRecovery(tenantID, actorID, interactionID)
	}
	tx, err := c.beginScopedTx(ctx, tenantID, actorID)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	// orchestration_runs is tenant scoped; only the principal whose decision
	// the run executes may recover it.
	var runID, state string
	err = tx.QueryRow(ctx, `SELECT r.id::text, r.status FROM orchestration_runs r WHERE r.tenant_id=$1 AND r.interaction_id=$2 AND EXISTS (SELECT 1 FROM routing_decisions d WHERE d.orchestration_run_id=r.id AND d.actor_id=$3) FOR UPDATE OF r`, tenantID, interactionID, actorID).Scan(&runID, &state)
	if err != nil {
		return "", err
	}
	if state != runRunning && state != runInterrupted {
		if err = tx.Commit(ctx); err != nil {
			return "", err
		}
		return state, nil
	}
	// Clearing the execution attempt fences out the executor that left the
	// run running: its next progress write no longer matches and is refused.
	if _, err = tx.Exec(ctx, `UPDATE orchestration_runs SET status='recovering', current_stage='recovery', execution_attempt=NULL, checkpoint=checkpoint || jsonb_build_object('status','recovering') WHERE id=$1`, runID); err != nil {
		return "", err
	}
	if err = tx.Commit(ctx); err != nil {
		return "", err
	}
	if c.store != nil {
		if _, err := c.store.beginRecovery(tenantID, actorID, interactionID); err != nil && !errors.Is(err, errLocalStateNotFound) {
			return "", err
		}
	}
	return runRecovering, nil
}

type RouteStep struct {
	Order    int    `json:"order"`
	System   string `json:"system"`
	Action   string `json:"action"`
	Required bool   `json:"required"`
}

type RouteDecision struct {
	DecisionID    string   `json:"decision_id"`
	InteractionID string   `json:"interaction_id"`
	IntentLabels  []string `json:"intent_labels"`
	// ClassifiedLabels carries the confidence and evidence behind each applied
	// label; AlternativeLabels carries the labels that were considered and not
	// applied, so the classifier never hides uncertainty (§21.5).
	ClassifiedLabels  []LabelScore `json:"classified_labels"`
	AlternativeLabels []LabelScore `json:"alternative_labels"`

	RiskClass            string      `json:"risk_class"`
	FreshnessRequirement string      `json:"freshness_requirement"`
	SelectedPlan         []RouteStep `json:"selected_plan"`

	// Capability selection outcome (§21.7 steps 7-8): what was chosen, what was
	// rejected and why, and which safe fallbacks are held in reserve.
	SelectedCapabilities []string                       `json:"selected_capabilities"`
	RejectedCapabilities []capabilityregistry.Exclusion `json:"rejected_capabilities"`
	ReservedFallbacks    []string                       `json:"reserved_fallbacks"`

	Confidence           float64   `json:"confidence"`
	ConfirmationRequired bool      `json:"confirmation_required"`
	PolicyVersion        string    `json:"policy_version"`
	RegistryGeneration   int64     `json:"registry_generation"`
	ReasonCodes          []string  `json:"reason_codes"`
	CreatedAt            time.Time `json:"created_at"`
}

// governanceOperationID derives the tool-governance operation ID for an
// interaction's governance stage. It is stable, so a retried ingest names
// the same operation (tool-governance refuses to start a second one for a
// non-idempotent tool), and distinct from the interaction ID itself, which
// execution-proxy prepares its own operation under.
func governanceOperationID(interactionID string) string {
	digest := sha256.Sum256([]byte(interactionID + "\x00tool-governance"))
	return hex.EncodeToString(digest[:16])
}

func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (c *controlPlane) handleIngest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeControlError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req RouteRequest
	if err := decodeControlJSON(w, r, &req); err != nil {
		writeControlError(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	tenantID, actorID, authStatus, message := authenticatedIdentity(r, req.TenantID, req.ActorID)
	if authStatus != 0 {
		writeControlError(w, authStatus, message)
		return
	}
	req.TenantID, req.ActorID = tenantID, actorID
	planningStarted := time.Now()
	decision, err := buildRoute(req)
	if err != nil {
		recordBypassAttempt("invalid_ingress", req)
		writeControlError(w, http.StatusBadRequest, err.Error())
		return
	}
	recordRouteMetrics(req, decision, time.Since(planningStarted))
	if err = c.persistRoute(r.Context(), req, decision); err != nil {
		writeControlError(w, http.StatusServiceUnavailable, "persist route decision: "+err.Error())
		return
	}
	if decision.ConfirmationRequired {
		// Nothing runs until the requesting principal answers through
		// /v1/control/confirm; the awaiting confirmation is already durable.
		lifecycle, err := c.currentLifecycle(r.Context(), tenantID, actorID, req.InteractionID, decision)
		if err != nil {
			writeControlError(w, http.StatusServiceUnavailable, "load lifecycle: "+err.Error())
			return
		}
		writeControlJSON(w, http.StatusAccepted, routeReceipt{RouteDecision: decision, Status: runAwaitingConfirmation, Lifecycle: lifecycle})
		return
	}
	status, lifecycle := c.executeRoute(r.Context(), req, decision, nil)
	writeControlJSON(w, c.executionHTTPStatus(status), routeReceipt{RouteDecision: decision, Status: status, Lifecycle: lifecycle})
}

// executionHTTPStatus maps an execution outcome onto the response code.
func (c *controlPlane) executionHTTPStatus(status string) int {
	switch status {
	case runCompleted:
		return http.StatusOK
	case runAwaitingConfirmation:
		return http.StatusAccepted
	case runBlocked:
		return http.StatusForbidden
	case runNotStarted, runSuperseded:
		return http.StatusConflict
	}
	if c.db != nil {
		return http.StatusBadGateway
	}
	return http.StatusOK
}

// handleConfirm records the requesting principal's answer to a decision
// awaiting confirmation. Only that principal, authenticated with its own
// (non-delegated) credential, may answer; the answer is bound to the exact
// decision_id and recorded server-side before anything executes. A
// confirmation executes the request frozen at ingress.
func (c *controlPlane) handleConfirm(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeControlError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req confirmRequest
	if err := decodeControlJSON(w, r, &req); err != nil {
		writeControlError(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	tenantID, actorID, authStatus, message := authenticatedIdentity(r, req.TenantID, req.ActorID)
	if authStatus != 0 {
		writeControlError(w, authStatus, message)
		return
	}
	if claims, ok := auth.CanonicalClaims(r.Context()); ok && strings.TrimSpace(claims.DelegatedAuthority) != "" {
		writeControlError(w, http.StatusForbidden, "a delegated credential cannot confirm on behalf of its principal")
		return
	}
	if strings.TrimSpace(req.InteractionID) == "" || strings.TrimSpace(req.DecisionID) == "" {
		writeControlError(w, http.StatusBadRequest, "interaction_id and decision_id are required")
		return
	}
	if req.Resolution != confirmationConfirmed && req.Resolution != confirmationRejected {
		writeControlError(w, http.StatusBadRequest, "resolution must be confirmed or rejected")
		return
	}
	routeReq, decision, record, err := c.resolveConfirmation(r.Context(), tenantID, actorID, req.InteractionID, req.DecisionID, req.Resolution)
	if err != nil {
		switch {
		case errors.Is(err, pgx.ErrNoRows) || errors.Is(err, errLocalStateNotFound):
			writeControlJSON(w, http.StatusNotFound, controlStatus{Status: "not_found", Performed: false, InteractionID: req.InteractionID})
		case errors.Is(err, errConfirmationRequester):
			writeControlError(w, http.StatusForbidden, err.Error())
		case errors.Is(err, errConfirmationDecisionMismatch), errors.Is(err, errConfirmationNotAwaiting), errors.Is(err, errRouteRequestMissing):
			writeControlJSON(w, http.StatusConflict, controlStatus{Status: "not_awaiting_confirmation", Performed: false, InteractionID: req.InteractionID, DecisionID: req.DecisionID, Detail: err.Error()})
		default:
			writeControlError(w, http.StatusServiceUnavailable, "record confirmation: "+err.Error())
		}
		return
	}
	if req.Resolution == confirmationRejected {
		lifecycle, err := c.currentLifecycle(r.Context(), tenantID, actorID, req.InteractionID, decision)
		if err != nil {
			writeControlError(w, http.StatusServiceUnavailable, "load lifecycle: "+err.Error())
			return
		}
		writeControlJSON(w, http.StatusOK, routeReceipt{RouteDecision: decision, Status: runRejected, Lifecycle: lifecycle})
		return
	}
	status, lifecycle := c.executeRoute(r.Context(), routeReq, decision, record)
	writeControlJSON(w, c.executionHTTPStatus(status), routeReceipt{RouteDecision: decision, Status: status, Lifecycle: lifecycle})
}

func (c *controlPlane) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeControlError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	tenantID, actorID, status, message := authenticatedIdentity(r, "", "")
	if status != 0 {
		writeControlError(w, status, message)
		return
	}
	interactionID := strings.TrimPrefix(r.URL.Path, "/v1/control/status/")
	if interactionID == r.URL.Path {
		interactionID = r.URL.Query().Get("interaction_id")
	}
	decision, err := c.decisionForInteraction(r.Context(), tenantID, actorID, interactionID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, errLocalStateNotFound) {
			writeControlError(w, http.StatusNotFound, "interaction not found")
			return
		}
		writeControlError(w, http.StatusServiceUnavailable, "load interaction: "+err.Error())
		return
	}
	lifecycle, err := c.currentLifecycle(r.Context(), tenantID, actorID, interactionID, decision)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeControlError(w, http.StatusNotFound, "interaction not found")
			return
		}
		writeControlError(w, http.StatusServiceUnavailable, "load lifecycle: "+err.Error())
		return
	}
	writeControlJSON(w, http.StatusOK, lifecycle)
}

func (c *controlPlane) handleReplay(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeControlError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req controlRequest
	if err := decodeControlJSON(w, r, &req); err != nil {
		writeControlError(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	tenantID, actorID, status, message := authenticatedIdentity(r, req.TenantID, req.ActorID)
	if status != 0 {
		writeControlError(w, status, message)
		return
	}
	if strings.TrimSpace(req.InteractionID) == "" {
		writeControlError(w, http.StatusBadRequest, "interaction_id is required")
		return
	}
	decision, err := c.decisionForInteraction(r.Context(), tenantID, actorID, req.InteractionID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, errLocalStateNotFound) {
			writeControlJSON(w, http.StatusNotFound, controlStatus{Status: "not_found", Performed: false, InteractionID: req.InteractionID})
			return
		}
		writeControlError(w, http.StatusServiceUnavailable, "load route decision: "+err.Error())
		return
	}
	if c.store != nil {
		if err := c.store.markReplay(tenantID, actorID, req.InteractionID); err != nil {
			if c.db == nil {
				writeControlError(w, http.StatusServiceUnavailable, "persist replay state: "+err.Error())
				return
			}
		}
	}
	// Reading and validating a persisted decision is not a replay execution.
	// Keep that distinction explicit until an executor can rerun the plan.
	writeControlJSON(w, http.StatusOK, controlStatus{Status: "not_performed", Performed: false, InteractionID: req.InteractionID, DecisionID: decision.DecisionID, Decision: &decision})
}

func (c *controlPlane) handleRecover(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeControlError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req controlRequest
	if err := decodeControlJSON(w, r, &req); err != nil {
		writeControlError(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	tenantID, actorID, status, message := authenticatedIdentity(r, req.TenantID, req.ActorID)
	if status != 0 {
		writeControlError(w, status, message)
		return
	}
	if strings.TrimSpace(req.InteractionID) == "" {
		writeControlError(w, http.StatusBadRequest, "interaction_id is required")
		return
	}
	state, err := c.beginRecovery(r.Context(), tenantID, actorID, req.InteractionID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, errLocalStateNotFound) {
			writeControlJSON(w, http.StatusNotFound, controlStatus{Status: "not_found", Performed: false, InteractionID: req.InteractionID})
			return
		}
		writeControlError(w, http.StatusServiceUnavailable, "start recovery: "+err.Error())
		return
	}
	if state != runRecovering {
		writeControlJSON(w, http.StatusConflict, controlStatus{Status: state, Performed: false, InteractionID: req.InteractionID})
		return
	}
	input, err := c.recoveryInput(r.Context(), tenantID, actorID, req.InteractionID)
	if err != nil {
		writeControlError(w, http.StatusServiceUnavailable, "load recovery state: "+err.Error())
		return
	}
	detail, unsafe := sideEffectInFlight(input.Stages)
	if !unsafe && input.Request == nil {
		detail, unsafe = "the run has no persisted route request to resume from", true
	}
	if unsafe {
		if err := c.markReconciliationRequired(r.Context(), tenantID, actorID, req.InteractionID, detail); err != nil {
			writeControlError(w, http.StatusServiceUnavailable, "record reconciliation requirement: "+err.Error())
			return
		}
		writeControlJSON(w, http.StatusConflict, controlStatus{Status: runReconciliationRequired, Performed: false, InteractionID: req.InteractionID, DecisionID: input.Decision.DecisionID, Detail: detail})
		return
	}
	// The plan is re-run from the frozen request under the confirmation that
	// was recorded for this decision, if one was required; read-only stages
	// repeat and the turn commit is idempotent on the interaction.
	outcome, lifecycle := c.executeRoute(r.Context(), *input.Request, input.Decision, input.Confirmation)
	writeControlJSON(w, c.executionHTTPStatus(outcome), controlStatus{Status: outcome, Performed: outcome != runNotStarted && outcome != runSuperseded, InteractionID: req.InteractionID, DecisionID: input.Decision.DecisionID, Detail: lifecycle.Detail, Lifecycle: &lifecycle})
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

	// Multi-label classification with confidence, evidence and alternatives
	// (§21.5) replaces the previous single-pass keyword scan.
	class := classifyIntent(req)
	reasons := []string{"mandatory_interception", "automatic_memory_retrieval"}

	codeIntent := class.has("code_lookup") || class.has("repository_analysis") || class.has("code_change")
	if codeIntent {
		reasons = append(reasons, "code_indicator_detected")
	}
	toolIntent := class.has("tool_operation") || class.has("command_execution")
	if toolIntent {
		reasons = append(reasons, "tool_indicator_detected")
	}
	if class.has("fresh_external_information") {
		reasons = append(reasons, "freshness_required")
	}

	confirmation := risk == "high" || risk == "critical" || class.has("high_impact_action")
	// The execution proxy refuses a destructive command unless the caller
	// asserts confirmation. Grading the proposed command with the proxy's own
	// classifier makes such a command require a recorded confirmation here, so
	// the control plane never has to assert one it does not hold.
	destructive := toolIntent && strings.TrimSpace(req.ProposedTool) != "" &&
		executionadapters.ClassifyRisk(req.ProposedTool) == executionadapters.RiskDestructive
	if destructive {
		confirmation = true
		reasons = append(reasons, "destructive_command_detected")
		// The caller's declared risk never understates the command: a
		// destructive command is critical whatever the request claimed, so
		// every downstream consumer of risk_class (context optimisation's
		// profile floor, the persisted decision) sees what will run.
		if risk != "critical" {
			risk = "critical"
		}
	}
	if confirmation {
		reasons = append(reasons, "confirmation_policy_applies")
	}

	// Capability selection runs against the registry rather than assuming a
	// fixed system per intent (§21.6, §21.7). No confirmation exists while a
	// route is being decided, so none is granted to selection: a requirement
	// for confirmation is not a confirmation.
	selectedCaps, rejectedCaps, reservedFallbacks := selectCapabilities(routingRegistry, req, class, false)
	if len(rejectedCaps) > 0 {
		reasons = append(reasons, "capabilities_excluded_by_policy")
	}

	steps := []RouteStep{
		{System: "journal", Action: "commit_ingress", Required: true},
		{System: "autonomous-control-plane", Action: "resolve_identity_scope_and_policy", Required: true},
		{System: "memory-gateway", Action: "retrieve_scoped_context", Required: true},
	}
	if codeIntent {
		steps = append(steps, RouteStep{System: "code-intelligence", Action: "retrieve_minimal_sufficient_code_context", Required: true})
	}
	if class.has("fresh_external_information") {
		steps = append(steps, RouteStep{System: "capability-router", Action: "select_current_authoritative_source", Required: true})
	}
	if class.has("recovery") {
		steps = append(steps, RouteStep{System: "recovery-orchestrator", Action: "rehydrate_checkpoint_and_reconcile", Required: true})
	}
	if class.has("monitoring") {
		steps = append(steps, RouteStep{System: "observability", Action: "collect_operational_status", Required: false})
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

	confidence := class.Confidence
	if req.ProjectID == "" && codeIntent && req.RepositoryPath == "" {
		// The repository was inferred from phrasing rather than declared, so
		// the decision is materially less certain than its labels suggest.
		if confidence > 0.78 {
			confidence = 0.78
		}
		reasons = append(reasons, "repository_scope_inferred")
	}

	freshness := "none"
	if class.has("fresh_external_information") {
		freshness = "current"
	}

	decisionID, err := newID()
	if err != nil {
		return RouteDecision{}, err
	}
	return RouteDecision{
		DecisionID:           decisionID,
		InteractionID:        req.InteractionID,
		IntentLabels:         class.labelNames(),
		ClassifiedLabels:     class.Labels,
		AlternativeLabels:    class.Alternatives,
		RiskClass:            risk,
		FreshnessRequirement: freshness,
		SelectedPlan:         steps,
		SelectedCapabilities: selectedCaps,
		RejectedCapabilities: rejectedCaps,
		ReservedFallbacks:    reservedFallbacks,
		Confidence:           confidence,
		ConfirmationRequired: confirmation,
		PolicyVersion:        "1.1.0",
		RegistryGeneration:   routingRegistry.Generation(),
		ReasonCodes:          reasons,
		CreatedAt:            time.Now().UTC(),
	}, nil
}

func main() {
	// `autonomous-control-plane classify` runs the production route builder
	// over JSON-lines RouteRequests on stdin and writes one RouteDecision per
	// line, with no database or network. The acceptance benchmark measures
	// routing through it so the evaluated router is the deployed one.
	if len(os.Args) > 1 && os.Args[1] == "classify" {
		if err := classifyStream(os.Stdin, os.Stdout); err != nil {
			_, _ = os.Stderr.WriteString(err.Error() + "\n")
			os.Exit(1)
		}
		return
	}
	databaseURL := os.Getenv("DATABASE_URL")
	statePath := os.Getenv("SCOS_CONTROL_PLANE_STATE_PATH")
	if statePath == "" {
		statePath = os.Getenv("SCOS_CONTROL_STATE_PATH")
	}
	var store *localStateStore
	var err error
	if databaseURL == "" && statePath != "" {
		store, err = newLocalStateStore(statePath)
		if err != nil {
			logger.Error("startup failed", "error", err)
			os.Exit(1)
		}
	}
	var db *pgxpool.Pool
	if databaseURL != "" {
		if err := database.RequireTLS(databaseURL); err != nil {
			logger.Error("startup failed", "error", err)
			os.Exit(1)
		}
		db, err = database.NewPool(context.Background(), databaseURL)
		if err != nil {
			logger.Error("startup failed", "error", err)
			os.Exit(1)
		}
		defer db.Close()
		if err := db.Ping(context.Background()); err != nil {
			logger.Error("startup failed", "error", err)
			os.Exit(1)
		}
	}
	if db == nil {
		logger.Error("startup failed", "error", "DATABASE_URL is required; local control-plane state is test-only fallback")
		os.Exit(1)
	}
	commitEndpoint := strings.TrimSpace(os.Getenv("SCOS_MEMORY_GATEWAY_URL"))
	if commitEndpoint == "" {
		logger.Error("startup failed", "error", "SCOS_MEMORY_GATEWAY_URL is required; authoritative turn commit is unavailable")
		os.Exit(1)
	}
	// memory-gateway is an internal service, so its committer uses the shared
	// internal TLS trust anchor rather than the system pool used for the
	// (potentially external) agent endpoint dispatcher below.
	commitClient, err := tlsserve.TrustedClient(10 * time.Second)
	if err != nil {
		logger.Error("startup failed", "error", err)
		os.Exit(1)
	}
	plane := &controlPlane{db: db, store: store, client: commitClient, committer: memoryGatewayCommitter{endpoint: commitEndpoint, client: commitClient}}
	restoreCtx, cancelRestore := context.WithTimeout(context.Background(), 15*time.Second)
	if err := plane.restoreCapabilityObservations(restoreCtx, "autonomous-control-plane"); err != nil {
		cancelRestore()
		logger.Error("startup failed", "error", "restore capability observations: "+err.Error())
		os.Exit(1)
	}
	cancelRestore()
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"live"}`))
	})
	mux.HandleFunc("/health/ready", readiness.Handler(func() error {
		return plane.readinessCheck()
	}))
	mux.HandleFunc("/health/version", release.Handler("autonomous-control-plane"))
	decisionHandler := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeControlError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		tenantID, actorID, authStatus, authMessage := authenticatedIdentity(r, "", "")
		if authStatus != 0 {
			writeControlError(w, authStatus, authMessage)
			return
		}
		interactionID := strings.TrimPrefix(r.URL.Path, "/v1/control/decisions/")
		if interactionID == r.URL.Path {
			interactionID = r.URL.Query().Get("interaction_id")
		}
		if strings.TrimSpace(interactionID) == "" {
			// No interaction selected: list recent decisions. Tenant-wide
			// visibility requires the control-plane operator token; any
			// other caller sees only its own decisions.
			limit := 50
			if raw := r.URL.Query().Get("limit"); raw != "" {
				n, err := strconv.Atoi(raw)
				if err != nil || n < 1 || n > 500 {
					writeControlError(w, http.StatusBadRequest, "limit must be between 1 and 500")
					return
				}
				limit = n
			}
			tenantWide := hasControlPlaneToken(r)
			decisions, err := plane.recentDecisions(r.Context(), tenantID, actorID, tenantWide, limit)
			if err != nil {
				writeControlError(w, http.StatusServiceUnavailable, "list route decisions: "+err.Error())
				return
			}
			writeControlJSON(w, http.StatusOK, decisions)
			return
		}
		decision, err := plane.decisionForInteraction(r.Context(), tenantID, actorID, interactionID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, errLocalStateNotFound) {
				writeControlError(w, http.StatusNotFound, "decision not found")
				return
			}
			if strings.Contains(err.Error(), "required") {
				writeControlError(w, http.StatusUnauthorized, err.Error())
				return
			}
			writeControlError(w, http.StatusServiceUnavailable, "load route decision: "+err.Error())
			return
		}
		writeControlJSON(w, http.StatusOK, decision)
	}
	mux.HandleFunc("/v1/control/decisions", decisionHandler)
	mux.HandleFunc("/v1/control/decisions/", decisionHandler)
	mux.HandleFunc("/v1/control/route", plane.handleIngest)
	mux.HandleFunc("/v1/control/ingest", plane.handleIngest)
	mux.HandleFunc("/v1/control/replay", plane.handleReplay)
	mux.HandleFunc("/v1/control/recover", plane.handleRecover)
	mux.HandleFunc("/v1/control/confirm", plane.handleConfirm)
	mux.HandleFunc("/v1/control/status", plane.handleStatus)
	mux.HandleFunc("/v1/control/status/", plane.handleStatus)
	mux.HandleFunc("/v1/triggers", plane.handleIngest)

	addr := os.Getenv("SCOS_CONTROL_HTTP_ADDR")
	if addr == "" {
		addr = ":8081"
	}

	handler := auth.AuthenticatedHandler(mux, auth.MustFromEnvironment())
	handler = httpmid.Recover(logger, "autonomous-control-plane")(httpmid.RequestLogging(logger)(handler))

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           telemetry.Instrument("autonomous-control-plane", handler),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	logger.Info("autonomous control plane listening", "addr", addr)
	if err := tlsserve.ServeGracefully(httpServer, 15*time.Second, logger); err != nil {
		logger.Error("server exited with error", "error", err)
		os.Exit(1)
	}
}
