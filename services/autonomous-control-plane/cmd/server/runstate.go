package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Durable run state: claiming a run for execution, persisting each stage
// transition under the claiming attempt, recording confirmations, and the
// state a recovery resumes from. Postgres is authoritative; the local store
// is the embedded test fallback with the same transitions.

// beginScopedTx opens a transaction carrying the RLS identity every
// control-plane table is scoped by.
func (c *controlPlane) beginScopedTx(ctx context.Context, tenantID, actorID string) (pgx.Tx, error) {
	tx, err := c.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `SELECT set_config('scos.tenant_id',$1,true)`, tenantID); err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	if _, err = tx.Exec(ctx, `SELECT set_config('scos.principal_id',$1,true)`, actorID); err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	return tx, nil
}

// dbStageStatus maps a stage status onto lifecycle_stages' vocabulary.
func dbStageStatus(status string) string {
	switch status {
	case stageDegraded, stageBlocked:
		return "failed"
	}
	return status
}

func nullableTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// currentStage names the stage a run is at: the running stage, else the last
// stage that left pending.
func currentStage(lifecycle lifecycleResponse) string {
	current := "execution"
	for _, stage := range lifecycle.Stages {
		if stage.Status == stageRunning {
			return stage.System
		}
		if stage.Status != stagePending {
			current = stage.System
		}
	}
	return current
}

