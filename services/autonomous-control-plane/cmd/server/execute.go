package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	hooksdk "github.com/dylmarriner/scos-memory/packages/hook-sdk"
)

// Turn-boundary events this runtime has always dispatched; they predate and
// sit outside the §22.6 catalogue, so hook-sdk has no constants for them.
const (
	hookPreTurn  = "pre_turn"
	hookPostTurn = "post_turn"
)

// hookTally counts lifecycle triggers for trigger coverage: those whose
// condition occurred while a hook dispatcher was configured, and how many
// were delivered.
type hookTally struct{ attempted, fired int }

// countedHook dispatches a lifecycle event and records it in t.
func (c *controlPlane) countedHook(ctx context.Context, t *hookTally, req RouteRequest, event string, payload map[string]any) error {
	err := c.dispatchHook(ctx, event, payload, req)
	if hooksConfigured() {
		t.attempted++
		if err == nil {
			t.fired++
		}
	}
	return err
}

// executeRoute runs a freshly routed plan from its first stage.
func (c *controlPlane) executeRoute(ctx context.Context, req RouteRequest, decision RouteDecision) (string, lifecycleResponse) {
	return c.runPlan(ctx, req, decision, nil)
}

// runPlan executes decision's plan. When resume carries the durable state of
// an interrupted run, stages that already completed are skipped and their
// saved outputs reused; a completed stage whose output was not stored runs
// again, and so does every stage that did not complete. Progress is recorded
// durably after every stage so a crash can be resumed.
func (c *controlPlane) runPlan(ctx context.Context, req RouteRequest, decision RouteDecision, resume *lifecycleState) (status string, lifecycle lifecycleResponse) {
	obs := runObservation{invoked: map[string]bool{}, phase: "ingest", hooks: &hookTally{}}
	// A recovery writes under its lease and reports "recovering" while it runs.
	owner, runningState := "", "running"
	if resume != nil {
		obs.phase, owner, runningState = "recovery", resume.LeaseOwner, "recovering"
	}
	var outputs stageOutputs
	var reply agentReply
	var failureDetail string
	defer func() {
		obs.reply = reply
		recordRunMetrics(req, decision, status, lifecycle, obs)
	}()
	hook := func(event string, payload map[string]any) error {
		return c.countedHook(ctx, obs.hooks, req, event, payload)
	}
	// recoveryFinished reports the recovery outcome to extensions. It is sent
	// once the outcome is final, so its own delivery cannot change it.
	recoveryFinished := func(outcome string) {
		_ = hook(hooksdk.OnRecovery, map[string]any{"interaction_id": req.InteractionID, "stage": "finished", "outcome": outcome})
	}
	save := func(p progress) error {
		p.failureDetail = failureDetail
		return c.saveProgress(ctx, req, lifecycle, owner, p)
	}

	status = "completed"
	lifecycle = lifecycleResponse{
		Status: runningState, InteractionID: req.InteractionID, DecisionID: decision.DecisionID,
		Reply: replyState{Status: "pending"}, Checkpoint: checkpointState{Status: "pending"},
		Outbox: outboxState{Status: "pending"}, Stages: make([]stageState, len(decision.SelectedPlan)),
	}
	for i, step := range decision.SelectedPlan {
		lifecycle.Stages[i] = stageState{Order: step.Order, System: step.System, Action: step.Action, Required: step.Required, Status: "pending"}
	}
	if resume != nil {
		outputs = resume.Outputs
		if outputs.Agent != nil {
			reply = *outputs.Agent
		}
		lifecycle.Reply, lifecycle.Checkpoint, lifecycle.Outbox = resume.Reply, resume.Checkpoint, resume.Outbox
		for i := range lifecycle.Stages {
			if i < len(resume.Stages) && resume.Stages[i].System == lifecycle.Stages[i].System &&
				resume.Stages[i].Status == "completed" && outputs.has(lifecycle.Stages[i].System) {
				lifecycle.Stages[i] = resume.Stages[i]
			}
		}
		// A failed start hook aborts the attempt, as a failed on_turn_start
		// aborts a turn.
		if err := hook(hooksdk.OnRecovery, map[string]any{"interaction_id": req.InteractionID, "stage": "started"}); err != nil {
			status, failureDetail = "degraded", err.Error()
			lifecycle.Status = "failed"
			_ = save(progress{state: "failed", recoveryStatus: "failed", release: true})
			recoveryFinished("failed")
			return status, lifecycle
		}
	} else {
		for _, event := range []string{hookPreTurn, hooksdk.OnTurnStart} {
			payload := map[string]any{"interaction_id": req.InteractionID}
			if event == hookPreTurn {
				payload["input"] = req.Input
			}
			if err := hook(event, payload); err != nil {
				status, failureDetail = "degraded", err.Error()
				lifecycle.Status = status
				_ = save(progress{state: status})
				return status, lifecycle
			}
		}
	}

	for _, step := range decision.SelectedPlan {
		idx := step.Order - 1
		if lifecycle.Stages[idx].Status == "completed" {
			// Completed before the interruption: its result is in outputs.
			if registrySystems[step.System] {
				obs.invoked[step.System] = true
			}
			continue
		}
		stage := stageState{Order: step.Order, System: step.System, Action: step.Action, Required: step.Required, Status: "running", StartedAt: time.Now().UTC()}
		lifecycle.Stages[idx] = stage
		if err := save(progress{state: runningState}); errors.Is(err, errLeaseLost) {
			// Another recoverer owns the run now; this execution stops here.
			lifecycle.Stages[idx].Status, lifecycle.Stages[idx].Detail = "degraded", err.Error()
			status, lifecycle.Status = "degraded", "failed"
			return status, lifecycle
		}

		// before_* hooks are synchronous: a failed dispatch skips the stage's
		// action and degrades the stage. after_* hooks follow a successful
		// action and a failed dispatch degrades the stage.
		var err error
		switch step.System {
		case "memory-gateway":
			err = hook(hooksdk.OnBeforeRetrieval, map[string]any{"interaction_id": req.InteractionID, "source": "memory", "query": req.Input})
		case "code-intelligence":
			err = hook(hooksdk.OnBeforeRetrieval, map[string]any{"interaction_id": req.InteractionID, "source": "code", "query": req.Input, "path": req.RepositoryPath})
		case "context-optimisation":
			err = hook(hooksdk.OnBeforeContextCompile, map[string]any{"interaction_id": req.InteractionID, "task_type": contextTaskType(decision)})
		case "agent-router":
			err = hook(hooksdk.OnBeforeModel, map[string]any{"interaction_id": req.InteractionID, "input": req.Input})
		case "tool-governance", "adaptive-execution-proxy":
			err = hook(hooksdk.OnBeforeTool, map[string]any{"interaction_id": req.InteractionID, "tool": req.ProposedTool})
		}
		if err == nil {
			err = c.runStage(ctx, req, decision, step, &lifecycle, &outputs, &reply, obs.invoked)
		}
		if err == nil {
			switch step.System {
			case "memory-gateway":
				err = hook(hooksdk.OnAfterRetrieval, map[string]any{"interaction_id": req.InteractionID, "source": "memory", "result_bytes": len(outputs.Memory)})
			case "code-intelligence":
				err = hook(hooksdk.OnAfterRetrieval, map[string]any{"interaction_id": req.InteractionID, "source": "code", "result_bytes": len(outputs.Code)})
			case "context-optimisation":
				err = hook(hooksdk.OnAfterContextCompile, map[string]any{"interaction_id": req.InteractionID, "compiled_bytes": len(outputs.Compiled)})
			case "agent-router":
				err = hook(hooksdk.OnAfterModel, map[string]any{"interaction_id": req.InteractionID, "reply": reply.Content})
			}
		}
		if step.System == "tool-governance" || step.System == "adaptive-execution-proxy" {
			hookEvent := hooksdk.OnAfterTool
			if err != nil {
				hookEvent = hooksdk.OnToolError
			}
			hookErr := hook(hookEvent, map[string]any{"interaction_id": req.InteractionID, "tool": req.ProposedTool, "error": errorText(err)})
			if err == nil {
				err = hookErr
			}
		}
		if step.System == "turn-coordinator" && err == nil {
			err = hook(hooksdk.OnCheckpoint, map[string]any{"interaction_id": req.InteractionID, "checkpoint_id": lifecycle.Checkpoint.ID})
		}
		if err == nil {
			// Each output is stored once, when its stage completes.
			err = c.saveOutput(ctx, req, owner, step.System, outputs)
		}
		stage.Status, stage.EndedAt = "completed", time.Now().UTC()
		if err != nil {
			stage.Status, stage.Detail = "degraded", err.Error()
			status = "degraded"
		}
		lifecycle.Stages[idx] = stage
		if saveErr := save(progress{state: runningState}); saveErr != nil {
			// Progress that is not durable cannot be claimed as complete.
			status = "degraded"
			lifecycle.Reply, lifecycle.Checkpoint, lifecycle.Outbox = replyState{Status: "pending"}, checkpointState{Status: "pending"}, outboxState{Status: "pending"}
			if errors.Is(saveErr, errLeaseLost) {
				lifecycle.Status = "failed"
				return status, lifecycle
			}
		}
	}
	if err := hook(hookPostTurn, map[string]any{"interaction_id": req.InteractionID, "reply": reply.Content}); err != nil {
		status, failureDetail = "degraded", err.Error()
	}

	final, recoveryStatus := status, ""
	if resume != nil {
		final, recoveryStatus = "completed", "recovered"
		if status != "completed" {
			final, recoveryStatus = "failed", "failed"
		}
	}
	lifecycle.Status = final
	if err := save(progress{state: final, recoveryStatus: recoveryStatus, release: resume != nil}); err != nil && status == "completed" {
		status = "degraded"
		lifecycle.Status = status
		if resume != nil {
			lifecycle.Status = "failed"
		}
	}
	if resume != nil {
		recoveryFinished(lifecycle.Status)
	}
	if c.store != nil {
		return status, c.lifecycle(ctx, req.TenantID, req.ActorID, req.InteractionID, decision)
	}
	return status, lifecycle
}

