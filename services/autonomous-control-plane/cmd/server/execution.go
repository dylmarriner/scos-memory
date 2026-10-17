package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Run lifecycle (orchestration_runs.status). A run is planned at ingress, or
// awaiting_confirmation when its decision requires the requesting principal
// to confirm it. Only planned, confirmed and recovering runs may be claimed
// for execution; a claimed run is running until it reaches a terminal status
// or is interrupted.
const (
	runPlanned                = "planned"
	runAwaitingConfirmation   = "awaiting_confirmation"
	runConfirmed              = "confirmed"
	runRejected               = "rejected"
	runRunning                = "running"
	runInterrupted            = "interrupted"
	runRecovering             = "recovering"
	runBlocked                = "blocked"
	runCompleted              = "completed"
	runDegraded               = "degraded"
	runReconciliationRequired = "reconciliation_required"

	// Response-only outcomes: nothing was executed, or this executor lost
	// ownership of the run to a newer attempt.
	runNotStarted = "not_started"
	runSuperseded = "superseded"
)

// Stage statuses. blocked is a stage that a policy gate refused; degraded is
// a stage whose work failed. Both persist as lifecycle_stages 'failed'.
const (
	stagePending     = "pending"
	stageRunning     = "running"
	stageCompleted   = "completed"
	stageDegraded    = "degraded"
	stageSkipped     = "skipped"
	stageBlocked     = "blocked"
	stageInterrupted = "interrupted"
)

// Confirmation record statuses (control_confirmations.status).
const (
	confirmationAwaiting   = "awaiting"
	confirmationConfirmed  = "confirmed"
	confirmationRejected   = "rejected"
	confirmationSuperseded = "superseded"
)

const (
	awaitingConfirmationDetail = "the decision requires confirmation by the requesting principal; no stage has been executed"
	rejectedDetail             = "not run: the requesting principal rejected the action"
)

var (
	errExecutionNotClaimable        = errors.New("run is not in a state that may start execution")
	errExecutionSuperseded          = errors.New("run is owned by a newer execution attempt")
	errConfirmationNotAwaiting      = errors.New("interaction is not awaiting confirmation")
	errConfirmationDecisionMismatch = errors.New("decision_id is not the decision awaiting confirmation for this interaction")
	errConfirmationRequester        = errors.New("only the principal that requested the action may confirm or reject it")
	errRouteRequestMissing          = errors.New("run has no persisted route request")
)

// confirmationRecord is the server-side record of the requesting principal's
// answer to a decision that requires confirmation. It is written only by an
// authenticated confirm call, never derived from the decision itself.
type confirmationRecord struct {
	DecisionID  string     `json:"decision_id"`
	Status      string     `json:"status"`
	RequestedBy string     `json:"requested_by"`
	ResolvedBy  string     `json:"resolved_by,omitempty"`
	ResolvedAt  *time.Time `json:"resolved_at,omitempty"`
}

// grants reports whether this record is a recorded confirmation of decision
// by the principal that requested it.
func (r *confirmationRecord) grants(decision RouteDecision, actorID string) bool {
	return r != nil && r.Status == confirmationConfirmed && r.DecisionID == decision.DecisionID &&
		r.ResolvedAt != nil && r.ResolvedBy != "" && r.ResolvedBy == r.RequestedBy && r.RequestedBy == actorID
}

// recoveryInput is the persisted state a recovery resumes from.
type recoveryInput struct {
	Request      *RouteRequest
	Decision     RouteDecision
	Confirmation *confirmationRecord
	Stages       []stageState
}

func claimableRunStatus(status string) bool {
	return status == runPlanned || status == runConfirmed || status == runRecovering
}

func terminalRunStatus(status string) bool {
	switch status {
	case runCompleted, runDegraded, runBlocked, runRejected, runReconciliationRequired:
		return true
	}
	return false
}

func pendingStages(decision RouteDecision) []stageState {
	stages := make([]stageState, len(decision.SelectedPlan))
	for i, step := range decision.SelectedPlan {
		stages[i] = stageState{Order: step.Order, System: step.System, Action: step.Action, Required: step.Required, Status: stagePending}
	}
	return stages
}

func newLifecycle(req RouteRequest, decision RouteDecision, status string) lifecycleResponse {
	return lifecycleResponse{
		Status: status, InteractionID: req.InteractionID, DecisionID: decision.DecisionID,
		Reply: replyState{Status: "pending"}, Checkpoint: checkpointState{Status: "pending"},
		Outbox: outboxState{Status: "pending"}, Stages: pendingStages(decision),
	}
}