// replaceStagesDB rewrites the persisted stage rows of an interaction so they
// mirror the lifecycle exactly, in plan order.
func replaceStagesDB(ctx context.Context, tx pgx.Tx, tenantID, interactionID, policyVersion string, stages []stageState) error {
	if _, err := tx.Exec(ctx, `DELETE FROM lifecycle_stages WHERE tenant_id=$1 AND input_reference=$2`, tenantID, interactionID); err != nil {
		return err
	}
	for i, stage := range stages {
		var detail *string
		if stage.Detail != "" {
			value := stage.Detail
			detail = &value
		}
		if _, err := tx.Exec(ctx, `INSERT INTO lifecycle_stages (tenant_id,stage_name,status,input_reference,policy_version,started_at,completed_at,error_detail,stage_order) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			tenantID, stage.System+":"+stage.Action, dbStageStatus(stage.Status), interactionID, policyVersion,
			nullableTime(stage.StartedAt), nullableTime(stage.EndedAt), detail, i+1); err != nil {
			return err
		}
	}
	return nil
}

func (c *controlPlane) claimExecution(ctx context.Context, req RouteRequest, attempt string) error {
	if c.db != nil {
		if err := c.claimExecutionDB(ctx, req, attempt); err != nil {
			return err
		}
		if c.store != nil {
			return c.store.claimExecution(req.TenantID, req.ActorID, req.InteractionID, attempt)
		}
		return nil
	}
	if c.store != nil {
		return c.store.claimExecution(req.TenantID, req.ActorID, req.InteractionID, attempt)
	}
	return nil
}

func (c *controlPlane) claimExecutionDB(ctx context.Context, req RouteRequest, attempt string) error {
	tx, err := c.beginScopedTx(ctx, req.TenantID, req.ActorID)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var status string
	if err = tx.QueryRow(ctx, `SELECT status FROM orchestration_runs WHERE tenant_id=$1 AND interaction_id=$2 FOR UPDATE`, req.TenantID, req.InteractionID).Scan(&status); err != nil {
		return err
	}
	if !claimableRunStatus(status) {
		return errExecutionNotClaimable
	}
	if _, err = tx.Exec(ctx, `UPDATE orchestration_runs SET status='running', current_stage='execution', execution_attempt=$3, completed_at=NULL, checkpoint=checkpoint || jsonb_build_object('status','running','detail','') WHERE tenant_id=$1 AND interaction_id=$2`, req.TenantID, req.InteractionID, attempt); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (c *controlPlane) recordProgress(ctx context.Context, req RouteRequest, decision RouteDecision, attempt string, lifecycle lifecycleResponse) error {
	if c.db != nil {
		if err := c.recordProgressDB(ctx, req, decision, attempt, lifecycle); err != nil {
			return err
		}
		if c.store != nil {
			return c.store.recordProgress(req.TenantID, req.ActorID, req.InteractionID, attempt, lifecycle)
		}
		return nil
	}
	if c.store != nil {
		return c.store.recordProgress(req.TenantID, req.ActorID, req.InteractionID, attempt, lifecycle)
	}
	return nil
}

func (c *controlPlane) recordProgressDB(ctx context.Context, req RouteRequest, decision RouteDecision, attempt string, lifecycle lifecycleResponse) error {
	checkpoint, err := json.Marshal(map[string]any{
		"reply": lifecycle.Reply, "checkpoint": lifecycle.Checkpoint, "outbox": lifecycle.Outbox,
		"status": lifecycle.Status, "detail": lifecycle.Detail,
	})
	if err != nil {
		return err
	}
	terminal := terminalRunStatus(lifecycle.Status)
	tx, err := c.beginScopedTx(ctx, req.TenantID, req.ActorID)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var runID string
	err = tx.QueryRow(ctx, `UPDATE orchestration_runs SET status=$1, current_stage=$2, checkpoint=checkpoint || $3::jsonb, completed_at=CASE WHEN $4::boolean THEN now() ELSE NULL END WHERE tenant_id=$5 AND interaction_id=$6 AND execution_attempt=$7 RETURNING id::text`,
		lifecycle.Status, currentStage(lifecycle), checkpoint, terminal, req.TenantID, req.InteractionID, attempt).Scan(&runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return errExecutionSuperseded
	}
	if err != nil {
		return err
	}
	if err = replaceStagesDB(ctx, tx, req.TenantID, req.InteractionID, decision.PolicyVersion, lifecycle.Stages); err != nil {
		return err
	}
	if terminal {
		// The executed result is the outcome policy evaluation judges each
		// policy version by: success only when every stage of the plan completed.
		executed, err := json.Marshal(map[string]any{"success": lifecycle.Status == runCompleted, "status": lifecycle.Status})
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE routing_decisions SET actual_outcome = COALESCE(actual_outcome,'{}'::jsonb) || $1::jsonb WHERE decision_id=$2 AND orchestration_run_id=$3`, executed, decision.DecisionID, runID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (c *controlPlane) resolveConfirmation(ctx context.Context, tenantID, actorID, interactionID, decisionID, resolution string) (RouteRequest, RouteDecision, *confirmationRecord, error) {
	if c == nil || (c.db == nil && c.store == nil) {
		return RouteRequest{}, RouteDecision{}, nil, errors.New("control-plane persistence is unavailable")
	}
	if c.db == nil {
		return c.store.resolveConfirmation(tenantID, actorID, interactionID, decisionID, resolution)
	}
	req, decision, record, err := c.resolveConfirmationDB(ctx, tenantID, actorID, interactionID, decisionID, resolution)
	if err != nil {
		return RouteRequest{}, RouteDecision{}, nil, err
	}
	if c.store != nil {
		if _, _, _, err := c.store.resolveConfirmation(tenantID, actorID, interactionID, decisionID, resolution); err != nil && !errors.Is(err, errLocalStateNotFound) {
			return RouteRequest{}, RouteDecision{}, nil, err
		}
	}
	return req, decision, record, nil
}

func (c *controlPlane) resolveConfirmationDB(ctx context.Context, tenantID, actorID, interactionID, decisionID, resolution string) (RouteRequest, RouteDecision, *confirmationRecord, error) {
	tx, err := c.beginScopedTx(ctx, tenantID, actorID)
	if err != nil {
		return RouteRequest{}, RouteDecision{}, nil, err
	}
	defer tx.Rollback(ctx)
	var (
		runID, runStatus string
		rawRequest       []byte
	)
	if err = tx.QueryRow(ctx, `SELECT id::text, status, route_request FROM orchestration_runs WHERE tenant_id=$1 AND interaction_id=$2 FOR UPDATE`, tenantID, interactionID).Scan(&runID, &runStatus, &rawRequest); err != nil {
		return RouteRequest{}, RouteDecision{}, nil, err
	}
	decision, err := scanRouteDecision(tx.QueryRow(ctx, decisionSelect+` WHERE d.tenant_id=$1 AND d.actor_id=$2 AND d.orchestration_run_id=$3 ORDER BY d.created_at DESC LIMIT 1`, tenantID, actorID, runID))
	if err != nil {
		return RouteRequest{}, RouteDecision{}, nil, err
	}
	if decision.DecisionID != decisionID {
		return RouteRequest{}, RouteDecision{}, nil, errConfirmationDecisionMismatch
	}
	var status, requestedBy string
	err = tx.QueryRow(ctx, `SELECT status, requested_by::text FROM control_confirmations WHERE orchestration_run_id=$1 AND decision_id=$2 FOR UPDATE`, runID, decisionID).Scan(&status, &requestedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return RouteRequest{}, RouteDecision{}, nil, errConfirmationNotAwaiting
	}
	if err != nil {
		return RouteRequest{}, RouteDecision{}, nil, err
	}
	if requestedBy != actorID {
		return RouteRequest{}, RouteDecision{}, nil, errConfirmationRequester
	}
	if status != confirmationAwaiting || runStatus != runAwaitingConfirmation {
		return RouteRequest{}, RouteDecision{}, nil, errConfirmationNotAwaiting
	}
	if len(rawRequest) == 0 {
		return RouteRequest{}, RouteDecision{}, nil, errRouteRequestMissing
	}
	var req RouteRequest
	if err = json.Unmarshal(rawRequest, &req); err != nil {
		return RouteRequest{}, RouteDecision{}, nil, err
	}
	var resolvedAt time.Time
	if err = tx.QueryRow(ctx, `UPDATE control_confirmations SET status=$1, resolved_by=$2, resolved_at=now() WHERE orchestration_run_id=$3 AND decision_id=$4 RETURNING resolved_at`, resolution, actorID, runID, decisionID).Scan(&resolvedAt); err != nil {
		return RouteRequest{}, RouteDecision{}, nil, err
	}
	if resolution == confirmationRejected {
		if _, err = tx.Exec(ctx, `UPDATE orchestration_runs SET status='rejected', current_stage='confirmation', completed_at=now(), checkpoint=checkpoint || jsonb_build_object('status','rejected','detail',$2::text) WHERE id=$1`, runID, rejectedDetail); err != nil {
			return RouteRequest{}, RouteDecision{}, nil, err
		}
		if _, err = tx.Exec(ctx, `UPDATE lifecycle_stages SET status='skipped', error_detail=$3 WHERE tenant_id=$1 AND input_reference=$2 AND status='pending'`, tenantID, interactionID, rejectedDetail); err != nil {
			return RouteRequest{}, RouteDecision{}, nil, err
		}
		if _, err = tx.Exec(ctx, `UPDATE routing_decisions SET actual_outcome = COALESCE(actual_outcome,'{}'::jsonb) || '{"success":false,"status":"rejected"}'::jsonb WHERE decision_id=$1 AND orchestration_run_id=$2`, decisionID, runID); err != nil {
			return RouteRequest{}, RouteDecision{}, nil, err
		}
	} else {
		if _, err = tx.Exec(ctx, `UPDATE orchestration_runs SET status='confirmed', current_stage='confirmation', checkpoint=checkpoint || jsonb_build_object('status','confirmed','detail','') WHERE id=$1`, runID); err != nil {
			return RouteRequest{}, RouteDecision{}, nil, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return RouteRequest{}, RouteDecision{}, nil, err
	}
	resolvedAt = resolvedAt.UTC()
	return req, decision, &confirmationRecord{DecisionID: decisionID, Status: resolution, RequestedBy: requestedBy, ResolvedBy: actorID, ResolvedAt: &resolvedAt}, nil
}

func (c *controlPlane) recoveryInput(ctx context.Context, tenantID, actorID, interactionID string) (recoveryInput, error) {
	if c.db == nil {
		return c.store.recoveryInput(tenantID, actorID, interactionID)
	}
	tx, err := c.beginScopedTx(ctx, tenantID, actorID)
	if err != nil {
		return recoveryInput{}, err
	}
	defer tx.Rollback(ctx)
	var (
		runID      string
		rawRequest []byte
	)
	if err = tx.QueryRow(ctx, `SELECT id::text, route_request FROM orchestration_runs WHERE tenant_id=$1 AND interaction_id=$2`, tenantID, interactionID).Scan(&runID, &rawRequest); err != nil {
		return recoveryInput{}, err
	}
	decision, err := scanRouteDecision(tx.QueryRow(ctx, decisionSelect+` WHERE d.tenant_id=$1 AND d.actor_id=$2 AND d.orchestration_run_id=$3 ORDER BY d.created_at DESC LIMIT 1`, tenantID, actorID, runID))
	if err != nil {
		return recoveryInput{}, err
	}
	input := recoveryInput{Decision: decision}
	if len(rawRequest) > 0 {
		var req RouteRequest
		if err = json.Unmarshal(rawRequest, &req); err != nil {
			return recoveryInput{}, err
		}
		input.Request = &req
	}
	confirmation, err := confirmationForDecisionDB(ctx, tx, runID, decision.DecisionID)
	if err != nil {
		return recoveryInput{}, err
	}
	input.Confirmation = confirmation
	if input.Stages, err = stagesDB(ctx, tx, tenantID, interactionID, decision); err != nil {
		return recoveryInput{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return recoveryInput{}, err
	}
	return input, nil
}

func confirmationForDecisionDB(ctx context.Context, tx pgx.Tx, runID, decisionID string) (*confirmationRecord, error) {
	var (
		record     confirmationRecord
		resolvedBy *string
		resolvedAt *time.Time
	)
	err := tx.QueryRow(ctx, `SELECT decision_id, status, requested_by::text, resolved_by::text, resolved_at FROM control_confirmations WHERE orchestration_run_id=$1 AND decision_id=$2`, runID, decisionID).Scan(&record.DecisionID, &record.Status, &record.RequestedBy, &resolvedBy, &resolvedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if resolvedBy != nil {
		record.ResolvedBy = *resolvedBy
	}
	if resolvedAt != nil {
		utc := resolvedAt.UTC()
		record.ResolvedAt = &utc
	}
	return &record, nil
}

// stagesDB reads the persisted stages of an interaction in plan order.
func stagesDB(ctx context.Context, tx pgx.Tx, tenantID, interactionID string, decision RouteDecision) ([]stageState, error) {
	rows, err := tx.Query(ctx, `SELECT stage_name, status, COALESCE(error_detail,''), started_at, completed_at, COALESCE(stage_order, 0) FROM lifecycle_stages WHERE tenant_id=$1 AND input_reference=$2 ORDER BY stage_order NULLS LAST, created_at`, tenantID, interactionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var stages []stageState
	for rows.Next() {
		var (
			name, status, detail string
			started, ended       *time.Time
			order                int
		)
		if err = rows.Scan(&name, &status, &detail, &started, &ended, &order); err != nil {
			return nil, err
		}
		parts := strings.SplitN(name, ":", 2)
		stage := stageState{System: parts[0], Status: status, Detail: detail}
		if len(parts) == 2 {
			stage.Action = parts[1]
		}
		if started != nil {
			stage.StartedAt = started.UTC()
		}
		if ended != nil {
			stage.EndedAt = ended.UTC()
		}
		stage.Order = len(stages) + 1
		if order > 0 {
			stage.Order = order
		}
		if stage.Order <= len(decision.SelectedPlan) {
			stage.Required = decision.SelectedPlan[stage.Order-1].Required
		}
		stages = append(stages, stage)
	}
	return stages, rows.Err()
}

func (c *controlPlane) markReconciliationRequired(ctx context.Context, tenantID, actorID, interactionID, detail string) error {
	if c.db == nil {
		return c.store.markReconciliationRequired(tenantID, actorID, interactionID, detail)
	}
	tx, err := c.beginScopedTx(ctx, tenantID, actorID)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `UPDATE orchestration_runs SET status='reconciliation_required', current_stage='recovery', completed_at=now(), checkpoint=checkpoint || jsonb_build_object('status','reconciliation_required','detail',$3::text) WHERE tenant_id=$1 AND interaction_id=$2 AND status='recovering'`, tenantID, interactionID, detail)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errExecutionNotClaimable
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	if c.store != nil {
		if err := c.store.markReconciliationRequired(tenantID, actorID, interactionID, detail); err != nil && !errors.Is(err, errLocalStateNotFound) {
			return err
		}
	}
	return nil
}

// sideEffectInFlight reports whether a recovery cannot safely re-run the
// plan: the execution proxy stage left pending means the command never
// started; any other state means it may have run.
func sideEffectInFlight(stages []stageState) (string, bool) {
	for _, stage := range stages {
		if stage.System == "adaptive-execution-proxy" && stage.Status != stagePending && stage.Status != stageSkipped {
			return "the execution proxy stage was " + stage.Status + " when the run stopped, so the command may already have run; reconcile its tool-governance operation before re-running the interaction", true
		}
	}
	return "", false
}
