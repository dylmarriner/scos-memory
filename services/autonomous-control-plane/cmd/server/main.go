package main

import "github.com/dylmarriner/scos-memory/packages/auth"
import "github.com/dylmarriner/scos-memory/packages/tlsserve"
import capabilityregistry "github.com/dylmarriner/scos-memory/packages/capability-registry"

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dylmarriner/scos-memory/packages/database"
	"github.com/dylmarriner/scos-memory/services/readiness"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

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
	return agentReply{}, errors.New("SCOS_AGENT_ENDPOINT is required; no agent dispatcher is configured")
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
		client = &http.Client{Timeout: 10 * time.Second}
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
	// A local store plus an injected dispatcher is intentionally a test seam,
	// not a production persistence fallback.
	if c.db == nil && c.store != nil && c.dispatcher != nil {
		return turnCommitReceipt{MessageID: "test-message-" + req.InteractionID, CheckpointID: "test-checkpoint-" + req.InteractionID}, nil
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

type controlStatus struct {
	Status        string         `json:"status"`
	Performed     bool           `json:"performed"`
	InteractionID string         `json:"interaction_id,omitempty"`
	DecisionID    string         `json:"decision_id,omitempty"`
	Decision      *RouteDecision `json:"decision,omitempty"`
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
	return c.store.saveRoute(req.TenantID, req.ActorID, decision)
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
		client = &http.Client{Timeout: 5 * time.Second}
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

func (c *controlPlane) executeRoute(ctx context.Context, req RouteRequest, decision RouteDecision) (string, lifecycleResponse) {
	status := "completed"
	lifecycle := lifecycleResponse{
		Status: "completed", InteractionID: req.InteractionID, DecisionID: decision.DecisionID,
		Reply: replyState{Status: "pending"}, Checkpoint: checkpointState{Status: "pending"},
		Outbox: outboxState{Status: "pending"}, Stages: make([]stageState, len(decision.SelectedPlan)),
	}
	for i, step := range decision.SelectedPlan {
		lifecycle.Stages[i] = stageState{Order: step.Order, System: step.System, Action: step.Action, Required: step.Required, Status: "pending"}
	}
	var memory, code []byte
	var reply agentReply
	if strings.TrimSpace(os.Getenv("SCOS_EXTENSION_MANAGER_URL")) != "" {
		if _, err := c.callService(ctx, "extension-manager", "/v1/hooks/dispatch", map[string]any{
			"event": "pre_turn", "payload": map[string]any{"interaction_id": req.InteractionID, "input": req.Input},
		}, req); err != nil {
			return "degraded", lifecycle
		}
	}
	for _, step := range decision.SelectedPlan {
		stage := stageState{Order: step.Order, System: step.System, Action: step.Action, Required: step.Required, Status: "running", StartedAt: time.Now().UTC()}
		lifecycle.Stages[step.Order-1] = stage
		if c.store != nil {
			_ = c.store.updateLifecycle(req.TenantID, req.ActorID, req.InteractionID, func(state *lifecycleState) {
				if step.Order <= len(state.Stages) {
					state.Stages[step.Order-1] = stage
				}
				state.State = "running"
			})
		}
		var err error
		switch step.System {
		case "journal", "autonomous-control-plane", "capability-router", "consolidation":
			// These stages are owned by this transaction; their completion is
			// recorded with the lifecycle rather than delegated over HTTP.
		case "memory-gateway":
			memory, err = c.callService(ctx, step.System, "/v1/retrieve", map[string]any{
				"tenant_id": req.TenantID, "principal_id": req.ActorID, "query": req.Input,
				"agent_id": req.AgentID, "session_id": req.SessionID, "include_recent": true, "include_session": true,
			}, req)
		case "code-intelligence":
			code, err = c.callService(ctx, step.System, "/v1/code/query", map[string]any{
				"intent": "implementation_search", "query": req.Input, "path": req.RepositoryPath, "max_results": 10,
			}, req)
		case "context-optimisation":
			parts := []string{req.Input, string(memory), string(code)}
			_, err = c.callService(ctx, step.System, "/v1/context/compile", map[string]any{
				"input": strings.Join(parts, "\n\n"), "task_type": "conversation", "profile": "balanced", "token_budget": 8000,
			}, req)
		case "tool-governance":
			if strings.TrimSpace(req.ProposedTool) == "" {
				err = errors.New("proposed_tool is required for governance")
			} else {
				_, err = c.callService(ctx, step.System, "/v1/operations/start", map[string]any{
					"tool_id": req.ProposedTool, "input": map[string]any{"interaction_id": req.InteractionID, "input": req.Input},
				}, req)
			}
		case "adaptive-execution-proxy":
			_, err = c.callService(ctx, step.System, "/v1/proxy/execute", map[string]any{
				"tenant_id": req.TenantID, "principal_id": req.ActorID, "agent_id": req.AgentID,
				"operation_id": req.InteractionID, "command": req.ProposedTool, "working_directory": req.RepositoryPath,
				"confirmed": !decision.ConfirmationRequired,
			}, req)
		case "agent-router":
			reply, err = c.dispatchAgent(ctx, agentRequest{InteractionID: req.InteractionID, TenantID: req.TenantID, PrincipalID: req.ActorID, AgentID: req.AgentID, Input: req.Input, Context: strings.Join([]string{string(memory), string(code)}, "\n\n")})
		case "turn-coordinator":
			if strings.TrimSpace(reply.Content) == "" {
				err = errors.New("agent reply was not captured")
			}
			if err == nil {
				var receipt turnCommitReceipt
				receipt, err = c.commitReply(ctx, req, reply)
				if err == nil {
					lifecycle.Checkpoint = checkpointState{Status: "completed", ID: receipt.CheckpointID}
					lifecycle.Outbox = outboxState{Status: "completed"}
				}
			}
			if err == nil {
				lifecycle.Reply = replyState{Status: "completed", Content: reply.Content}
			}
		default:
			err = errors.New("service URL is not configured")
		}
		stage.Status, stage.EndedAt = "completed", time.Now().UTC()
		if err != nil {
			stage.Status, stage.Detail = "degraded", err.Error()
			if status == "completed" {
				status = "degraded"
			}
		}
		lifecycle.Stages[step.Order-1] = stage
		lifecycle.Status = status
		if c.store != nil {
			if updateErr := c.store.updateLifecycle(req.TenantID, req.ActorID, req.InteractionID, func(state *lifecycleState) {
				if step.Order <= len(state.Stages) {
					state.Stages[step.Order-1] = stage
				}
				state.State = status
				if err == nil && step.System == "turn-coordinator" {
					state.Reply, state.Checkpoint, state.Outbox = lifecycle.Reply, lifecycle.Checkpoint, lifecycle.Outbox
				}
			}); updateErr != nil {
				status = "degraded"
				lifecycle.Status = status
				lifecycle.Reply, lifecycle.Checkpoint, lifecycle.Outbox = replyState{Status: "pending"}, checkpointState{Status: "pending"}, outboxState{Status: "pending"}
			}
		}
	}
	if strings.TrimSpace(os.Getenv("SCOS_EXTENSION_MANAGER_URL")) != "" {
		if _, err := c.callService(ctx, "extension-manager", "/v1/hooks/dispatch", map[string]any{
			"event": "post_turn", "payload": map[string]any{"interaction_id": req.InteractionID, "reply": reply.Content},
		}, req); err != nil && status == "completed" {
			status = "degraded"
			lifecycle.Status = status
		}
	}
	if c.store != nil {
		return status, c.lifecycle(ctx, req.TenantID, req.ActorID, req.InteractionID, decision)
	}
	return status, lifecycle
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
	}
	return result
}

func (c *controlPlane) lifecycleDB(ctx context.Context, tenantID, actorID, interactionID string, decision RouteDecision) (lifecycleResponse, error) {
	result := lifecycleResponse{Status: "unknown", InteractionID: interactionID, DecisionID: decision.DecisionID}
	tx, err := c.db.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT set_config('scos.tenant_id',$1,true)`, tenantID); err != nil {
		return result, err
	}
	if _, err = tx.Exec(ctx, `SELECT set_config('scos.principal_id',$1,true)`, actorID); err != nil {
		return result, err
	}
	var checkpoint []byte
	if err = tx.QueryRow(ctx, `SELECT status,checkpoint FROM orchestration_runs WHERE tenant_id=$1 AND interaction_id=$2`, tenantID, interactionID).Scan(&result.Status, &checkpoint); err != nil {
		return result, err
	}
	var durable struct {
		Reply      replyState      `json:"reply"`
		Checkpoint checkpointState `json:"checkpoint"`
		Outbox     outboxState     `json:"outbox"`
	}
	if len(checkpoint) > 0 {
		_ = json.Unmarshal(checkpoint, &durable)
	}
	result.Reply, result.Checkpoint, result.Outbox = durable.Reply, durable.Checkpoint, durable.Outbox
	rows, err := tx.Query(ctx, `SELECT stage_name,status,COALESCE(error_detail,''),COALESCE(started_at,now()),COALESCE(completed_at,now()) FROM lifecycle_stages WHERE tenant_id=$1 AND input_reference=$2 ORDER BY created_at`, tenantID, interactionID)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var name, stageStatus, detail string
		var started, ended time.Time
		if err = rows.Scan(&name, &stageStatus, &detail, &started, &ended); err != nil {
			return result, err
		}
		parts := strings.SplitN(name, ":", 2)
		stage := stageState{System: parts[0], Status: stageStatus, Detail: detail, StartedAt: started, EndedAt: ended}
		if len(parts) == 2 {
			stage.Action = parts[1]
		}
		stage.Order = len(result.Stages) + 1
		result.Stages = append(result.Stages, stage)
	}
	if err = rows.Err(); err != nil {
		return result, err
	}
	if err = tx.Commit(ctx); err != nil {
		return result, err
	}
	return result, nil
}

func (c *controlPlane) persistLifecycleDB(ctx context.Context, req RouteRequest, decision RouteDecision, lifecycle lifecycleResponse) error {
	if c.db == nil {
		return nil
	}
	checkpoint, err := json.Marshal(map[string]any{
		"reply": lifecycle.Reply, "checkpoint": lifecycle.Checkpoint, "outbox": lifecycle.Outbox,
		"status": lifecycle.Status,
	})
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
	if _, err = tx.Exec(ctx, `UPDATE orchestration_runs SET status=$1,current_stage=$2,checkpoint=$3,completed_at=CASE WHEN $1 IN ('completed','degraded') THEN now() ELSE completed_at END WHERE tenant_id=$4 AND interaction_id=$5`, lifecycle.Status, lastStage(decision), checkpoint, req.TenantID, req.InteractionID); err != nil {
		return err
	}
	for _, stage := range lifecycle.Stages {
		dbStatus := stage.Status
		if dbStatus == "degraded" {
			dbStatus = "failed"
		}
		if _, err = tx.Exec(ctx, `INSERT INTO lifecycle_stages (tenant_id,stage_name,status,input_reference,policy_version,started_at,completed_at,error_detail) SELECT tenant_id,$1,$2,$3,$4,$5,$6,$7 FROM orchestration_runs WHERE tenant_id=$8 AND interaction_id=$9`, stage.System+":"+stage.Action, dbStatus, req.InteractionID, decision.PolicyVersion, stage.StartedAt, stage.EndedAt, stage.Detail, req.TenantID, req.InteractionID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func lastStage(decision RouteDecision) string {
	if len(decision.SelectedPlan) == 0 {
		return "unknown"
	}
	return decision.SelectedPlan[len(decision.SelectedPlan)-1].System
}

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
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if c.store != nil {
		return c.store.saveRoute(req.TenantID, req.ActorID, decision)
	}
	return nil
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
	tx, err := c.db.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT set_config('scos.tenant_id',$1,true)`, tenantID); err != nil {
		return "", err
	}
	if _, err = tx.Exec(ctx, `SELECT set_config('scos.principal_id',$1,true)`, actorID); err != nil {
		return "", err
	}
	var runID, state string
	err = tx.QueryRow(ctx, `SELECT id::text, status FROM orchestration_runs WHERE tenant_id=$1 AND interaction_id=$2 FOR UPDATE`, tenantID, interactionID).Scan(&runID, &state)
	if err != nil {
		return "", err
	}
	if state != "running" && state != "interrupted" {
		if err = tx.Commit(ctx); err != nil {
			return "", err
		}
		return state, nil
	}
	if _, err = tx.Exec(ctx, `UPDATE orchestration_runs SET status='recovering', current_stage='recovery' WHERE id=$1`, runID); err != nil {
		return "", err
	}
	if err = tx.Commit(ctx); err != nil {
		return "", err
	}
	if c.store != nil {
		key := stateKey(tenantID, actorID, interactionID)
		c.store.mu.Lock()
		if lifecycle, ok := c.store.data.Lifecycles[key]; ok {
			lifecycle.State = "recovering"
			lifecycle.RecoveryStatus = "started"
			lifecycle.UpdatedAt = time.Now().UTC()
			c.store.data.Lifecycles[key] = lifecycle
			if err := c.store.writeLocked(); err != nil {
				c.store.mu.Unlock()
				return "", err
			}
		}
		c.store.mu.Unlock()
	}
	return "recovering", nil
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
	decision, err := buildRoute(req)
	if err != nil {
		writeControlError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err = c.persistRoute(r.Context(), req, decision); err != nil {
		writeControlError(w, http.StatusServiceUnavailable, "persist route decision: "+err.Error())
		return
	}
	status, lifecycle := c.executeRoute(r.Context(), req, decision)
	if err := c.persistLifecycleDB(r.Context(), req, decision, lifecycle); err != nil {
		writeControlError(w, http.StatusServiceUnavailable, "persist lifecycle: "+err.Error())
		return
	}
	if c.db != nil && status != "completed" {
		writeControlJSON(w, http.StatusBadGateway, routeReceipt{RouteDecision: decision, Status: status, Lifecycle: lifecycle})
		return
	}
	writeControlJSON(w, http.StatusOK, routeReceipt{RouteDecision: decision, Status: status, Lifecycle: lifecycle})
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
	if c.store != nil {
		writeControlJSON(w, http.StatusOK, c.lifecycle(r.Context(), tenantID, actorID, interactionID, decision))
		return
	}
	lifecycle, err := c.lifecycleDB(r.Context(), tenantID, actorID, interactionID, decision)
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
	if state != "recovering" {
		writeControlJSON(w, http.StatusConflict, controlStatus{Status: state, Performed: false, InteractionID: req.InteractionID})
		return
	}
	writeControlJSON(w, http.StatusAccepted, controlStatus{Status: "recovering", Performed: true, InteractionID: req.InteractionID})
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
	if confirmation {
		reasons = append(reasons, "confirmation_policy_applies")
	}

	// Capability selection runs against the registry rather than assuming a
	// fixed system per intent (§21.6, §21.7).
	selectedCaps, rejectedCaps, reservedFallbacks := selectCapabilities(routingRegistry, req, class, confirmation)
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
			log.Fatalf("load control-plane state: %v", err)
		}
	}
	var db *pgxpool.Pool
	if databaseURL != "" {
		if err := database.RequireTLS(databaseURL); err != nil {
			log.Fatal(err)
		}
		db, err = pgxpool.New(context.Background(), databaseURL)
		if err != nil {
			log.Fatalf("connect to database: %v", err)
		}
		defer db.Close()
		if err := db.Ping(context.Background()); err != nil {
			log.Fatalf("database is not ready: %v", err)
		}
	}
	if db == nil {
		log.Fatal("DATABASE_URL is required; local control-plane state is test-only fallback")
	}
	commitEndpoint := strings.TrimSpace(os.Getenv("SCOS_MEMORY_GATEWAY_URL"))
	if commitEndpoint == "" {
		log.Fatal("SCOS_MEMORY_GATEWAY_URL is required; authoritative turn commit is unavailable")
	}
	// memory-gateway is an internal service, so its committer uses the shared
	// internal TLS trust anchor rather than the system pool used for the
	// (potentially external) agent endpoint dispatcher below.
	commitClient, err := tlsserve.TrustedClient(10 * time.Second)
	if err != nil {
		log.Fatalf("configure internal TLS client: %v", err)
	}
	plane := &controlPlane{db: db, store: store, client: commitClient, committer: memoryGatewayCommitter{endpoint: commitEndpoint, client: commitClient}}
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"live"}`))
	})
	mux.HandleFunc("/health/ready", readiness.Handler(func() error {
		return plane.readinessCheck()
	}))
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
	mux.HandleFunc("/v1/control/status", plane.handleStatus)
	mux.HandleFunc("/v1/control/status/", plane.handleStatus)
	mux.HandleFunc("/v1/triggers", plane.handleIngest)

	addr := os.Getenv("SCOS_CONTROL_HTTP_ADDR")
	if addr == "" {
		addr = ":8081"
	}
	log.Printf("autonomous control plane listening on %s", addr)
	if err := tlsserve.ListenAndServe(addr, auth.AuthenticatedHandler(mux, auth.MustFromEnvironment())); err != nil {
		log.Fatal(err)
	}
}