// executionState carries what earlier stages produced to later ones.
type executionState struct {
	confirmed    bool
	memory, code []byte
	// context is the compiled, validated context the optimiser returned;
	// the agent receives it in place of the raw retrieval results.
	context             string
	reply               agentReply
	receipt             turnCommitReceipt
	committed           bool
	governedOperationID string
}

// agentContext is what the agent is given: the compiled context when the
// optimiser produced a validated one, otherwise the raw retrieval results.
func (s *executionState) agentContext() string {
	if s.context != "" {
		return s.context
	}
	return strings.Join([]string{string(s.memory), string(s.code)}, "\n\n")
}

type stageResult struct {
	status string
	detail string
}

func completedStage(detail string) stageResult {
	return stageResult{status: stageCompleted, detail: detail}
}
func degradedStage(detail string) stageResult {
	return stageResult{status: stageDegraded, detail: detail}
}
func skippedStage(detail string) stageResult {
	return stageResult{status: stageSkipped, detail: detail}
}
func blockedStage(detail string) stageResult {
	return stageResult{status: stageBlocked, detail: detail}
}

// executeRoute runs a persisted decision's plan. A decision that requires
// confirmation runs only with a recorded confirmation of that decision by
// the requesting principal; without one nothing is executed. Every stage
// transition is persisted under the attempt that claimed the run, so a crash
// leaves the run running (recoverable) and a cancelled request leaves it
// interrupted.
func (c *controlPlane) executeRoute(ctx context.Context, req RouteRequest, decision RouteDecision, confirmation *confirmationRecord) (string, lifecycleResponse) {
	lifecycle := newLifecycle(req, decision, runPlanned)
	if decision.ConfirmationRequired && !confirmation.grants(decision, req.ActorID) {
		lifecycle.Status, lifecycle.Detail = runAwaitingConfirmation, awaitingConfirmationDetail
		return runAwaitingConfirmation, lifecycle
	}
	state := executionState{confirmed: confirmation.grants(decision, req.ActorID)}
	// Progress writes must land even when the request context ends, so an
	// interruption is recorded rather than lost.
	persistCtx := context.WithoutCancel(ctx)
	attempt, err := newID()
	if err != nil {
		lifecycle.Status, lifecycle.Detail = runNotStarted, "allocate execution attempt: "+err.Error()
		return runNotStarted, lifecycle
	}
	if err := c.claimExecution(persistCtx, req, attempt); err != nil {
		lifecycle.Status, lifecycle.Detail = runNotStarted, "claim execution: "+err.Error()
		return runNotStarted, lifecycle
	}
	lifecycle.Status = runRunning

	outcome := runCompleted
	if err := c.dispatchHook(ctx, "pre_turn", map[string]any{"interaction_id": req.InteractionID, "input": req.Input}, req); err != nil {
		outcome, lifecycle.Detail = runDegraded, "pre_turn hook: "+err.Error()
	} else if err := c.dispatchHook(ctx, "on_turn_start", map[string]any{"interaction_id": req.InteractionID}, req); err != nil {
		outcome, lifecycle.Detail = runDegraded, "on_turn_start hook: "+err.Error()
	}
	if outcome == runCompleted {
		for idx, step := range decision.SelectedPlan {
			if ctx.Err() != nil {
				outcome, lifecycle.Detail = runInterrupted, "execution ended before stage "+step.System+": "+ctx.Err().Error()
				break
			}
			lifecycle.Stages[idx].Status, lifecycle.Stages[idx].StartedAt = stageRunning, time.Now().UTC()
			if err := c.recordProgress(persistCtx, req, decision, attempt, lifecycle); err != nil {
				return c.abandonExecution(req, decision, lifecycle, err)
			}
			result := c.runStage(ctx, req, decision, step, &state, &lifecycle)
			if result.status != stageCompleted && ctx.Err() != nil {
				// The stage was cut short: its effect is unknown, so it is
				// recorded as interrupted for recovery to reconcile.
				result = stageResult{status: stageInterrupted, detail: result.detail}
			}
			stage := lifecycle.Stages[idx]
			stage.Status, stage.Detail, stage.EndedAt = result.status, result.detail, time.Now().UTC()
			lifecycle.Stages[idx] = stage
			switch result.status {
			case stageCompleted:
				c.recordStageOutcome(persistCtx, req, step.System, true)
			case stageDegraded:
				c.recordStageOutcome(persistCtx, req, step.System, false)
				if outcome == runCompleted {
					outcome = runDegraded
				}
			case stageSkipped:
				if step.Required && outcome == runCompleted {
					outcome = runDegraded
				}
			case stageInterrupted:
				outcome, lifecycle.Detail = runInterrupted, "execution ended during stage "+step.System+": "+ctx.Err().Error()
			case stageBlocked:
				outcome, lifecycle.Detail = runBlocked, result.detail
				for rest := idx + 1; rest < len(lifecycle.Stages); rest++ {
					lifecycle.Stages[rest].Status = stageSkipped
					lifecycle.Stages[rest].Detail = "not run: execution was blocked at " + step.System
				}
			}
			if outcome == runInterrupted || outcome == runBlocked {
				break
			}
			if err := c.recordProgress(persistCtx, req, decision, attempt, lifecycle); err != nil {
				return c.abandonExecution(req, decision, lifecycle, err)
			}
		}
	}
	if outcome != runInterrupted {
		if err := c.dispatchHook(ctx, "post_turn", map[string]any{"interaction_id": req.InteractionID, "reply": state.reply.Content}, req); err != nil && outcome == runCompleted {
			outcome, lifecycle.Detail = runDegraded, "post_turn hook: "+err.Error()
		}
	}
	lifecycle.Status = outcome
	if err := c.recordProgress(persistCtx, req, decision, attempt, lifecycle); err != nil {
		return c.abandonExecution(req, decision, lifecycle, err)
	}
	if c.store != nil {
		return outcome, c.lifecycle(persistCtx, req.TenantID, req.ActorID, req.InteractionID, decision)
	}
	return outcome, lifecycle
}