// runStage performs one plan step and records its outputs.
func (c *controlPlane) runStage(ctx context.Context, req RouteRequest, decision RouteDecision, step RouteStep,
	lifecycle *lifecycleResponse, outputs *stageOutputs, reply *agentReply, invoked map[string]bool) error {
	switch step.System {
	case "journal", "autonomous-control-plane", "capability-router":
		// Owned by this transaction; completion is recorded with the lifecycle.
		return nil
	case "memory-gateway":
		invoked[step.System] = true
		body, err := c.callService(ctx, step.System, "/v1/retrieve", map[string]any{
			"tenant_id": req.TenantID, "principal_id": req.ActorID, "query": req.Input,
			"agent_id": req.AgentID, "session_id": req.SessionID, "include_recent": true, "include_session": true,
		}, req)
		if err == nil {
			outputs.Memory = string(body)
		}
		return err
	case "code-intelligence":
		invoked[step.System] = true
		body, err := c.callService(ctx, step.System, "/v1/code/query", map[string]any{
			"intent": "implementation_search", "query": req.Input, "path": req.RepositoryPath, "max_results": 10,
		}, req)
		if err == nil {
			outputs.Code = string(body)
		}
		return err
	case "context-optimisation":
		invoked[step.System] = true
		// No profile is requested: the optimiser chooses it and applies its
		// §18 floor from task type, risk and sensitivity. Sensitivity uses the
		// same conservative default as capability selection, so an undeclared
		// classification is treated as confidential here too.
		body, err := c.callService(ctx, step.System, "/v1/context/compile", map[string]any{
			"input": strings.Join([]string{req.Input, outputs.Memory, outputs.Code}, "\n\n"), "task_type": contextTaskType(decision),
			"token_budget": 8000, "risk_class": decision.RiskClass, "sensitivity": dataClassificationFor(req),
		}, req)
		if err != nil {
			return err
		}
		compiled, err := compiledContext(body)
		if err == nil {
			outputs.Compiled = compiled
		}
		return err
	case "tool-governance":
		if strings.TrimSpace(req.ProposedTool) == "" {
			return errors.New("proposed_tool is required for governance")
		}
		invoked[step.System] = true
		_, err := c.callService(ctx, step.System, "/v1/operations/start", map[string]any{
			"tool_id": req.ProposedTool, "input": map[string]any{"interaction_id": req.InteractionID, "input": req.Input},
		}, req)
		return err
	case "adaptive-execution-proxy":
		invoked[step.System] = true
		_, err := c.callService(ctx, step.System, "/v1/proxy/execute", map[string]any{
			"tenant_id": req.TenantID, "principal_id": req.ActorID, "agent_id": req.AgentID,
			"operation_id": req.InteractionID, "command": req.ProposedTool, "working_directory": req.RepositoryPath,
			"confirmed": !decision.ConfirmationRequired,
		}, req)
		return err
	case "agent-router":
		// The validated compiled capsule replaces the raw context. Without it
		// (compilation failed or was not planned) the authoritative
		// uncompressed context is dispatched instead (§21.16).
		agentContext := outputs.Compiled
		if agentContext == "" {
			agentContext = strings.Join([]string{outputs.Memory, outputs.Code}, "\n\n")
		}
		result, err := c.dispatchAgent(ctx, agentRequest{InteractionID: req.InteractionID, TenantID: req.TenantID, PrincipalID: req.ActorID, AgentID: req.AgentID, Input: req.Input, Context: agentContext})
		if err != nil {
			return err
		}
		*reply = result
		outputs.Agent = &result
		return nil
	case "turn-coordinator":
		if strings.TrimSpace(reply.Content) == "" {
			return errors.New("agent reply was not captured")
		}
		receipt, err := c.commitReply(ctx, req, *reply)
		if err != nil {
			return err
		}
		lifecycle.Checkpoint = checkpointState{Status: "completed", ID: receipt.CheckpointID}
		if receipt.OutboxOperationID != "" {
			lifecycle.Outbox = outboxState{Status: "completed", ID: receipt.OutboxOperationID}
		}
		lifecycle.Reply = replyState{Status: "completed", Content: reply.Content}
		return nil
	case "consolidation":
		// Consolidation is enqueued by memory-gateway as a turn.completed
		// outbox job in the same transaction that commits the reply and
		// checkpoint. This stage verifies that enqueue happened for this turn.
		if lifecycle.Outbox.Status != "completed" || lifecycle.Outbox.ID == "" {
			return errors.New("consolidation was not enqueued: no committed turn.completed outbox job for this turn")
		}
		return nil
	}
	return errors.New("service URL is not configured")
}

// compiledContext extracts the validated compiled text from a
// /v1/context/compile response. An unvalidated capsule dropped mandatory
// content and is treated as a compilation failure. A capsule the optimiser
// fell back to the lossless "exact" profile for (transformations include
// "fallback:exact") is validated and therefore a success; the capsule's
// profile and transformations are informational and deliberately not
// inspected, since they list only the stages that actually ran.
func compiledContext(body []byte) (string, error) {
	var result struct {
		CompiledText string `json:"compiled_text"`
		Validated    bool   `json:"validated"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", errors.New("context compilation returned an unreadable capsule: " + err.Error())
	}
	if !result.Validated {
		return "", errors.New("context compilation failed invariant validation")
	}
	if strings.TrimSpace(result.CompiledText) == "" {
		return "", errors.New("context compilation returned no compiled context")
	}
	return result.CompiledText, nil
}