// abandonExecution reports a run whose progress could not be persisted. A
// run another attempt now owns is reported as superseded; otherwise the
// durable state is unknown, so nothing is claimed as committed.
func (c *controlPlane) abandonExecution(req RouteRequest, decision RouteDecision, lifecycle lifecycleResponse, err error) (string, lifecycleResponse) {
	if errors.Is(err, errExecutionSuperseded) {
		lifecycle.Status, lifecycle.Detail = runSuperseded, "execution stopped: "+err.Error()
		return runSuperseded, lifecycle
	}
	lifecycle.Status, lifecycle.Detail = runDegraded, "persist lifecycle: "+err.Error()
	lifecycle.Reply, lifecycle.Checkpoint, lifecycle.Outbox = replyState{Status: "pending"}, checkpointState{Status: "pending"}, outboxState{Status: "pending"}
	return runDegraded, lifecycle
}

// runStage performs one plan stage. A stage reports completed only when its
// work was done and verified; a stage with no executor reports skipped, and
// so does a stage whose capability the decision's capability selection
// rejected.
func (c *controlPlane) runStage(ctx context.Context, req RouteRequest, decision RouteDecision, step RouteStep, state *executionState, lifecycle *lifecycleResponse) stageResult {
	if reasons, rejected := rejectedStageCapability(decision, step); rejected {
		return skippedStage("not run: capability " + stepCapabilities[step.System] + " was rejected by capability selection (" + strings.Join(reasons, ", ") + ")")
	}
	switch step.System {
	case "journal":
		return c.verifyIngressStage(ctx, req, decision)
	case "autonomous-control-plane":
		return verifyIdentityAndPolicyStage(req, decision)
	case "capability-router":
		return authoritativeSourceStage(decision)
	case "memory-gateway":
		memory, err := c.callService(ctx, step.System, "/v1/retrieve", map[string]any{
			"tenant_id": req.TenantID, "principal_id": req.ActorID, "query": req.Input,
			"agent_id": req.AgentID, "session_id": req.SessionID, "include_recent": true, "include_session": true,
		}, req)
		if err != nil {
			return degradedStage(err.Error())
		}
		state.memory = memory
		return completedStage("")
	case "code-intelligence":
		// Code context comes from the durable (v2) index of the named
		// repository -- the one scos_code_index builds -- resolved by its
		// canonical path. No repository named means no code context.
		if strings.TrimSpace(req.RepositoryPath) == "" {
			return skippedStage("no repository_path on the request")
		}
		registered, err := c.callService(ctx, step.System, "/v2/code/repos", map[string]any{"canonical_path": req.RepositoryPath}, req)
		if err != nil {
			return degradedStage("resolve repository: " + err.Error())
		}
		var repo struct {
			RepositoryID string `json:"repository_id"`
		}
		if err := json.Unmarshal(registered, &repo); err != nil || repo.RepositoryID == "" {
			return degradedStage("resolve repository: code-intelligence returned no repository_id")
		}
		code, err := c.callService(ctx, step.System, "/v2/code/query", map[string]any{
			"repository_id": repo.RepositoryID, "query": req.Input, "max_results": 10,
		}, req)
		if err != nil {
			return degradedStage(err.Error())
		}
		state.code = code
		return completedStage("")
	case "context-optimisation":
		parts := []string{req.Input, string(state.memory), string(state.code)}
		// Sensitivity and risk travel with the request so the optimiser can
		// apply its §18 profile floor; a hardcoded "balanced" here would
		// silently compress high-impact work.
		compiled, err := c.callService(ctx, step.System, "/v1/context/compile", map[string]any{
			"input": strings.Join(parts, "\n\n"), "task_type": contextTaskType(decision),
			"profile": "", "token_budget": 8000,
			"risk_class": decision.RiskClass, "sensitivity": dataClassificationFor(req),
		}, req)
		if err != nil {
			return degradedStage(err.Error())
		}
		var out struct {
			CompiledText string `json:"compiled_text"`
			Validated    bool   `json:"validated"`
			Capsule      struct {
				Profile        string `json:"compression_profile"`
				OriginalTokens int    `json:"original_token_count"`
				CompiledTokens int    `json:"compiled_token_count"`
			} `json:"capsule"`
		}
		if err := json.Unmarshal(compiled, &out); err != nil {
			return degradedStage("compile response: " + err.Error())
		}
		// An unvalidated capsule is not handed to the agent: the agent gets
		// the raw retrieval results and the stage reports why.
		if !out.Validated || strings.TrimSpace(out.CompiledText) == "" {
			return degradedStage("the compiled context did not pass the optimiser's validation; the agent receives the uncompiled context")
		}
		state.context = out.CompiledText
		return completedStage(fmt.Sprintf("profile %s, %d of %d tokens", out.Capsule.Profile, out.Capsule.CompiledTokens, out.Capsule.OriginalTokens))
	case "agent-router":
		if err := c.dispatchHook(ctx, "on_before_model", map[string]any{"interaction_id": req.InteractionID, "input": req.Input}, req); err != nil {
			return degradedStage("on_before_model hook: " + err.Error())
		}
		reply, err := c.dispatchAgent(ctx, agentRequest{InteractionID: req.InteractionID, TenantID: req.TenantID, PrincipalID: req.ActorID, AgentID: req.AgentID, Input: req.Input, Context: state.agentContext()})
		if err != nil {
			return degradedStage(err.Error())
		}
		state.reply = reply
		if err := c.dispatchHook(ctx, "on_after_model", map[string]any{"interaction_id": req.InteractionID, "reply": reply.Content}, req); err != nil {
			return degradedStage("on_after_model hook: " + err.Error())
		}
		return completedStage("")
	case "tool-governance":
		return c.governanceStage(ctx, req, state)
	case "adaptive-execution-proxy":
		return c.executionProxyStage(ctx, req, decision, state)
	case "turn-coordinator":
		if strings.TrimSpace(state.reply.Content) == "" {
			return degradedStage("agent reply was not captured")
		}
		receipt, err := c.commitReply(ctx, req, state.reply)
		if err != nil {
			return degradedStage(err.Error())
		}
		state.receipt, state.committed = receipt, true
		lifecycle.Checkpoint = checkpointState{Status: "completed", ID: receipt.CheckpointID}
		lifecycle.Outbox = outboxState{Status: "completed"}
		lifecycle.Reply = replyState{Status: "completed", Content: state.reply.Content}
		if err := c.dispatchHook(ctx, "on_checkpoint", map[string]any{"interaction_id": req.InteractionID, "checkpoint_id": receipt.CheckpointID}, req); err != nil {
			return degradedStage("on_checkpoint hook: " + err.Error())
		}
		return completedStage("checkpoint " + receipt.CheckpointID)
	case "consolidation":
		// memory-gateway enqueues the turn.completed outbox job the
		// consolidation worker consumes in the same transaction that commits
		// the turn, so post-turn analysis is enqueued exactly when the turn
		// commit succeeded in this run.
		if !state.committed || strings.TrimSpace(state.receipt.CheckpointID) == "" {
			return skippedStage("not run: the turn was not committed, so no post-turn analysis job was enqueued")
		}
		return completedStage("post-turn analysis enqueued with the turn.completed outbox job for checkpoint " + state.receipt.CheckpointID)
	case "recovery-orchestrator":
		return skippedStage("no in-turn checkpoint rehydration executor exists; resume an interrupted interaction with POST /v1/control/recover")
	default:
		return skippedStage("no executor is configured for " + step.System)
	}
}

// verifyIngressStage confirms the ingress was durably committed: the decision
// being executed must be the one persisted for the interaction.
func (c *controlPlane) verifyIngressStage(ctx context.Context, req RouteRequest, decision RouteDecision) stageResult {
	persisted, err := c.decisionForInteraction(ctx, req.TenantID, req.ActorID, req.InteractionID)
	if err != nil {
		return degradedStage("ingress is not durably committed: " + err.Error())
	}
	if persisted.DecisionID != decision.DecisionID {
		return degradedStage("ingress is not durably committed: the persisted decision for this interaction is " + persisted.DecisionID)
	}
	return completedStage("route decision " + decision.DecisionID + " is durably committed")
}

// verifyIdentityAndPolicyStage checks the evidence that identity resolution
// and policy evaluation produced before execution: an authenticated tenant
// and principal, and a decision stamped with the policy that made it.
func verifyIdentityAndPolicyStage(req RouteRequest, decision RouteDecision) stageResult {
	if strings.TrimSpace(req.TenantID) == "" || strings.TrimSpace(req.ActorID) == "" {
		return degradedStage("no authenticated tenant and principal were resolved")
	}
	if strings.TrimSpace(decision.PolicyVersion) == "" || strings.TrimSpace(decision.DecisionID) == "" {
		return degradedStage("the decision carries no policy version")
	}
	scope := decision.ResolvedScope
	if scope.TenantID != req.TenantID || scope.PrincipalID != req.ActorID {
		return degradedStage("the decision's resolved scope (tenant " + scope.TenantID + ", principal " + scope.PrincipalID + ") is not the authenticated identity")
	}
	return completedStage("tenant " + req.TenantID + ", principal " + req.ActorID + ", repository scope " + scope.RepositoryScope + ", data " + scope.DataClassification + ", policy " + decision.PolicyVersion + ", risk " + decision.RiskClass)
}

// authoritativeSourceStage reports the capability selection made for fresh
// external information. It completes only when a registered capability that
// serves that intent was selected.
func authoritativeSourceStage(decision RouteDecision) stageResult {
	for _, id := range decision.SelectedCapabilities {
		capability, ok := routingRegistry.Get(id)
		if !ok {
			continue
		}
		for _, intent := range capability.SupportedIntents {
			if intent == "fresh_external_information" {
				return completedStage("selected " + id)
			}
		}
	}
	return skippedStage("no registered capability serves fresh_external_information, so no current authoritative source was selected")
}

// governanceStage starts the proposed operation through tool governance and
// fails closed: any error, refusal, or response that does not prove an
// operation was started under an authorised tool blocks the run.
func (c *controlPlane) governanceStage(ctx context.Context, req RouteRequest, state *executionState) stageResult {
	if err := c.dispatchHook(ctx, "on_before_tool", map[string]any{"interaction_id": req.InteractionID, "tool": req.ProposedTool}, req); err != nil {
		return blockedStage("tool governance was not consulted: on_before_tool hook failed: " + err.Error())
	}
	operationID, err := c.startGovernedOperation(ctx, req, state.confirmed)
	hookEvent := "on_after_tool"
	if err != nil {
		hookEvent = "on_tool_error"
	}
	hookErr := c.dispatchHook(ctx, hookEvent, map[string]any{"interaction_id": req.InteractionID, "tool": req.ProposedTool, "error": errorText(err)}, req)
	if err != nil {
		return blockedStage("tool governance did not authorise the operation: " + err.Error())
	}
	if hookErr != nil {
		return blockedStage("tool governance authorised operation " + operationID + " but the on_after_tool hook failed: " + hookErr.Error())
	}
	state.governedOperationID = operationID
	return completedStage("tool governance started operation " + operationID)
}

// executionProxyToolName is the tool-governance name of the execution
// proxy, seeded by migration 000047's builtin_tool_definitions.
const executionProxyToolName = "execution-proxy"

func (c *controlPlane) startGovernedOperation(ctx context.Context, req RouteRequest, confirmed bool) (string, error) {
	if strings.TrimSpace(req.ProposedTool) == "" {
		return "", errors.New("proposed_tool is required for governance")
	}
	// The proposed tool is a command, and the tool that runs it is the
	// execution proxy (the built-in governed tool), so that is the tool
	// governance must have registered and authorised for the tenant. The
	// plan names the command so governance grades its risk with the shared
	// classifier, and confirmation is the recorded one: governance stores it
	// on the operation, and the proxy acts on governance's record of it.
	body, err := c.callService(ctx, "tool-governance", "/v1/operations/start", map[string]any{
		"tool_name": executionProxyToolName, "input": map[string]any{"interaction_id": req.InteractionID, "input": req.Input},
		"plan": map[string]any{
			"original_request": req.ProposedTool, "executed_request": req.ProposedTool,
			"working_directory": req.RepositoryPath,
		},
		"confirmed":    confirmed,
		"operation_id": governanceOperationID(req.InteractionID),
	}, req)
	if err != nil {
		return "", err
	}
	var operation struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(body, &operation); err != nil {
		return "", errors.New("tool governance returned an unreadable operation: " + err.Error())
	}
	if strings.TrimSpace(operation.ID) == "" || operation.State != "executing" {
		return "", errors.New("tool governance returned no started operation (state " + strings.TrimSpace(operation.State) + ")")
	}
	return operation.ID, nil
}

// executionProxyStage runs the proposed command under the operation tool
// governance started in this run. It asserts no confirmation to the proxy:
// the proxy reads the confirmation governance recorded on that operation,
// which the governance stage started confirmed only when a recorded
// confirmation of this decision exists.
func (c *controlPlane) executionProxyStage(ctx context.Context, req RouteRequest, decision RouteDecision, state *executionState) stageResult {
	if state.governedOperationID == "" {
		return blockedStage("execution requires an operation started by tool governance earlier in this run")
	}
	if decision.ConfirmationRequired && !state.confirmed {
		return blockedStage("execution requires a recorded confirmation of decision " + decision.DecisionID)
	}
	if err := c.dispatchHook(ctx, "on_before_tool", map[string]any{"interaction_id": req.InteractionID, "tool": req.ProposedTool}, req); err != nil {
		return degradedStage("not executed: on_before_tool hook failed: " + err.Error())
	}
	body, err := c.callService(ctx, "adaptive-execution-proxy", "/v1/proxy/execute", map[string]any{
		"tenant_id": req.TenantID, "principal_id": req.ActorID, "agent_id": req.AgentID,
		"operation_id": state.governedOperationID, "command": req.ProposedTool, "working_directory": req.RepositoryPath,
	}, req)
	// A command the proxy ran but that failed is a failed stage: the proxy
	// answers 200 with the command's own exit status, which is the outcome.
	if err == nil {
		err = commandOutcome(body)
	}
	hookEvent := "on_after_tool"
	if err != nil {
		hookEvent = "on_tool_error"
	}
	hookErr := c.dispatchHook(ctx, hookEvent, map[string]any{"interaction_id": req.InteractionID, "tool": req.ProposedTool, "error": errorText(err)}, req)
	if err != nil {
		return degradedStage(err.Error())
	}
	if hookErr != nil {
		return degradedStage("on_after_tool hook: " + hookErr.Error())
	}
	return completedStage("executed under governed operation " + state.governedOperationID)
}

// commandOutcome reads the execution proxy's result and reports a command
// that exited non-zero, with what it wrote to stderr: a reduced result lists
// stderr's error lines under errors and the rest in stderr_summary.
func commandOutcome(body []byte) error {
	var result struct {
		ExitStatus    *int     `json:"exit_status"`
		StderrSummary string   `json:"stderr_summary"`
		Errors        []string `json:"errors"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return errors.New("execution proxy returned an unreadable result: " + err.Error())
	}
	if result.ExitStatus == nil {
		return errors.New("execution proxy result carries no exit status")
	}
	if *result.ExitStatus != 0 {
		detail := strings.TrimSpace(strings.Join(append(append([]string(nil), result.Errors...), result.StderrSummary), "\n"))
		if detail == "" {
			detail = "no stderr"
		}
		return fmt.Errorf("command exited with status %d: %s", *result.ExitStatus, detail)
	}
	return nil
}
